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
	"fmt"
	"net/netip"
	"slices"
	"strings"
)

// Policy is the source-agnostic rule for which acquired prefixes are accepted.
// It is fixed for a receiver's lifetime, which is why receivers are pooled per
// interface *and* policy and rebuilt when the spec changes.
type Policy struct {
	// RequireGlobalUnicast rejects anything outside 2000::/3.
	RequireGlobalUnicast bool
	// MinPrefixLength and MaxPrefixLength bound the accepted length in bits.
	// Zero means unbounded at that end.
	MinPrefixLength int
	MaxPrefixLength int
}

// DefaultPolicy is what a spec that says nothing means.
func DefaultPolicy() Policy {
	return Policy{RequireGlobalUnicast: true}
}

// Validate reports whether a prefix may be used, and why not if it may not.
func (p Policy) Validate(network netip.Prefix) error {
	if err := ValidateDelegatedPrefix(network, p.RequireGlobalUnicast); err != nil {
		return err
	}
	return p.checkLength(network.Bits())
}

// checkLength applies the length bounds on their own, for the receive paths
// that can screen a prefix before building anything from it.
func (p Policy) checkLength(bits int) error {
	if p.MinPrefixLength > 0 && bits < p.MinPrefixLength {
		return fmt.Errorf("prefix length /%d is shorter than the configured minimum /%d", bits, p.MinPrefixLength)
	}
	if p.MaxPrefixLength > 0 && bits > p.MaxPrefixLength {
		return fmt.Errorf("prefix length /%d is longer than the configured maximum /%d", bits, p.MaxPrefixLength)
	}
	return nil
}

// Key identifies a policy, so receivers sharing an interface are only shared
// when they would apply the same rules to what arrives on it.
func (p Policy) Key() string {
	return fmt.Sprintf("gua=%t|min=%d|max=%d", p.RequireGlobalUnicast, p.MinPrefixLength, p.MaxPrefixLength)
}

// RAPolicy adds the rules that only apply to Router Advertisements, which are
// unsolicited and can come from anything on the link.
type RAPolicy struct {
	Policy

	// TrustedRouters is the set of link-local source addresses whose
	// advertisements are believed. Empty means any on-link router that passes
	// the RFC 4861 checks.
	TrustedRouters []netip.Addr
}

// DefaultRAPolicy is what a spec that says nothing means.
func DefaultRAPolicy() RAPolicy {
	return RAPolicy{Policy: DefaultPolicy()}
}

// Trusts reports whether an advertisement's source may be believed.
func (p RAPolicy) Trusts(source netip.Addr) bool {
	if len(p.TrustedRouters) == 0 {
		return true
	}
	// Compare unzoned: the source address as read off the socket carries no
	// zone, and the interface is already fixed by the receiver.
	source = source.WithZone("")
	return slices.ContainsFunc(p.TrustedRouters, func(trusted netip.Addr) bool {
		return trusted == source
	})
}

// Key identifies an RA policy for the receiver pool.
func (p RAPolicy) Key() string {
	if len(p.TrustedRouters) == 0 {
		return p.Policy.Key() + "|trusted="
	}
	// Sorted, so two specs listing the same routers in different orders share a
	// receiver instead of opening a second socket on the same interface.
	routers := make([]string, 0, len(p.TrustedRouters))
	for _, addr := range p.TrustedRouters {
		routers = append(routers, addr.String())
	}
	slices.Sort(routers)
	return p.Policy.Key() + "|trusted=" + strings.Join(routers, ",")
}

// ParseTrustedRouters turns the configured strings into addresses, rejecting
// anything that is not a link-local unicast address: an advertisement's source
// is always one, so any other value could never match and would silently
// discard every advertisement on the link.
func ParseTrustedRouters(values []string) ([]netip.Addr, error) {
	if len(values) == 0 {
		return nil, nil
	}

	routers := make([]netip.Addr, 0, len(values))
	for _, value := range values {
		addr, err := netip.ParseAddr(value)
		if err != nil {
			return nil, fmt.Errorf("trusted router %q is not an IP address: %w", value, err)
		}
		if addr.Zone() != "" {
			return nil, fmt.Errorf("trusted router %q must not carry a zone; the interface is set on the acquisition spec", value)
		}
		if !addr.Is6() || addr.Is4In6() {
			return nil, fmt.Errorf("trusted router %q is not an IPv6 address", value)
		}
		if !addr.IsLinkLocalUnicast() {
			return nil, fmt.Errorf("trusted router %q is not link-local; a Router Advertisement is sent from the router's fe80:: address on this link", value)
		}
		routers = append(routers, addr)
	}
	return routers, nil
}
