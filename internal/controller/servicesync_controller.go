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
	"hash/fnv"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	dynamicprefixiov1alpha1 "github.com/pkizzle/dynamic-prefix-operator/api/v1alpha1"
	"github.com/pkizzle/dynamic-prefix-operator/internal/prefix"
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
	// AnnotationSkipExternalDNSUpdate when set to "true" on a Service, prevents the
	// operator from managing the external-dns.alpha.kubernetes.io/target annotation.
	// The operator will still manage lbipam.cilium.io/ips normally.
	// Only has effect in HA mode (the external-dns target annotation is only managed in HA mode).
	AnnotationSkipExternalDNSUpdate = "dynamic-prefix.io/skip-external-dns-update"

	// AnnotationL2Nudge records the set of assigned LoadBalancer addresses the
	// operator last forced Cilium's L2 announcer to re-read. See nudgeL2Announcer
	// for why the nudge is needed; the value is an opaque fingerprint, and only
	// its inequality with the current one is meaningful.
	AnnotationL2Nudge = "dynamic-prefix.io/l2-announce-nudge"

	// AnnotationSkipL2Nudge when set to "true" on a Service disables the L2
	// announcer nudge for it. Intended for clusters on a Cilium release that has
	// fixed the underlying bug, or that do not use Cilium L2 announcements at all.
	AnnotationSkipL2Nudge = "dynamic-prefix.io/skip-l2-nudge"

	// AnnotationValueTrue is the opt-in value for boolean annotations.
	AnnotationValueTrue = "true"

	// serviceDynamicPrefixIndex indexes Services by the DynamicPrefix they reference.
	serviceDynamicPrefixIndex = "metadata.annotations.dynamic-prefix.io/name"
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
		// De-annotated. Anything the operator wrote is still sitting in the
		// Service, and the addresses in it stop resolving once the prefix rotates,
		// so hand the fields back rather than walking away from them.
		if hasOwnershipRecord(annotations) {
			return r.releaseService(ctx, &svc)
		}
		return ctrl.Result{}, nil
	}

	// Fetch the referenced DynamicPrefix
	var dp dynamicprefixiov1alpha1.DynamicPrefix
	if err := r.Get(ctx, types.NamespacedName{Name: dpName}, &dp); err != nil {
		if apierrors.IsNotFound(err) {
			log.V(1).Info("Referenced DynamicPrefix not found, will retry", "name", dpName)
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
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

	// Never rewrite the annotations from an empty calculation. Writing an empty
	// lbipam.cilium.io/ips withdraws the Service's LoadBalancer IP request, and
	// an empty current IP leaves a trailing comma in the external-dns target.
	// Leaving the annotations untouched keeps the Service on its existing
	// address until the next reconcile produces a real answer.
	if len(allIPs) == 0 || currentIP == "" {
		log.Info("Calculated no addresses for Service; leaving annotations unchanged",
			"service", req.NamespacedName, "currentIP", currentIP, "addresses", len(allIPs))
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Collect all prefixes the operator manages (current + historical). These are
	// only used as a fallback for objects that predate the ownership record; see
	// ownership.go for why geometry alone cannot answer "is this entry mine?".
	managedPrefixes := r.collectManagedPrefixes(&dp)

	ipsRecordValue, ipsRecordExists := annotations[AnnotationManagedIPs]
	ipsRecord := parseOwnershipRecord(ipsRecordValue, ipsRecordExists)

	existingIPs := annotations[AnnotationCiliumIPs]
	preservedIPs := preserveUnownedIPs(existingIPs, ipsRecord, managedPrefixes)

	// An address already present and unowned is the user's, even where it matches
	// one this pass would write. Keep their copy and drop it from what the operator
	// claims: recording it would let a later rotation, once the address stops being
	// generated, recognise the pin as the operator's and delete it.
	claimedIPs := excludePinned(allIPs, preservedIPs)

	// Build final IP list: preserved (IPv4 + static IPv6) first, then calculated IPv6.
	finalIPs := dedupePreservingOrder(append(preservedIPs, allIPs...))

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

	// Record what we just claimed, in the same Update as the field it describes,
	// so the record and lbipam.cilium.io/ips cannot diverge. This is written even
	// when the IP list itself is unchanged: on the first pass after an upgrade the
	// list often already matches, and without the record the next rotation would
	// fall back to the leaky prefix test.
	managedIPsStr := formatOwnershipRecord(claimedIPs)
	if annotations[AnnotationManagedIPs] != managedIPsStr {
		newAnnotations[AnnotationManagedIPs] = managedIPsStr
		updated = true
	}

	// Set external-dns target unless the Service has opted out via
	// dynamic-prefix.io/skip-external-dns-update: "true".
	skipExternalDNS := annotations[AnnotationSkipExternalDNSUpdate] == AnnotationValueTrue

	var finalTargetStr string
	if !skipExternalDNS {
		// Preserve non-managed entries (hostnames, IPv4, static IPv6) and set
		// only the current IPv6 as the managed target.
		// This supports dual-stack NAT setups where IPv4 is a hostname (e.g.,
		// "example.com") pointing to the router's public IPv4 via NAT, while IPv6
		// uses direct per-service addresses that change with prefix rotation.
		targetRecordValue, targetRecordExists := annotations[AnnotationManagedTargets]
		targetRecord := parseOwnershipRecord(targetRecordValue, targetRecordExists)

		existingTarget := annotations[AnnotationExternalDNSTarget]
		preservedTargets := preserveUnownedIPs(existingTarget, targetRecord, managedPrefixes)
		finalTargets := dedupePreservingOrder(append(preservedTargets, currentIP))
		finalTargetStr = strings.Join(finalTargets, ",")
		if annotations[AnnotationExternalDNSTarget] != finalTargetStr {
			newAnnotations[AnnotationExternalDNSTarget] = finalTargetStr
			updated = true
		}

		managedTargetStr := formatOwnershipRecord(excludePinned([]string{currentIP}, preservedTargets))
		if annotations[AnnotationManagedTargets] != managedTargetStr {
			newAnnotations[AnnotationManagedTargets] = managedTargetStr
			updated = true
		}
	} else {
		// Opting out has to hand the field back, not merely stop touching it.
		// Walking away would leave the last address the operator wrote sitting in
		// the target, and that address stops resolving at the next rotation -- so
		// the opt-out would quietly turn into a broken DNS record rather than a
		// field the user now controls.
		targetRecordValue, targetRecordExists := annotations[AnnotationManagedTargets]
		if targetRecordExists {
			targetRecord := parseOwnershipRecord(targetRecordValue, true)
			released := preserveUnownedIPs(annotations[AnnotationExternalDNSTarget], targetRecord, managedPrefixes)
			releasedStr := strings.Join(released, ",")
			if annotations[AnnotationExternalDNSTarget] != releasedStr {
				if releasedStr == "" {
					delete(newAnnotations, AnnotationExternalDNSTarget)
				} else {
					newAnnotations[AnnotationExternalDNSTarget] = releasedStr
				}
			}
			// Dropping the record is itself a change, so this covers the target
			// rewrite above as well.
			delete(newAnnotations, AnnotationManagedTargets)
			updated = true
			log.Info("Released external-dns target on opt-out", "service", req.NamespacedName,
				"remainingTarget", releasedStr)
		}
		log.V(1).Info("Skipping external-dns target update (opted out via annotation)", "service", req.NamespacedName)
	}

	if updated {
		// Update last-sync annotation only when this reconcile actually changes managed annotations.
		newAnnotations[AnnotationLastSync] = time.Now().UTC().Format(time.RFC3339)
		svc.SetAnnotations(newAnnotations)
		if err := r.Update(ctx, &svc); err != nil {
			log.Error(err, "Failed to update Service annotations")
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		log.Info("Service annotations updated", "service", req.NamespacedName,
			"allIPs", finalIPsStr, "dnsTarget", finalTargetStr, "skipExternalDNS", skipExternalDNS,
			"preservedCount", len(preservedIPs), "managedCount", len(allIPs))
	}

	return r.nudgeL2Announcer(ctx, &svc, currentIP)
}

// nudgeL2Announcer makes Cilium re-read a Service's assigned LoadBalancer
// addresses by writing a fingerprint of that set back onto the Service.
//
// It works around a bug in Cilium's L2 announcer, reproduced on 1.20.0.
// pkg/l2announcer derives a Service's announced addresses from the frontend
// table, but its event loop wakes only on Service, policy, local-node and lease
// events -- never on a frontend change. On a prefix rotation the events arrive in
// the losing order: the operator's annotation update reaches the announcer first,
// while LB-IPAM has not yet assigned the new address, so the announcer stores the
// previous address set. LB-IPAM then creates the frontend, and with no further
// Service event the new address is never announced.
//
// The failure is quiet and easy to misread, because everything else is correct:
// the pool block, the lbipam.cilium.io/ips annotation, the assignment in
// status.loadBalancer.ingress and the datapath frontends all carry the address.
// Only the l2-announce table lacks it, so the address simply never answers NDP.
//
// Writing the annotation supplies the missing Service event. Fingerprinting the
// assigned set keeps this to one write per change rather than a hot loop: when
// nothing has moved the annotation already matches and no update is issued.
func (r *ServiceSyncReconciler) nudgeL2Announcer(ctx context.Context, svc *corev1.Service, currentIP string) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	annotations := svc.GetAnnotations()
	if annotations[AnnotationSkipL2Nudge] == AnnotationValueTrue {
		return ctrl.Result{}, nil
	}

	assigned := assignedLoadBalancerIPs(svc)

	// Nudging before LB-IPAM has assigned the current address would fingerprint the
	// old set and then sit still -- which is precisely the state being worked
	// around. Wait for the address to show up instead. The status write that brings
	// it re-triggers this reconcile, so the requeue is only a backstop.
	if !slices.Contains(assigned, currentIP) {
		log.V(1).Info("Current address not assigned yet, deferring L2 announcer nudge",
			"service", client.ObjectKeyFromObject(svc), "currentIP", currentIP)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	fingerprint := fingerprintAddresses(assigned)
	if annotations[AnnotationL2Nudge] == fingerprint {
		return ctrl.Result{}, nil
	}

	newAnnotations := make(map[string]string, len(annotations)+1)
	for k, v := range annotations {
		newAnnotations[k] = v
	}
	newAnnotations[AnnotationL2Nudge] = fingerprint
	svc.SetAnnotations(newAnnotations)

	if err := r.Update(ctx, svc); err != nil {
		log.Error(err, "Failed to nudge Cilium L2 announcer")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	log.Info("Nudged Cilium L2 announcer to re-read assigned addresses",
		"service", client.ObjectKeyFromObject(svc), "addresses", assigned)
	return ctrl.Result{}, nil
}

// assignedLoadBalancerIPs returns the addresses Cilium has actually assigned to
// the Service, sorted so that the fingerprint does not depend on the order the
// addresses happen to appear in status.
func assignedLoadBalancerIPs(svc *corev1.Service) []string {
	ips := make([]string, 0, len(svc.Status.LoadBalancer.Ingress))
	for _, ingress := range svc.Status.LoadBalancer.Ingress {
		if ingress.IP != "" {
			ips = append(ips, ingress.IP)
		}
	}
	slices.Sort(ips)
	return ips
}

// fingerprintAddresses reduces an address set to a short annotation value. Only
// equality is ever tested, so a non-cryptographic hash is sufficient and keeps
// the annotation short next to the addresses it stands for.
//
// The result deliberately does not depend on the order of the input: a caller
// that passed the addresses in whatever order status listed them would otherwise
// see a fresh fingerprint on most reconciles, and the operator would rewrite the
// Service forever. Sorting here makes that guarantee the function's own rather
// than something every caller has to remember.
func fingerprintAddresses(ips []string) string {
	sorted := slices.Clone(ips)
	slices.Sort(sorted)

	h := fnv.New64a()
	for _, ip := range sorted {
		_, _ = h.Write([]byte(ip))
		_, _ = h.Write([]byte{0})
	}
	return strconv.FormatUint(h.Sum64(), 16)
}

// releaseService strips the entries the operator wrote from a Service that is no
// longer opted in, and removes the records describing them.
//
// Only recorded entries are touched. Everything else in those annotations was put
// there by the user and stays exactly as it is, including addresses that merely
// resemble the operator's.
func (r *ServiceSyncReconciler) releaseService(ctx context.Context, svc *corev1.Service) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	annotations := svc.GetAnnotations()

	newAnnotations := make(map[string]string, len(annotations))
	for k, v := range annotations {
		newAnnotations[k] = v
	}

	// No DynamicPrefix to consult any more, so there are no managed prefixes to
	// fall back on: the records are the only evidence of what belonged to the
	// operator, which is exactly the case they were introduced for.
	changed := false
	release := func(fieldKey, recordKey string) {
		recordValue, exists := annotations[recordKey]
		if !exists {
			return
		}
		record := parseOwnershipRecord(recordValue, true)
		remaining := preserveUnownedIPs(annotations[fieldKey], record, nil)
		remainingStr := strings.Join(remaining, ",")
		if remainingStr == "" {
			delete(newAnnotations, fieldKey)
		} else {
			newAnnotations[fieldKey] = remainingStr
		}
		delete(newAnnotations, recordKey)
		changed = true
	}

	release(AnnotationCiliumIPs, AnnotationManagedIPs)
	release(AnnotationExternalDNSTarget, AnnotationManagedTargets)
	if changed {
		delete(newAnnotations, AnnotationLastSync)
	}

	if !changed {
		return ctrl.Result{}, nil
	}

	svc.SetAnnotations(newAnnotations)
	if err := r.Update(ctx, svc); err != nil {
		log.Error(err, "Failed to release Service annotations")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	log.Info("Released Service annotations after removal of the dynamic-prefix.io/name annotation",
		"service", client.ObjectKeyFromObject(svc))
	return ctrl.Result{}, nil
}

// collectManagedPrefixes returns all prefixes the operator manages for a
// DynamicPrefix (current + historical). These are used to identify which
// IPv6 addresses in an annotation are operator-managed vs static.
// collectManagedPrefixesForService delegates to the shared collectManagedPrefixes.
func (r *ServiceSyncReconciler) collectManagedPrefixes(dp *dynamicprefixiov1alpha1.DynamicPrefix) []netip.Prefix {
	return collectManagedPrefixes(dp)
}

// preserveUnownedIPs returns the entries of a comma-separated IP annotation that
// the operator must leave alone.
//
// When an ownership record is present it is authoritative: an entry is the
// operator's if and only if the operator recorded writing it last time. No prefix
// arithmetic is involved, so an address cannot be disowned merely because its
// prefix has aged out of the history window -- which is the bug this replaces.
//
// When no record is present the object predates this mechanism, so fall back to
// the legacy prefix test. That test over-preserves (it leaks one entry per
// rotation) but never deletes a user's address, which is the right way to be
// wrong while the record is being established. The caller writes the record on
// the same pass, so this fallback applies at most once per object.
func preserveUnownedIPs(ipsAnnotation string, record ownershipRecord, managedPrefixes []netip.Prefix) []string {
	if !record.present {
		return extractUnmanagedIPs(ipsAnnotation, managedPrefixes)
	}
	if ipsAnnotation == "" {
		return nil
	}
	var preserved []string
	for _, raw := range strings.Split(ipsAnnotation, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if !record.owns(raw) {
			preserved = append(preserved, raw)
		}
	}
	return preserved
}

// extractUnmanagedIPs parses a comma-separated IP list and returns all IPs
// that are NOT managed by the operator. An IP is considered managed if it is
// an IPv6 address that falls within any of the given managed prefixes.
//
// Deprecated for ownership decisions: this is the legacy geometric test, kept
// only as the first-pass fallback in preserveUnownedIPs. It cannot recognise an
// address whose prefix has left Status.History. See ownership.go.
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
		// No range or subnet named, so the address itself is the only thing to go
		// on. Returning it unchanged is what made this mode unable to rotate: the
		// assigned address still sits in the prefix that has just been superseded,
		// so the operator would keep requesting the old address and the Service
		// would never ask for one in the new prefix -- waiting on an assignment
		// that only arrives once something else moves first.
		//
		// Infer the host part from the assigned address instead and graft it onto
		// the current and historical prefixes, which is exactly what suffix mode
		// does once it knows the suffix.
		currentPrefixIP, allIPs = r.calculateInferredSuffixIPs(dp, currentAddr, maxHistory)
		if currentPrefixIP == "" {
			// The address is not inside any prefix the operator manages -- a
			// statically pinned address, most likely. Grafting its host bits onto
			// the delegated prefix would invent an address nobody asked for, so
			// leave it exactly as it is.
			return []string{currentServiceIP}, currentServiceIP, nil
		}
	}

	return allIPs, currentPrefixIP, nil
}

// calculateInferredSuffixIPs derives the host part of an already-assigned address
// and rebuilds it against the current and historical prefixes.
//
// Returns ("", nil) when the address does not belong to any managed prefix, which
// is the caller's signal to leave the Service alone.
func (r *ServiceSyncReconciler) calculateInferredSuffixIPs(
	dp *dynamicprefixiov1alpha1.DynamicPrefix,
	currentAddr netip.Addr,
	maxHistory int,
) (string, []string) {
	currentPrefix, err := netip.ParsePrefix(dp.Status.CurrentPrefix)
	if err != nil {
		return "", nil
	}

	// Only claim the address if it sits in a prefix the operator is responsible
	// for; the current prefix or one still within the history window.
	inManaged := currentPrefix.Contains(currentAddr)
	if !inManaged {
		for i, entry := range dp.Status.History {
			if i >= maxHistory {
				break
			}
			if p, err := netip.ParsePrefix(entry.Prefix); err == nil && p.Contains(currentAddr) {
				inManaged = true
				break
			}
		}
	}
	if !inManaged {
		return "", nil
	}

	// combinePrefixSuffix takes the high bits from the prefix and the low bits
	// from the supplied address, so the assigned address doubles as the suffix.
	suffixBytes := currentAddr.As16()
	currentIP := combinePrefixSuffix(currentPrefix, suffixBytes)
	allIPs := []string{currentIP.String()}

	for i, entry := range dp.Status.History {
		if i >= maxHistory {
			break
		}
		histPrefix, err := netip.ParsePrefix(entry.Prefix)
		if err != nil {
			continue
		}
		allIPs = append(allIPs, combinePrefixSuffix(histPrefix, suffixBytes).String())
	}

	return currentIP.String(), allIPs
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
	// ParseAddr accepts IPv4 too, and As16() then yields its v4-mapped form,
	// so "0.0.0.2" produced a plausible but wrong address instead of the error
	// this message promises. Is4 is true only for genuine IPv4 input --
	// ::ffff:0:2 is written as IPv6 and stays valid.
	if suffixAddr.Is4() {
		return nil, "", fmt.Errorf("invalid IPv6 suffix %q: not an IPv6 address", suffix)
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
		// Must be an error, not an empty result: the caller only falls back to
		// the Service's current IP when err != nil, so returning ("", nil, nil)
		// used to propagate an empty IP list all the way to the annotations.
		return "", nil, fmt.Errorf("address range %q is not defined in DynamicPrefix %q", addressRangeName, dp.Name)
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
		// See calculateAddressRangeIPs: an empty result with a nil error slips
		// past the caller's fallback and blanks the Service's annotations.
		return "", nil, fmt.Errorf("subnet %q is not defined in DynamicPrefix %q", subnetName, dp.Name)
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
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &corev1.Service{}, serviceDynamicPrefixIndex, indexServiceByDynamicPrefix); err != nil {
		return fmt.Errorf("failed to index Services by DynamicPrefix annotation: %w", err)
	}

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
		if _, ok := annotations[AnnotationName]; ok {
			return true
		}
		// Keep watching a Service that still carries operator-written entries even
		// after it was de-annotated, so the reconciler gets one more chance to
		// hand them back rather than abandoning them.
		return hasOwnershipRecord(annotations)
	})

	return ctrl.NewControllerManagedBy(mgr).
		Named("servicesync").
		For(&corev1.Service{}, builder.WithPredicates(hasAnnotation)).
		Watches(&dynamicprefixiov1alpha1.DynamicPrefix{}, handler.EnqueueRequestsFromMapFunc(r.findReferencingServices), builder.WithPredicates(dynamicPrefixDependentChangePredicate())).
		Complete(r)
}

func indexServiceByDynamicPrefix(obj client.Object) []string {
	svc, ok := obj.(*corev1.Service)
	if !ok || svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
		return nil
	}

	annotations := svc.GetAnnotations()
	if annotations == nil || annotations[AnnotationName] == "" {
		return nil
	}

	return []string{annotations[AnnotationName]}
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

	// List only Services that the field index says reference this DynamicPrefix.
	var serviceList corev1.ServiceList
	if err := r.List(ctx, &serviceList, client.MatchingFields{serviceDynamicPrefixIndex: dp.Name}); err != nil {
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
