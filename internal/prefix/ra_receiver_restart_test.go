/*
Copyright 2026 jr42.
Copyright 2026 PKizzle.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package prefix

import (
	"context"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/mdlayher/ndp"
	"golang.org/x/net/ipv6"
)

// blockingNDPConn parks in ReadFrom until it is closed, holding a receive loop
// inside the read for as long as the test wants it there.
type blockingNDPConn struct {
	closed    chan struct{}
	closeOnce sync.Once
}

func newBlockingNDPConn() *blockingNDPConn {
	return &blockingNDPConn{closed: make(chan struct{})}
}

func (c *blockingNDPConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}

func (c *blockingNDPConn) SetControlMessage(ipv6.ControlFlags, bool) error { return nil }

func (c *blockingNDPConn) ReadFrom() (ndp.Message, *ipv6.ControlMessage, netip.Addr, error) {
	<-c.closed
	return nil, nil, netip.Addr{}, net.ErrClosed
}

func (c *blockingNDPConn) SetReadDeadline(time.Time) error  { return nil }
func (c *blockingNDPConn) SetWriteDeadline(time.Time) error { return nil }

func (c *blockingNDPConn) WriteTo(ndp.Message, *ipv6.ControlMessage, netip.Addr) error {
	return nil
}

// TestRAReceiverSurvivesStartStopCycles covers restart, which the shared RA pool
// performs whenever a stopped entry is re-armed. Each generation has to get its
// own stop channel, socket and hop-limit policy: Stop does not wait for the
// previous receive loop to leave ReadFrom, so a generation that read those from
// the receiver would race the next Start for them and could act on the wrong
// socket. Run under -race, which is how `make test` runs it.
func TestRAReceiverSurvivesStartStopCycles(t *testing.T) {
	interfaces, err := net.Interfaces()
	if err != nil || len(interfaces) == 0 {
		t.Skipf("no network interfaces available: %v", err)
	}

	r := NewRAReceiver(interfaces[0].Name)

	var mu sync.Mutex
	var conns []*blockingNDPConn
	r.listen = func(*net.Interface, ndp.Addr) (ndpConn, netip.Addr, error) {
		conn := newBlockingNDPConn()
		mu.Lock()
		conns = append(conns, conn)
		mu.Unlock()
		return conn, netip.MustParseAddr("fe80::1"), nil
	}

	stopChannels := make([]chan struct{}, 0, 3)
	for cycle := range 3 {
		if err := r.Start(context.Background()); err != nil {
			t.Fatalf("Start() on cycle %d error = %v", cycle, err)
		}

		r.mu.RLock()
		stopCh := r.stopCh
		r.mu.RUnlock()
		for _, seen := range stopChannels {
			if seen == stopCh {
				t.Fatalf("cycle %d reused the previous stop channel; the new loop would exit at once", cycle)
			}
		}
		stopChannels = append(stopChannels, stopCh)

		// Stop while the loop is parked in ReadFrom, which is the interleaving the
		// next Start has to be safe against.
		if err := r.Stop(); err != nil {
			t.Fatalf("Stop() on cycle %d error = %v", cycle, err)
		}
	}

	// Stopping twice must stay safe: the second call has no generation to end.
	if err := r.Stop(); err != nil {
		t.Fatalf("second Stop() error = %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(conns) != 3 {
		t.Errorf("opened %d sockets across 3 cycles, want 3", len(conns))
	}
}
