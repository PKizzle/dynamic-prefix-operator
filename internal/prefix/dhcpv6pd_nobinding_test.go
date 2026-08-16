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
	"github.com/insomniacslk/dhcp/iana"
)

// noBindingReply answers as a server that has forgotten the delegation does --
// after a reboot, a configuration change, or a lease database it no longer has.
func noBindingReply(onMessage bool) respondFunc {
	return func(req *dhcpv6.Message) (*dhcpv6.Message, error) {
		status := &dhcpv6.OptStatusCode{
			StatusCode:    iana.StatusNoBinding,
			StatusMessage: "no binding for that IA_PD",
		}
		if onMessage {
			iapd, err := iaPDFor(req, testDelegatedPrefix, leaseLifetime/2)
			if err != nil {
				return nil, err
			}
			return answer(req, dhcpv6.MessageTypeReply, iapd, status), nil
		}
		iapd, err := iaPDFor(req, "", leaseLifetime/2, status)
		if err != nil {
			return nil, err
		}
		return answer(req, dhcpv6.MessageTypeReply, iapd), nil
	}
}

func TestDHCPv6PDRenewRecognisesNoBinding(t *testing.T) {
	for _, tt := range []struct {
		name      string
		onMessage bool
	}{
		{name: "status on the association", onMessage: false},
		{name: "status on the message", onMessage: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var mu sync.Mutex
			forgotten := false

			r, _ := newWireTestReceiver(t, func(req *dhcpv6.Message) (*dhcpv6.Message, error) {
				mu.Lock()
				gone := forgotten
				mu.Unlock()
				if gone && req.MessageType == dhcpv6.MessageTypeRenew {
					return noBindingReply(tt.onMessage)(req)
				}
				return delegating(testDelegatedPrefix)(req)
			})

			if err := r.acquirePrefix(context.Background()); err != nil {
				t.Fatalf("acquirePrefix() = %v", err)
			}
			mu.Lock()
			forgotten = true
			mu.Unlock()

			err := r.renewPrefix(context.Background())
			if err == nil {
				t.Fatal("expected the renewal to fail")
			}
			if !errors.Is(err, errNoBinding) {
				t.Fatalf("error = %v, want it to be recognisable as %v", err, errNoBinding)
			}
		})
	}
}

// A server that says it holds no binding will say the same thing at T2, so
// waiting out the renewal window before rebinding -- and rebinding on a lease
// the server has already disowned -- just delays the SOLICIT that has to
// happen anyway. Until then the operator kept serving the delegated prefix for
// the rest of the renewal window even though nothing upstream still routed it.
func TestDHCPv6PDReSolicitsAfterNoBinding(t *testing.T) {
	var mu sync.Mutex
	forgotten := false
	const replacement = "2001:db8:f00d::/56"

	r, _ := newWireTestReceiver(t, func(req *dhcpv6.Message) (*dhcpv6.Message, error) {
		mu.Lock()
		gone := forgotten
		mu.Unlock()

		switch {
		case gone && req.MessageType == dhcpv6.MessageTypeRenew:
			return noBindingReply(false)(req)
		case gone:
			// The re-solicit lands on a server that now delegates something else.
			return delegatingFor(replacement, time.Hour)(req)
		default:
			return delegatingFor(testDelegatedPrefix, 20*time.Millisecond)(req)
		}
	})
	r.acquireBackoffMin = 5 * time.Millisecond
	r.acquireBackoffMax = 20 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := r.Start(ctx); err != nil {
		t.Fatalf("Start() = %v", err)
	}
	t.Cleanup(func() { _ = r.Stop() })

	// Wait for the first delegation, then take the binding away.
	waitForEvent(t, r, EventTypeAcquired, 2*time.Second)
	mu.Lock()
	forgotten = true
	mu.Unlock()

	// The lease must be given up rather than renewed to its bitter end...
	waitForEvent(t, r, EventTypeExpired, 3*time.Second)
	// ...and a fresh exchange must follow.
	waitForEvent(t, r, EventTypeAcquired, 3*time.Second)

	if got := r.CurrentPrefix(); got == nil || got.Network.String() != replacement {
		t.Fatalf("prefix after re-soliciting = %v, want %s", got, replacement)
	}
}

func waitForEvent(t *testing.T, r *DHCPv6PDReceiver, want EventType, within time.Duration) {
	t.Helper()

	deadline := time.After(within)
	for {
		select {
		case e := <-r.Events():
			if e.Type == want {
				return
			}
		case <-deadline:
			t.Fatalf("no %s event arrived within %v", want, within)
		}
	}
}
