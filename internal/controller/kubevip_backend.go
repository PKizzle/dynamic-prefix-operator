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
	"net/netip"
	"regexp"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// The kube-vip cloud provider keeps its pools in one ConfigMap rather than a
// CRD, which makes this backend differ from the others in two ways that are
// deliberate and documented here so the "adding a backend" recipe stays honest:
//
//   - It is not discovered. Every cluster has ConfigMaps, so there is nothing
//     to detect; watching them on the chance that kube-vip is installed would
//     put a cluster-wide ConfigMap informer in every deployment. It is enabled
//     by naming the ConfigMap on the command line instead, which also scopes
//     the informer to that one object.
//   - Its RBAC is a namespaced Role, granted only when it is enabled, rather
//     than a rule in the generated ClusterRole. Cluster-wide write access to
//     ConfigMaps is a far larger grant than the CRD backends need, and nothing
//     that is not using kube-vip should be carrying it.

// kubevipKeyPattern matches the data keys that hold pools: `cidr-global`,
// `range-global`, and the per-namespace `cidr-<ns>` / `range-<ns>` forms.
var kubevipKeyPattern = regexp.MustCompile(`^(cidr|range)-[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// KubevipConfigMapGVK is the object this backend manages.
var KubevipConfigMapGVK = schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}

type kubevipConfigMapBackend struct {
	resourceGVK schema.GroupVersionKind
}

func (b kubevipConfigMapBackend) name() string { return "kubevip-configmap" }

func (b kubevipConfigMapBackend) gvk() schema.GroupVersionKind { return b.resourceGVK }

// The kube-vip ConfigMap lives in a namespace, conventionally kube-system.
func (b kubevipConfigMapBackend) namespaced() bool { return true }

// kubevipKeyFor reads the annotation naming which pool key to manage.
//
// It is required rather than inferred. One ConfigMap multiplexes every pool in
// the cluster through key names, and the cidr/range split is a real choice --
// kube-vip allocates differently from each -- so a guess would be wrong for
// somebody, silently, in a file they share with every other pool.
func kubevipKeyFor(pool *unstructured.Unstructured) (string, error) {
	key := strings.TrimSpace(pool.GetAnnotations()[AnnotationKubevipKey])
	if key == "" {
		return "", fmt.Errorf("ConfigMap %s/%s is bound to a DynamicPrefix but has no %s annotation naming the pool key to manage (for example %q)",
			pool.GetNamespace(), pool.GetName(), AnnotationKubevipKey, "cidr-global")
	}
	if !kubevipKeyPattern.MatchString(key) {
		return "", fmt.Errorf("%s=%q is not a kube-vip pool key; expected cidr-global, range-global, or cidr-/range- followed by a namespace",
			AnnotationKubevipKey, key)
	}
	return key, nil
}

// kubevipEntriesFor renders the configurations in the form the selected key
// takes: a `range-` key holds first-last pairs, a `cidr-` key holds CIDRs.
func kubevipEntriesFor(key string, configs []poolConfiguration) ([]string, error) {
	wantRange := strings.HasPrefix(key, "range-")

	entries := make([]string, 0, len(configs))
	for _, config := range configs {
		entry, err := kubevipEntryFor(key, wantRange, config)
		if err != nil {
			return nil, err
		}
		if entry != "" {
			entries = append(entries, entry)
		}
	}
	return entries, nil
}

func kubevipEntryFor(key string, wantRange bool, config poolConfiguration) (string, error) {
	if wantRange {
		if config.useAddressRange && config.start != "" && config.end != "" {
			return config.start + "-" + config.end, nil
		}
		// A CIDR converts to a range exactly, so a subnet-mode or raw-prefix
		// configuration can still feed a range key.
		if config.cidr == "" {
			return "", nil
		}
		parsed, err := netip.ParsePrefix(config.cidr)
		if err != nil {
			return "", fmt.Errorf("cannot render %q as a kube-vip range for key %s: %w", config.cidr, key, err)
		}
		last, err := lastAddrOfPrefix(parsed)
		if err != nil {
			return "", fmt.Errorf("cannot render %q as a kube-vip range for key %s: %w", config.cidr, key, err)
		}
		return parsed.Masked().Addr().String() + "-" + last.String(), nil
	}

	// A cidr- key takes CIDRs, and an address range only becomes one exactly
	// when it happens to fall on a CIDR boundary. Widening it silently would
	// hand kube-vip addresses the user deliberately left out of the range.
	if config.useAddressRange {
		cidr, exact, err := exactCIDRForAddressRange(config.start, config.end)
		if err != nil {
			return "", fmt.Errorf("cannot render the address range %s-%s as a CIDR for key %s: %w",
				config.start, config.end, key, err)
		}
		if !exact {
			return "", fmt.Errorf("address range %s-%s is not CIDR-aligned, so it cannot be written to the %s key; "+
				"use a range- key instead, or align the range to a CIDR boundary", config.start, config.end, key)
		}
		return cidr, nil
	}
	return config.cidr, nil
}

// splitKubevipEntries parses one key's value. kube-vip accepts a comma-separated
// list, and tolerates whitespace around the entries.
func splitKubevipEntries(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}

	parts := strings.Split(value, ",")
	entries := make([]string, 0, len(parts))
	for _, part := range parts {
		if entry := strings.TrimSpace(part); entry != "" {
			entries = append(entries, entry)
		}
	}
	return entries
}

func (b kubevipConfigMapBackend) update(ctx context.Context, r *PoolSyncReconciler, pool *unstructured.Unstructured, configs []poolConfiguration, managedPrefixes []netip.Prefix) (bool, error) {
	logger := log.FromContext(ctx)

	key, err := kubevipKeyFor(pool)
	if err != nil {
		return false, err
	}

	desired, err := kubevipEntriesFor(key, configs)
	if err != nil {
		return false, err
	}

	data, _, err := unstructured.NestedStringMap(pool.Object, "data")
	if err != nil {
		return false, fmt.Errorf("failed to read the ConfigMap data: %w", err)
	}
	if data == nil {
		data = map[string]string{}
	}

	record := parseOwnershipRecord(recordFor(pool, AnnotationManagedKubevipEntries))

	// The value shares a key with whatever the user put there, so ownership is
	// decided the same way as for every other shared field: by the record, with
	// address geometry only as a fallback for entries written before records
	// existed. IPv4 entries never match, so a dual-stack pool keeps its v4 half.
	owned := func(entry string) bool {
		if !record.present {
			return isManagedAddressEntry(entry, managedPrefixes)
		}
		return record.owns(canonicalAddressEntry(entry))
	}

	preserved := make([]string, 0)
	pinned := make([]string, 0)
	for _, entry := range splitKubevipEntries(data[key]) {
		if owned(entry) {
			continue
		}
		preserved = append(preserved, entry)
		pinned = append(pinned, canonicalAddressEntry(entry))
	}
	if len(preserved) > 0 {
		logger.V(1).Info("Preserving unmanaged kube-vip entries",
			"configMap", pool.GetName(), "key", key, "count", len(preserved))
	}

	// An entry the user has pinned themselves is left to them: writing it again
	// would claim it, and the next rotation would take it away.
	managed := excludePinned(dedupePreservingOrder(desired), pinned)

	changed := false

	// The key can be re-pointed by editing the annotation. Entries the operator
	// wrote under the old key would otherwise stay there for good, since nothing
	// afterwards looks at a key the annotation no longer names.
	for other := range data {
		if other == key || !kubevipKeyPattern.MatchString(other) {
			continue
		}
		if kept, dropped := stripOwnedEntries(data[other], record); dropped {
			logger.V(1).Info("Removing entries left under a kube-vip key this pool no longer manages",
				"configMap", pool.GetName(), "key", other)
			setOrDeleteKubevipKey(data, other, kept)
			changed = true
		}
	}

	value := strings.Join(append(preserved, managed...), ",")
	if data[key] != value {
		setOrDeleteKubevipKey(data, key, value)
		changed = true
	}

	recordValue := formatOwnershipRecord(canonicalEntries(managed))
	existingRecord, recordExists := recordFor(pool, AnnotationManagedKubevipEntries)
	if !changed && recordExists && existingRecord == recordValue {
		return false, nil
	}

	if err := unstructured.SetNestedStringMap(pool.Object, data, "data"); err != nil {
		return false, fmt.Errorf("failed to set the ConfigMap data: %w", err)
	}
	setPoolAnnotation(pool, AnnotationManagedKubevipEntries, recordValue)
	r.setLastSyncAnnotation(pool)

	return true, r.Update(ctx, pool)
}

func (b kubevipConfigMapBackend) release(_ context.Context, _ *PoolSyncReconciler, pool *unstructured.Unstructured) (bool, error) {
	recordValue, ok := recordFor(pool, AnnotationManagedKubevipEntries)
	if !ok {
		return false, nil
	}
	record := parseOwnershipRecord(recordValue, true)

	data, _, err := unstructured.NestedStringMap(pool.Object, "data")
	if err != nil {
		return false, fmt.Errorf("failed to read the ConfigMap data: %w", err)
	}

	// Sweep every pool key rather than the annotated one: the annotation may
	// have been removed along with the binding, and an entry the operator wrote
	// is recognisable wherever it ended up.
	for key := range data {
		if !kubevipKeyPattern.MatchString(key) {
			continue
		}
		if kept, dropped := stripOwnedEntries(data[key], record); dropped {
			setOrDeleteKubevipKey(data, key, kept)
		}
	}

	if err := unstructured.SetNestedStringMap(pool.Object, data, "data"); err != nil {
		return false, fmt.Errorf("failed to set the ConfigMap data: %w", err)
	}

	annotations := pool.GetAnnotations()
	delete(annotations, AnnotationManagedKubevipEntries)
	pool.SetAnnotations(annotations)
	return true, nil
}

// stripOwnedEntries removes the recorded entries from one key's value,
// reporting whether anything went.
func stripOwnedEntries(value string, record ownershipRecord) (kept string, dropped bool) {
	entries := splitKubevipEntries(value)
	remaining := make([]string, 0, len(entries))
	for _, entry := range entries {
		if record.owns(canonicalAddressEntry(entry)) {
			dropped = true
			continue
		}
		remaining = append(remaining, entry)
	}
	return strings.Join(remaining, ","), dropped
}

// setOrDeleteKubevipKey writes a value, or removes the key when nothing is left.
// kube-vip reads a present-but-empty key as a pool it cannot allocate from
// rather than as no pool at all.
func setOrDeleteKubevipKey(data map[string]string, key, value string) {
	if value == "" {
		delete(data, key)
		return
	}
	data[key] = value
}

// canonicalEntries keys entries for the ownership record.
func canonicalEntries(entries []string) []string {
	keys := make([]string, 0, len(entries))
	for _, entry := range entries {
		keys = append(keys, canonicalAddressEntry(entry))
	}
	return slices.Clip(keys)
}
