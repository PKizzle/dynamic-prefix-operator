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
	"fmt"
	"math/big"
	"net/netip"
	"strings"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/pkizzle/dynamic-prefix-operator/internal/prefix"
)

type poolBackend interface {
	name() string
	gvk() schema.GroupVersionKind
	update(ctx context.Context, r *PoolSyncReconciler, pool *unstructured.Unstructured, configs []poolConfiguration, managedPrefixes []netip.Prefix) (bool, error)
}

type ciliumLoadBalancerIPPoolBackend struct {
	resourceGVK schema.GroupVersionKind
}

func (b ciliumLoadBalancerIPPoolBackend) name() string { return "cilium-load-balancer-ip-pool" }
func (b ciliumLoadBalancerIPPoolBackend) gvk() schema.GroupVersionKind {
	return b.resourceGVK
}
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
func (b metalLBIPAddressPoolBackend) update(ctx context.Context, r *PoolSyncReconciler, pool *unstructured.Unstructured, configs []poolConfiguration, managedPrefixes []netip.Prefix) (bool, error) {
	logger := log.FromContext(ctx)

	existingAddresses, _, err := unstructured.NestedStringSlice(pool.Object, "spec", "addresses")
	if err != nil {
		return false, fmt.Errorf("failed to read spec.addresses: %w", err)
	}

	preserved := make([]string, 0, len(existingAddresses))
	for _, address := range existingAddresses {
		if !isManagedAddressEntry(address, managedPrefixes) {
			preserved = append(preserved, address)
		}
	}
	if len(preserved) > 0 {
		logger.V(1).Info("Preserving unmanaged MetalLB addresses", "count", len(preserved))
	}

	addresses := make([]string, 0, len(preserved)+len(configs))
	addresses = append(addresses, preserved...)
	for _, config := range configs {
		if config.useAddressRange && config.start != "" && config.end != "" {
			addresses = append(addresses, config.start+"-"+config.end)
			continue
		}
		addresses = append(addresses, config.cidr)
	}

	currentAddresses, _, err := unstructured.NestedStringSlice(pool.Object, "spec", "addresses")
	if err != nil {
		return false, fmt.Errorf("failed to read current spec.addresses: %w", err)
	}
	if equality.Semantic.DeepEqual(currentAddresses, addresses) {
		logger.V(2).Info("MetalLB IPAddressPool addresses unchanged, skipping update", "pool", pool.GetName())
		return false, nil
	}

	if err := unstructured.SetNestedStringSlice(pool.Object, addresses, "spec", "addresses"); err != nil {
		return false, fmt.Errorf("failed to set spec.addresses: %w", err)
	}

	r.setLastSyncAnnotation(pool)
	return true, r.Update(ctx, pool)
}

type calicoIPPoolBackend struct {
	resourceGVK schema.GroupVersionKind
}

func (b calicoIPPoolBackend) name() string { return "calico-ip-pool" }
func (b calicoIPPoolBackend) gvk() schema.GroupVersionKind {
	return b.resourceGVK
}
func (b calicoIPPoolBackend) update(ctx context.Context, r *PoolSyncReconciler, pool *unstructured.Unstructured, configs []poolConfiguration, managedPrefixes []netip.Prefix) (bool, error) {
	logger := log.FromContext(ctx)

	if len(configs) == 0 {
		return false, nil
	}

	cidr, err := calicoCIDRForConfig(configs[0])
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
		return false, nil
	}

	if err := unstructured.SetNestedField(pool.Object, cidr, "spec", "cidr"); err != nil {
		return false, fmt.Errorf("failed to set spec.cidr: %w", err)
	}

	r.setLastSyncAnnotation(pool)
	return true, r.Update(ctx, pool)
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
		return addressRangeOverlapsManaged(start, end, managedPrefixes)
	}

	if p, err := netip.ParsePrefix(entry); err == nil {
		if p.Addr().Is4() {
			return false
		}
		return isPrefixManaged(p, managedPrefixes)
	}

	addr, err := netip.ParseAddr(entry)
	if err != nil || addr.Is4() {
		return false
	}
	return addrInManagedPrefixes(addr, managedPrefixes)
}

func addressRangeOverlapsManaged(start, end netip.Addr, managedPrefixes []netip.Prefix) bool {
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

		if addressesOverlap(start, end, managedStart, managedEnd) {
			return true
		}
	}
	return false
}

func addressesOverlap(startA, endA, startB, endB netip.Addr) bool {
	return startA.Compare(endB) <= 0 && startB.Compare(endA) <= 0
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
