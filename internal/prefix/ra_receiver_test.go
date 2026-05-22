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
	"bytes"
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/mdlayher/ndp"
	"golang.org/x/net/ipv6"
)

func TestIsGlobalUnicast(t *testing.T) {
	tests := []struct {
		name     string
		addr     string
		expected bool
	}{
		{
			name:     "GUA 2001:db8::1",
			addr:     "2001:db8::1",
			expected: true,
		},
		{
			name:     "GUA 2620:fe::fe",
			addr:     "2620:fe::fe",
			expected: true,
		},
		{
			name:     "GUA 2000::1",
			addr:     "2000::1",
			expected: true,
		},
		{
			name:     "GUA 3fff:ffff::1 (edge of range)",
			addr:     "3fff:ffff::1",
			expected: true,
		},
		{
			name:     "ULA fd00::1",
			addr:     "fd00::1",
			expected: false,
		},
		{
			name:     "ULA fc00::1",
			addr:     "fc00::1",
			expected: false,
		},
		{
			name:     "Link-local fe80::1",
			addr:     "fe80::1",
			expected: false,
		},
		{
			name:     "Loopback ::1",
			addr:     "::1",
			expected: false,
		},
		{
			name:     "Multicast ff02::1",
			addr:     "ff02::1",
			expected: false,
		},
		{
			name:     "IPv4 mapped",
			addr:     "::ffff:192.0.2.1",
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			addr := netip.MustParseAddr(tt.addr)
			result := isGlobalUnicast(addr)
			if result != tt.expected {
				t.Errorf("isGlobalUnicast(%s) = %v, want %v", tt.addr, result, tt.expected)
			}
		})
	}
}

func TestIsULA(t *testing.T) {
	tests := []struct {
		name     string
		addr     string
		expected bool
	}{
		{
			name:     "ULA fd00::1",
			addr:     "fd00::1",
			expected: true,
		},
		{
			name:     "ULA fc00::1",
			addr:     "fc00::1",
			expected: true,
		},
		{
			name:     "ULA fdab:cdef:1234::1",
			addr:     "fdab:cdef:1234::1",
			expected: true,
		},
		{
			name:     "GUA 2001:db8::1",
			addr:     "2001:db8::1",
			expected: false,
		},
		{
			name:     "Link-local fe80::1",
			addr:     "fe80::1",
			expected: false,
		},
		{
			name:     "Not ULA fb00::1",
			addr:     "fb00::1",
			expected: false,
		},
		{
			name:     "Not ULA fe00::1",
			addr:     "fe00::1",
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			addr := netip.MustParseAddr(tt.addr)
			result := isULA(addr)
			if result != tt.expected {
				t.Errorf("isULA(%s) = %v, want %v", tt.addr, result, tt.expected)
			}
		})
	}
}

func TestIsLinkLocal(t *testing.T) {
	tests := []struct {
		name     string
		addr     string
		expected bool
	}{
		{
			name:     "Link-local fe80::1",
			addr:     "fe80::1",
			expected: true,
		},
		{
			name:     "Link-local fe80::abcd:1234",
			addr:     "fe80::abcd:1234",
			expected: true,
		},
		{
			name:     "GUA 2001:db8::1",
			addr:     "2001:db8::1",
			expected: false,
		},
		{
			name:     "ULA fd00::1",
			addr:     "fd00::1",
			expected: false,
		},
		{
			name:     "Not link-local fec0::1",
			addr:     "fec0::1",
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			addr := netip.MustParseAddr(tt.addr)
			result := isLinkLocal(addr)
			if result != tt.expected {
				t.Errorf("isLinkLocal(%s) = %v, want %v", tt.addr, result, tt.expected)
			}
		})
	}
}

func TestRAReceiverSource(t *testing.T) {
	r := NewRAReceiver("eth0")
	if r.Source() != SourceRouterAdvertisement {
		t.Errorf("Source() = %v, want %v", r.Source(), SourceRouterAdvertisement)
	}
}

func TestRAReceiverInitialState(t *testing.T) {
	r := NewRAReceiver("eth0")

	if r.CurrentPrefix() != nil {
		t.Error("Expected CurrentPrefix() to be nil initially")
	}

	// Events channel should be available
	if r.Events() == nil {
		t.Error("Expected Events() channel to be non-nil")
	}
}

func TestRAReceiverEventChannel(t *testing.T) {
	r := NewRAReceiver("eth0")

	// Verify the event channel is buffered
	events := r.Events()
	if cap(events) != 10 {
		t.Errorf("Events channel capacity = %d, want 10", cap(events))
	}
}

func TestRAReceiver_sendRouterSolicitation(t *testing.T) {
	r := NewRAReceiver("eth0")
	conn := &fakeNDPConn{}
	r.conn = conn

	hwAddr := net.HardwareAddr{0x02, 0x42, 0xac, 0x11, 0x00, 0x02}
	if err := r.sendRouterSolicitation(hwAddr); err != nil {
		t.Fatalf("sendRouterSolicitation() error = %v", err)
	}

	conn.mu.Lock()
	defer conn.mu.Unlock()

	if len(conn.messages) != 1 {
		t.Fatalf("sent %d messages, want 1", len(conn.messages))
	}
	if conn.destinations[0] != allRoutersMulticast {
		t.Fatalf("destination = %s, want %s", conn.destinations[0], allRoutersMulticast)
	}
	if len(conn.writeDeadlines) != 1 {
		t.Fatalf("write deadlines = %d, want 1", len(conn.writeDeadlines))
	}

	rs, ok := conn.messages[0].(*ndp.RouterSolicitation)
	if !ok {
		t.Fatalf("message = %T, want *ndp.RouterSolicitation", conn.messages[0])
	}
	if len(rs.Options) != 1 {
		t.Fatalf("options = %d, want 1", len(rs.Options))
	}

	lla, ok := rs.Options[0].(*ndp.LinkLayerAddress)
	if !ok {
		t.Fatalf("option = %T, want *ndp.LinkLayerAddress", rs.Options[0])
	}
	if lla.Direction != ndp.Source {
		t.Fatalf("direction = %v, want %v", lla.Direction, ndp.Source)
	}
	if !bytes.Equal(lla.Addr, hwAddr) {
		t.Fatalf("link-layer address = %s, want %s", lla.Addr, hwAddr)
	}
}

func TestRAReceiver_sendRouterSolicitationWithoutHardwareAddress(t *testing.T) {
	r := NewRAReceiver("eth0")
	conn := &fakeNDPConn{}
	r.conn = conn

	if err := r.sendRouterSolicitation(nil); err != nil {
		t.Fatalf("sendRouterSolicitation() error = %v", err)
	}

	conn.mu.Lock()
	defer conn.mu.Unlock()

	rs, ok := conn.messages[0].(*ndp.RouterSolicitation)
	if !ok {
		t.Fatalf("message = %T, want *ndp.RouterSolicitation", conn.messages[0])
	}
	if len(rs.Options) != 0 {
		t.Fatalf("options = %d, want 0", len(rs.Options))
	}
}

func TestRAReceiver_sendInitialRouterSolicitationsStopsAfterPrefix(t *testing.T) {
	r := NewRAReceiver("eth0")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.ctx = ctx
	r.stopCh = make(chan struct{})
	r.maxRouterSolicitations = 3
	r.routerSolicitationInterval = time.Millisecond

	conn := &fakeNDPConn{}
	conn.afterWrite = func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.currentPrefix = &Prefix{Network: netip.MustParsePrefix("2001:db8::/48")}
	}
	r.conn = conn

	r.sendInitialRouterSolicitations(nil)

	if got := conn.messageCount(); got != 1 {
		t.Fatalf("sent %d Router Solicitations, want 1", got)
	}
}

func TestRAReceiver_sendInitialRouterSolicitationsUsesMaxAttempts(t *testing.T) {
	r := NewRAReceiver("eth0")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.ctx = ctx
	r.stopCh = make(chan struct{})
	r.maxRouterSolicitations = 3
	r.routerSolicitationInterval = time.Millisecond
	r.conn = &fakeNDPConn{}

	r.sendInitialRouterSolicitations(nil)

	conn := r.conn.(*fakeNDPConn)
	if got := conn.messageCount(); got != 3 {
		t.Fatalf("sent %d Router Solicitations, want 3", got)
	}
}

type fakeNDPConn struct {
	mu             sync.Mutex
	messages       []ndp.Message
	destinations   []netip.Addr
	writeDeadlines []time.Time
	writeErr       error
	afterWrite     func()
}

func (c *fakeNDPConn) Close() error { return nil }

func (c *fakeNDPConn) ReadFrom() (ndp.Message, *ipv6.ControlMessage, netip.Addr, error) {
	return nil, nil, netip.Addr{}, errors.New("not implemented")
}

func (c *fakeNDPConn) SetReadDeadline(time.Time) error { return nil }

func (c *fakeNDPConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writeDeadlines = append(c.writeDeadlines, t)
	return nil
}

func (c *fakeNDPConn) WriteTo(m ndp.Message, _ *ipv6.ControlMessage, dst netip.Addr) error {
	c.mu.Lock()
	c.messages = append(c.messages, m)
	c.destinations = append(c.destinations, dst)
	writeErr := c.writeErr
	afterWrite := c.afterWrite
	c.mu.Unlock()

	if afterWrite != nil {
		afterWrite()
	}

	return writeErr
}

func (c *fakeNDPConn) messageCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.messages)
}
