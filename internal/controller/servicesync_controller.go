/*
Copyright 2026 jr42.

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
	"net/netip"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	dynamicprefixiov1alpha1 "github.com/jr42/dynamic-prefix-operator/api/v1alpha1"
	"github.com/jr42/dynamic-prefix-operator/internal/prefix"
)

const (
	// AnnotationCiliumIPs is the Cilium LB-IPAM annotation for requesting specific IPs.
	AnnotationCiliumIPs = "lbipam.cilium.io/ips"

	// AnnotationExternalDNSTarget is the external-dns annotation for overriding DNS target.
	AnnotationExternalDNSTarget = "external-dns.alpha.kubernetes.io/target"

	// AnnotationServiceAddressRange specifies which address range to use for Service IPs.
	// This is used when the DynamicPrefix uses address ranges (Mode 1).
	AnnotationServiceAddressRange = "dynamic-prefix.io/service-address-range"

	// AnnotationServiceSubnet specifies which subnet to use for Service IPs.
	// This is used when the DynamicPrefix uses subnets (Mode 2).
	AnnotationServiceSubnet = "dynamic-prefix.io/service-subnet"

	// AnnotationSuffix specifies a static IPv6 suffix (host part) for a Service.
	// When set, the operator calculates full IPv6 addresses by combining the current
	// (and historical) prefix with this suffix, instead of inferring it from the
	// Service's currently assigned IP. This is the preferred way to declare intent
	// for dual-stack Services: only put IPv4 in lbipam.cilium.io/ips and let the
	// operator manage the IPv6 portion entirely.
	// Requires dynamic-prefix.io/name to also be set.
	// Example: "::ffff:0:2" combined with prefix 2001:db8:abcd:100::/56
	// produces 2001:db8:abcd:100::ffff:0:2.
	AnnotationSuffix = "dynamic-prefix.io/suffix"
)

// ServiceSyncReconciler reconciles LoadBalancer Services for HA mode prefix transitions.
// In HA mode, it manages both lbipam.cilium.io/ips and external-dns.alpha.kubernetes.io/target
// annotations to ensure graceful transitions when prefixes change.
type ServiceSyncReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;update;patch

// Reconcile handles Service synchronization for HA mode prefix transitions.
func (r *ServiceSyncReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Fetch the Service
	var svc corev1.Service
	if err := r.Get(ctx, req.NamespacedName, &svc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Skip non-LoadBalancer services
	if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
		return ctrl.Result{}, nil
	}

	// Check for DynamicPrefix annotation
	annotations := svc.GetAnnotations()
	if annotations == nil {
		return ctrl.Result{}, nil
	}

	dpName, hasDP := annotations[AnnotationName]
	if !hasDP {
		return ctrl.Result{}, nil
	}

	// Fetch the referenced DynamicPrefix
	var dp dynamicprefixiov1alpha1.DynamicPrefix
	if err := r.Get(ctx, types.NamespacedName{Name: dpName}, &dp); err != nil {
		log.Error(err, "Failed to get DynamicPrefix", "name", dpName)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Check if HA mode is enabled
	if dp.Spec.Transition == nil || dp.Spec.Transition.Mode != dynamicprefixiov1alpha1.TransitionModeHA {
		// Not HA mode, skip Service management
		return ctrl.Result{}, nil
	}

	log.Info("Syncing Service for HA mode", "service", req.NamespacedName, "dynamicPrefix", dpName)

	var allIPs []string
	var currentIP string

	if suffix, ok := annotations[AnnotationSuffix]; ok && suffix != "" {
		// Suffix-based mode: calculate full IPv6 from prefix + suffix directly.
		// This is the preferred path — no need to wait for Cilium to assign an IP first.
		var err error
		allIPs, currentIP, err = r.calculateSuffixIPs(&dp, suffix)
		if err != nil {
			log.Error(err, "Failed to calculate IPs from suffix", "suffix", suffix)
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		log.V(1).Info("Using suffix-based IP calculation", "suffix", suffix, "currentIP", currentIP)
	} else {
		// Legacy mode: infer suffix from the Service's currently assigned IP.
		currentServiceIP := r.getCurrentServiceIP(&svc)
		if currentServiceIP == "" {
			log.V(1).Info("Service has no IP assigned yet, skipping")
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}

		var err error
		allIPs, currentIP, err = r.calculateServiceIPs(ctx, &dp, &svc, currentServiceIP)
		if err != nil {
			log.Error(err, "Failed to calculate Service IPs")
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
	}

	// Collect all prefixes the operator manages (current + historical).
	// Any IPv6 in the annotation that falls within these prefixes is operator-managed
	// and will be replaced. Everything else (IPv4, static IPv6) is preserved.
	managedPrefixes := r.collectManagedPrefixes(&dp)

	existingIPs := annotations[AnnotationCiliumIPs]
	preservedIPs := extractUnmanagedIPs(existingIPs, managedPrefixes)

	// Build final IP list: preserved (IPv4 + static IPv6) first, then calculated IPv6
	finalIPs := append(preservedIPs, allIPs...)

	// Update Service annotations
	updated := false
	newAnnotations := make(map[string]string)
	for k, v := range annotations {
		newAnnotations[k] = v
	}

	// Set lbipam.cilium.io/ips with preserved IPs + all managed IPv6 IPs
	finalIPsStr := strings.Join(finalIPs, ",")
	if annotations[AnnotationCiliumIPs] != finalIPsStr {
		newAnnotations[AnnotationCiliumIPs] = finalIPsStr
		updated = true
	}

	// Set external-dns target to current IPv6 only
	if annotations[AnnotationExternalDNSTarget] != currentIP {
		newAnnotations[AnnotationExternalDNSTarget] = currentIP
		updated = true
	}

	// Update last-sync annotation
	newAnnotations[AnnotationLastSync] = time.Now().UTC().Format(time.RFC3339)

	if updated {
		svc.SetAnnotations(newAnnotations)
		if err := r.Update(ctx, &svc); err != nil {
			log.Error(err, "Failed to update Service annotations")
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		log.Info("Service annotations updated", "service", req.NamespacedName,
			"allIPs", finalIPsStr, "dnsTarget", currentIP,
			"preservedCount", len(preservedIPs), "managedCount", len(allIPs))
	}

	return ctrl.Result{}, nil
}

// collectManagedPrefixes returns all prefixes the operator manages for a
// DynamicPrefix (current + historical). These are used to identify which
// IPv6 addresses in an annotation are operator-managed vs static.
// collectManagedPrefixesForService delegates to the shared collectManagedPrefixes.
func (r *ServiceSyncReconciler) collectManagedPrefixes(dp *dynamicprefixiov1alpha1.DynamicPrefix) []netip.Prefix {
	return collectManagedPrefixes(dp)
}

// extractUnmanagedIPs parses a comma-separated IP list and returns all IPs
// that are NOT managed by the operator. An IP is considered managed if it is
// an IPv6 address that falls within any of the given managed prefixes.
// All IPv4 addresses and IPv6 addresses outside the managed prefixes are
// preserved. This supports:
// - Multiple static IPv4 addresses
// - Multiple static (non-dynamic) IPv6 addresses
// - Mixed dual-stack annotations
func extractUnmanagedIPs(ipsAnnotation string, managedPrefixes []netip.Prefix) []string {
	if ipsAnnotation == "" {
		return nil
	}
	var preserved []string
	for _, raw := range strings.Split(ipsAnnotation, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		addr, err := netip.ParseAddr(raw)
		if err != nil {
			// Not a valid IP — preserve as-is to avoid data loss
			preserved = append(preserved, raw)
			continue
		}
		if addr.Is4() || addr.Is4In6() {
			// Always preserve IPv4
			preserved = append(preserved, raw)
			continue
		}
		// IPv6 — check if it falls within any managed prefix
		managed := false
		for _, p := range managedPrefixes {
			if p.Contains(addr) {
				managed = true
				break
			}
		}
		if !managed {
			// Static IPv6 outside managed prefixes — preserve
			preserved = append(preserved, raw)
		}
	}
	return preserved
}

// getCurrentServiceIP returns the current IPv6 IP from Service status.
func (r *ServiceSyncReconciler) getCurrentServiceIP(svc *corev1.Service) string {
	for _, ingress := range svc.Status.LoadBalancer.Ingress {
		if ingress.IP != "" {
			// Prefer IPv6
			addr, err := netip.ParseAddr(ingress.IP)
			if err == nil && addr.Is6() {
				return ingress.IP
			}
		}
	}
	// Fall back to any IP
	for _, ingress := range svc.Status.LoadBalancer.Ingress {
		if ingress.IP != "" {
			return ingress.IP
		}
	}
	return ""
}

// calculateServiceIPs calculates all IPs for a Service based on current prefix and history.
// Returns (allIPs, currentIP, error).
func (r *ServiceSyncReconciler) calculateServiceIPs(
	ctx context.Context,
	dp *dynamicprefixiov1alpha1.DynamicPrefix,
	svc *corev1.Service,
	currentServiceIP string,
) ([]string, string, error) {
	log := logf.FromContext(ctx)
	annotations := svc.GetAnnotations()

	// Get max history count
	maxHistory := 2 // Default
	if dp.Spec.Transition != nil && dp.Spec.Transition.MaxPrefixHistory > 0 {
		maxHistory = dp.Spec.Transition.MaxPrefixHistory
	}

	// Determine the IP offset within the prefix from the current Service IP
	// This allows us to calculate corresponding IPs in historical prefixes
	currentAddr, err := netip.ParseAddr(currentServiceIP)
	if err != nil {
		return nil, "", err
	}

	addressRangeName := annotations[AnnotationServiceAddressRange]
	subnetName := annotations[AnnotationServiceSubnet]
	// Also check the pool-level annotations for backward compatibility
	if addressRangeName == "" {
		addressRangeName = annotations[AnnotationAddressRange]
	}
	if subnetName == "" {
		subnetName = annotations[AnnotationSubnet]
	}

	var allIPs []string
	var currentPrefixIP string

	if addressRangeName != "" {
		// Mode 1: Address ranges
		currentPrefixIP, allIPs, err = r.calculateAddressRangeIPs(dp, currentAddr, addressRangeName, maxHistory)
		if err != nil {
			log.Error(err, "Failed to calculate address range IPs")
			// Fall back to current IP only
			return []string{currentServiceIP}, currentServiceIP, nil
		}
	} else if subnetName != "" {
		// Mode 2: Subnets
		currentPrefixIP, allIPs, err = r.calculateSubnetIPs(dp, currentAddr, subnetName, maxHistory)
		if err != nil {
			log.Error(err, "Failed to calculate subnet IPs")
			// Fall back to current IP only
			return []string{currentServiceIP}, currentServiceIP, nil
		}
	} else {
		// No specific range/subnet, use current IP
		return []string{currentServiceIP}, currentServiceIP, nil
	}

	return allIPs, currentPrefixIP, nil
}

// calculateSuffixIPs calculates IPv6 addresses by combining a static suffix with
// the current and historical prefixes. Returns (allIPs, currentIP, error).
// The suffix is the host part of the address (e.g. "::ffff:0:2").
func (r *ServiceSyncReconciler) calculateSuffixIPs(
	dp *dynamicprefixiov1alpha1.DynamicPrefix,
	suffix string,
) ([]string, string, error) {
	suffixAddr, err := netip.ParseAddr(suffix)
	if err != nil {
		return nil, "", fmt.Errorf("invalid IPv6 suffix %q: %w", suffix, err)
	}
	suffixBytes := suffixAddr.As16()

	if dp.Status.CurrentPrefix == "" {
		return nil, "", fmt.Errorf("DynamicPrefix has no current prefix")
	}

	maxHistory := 2
	if dp.Spec.Transition != nil && dp.Spec.Transition.MaxPrefixHistory > 0 {
		maxHistory = dp.Spec.Transition.MaxPrefixHistory
	}

	currentPrefix, err := netip.ParsePrefix(dp.Status.CurrentPrefix)
	if err != nil {
		return nil, "", fmt.Errorf("invalid current prefix %q: %w", dp.Status.CurrentPrefix, err)
	}

	currentIP := combinePrefixSuffix(currentPrefix, suffixBytes)
	allIPs := []string{currentIP.String()}

	for i, histEntry := range dp.Status.History {
		if i >= maxHistory {
			break
		}
		histPrefix, err := netip.ParsePrefix(histEntry.Prefix)
		if err != nil {
			continue
		}
		histIP := combinePrefixSuffix(histPrefix, suffixBytes)
		allIPs = append(allIPs, histIP.String())
	}

	return allIPs, currentIP.String(), nil
}

// combinePrefixSuffix combines a prefix's network part with a suffix's host part.
// For a /48 prefix and suffix ::ffff:0:2, the first 48 bits come from the prefix
// and the remaining 80 bits come from the suffix.
func combinePrefixSuffix(pfx netip.Prefix, suffixBytes [16]byte) netip.Addr {
	prefixBytes := pfx.Addr().As16()
	prefixLen := pfx.Bits()

	var result [16]byte
	for i := 0; i < 16; i++ {
		bitPos := i * 8
		if bitPos+8 <= prefixLen {
			// Entire byte comes from prefix
			result[i] = prefixBytes[i]
		} else if bitPos >= prefixLen {
			// Entire byte comes from suffix
			result[i] = suffixBytes[i]
		} else {
			// Split byte: high bits from prefix, low bits from suffix
			mask := byte(0xFF << (8 - (prefixLen - bitPos)))
			result[i] = (prefixBytes[i] & mask) | (suffixBytes[i] & ^mask)
		}
	}
	return netip.AddrFrom16(result)
}

// calculateAddressRangeIPs calculates IPs for address range mode.
func (r *ServiceSyncReconciler) calculateAddressRangeIPs(
	dp *dynamicprefixiov1alpha1.DynamicPrefix,
	currentAddr netip.Addr,
	addressRangeName string,
	maxHistory int,
) (string, []string, error) {
	// Find the address range spec
	var rangeSpec *dynamicprefixiov1alpha1.AddressRangeSpec
	for i := range dp.Spec.AddressRanges {
		if dp.Spec.AddressRanges[i].Name == addressRangeName {
			rangeSpec = &dp.Spec.AddressRanges[i]
			break
		}
	}
	if rangeSpec == nil {
		return "", nil, nil
	}

	// Calculate offset of current IP within its range
	currentPrefix, err := netip.ParsePrefix(dp.Status.CurrentPrefix)
	if err != nil {
		return "", nil, err
	}

	cfg := prefix.AddressRangeConfig{
		Name:  rangeSpec.Name,
		Start: rangeSpec.Start,
		End:   rangeSpec.End,
	}

	currentRange, err := prefix.CalculateAddressRange(currentPrefix, cfg)
	if err != nil {
		return "", nil, err
	}

	// Calculate offset from start of range
	offset := r.calculateIPOffset(currentRange.Start, currentAddr)

	var allIPs []string
	currentPrefixIP := currentAddr.String()

	// Add current prefix IP
	allIPs = append(allIPs, currentPrefixIP)

	// Calculate IPs for historical prefixes
	for i, histEntry := range dp.Status.History {
		if i >= maxHistory {
			break
		}

		histPrefix, err := netip.ParsePrefix(histEntry.Prefix)
		if err != nil {
			continue
		}

		histRange, err := prefix.CalculateAddressRange(histPrefix, cfg)
		if err != nil {
			continue
		}

		histIP := r.applyIPOffset(histRange.Start, offset)
		if histIP.IsValid() {
			allIPs = append(allIPs, histIP.String())
		}
	}

	return currentPrefixIP, allIPs, nil
}

// calculateSubnetIPs calculates IPs for subnet mode.
func (r *ServiceSyncReconciler) calculateSubnetIPs(
	dp *dynamicprefixiov1alpha1.DynamicPrefix,
	currentAddr netip.Addr,
	subnetName string,
	maxHistory int,
) (string, []string, error) {
	// Find the subnet spec
	var subnetSpec *dynamicprefixiov1alpha1.SubnetSpec
	for i := range dp.Spec.Subnets {
		if dp.Spec.Subnets[i].Name == subnetName {
			subnetSpec = &dp.Spec.Subnets[i]
			break
		}
	}
	if subnetSpec == nil {
		return "", nil, nil
	}

	// Calculate current subnet
	currentPrefix, err := netip.ParsePrefix(dp.Status.CurrentPrefix)
	if err != nil {
		return "", nil, err
	}

	cfg := prefix.SubnetConfig{
		Name:         subnetSpec.Name,
		Offset:       subnetSpec.Offset,
		PrefixLength: subnetSpec.PrefixLength,
	}

	currentSubnet, err := prefix.CalculateSubnet(currentPrefix, cfg)
	if err != nil {
		return "", nil, err
	}

	// Calculate offset from start of subnet
	offset := r.calculateIPOffset(currentSubnet.CIDR.Addr(), currentAddr)

	var allIPs []string
	currentPrefixIP := currentAddr.String()

	// Add current prefix IP
	allIPs = append(allIPs, currentPrefixIP)

	// Calculate IPs for historical prefixes
	for i, histEntry := range dp.Status.History {
		if i >= maxHistory {
			break
		}

		histPrefix, err := netip.ParsePrefix(histEntry.Prefix)
		if err != nil {
			continue
		}

		histSubnet, err := prefix.CalculateSubnet(histPrefix, cfg)
		if err != nil {
			continue
		}

		histIP := r.applyIPOffset(histSubnet.CIDR.Addr(), offset)
		if histIP.IsValid() {
			allIPs = append(allIPs, histIP.String())
		}
	}

	return currentPrefixIP, allIPs, nil
}

// calculateIPOffset calculates the offset between two IPv6 addresses.
func (r *ServiceSyncReconciler) calculateIPOffset(base, target netip.Addr) [16]byte {
	baseBytes := base.As16()
	targetBytes := target.As16()
	var offset [16]byte

	borrow := uint16(0)
	for i := 15; i >= 0; i-- {
		diff := int16(targetBytes[i]) - int16(baseBytes[i]) - int16(borrow)
		if diff < 0 {
			diff += 256
			borrow = 1
		} else {
			borrow = 0
		}
		offset[i] = byte(diff)
	}

	return offset
}

// applyIPOffset applies an offset to an IPv6 address.
func (r *ServiceSyncReconciler) applyIPOffset(base netip.Addr, offset [16]byte) netip.Addr {
	baseBytes := base.As16()
	var result [16]byte

	carry := uint16(0)
	for i := 15; i >= 0; i-- {
		sum := uint16(baseBytes[i]) + uint16(offset[i]) + carry
		result[i] = byte(sum & 0xFF)
		carry = sum >> 8
	}

	return netip.AddrFrom16(result)
}

// SetupWithManager sets up the controller with the Manager.
func (r *ServiceSyncReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Create predicate for LoadBalancer Services with dynamic-prefix.io/name annotation
	hasAnnotation := predicate.NewPredicateFuncs(func(obj client.Object) bool {
		svc, ok := obj.(*corev1.Service)
		if !ok {
			return false
		}
		if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
			return false
		}
		annotations := svc.GetAnnotations()
		if annotations == nil {
			return false
		}
		_, ok = annotations[AnnotationName]
		return ok
	})

	return ctrl.NewControllerManagedBy(mgr).
		Named("servicesync").
		For(&corev1.Service{}, builder.WithPredicates(hasAnnotation)).
		Watches(&dynamicprefixiov1alpha1.DynamicPrefix{}, handler.EnqueueRequestsFromMapFunc(r.findReferencingServices)).
		Complete(r)
}

// findReferencingServices finds all Services that reference the given DynamicPrefix.
func (r *ServiceSyncReconciler) findReferencingServices(ctx context.Context, obj client.Object) []reconcile.Request {
	dp, ok := obj.(*dynamicprefixiov1alpha1.DynamicPrefix)
	if !ok {
		return nil
	}

	// Only process if HA mode is enabled
	if dp.Spec.Transition == nil || dp.Spec.Transition.Mode != dynamicprefixiov1alpha1.TransitionModeHA {
		return nil
	}

	log := logf.FromContext(ctx)
	var requests []reconcile.Request

	// List all Services
	var serviceList corev1.ServiceList
	if err := r.List(ctx, &serviceList); err != nil {
		log.V(1).Info("Failed to list Services", "error", err)
		return nil
	}

	for _, svc := range serviceList.Items {
		if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
			continue
		}
		annotations := svc.GetAnnotations()
		if annotations == nil {
			continue
		}
		if annotations[AnnotationName] == dp.Name {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      svc.Name,
					Namespace: svc.Namespace,
				},
			})
		}
	}

	if len(requests) > 0 {
		log.Info("DynamicPrefix changed, enqueuing referencing Services", "dynamicPrefix", dp.Name, "serviceCount", len(requests))
	}

	return requests
}
