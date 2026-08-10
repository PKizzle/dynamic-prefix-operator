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
// without bound, until the accumulated entries were enough to saturate the
// load-balancer implementation's layer-2 announcements: IPv6 connectivity then
// failed for most Services while DNS and every other layer still looked healthy.
//
// Geometry cannot answer the question, because after eviction there is nothing
// left in the object to distinguish "an address I wrote three rotations ago" from
// "an address the user pinned". So the operator records what it wrote instead.
// Each reconcile diffs against that record, which is exact, needs no prefix math,
// and cannot be confused by an address that merely looks operator-shaped. That
// last point is not hypothetical: a unique-local address pinned for stable
// internal reachability commonly shares the reserved host-suffix range with the
// operator's own addresses, so any structural heuristic would have to special-case
// it or silently delete it.
//
// The record is written in the same Update call as the field it describes, so the
// two cannot diverge. When it is absent -- an object this code has not touched
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

	// AnnotationManagedAddresses records the entries the operator last wrote into
	// spec.addresses on a MetalLB IPAddressPool.
	AnnotationManagedAddresses = "dynamic-prefix.io/managed-addresses"

	// AnnotationManagedCIDRs records the CIDRs the operator last wrote into
	// spec.externalCIDRs on a CiliumCIDRGroup.
	AnnotationManagedCIDRs = "dynamic-prefix.io/managed-cidrs"

	// AnnotationManagedCIDR records the single CIDR the operator last wrote into
	// spec.cidr on a Calico IPPool.
	//
	// Calico's field holds one scalar rather than a list, so there is nothing to
	// preserve alongside it and the record is not needed to tell the operator's
	// entries from the user's. It is needed to answer the other question: whether
	// the operator put the current value there at all. Without it a released or
	// orphaned IPPool is indistinguishable from one the user wrote by hand, so
	// the watch predicate stopped matching it and the CIDR was left pointing at a
	// prefix nothing maintains.
	AnnotationManagedCIDR = "dynamic-prefix.io/managed-cidr"
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
			rec.entries[canonicalEntry(item)] = struct{}{}
		}
	}
	return rec
}

// owns reports whether the operator recorded this entry on its previous pass.
func (r ownershipRecord) owns(entry string) bool {
	_, ok := r.entries[canonicalEntry(entry)]
	return ok
}

// canonicalEntry normalises an entry so ownership is decided by what an address
// *is*, not by how it happened to be spelled.
//
// Everything the operator writes already comes from netip's String(), so records
// round-trip its own writes regardless. Users are not so constrained: an address
// pinned as 2001:0DB8::0001 is the same address as 2001:db8::1, and comparing the
// two as raw strings both fails to recognise a pin and lets the same address be
// requested twice. Entries that are not addresses or prefixes -- external-dns
// targets may be hostnames -- pass through untouched.
func canonicalEntry(entry string) string {
	if addr, err := netip.ParseAddr(entry); err == nil {
		return addr.String()
	}
	if p, err := netip.ParsePrefix(entry); err == nil {
		return p.String()
	}
	return entry
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
		return "cidr=" + canonicalEntry(cidr)
	}
	// An absent stop and an empty stop describe the same block, so they must key
	// the same way; otherwise the operator writes one shape, reads back the other,
	// and fails to recognise its own entry.
	start, _ := block["start"].(string)
	stop, _ := block["stop"].(string)
	if start != "" {
		if stop != "" {
			return "range=" + canonicalEntry(start) + "-" + canonicalEntry(stop)
		}
		return "range=" + canonicalEntry(start)
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
		return isPrefixSubsetOfManaged(p, managedPrefixes)
	}
	return record.owns(cidr)
}

// isPrefixSubsetOfManaged reports whether a prefix lies entirely inside one of the
// managed prefixes.
//
// Containment must be tested in one direction only. A test that also matched when
// the *entry* contains a managed prefix claims a user's supernet -- pinning the
// whole delegation while the operator manages a subnet of it is an ordinary thing
// to do -- and the fallback path then deletes it. That fires on the first pass
// after an upgrade, on every object that has no ownership record yet, which is the
// worst possible moment for a silent deletion.
func isPrefixSubsetOfManaged(p netip.Prefix, managedPrefixes []netip.Prefix) bool {
	for _, mp := range managedPrefixes {
		if p.Bits() >= mp.Bits() && mp.Contains(p.Addr()) {
			return true
		}
	}
	return false
}

// hasOwnershipRecord reports whether an object still carries anything the
// operator wrote.
//
// Watch predicates use this alongside the dynamic-prefix.io/name check. Matching
// only on that annotation means its removal is invisible: the object stops
// passing the filter, so no event is delivered and the entries the operator had
// written stay behind forever -- including an external-dns target that stops
// resolving at the next rotation. Matching on the records too keeps the object
// watched exactly long enough to hand it back.
func hasOwnershipRecord(annotations map[string]string) bool {
	for _, key := range []string{
		AnnotationManagedIPs,
		AnnotationManagedTargets,
		AnnotationManagedBlocks,
		AnnotationManagedCIDRs,
		AnnotationManagedCIDR,
		AnnotationManagedAddresses,
	} {
		if _, ok := annotations[key]; ok {
			return true
		}
	}
	return false
}

// excludePinned returns the entries the operator may claim: everything it wants
// to write, minus anything the user already had there.
//
// Claiming a user's pin is not harmless. The record grants the operator the right
// to remove an entry once it stops generating it, so an address that happens to
// coincide with a managed one today would be deleted a few rotations from now --
// turning a deliberate pin into a delayed failure with no trace of what removed
// it. The pinned copy is kept in the field either way; it just is not ours.
func excludePinned(candidates, pinned []string) []string {
	if len(pinned) == 0 {
		return candidates
	}
	pinnedKeys := make(map[string]struct{}, len(pinned))
	for _, p := range pinned {
		pinnedKeys[canonicalEntry(strings.TrimSpace(p))] = struct{}{}
	}
	out := make([]string, 0, len(candidates))
	for _, c := range candidates {
		if _, ok := pinnedKeys[canonicalEntry(strings.TrimSpace(c))]; ok {
			continue
		}
		out = append(out, c)
	}
	return out
}

// dedupePreservingOrder drops repeat entries while keeping first-seen order,
// comparing entries by identity rather than spelling. The operator's calculated
// entries are appended to preserved ones, and a user who pins an address the
// operator also manages would otherwise produce a duplicate that Cilium rejects.
func dedupePreservingOrder(items []string) []string {
	seen := make(map[string]struct{}, len(items))
	out := make([]string, 0, len(items))
	for _, item := range items {
		key := canonicalEntry(item)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, item)
	}
	return out
}
