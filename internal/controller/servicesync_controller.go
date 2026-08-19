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
	"k8s.io/client-go/tools/events"
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

	// AnnotationKubevipLBIPs is kube-vip's request-specific-addresses
	// annotation, the equivalent of AnnotationCiliumIPs for clusters whose
	// LoadBalancer addresses come from the kube-vip cloud provider.
	AnnotationKubevipLBIPs = "kube-vip.io/loadbalancerIPs"

	// AnnotationLBProvider selects which load-balancer annotation the operator
	// maintains on a Service: "cilium" (the default, so nothing changes for
	// existing installs) or "kube-vip".
	//
	// Per-Service rather than per-cluster because that is the level at which it
	// actually differs -- a cluster mid-migration runs both -- and because every
	// other ServiceSync toggle is already a Service annotation.
	AnnotationLBProvider = "dynamic-prefix.io/lb-provider"

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

	// AnnotationForceL2Nudge when set to "true" on a Service applies the L2
	// announcer nudge whatever version detection concluded.
	//
	// Detection can only be wrong in one direction that matters -- deciding a
	// Cilium is fixed when it is not -- and that mistake is silent: addresses
	// simply stop answering NDP after a rotation. This annotation is the recovery
	// for it on a fork or repackaging whose tag misreports, without waiting for
	// the operator to learn about that image. AnnotationSkipL2Nudge wins if both
	// are set, so an explicit opt-out is never overridden.
	AnnotationForceL2Nudge = "dynamic-prefix.io/force-l2-nudge"

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
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder

	// l2Nudge decides whether the running Cilium still needs the L2 announcer
	// nudge. Populated by SetupWithManager; a nil decider nudges unconditionally,
	// which is the safe direction and keeps tests that construct the reconciler
	// directly working unchanged.
	l2Nudge l2NudgeDecider
}

// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=apps,resources=daemonsets,verbs=list

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
			return r.releaseService(ctx, &svc, "the dynamic-prefix.io/name annotation was removed")
		}
		return ctrl.Result{}, nil
	}

	// Fetch the referenced DynamicPrefix
	var dp dynamicprefixiov1alpha1.DynamicPrefix
	if err := r.Get(ctx, types.NamespacedName{Name: dpName}, &dp); err != nil {
		if apierrors.IsNotFound(err) {
			// Polling for a DynamicPrefix that has been deleted never succeeds,
			// and meanwhile the Service keeps an external-dns target that stops
			// resolving at the next rotation. Hand the entries back instead.
			if hasOwnershipRecord(annotations) {
				return r.releaseService(ctx, &svc,
					fmt.Sprintf("DynamicPrefix %s no longer exists", dpName))
			}
			log.V(1).Info("Referenced DynamicPrefix not found, will retry", "name", dpName)
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		// Returned so a persistent API failure backs off and reaches
		// controller_runtime_reconcile_errors_total instead of retrying flat.
		return ctrl.Result{}, fmt.Errorf("failed to get DynamicPrefix %s: %w", dpName, err)
	}

	// Check if HA mode is enabled
	if dp.Spec.Transition == nil || dp.Spec.Transition.Mode != dynamicprefixiov1alpha1.TransitionModeHA {
		// HA mode was switched off. The addresses the operator put in
		// lbipam.cilium.io/ips and the external-dns target are still there and
		// stop being maintained from here on, so hand them back rather than
		// leaving them to go stale at the next rotation.
		if hasOwnershipRecord(annotations) {
			return r.releaseService(ctx, &svc,
				fmt.Sprintf("DynamicPrefix %s is no longer in HA mode", dpName))
		}
		return ctrl.Result{}, nil
	}

	if dp.Status.CurrentPrefix == "" {
		// Waiting for the first advertisement, which is the same kind of "not ready
		// yet" as the paths above, not a failure to report and back off from.
		log.V(1).Info("DynamicPrefix has not acquired a prefix yet, will retry", "name", dpName)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	log.V(1).Info("Syncing Service for HA mode", "service", req.NamespacedName, "dynamicPrefix", dpName)

	allIPs, currentIP, wait, err := r.addressesForService(ctx, &dp, &svc, annotations)
	if err != nil {
		return ctrl.Result{}, err
	}
	if wait > 0 {
		return ctrl.Result{RequeueAfter: wait}, nil
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

	provider, lbAddressField, err := lbAddressAnnotationFor(annotations)
	if err != nil {
		emitWarningEvent(r.Recorder, &svc, eventReasonInvalidLBProvider, err.Error())
		return ctrl.Result{}, err
	}

	ipsRecordValue, ipsRecordExists := annotations[AnnotationManagedIPs]
	ipsRecord := parseOwnershipRecord(ipsRecordValue, ipsRecordExists)

	existingIPs := annotations[lbAddressField]
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

	finalIPsStr := strings.Join(finalIPs, ",")
	if writeLBAddresses(annotations, newAnnotations, lbAddressField, finalIPsStr, ipsRecord, managedPrefixes) {
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
	// Set when the target is withheld because the address is not assigned yet, so
	// the reconcile ends in a requeue instead of waiting for an event that has
	// already happened.
	var deferredDNSTarget bool
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

		// DNS follows the address; it does not lead it. The LB-IPAM request
		// above is what makes the new address exist, and it is not answerable
		// until Cilium has assigned it and started announcing it -- which is a
		// later reconcile, woken by the status write. Publishing the target in
		// the same pass put the new address into DNS while nothing answered on
		// it, so every rotation opened a window where the name resolved and the
		// connection was refused.
		//
		// Holding the old target through that window is the better failure: it
		// keeps pointing at an address that still works. If the address is never
		// assigned at all, DNS simply never moves, which is also what should
		// happen.
		// Deferred only when the Service is publishing assigned addresses and the
		// current one is not among them yet -- which is exactly the rotation
		// window. A Service with no assigned addresses at all is a different
		// situation: some providers never populate status, and withholding the
		// target there would mean never publishing DNS at all.
		assignedIPs := assignedLoadBalancerIPs(&svc)
		dnsTargetReady := len(assignedIPs) == 0 || slices.Contains(assignedIPs, currentIP)
		switch {
		case annotations[AnnotationExternalDNSTarget] == finalTargetStr:
			// Already where it should be.
		case dnsTargetReady:
			newAnnotations[AnnotationExternalDNSTarget] = finalTargetStr
			updated = true
		default:
			log.V(1).Info("Deferring external-dns target until the address is assigned",
				"service", req.NamespacedName, "currentIP", currentIP, "assigned", assignedIPs)
			deferredDNSTarget = true
		}

		// The ownership record tracks what the operator has actually published,
		// so it moves with the target rather than ahead of it. Recording the new
		// address while the annotation still holds the old one would make the
		// old entry look unowned, and the next pass would preserve it forever.
		if newAnnotations[AnnotationExternalDNSTarget] == finalTargetStr {
			managedTargetStr := formatOwnershipRecord(excludePinned([]string{currentIP}, preservedTargets))
			if annotations[AnnotationManagedTargets] != managedTargetStr {
				newAnnotations[AnnotationManagedTargets] = managedTargetStr
				updated = true
			}
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
			// A conflict means another writer won and the annotations have to be
			// recomputed against the new state; anything else needs to back off
			// and be counted, which returning the error does and a flat retry
			// does not.
			return ctrl.Result{}, fmt.Errorf("failed to update Service annotations: %w", err)
		}
		log.Info("Service annotations updated", "service", req.NamespacedName,
			"allIPs", finalIPsStr, "dnsTarget", finalTargetStr, "skipExternalDNS", skipExternalDNS,
			"preservedCount", len(preservedIPs), "managedCount", len(allIPs))
	}

	if provider != lbProviderCilium {
		// The nudge works around a bug in Cilium's L2 announcer. On a cluster
		// whose addresses come from somewhere else there is no announcer to
		// nudge, and the fingerprint annotation would be noise on every change.
		return requeueIfDNSTargetDeferred(ctrl.Result{}, deferredDNSTarget), nil
	}

	result, err := r.nudgeL2Announcer(ctx, &svc, currentIP)
	if err != nil {
		return result, err
	}
	return requeueIfDNSTargetDeferred(result, deferredDNSTarget), nil
}

// requeueIfDNSTargetDeferred keeps a withheld external-dns target from waiting
// on an event that may never come. The address showing up in status normally
// wakes another reconcile, but if it was already there and something else held
// the target back, nothing further would arrive. A minute matches the L2 nudge's
// backstop for the same reason: it is cheap enough to sit on indefinitely.
func requeueIfDNSTargetDeferred(result ctrl.Result, deferred bool) ctrl.Result {
	if !deferred || result.Requeue || result.RequeueAfter > 0 {
		return result
	}
	result.RequeueAfter = time.Minute
	return result
}

// writeLBAddresses puts the calculated addresses into the annotation this
// Service's provider reads, and takes the operator's entries out of the other
// provider's. Flipping the provider annotation otherwise leaves them behind,
// requesting a prefix that stops existing at the next rotation.
func writeLBAddresses(
	annotations, newAnnotations map[string]string,
	field, addresses string,
	record ownershipRecord,
	managedPrefixes []netip.Prefix,
) bool {
	updated := false

	if annotations[field] != addresses {
		newAnnotations[field] = addresses
		updated = true
	}

	abandoned := otherLBAddressAnnotation(field)
	if annotations[abandoned] == "" {
		return updated
	}

	remaining := strings.Join(preserveUnownedIPs(annotations[abandoned], record, managedPrefixes), ",")
	if remaining == "" {
		delete(newAnnotations, abandoned)
	} else {
		newAnnotations[abandoned] = remaining
	}
	return true
}

// Load-balancer providers the operator can write addresses for.
const (
	lbProviderCilium  = "cilium"
	lbProviderKubevip = "kube-vip"
)

// lbAddressAnnotationFor reports which annotation carries this Service's
// requested addresses, defaulting to Cilium's so existing Services are
// unaffected.
func lbAddressAnnotationFor(annotations map[string]string) (provider, field string, err error) {
	switch value := strings.TrimSpace(annotations[AnnotationLBProvider]); value {
	case "", lbProviderCilium:
		return lbProviderCilium, AnnotationCiliumIPs, nil
	case lbProviderKubevip:
		return lbProviderKubevip, AnnotationKubevipLBIPs, nil
	default:
		return "", "", fmt.Errorf("%s=%q is not a load-balancer provider this operator writes addresses for; use %q or %q",
			AnnotationLBProvider, value, lbProviderCilium, lbProviderKubevip)
	}
}

// otherLBAddressAnnotation is the field the selected provider is not using.
// Flipping the annotation has to take the addresses with it, or they stay
// behind requesting a prefix that will not exist after the next rotation.
func otherLBAddressAnnotation(field string) string {
	if field == AnnotationCiliumIPs {
		return AnnotationKubevipLBIPs
	}
	return AnnotationCiliumIPs
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

	if annotations[AnnotationForceL2Nudge] != AnnotationValueTrue && r.l2Nudge != nil {
		if needed, reason := r.l2Nudge.Needed(ctx); !needed {
			log.V(1).Info("Skipping L2 announcer nudge", "service", client.ObjectKeyFromObject(svc), "reason", reason)
			return ctrl.Result{}, nil
		}
	}

	assigned := assignedLoadBalancerIPs(svc)

	// Nudging before LB-IPAM has assigned the current address would fingerprint the
	// old set and then sit still -- which is precisely the state being worked
	// around. Wait for the address to show up instead. The status write that brings
	// it re-triggers this reconcile, so the requeue is only a backstop.
	//
	// It is a backstop that has to stay cheap: if LB-IPAM never assigns this
	// address -- no pool block covers it, the pool is exhausted, another Service
	// holds it -- then this waits forever. A minute rather than ten seconds keeps
	// that from being six wakeups a minute per Service for the life of the
	// process, and the reason is stated on the Service so the wait is diagnosable
	// rather than merely quiet.
	if !slices.Contains(assigned, currentIP) {
		log.V(1).Info("Current address not assigned yet, deferring L2 announcer nudge",
			"service", client.ObjectKeyFromObject(svc), "currentIP", currentIP, "assigned", assigned)
		return ctrl.Result{RequeueAfter: time.Minute}, nil
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
		// Returned rather than swallowed: a conflict here means another writer
		// touched the Service and the nudge has to be recomputed against the new
		// state, and a persistent failure has to reach the error metric instead
		// of retrying flat forever.
		return ctrl.Result{}, fmt.Errorf("failed to nudge Cilium L2 announcer: %w", err)
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

// releaseService strips the entries the operator wrote from a Service it no
// longer manages, and removes the records describing them. The reason is logged,
// since a Service can reach this through any of three routes: it was
// de-annotated, its DynamicPrefix was deleted, or HA mode was switched off.
//
// Only recorded entries are touched. Everything else in those annotations was put
// there by the user and stays exactly as it is, including addresses that merely
// resemble the operator's.
func (r *ServiceSyncReconciler) releaseService(ctx context.Context, svc *corev1.Service, reason string) (ctrl.Result, error) {
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

	// Both provider fields, because the record names addresses rather than the
	// field they went into, and the Service may have been flipped between them.
	release(AnnotationCiliumIPs, AnnotationManagedIPs)
	release(AnnotationKubevipLBIPs, AnnotationManagedIPs)
	release(AnnotationExternalDNSTarget, AnnotationManagedTargets)
	if changed {
		delete(newAnnotations, AnnotationLastSync)
		// The nudge fingerprint describes addresses the operator no longer
		// maintains, and nothing reads it once the records are gone. Leaving it
		// behind would strand an operator-written annotation on an object that
		// has otherwise been handed back.
		delete(newAnnotations, AnnotationL2Nudge)
	}

	if !changed {
		return ctrl.Result{}, nil
	}

	svc.SetAnnotations(newAnnotations)
	if err := r.Update(ctx, svc); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to release Service annotations: %w", err)
	}
	log.Info("Released Service annotations",
		"service", client.ObjectKeyFromObject(svc), "reason", reason)
	emitNormalEvent(r.Recorder, svc, eventReasonServiceReleased,
		fmt.Sprintf("Released the annotations this operator wrote to Service %s: %s",
			client.ObjectKeyFromObject(svc), reason))
	return ctrl.Result{}, nil
}

// addressesForService calculates the addresses this Service should carry, in
// whichever of the two modes it is configured for. A non-zero wait means the
// answer is not available yet and the reconcile should requeue rather than
// treat the absence as an error.
func (r *ServiceSyncReconciler) addressesForService(
	ctx context.Context,
	dp *dynamicprefixiov1alpha1.DynamicPrefix,
	svc *corev1.Service,
	annotations map[string]string,
) (allIPs []string, currentIP string, wait time.Duration, err error) {
	log := logf.FromContext(ctx)

	if suffix, ok := annotations[AnnotationSuffix]; ok && suffix != "" {
		// Suffix-based mode: calculate full IPv6 from prefix + suffix directly.
		// This is the preferred path -- no need to wait for Cilium to assign an IP first.
		allIPs, currentIP, err = r.calculateSuffixIPs(dp, suffix)
		if err != nil {
			// A malformed suffix annotation never fixes itself, so this is returned
			// to back off and be counted rather than retried flat and silently.
			return nil, "", 0, fmt.Errorf("failed to calculate IPs from suffix %q: %w", suffix, err)
		}
		log.V(1).Info("Using suffix-based IP calculation", "suffix", suffix, "currentIP", currentIP)
		return allIPs, currentIP, 0, nil
	}

	// Legacy mode: infer suffix from the Service's currently assigned IP.
	currentServiceIP := r.getCurrentServiceIP(svc)
	if currentServiceIP == "" {
		log.V(1).Info("Service has no IP assigned yet, skipping")
		return nil, "", 5 * time.Second, nil
	}

	allIPs, currentIP, err = r.calculateServiceIPs(ctx, dp, svc, currentServiceIP)
	if err != nil {
		return nil, "", 0, fmt.Errorf("failed to calculate Service IPs: %w", err)
	}
	return allIPs, currentIP, 0, nil
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
		// the Service's current IP when err != nil, so ("", nil, nil) would
		// propagate an empty IP list all the way to the annotations.
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
	offset, ok := r.calculateIPOffset(currentRange.Start, currentAddr)
	if !ok {
		// The Service holds an address below the range it is supposed to come
		// from -- a pin, or a range narrowed after assignment. There is no
		// meaningful offset to carry to the historical prefixes, and computing
		// one anyway produces addresses outside every managed prefix.
		return "", nil, fmt.Errorf("assigned address %s is below address range %q (starts at %s)",
			currentAddr, rangeSpec.Name, currentRange.Start)
	}

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

		histIP, ok := r.offsetAddressWithin(histRange.Start, histRange.End, offset)
		if !ok {
			continue
		}
		allIPs = append(allIPs, histIP.String())
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
	offset, ok := r.calculateIPOffset(currentSubnet.CIDR.Addr(), currentAddr)
	if !ok {
		return "", nil, fmt.Errorf("assigned address %s is below subnet %q (%s)",
			currentAddr, subnetSpec.Name, currentSubnet.CIDR)
	}

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

		histLast, err := lastAddrOfPrefix(histSubnet.CIDR)
		if err != nil {
			continue
		}

		histIP, ok := r.offsetAddressWithin(histSubnet.CIDR.Addr(), histLast, offset)
		if !ok {
			continue
		}
		allIPs = append(allIPs, histIP.String())
	}

	return currentPrefixIP, allIPs, nil
}

// calculateIPOffset calculates the offset between two IPv6 addresses.
// The second return reports whether the target actually sits at or above the
// base. A target below it yields a borrow out of the top byte -- the two's
// complement of the real distance, a number just under 2^128 -- and adding that
// to a historical base wraps back around to an address unrelated to any prefix
// the operator manages. Since the result is then written into
// lbipam.cilium.io/ips *and recorded as the operator's*, it is not enough for it
// to be harmless: the operator would be claiming an address it has no business
// owning. Callers skip the entry instead.
func (r *ServiceSyncReconciler) calculateIPOffset(base, target netip.Addr) ([16]byte, bool) {
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
		// diff has just been normalised into [0, 255] by the borrow above, so
		// this is the byte it already is.
		offset[i] = byte(diff) // #nosec G115 -- diff is in [0,255] after the borrow adjustment
	}

	return offset, borrow == 0
}

// applyIPOffset applies an offset to an IPv6 address, reporting false if the
// addition carried past 128 bits and wrapped.
func (r *ServiceSyncReconciler) applyIPOffset(base netip.Addr, offset [16]byte) (netip.Addr, bool) {
	baseBytes := base.As16()
	var result [16]byte

	carry := uint16(0)
	for i := 15; i >= 0; i-- {
		sum := uint16(baseBytes[i]) + uint16(offset[i]) + carry
		result[i] = byte(sum & 0xFF)
		carry = sum >> 8
	}

	return netip.AddrFrom16(result), carry == 0
}

// offsetAddressWithin applies an offset to base and keeps the result only if it
// is still inside the window the operator manages, given by base and last
// inclusive.
//
// netip.AddrFrom16 always returns a valid address, so validity is not the
// question worth asking here: an address outside the managed window is
// well-formed and still wrong.
func (r *ServiceSyncReconciler) offsetAddressWithin(base, last netip.Addr, offset [16]byte) (netip.Addr, bool) {
	addr, ok := r.applyIPOffset(base, offset)
	if !ok {
		return netip.Addr{}, false
	}
	if addr.Less(base) || last.Less(addr) {
		return netip.Addr{}, false
	}
	return addr, true
}

// SetupWithManager sets up the controller with the Manager.
func (r *ServiceSyncReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &corev1.Service{}, serviceDynamicPrefixIndex, indexServiceByDynamicPrefix); err != nil {
		return fmt.Errorf("failed to index Services by DynamicPrefix annotation: %w", err)
	}

	// Read the agent DaemonSet uncached: it is consulted once every few minutes,
	// and going through the manager's cache would start an informer over every
	// DaemonSet in the cluster to answer it.
	r.l2Nudge = newL2NudgeDetector(mgr.GetAPIReader())

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

	// Deliberately not filtered on HA mode. Switching a DynamicPrefix out of HA
	// mode, or deleting it, is exactly when the Services it managed need to hear
	// about it -- refusing to fan out for a non-HA prefix meant the annotations
	// the operator had written were simply abandoned. Reconcile decides what to
	// do; this only decides who is told.
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
