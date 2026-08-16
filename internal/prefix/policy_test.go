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
	"strings"
	"testing"

	"k8s.io/utils/ptr"

	dynamicprefixiov1alpha1 "github.com/pkizzle/dynamic-prefix-operator/api/v1alpha1"
)

func TestPolicyValidatesPrefixLengthBounds(t *testing.T) {
	tests := []struct {
		name    string
		policy  Policy
		prefix  string
		wantErr string
	}{
		{
			name:   "no bounds accepts anything otherwise valid",
			policy: DefaultPolicy(),
			prefix: "2001:db8::/56",
		},
		{
			name:    "shorter than the minimum is rejected",
			policy:  Policy{RequireGlobalUnicast: true, MinPrefixLength: 56},
			prefix:  "2001:db8::/48",
			wantErr: "shorter than the configured minimum",
		},
		{
			name:   "exactly the minimum is accepted",
			policy: Policy{RequireGlobalUnicast: true, MinPrefixLength: 56},
			prefix: "2001:db8::/56",
		},
		{
			name:    "longer than the maximum is rejected",
			policy:  Policy{RequireGlobalUnicast: true, MaxPrefixLength: 60},
			prefix:  "2001:db8::/64",
			wantErr: "longer than the configured maximum",
		},
		{
			name:   "exactly the maximum is accepted",
			policy: Policy{RequireGlobalUnicast: true, MaxPrefixLength: 64},
			prefix: "2001:db8::/64",
		},
		{
			name:   "inside both bounds is accepted",
			policy: Policy{RequireGlobalUnicast: true, MinPrefixLength: 48, MaxPrefixLength: 64},
			prefix: "2001:db8::/56",
		},
		{
			// The length bounds do not replace the checks that were already
			// there; a policy that only bounds the length still rejects a
			// unique-local prefix.
			name:    "the existing address-class rule still applies",
			policy:  Policy{RequireGlobalUnicast: true, MaxPrefixLength: 64},
			prefix:  "fd00::/56",
			wantErr: "unique-local",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.policy.Validate(netip.MustParsePrefix(tt.prefix))
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate(%s) = %v, want it accepted", tt.prefix, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate(%s) accepted the prefix, want %q", tt.prefix, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %q, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestRAPolicyTrusts(t *testing.T) {
	router := netip.MustParseAddr("fe80::1")
	named := RAPolicy{TrustedRouters: []netip.Addr{router}}

	anyRouter := RAPolicy{}
	if !anyRouter.Trusts(netip.MustParseAddr("fe80::dead")) {
		t.Error("an empty list must trust any router that passed the other checks")
	}
	if !named.Trusts(router) {
		t.Error("a named router must be trusted")
	}
	if named.Trusts(netip.MustParseAddr("fe80::dead")) {
		t.Error("a router that is not named must not be trusted")
	}
	// The source address read off the socket carries the receiving zone; the
	// configured address cannot, so the comparison has to ignore it or nothing
	// would ever match.
	if !named.Trusts(router.WithZone("eth0")) {
		t.Error("a zone on the received source address must not defeat the match")
	}
}

// Receivers are shared per interface and policy. Two resources that disagree
// about which routers to believe must not end up behind one socket, or the
// stricter one is fed advertisements it rejected.
func TestRAPolicyKeySeparatesDifferentTrust(t *testing.T) {
	base := DefaultRAPolicy()
	withRouter := RAPolicy{Policy: base.Policy, TrustedRouters: []netip.Addr{netip.MustParseAddr("fe80::1")}}
	withOther := RAPolicy{Policy: base.Policy, TrustedRouters: []netip.Addr{netip.MustParseAddr("fe80::2")}}
	bounded := RAPolicy{Policy: Policy{RequireGlobalUnicast: true, MinPrefixLength: 56}}

	keys := map[string]string{
		"default":  base.Key(),
		"router 1": withRouter.Key(),
		"router 2": withOther.Key(),
		"bounded":  bounded.Key(),
	}
	seen := make(map[string]string, len(keys))
	for name, key := range keys {
		if other, clash := seen[key]; clash {
			t.Errorf("%s and %s share the pool key %q", name, other, key)
		}
		seen[key] = name
	}

	// Order is not a difference, and treating it as one would open a second
	// socket on the same interface for the same configuration.
	a := RAPolicy{Policy: base.Policy, TrustedRouters: []netip.Addr{
		netip.MustParseAddr("fe80::1"), netip.MustParseAddr("fe80::2"),
	}}
	b := RAPolicy{Policy: base.Policy, TrustedRouters: []netip.Addr{
		netip.MustParseAddr("fe80::2"), netip.MustParseAddr("fe80::1"),
	}}
	if a.Key() != b.Key() {
		t.Errorf("the same routers in a different order produced %q and %q", a.Key(), b.Key())
	}
}

func TestParseTrustedRouters(t *testing.T) {
	tests := []struct {
		name    string
		values  []string
		wantErr string
	}{
		{name: "empty is allowed", values: nil},
		{name: "link-local addresses are accepted", values: []string{"fe80::1", "FE80::CAFE"}},
		{
			name:    "a global address could never be an advertisement's source",
			values:  []string{"2001:db8::1"},
			wantErr: "not link-local",
		},
		{
			name:    "an IPv4 address is not a router on an IPv6 link",
			values:  []string{"192.0.2.1"},
			wantErr: "not an IPv6 address",
		},
		{
			name:    "a zone belongs on the acquisition spec, not here",
			values:  []string{"fe80::1%eth0"},
			wantErr: "must not carry a zone",
		},
		{
			name:    "nonsense is reported rather than ignored",
			values:  []string{"not-an-address"},
			wantErr: "not an IP address",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseTrustedRouters(tt.values)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ParseTrustedRouters(%v) = %v", tt.values, err)
				}
				if len(got) != len(tt.values) {
					t.Fatalf("parsed %d routers, want %d", len(got), len(tt.values))
				}
				return
			}
			if err == nil {
				t.Fatalf("ParseTrustedRouters(%v) accepted the value", tt.values)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %q, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

// A trusted-router list that cannot be parsed must fail loudly. Ignoring it
// would leave the receiver believing every router on a link whose owner had
// asked for exactly the opposite.
func TestFactoryRejectsAnUnusableTrustedRouterList(t *testing.T) {
	factory := NewReceiverFactory()

	_, err := factory.CreateReceiver(dynamicprefixiov1alpha1.AcquisitionSpec{
		RouterAdvertisement: &dynamicprefixiov1alpha1.RouterAdvertisementSpec{
			Interface:      "eth0",
			Enabled:        ptr.To(true),
			TrustedRouters: []string{"2001:db8::1"},
		},
	})
	if err == nil {
		t.Fatal("expected a receiver built on an unusable trusted-router list to fail")
	}
	if !strings.Contains(err.Error(), "not link-local") {
		t.Fatalf("error = %q, want it to explain the rejected value", err)
	}
}

func TestPolicyFromSpecCarriesTheLengthBounds(t *testing.T) {
	spec := dynamicprefixiov1alpha1.AcquisitionSpec{
		DHCPv6PD: &dynamicprefixiov1alpha1.DHCPv6PDSpec{Interface: "eth0"},
		PrefixFilter: &dynamicprefixiov1alpha1.PrefixFilterSpec{
			MinPrefixLength: ptr.To(48),
			MaxPrefixLength: ptr.To(60),
		},
	}

	policy := policyFromSpec(spec)
	if policy.MinPrefixLength != 48 || policy.MaxPrefixLength != 60 {
		t.Fatalf("policy = %+v, want the configured bounds", policy)
	}
	// An unset requireGlobalUnicast keeps the safe default rather than becoming
	// the zero value.
	if !policy.RequireGlobalUnicast {
		t.Error("requireGlobalUnicast defaulted to false")
	}
}
