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
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/big"
	"net/netip"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/pkizzle/dynamic-prefix-operator/internal/prefix"
)

type poolBackend interface {
	name() string
	gvk() schema.GroupVersionKind
	// namespaced reports whether this backend's resource is namespace-scoped.
	// A reconcile.Request carries no GVK, so getPool has to probe backends by
	// name -- and the API server ignores the namespace when reading a
	// cluster-scoped resource. Without the scope to discriminate on, a request
	// for a namespaced pool matches a same-named cluster-scoped one.
	namespaced() bool
	update(ctx context.Context, r *PoolSyncReconciler, pool *unstructured.Unstructured, configs []poolConfiguration, managedPrefixes []netip.Prefix) (bool, error)
}

type ciliumLoadBalancerIPPoolBackend struct {
	resourceGVK schema.GroupVersionKind
}

func (b ciliumLoadBalancerIPPoolBackend) name() string { return "cilium-load-balancer-ip-pool" }
func (b ciliumLoadBalancerIPPoolBackend) gvk() schema.GroupVersionKind {
	return b.resourceGVK
}

// CiliumLoadBalancerIPPool is cluster-scoped.
func (b ciliumLoadBalancerIPPoolBackend) namespaced() bool { return false }
func (b ciliumLoadBalancerIPPoolBackend) update(ctx context.Context, r *PoolSyncReconciler, pool *unstructured.Unstructured, configs []poolConfiguration, managedPrefixes []netip.Prefix) (bool, error) {
	return r.updateLoadBalancerIPPool(ctx, pool, configs, managedPrefixes)
}

type ciliumCIDRGroupBackend struct {
	resourceGVK schema.GroupVersionKind
}

func (b ciliumCIDRGroupBackend) name() string { return "cilium-cidr-group" }
func (b ciliumCIDRGroupBackend) gvk() schema.GroupVersionKind {
	return b.resourceGVK
}

// CiliumCIDRGroup is cluster-scoped.
func (b ciliumCIDRGroupBackend) namespaced() bool { return false }
func (b ciliumCIDRGroupBackend) update(ctx context.Context, r *PoolSyncReconciler, pool *unstructured.Unstructured, configs []poolConfiguration, managedPrefixes []netip.Prefix) (bool, error) {
	return r.updateCIDRGroup(ctx, pool, configs, managedPrefixes)
}

type metalLBIPAddressPoolBackend struct {
	resourceGVK schema.GroupVersionKind
}

func (b metalLBIPAddressPoolBackend) name() string { return "metallb-ip-address-pool" }
func (b metalLBIPAddressPoolBackend) gvk() schema.GroupVersionKind {
	return b.resourceGVK
}

// MetalLB IPAddressPool lives in a namespace (metallb-system by default).
func (b metalLBIPAddressPoolBackend) namespaced() bool { return true }
func (b metalLBIPAddressPoolBackend) update(ctx context.Context, r *PoolSyncReconciler, pool *unstructured.Unstructured, configs []poolConfiguration, managedPrefixes []netip.Prefix) (bool, error) {
	logger := log.FromContext(ctx)

	existingAddresses, _, err := unstructured.NestedStringSlice(pool.Object, "spec", "addresses")
	if err != nil {
		return false, fmt.Errorf("failed to read spec.addresses: %w", err)
	}

	// spec.addresses is shared with the user exactly like the Cilium fields are,
	// so it needs the same ownership record. Deciding by address geometry alone
	// loses track of an entry the moment its prefix ages out of history, after
	// which the entry is preserved as though the user had written it and a fresh
	// one is appended alongside -- one leaked address per rotation, forever.
	recordValue, recordExists := pool.GetAnnotations()[AnnotationManagedAddresses]
	record := parseOwnershipRecord(recordValue, recordExists)

	preserved := make([]string, 0, len(existingAddresses))
	preservedKeys := make(map[string]struct{}, len(existingAddresses))
	for _, address := range existingAddresses {
		if isOwnedAddressEntry(address, record, managedPrefixes) {
			continue
		}
		preserved = append(preserved, address)
		preservedKeys[canonicalAddressEntry(address)] = struct{}{}
	}
	if len(preserved) > 0 {
		logger.V(1).Info("Preserving unmanaged MetalLB addresses", "count", len(preserved))
	}

	addresses := make([]string, 0, len(preserved)+len(configs))
	addresses = append(addresses, preserved...)
	managedAddresses := make([]string, 0, len(configs))
	seen := make(map[string]struct{}, len(configs))
	for _, config := range configs {
		entry := config.cidr
		if config.useAddressRange && config.start != "" && config.end != "" {
			entry = config.start + "-" + config.end
		}
		if entry == "" {
			continue
		}
		key := canonicalAddressEntry(entry)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		// Leave a user's pre-existing pin as theirs rather than claiming it.
		if _, pinned := preservedKeys[key]; pinned {
			continue
		}
		addresses = append(addresses, entry)
		managedAddresses = append(managedAddresses, entry)
	}

	currentAddresses, _, err := unstructured.NestedStringSlice(pool.Object, "spec", "addresses")
	if err != nil {
		return false, fmt.Errorf("failed to read current spec.addresses: %w", err)
	}
	managedAddressesStr := formatOwnershipRecord(managedAddresses)
	addressesChanged := !equality.Semantic.DeepEqual(currentAddresses, addresses)
	recordChanged := recordValue != managedAddressesStr || !recordExists

	// Persist the record even when the address list is byte-identical, or the
	// first pass after an upgrade leaves nothing behind and the next rotation
	// falls back to the geometric test again.
	if !addressesChanged && !recordChanged {
		logger.V(2).Info("MetalLB IPAddressPool addresses unchanged, skipping update", "pool", pool.GetName())
		return false, nil
	}

	if addressesChanged {
		if err := unstructured.SetNestedStringSlice(pool.Object, addresses, "spec", "addresses"); err != nil {
			return false, fmt.Errorf("failed to set spec.addresses: %w", err)
		}
	}

	setPoolAnnotation(pool, AnnotationManagedAddresses, managedAddressesStr)
	r.setLastSyncAnnotation(pool)
	return true, r.Update(ctx, pool)
}

// canonicalAddressEntry normalises a MetalLB address entry so ownership survives
// a difference in spelling. Entries are either "start-end" or a CIDR/address.
func canonicalAddressEntry(entry string) string {
	entry = strings.TrimSpace(entry)
	if startStr, endStr, ok := strings.Cut(entry, "-"); ok {
		return canonicalEntry(strings.TrimSpace(startStr)) + "-" + canonicalEntry(strings.TrimSpace(endStr))
	}
	return canonicalEntry(entry)
}

// isOwnedAddressEntry reports whether a MetalLB address entry is the operator's,
// with the same record-first, geometry-as-fallback rule the other backends use.
func isOwnedAddressEntry(entry string, record ownershipRecord, managedPrefixes []netip.Prefix) bool {
	if !record.present {
		return isManagedAddressEntry(entry, managedPrefixes)
	}
	return record.owns(canonicalAddressEntry(entry))
}

type calicoIPPoolBackend struct {
	resourceGVK schema.GroupVersionKind
}

func (b calicoIPPoolBackend) name() string { return "calico-ip-pool" }
func (b calicoIPPoolBackend) gvk() schema.GroupVersionKind {
	return b.resourceGVK
}

// Calico IPPool is cluster-scoped.
func (b calicoIPPoolBackend) namespaced() bool { return false }

// update points the primary IPPool at the current prefix and keeps one sibling
// IPPool per historical prefix.
//
// spec.cidr is a single scalar, so unlike every other backend Calico has nowhere
// to hold a prefix that is draining: writing the new prefix removes the old one in
// the same operation, and anything still using an address from it loses
// connectivity the moment the delegation rotates. Since the field cannot express
// more than one prefix, the drain window is expressed as separate objects instead.
//
// Siblings are owned wholesale rather than through an ownership record: the record
// mechanism exists for fields shared with the user, whereas these objects are
// entirely the operator's, so labelling them and deleting the ones that no longer
// correspond to a live prefix is both simpler and unambiguous.
func (b calicoIPPoolBackend) update(ctx context.Context, r *PoolSyncReconciler, pool *unstructured.Unstructured, configs []poolConfiguration, managedPrefixes []netip.Prefix) (bool, error) {
	logger := log.FromContext(ctx)

	if len(configs) == 0 {
		return false, nil
	}

	cidr, err := calicoCIDRForConfig(configs[0])
	if err != nil {
		return false, err
	}

	// configs[0] is the current prefix; the rest are draining history.
	siblingsChanged, err := b.syncDrainingSiblings(ctx, r, pool, configs[1:])
	if err != nil {
		return false, err
	}

	if allowedUses, found, err := unstructured.NestedStringSlice(pool.Object, "spec", "allowedUses"); err != nil {
		return false, fmt.Errorf("failed to read spec.allowedUses: %w", err)
	} else if found && !containsString(allowedUses, "LoadBalancer") {
		logger.V(1).Info("Calico IPPool does not include LoadBalancer in spec.allowedUses", "pool", pool.GetName(), "allowedUses", allowedUses)
	}

	currentCIDR, _, err := unstructured.NestedString(pool.Object, "spec", "cidr")
	if err != nil {
		return false, fmt.Errorf("failed to read spec.cidr: %w", err)
	}
	if currentCIDR == cidr {
		logger.V(2).Info("Calico IPPool CIDR unchanged, skipping update", "pool", pool.GetName())
		return siblingsChanged, nil
	}

	if err := unstructured.SetNestedField(pool.Object, cidr, "spec", "cidr"); err != nil {
		return false, fmt.Errorf("failed to set spec.cidr: %w", err)
	}

	r.setLastSyncAnnotation(pool)
	return true, r.Update(ctx, pool)
}

// LabelCalicoParentPool marks a Calico IPPool as a draining sibling created for a
// historical prefix, and names the primary pool it belongs to.
const LabelCalicoParentPool = "dynamic-prefix.io/parent-pool"

// syncDrainingSiblings creates or updates one IPPool per draining prefix and
// removes the ones whose prefix has left the history window.
//
// Reports whether anything changed, so a reconcile that only had siblings to
// adjust is still recorded as an update.
func (b calicoIPPoolBackend) syncDrainingSiblings(
	ctx context.Context,
	r *PoolSyncReconciler,
	parent *unstructured.Unstructured,
	drainingConfigs []poolConfiguration,
) (bool, error) {
	logger := log.FromContext(ctx)

	expected := make(map[string]string, len(drainingConfigs))
	for _, config := range drainingConfigs {
		cidr, err := calicoCIDRForConfig(config)
		if err != nil {
			// A historical prefix that cannot be expressed as an exact CIDR is not
			// worth failing the whole sync over; the current prefix still applies.
			logger.V(1).Info("Skipping draining Calico sibling", "pool", parent.GetName(), "reason", err.Error())
			continue
		}
		expected[siblingPoolName(parent.GetName(), cidr)] = cidr
	}

	changed := false

	// Reconcile the siblings that should exist.
	for name, cidr := range expected {
		sibling := &unstructured.Unstructured{}
		sibling.SetGroupVersionKind(b.resourceGVK)
		err := r.Get(ctx, types.NamespacedName{Name: name}, sibling)
		switch {
		case err == nil:
			existing, _, readErr := unstructured.NestedString(sibling.Object, "spec", "cidr")
			if readErr != nil {
				return changed, fmt.Errorf("failed to read sibling spec.cidr: %w", readErr)
			}
			if existing == cidr {
				continue
			}
			if err := unstructured.SetNestedField(sibling.Object, cidr, "spec", "cidr"); err != nil {
				return changed, fmt.Errorf("failed to set sibling spec.cidr: %w", err)
			}
			if err := r.Update(ctx, sibling); err != nil {
				return changed, fmt.Errorf("failed to update draining Calico IPPool %s: %w", name, err)
			}
			changed = true
		case apierrors.IsNotFound(err):
			sibling = newCalicoSibling(b.resourceGVK, parent, name, cidr)
			if err := r.Create(ctx, sibling); err != nil {
				return changed, fmt.Errorf("failed to create draining Calico IPPool %s: %w", name, err)
			}
			logger.Info("Created draining Calico IPPool", "pool", name, "cidr", cidr, "parent", parent.GetName())
			changed = true
		default:
			return changed, fmt.Errorf("failed to read draining Calico IPPool %s: %w", name, err)
		}
	}

	// Remove siblings whose prefix has aged out.
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(ListGVK(b.resourceGVK))
	if err := r.List(ctx, list, client.MatchingLabels{
		LabelManagedBy:        LabelManagedByValue,
		LabelCalicoParentPool: parent.GetName(),
	}); err != nil {
		return changed, fmt.Errorf("failed to list draining Calico IPPools: %w", err)
	}
	for i := range list.Items {
		item := &list.Items[i]
		if _, keep := expected[item.GetName()]; keep {
			continue
		}
		if err := r.Delete(ctx, item); err != nil && !apierrors.IsNotFound(err) {
			return changed, fmt.Errorf("failed to delete drained Calico IPPool %s: %w", item.GetName(), err)
		}
		logger.Info("Deleted drained Calico IPPool", "pool", item.GetName(), "parent", parent.GetName())
		changed = true
	}

	return changed, nil
}

// siblingPoolName derives a stable, DNS-safe name for a draining sibling. The
// CIDR is hashed rather than embedded because a prefix contains characters a
// resource name cannot carry.
func siblingPoolName(parent, cidr string) string {
	sum := sha256.Sum256([]byte(cidr))
	return fmt.Sprintf("%s-%s", parent, hex.EncodeToString(sum[:])[:8])
}

func newCalicoSibling(gvk schema.GroupVersionKind, parent *unstructured.Unstructured, name, cidr string) *unstructured.Unstructured {
	sibling := &unstructured.Unstructured{}
	sibling.SetGroupVersionKind(gvk)
	sibling.SetName(name)
	sibling.SetLabels(map[string]string{
		LabelManagedBy:         LabelManagedByValue,
		LabelCalicoParentPool:  parent.GetName(),
		LabelDynamicPrefixName: parent.GetAnnotations()[AnnotationName],
	})
	sibling.SetAnnotations(map[string]string{
		AnnotationLastSync: time.Now().UTC().Format(time.RFC3339),
	})
	spec := map[string]interface{}{"cidr": cidr}
	// Mirror the parent's allowedUses so the draining prefix keeps serving the
	// same purpose while connections on it wind down.
	if allowedUses, found, err := unstructured.NestedStringSlice(parent.Object, "spec", "allowedUses"); err == nil && found {
		uses := make([]interface{}, 0, len(allowedUses))
		for _, u := range allowedUses {
			uses = append(uses, u)
		}
		spec["allowedUses"] = uses
	}
	sibling.Object["spec"] = spec
	return sibling
}

func (r *PoolSyncReconciler) poolBackends() []poolBackend {
	if len(r.BackendGVKs) > 0 {
		return backendsForGVKs(r.BackendGVKs)
	}

	return []poolBackend{
		ciliumLoadBalancerIPPoolBackend{resourceGVK: r.lbIPPoolGVK()},
		ciliumCIDRGroupBackend{resourceGVK: r.cidrGroupGVK()},
	}
}

func backendsForGVKs(gvks []schema.GroupVersionKind) []poolBackend {
	backends := make([]poolBackend, 0, len(gvks))
	seen := make(map[schema.GroupVersionKind]bool, len(gvks))
	for _, gvk := range gvks {
		if seen[gvk] {
			continue
		}
		backend := backendForGVK(gvk)
		if backend == nil {
			continue
		}
		seen[gvk] = true
		backends = append(backends, backend)
	}
	return backends
}

func backendForGVK(gvk schema.GroupVersionKind) poolBackend {
	switch {
	case gvk.Group == "cilium.io" && gvk.Kind == "CiliumLoadBalancerIPPool":
		return ciliumLoadBalancerIPPoolBackend{resourceGVK: gvk}
	case gvk.Group == "cilium.io" && gvk.Kind == "CiliumCIDRGroup":
		return ciliumCIDRGroupBackend{resourceGVK: gvk}
	case gvk.Group == "metallb.io" && gvk.Kind == "IPAddressPool":
		return metalLBIPAddressPoolBackend{resourceGVK: gvk}
	case gvk.Group == "projectcalico.org" && gvk.Kind == "IPPool":
		return calicoIPPoolBackend{resourceGVK: gvk}
	default:
		return nil
	}
}

func calicoCIDRForConfig(config poolConfiguration) (string, error) {
	if !config.useAddressRange {
		if config.cidr == "" {
			return "", fmt.Errorf("calico IPPool requires a CIDR configuration")
		}
		return config.cidr, nil
	}

	cidr, exact, err := exactCIDRForAddressRange(config.start, config.end)
	if err != nil {
		return "", err
	}
	if !exact {
		return "", fmt.Errorf("calico IPPool spec.cidr can only represent exact CIDR ranges; %s-%s is not CIDR-aligned", config.start, config.end)
	}
	return cidr, nil
}

func exactCIDRForAddressRange(startStr, endStr string) (string, bool, error) {
	start, err := netip.ParseAddr(startStr)
	if err != nil {
		return "", false, fmt.Errorf("invalid address range start %q: %w", startStr, err)
	}
	end, err := netip.ParseAddr(endStr)
	if err != nil {
		return "", false, fmt.Errorf("invalid address range end %q: %w", endStr, err)
	}
	if start.Is4() != end.Is4() {
		return "", false, fmt.Errorf("address range start and end must use the same IP family")
	}
	if start.Compare(end) > 0 {
		return "", false, fmt.Errorf("address range start %s is greater than end %s", start, end)
	}
	if !start.Is6() {
		return "", false, fmt.Errorf("dynamic prefix address ranges are only supported for IPv6")
	}

	cidr := prefix.RangeToCIDR(start, end)
	last, err := lastAddrOfPrefix(cidr)
	if err != nil {
		return "", false, err
	}

	return cidr.String(), cidr.Masked().Addr() == start && last == end, nil
}

func isManagedAddressEntry(entry string, managedPrefixes []netip.Prefix) bool {
	entry = strings.TrimSpace(entry)
	if entry == "" {
		return false
	}

	if startStr, endStr, ok := strings.Cut(entry, "-"); ok {
		start, err := netip.ParseAddr(strings.TrimSpace(startStr))
		if err != nil {
			return false
		}
		end, err := netip.ParseAddr(strings.TrimSpace(endStr))
		if err != nil {
			return false
		}
		if start.Is4() || end.Is4() || start.Is4() != end.Is4() {
			return false
		}
		return addressRangeWithinManaged(start, end, managedPrefixes)
	}

	if p, err := netip.ParsePrefix(entry); err == nil {
		if p.Addr().Is4() {
			return false
		}
		return isPrefixSubsetOfManaged(p, managedPrefixes)
	}

	addr, err := netip.ParseAddr(entry)
	if err != nil || addr.Is4() {
		return false
	}
	return addrInManagedPrefixes(addr, managedPrefixes)
}

// addressRangeWithinManaged reports whether an address range lies entirely inside
// a single managed prefix.
//
// Containment, not overlap. A range that merely overlaps also spans addresses the
// operator does not manage, so claiming it would delete a user's range that
// happens to straddle a managed prefix -- and on the no-record fallback path that
// deletion is silent and permanent.
func addressRangeWithinManaged(start, end netip.Addr, managedPrefixes []netip.Prefix) bool {
	if start.Compare(end) > 0 {
		start, end = end, start
	}

	for _, managedPrefix := range managedPrefixes {
		if managedPrefix.Addr().Is4() != start.Is4() {
			continue
		}

		managedStart := managedPrefix.Masked().Addr()
		managedEnd, err := lastAddrOfPrefix(managedPrefix)
		if err != nil {
			continue
		}

		if start.Compare(managedStart) >= 0 && end.Compare(managedEnd) <= 0 {
			return true
		}
	}
	return false
}

func addrInManagedPrefixes(addr netip.Addr, managedPrefixes []netip.Prefix) bool {
	for _, managedPrefix := range managedPrefixes {
		if managedPrefix.Contains(addr) {
			return true
		}
	}
	return false
}

func lastAddrOfPrefix(p netip.Prefix) (netip.Addr, error) {
	p = p.Masked()
	base, bitLen, err := addrToBigInt(p.Addr())
	if err != nil {
		return netip.Addr{}, err
	}

	hostBits := bitLen - p.Bits()
	if hostBits < 0 {
		return netip.Addr{}, fmt.Errorf("invalid prefix length %d for %d-bit address", p.Bits(), bitLen)
	}

	hostMask := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), uint(hostBits)), big.NewInt(1))
	last := new(big.Int).Or(base, hostMask)
	return bigIntToAddr(last, bitLen), nil
}

func addrToBigInt(addr netip.Addr) (*big.Int, int, error) {
	if addr.Is4() {
		bytes := addr.As4()
		return new(big.Int).SetBytes(bytes[:]), 32, nil
	}
	if addr.Is6() {
		bytes := addr.As16()
		return new(big.Int).SetBytes(bytes[:]), 128, nil
	}
	return nil, 0, fmt.Errorf("unsupported IP address %s", addr)
}

func bigIntToAddr(value *big.Int, bitLen int) netip.Addr {
	if bitLen == 32 {
		bytes := value.FillBytes(make([]byte, 4))
		var addr [4]byte
		copy(addr[:], bytes)
		return netip.AddrFrom4(addr)
	}

	bytes := value.FillBytes(make([]byte, 16))
	var addr [16]byte
	copy(addr[:], bytes)
	return netip.AddrFrom16(addr)
}

func containsString(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}
