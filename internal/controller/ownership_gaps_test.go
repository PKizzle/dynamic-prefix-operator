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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	dynamicprefixiov1alpha1 "github.com/pkizzle/dynamic-prefix-operator/api/v1alpha1"
)

// newMetalLBPool builds a namespaced MetalLB IPAddressPool seeded with addresses.
func newMetalLBPool(name, namespace string, addresses []interface{}) *unstructured.Unstructured {
	pool := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": APIVersion(DefaultMetalLBIPAddressPoolGVK),
			"kind":       DefaultMetalLBIPAddressPoolGVK.Kind,
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": namespace,
			},
			"spec": map[string]interface{}{"addresses": addresses},
		},
	}
	pool.SetGroupVersionKind(DefaultMetalLBIPAddressPoolGVK)
	return pool
}

// TestMetalLBAddressesStayBoundedAcrossRotations is the MetalLB counterpart of the
// Cilium rotation tests. This backend kept using the geometric ownership test long
// after the others moved to records, so it leaked one address per rotation with
// nothing asserting otherwise: its only test was a single-snapshot update.
func TestMetalLBAddressesStayBoundedAcrossRotations(t *testing.T) {
	ctx := context.Background()
	const maxHistory = 2
	scheme := newPoolBackendTestScheme(t)

	const staticAddress = "198.51.100.0/24"
	pool := newMetalLBPool("lb-pool", "metallb-system", []interface{}{staticAddress})

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pool).Build()
	reconciler := &PoolSyncReconciler{Client: fakeClient, Scheme: scheme}
	backend := metalLBIPAddressPoolBackend{resourceGVK: DefaultMetalLBIPAddressPoolGVK}
	key := types.NamespacedName{Name: "lb-pool", Namespace: "metallb-system"}

	const rotations = 25
	for gen := 0; gen < rotations; gen++ {
		fetched := &unstructured.Unstructured{}
		fetched.SetGroupVersionKind(DefaultMetalLBIPAddressPoolGVK)
		if err := fakeClient.Get(ctx, key, fetched); err != nil {
			t.Fatalf("gen %d: get: %v", gen, err)
		}
		configs, managed := poolStateForGeneration(gen, maxHistory)
		if _, err := backend.update(ctx, reconciler, fetched, configs, managed); err != nil {
			t.Fatalf("gen %d: update: %v", gen, err)
		}
	}

	fetched := &unstructured.Unstructured{}
	fetched.SetGroupVersionKind(DefaultMetalLBIPAddressPoolGVK)
	if err := fakeClient.Get(ctx, key, fetched); err != nil {
		t.Fatalf("final get: %v", err)
	}
	addresses, _, err := unstructured.NestedStringSlice(fetched.Object, "spec", "addresses")
	if err != nil {
		t.Fatalf("read addresses: %v", err)
	}

	want := 1 + maxHistory + 1 // static IPv4 + history + current
	if len(addresses) != want {
		t.Errorf("after %d rotations: %d addresses, want %d -- MetalLB is accumulating "+
			"one address per rotation again\n  %v", rotations, len(addresses), want, addresses)
	}
	if !containsString(addresses, staticAddress) {
		t.Errorf("the user's static address was deleted: %v", addresses)
	}
}

// TestFallbackPreservesUserSupernet covers the reverse-containment defect. A user
// pinning the whole delegation while the operator manages a subnet of it is
// ordinary; treating that supernet as the operator's deleted it on the first pass
// after an upgrade, when no ownership record exists yet.
func TestFallbackPreservesUserSupernet(t *testing.T) {
	ctx := context.Background()
	scheme := newPoolBackendTestScheme(t)
	scheme.AddKnownTypeWithName(DefaultCiliumLBIPPoolGVK, &unstructured.Unstructured{})

	// The user pins the entire delegation; the operator manages a /64 inside it.
	supernet := map[string]interface{}{"cidr": "2001:db8:abcd::/48"}
	pool := newCiliumPool("default-ipv6", []interface{}{supernet})

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pool).Build()
	reconciler := &PoolSyncReconciler{Client: fakeClient, Scheme: scheme}
	backend := ciliumLoadBalancerIPPoolBackend{resourceGVK: DefaultCiliumLBIPPoolGVK}

	fetched := &unstructured.Unstructured{}
	fetched.SetGroupVersionKind(DefaultCiliumLBIPPoolGVK)
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: "default-ipv6"}, fetched); err != nil {
		t.Fatalf("get: %v", err)
	}
	// No record on the object: the legacy geometric path, i.e. the first pass
	// after an upgrade.
	configs, managed := poolStateForGeneration(0, 2)
	if _, err := backend.update(ctx, reconciler, fetched, configs, managed); err != nil {
		t.Fatalf("update: %v", err)
	}

	if err := fakeClient.Get(ctx, types.NamespacedName{Name: "default-ipv6"}, fetched); err != nil {
		t.Fatalf("get after update: %v", err)
	}
	blocks, _, err := unstructured.NestedSlice(fetched.Object, "spec", "blocks")
	if err != nil {
		t.Fatalf("read blocks: %v", err)
	}

	var sawSupernet bool
	for _, b := range blocks {
		if block, ok := b.(map[string]interface{}); ok && blockKey(block) == blockKey(supernet) {
			sawSupernet = true
		}
	}
	if !sawSupernet {
		t.Errorf("the user's supernet block was deleted on the no-record path: %v", blocks)
	}
}

// TestUnkeyableBlockIsNeverWritten covers a config that yields no usable identity.
// Writing it would be unrecoverable: it could never be recognised as the
// operator's afterwards, so it would be preserved as a user entry and a fresh copy
// appended on every reconcile.
func TestUnkeyableBlockIsNeverWritten(t *testing.T) {
	ctx := context.Background()
	scheme := newPoolBackendTestScheme(t)
	scheme.AddKnownTypeWithName(DefaultCiliumLBIPPoolGVK, &unstructured.Unstructured{})

	pool := newCiliumPool("default-ipv6", []interface{}{})
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pool).Build()
	reconciler := &PoolSyncReconciler{Client: fakeClient, Scheme: scheme}
	backend := ciliumLoadBalancerIPPoolBackend{resourceGVK: DefaultCiliumLBIPPoolGVK}

	// An empty CIDR: reachable when status carries an address range with no
	// usable start/end and no CIDR.
	configs := []poolConfiguration{{cidr: ""}, {cidr: "2001:db8:abcd::/64"}}
	managed := []netip.Prefix{netip.MustParsePrefix("2001:db8:abcd::/64")}

	for pass := 0; pass < 3; pass++ {
		fetched := &unstructured.Unstructured{}
		fetched.SetGroupVersionKind(DefaultCiliumLBIPPoolGVK)
		if err := fakeClient.Get(ctx, types.NamespacedName{Name: "default-ipv6"}, fetched); err != nil {
			t.Fatalf("pass %d: get: %v", pass, err)
		}
		if _, err := backend.update(ctx, reconciler, fetched, configs, managed); err != nil {
			t.Fatalf("pass %d: update: %v", pass, err)
		}
	}

	fetched := &unstructured.Unstructured{}
	fetched.SetGroupVersionKind(DefaultCiliumLBIPPoolGVK)
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: "default-ipv6"}, fetched); err != nil {
		t.Fatalf("final get: %v", err)
	}
	blocks, _, err := unstructured.NestedSlice(fetched.Object, "spec", "blocks")
	if err != nil {
		t.Fatalf("read blocks: %v", err)
	}
	if len(blocks) != 1 {
		t.Fatalf("after 3 passes: %d blocks, want 1 (the unkeyable block must be "+
			"refused, not written and re-appended)\n  %v", len(blocks), blocks)
	}
}

// TestSameNameAcrossKindsSyncsBoth covers the collision where two cluster-scoped
// backends share a name. Stopping at the first match left the other kind
// unreconciled forever, with no error to show for it.
func TestSameNameAcrossKindsSyncsBoth(t *testing.T) {
	ctx := context.Background()
	scheme := newPoolBackendTestScheme(t)
	scheme.AddKnownTypeWithName(DefaultCiliumLBIPPoolGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(DefaultCiliumCIDRGroupGVK, &unstructured.Unstructured{})

	lbPool := newCiliumPool("shared-name", nil)
	cidrGroup := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": APIVersion(DefaultCiliumCIDRGroupGVK),
			"kind":       DefaultCiliumCIDRGroupGVK.Kind,
			"metadata":   map[string]interface{}{"name": "shared-name"},
			"spec":       map[string]interface{}{"externalCIDRs": []interface{}{}},
		},
	}
	cidrGroup.SetGroupVersionKind(DefaultCiliumCIDRGroupGVK)

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lbPool, cidrGroup).Build()
	reconciler := &PoolSyncReconciler{
		Client: fakeClient,
		Scheme: scheme,
		BackendGVKs: []schema.GroupVersionKind{
			DefaultCiliumLBIPPoolGVK,
			DefaultCiliumCIDRGroupGVK,
		},
	}

	matches, err := reconciler.getPools(ctx, types.NamespacedName{Name: "shared-name"})
	if err != nil {
		t.Fatalf("getPools() unexpected error: %v", err)
	}
	if len(matches) != 2 {
		t.Fatalf("getPools() returned %d matches, want 2: a name shared across kinds "+
			"must reconcile every kind, not just the first found", len(matches))
	}
	seen := map[string]bool{}
	for _, m := range matches {
		seen[m.backend.name()] = true
	}
	for _, want := range []string{"cilium-load-balancer-ip-pool", "cilium-cidr-group"} {
		if !seen[want] {
			t.Errorf("getPools() did not return backend %q", want)
		}
	}
}

// TestCalicoKeepsDrainingSiblings covers the drain window. spec.cidr holds a
// single prefix, so without sibling pools the previous prefix is cut over the
// instant the delegation rotates and anything still using it loses connectivity.
func TestCalicoKeepsDrainingSiblings(t *testing.T) {
	ctx := context.Background()
	const maxHistory = 2
	scheme := newPoolBackendTestScheme(t)

	pool := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": APIVersion(DefaultCalicoIPPoolGVK),
			"kind":       DefaultCalicoIPPoolGVK.Kind,
			"metadata": map[string]interface{}{
				"name":        "calico-pool",
				"annotations": map[string]interface{}{AnnotationName: "home-ipv6"},
			},
			"spec": map[string]interface{}{"cidr": "2001:db8:abcd::/64"},
		},
	}
	pool.SetGroupVersionKind(DefaultCalicoIPPoolGVK)

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pool).Build()
	reconciler := &PoolSyncReconciler{Client: fakeClient, Scheme: scheme}
	backend := calicoIPPoolBackend{resourceGVK: DefaultCalicoIPPoolGVK}

	const rotations = 12
	for gen := 0; gen < rotations; gen++ {
		fetched := &unstructured.Unstructured{}
		fetched.SetGroupVersionKind(DefaultCalicoIPPoolGVK)
		if err := fakeClient.Get(ctx, types.NamespacedName{Name: "calico-pool"}, fetched); err != nil {
			t.Fatalf("gen %d: get: %v", gen, err)
		}
		configs, managed := calicoPoolStateForGeneration(gen, maxHistory)
		if _, err := backend.update(ctx, reconciler, fetched, configs, managed); err != nil {
			t.Fatalf("gen %d: update: %v", gen, err)
		}
	}

	// The primary must carry the current prefix.
	fetched := &unstructured.Unstructured{}
	fetched.SetGroupVersionKind(DefaultCalicoIPPoolGVK)
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: "calico-pool"}, fetched); err != nil {
		t.Fatalf("final get: %v", err)
	}
	gotCIDR, _, err := unstructured.NestedString(fetched.Object, "spec", "cidr")
	if err != nil {
		t.Fatalf("read cidr: %v", err)
	}
	wantCIDR := fmt.Sprintf("2001:db8:abcd:%x00::/64", rotations-1)
	if netip.MustParsePrefix(gotCIDR) != netip.MustParsePrefix(wantCIDR) {
		t.Errorf("primary spec.cidr = %s, want %s", gotCIDR, wantCIDR)
	}

	// Siblings exist for the draining prefixes, and are bounded by maxHistory.
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(ListGVK(DefaultCalicoIPPoolGVK))
	if err := fakeClient.List(ctx, list); err != nil {
		t.Fatalf("list: %v", err)
	}
	siblings := 0
	for i := range list.Items {
		if list.Items[i].GetLabels()[LabelCalicoParentPool] == "calico-pool" {
			siblings++
		}
	}
	if siblings != maxHistory {
		t.Errorf("after %d rotations: %d draining siblings, want %d (drained pools must "+
			"be deleted, and a draining prefix must keep a pool)", rotations, siblings, maxHistory)
	}
}

// calicoPoolStateForGeneration mirrors the production builder's ordering, which
// puts the current prefix first and history after it. Order is irrelevant to the
// tests that only count entries, but Calico reads configs[0] as the prefix its
// primary pool must carry.
func calicoPoolStateForGeneration(gen, maxHistory int) ([]poolConfiguration, []netip.Prefix) {
	current := fmt.Sprintf("2001:db8:abcd:%x00::/64", gen)
	configs := []poolConfiguration{{cidr: current}}
	managed := []netip.Prefix{netip.MustParsePrefix(current)}
	for i := gen - 1; i >= gen-maxHistory; i-- {
		if i < 0 {
			continue
		}
		cidr := fmt.Sprintf("2001:db8:abcd:%x00::/64", i)
		configs = append(configs, poolConfiguration{cidr: cidr})
		managed = append(managed, netip.MustParsePrefix(cidr))
	}
	return configs, managed
}

// TestServiceReleasedOnDeannotation covers the cleanup path. Removing the name
// annotation must not strand the operator's entries: the watch predicate stops
// matching the object, so without cleanup on this path no event is delivered
// and an external-dns target that stops resolving at the next rotation is
// simply left behind.
func TestServiceReleasedOnDeannotation(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = dynamicprefixiov1alpha1.AddToScheme(scheme)

	const userIPv4 = "192.0.2.238"
	const userULA = "fd00:db8:abcd::ffff:0:2"
	const operatorAddr = "2001:db8:abcd::ffff:0:2"

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "svc",
			Namespace: "network",
			Annotations: map[string]string{
				// No dynamic-prefix.io/name: the user has just removed it.
				AnnotationCiliumIPs:         strings.Join([]string{userIPv4, userULA, operatorAddr}, ","),
				AnnotationManagedIPs:        operatorAddr,
				AnnotationExternalDNSTarget: operatorAddr,
				AnnotationManagedTargets:    operatorAddr,
			},
		},
		Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(svc).Build()
	r := &ServiceSyncReconciler{Client: fakeClient, Scheme: scheme}
	key := types.NamespacedName{Name: "svc", Namespace: "network"}

	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var got corev1.Service
	if err := fakeClient.Get(ctx, key, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	annotations := got.GetAnnotations()

	ips := strings.Split(annotations[AnnotationCiliumIPs], ",")
	if containsString(ips, operatorAddr) {
		t.Errorf("operator address was not released: %v", ips)
	}
	for _, must := range []string{userIPv4, userULA} {
		if !containsString(ips, must) {
			t.Errorf("user entry %q was removed; only recorded entries may be released: %v", must, ips)
		}
	}
	if _, ok := annotations[AnnotationManagedIPs]; ok {
		t.Error("managed-ips record should be gone after release")
	}
	if _, ok := annotations[AnnotationManagedTargets]; ok {
		t.Error("managed-targets record should be gone after release")
	}
	if v, ok := annotations[AnnotationExternalDNSTarget]; ok {
		t.Errorf("external-dns target should be gone after release, got %q", v)
	}
}

// newManagedService builds a Service carrying operator-written entries and their
// records, as it would look after a normal HA-mode sync.
func newManagedService(dpName, operatorAddr, userIPv4 string) *corev1.Service {
	annotations := map[string]string{
		AnnotationCiliumIPs:         strings.Join([]string{userIPv4, operatorAddr}, ","),
		AnnotationManagedIPs:        operatorAddr,
		AnnotationExternalDNSTarget: operatorAddr,
		AnnotationManagedTargets:    operatorAddr,
		AnnotationL2Nudge:           "deadbeef",
	}
	if dpName != "" {
		annotations[AnnotationName] = dpName
	}
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "network", Annotations: annotations},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer},
	}
}

// assertServiceReleased checks that the operator's entries and records are gone
// while the user's own entry survives.
func assertServiceReleased(t *testing.T, annotations map[string]string, operatorAddr, userIPv4 string) {
	t.Helper()

	ips := strings.Split(annotations[AnnotationCiliumIPs], ",")
	if containsString(ips, operatorAddr) {
		t.Errorf("operator address was not released: %v", ips)
	}
	if !containsString(ips, userIPv4) {
		t.Errorf("user entry %q was removed; only recorded entries may be released: %v", userIPv4, ips)
	}
	for _, key := range []string{
		AnnotationManagedIPs, AnnotationManagedTargets, AnnotationExternalDNSTarget, AnnotationL2Nudge,
	} {
		if v, ok := annotations[key]; ok {
			t.Errorf("%s should be gone after release, got %q", key, v)
		}
	}
}

// TestServiceReleasedWhenDynamicPrefixDeleted covers deleting the DynamicPrefix
// itself. The Service kept an lbipam annotation and an external-dns target that
// stop being maintained the moment the CR goes, and the controller merely polled
// for the missing CR every 30 seconds, forever, without touching them.
func TestServiceReleasedWhenDynamicPrefixDeleted(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = dynamicprefixiov1alpha1.AddToScheme(scheme)

	const userIPv4 = "192.0.2.238"
	const operatorAddr = "2001:db8:abcd::ffff:0:2"

	// The Service still references a DynamicPrefix, but it no longer exists.
	svc := newManagedService("home-ipv6", operatorAddr, userIPv4)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(svc).Build()
	r := &ServiceSyncReconciler{Client: fakeClient, Scheme: scheme}
	key := types.NamespacedName{Name: "svc", Namespace: "network"}

	result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Errorf("RequeueAfter = %v; a deleted DynamicPrefix never comes back, so polling for it is pointless",
			result.RequeueAfter)
	}

	var got corev1.Service
	if err := fakeClient.Get(ctx, key, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	assertServiceReleased(t, got.GetAnnotations(), operatorAddr, userIPv4)
}

// TestServiceReleasedWhenHAModeDisabled covers switching transition.mode from ha
// back to simple. Reconcile returned early for a non-HA prefix, so the entries
// written while HA was on were left behind and stopped being updated.
func TestServiceReleasedWhenHAModeDisabled(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = dynamicprefixiov1alpha1.AddToScheme(scheme)

	const userIPv4 = "192.0.2.238"
	const operatorAddr = "2001:db8:abcd::ffff:0:2"

	dp := &dynamicprefixiov1alpha1.DynamicPrefix{
		ObjectMeta: metav1.ObjectMeta{Name: "home-ipv6"},
		Spec: dynamicprefixiov1alpha1.DynamicPrefixSpec{
			Transition: &dynamicprefixiov1alpha1.TransitionSpec{
				Mode: dynamicprefixiov1alpha1.TransitionModeSimple,
			},
		},
		Status: dynamicprefixiov1alpha1.DynamicPrefixStatus{CurrentPrefix: "2001:db8:abcd::/64"},
	}
	svc := newManagedService("home-ipv6", operatorAddr, userIPv4)

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(dp, svc).Build()
	r := &ServiceSyncReconciler{Client: fakeClient, Scheme: scheme}
	key := types.NamespacedName{Name: "svc", Namespace: "network"}

	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var got corev1.Service
	if err := fakeClient.Get(ctx, key, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	assertServiceReleased(t, got.GetAnnotations(), operatorAddr, userIPv4)
}

// TestPoolReleasedWhenDynamicPrefixDeleted covers the same deletion for pools.
// syncPool returned the NotFound error and retried with backoff forever, leaving
// the blocks in place and the error counter climbing.
func TestPoolReleasedWhenDynamicPrefixDeleted(t *testing.T) {
	ctx := context.Background()
	scheme := newPoolBackendTestScheme(t)
	scheme.AddKnownTypeWithName(DefaultCiliumLBIPPoolGVK, &unstructured.Unstructured{})

	const userBlock = "2001:db8:9999::/64"
	const operatorBlock = "2001:db8:abcd::/64"

	pool := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": APIVersion(DefaultCiliumLBIPPoolGVK),
			"kind":       DefaultCiliumLBIPPoolGVK.Kind,
			"metadata": map[string]interface{}{
				"name": "lb-pool",
				"annotations": map[string]interface{}{
					AnnotationName: "home-ipv6",
					// Records key blocks the way blockKey does, not as bare CIDRs.
					AnnotationManagedBlocks: blockKey(map[string]interface{}{"cidr": operatorBlock}),
				},
			},
			"spec": map[string]interface{}{
				"blocks": []interface{}{
					map[string]interface{}{"cidr": userBlock},
					map[string]interface{}{"cidr": operatorBlock},
				},
			},
		},
	}
	pool.SetGroupVersionKind(DefaultCiliumLBIPPoolGVK)

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pool).Build()
	r := &PoolSyncReconciler{Client: fakeClient, Scheme: scheme}

	if _, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "lb-pool"},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	fetched := &unstructured.Unstructured{}
	fetched.SetGroupVersionKind(DefaultCiliumLBIPPoolGVK)
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: "lb-pool"}, fetched); err != nil {
		t.Fatalf("get: %v", err)
	}

	blocks, _, err := unstructured.NestedSlice(fetched.Object, "spec", "blocks")
	if err != nil {
		t.Fatalf("read blocks: %v", err)
	}
	var cidrs []string
	for _, b := range blocks {
		if block, ok := b.(map[string]interface{}); ok {
			if cidr, ok := block["cidr"].(string); ok {
				cidrs = append(cidrs, cidr)
			}
		}
	}
	if containsString(cidrs, operatorBlock) {
		t.Errorf("operator block was not released: %v", cidrs)
	}
	if !containsString(cidrs, userBlock) {
		t.Errorf("user block %q was removed; only recorded entries may be released: %v", userBlock, cidrs)
	}
	if _, ok := fetched.GetAnnotations()[AnnotationManagedBlocks]; ok {
		t.Error("managed-blocks record should be gone after release")
	}
}

// TestCalicoReleaseRemovesDrainingSiblings covers the objects the operator
// created outright. Siblings carry no owner reference and were pruned only from
// inside a successful sync, so de-annotating the parent left them allocating from
// a prefix the ISP had already withdrawn.
func TestCalicoReleaseRemovesDrainingSiblings(t *testing.T) {
	ctx := context.Background()
	const maxHistory = 2
	scheme := newPoolBackendTestScheme(t)

	pool := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": APIVersion(DefaultCalicoIPPoolGVK),
			"kind":       DefaultCalicoIPPoolGVK.Kind,
			"metadata": map[string]interface{}{
				"name":        "calico-pool",
				"annotations": map[string]interface{}{AnnotationName: "home-ipv6"},
			},
			"spec": map[string]interface{}{"cidr": "2001:db8:abcd::/64"},
		},
	}
	pool.SetGroupVersionKind(DefaultCalicoIPPoolGVK)

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pool).Build()
	r := &PoolSyncReconciler{
		Client: fakeClient,
		Scheme: scheme,
		// Calico is only reconciled when its CRD was discovered, which is what
		// BackendGVKs stands in for here.
		BackendGVKs: []schema.GroupVersionKind{DefaultCalicoIPPoolGVK},
	}
	backend := calicoIPPoolBackend{resourceGVK: DefaultCalicoIPPoolGVK}

	// Rotate enough to accumulate draining siblings.
	for gen := 0; gen < 4; gen++ {
		fetched := &unstructured.Unstructured{}
		fetched.SetGroupVersionKind(DefaultCalicoIPPoolGVK)
		if err := fakeClient.Get(ctx, types.NamespacedName{Name: "calico-pool"}, fetched); err != nil {
			t.Fatalf("gen %d: get: %v", gen, err)
		}
		configs, managed := calicoPoolStateForGeneration(gen, maxHistory)
		if _, err := backend.update(ctx, r, fetched, configs, managed); err != nil {
			t.Fatalf("gen %d: update: %v", gen, err)
		}
	}

	fetched := &unstructured.Unstructured{}
	fetched.SetGroupVersionKind(DefaultCalicoIPPoolGVK)
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: "calico-pool"}, fetched); err != nil {
		t.Fatalf("get before release: %v", err)
	}
	if _, ok := fetched.GetAnnotations()[AnnotationManagedCIDR]; !ok {
		t.Fatal("Calico sync must record spec.cidr as managed, or the release path can never find it")
	}

	// De-annotate, exactly as a user removing the binding would.
	annotations := fetched.GetAnnotations()
	delete(annotations, AnnotationName)
	fetched.SetAnnotations(annotations)
	if err := fakeClient.Update(ctx, fetched); err != nil {
		t.Fatalf("de-annotate: %v", err)
	}

	if _, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "calico-pool"},
	}); err != nil {
		t.Fatalf("reconcile after de-annotation: %v", err)
	}

	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(ListGVK(DefaultCalicoIPPoolGVK))
	if err := fakeClient.List(ctx, list); err != nil {
		t.Fatalf("list: %v", err)
	}
	for i := range list.Items {
		if list.Items[i].GetLabels()[LabelCalicoParentPool] == "calico-pool" {
			t.Errorf("draining sibling %s survived release; it allocates from a withdrawn prefix",
				list.Items[i].GetName())
		}
	}

	released := &unstructured.Unstructured{}
	released.SetGroupVersionKind(DefaultCalicoIPPoolGVK)
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: "calico-pool"}, released); err != nil {
		t.Fatalf("get after release: %v", err)
	}
	if _, ok := released.GetAnnotations()[AnnotationManagedCIDR]; ok {
		t.Error("managed-cidr record should be gone after release")
	}
}

// TestReleaseDoesNotInventFieldsOnOtherBackends pins the backend dispatch. The
// release path must branch on the matched backend, not on which record
// annotation is present -- otherwise a CIDR group carrying a blocks record gets
// an empty spec.blocks created on it, a field its schema does not have.
func TestReleaseDoesNotInventFieldsOnOtherBackends(t *testing.T) {
	ctx := context.Background()
	scheme := newPoolBackendTestScheme(t)

	group := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": APIVersion(DefaultCiliumCIDRGroupGVK),
			"kind":       DefaultCiliumCIDRGroupGVK.Kind,
			"metadata": map[string]interface{}{
				"name": "cidr-group",
				"annotations": map[string]interface{}{
					// A blocks record on an object that has no spec.blocks.
					AnnotationManagedBlocks: "2001:db8:abcd::/64",
					AnnotationManagedCIDRs:  "2001:db8:abcd::/64",
				},
			},
			"spec": map[string]interface{}{
				"externalCIDRs": []interface{}{"2001:db8:abcd::/64"},
			},
		},
	}
	group.SetGroupVersionKind(DefaultCiliumCIDRGroupGVK)

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(group).Build()
	r := &PoolSyncReconciler{Client: fakeClient, Scheme: scheme}

	if _, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "cidr-group"},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	fetched := &unstructured.Unstructured{}
	fetched.SetGroupVersionKind(DefaultCiliumCIDRGroupGVK)
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: "cidr-group"}, fetched); err != nil {
		t.Fatalf("get: %v", err)
	}
	if _, found, _ := unstructured.NestedSlice(fetched.Object, "spec", "blocks"); found {
		t.Error("spec.blocks was created on a CiliumCIDRGroup, which has no such field")
	}
}

// TestExternalDNSTargetReleasedOnOptOut covers opting out after the target was
// already managed. Merely ceasing to update it would leave an address that stops
// resolving at the next rotation, so the opt-out has to hand the field back.
func TestExternalDNSTargetReleasedOnOptOut(t *testing.T) {
	ctx := context.Background()
	const maxHistory = 2
	r, dp, key := newOwnershipTestFixture(t, maxHistory)

	rotatePrefix(dp, 3, maxHistory)
	if err := r.Status().Update(ctx, dp); err != nil {
		t.Fatalf("status update: %v", err)
	}
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatalf("initial reconcile: %v", err)
	}

	var svc corev1.Service
	if err := r.Get(ctx, key, &svc); err != nil {
		t.Fatalf("get: %v", err)
	}
	if svc.Annotations[AnnotationExternalDNSTarget] == "" {
		t.Fatal("expected a managed external-dns target before opting out")
	}

	// A hostname the user put there must survive the handover.
	annotations := svc.GetAnnotations()
	annotations[AnnotationExternalDNSTarget] = "example.com," + annotations[AnnotationExternalDNSTarget]
	annotations[AnnotationSkipExternalDNSUpdate] = AnnotationValueTrue
	svc.SetAnnotations(annotations)
	if err := r.Update(ctx, &svc); err != nil {
		t.Fatalf("opt out: %v", err)
	}

	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile after opt-out: %v", err)
	}

	if err := r.Get(ctx, key, &svc); err != nil {
		t.Fatalf("get after opt-out: %v", err)
	}
	if got := svc.Annotations[AnnotationExternalDNSTarget]; got != "example.com" {
		t.Errorf("external-dns target = %q, want just the user's hostname", got)
	}
	if _, ok := svc.Annotations[AnnotationManagedTargets]; ok {
		t.Error("managed-targets record should be gone after opting out")
	}
}

// TestUserPinnedAddressSurvivesRotation covers a user pinning an address that the
// operator will later start generating. Claiming it on the pass where the two
// coincide would grant the operator the right to delete it once it stops being
// generated, turning a deliberate pin into a delayed failure with nothing to point
// at as the cause.
//
// Note the limit of what any ownership scheme can promise here: an address that
// the operator already has in the field and in its record is indistinguishable
// from one the user typed there, so the protection begins at the moment the
// operator would otherwise take a new claim over it.
func TestUserPinnedAddressSurvivesRotation(t *testing.T) {
	ctx := context.Background()
	const maxHistory = 2
	r, dp, key := newOwnershipTestFixture(t, maxHistory)

	// Establish a record at generation 0.
	rotatePrefix(dp, 0, maxHistory)
	if err := r.Status().Update(ctx, dp); err != nil {
		t.Fatalf("status update: %v", err)
	}
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatalf("initial reconcile: %v", err)
	}

	// Pin the address a *later* generation will produce, before the operator ever
	// generates it.
	pinned := genAddr(4)
	var svc corev1.Service
	if err := r.Get(ctx, key, &svc); err != nil {
		t.Fatalf("get: %v", err)
	}
	annotations := svc.GetAnnotations()
	annotations[AnnotationCiliumIPs] = annotations[AnnotationCiliumIPs] + "," + pinned
	svc.SetAnnotations(annotations)
	if err := r.Update(ctx, &svc); err != nil {
		t.Fatalf("pin: %v", err)
	}

	// Rotate onto that generation and then well past it, so the address stops
	// being generated and leaves the history window entirely.
	for gen := 1; gen < 9; gen++ {
		rotatePrefix(dp, gen, maxHistory)
		if err := r.Status().Update(ctx, dp); err != nil {
			t.Fatalf("gen %d: status update: %v", gen, err)
		}
		if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
			t.Fatalf("gen %d: reconcile: %v", gen, err)
		}
	}

	if err := r.Get(ctx, key, &svc); err != nil {
		t.Fatalf("final get: %v", err)
	}
	ips := strings.Split(svc.Annotations[AnnotationCiliumIPs], ",")
	if !containsString(ips, pinned) {
		t.Errorf("the user's pinned address %q was deleted after it stopped being "+
			"generated: %v", pinned, ips)
	}
}

// TestOwnershipSurvivesNonCanonicalSpelling covers ownership being decided by
// what an address is rather than how it was typed. Comparing raw strings both
// fails to recognise a user's pin and lets the same address be requested twice.
func TestOwnershipSurvivesNonCanonicalSpelling(t *testing.T) {
	// Upper case and leading zeros: the same address, spelled differently.
	rec := parseOwnershipRecord("2001:0DB8:ABCD::FFFF:0:2", true)
	if !rec.owns("2001:db8:abcd::ffff:0:2") {
		t.Error("owns() failed to recognise the same address in canonical form")
	}

	got := dedupePreservingOrder([]string{"2001:0DB8:ABCD::FFFF:0:2", "2001:db8:abcd::ffff:0:2"})
	if len(got) != 1 {
		t.Errorf("dedupePreservingOrder() kept %d entries, want 1: the same address "+
			"must not be requested twice\n  %v", len(got), got)
	}

	// An absent stop and an empty stop describe the same block.
	withEmpty := blockKey(map[string]interface{}{"start": "2001:db8::1", "stop": ""})
	without := blockKey(map[string]interface{}{"start": "2001:db8::1"})
	if withEmpty != without {
		t.Errorf("blockKey() = %q with an empty stop and %q without; they must match",
			withEmpty, without)
	}
}

// TestSuffixChangeRetiresOldAddresses pins a behaviour the record buys that
// geometry never could: when the suffix changes, the addresses derived from the
// old one are recognised as the operator's and withdrawn.
func TestSuffixChangeRetiresOldAddresses(t *testing.T) {
	ctx := context.Background()
	const maxHistory = 2
	r, dp, key := newOwnershipTestFixture(t, maxHistory)

	rotatePrefix(dp, 2, maxHistory)
	if err := r.Status().Update(ctx, dp); err != nil {
		t.Fatalf("status update: %v", err)
	}
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var svc corev1.Service
	if err := r.Get(ctx, key, &svc); err != nil {
		t.Fatalf("get: %v", err)
	}
	before := svc.Annotations[AnnotationCiliumIPs]
	if !strings.Contains(before, ":0:2") {
		t.Fatalf("expected addresses derived from the original suffix, got %q", before)
	}

	annotations := svc.GetAnnotations()
	annotations[AnnotationSuffix] = "::ffff:0:8"
	svc.SetAnnotations(annotations)
	if err := r.Update(ctx, &svc); err != nil {
		t.Fatalf("change suffix: %v", err)
	}
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile after suffix change: %v", err)
	}

	if err := r.Get(ctx, key, &svc); err != nil {
		t.Fatalf("get after suffix change: %v", err)
	}
	for _, ip := range strings.Split(svc.Annotations[AnnotationCiliumIPs], ",") {
		ip = strings.TrimSpace(ip)
		if strings.HasPrefix(ip, "2003:") && strings.HasSuffix(ip, ":0:2") {
			t.Errorf("address from the previous suffix was not retired: %q in %q",
				ip, svc.Annotations[AnnotationCiliumIPs])
		}
	}
}

// TestLegacyModeFollowsRotation covers a Service with no suffix annotation. Such a
// Service derived everything from its already-assigned address, so after a
// rotation it kept requesting the superseded one and never asked for an address in
// the new prefix -- waiting on an assignment that only arrives once something else
// moves first.
func TestLegacyModeFollowsRotation(t *testing.T) {
	ctx := context.Background()
	const maxHistory = 2
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = dynamicprefixiov1alpha1.AddToScheme(scheme)

	dp := &dynamicprefixiov1alpha1.DynamicPrefix{
		ObjectMeta: metav1.ObjectMeta{Name: "home-ipv6"},
		Spec: dynamicprefixiov1alpha1.DynamicPrefixSpec{
			Transition: &dynamicprefixiov1alpha1.TransitionSpec{
				Mode:             dynamicprefixiov1alpha1.TransitionModeHA,
				MaxPrefixHistory: maxHistory,
			},
		},
	}
	rotatePrefix(dp, 1, maxHistory)

	// Assigned in the prefix that is now merely historical.
	assigned := genAddr(0)
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "legacy",
			Namespace: "network",
			// No dynamic-prefix.io/suffix: this is legacy mode.
			Annotations: map[string]string{AnnotationName: "home-ipv6"},
		},
		Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer},
		Status: corev1.ServiceStatus{
			LoadBalancer: corev1.LoadBalancerStatus{
				Ingress: []corev1.LoadBalancerIngress{{IP: assigned}},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(dp, svc).
		WithStatusSubresource(dp, svc).
		WithTypeConverters(testTypeConverters(scheme)...).
		Build()
	r := &ServiceSyncReconciler{Client: fakeClient, Scheme: scheme}
	key := types.NamespacedName{Name: "legacy", Namespace: "network"}

	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var got corev1.Service
	if err := fakeClient.Get(ctx, key, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	ips := strings.Split(got.Annotations[AnnotationCiliumIPs], ",")
	want := genAddr(1) // the current prefix
	if !containsString(ips, want) {
		t.Errorf("legacy mode did not request an address in the current prefix: "+
			"want %q in %v", want, ips)
	}
}
