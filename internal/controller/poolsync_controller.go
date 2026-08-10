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

	"k8s.io/apimachinery/pkg/api/equality"
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
// and stopping at the first match meant a pool whose name it shared with another
// kind was never reconciled at all -- invisibly, because the other Get succeeded.
func (r *PoolSyncReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	pools, err := r.getPools(ctx, req.NamespacedName)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if len(pools) == 0 {
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
		r.updatePoolsSyncedCondition(ctx, dpName, req.String(), err)
		return ctrl.Result{}, fmt.Errorf("failed to build pool configurations for %s: %w", req.Name, err)
	}

	if len(configs) == 0 {
		// Nothing to write yet -- typically a DynamicPrefix that has not acquired
		// a prefix. That is a wait, not a failure, so it requeues rather than
		// erroring; but it is reported on the pool so an empty pool is
		// explicable.
		log.V(1).Info("No pool configurations generated", "pool", req.Name, "dynamicPrefix", dpName)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Collect all managed prefixes for block preservation logic
	managedPrefixes := collectManagedPrefixes(&dp)

	updated, updateErr := backend.update(ctx, r, pool, configs, managedPrefixes)

	if updateErr != nil {
		log.Error(updateErr, "Failed to update pool")
		// Surfaced so conflicts retry promptly with backoff and rejected
		// updates (immutable fields, webhooks) become visible in the metrics.
		recordPoolSyncFailedMetric(backend.name(), dpName, req.String())
		r.updatePoolsSyncedCondition(ctx, dpName, req.String(), updateErr)
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
	r.updatePoolsSyncedCondition(ctx, dpName, req.String(), nil)
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
	// happens to be present. The two disagree when a pool carries a record from
	// another backend, and the old form then read and wrote spec.blocks on
	// objects that have no such field -- creating an empty list on a CIDR group
	// or a MetalLB pool.
	changed, err := backend.release(ctx, r, pool)
	if err != nil {
		return fmt.Errorf("failed to release %s pool %s: %w", backend.name(), pool.GetName(), err)
	}
	if !changed {
		return nil
	}

	annotations := pool.GetAnnotations()
	delete(annotations, AnnotationLastSync)
	pool.SetAnnotations(annotations)

	if err := r.Update(ctx, pool); err != nil {
		return fmt.Errorf("failed to release pool %s: %w", pool.GetName(), err)
	}
	forgetPoolMetrics(backend.name(), pool.GetAnnotations()[AnnotationName], client.ObjectKeyFromObject(pool).String())
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
// a name. Returning the first match meant whichever kind lost the discovery-order
// race was never reconciled, with no error to show for it. All matches are
// returned instead, and each is synced independently.
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

// buildAddressRangeConfigs builds configurations for address range mode.
func (r *PoolSyncReconciler) buildAddressRangeConfigs(
	ctx context.Context,
	dp *dynamicprefixiov1alpha1.DynamicPrefix,
	addressRangeName string,
	maxHistory int,
) ([]poolConfiguration, error) {
	log := logf.FromContext(ctx)
	var configs []poolConfiguration

	// Find the address range spec
	rangeSpec := r.findAddressRangeSpec(dp, addressRangeName)

	// Get current config from status or calculate from spec
	currentConfig := r.findAddressRangeInStatus(dp, addressRangeName)
	if currentConfig == nil {
		if rangeSpec == nil {
			return nil, fmt.Errorf("address range %q not found in status or spec", addressRangeName)
		}
		calculated, err := r.calculateAddressRangeConfig(dp.Status.CurrentPrefix, rangeSpec)
		if err != nil {
			return nil, fmt.Errorf("failed to calculate address range for current prefix: %w", err)
		}
		currentConfig = &calculated
	}
	configs = append(configs, *currentConfig)

	// Calculate for historical prefixes
	if rangeSpec != nil {
		for i, histEntry := range dp.Status.History {
			if i >= maxHistory {
				break
			}
			histConfig, err := r.calculateAddressRangeConfig(histEntry.Prefix, rangeSpec)
			if err != nil {
				log.V(1).Info("Failed to calculate address range for historical prefix",
					"prefix", histEntry.Prefix, "error", err.Error())
				continue
			}
			configs = append(configs, histConfig)
		}
	}

	return configs, nil
}

// buildSubnetConfigs builds configurations for subnet mode.
func (r *PoolSyncReconciler) buildSubnetConfigs(
	ctx context.Context,
	dp *dynamicprefixiov1alpha1.DynamicPrefix,
	subnetName string,
	maxHistory int,
) ([]poolConfiguration, error) {
	log := logf.FromContext(ctx)
	var configs []poolConfiguration

	// Find the subnet spec
	subnetSpec := r.findSubnetSpec(dp, subnetName)

	// Get current config from status or calculate from spec
	currentConfig := r.findSubnetInStatus(dp, subnetName)
	if currentConfig == nil {
		if subnetSpec == nil {
			return nil, fmt.Errorf("subnet %q not found in status or spec", subnetName)
		}
		calculated, err := r.calculateSubnetConfig(dp.Status.CurrentPrefix, subnetSpec)
		if err != nil {
			return nil, fmt.Errorf("failed to calculate subnet for current prefix: %w", err)
		}
		currentConfig = &calculated
	}
	configs = append(configs, *currentConfig)

	// Calculate for historical prefixes
	if subnetSpec != nil {
		for i, histEntry := range dp.Status.History {
			if i >= maxHistory {
				break
			}
			histConfig, err := r.calculateSubnetConfig(histEntry.Prefix, subnetSpec)
			if err != nil {
				log.V(1).Info("Failed to calculate subnet for historical prefix",
					"prefix", histEntry.Prefix, "error", err.Error())
				continue
			}
			configs = append(configs, histConfig)
		}
	}

	return configs, nil
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
	log := logf.FromContext(ctx)

	// Preserve existing blocks that are NOT within managed prefixes.
	// This includes IPv4 blocks, static IPv6 blocks, and any other blocks
	// that the operator should not touch.
	// A swallowed error here reads back as "no blocks", and the write below
	// replaces spec.blocks wholesale -- destroying exactly the unmanaged
	// entries this preservation pass exists to protect.
	existingBlocks, _, err := unstructured.NestedSlice(pool.Object, "spec", "blocks")
	if err != nil {
		return false, fmt.Errorf("failed to read spec.blocks: %w", err)
	}

	recordValue, recordExists := pool.GetAnnotations()[AnnotationManagedBlocks]
	record := parseOwnershipRecord(recordValue, recordExists)

	// Build the blocks this pass claims first, so their keys can suppress any
	// duplicate sitting in the existing list.
	// CiliumLoadBalancerIPPool spec.blocks is a list of IP blocks
	// Format can be either:
	// - spec.blocks[].cidr for CIDR-based allocation
	// - spec.blocks[].start + spec.blocks[].stop for address range (Cilium uses "stop" not "end")
	configBlocks := make([]interface{}, 0, len(configs))
	configKeys := make(map[string]struct{}, len(configs))
	for _, config := range configs {
		var block map[string]interface{}
		if config.useAddressRange && config.start != "" && config.end != "" {
			// Use start/stop for precise address range (Mode 1)
			block = map[string]interface{}{
				"start": config.start,
				"stop":  config.end,
			}
		} else {
			// Use CIDR (Mode 2 or fallback)
			block = map[string]interface{}{
				"cidr": config.cidr,
			}
		}
		key := blockKey(block)
		if key == "" {
			// Refuse to write what cannot be recorded. An unkeyable block could
			// never be recognised as the operator's on a later pass, so it would
			// be preserved as a user's entry and a fresh copy appended every
			// reconcile -- reintroducing precisely the unbounded growth the
			// ownership record exists to stop.
			log.Error(nil, "Skipping pool block with no usable identity", "pool", pool.GetName(), "block", block)
			continue
		}
		if _, dup := configKeys[key]; dup {
			continue
		}
		configKeys[key] = struct{}{}
		configBlocks = append(configBlocks, block)
	}

	var preservedBlocks []interface{}
	preservedKeys := make(map[string]struct{}, len(existingBlocks))
	for _, b := range existingBlocks {
		block, ok := b.(map[string]interface{})
		if !ok {
			continue
		}
		if isOwnedBlock(block, record, managedPrefixes) {
			continue
		}
		preservedBlocks = append(preservedBlocks, block)
		if key := blockKey(block); key != "" {
			preservedKeys[key] = struct{}{}
		}
	}
	if len(preservedBlocks) > 0 {
		log.V(1).Info("Preserving unmanaged blocks in pool", "count", len(preservedBlocks))
	}

	// Anything already present and unowned belongs to the user, even where it
	// coincides with a block this pass would write. Keep their copy, skip ours,
	// and leave it out of the record -- claiming it would hand the operator the
	// right to delete it once the prefix rotates out of range, turning a pin into
	// a time bomb.
	blocks := make([]interface{}, 0, len(preservedBlocks)+len(configBlocks))
	blocks = append(blocks, preservedBlocks...)
	managedKeys := make([]string, 0, len(configBlocks))
	for _, b := range configBlocks {
		key := blockKey(b.(map[string]interface{}))
		if _, pinned := preservedKeys[key]; pinned {
			continue
		}
		blocks = append(blocks, b)
		managedKeys = append(managedKeys, key)
	}

	// Check if blocks actually changed before updating to avoid feedback loops
	currentBlocks, _, err := unstructured.NestedSlice(pool.Object, "spec", "blocks")
	if err != nil {
		return false, fmt.Errorf("failed to read current spec.blocks: %w", err)
	}
	managedBlocksStr := formatOwnershipRecord(managedKeys)
	blocksChanged := !equality.Semantic.DeepEqual(currentBlocks, blocks)
	recordChanged := recordValue != managedBlocksStr || !recordExists

	// The record must be written even when the block list is byte-identical:
	// the first pass after an upgrade usually produces the same blocks, and
	// without persisting the record the next rotation would fall back to the
	// leaky geometric test all over again.
	if !blocksChanged && !recordChanged {
		log.V(2).Info("Pool blocks unchanged, skipping update", "pool", pool.GetName())
		return false, nil
	}

	if blocksChanged {
		if err := unstructured.SetNestedField(pool.Object, blocks, "spec", "blocks"); err != nil {
			return false, fmt.Errorf("failed to set spec.blocks: %w", err)
		}
	}

	setPoolAnnotation(pool, AnnotationManagedBlocks, managedBlocksStr)

	// Update last-sync annotation
	r.setLastSyncAnnotation(pool)

	return true, r.Update(ctx, pool)
}

// updateCIDRGroup updates a CiliumCIDRGroup with the new CIDRs.
// Multiple CIDRs are added for current prefix plus historical prefixes.
// Existing CIDRs that are not within managed prefixes are preserved.
func (r *PoolSyncReconciler) updateCIDRGroup(ctx context.Context, pool *unstructured.Unstructured, configs []poolConfiguration, managedPrefixes []netip.Prefix) (bool, error) {
	log := logf.FromContext(ctx)

	// Preserve existing CIDRs that are not within managed prefixes
	// As with spec.blocks: an ignored error here silently drops the
	// unmanaged CIDRs that the write below would otherwise preserve.
	existingCIDRs, _, err := unstructured.NestedSlice(pool.Object, "spec", "externalCIDRs")
	if err != nil {
		return false, fmt.Errorf("failed to read spec.externalCIDRs: %w", err)
	}
	recordValue, recordExists := pool.GetAnnotations()[AnnotationManagedCIDRs]
	record := parseOwnershipRecord(recordValue, recordExists)

	configCIDRs := make([]string, 0, len(configs))
	seenConfig := make(map[string]struct{}, len(configs))
	for _, config := range configs {
		key := canonicalEntry(config.cidr)
		if key == "" {
			continue
		}
		if _, dup := seenConfig[key]; dup {
			continue
		}
		seenConfig[key] = struct{}{}
		configCIDRs = append(configCIDRs, config.cidr)
	}

	var preserved []interface{}
	preservedKeys := make(map[string]struct{}, len(existingCIDRs))
	for _, c := range existingCIDRs {
		cidrStr, ok := c.(string)
		if !ok {
			continue
		}
		if isOwnedCIDR(cidrStr, record, managedPrefixes) {
			continue
		}
		preserved = append(preserved, c)
		preservedKeys[canonicalEntry(cidrStr)] = struct{}{}
	}

	// A CIDR the user already pinned stays theirs even where it coincides with one
	// this pass would write; see updateLoadBalancerIPPool for why claiming it would
	// make the pin deletable later.
	externalCIDRs := make([]interface{}, 0, len(preserved)+len(configCIDRs))
	externalCIDRs = append(externalCIDRs, preserved...)
	managedCIDRs := make([]string, 0, len(configCIDRs))
	for _, cidr := range configCIDRs {
		if _, pinned := preservedKeys[canonicalEntry(cidr)]; pinned {
			continue
		}
		externalCIDRs = append(externalCIDRs, cidr)
		managedCIDRs = append(managedCIDRs, cidr)
	}

	// Check if CIDRs actually changed before updating to avoid feedback loops
	currentCIDRs, _, err := unstructured.NestedSlice(pool.Object, "spec", "externalCIDRs")
	if err != nil {
		return false, fmt.Errorf("failed to read current spec.externalCIDRs: %w", err)
	}
	managedCIDRsStr := formatOwnershipRecord(managedCIDRs)
	cidrsChanged := !equality.Semantic.DeepEqual(currentCIDRs, externalCIDRs)
	recordChanged := recordValue != managedCIDRsStr || !recordExists

	// As in updateLoadBalancerIPPool: persist the record even when the CIDR list
	// is unchanged, otherwise the next rotation falls back to the geometric test.
	if !cidrsChanged && !recordChanged {
		log.V(2).Info("CIDRGroup unchanged, skipping update", "cidrGroup", pool.GetName())
		return false, nil
	}
	setPoolAnnotation(pool, AnnotationManagedCIDRs, managedCIDRsStr)

	if err := unstructured.SetNestedField(pool.Object, externalCIDRs, "spec", "externalCIDRs"); err != nil {
		return false, fmt.Errorf("failed to set spec.externalCIDRs: %w", err)
	}

	// Update last-sync annotation
	r.setLastSyncAnnotation(pool)

	return true, r.Update(ctx, pool)
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
