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

package controller

import (
	"net/netip"
	"strings"
)

// Ownership records.
//
// The operator writes entries into fields it does not exclusively own:
// lbipam.cilium.io/ips on Services, spec.blocks on CiliumLoadBalancerIPPools and
// spec.externalCIDRs on CiliumCIDRGroups all mix operator-managed entries with
// static ones the user supplied. Every reconcile therefore has to answer "which
// of these entries are mine?" before it can replace them.
//
// That question used to be answered geometrically, by testing each entry against
// the currently managed prefixes (current + Status.History). That is wrong at
// exactly one moment, and it is a moment that arrives on every rotation: when a
// prefix falls out of the history window, the address the operator itself wrote
// for it stops matching any managed prefix and is reclassified as a user's static
// address -- and preserved forever. One entry leaked per object per rotation,
// without bound. In a live cluster that reached 109 pool blocks and 108 requested
// IPs on a single Service, which starved Cilium's L2 announcer and black-holed
// IPv6 for most Services while DNS still looked perfect.
//
// Geometry cannot answer the question, because after eviction there is nothing
// left in the object to distinguish "an address I wrote three rotations ago" from
// "an address the user pinned". So the operator records what it wrote instead.
// Each reconcile diffs against that record, which is exact, needs no prefix math,
// and cannot be confused by an address that merely looks operator-shaped. That
// last point is not hypothetical: the ULA addresses users pin for stable internal
// access (fdb3:...::ffff:0:2) share the reserved host-suffix range with the
// operator's own addresses, so any structural heuristic would have to special-case
// them or silently delete them.
//
// The record is written in the same Update call as the field it describes, so the
// two cannot diverge. When it is absent -- a Service the new code has not touched
// yet -- callers fall back to the legacy prefix test, which preserves too much
// rather than too little. The leak stops from the first reconcile onward; entries
// leaked before the upgrade are not retroactively adopted and need a one-time
// cleanup.
const (
	// AnnotationManagedIPs records the addresses the operator last wrote into
	// lbipam.cilium.io/ips on a Service.
	AnnotationManagedIPs = "dynamic-prefix.io/managed-ips"

	// AnnotationManagedTargets records the entries the operator last wrote into
	// external-dns.alpha.kubernetes.io/target on a Service. That field holds only
	// the current address, so it survives an ordinary rotation without a record --
	// the address it carries is always still within the history window when the
	// next reconcile rewrites it. It does not survive an outage: if the operator is
	// down for more than maxPrefixHistory rotations, the address it left behind has
	// aged out by the time it returns, and the legacy prefix test would preserve it
	// as a user's static target. Same leak, rarer trigger.
	AnnotationManagedTargets = "dynamic-prefix.io/managed-targets"

	// AnnotationManagedBlocks records the pool blocks the operator last wrote into
	// spec.blocks, as canonical keys (see blockKey).
	AnnotationManagedBlocks = "dynamic-prefix.io/managed-blocks"

	// AnnotationManagedCIDRs records the CIDRs the operator last wrote into
	// spec.externalCIDRs on a CiliumCIDRGroup.
	AnnotationManagedCIDRs = "dynamic-prefix.io/managed-cidrs"
)

// ownershipRecord is the set of entries the operator wrote on its previous pass.
type ownershipRecord struct {
	entries map[string]struct{}
	// present distinguishes "the operator has never recorded anything here" from
	// "the operator recorded an empty set". Only the former may fall back to the
	// legacy prefix test; the latter is authoritative and means nothing is owned.
	present bool
}

// parseOwnershipRecord reads a record from an annotation value. The second return
// reports whether the annotation existed at all, which callers use to decide
// between the record and the legacy prefix test.
func parseOwnershipRecord(value string, exists bool) ownershipRecord {
	rec := ownershipRecord{entries: make(map[string]struct{}), present: exists}
	if !exists {
		return rec
	}
	for _, raw := range strings.Split(value, ",") {
		if item := strings.TrimSpace(raw); item != "" {
			rec.entries[item] = struct{}{}
		}
	}
	return rec
}

// owns reports whether the operator recorded this entry on its previous pass.
func (r ownershipRecord) owns(entry string) bool {
	_, ok := r.entries[entry]
	return ok
}

// formatOwnershipRecord serialises the entries the operator is writing now. The
// order mirrors the field being written so the annotation stays diffable by eye.
func formatOwnershipRecord(entries []string) string {
	return strings.Join(entries, ",")
}

// blockKey returns a canonical, comparable identity for a pool block. Blocks are
// either {cidr} or {start, stop}; both shapes collapse to a single string so the
// record can be a flat list. Unrecognised shapes yield "" and are never treated
// as owned, which keeps them preserved.
func blockKey(block map[string]interface{}) string {
	if cidr, ok := block["cidr"].(string); ok && cidr != "" {
		return "cidr=" + cidr
	}
	start, hasStart := block["start"].(string)
	stop, hasStop := block["stop"].(string)
	if hasStart && start != "" {
		if hasStop && stop != "" {
			return "range=" + start + "-" + stop
		}
		return "range=" + start
	}
	return ""
}

// isOwnedBlock reports whether a pool block is the operator's. Mirrors
// preserveUnownedIPs: the record decides when present, the legacy geometric test
// only covers pools this code has not written yet. A block whose shape yields no
// key is never owned, so unrecognised entries stay preserved.
func isOwnedBlock(block map[string]interface{}, record ownershipRecord, managedPrefixes []netip.Prefix) bool {
	if !record.present {
		return isManagedBlock(block, managedPrefixes)
	}
	key := blockKey(block)
	if key == "" {
		return false
	}
	return record.owns(key)
}

// isOwnedCIDR reports whether an externalCIDRs entry is the operator's, with the
// same record-first, geometry-as-fallback rule as isOwnedBlock.
func isOwnedCIDR(cidr string, record ownershipRecord, managedPrefixes []netip.Prefix) bool {
	if !record.present {
		p, err := netip.ParsePrefix(cidr)
		if err != nil {
			// Unparseable entries are never ours; preserve to avoid data loss.
			return false
		}
		return isPrefixManaged(p, managedPrefixes)
	}
	return record.owns(cidr)
}

// dedupePreservingOrder drops repeat entries while keeping first-seen order. The
// operator's calculated entries are appended to preserved ones, and a user who
// pins an address the operator also manages would otherwise produce a duplicate
// that Cilium rejects.
func dedupePreservingOrder(items []string) []string {
	seen := make(map[string]struct{}, len(items))
	out := make([]string, 0, len(items))
	for _, item := range items {
		if _, ok := seen[item]; ok {
			continue
		}
		seen[item] = struct{}{}
		out = append(out, item)
	}
	return out
}
