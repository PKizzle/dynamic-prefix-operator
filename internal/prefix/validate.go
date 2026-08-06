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
)

// IsGlobalUnicastPrefix reports whether a prefix falls inside 2000::/3.
//
// This is a deliberate exported wrapper around the strict bit test used when
// selecting a prefix from a Router Advertisement. Note that netip's
// Addr.IsGlobalUnicast is NOT a substitute: it returns true for unique-local
// addresses, which is precisely the class this needs to exclude.
func IsGlobalUnicastPrefix(p netip.Prefix) bool {
	return isGlobalUnicast(p.Addr())
}

// IsULAPrefix reports whether a prefix falls inside fc00::/7.
func IsULAPrefix(p netip.Prefix) bool {
	return isULA(p.Addr())
}

// ValidateDelegatedPrefix checks that a prefix is usable as a delegated prefix
// before anything derives addresses from it or records it in status.
//
// Every acquisition source funnels through here, and so does the controller
// before it writes status, because a receiver constructed under an older
// configuration can still be feeding prefixes acquired under the old rules.
//
// requireGlobalUnicast rejects anything outside 2000::/3. Address class is not a
// cosmetic concern: a link commonly advertises more than one prefix, and if the
// global one is momentarily absent a receiver that merely *prefers* global
// unicast will accept a unique-local prefix and rotate it in as though the
// delegation had changed. Every derived address then moves into a range that is
// not routable off-link, and -- worse -- the real prefix ages out of history as
// if it had been retired, so anything keyed on "is this prefix still mine"
// disowns addresses that are still in use.
func ValidateDelegatedPrefix(p netip.Prefix, requireGlobalUnicast bool) error {
	if !p.IsValid() {
		return fmt.Errorf("prefix is not valid")
	}
	if !p.Addr().Is6() || p.Addr().Is4In6() {
		return fmt.Errorf("prefix %s is not IPv6", p)
	}
	if p.Bits() <= 0 {
		// A /0 covers the entire address space; accepting one would make every
		// address on earth look operator-managed.
		return fmt.Errorf("prefix %s has a zero prefix length", p)
	}
	if p.Bits() > p.Addr().BitLen() {
		return fmt.Errorf("prefix %s has an out-of-range prefix length", p)
	}
	if p.Addr().IsLoopback() || p.Addr().IsMulticast() || p.Addr().IsUnspecified() {
		return fmt.Errorf("prefix %s is not a unicast prefix", p)
	}
	if p.Addr().IsLinkLocalUnicast() {
		return fmt.Errorf("prefix %s is link-local", p)
	}
	if requireGlobalUnicast && !isGlobalUnicast(p.Addr()) {
		if isULA(p.Addr()) {
			return fmt.Errorf("prefix %s is unique-local, not global unicast "+
				"(set acquisition.prefixFilter.requireGlobalUnicast=false to allow)", p)
		}
		return fmt.Errorf("prefix %s is not global unicast "+
			"(set acquisition.prefixFilter.requireGlobalUnicast=false to allow)", p)
	}
	return nil
}
