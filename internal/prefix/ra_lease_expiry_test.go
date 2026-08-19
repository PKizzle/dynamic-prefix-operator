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
	"net/netip"
	"testing"
	"time"
)

// A lease nothing renewed within its valid lifetime must be given up, the way
// the DHCPv6-PD path gives up a lease the server stopped extending.
//
// Before this existed the Router Advertisement path had no expiry at all: a
// router that went silent -- replaced, reconfigured, or no longer trusted after
// a trustedRouters change -- left the receiver serving its last prefix forever
// with every condition green.
func TestRALeaseExpiresWhenNothingRenewsIt(t *testing.T) {
	r := NewRAReceiver("eth0")

	r.updatePrefix(netip.MustParsePrefix("2001:db8::/64"), time.Hour, 30*time.Minute)
	if r.CurrentPrefix() == nil {
		t.Fatal("CurrentPrefix() = nil right after acquiring, want the prefix")
	}
	// Drain the acquisition event so the expiry event is unambiguous.
	select {
	case <-r.Events():
	default:
	}

	// Backdate the acquisition past the valid lifetime, which is what a router
	// going quiet looks like from here.
	r.mu.Lock()
	r.currentPrefix.ReceivedAt = time.Now().Add(-2 * time.Hour)
	r.mu.Unlock()

	r.expireLeaseIfStale()

	if got := r.CurrentPrefix(); got != nil {
		t.Errorf("CurrentPrefix() = %v, want nil: a prefix nothing renewed is no longer routed, "+
			"and reporting it as held keeps it in the pools and in DNS", got.Network)
	}
	if r.LastError() == nil {
		t.Error("LastError() = nil, want an error: the expiry is what should turn Degraded on " +
			"and take receiver_healthy to 0")
	}

	select {
	case ev := <-r.Events():
		if ev.Type != EventTypeExpired {
			t.Errorf("event type = %q, want %q", ev.Type, EventTypeExpired)
		}
	default:
		t.Error("no event sent, want an expiry event to wake the reconciler")
	}
}

// The common case: advertisements are still arriving, so nothing expires. Each
// accepted advertisement restamps ReceivedAt, which is what keeps the deadline
// moving.
func TestRALeaseSurvivesWhileAdvertisementsKeepArriving(t *testing.T) {
	r := NewRAReceiver("eth0")
	r.updatePrefix(netip.MustParsePrefix("2001:db8::/64"), time.Hour, 30*time.Minute)

	r.expireLeaseIfStale()

	if r.CurrentPrefix() == nil {
		t.Fatal("CurrentPrefix() = nil, want the prefix: its valid lifetime has not run out")
	}
	if r.LastError() != nil {
		t.Errorf("LastError() = %v, want nil while the lease is still current", r.LastError())
	}
}
