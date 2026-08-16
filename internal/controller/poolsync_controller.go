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
	"errors"
	"fmt"
	"net/netip"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
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
	// AnnotationName references the DynamicPrefix CR name.
	AnnotationName = "dynamic-prefix.io/name"
	// AnnotationSubnet specifies which subnet from status.subnets to use (Mode 2).
	AnnotationSubnet = "dynamic-prefix.io/subnet"
	// AnnotationAddressRange specifies which address range from status.addressRanges to use (Mode 1).
	AnnotationAddressRange = "dynamic-prefix.io/address-range"
	// AnnotationKubevipKey names which key of the kube-vip pool ConfigMap this
	// binding manages, e.g. "cidr-global" or "range-production".
	AnnotationKubevipKey = "dynamic-prefix.io/kubevip-key"

	// AnnotationLastSync is the timestamp set by operator after update.
	AnnotationLastSync = "dynamic-prefix.io/last-sync"
)

// Default GVKs used when CiliumVersions is not injected (e.g. in tests).
var (
	DefaultCiliumLBIPPoolGVK = schema.GroupVersionKind{
		Group:   "cilium.io",
		Version: "v2",
		Kind:    "CiliumLoadBalancerIPPool",
	}
	DefaultCiliumCIDRGroupGVK = schema.GroupVersionKind{
		Group:   "cilium.io",
		Version: "v2",
		Kind:    "CiliumCIDRGroup",
	}
	DefaultMetalLBIPAddressPoolGVK = schema.GroupVersionKind{
		Group:   "metallb.io",
		Version: "v1beta1",
		Kind:    "IPAddressPool",
	}
	DefaultCalicoIPPoolGVK = schema.GroupVersionKind{
		Group:   "projectcalico.org",
		Version: "v3",
		Kind:    "IPPool",
	}
)

// poolConfiguration holds the resolved configuration for a pool update.
type poolConfiguration struct {
	// useAddressRange indicates whether to use start/end addresses (true) or CIDR (false).
	useAddressRange bool
	// start is the first address in the range (Mode 1 only).
	start string
	// end is the last address in the range (Mode 1 only).
	end string
	// cidr is the CIDR notation (Mode 2 or fallback).
	cidr string
}

// PoolSyncReconciler reconciles supported pool backend resources annotated with dynamic-prefix.io annotations.
type PoolSyncReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
	// CiliumVersions holds the resolved Cilium API versions. If nil, defaults are used.
	CiliumVersions *CiliumVersions
	// BackendGVKs holds discovered pool backend resources. If empty, the reconciler
	// falls back to the default Cilium resources for tests and backward compatibility.
	BackendGVKs []schema.GroupVersionKind

	// poolState aggregates per-pool sync outcomes into the PoolsSynced condition
	// on each DynamicPrefix.
	poolState poolSyncState
}

// lbIPPoolGVK returns the GVK for CiliumLoadBalancerIPPool.
func (r *PoolSyncReconciler) lbIPPoolGVK() schema.GroupVersionKind {
	if r.CiliumVersions != nil {
		return r.CiliumVersions.LoadBalancerIPPool
	}
	return DefaultCiliumLBIPPoolGVK
}

// cidrGroupGVK returns the GVK for CiliumCIDRGroup.
func (r *PoolSyncReconciler) cidrGroupGVK() schema.GroupVersionKind {
	if r.CiliumVersions != nil {
		return r.CiliumVersions.CIDRGroup
	}
	return DefaultCiliumCIDRGroupGVK
}

// +kubebuilder:rbac:groups=cilium.io,resources=ciliumloadbalancerippools,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=cilium.io,resources=ciliumcidrgroups,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=metallb.io,resources=ipaddresspools,verbs=get;list;watch;update;patch
// Calico needs create/delete on top of the usual verbs: its spec.cidr holds a
// single prefix, so a draining prefix has to live in a separate IPPool object that
// the operator creates and removes as the history window moves.
// +kubebuilder:rbac:groups=projectcalico.org,resources=ippools,verbs=get;list;watch;update;patch;create;delete

// Reconcile handles pool synchronization for annotated backend resources.
//
// A reconcile.Request carries only a name, so every backend of matching scope is
// synced rather than just the first one found. Several backends are cluster-scoped,
// and stopping at the first match would leave a pool sharing its name with another
// kind unreconciled -- invisibly, because the other Get succeeds.
func (r *PoolSyncReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	pools, err := r.getPools(ctx, req.NamespacedName)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if len(pools) == 0 {
		// No backend object of any kind answers to this name any more, so nothing
		// will ever reconcile it through the success path. Drop whatever failure
		// state it left behind, or the condition goes on naming a pool that is
		// gone.
		r.releasePoolsSyncedEntries(ctx, func(key poolStateKey) bool {
			return key.pool == req.String()
		})
		return ctrl.Result{}, nil
	}

	var errs []error
	var result ctrl.Result
	for _, match := range pools {
		res, err := r.syncPool(ctx, req, match.pool, match.backend)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		// Keep the soonest requeue any backend asked for.
		if res.RequeueAfter > 0 && (result.RequeueAfter == 0 || res.RequeueAfter < result.RequeueAfter) {
			result.RequeueAfter = res.RequeueAfter
		}
	}
	if len(errs) > 0 {
		return ctrl.Result{}, errors.Join(errs...)
	}
	return result, nil
}

// syncPool reconciles one pool object against its DynamicPrefix.
func (r *PoolSyncReconciler) syncPool(ctx context.Context, req ctrl.Request, pool *unstructured.Unstructured, backend poolBackend) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Get annotations
	annotations := pool.GetAnnotations()
	if annotations == nil {
		return ctrl.Result{}, nil
	}

	dpName, hasName := annotations[AnnotationName]
	subnetName, hasSubnet := annotations[AnnotationSubnet]
	addressRangeName, hasAddressRange := annotations[AnnotationAddressRange]

	if !hasName {
		// De-annotated. The operator's entries are still in the pool and the
		// addresses in them stop being routable at the next rotation, so release
		// them instead of leaving them to rot.
		if hasOwnershipRecord(annotations) {
			return ctrl.Result{}, r.releasePool(ctx, pool, backend, "the dynamic-prefix.io/name annotation was removed")
		}
		return ctrl.Result{}, nil
	}

	log.Info("Syncing pool", "backend", backend.name(), "pool", req.Name, "dynamicPrefix", dpName, "subnet", subnetName, "addressRange", addressRangeName)

	stateKey := poolStateKey{backend: backend.name(), pool: req.String()}

	// Fetch the referenced DynamicPrefix
	var dp dynamicprefixiov1alpha1.DynamicPrefix
	if err := r.Get(ctx, types.NamespacedName{Name: dpName}, &dp); err != nil {
		if apierrors.IsNotFound(err) {
			// The DynamicPrefix is gone for good. Retrying cannot bring it back,
			// and the entries the operator wrote are still in the pool, pointing
			// at a prefix nothing maintains any more -- so hand them back rather
			// than error-looping forever with the pool left as it was.
			if hasOwnershipRecord(annotations) {
				return ctrl.Result{}, r.releasePool(ctx, pool, backend,
					fmt.Sprintf("DynamicPrefix %s no longer exists", dpName))
			}
			log.V(1).Info("Referenced DynamicPrefix does not exist", "name", dpName)
			return ctrl.Result{}, nil
		}
		log.Error(err, "Failed to get DynamicPrefix", "name", dpName)
		// Returning nil here hid API failures from
		// controller_runtime_reconcile_errors_total and replaced
		// controller-runtime's exponential backoff with a flat 30s retry, so a
		// permanently failing sync retried forever, invisibly.
		return ctrl.Result{}, err
	}

	if dp.Status.CurrentPrefix == "" {
		// Waiting for the first advertisement, not failing. Treated as an error it
		// would raise a Warning on every pool, park the condition at False and back
		// off exponentially through the whole of a fresh install. The DynamicPrefix
		// watch re-enqueues this pool when the prefix lands; the requeue is only a
		// backstop.
		log.V(1).Info("DynamicPrefix has not acquired a prefix yet",
			"pool", req.Name, "dynamicPrefix", dpName)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Build pool configurations for current prefix and historical prefixes
	configs, err := r.buildPoolConfigurations(ctx, &dp, hasAddressRange, addressRangeName, hasSubnet, subnetName)
	if err != nil {
		// A misspelled address-range or subnet name lands here and never resolves
		// on its own. Polling silently every 10 seconds left the pool unsynced
		// with nothing to show for it: no event on the pool, no condition, and
		// nothing in the error metric. Say so on the object, and let the error
		// back off rather than spin.
		emitWarningEvent(r.Recorder, pool, eventReasonPoolSyncFailed,
			fmt.Sprintf("Cannot build configurations for %s pool %s from DynamicPrefix %s: %v",
				backend.name(), req.Name, dpName, err))
		recordPoolSyncFailedMetric(backend.name(), dpName, req.String())
		r.updatePoolsSyncedCondition(ctx, dpName, stateKey, err)
		return ctrl.Result{}, fmt.Errorf("failed to build pool configurations for %s: %w", req.Name, err)
	}

	// Collect all managed prefixes for block preservation logic
	managedPrefixes := collectManagedPrefixes(&dp)

	updated, updateErr := backend.update(ctx, r, pool, configs, managedPrefixes)

	if updateErr != nil {
		log.Error(updateErr, "Failed to update pool")
		// Surfaced so conflicts retry promptly with backoff and rejected
		// updates (immutable fields, webhooks) become visible in the metrics.
		recordPoolSyncFailedMetric(backend.name(), dpName, req.String())
		r.updatePoolsSyncedCondition(ctx, dpName, stateKey, updateErr)
		return ctrl.Result{}, updateErr
	}

	if updated {
		log.Info("Pool updated successfully", "backend", backend.name(), "pool", req.Name, "blockCount", len(configs))
		emitNormalEvent(r.Recorder, pool, eventReasonPoolUpdated,
			fmt.Sprintf("Synced %s pool %s from DynamicPrefix %s with %d managed configuration(s)", backend.name(), req.Name, dpName, len(configs)))
	} else {
		log.Info("Pool already up-to-date", "backend", backend.name(), "pool", req.Name, "blockCount", len(configs))
	}
	recordPoolSyncedMetric(backend.name(), dpName, req.String())
	r.updatePoolsSyncedCondition(ctx, dpName, stateKey, nil)
	return ctrl.Result{}, nil
}

// releasePool strips the entries the operator wrote from a pool that is no longer
// opted in, and removes the records describing them.
//
// The records are the only evidence of what was the operator's -- there is no
// DynamicPrefix left to consult -- so anything not named in them was the user's
// and is left untouched.
func (r *PoolSyncReconciler) releasePool(
	ctx context.Context,
	pool *unstructured.Unstructured,
	backend poolBackend,
	reason string,
) error {
	log := logf.FromContext(ctx)

	// Dispatch on the backend that matched, not on which record annotation
	// happens to be present: the two disagree when a pool carries another
	// backend's record, and only the backend knows which field its record
	// describes.
	changed, err := backend.release(ctx, r, pool)
	if err != nil {
		return fmt.Errorf("failed to release %s pool %s: %w", backend.name(), pool.GetName(), err)
	}

	// Records belonging to other backends are dropped too. Only the matched
	// backend can undo its own writes, but any record left behind keeps the
	// object inside the watch filter for a binding that no longer exists.
	annotations := pool.GetAnnotations()
	strayRecords := false
	for _, key := range []string{
		AnnotationManagedBlocks,
		AnnotationManagedCIDRs,
		AnnotationManagedAddresses,
		AnnotationManagedCIDR,
		AnnotationManagedKubevipEntries,
		AnnotationLastSync,
	} {
		if _, ok := annotations[key]; ok {
			delete(annotations, key)
			strayRecords = true
		}
	}
	if !changed && !strayRecords {
		return nil
	}
	pool.SetAnnotations(annotations)

	poolKey := client.ObjectKeyFromObject(pool).String()
	if err := r.Update(ctx, pool); err != nil {
		return fmt.Errorf("failed to release pool %s: %w", pool.GetName(), err)
	}
	forgetPoolMetrics(backend.name(), poolKey)
	r.releasePoolsSyncedEntries(ctx, func(key poolStateKey) bool {
		return key.backend == backend.name() && key.pool == poolKey
	})
	log.Info("Released pool entries", "pool", pool.GetName(), "backend", backend.name(), "reason", reason)
	emitNormalEvent(r.Recorder, pool, eventReasonPoolReleased,
		fmt.Sprintf("Released the entries this operator wrote to %s pool %s: %s", backend.name(), pool.GetName(), reason))
	return nil
}

// poolMatch pairs a fetched pool object with the backend that understands it.
type poolMatch struct {
	pool    *unstructured.Unstructured
	backend poolBackend
}

// getPools returns every backend object that exists under the requested name.
//
// Only backends whose scope matches the request are probed. A reconcile.Request
// carries no GVK, and the API server drops the namespace when reading a
// cluster-scoped resource, so probing every backend would let a request for a
// namespaced pool (MetalLB's IPAddressPool) match a same-named cluster-scoped
// one (Cilium's CiliumLoadBalancerIPPool).
//
// Within a scope the name is still ambiguous: several backends are cluster-scoped,
// and nothing stops a CiliumLoadBalancerIPPool and a CiliumCIDRGroup from sharing
// a name. Returning only the first match would leave whichever kind lost the
// discovery-order race unreconciled, with no error to show for it, so all matches
// are returned and each is synced independently.
func (r *PoolSyncReconciler) getPools(ctx context.Context, name types.NamespacedName) ([]poolMatch, error) {
	var matches []poolMatch
	var lastErr error
	wantNamespaced := name.Namespace != ""
	for _, backend := range r.poolBackends() {
		if backend.namespaced() != wantNamespaced {
			continue
		}
		pool := &unstructured.Unstructured{}
		pool.SetGroupVersionKind(backend.gvk())
		if err := r.Get(ctx, name, pool); err != nil {
			if client.IgnoreNotFound(err) != nil {
				lastErr = err
			}
			continue
		}
		matches = append(matches, poolMatch{pool: pool, backend: backend})
	}
	if len(matches) == 0 {
		return nil, lastErr
	}
	return matches, nil
}

// buildPoolConfigurations builds pool configurations for current prefix and historical prefixes.
func (r *PoolSyncReconciler) buildPoolConfigurations(
	ctx context.Context,
	dp *dynamicprefixiov1alpha1.DynamicPrefix,
	hasAddressRange bool,
	addressRangeName string,
	hasSubnet bool,
	subnetName string,
) ([]poolConfiguration, error) {
	if dp.Status.CurrentPrefix == "" {
		return nil, fmt.Errorf("DynamicPrefix has no current prefix")
	}

	maxHistory := r.getMaxHistory(dp)

	if hasAddressRange && addressRangeName != "" {
		return r.buildAddressRangeConfigs(ctx, dp, addressRangeName, maxHistory)
	}

	if hasSubnet && subnetName != "" {
		return r.buildSubnetConfigs(ctx, dp, subnetName, maxHistory)
	}

	return r.buildRawPrefixConfigs(dp, maxHistory), nil
}

// getMaxHistory returns the maximum number of historical prefixes to retain.
func (r *PoolSyncReconciler) getMaxHistory(dp *dynamicprefixiov1alpha1.DynamicPrefix) int {
	if dp.Spec.Transition != nil && dp.Spec.Transition.MaxPrefixHistory > 0 {
		return dp.Spec.Transition.MaxPrefixHistory
	}
	return 2 // Default
}

// buildModeConfigs builds the configurations for one named address range or
// subnet: the current prefix first, then one per historical prefix.
//
// Address-range mode and subnet mode differ only in which spec they look up and
// which calculation they run, so they share this.
func buildModeConfigs[S any](
	ctx context.Context,
	dp *dynamicprefixiov1alpha1.DynamicPrefix,
	kind, name string,
	maxHistory int,
	findSpec func(*dynamicprefixiov1alpha1.DynamicPrefix, string) *S,
	findInStatus func(*dynamicprefixiov1alpha1.DynamicPrefix, string) *poolConfiguration,
	calculate func(basePrefix string, spec *S) (poolConfiguration, error),
) ([]poolConfiguration, error) {
	log := logf.FromContext(ctx)
	var configs []poolConfiguration

	spec := findSpec(dp, name)

	// Prefer what status already resolved; fall back to calculating it.
	currentConfig := findInStatus(dp, name)
	if currentConfig == nil {
		if spec == nil {
			return nil, fmt.Errorf("%s %q not found in status or spec", kind, name)
		}
		calculated, err := calculate(dp.Status.CurrentPrefix, spec)
		if err != nil {
			return nil, fmt.Errorf("failed to calculate %s for current prefix: %w", kind, err)
		}
		currentConfig = &calculated
	}
	configs = append(configs, *currentConfig)

	if spec == nil {
		return configs, nil
	}

	for i, histEntry := range dp.Status.History {
		if i >= maxHistory {
			break
		}
		histConfig, err := calculate(histEntry.Prefix, spec)
		if err != nil {
			// One unusable historical prefix must not cost the current one.
			log.V(1).Info("Failed to calculate configuration for historical prefix",
				"kind", kind, "prefix", histEntry.Prefix, "error", err.Error())
			continue
		}
		configs = append(configs, histConfig)
	}

	return configs, nil
}

// buildAddressRangeConfigs builds configurations for address range mode.
func (r *PoolSyncReconciler) buildAddressRangeConfigs(
	ctx context.Context,
	dp *dynamicprefixiov1alpha1.DynamicPrefix,
	addressRangeName string,
	maxHistory int,
) ([]poolConfiguration, error) {
	return buildModeConfigs(ctx, dp, "address range", addressRangeName, maxHistory,
		r.findAddressRangeSpec, r.findAddressRangeInStatus, r.calculateAddressRangeConfig)
}

// buildSubnetConfigs builds configurations for subnet mode.
func (r *PoolSyncReconciler) buildSubnetConfigs(
	ctx context.Context,
	dp *dynamicprefixiov1alpha1.DynamicPrefix,
	subnetName string,
	maxHistory int,
) ([]poolConfiguration, error) {
	return buildModeConfigs(ctx, dp, "subnet", subnetName, maxHistory,
		r.findSubnetSpec, r.findSubnetInStatus, r.calculateSubnetConfig)
}

// buildRawPrefixConfigs builds configurations using raw prefixes (no address range or subnet).
func (r *PoolSyncReconciler) buildRawPrefixConfigs(
	dp *dynamicprefixiov1alpha1.DynamicPrefix,
	maxHistory int,
) []poolConfiguration {
	configs := []poolConfiguration{{
		useAddressRange: false,
		cidr:            dp.Status.CurrentPrefix,
	}}

	for i, histEntry := range dp.Status.History {
		if i >= maxHistory {
			break
		}
		configs = append(configs, poolConfiguration{
			useAddressRange: false,
			cidr:            histEntry.Prefix,
		})
	}

	return configs
}

// findAddressRangeSpec finds an address range spec by name.
func (r *PoolSyncReconciler) findAddressRangeSpec(
	dp *dynamicprefixiov1alpha1.DynamicPrefix,
	name string,
) *dynamicprefixiov1alpha1.AddressRangeSpec {
	for i := range dp.Spec.AddressRanges {
		if dp.Spec.AddressRanges[i].Name == name {
			return &dp.Spec.AddressRanges[i]
		}
	}
	return nil
}

// findAddressRangeInStatus finds an address range in status by name.
func (r *PoolSyncReconciler) findAddressRangeInStatus(
	dp *dynamicprefixiov1alpha1.DynamicPrefix,
	name string,
) *poolConfiguration {
	for _, ar := range dp.Status.AddressRanges {
		if ar.Name == name {
			return &poolConfiguration{
				useAddressRange: true,
				start:           ar.Start,
				end:             ar.End,
				cidr:            ar.CIDR,
			}
		}
	}
	return nil
}

// findSubnetSpec finds a subnet spec by name.
func (r *PoolSyncReconciler) findSubnetSpec(
	dp *dynamicprefixiov1alpha1.DynamicPrefix,
	name string,
) *dynamicprefixiov1alpha1.SubnetSpec {
	for i := range dp.Spec.Subnets {
		if dp.Spec.Subnets[i].Name == name {
			return &dp.Spec.Subnets[i]
		}
	}
	return nil
}

// findSubnetInStatus finds a subnet in status by name.
func (r *PoolSyncReconciler) findSubnetInStatus(
	dp *dynamicprefixiov1alpha1.DynamicPrefix,
	name string,
) *poolConfiguration {
	for _, s := range dp.Status.Subnets {
		if s.Name == name {
			return &poolConfiguration{
				useAddressRange: false,
				cidr:            s.CIDR,
			}
		}
	}
	return nil
}

// calculateAddressRangeConfig calculates a pool configuration from a prefix and address range spec.
func (r *PoolSyncReconciler) calculateAddressRangeConfig(
	prefixStr string,
	rangeSpec *dynamicprefixiov1alpha1.AddressRangeSpec,
) (poolConfiguration, error) {
	basePrefix, err := netip.ParsePrefix(prefixStr)
	if err != nil {
		return poolConfiguration{}, fmt.Errorf("invalid prefix %q: %w", prefixStr, err)
	}

	cfg := prefix.AddressRangeConfig{
		Name:  rangeSpec.Name,
		Start: rangeSpec.Start,
		End:   rangeSpec.End,
	}

	ar, err := prefix.CalculateAddressRange(basePrefix, cfg)
	if err != nil {
		return poolConfiguration{}, err
	}

	return poolConfiguration{
		useAddressRange: true,
		start:           ar.Start.String(),
		end:             ar.End.String(),
		cidr:            prefix.RangeToCIDR(ar.Start, ar.End).String(),
	}, nil
}

// calculateSubnetConfig calculates a pool configuration from a prefix and subnet spec.
func (r *PoolSyncReconciler) calculateSubnetConfig(
	prefixStr string,
	subnetSpec *dynamicprefixiov1alpha1.SubnetSpec,
) (poolConfiguration, error) {
	basePrefix, err := netip.ParsePrefix(prefixStr)
	if err != nil {
		return poolConfiguration{}, fmt.Errorf("invalid prefix %q: %w", prefixStr, err)
	}

	cfg := prefix.SubnetConfig{
		Name:         subnetSpec.Name,
		Offset:       subnetSpec.Offset,
		PrefixLength: subnetSpec.PrefixLength,
	}

	subnet, err := prefix.CalculateSubnet(basePrefix, cfg)
	if err != nil {
		return poolConfiguration{}, err
	}

	return poolConfiguration{
		useAddressRange: false,
		cidr:            subnet.CIDR.String(),
	}, nil
}

// updateLoadBalancerIPPool updates a CiliumLoadBalancerIPPool with the new configurations.
// It supports both CIDR-based blocks (Mode 2) and start/end address ranges (Mode 1).
// Multiple blocks are created for current prefix plus historical prefixes.
// Existing blocks that are not within the operator's managed prefixes (IPv4 blocks,
// static IPv6 blocks from other prefixes) are preserved.
func (r *PoolSyncReconciler) updateLoadBalancerIPPool(ctx context.Context, pool *unstructured.Unstructured, configs []poolConfiguration, managedPrefixes []netip.Prefix) (bool, error) {
	record := parseOwnershipRecord(recordFor(pool, AnnotationManagedBlocks))

	// spec.blocks entries are objects: either a cidr, or a start/stop pair
	// (Cilium spells the upper bound "stop", not "end").
	desired := make([]ownedEntry, 0, len(configs))
	for _, config := range configs {
		block := map[string]interface{}{"cidr": config.cidr}
		if config.useAddressRange && config.start != "" && config.end != "" {
			block = map[string]interface{}{"start": config.start, "stop": config.end}
		}
		desired = append(desired, ownedEntry{value: block, key: blockKey(block)})
	}

	return syncOwnedList(ctx, r, pool, ownedListSync{
		fields:    []string{"spec", "blocks"},
		recordKey: AnnotationManagedBlocks,
		desired:   desired,
		keyOf:     blockKeyOf,
		owned: func(existing interface{}) bool {
			block, ok := existing.(map[string]interface{})
			if !ok {
				// Not a shape this backend writes, so not the operator's.
				return false
			}
			return isOwnedBlock(block, record, managedPrefixes)
		},
	})
}

// updateCIDRGroup updates a CiliumCIDRGroup with the new CIDRs.
// Multiple CIDRs are added for current prefix plus historical prefixes.
// Existing CIDRs that are not within managed prefixes are preserved.
func (r *PoolSyncReconciler) updateCIDRGroup(ctx context.Context, pool *unstructured.Unstructured, configs []poolConfiguration, managedPrefixes []netip.Prefix) (bool, error) {
	record := parseOwnershipRecord(recordFor(pool, AnnotationManagedCIDRs))

	desired := make([]ownedEntry, 0, len(configs))
	for _, config := range configs {
		desired = append(desired, ownedEntry{value: config.cidr, key: canonicalEntry(config.cidr)})
	}

	return syncOwnedList(ctx, r, pool, ownedListSync{
		fields:    []string{"spec", "externalCIDRs"},
		recordKey: AnnotationManagedCIDRs,
		desired:   desired,
		keyOf:     cidrKeyOf,
		owned: func(existing interface{}) bool {
			cidr, ok := existing.(string)
			if !ok {
				return false
			}
			return isOwnedCIDR(cidr, record, managedPrefixes)
		},
	})
}

// setLastSyncAnnotation sets the last-sync annotation to the current timestamp.
func (r *PoolSyncReconciler) setLastSyncAnnotation(pool *unstructured.Unstructured) {
	setPoolAnnotation(pool, AnnotationLastSync, time.Now().UTC().Format(time.RFC3339))
}

// setPoolAnnotation sets a single annotation on an unstructured pool object,
// allocating the map if the object has none.
func setPoolAnnotation(pool *unstructured.Unstructured, key, value string) {
	annotations := pool.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}
	annotations[key] = value
	pool.SetAnnotations(annotations)
}

// isIPv4Block returns true if a pool block contains an IPv4 address (CIDR, start, or stop).
func isIPv4Block(block map[string]interface{}) bool {
	// Check CIDR field
	if cidr, ok := block["cidr"].(string); ok {
		p, err := netip.ParsePrefix(cidr)
		if err == nil && p.Addr().Is4() {
			return true
		}
	}
	// Check start field
	if start, ok := block["start"].(string); ok {
		a, err := netip.ParseAddr(start)
		if err == nil && a.Is4() {
			return true
		}
	}
	// Check stop field
	if stop, ok := block["stop"].(string); ok {
		a, err := netip.ParseAddr(stop)
		if err == nil && a.Is4() {
			return true
		}
	}
	return false
}

// isManagedBlock returns true if a pool block falls within any of the managed prefixes.
// A block is considered managed if its start address (for start/stop blocks) or
// the prefix address (for CIDR blocks) is contained within a managed prefix.
// IPv4 blocks are never considered managed by this operator.
func isManagedBlock(block map[string]interface{}, managedPrefixes []netip.Prefix) bool {
	// Check CIDR field
	if cidr, ok := block["cidr"].(string); ok {
		p, err := netip.ParsePrefix(cidr)
		if err == nil {
			if p.Addr().Is4() {
				return false
			}
			return isPrefixSubsetOfManaged(p, managedPrefixes)
		}
	}
	// Check start field (for start/stop blocks). Both ends must land inside the
	// same managed prefix: a range that merely starts inside one and runs past its
	// end covers addresses the operator does not manage, so claiming it would
	// delete a user's range on the fallback path.
	if start, ok := block["start"].(string); ok {
		a, err := netip.ParseAddr(start)
		if err == nil {
			if a.Is4() {
				return false
			}
			end := a
			if stop, ok := block["stop"].(string); ok && stop != "" {
				parsed, err := netip.ParseAddr(stop)
				if err != nil || parsed.Is4() {
					return false
				}
				end = parsed
			}
			for _, mp := range managedPrefixes {
				if mp.Contains(a) && mp.Contains(end) {
					return true
				}
			}
		}
	}
	return false
}

// collectManagedPrefixes returns all prefixes the operator manages for a
// DynamicPrefix (current + historical).
func collectManagedPrefixes(dp *dynamicprefixiov1alpha1.DynamicPrefix) []netip.Prefix {
	var prefixes []netip.Prefix
	if dp.Status.CurrentPrefix != "" {
		if p, err := netip.ParsePrefix(dp.Status.CurrentPrefix); err == nil {
			prefixes = append(prefixes, p)
		}
	}
	for _, h := range dp.Status.History {
		if p, err := netip.ParsePrefix(h.Prefix); err == nil {
			prefixes = append(prefixes, p)
		}
	}
	return prefixes
}

// SetupWithManager sets up the controller with the Manager.
func (r *PoolSyncReconciler) SetupWithManager(mgr ctrl.Manager) error {
	backends := r.poolBackends()
	if len(backends) == 0 {
		return fmt.Errorf("no pool backends configured")
	}

	// Create predicate for resources with dynamic-prefix.io/name annotation
	hasAnnotation := predicate.NewPredicateFuncs(func(obj client.Object) bool {
		annotations := obj.GetAnnotations()
		if annotations == nil {
			return false
		}
		if _, ok := annotations[AnnotationName]; ok {
			return true
		}
		// Also match pools that still hold operator-written entries, so removing
		// the name annotation delivers one final event to release them instead of
		// silently stranding them.
		return hasOwnershipRecord(annotations)
	})

	// Build controller
	primary := &unstructured.Unstructured{}
	primary.SetGroupVersionKind(backends[0].gvk())
	controllerBuilder := ctrl.NewControllerManagedBy(mgr).
		Named("poolsync").
		For(primary, builder.WithPredicates(hasAnnotation))

	for _, backend := range backends[1:] {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(backend.gvk())
		controllerBuilder = controllerBuilder.
			Watches(obj, &handler.EnqueueRequestForObject{}, builder.WithPredicates(hasAnnotation))
	}

	// Watch DynamicPrefix and enqueue referencing pools
	controllerBuilder = controllerBuilder.
		Watches(&dynamicprefixiov1alpha1.DynamicPrefix{}, handler.EnqueueRequestsFromMapFunc(r.findReferencingPools), builder.WithPredicates(dynamicPrefixDependentChangePredicate()))

	return controllerBuilder.Complete(r)
}

// findReferencingPools finds all pools that reference the given DynamicPrefix.
func (r *PoolSyncReconciler) findReferencingPools(ctx context.Context, obj client.Object) []reconcile.Request {
	dp, ok := obj.(*dynamicprefixiov1alpha1.DynamicPrefix)
	if !ok {
		return nil
	}

	log := logf.FromContext(ctx)
	var requests []reconcile.Request

	for _, backend := range r.poolBackends() {
		poolList := &unstructured.UnstructuredList{}
		poolList.SetGroupVersionKind(ListGVK(backend.gvk()))

		if err := r.List(ctx, poolList); err == nil {
			for _, pool := range poolList.Items {
				if annotations := pool.GetAnnotations(); annotations != nil {
					if annotations[AnnotationName] == dp.Name {
						requests = append(requests, reconcile.Request{
							NamespacedName: types.NamespacedName{
								Name:      pool.GetName(),
								Namespace: pool.GetNamespace(),
							},
						})
					}
				}
			}
		} else {
			log.V(1).Info("Failed to list pool backend resources", "backend", backend.name(), "error", err)
		}
	}

	if len(requests) > 0 {
		log.Info("DynamicPrefix changed, enqueuing referencing pools", "dynamicPrefix", dp.Name, "poolCount", len(requests))
	}

	return requests
}
