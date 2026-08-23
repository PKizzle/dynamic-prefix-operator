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
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv6"
)

func TestNextBackoff(t *testing.T) {
	tests := []struct {
		name string
		cur  time.Duration
		max  time.Duration
		want time.Duration
	}{
		{name: "doubles below the ceiling", cur: 10 * time.Second, max: 5 * time.Minute, want: 20 * time.Second},
		{name: "stops at the ceiling", cur: 4 * time.Minute, max: 5 * time.Minute, want: 5 * time.Minute},
		{name: "stays at the ceiling", cur: 5 * time.Minute, max: 5 * time.Minute, want: 5 * time.Minute},
		{name: "never exceeds the ceiling from above", cur: time.Hour, max: 5 * time.Minute, want: 5 * time.Minute},
		{
			// Doubling a duration near the end of the range wraps to negative,
			// which as a timer argument fires immediately -- the busy loop the
			// ceiling exists to prevent.
			name: "does not overflow",
			cur:  time.Duration(1) << 62,
			max:  5 * time.Minute,
			want: 5 * time.Minute,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := nextBackoff(tt.cur, tt.max); got != tt.want {
				t.Errorf("nextBackoff(%v, %v) = %v, want %v", tt.cur, tt.max, got, tt.want)
			}
		})
	}
}

// Jitter keeps a fleet of operators that lost the same uplink from soliciting
// in lockstep when it returns.
func TestJitteredStaysWithinBounds(t *testing.T) {
	const base = time.Minute
	low, high := base-base/5, base+base/5

	sawSpread := false
	for range 200 {
		got := jittered(base)
		if got < low || got > high {
			t.Fatalf("jittered(%v) = %v, want it within [%v, %v]", base, got, low, high)
		}
		if got != base {
			sawSpread = true
		}
	}
	if !sawSpread {
		t.Error("jittered() returned the same delay every time; nothing is being spread")
	}

	if got := jittered(0); got != 0 {
		t.Errorf("jittered(0) = %v, want 0", got)
	}
}

// Without backoff, a failing acquisition retries every ten seconds for as long
// as the uplink stays down -- an exchange a minute forever against a server
// that has already refused, or none at all.
func TestDHCPv6PDBacksOffWhileAcquisitionKeepsFailing(t *testing.T) {
	attempts := make(chan struct{}, 8)
	r, _ := newWireTestReceiver(t, func(*dhcpv6.Message) (*dhcpv6.Message, error) {
		select {
		case attempts <- struct{}{}:
		default:
		}
		return nil, errors.New("no server answered")
	})
	// Long enough that a second attempt inside the window means the loop is not
	// waiting at all, short enough not to slow the suite down.
	r.acquireBackoffMin = 2 * time.Second
	r.acquireBackoffMax = 4 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := r.Start(ctx); err != nil {
		t.Fatalf("Start() = %v", err)
	}
	t.Cleanup(func() { _ = r.Stop() })

	select {
	case <-attempts:
	case <-time.After(2 * time.Second):
		t.Fatal("the receiver never attempted an acquisition")
	}

	select {
	case <-attempts:
		t.Fatal("a second attempt arrived immediately; the loop is retrying without backing off")
	case <-time.After(500 * time.Millisecond):
	}
}

// Backing off must not outlive the failure: once the uplink returns, the next
// rotation has to be picked up promptly rather than at the grown interval.
func TestAcquireBackoffResetsOnSuccess(t *testing.T) {
	b := newAcquireBackoff(10*time.Second, time.Minute)

	if got := b.current(); got != 10*time.Second {
		t.Fatalf("initial delay = %v, want the minimum", got)
	}

	b.failed()
	b.failed()
	if got := b.current(); got != 40*time.Second {
		t.Fatalf("delay after two failures = %v, want 40s", got)
	}

	b.succeeded()
	if got := b.current(); got != 10*time.Second {
		t.Fatalf("delay after a success = %v, want it back at the minimum", got)
	}

	for range 10 {
		b.failed()
	}
	if got := b.current(); got != time.Minute {
		t.Fatalf("delay after ten failures = %v, want it capped at a minute", got)
	}
}

// A receiver whose uplink comes back has to acquire, not stay stuck in the
// retry branch it entered while the uplink was down.
func TestDHCPv6PDAcquiresOnceTheServerStartsAnswering(t *testing.T) {
	var mu sync.Mutex
	failing := true

	r, _ := newWireTestReceiver(t, func(req *dhcpv6.Message) (*dhcpv6.Message, error) {
		mu.Lock()
		down := failing
		mu.Unlock()
		if down {
			return nil, errors.New("no server answered")
		}
		return delegating(testDelegatedPrefix)(req)
	})
	r.acquireBackoffMin = 10 * time.Millisecond
	r.acquireBackoffMax = 50 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := r.Start(ctx); err != nil {
		t.Fatalf("Start() = %v", err)
	}
	t.Cleanup(func() { _ = r.Stop() })

	// Let it fail a few times so the delay has grown before the server returns.
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	failing = false
	mu.Unlock()

	deadline := time.After(3 * time.Second)
	for {
		select {
		case e := <-r.Events():
			if e.Type == EventTypeAcquired {
				if got := r.CurrentPrefix(); got == nil || got.Network.String() != testDelegatedPrefix {
					t.Fatalf("acquired prefix = %v, want %s", got, testDelegatedPrefix)
				}
				return
			}
		case <-deadline:
			t.Fatal("no prefix was acquired after the server started answering")
		}
	}
}
