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
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/mdlayher/ndp"
)

func TestValidateDelegatedPrefix(t *testing.T) {
	tests := []struct {
		name       string
		prefix     string
		requireGUA bool
		wantErr    bool
	}{
		{name: "global unicast /64", prefix: "2001:db8::/64", requireGUA: true},
		{name: "global unicast /56", prefix: "2001:db8:abcd::/56", requireGUA: true},
		{name: "unique-local rejected when GUA required", prefix: "fd00::/64", requireGUA: true, wantErr: true},
		{name: "unique-local allowed when GUA not required", prefix: "fd00::/64", requireGUA: false},
		{name: "link-local always rejected", prefix: "fe80::/64", requireGUA: false, wantErr: true},
		{name: "loopback rejected", prefix: "::1/128", requireGUA: false, wantErr: true},
		{name: "unspecified rejected", prefix: "::/128", requireGUA: false, wantErr: true},
		{name: "multicast rejected", prefix: "ff00::/8", requireGUA: false, wantErr: true},
		// A /0 would make every address on earth look operator-managed.
		{name: "zero-length rejected", prefix: "::/0", requireGUA: false, wantErr: true},
		{name: "IPv4 rejected", prefix: "192.0.2.0/24", requireGUA: false, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateDelegatedPrefix(netip.MustParsePrefix(tt.prefix), tt.requireGUA)
			if tt.wantErr && err == nil {
				t.Errorf("ValidateDelegatedPrefix(%s, %v) = nil, want error", tt.prefix, tt.requireGUA)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("ValidateDelegatedPrefix(%s, %v) = %v, want nil", tt.prefix, tt.requireGUA, err)
			}
		})
	}
}

// ulaOnlyRouterAdvertisement is the advertisement shape that has no global prefix
// at all. Every existing fixture pairs a global prefix with a unique-local one, so
// the global one always wins and the fallback branch is never reached -- which is
// how a unique-local prefix could be accepted without any test noticing.
func ulaOnlyRouterAdvertisement() *ndp.RouterAdvertisement {
	return &ndp.RouterAdvertisement{Options: []ndp.Option{
		&ndp.PrefixInformation{
			Prefix:                         netip.MustParseAddr("fd00::"),
			PrefixLength:                   64,
			OnLink:                         true,
			AutonomousAddressConfiguration: true,
			ValidLifetime:                  2 * time.Hour,
			PreferredLifetime:              time.Hour,
		},
	}}
}

func TestRAReceiverRejectsULAOnlyAdvertisement(t *testing.T) {
	r := NewRAReceiver("eth0") // defaults to requiring global unicast

	r.handleRouterAdvertisement(ulaOnlyRouterAdvertisement())

	if got := r.CurrentPrefix(); got != nil {
		t.Fatalf("CurrentPrefix() = %v, want nil: a unique-local prefix must not be "+
			"accepted as a delegation", got.Network)
	}
}

func TestRAReceiverAcceptsULAOnlyWhenPolicyAllows(t *testing.T) {
	r := NewRAReceiverWithPolicy("eth0", false)

	r.handleRouterAdvertisement(ulaOnlyRouterAdvertisement())

	got := r.CurrentPrefix()
	if got == nil {
		t.Fatal("CurrentPrefix() = nil, want the unique-local prefix when the policy allows it")
	}
	if want := netip.MustParsePrefix("fd00::/64"); got.Network != want {
		t.Errorf("CurrentPrefix() = %v, want %v", got.Network, want)
	}
}

// A unique-local prefix arriving after a global one must not displace it. This is
// the sequence that matters in practice: the delegation is acquired normally, then
// an advertisement without the global prefix arrives.
func TestRAReceiverKeepsGlobalPrefixWhenULAOnlyArrives(t *testing.T) {
	r := NewRAReceiver("eth0")

	r.handleRouterAdvertisement(testRouterAdvertisement())
	first := r.CurrentPrefix()
	if first == nil {
		t.Fatal("CurrentPrefix() = nil after a global advertisement")
	}

	r.handleRouterAdvertisement(ulaOnlyRouterAdvertisement())

	got := r.CurrentPrefix()
	if got == nil || got.Network != first.Network {
		t.Fatalf("CurrentPrefix() = %v, want it to stay %v", got, first.Network)
	}
}

// A DHCPv6 server is not obliged to send a prefix with its host bits cleared, and
// unmasked bits would flow into status, into change detection, and into every
// address derived from the prefix. The RA path has always masked; this one did not.
func TestProcessIAPDReplyMasksHostBits(t *testing.T) {
	iaid := [4]byte{4, 4, 4, 4}
	// A /56 whose low bits are set well past the prefix length: the 4th group's
	// low 8 bits (0x00ff) and the interface identifier both sit outside the /56.
	dirty := decodeIAPrefix(t, iaPrefixBytes(1800, 3600, 56, net.ParseIP("2001:db8:1:20ff::1")))

	r := NewDHCPv6PDReceiver("eth0", 56)
	if err := r.processIAPDReply(replyWithPrefixes(iaid, dirty), iaid, nil); err != nil {
		t.Fatalf("processIAPDReply() unexpected error: %v", err)
	}

	got := r.CurrentPrefix()
	if got == nil {
		t.Fatal("expected a prefix to be recorded")
	}
	if want := "2001:db8:1:2000::/56"; got.Network.String() != want {
		t.Errorf("CurrentPrefix() = %v, want %v (bits below the prefix length must be masked off)",
			got.Network, want)
	}
}

func TestProcessIAPDReplyRejectsULAWhenGUARequired(t *testing.T) {
	iaid := [4]byte{5, 5, 5, 5}
	ula := decodeIAPrefix(t, iaPrefixBytes(1800, 3600, 56, net.ParseIP("fd00::")))

	r := NewDHCPv6PDReceiver("eth0", 56) // defaults to requiring global unicast
	if err := r.processIAPDReply(replyWithPrefixes(iaid, ula), iaid, nil); err == nil {
		t.Fatal("processIAPDReply() = nil, want an error for a unique-local delegation")
	}
	if got := r.CurrentPrefix(); got != nil {
		t.Errorf("CurrentPrefix() = %v, want nil", got.Network)
	}
}

func TestSharedRAPoolSeparatesReceiversByPolicy(t *testing.T) {
	created := 0
	pool := newSharedRAReceiverPool(func(iface string, requireGlobalUnicast bool) Receiver {
		created++
		return NewRAReceiverWithPolicy(iface, requireGlobalUnicast)
	})

	// Same interface, same policy: one receiver, shared.
	pool.subscribe("eth0", true)
	pool.subscribe("eth0", true)
	if created != 1 {
		t.Fatalf("created %d receivers for one interface and policy, want 1", created)
	}

	// Same interface, different policy: a second receiver, because the policy is
	// baked in and the stricter subscriber must not see what the looser one accepts.
	pool.subscribe("eth0", false)
	if created != 2 {
		t.Fatalf("created %d receivers after adding a second policy, want 2", created)
	}
}
