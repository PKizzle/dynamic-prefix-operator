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
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	dynamicprefixiov1alpha1 "github.com/pkizzle/dynamic-prefix-operator/api/v1alpha1"
)

// The user's static entries. The ULA matters specifically: its host suffix
// (0:ffff:0:2) is identical to the operator's, so any ownership rule based on
// address shape rather than on a record would claim and then delete it, breaking
// stable internal addressing. It must survive every rotation untouched.
const (
	testStaticIPv4 = "192.168.178.238"
	testStaticULA  = "fdb3:e6dc:eb35::ffff:0:2"
	testSuffix     = "::ffff:0:2"
)

// rotatePrefix sets Status to the state the DynamicPrefix controller would leave
// after `gen` rotations: one current prefix plus the most recent maxHistory
// entries, oldest first. Everything older has been evicted, which is precisely
// the moment the old geometric ownership test lost track of its own addresses.
func rotatePrefix(dp *dynamicprefixiov1alpha1.DynamicPrefix, gen, maxHistory int) {
	dp.Status.CurrentPrefix = fmt.Sprintf("2003:107:c700:%x00::/64", gen)
	dp.Status.History = nil
	for i := gen - maxHistory; i < gen; i++ {
		if i < 0 {
			continue
		}
		dp.Status.History = append(dp.Status.History, dynamicprefixiov1alpha1.PrefixHistoryEntry{
			Prefix:     fmt.Sprintf("2003:107:c700:%x00::/64", i),
			AcquiredAt: metav1.Now(),
			State:      dynamicprefixiov1alpha1.PrefixStateDraining,
		})
	}
}

func newOwnershipTestFixture(t *testing.T, maxHistory int) (*ServiceSyncReconciler, *dynamicprefixiov1alpha1.DynamicPrefix, types.NamespacedName) {
	t.Helper()

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
	rotatePrefix(dp, 0, maxHistory)

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "haproxy-external",
			Namespace: "network",
			Annotations: map[string]string{
				AnnotationName:      "home-ipv6",
				AnnotationSuffix:    testSuffix,
				AnnotationCiliumIPs: testStaticIPv4 + "," + testStaticULA,
			},
		},
		Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(dp, svc).
		WithStatusSubresource(dp, svc).
		Build()

	return &ServiceSyncReconciler{Client: fakeClient, Scheme: scheme},
		dp,
		types.NamespacedName{Name: "haproxy-external", Namespace: "network"}
}

// reconcileRotations drives `rotations` prefix changes through the reconciler and
// returns the resulting lbipam.cilium.io/ips entries. stripRecord simulates the
// pre-fix operator by deleting the ownership record before each pass.
func reconcileRotations(t *testing.T, rotations, maxHistory int, stripRecord bool) []string {
	t.Helper()
	ctx := context.Background()
	r, dp, key := newOwnershipTestFixture(t, maxHistory)

	var svc corev1.Service
	for gen := 0; gen < rotations; gen++ {
		rotatePrefix(dp, gen, maxHistory)
		if err := r.Status().Update(ctx, dp); err != nil {
			t.Fatalf("rotation %d: status update: %v", gen, err)
		}

		if stripRecord {
			if err := r.Get(ctx, key, &svc); err != nil {
				t.Fatalf("rotation %d: get: %v", gen, err)
			}
			annotations := svc.GetAnnotations()
			delete(annotations, AnnotationManagedIPs)
			svc.SetAnnotations(annotations)
			if err := r.Update(ctx, &svc); err != nil {
				t.Fatalf("rotation %d: strip record: %v", gen, err)
			}
		}

		if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
			t.Fatalf("rotation %d: reconcile: %v", gen, err)
		}
	}

	if err := r.Get(ctx, key, &svc); err != nil {
		t.Fatalf("final get: %v", err)
	}
	return strings.Split(svc.GetAnnotations()[AnnotationCiliumIPs], ",")
}

// TestOwnershipRecordKeepsRequestedIPsBounded is the regression test for the leak
// that silently grew lbipam.cilium.io/ips to 108 entries on a live cluster and
// starved Cilium's L2 announcer. With maxPrefixHistory=2 the operator should
// request exactly the user's two static addresses plus current+2 historical
// addresses, no matter how many times the ISP prefix rotates.
func TestOwnershipRecordKeepsRequestedIPsBounded(t *testing.T) {
	const maxHistory = 2

	for _, rotations := range []int{1, 3, 10, 40} {
		t.Run(fmt.Sprintf("%d_rotations", rotations), func(t *testing.T) {
			got := reconcileRotations(t, rotations, maxHistory, false)

			// Before the history window has filled there are simply fewer
			// generations to request; after that the count must stay flat.
			wantDynamic := rotations
			if wantDynamic > maxHistory+1 {
				wantDynamic = maxHistory + 1
			}
			wantEntries := 2 + wantDynamic // static IPv4 + ULA, then current + history

			if len(got) != wantEntries {
				t.Errorf("after %d rotations: %d requested IPs, want %d\n  %v",
					rotations, len(got), wantEntries, got)
			}

			// The user's static entries must survive untouched. The ULA is the
			// one a shape-based ownership rule would have eaten.
			for _, must := range []string{testStaticIPv4, testStaticULA} {
				if !containsString(got, must) {
					t.Errorf("after %d rotations: static entry %q was dropped\n  %v",
						rotations, must, got)
				}
			}

			// Every dynamic entry must belong to a prefix still in play; none may
			// be a fossil from an evicted generation.
			live := livePrefixes(rotations-1, maxHistory)
			for _, entry := range got {
				if entry == testStaticIPv4 || entry == testStaticULA {
					continue
				}
				if !strings.HasPrefix(entry, "2003:107:c700:") {
					t.Errorf("unexpected entry %q", entry)
					continue
				}
				if !containsString(live, entry) {
					t.Errorf("after %d rotations: stale entry %q from an evicted prefix\n  live: %v",
						rotations, entry, live)
				}
			}
		})
	}
}

// TestLegacyPrefixTestLeaksWithoutRecord pins the behaviour of the code this
// replaces, so the test above cannot silently stop testing anything. Stripping
// the record each pass forces the old geometric fallback, and the entry count
// then grows without bound — one fossil per rotation.
func TestLegacyPrefixTestLeaksWithoutRecord(t *testing.T) {
	const maxHistory = 2
	bounded := len(reconcileRotations(t, 3, maxHistory, false))
	leaked := len(reconcileRotations(t, 10, maxHistory, true))

	if leaked <= bounded {
		t.Fatalf("expected the record-less path to leak: got %d entries after 10 rotations, "+
			"bounded path gives %d — if this now passes, the fallback changed and "+
			"TestOwnershipRecordKeepsRequestedIPsBounded may no longer be exercising the fix",
			leaked, bounded)
	}
}

// genAddr returns the address the operator derives for generation gen, in the
// canonical form netip produces. Building the string by hand is not enough:
// generation 0 yields 2003:107:c700:000:0:ffff:0:2, which canonicalises to
// 2003:107:c700::ffff:0:2, and a literal comparison would spuriously fail.
func genAddr(gen int) string {
	return netip.MustParseAddr(fmt.Sprintf("2003:107:c700:%x00:0:ffff:0:2", gen)).String()
}

// livePrefixes returns the addresses derivable from the current generation plus
// its retained history.
func livePrefixes(gen, maxHistory int) []string {
	var out []string
	for i := gen - maxHistory; i <= gen; i++ {
		if i < 0 {
			continue
		}
		out = append(out, genAddr(i))
	}
	return out
}

// TestReconcileIsIdempotentWithoutRotation guards the other direction: writing an
// ownership record every pass must not make the operator rewrite objects it has
// already settled. A controller that reports "updated" forever burns API calls
// and, with the pool backends, can oscillate against Cilium.
func TestReconcileIsIdempotentWithoutRotation(t *testing.T) {
	ctx := context.Background()
	r, dp, key := newOwnershipTestFixture(t, 2)
	rotatePrefix(dp, 5, 2)
	if err := r.Status().Update(ctx, dp); err != nil {
		t.Fatalf("status update: %v", err)
	}

	var first, second corev1.Service
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if err := r.Get(ctx, key, &first); err != nil {
		t.Fatalf("get after first: %v", err)
	}
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if err := r.Get(ctx, key, &second); err != nil {
		t.Fatalf("get after second: %v", err)
	}

	for _, key := range []string{AnnotationCiliumIPs, AnnotationManagedIPs, AnnotationExternalDNSTarget, AnnotationManagedTargets} {
		if first.Annotations[key] != second.Annotations[key] {
			t.Errorf("annotation %q churned on a no-op reconcile:\n  first:  %q\n  second: %q",
				key, first.Annotations[key], second.Annotations[key])
		}
	}
	if first.ResourceVersion != second.ResourceVersion {
		t.Errorf("Service was rewritten on a no-op reconcile (resourceVersion %s -> %s)",
			first.ResourceVersion, second.ResourceVersion)
	}
}

// TestExternalDNSTargetSurvivesOperatorDowntime covers the rarer sibling of the
// main leak. The target annotation holds only the current address, so an ordinary
// rotation always rewrites it while it is still inside the history window. If the
// operator is down for longer than maxPrefixHistory rotations, the address it left
// behind has aged out by the time it returns — and the geometric test would then
// preserve it as a user's static target, permanently.
func TestExternalDNSTargetSurvivesOperatorDowntime(t *testing.T) {
	ctx := context.Background()
	const maxHistory = 2
	r, dp, key := newOwnershipTestFixture(t, maxHistory)

	rotatePrefix(dp, 0, maxHistory)
	if err := r.Status().Update(ctx, dp); err != nil {
		t.Fatalf("status update: %v", err)
	}
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatalf("initial reconcile: %v", err)
	}

	// Operator offline for 6 rotations; generation 0 is long evicted on return.
	rotatePrefix(dp, 6, maxHistory)
	if err := r.Status().Update(ctx, dp); err != nil {
		t.Fatalf("status update after downtime: %v", err)
	}
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile after downtime: %v", err)
	}

	var svc corev1.Service
	if err := r.Get(ctx, key, &svc); err != nil {
		t.Fatalf("get: %v", err)
	}
	target := svc.Annotations[AnnotationExternalDNSTarget]
	if want := genAddr(6); target != want {
		t.Errorf("external-dns target = %q, want exactly %q — a stale address from before "+
			"the downtime was preserved instead of replaced", target, want)
	}
}

// newCiliumPool builds a CiliumLoadBalancerIPPool seeded with entries the user
// owns, which must survive every rotation.
func newCiliumPool(name string, blocks []interface{}) *unstructured.Unstructured {
	pool := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": APIVersion(DefaultCiliumLBIPPoolGVK),
			"kind":       DefaultCiliumLBIPPoolGVK.Kind,
			"metadata":   map[string]interface{}{"name": name},
			"spec":       map[string]interface{}{"blocks": blocks},
		},
	}
	pool.SetGroupVersionKind(DefaultCiliumLBIPPoolGVK)
	return pool
}

// TestPoolBlocksStayBoundedAcrossRotations is the pool-side twin of the Service
// regression test. This is the site that reached 109 blocks in production.
func TestPoolBlocksStayBoundedAcrossRotations(t *testing.T) {
	for _, maxHistory := range []int{1, 2, 3} {
		t.Run(fmt.Sprintf("maxHistory_%d", maxHistory), func(t *testing.T) {
			ctx := context.Background()
			scheme := newPoolBackendTestScheme(t)
			scheme.AddKnownTypeWithName(DefaultCiliumLBIPPoolGVK, &unstructured.Unstructured{})

			// A static ULA block the user owns; it shares the reserved host
			// suffix with the operator's own ranges.
			staticBlock := map[string]interface{}{
				"start": "fdb3:e6dc:eb35::ffff:0:1",
				"stop":  "fdb3:e6dc:eb35::ffff:0:ffff",
			}
			pool := newCiliumPool("default-ipv6", []interface{}{staticBlock})

			fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pool).Build()
			reconciler := &PoolSyncReconciler{Client: fakeClient, Scheme: scheme}
			backend := ciliumLoadBalancerIPPoolBackend{resourceGVK: DefaultCiliumLBIPPoolGVK}

			const rotations = 25
			for gen := 0; gen < rotations; gen++ {
				fetched := &unstructured.Unstructured{}
				fetched.SetGroupVersionKind(DefaultCiliumLBIPPoolGVK)
				if err := fakeClient.Get(ctx, types.NamespacedName{Name: "default-ipv6"}, fetched); err != nil {
					t.Fatalf("gen %d: get: %v", gen, err)
				}

				configs, managed := poolStateForGeneration(gen, maxHistory)
				if _, err := backend.update(ctx, reconciler, fetched, configs, managed); err != nil {
					t.Fatalf("gen %d: update: %v", gen, err)
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

			wantBlocks := 1 + maxHistory + 1 // static ULA + history + current
			if len(blocks) != wantBlocks {
				t.Errorf("after %d rotations: %d blocks, want %d — the pool is accumulating "+
					"one block per rotation again\n  %v", rotations, len(blocks), wantBlocks, blocks)
			}

			var sawStatic bool
			for _, b := range blocks {
				block, ok := b.(map[string]interface{})
				if !ok {
					continue
				}
				if blockKey(block) == blockKey(staticBlock) {
					sawStatic = true
				}
			}
			if !sawStatic {
				t.Errorf("the user's static ULA block was deleted: %v", blocks)
			}
		})
	}
}

// TestCIDRGroupStaysBoundedAcrossRotations covers the third leak site.
func TestCIDRGroupStaysBoundedAcrossRotations(t *testing.T) {
	ctx := context.Background()
	const maxHistory = 2
	scheme := newPoolBackendTestScheme(t)
	scheme.AddKnownTypeWithName(DefaultCiliumCIDRGroupGVK, &unstructured.Unstructured{})

	const staticCIDR = "fdb3:e6dc:eb35::/48"
	group := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": APIVersion(DefaultCiliumCIDRGroupGVK),
			"kind":       DefaultCiliumCIDRGroupGVK.Kind,
			"metadata":   map[string]interface{}{"name": "home"},
			"spec":       map[string]interface{}{"externalCIDRs": []interface{}{staticCIDR}},
		},
	}
	group.SetGroupVersionKind(DefaultCiliumCIDRGroupGVK)

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(group).Build()
	reconciler := &PoolSyncReconciler{Client: fakeClient, Scheme: scheme}
	backend := ciliumCIDRGroupBackend{resourceGVK: DefaultCiliumCIDRGroupGVK}

	const rotations = 25
	for gen := 0; gen < rotations; gen++ {
		fetched := &unstructured.Unstructured{}
		fetched.SetGroupVersionKind(DefaultCiliumCIDRGroupGVK)
		if err := fakeClient.Get(ctx, types.NamespacedName{Name: "home"}, fetched); err != nil {
			t.Fatalf("gen %d: get: %v", gen, err)
		}
		configs, managed := poolStateForGeneration(gen, maxHistory)
		if _, err := backend.update(ctx, reconciler, fetched, configs, managed); err != nil {
			t.Fatalf("gen %d: update: %v", gen, err)
		}
	}

	fetched := &unstructured.Unstructured{}
	fetched.SetGroupVersionKind(DefaultCiliumCIDRGroupGVK)
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: "home"}, fetched); err != nil {
		t.Fatalf("final get: %v", err)
	}
	cidrs, _, err := unstructured.NestedSlice(fetched.Object, "spec", "externalCIDRs")
	if err != nil {
		t.Fatalf("read externalCIDRs: %v", err)
	}

	wantCIDRs := 1 + maxHistory + 1 // static + history + current
	if len(cidrs) != wantCIDRs {
		t.Errorf("after %d rotations: %d CIDRs, want %d\n  %v", rotations, len(cidrs), wantCIDRs, cidrs)
	}
	var sawStatic bool
	for _, c := range cidrs {
		if s, ok := c.(string); ok && s == staticCIDR {
			sawStatic = true
		}
	}
	if !sawStatic {
		t.Errorf("the user's static CIDR was deleted: %v", cidrs)
	}
}

// poolStateForGeneration returns the pool configs and managed prefixes the
// operator would compute at generation gen, i.e. the current prefix plus the
// maxHistory most recent survivors. Everything older is gone from both — which is
// exactly why the geometric ownership test could not recognise its own entries.
func poolStateForGeneration(gen, maxHistory int) ([]poolConfiguration, []netip.Prefix) {
	var configs []poolConfiguration
	var managed []netip.Prefix
	for i := gen - maxHistory; i <= gen; i++ {
		if i < 0 {
			continue
		}
		cidr := fmt.Sprintf("2003:107:c700:%x00::/64", i)
		configs = append(configs, poolConfiguration{cidr: cidr})
		managed = append(managed, netip.MustParsePrefix(cidr))
	}
	return configs, managed
}

func TestParseOwnershipRecord(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		exists  bool
		present bool
		owns    []string
		notOwns []string
	}{
		{
			name:    "absent annotation is not present",
			exists:  false,
			present: false,
			notOwns: []string{"2001:db8::1"},
		},
		{
			name:    "empty but existing annotation is authoritative and owns nothing",
			value:   "",
			exists:  true,
			present: true,
			notOwns: []string{"2001:db8::1"},
		},
		{
			name:    "entries are parsed and whitespace trimmed",
			value:   "2001:db8::1, 2001:db8::2 ,",
			exists:  true,
			present: true,
			owns:    []string{"2001:db8::1", "2001:db8::2"},
			notOwns: []string{"2001:db8::3", ""},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := parseOwnershipRecord(tt.value, tt.exists)
			if rec.present != tt.present {
				t.Errorf("present = %v, want %v", rec.present, tt.present)
			}
			for _, e := range tt.owns {
				if !rec.owns(e) {
					t.Errorf("owns(%q) = false, want true", e)
				}
			}
			for _, e := range tt.notOwns {
				if rec.owns(e) {
					t.Errorf("owns(%q) = true, want false", e)
				}
			}
		})
	}
}

func TestPreserveUnownedIPs(t *testing.T) {
	managed := []netip.Prefix{netip.MustParsePrefix("2001:db8:1::/64")}

	t.Run("record is authoritative and ignores prefix geometry", func(t *testing.T) {
		// The recorded address is outside every managed prefix — exactly the
		// post-eviction case. It must still be recognised as ours and dropped.
		rec := parseOwnershipRecord("2001:db8:99::10", true)
		got := preserveUnownedIPs("10.0.0.1,2001:db8:99::10,fdb3::ffff:0:2", rec, managed)
		want := []string{"10.0.0.1", "fdb3::ffff:0:2"}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("falls back to prefix test when no record exists", func(t *testing.T) {
		rec := parseOwnershipRecord("", false)
		got := preserveUnownedIPs("10.0.0.1,2001:db8:1::10,2001:db8:99::10", rec, managed)
		// 2001:db8:99::10 is outside the managed prefix, so the legacy test
		// preserves it — this over-preservation is the leak, kept deliberately
		// as the safe first-pass behaviour.
		want := []string{"10.0.0.1", "2001:db8:99::10"}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("got %v, want %v", got, want)
		}
	})
}

func TestBlockKey(t *testing.T) {
	tests := []struct {
		name  string
		block map[string]interface{}
		want  string
	}{
		{"cidr block", map[string]interface{}{"cidr": "2001:db8::/64"}, "cidr=2001:db8::/64"},
		{"range block", map[string]interface{}{"start": "2001:db8::1", "stop": "2001:db8::ff"}, "range=2001:db8::1-2001:db8::ff"},
		{"start only", map[string]interface{}{"start": "2001:db8::1"}, "range=2001:db8::1"},
		{"unrecognised shape yields no key", map[string]interface{}{"foo": "bar"}, ""},
		{"empty cidr yields no key", map[string]interface{}{"cidr": ""}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := blockKey(tt.block); got != tt.want {
				t.Errorf("blockKey() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestIsOwnedBlock(t *testing.T) {
	managed := []netip.Prefix{netip.MustParsePrefix("2001:db8:1::/64")}
	evicted := map[string]interface{}{"start": "2001:db8:99::1", "stop": "2001:db8:99::ff"}

	t.Run("recorded block from an evicted prefix is still ours", func(t *testing.T) {
		rec := parseOwnershipRecord(blockKey(evicted), true)
		if !isOwnedBlock(evicted, rec, managed) {
			t.Error("isOwnedBlock() = false, want true for a recorded block")
		}
	})

	t.Run("unrecorded block is preserved even if geometry would claim it", func(t *testing.T) {
		rec := parseOwnershipRecord("", true)
		inRange := map[string]interface{}{"start": "2001:db8:1::1", "stop": "2001:db8:1::ff"}
		if isOwnedBlock(inRange, rec, managed) {
			t.Error("isOwnedBlock() = true, want false when a record exists but omits the block")
		}
	})

	t.Run("no record falls back to geometry", func(t *testing.T) {
		rec := parseOwnershipRecord("", false)
		inRange := map[string]interface{}{"start": "2001:db8:1::1", "stop": "2001:db8:1::ff"}
		if !isOwnedBlock(inRange, rec, managed) {
			t.Error("isOwnedBlock() = false, want true via the legacy prefix test")
		}
		if isOwnedBlock(evicted, rec, managed) {
			t.Error("isOwnedBlock() = true for an evicted prefix; that is the leak being fixed")
		}
	})
}

func TestDedupePreservingOrder(t *testing.T) {
	got := dedupePreservingOrder([]string{"a", "b", "a", "c", "b"})
	want := "a,b,c"
	if strings.Join(got, ",") != want {
		t.Errorf("got %v, want %v", got, want)
	}
}
