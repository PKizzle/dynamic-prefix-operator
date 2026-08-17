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
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	dynamicprefixiov1alpha1 "github.com/pkizzle/dynamic-prefix-operator/api/v1alpha1"
)

// testDynamicPrefixName is the DynamicPrefix every test in this file binds to.
const testDynamicPrefixName = "home-ipv6"

// poolsSyncedCondition reads the condition back off the stored DynamicPrefix.
func poolsSyncedCondition(t *testing.T, r *PoolSyncReconciler) *metav1.Condition {
	t.Helper()
	var dp dynamicprefixiov1alpha1.DynamicPrefix
	if err := r.Get(context.Background(), types.NamespacedName{Name: testDynamicPrefixName}, &dp); err != nil {
		t.Fatalf("get DynamicPrefix: %v", err)
	}
	return meta.FindStatusCondition(dp.Status.Conditions, dynamicprefixiov1alpha1.ConditionTypePoolsSynced)
}

// TestPoolsSyncedClearedWhenFailingPoolIsReleased covers a pool that fails and is
// then handed back. Nothing reconciles it through the success path again, so
// without clearing its state the condition goes on naming a pool the operator no
// longer manages and `kubectl wait --for=condition=PoolsSynced` never returns.
func TestPoolsSyncedClearedWhenFailingPoolIsReleased(t *testing.T) {
	ctx := context.Background()
	scheme := newPoolBackendTestScheme(t)
	scheme.AddKnownTypeWithName(DefaultCiliumLBIPPoolGVK, &unstructured.Unstructured{})

	const operatorBlock = "2001:db8:abcd::/64"

	dp := &dynamicprefixiov1alpha1.DynamicPrefix{
		ObjectMeta: metav1.ObjectMeta{Name: "home-ipv6"},
	}

	// Released because the binding annotation is gone, with the operator's own
	// record still on the object.
	pool := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": APIVersion(DefaultCiliumLBIPPoolGVK),
			"kind":       DefaultCiliumLBIPPoolGVK.Kind,
			"metadata": map[string]interface{}{
				"name": "lb-pool",
				"annotations": map[string]interface{}{
					AnnotationManagedBlocks: blockKey(map[string]interface{}{"cidr": operatorBlock}),
				},
			},
			"spec": map[string]interface{}{
				"blocks": []interface{}{
					map[string]interface{}{"cidr": operatorBlock},
				},
			},
		},
	}
	pool.SetGroupVersionKind(DefaultCiliumLBIPPoolGVK)

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(dp, pool).
		WithStatusSubresource(dp).
		WithTypeConverters(testTypeConverters(scheme)...).
		Build()
	r := &PoolSyncReconciler{Client: fakeClient, Scheme: scheme}

	// The pool failed its last sync.
	key := poolStateKey{backend: "cilium-load-balancer-ip-pool", pool: "/lb-pool"}
	r.updatePoolsSyncedCondition(ctx, "home-ipv6", key, errors.New("webhook rejected the update"))
	if cond := poolsSyncedCondition(t, r); cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("expected PoolsSynced=False after a failure, got %+v", cond)
	}

	if _, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "lb-pool"},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	cond := poolsSyncedCondition(t, r)
	if cond == nil {
		t.Fatal("PoolsSynced condition disappeared")
	}
	if cond.Status != metav1.ConditionTrue {
		t.Errorf("PoolsSynced = %s (%q) after the failing pool was released, want True",
			cond.Status, cond.Message)
	}
}

// TestPoolsSyncedKeyedByBackend covers two backends sharing one name. Several
// pool kinds are cluster-scoped, so keying the aggregate on the name alone lets a
// healthy pool clear a broken sibling's entry and report the whole set as synced.
func TestPoolsSyncedKeyedByBackend(t *testing.T) {
	ctx := context.Background()
	scheme := newPoolBackendTestScheme(t)

	dp := &dynamicprefixiov1alpha1.DynamicPrefix{
		ObjectMeta: metav1.ObjectMeta{Name: "home-ipv6"},
	}
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(dp).
		WithStatusSubresource(dp).
		WithTypeConverters(testTypeConverters(scheme)...).
		Build()
	r := &PoolSyncReconciler{Client: fakeClient, Scheme: scheme}

	failing := poolStateKey{backend: "cilium-load-balancer-ip-pool", pool: "/shared-name"}
	healthy := poolStateKey{backend: "cilium-cidr-group", pool: "/shared-name"}

	r.updatePoolsSyncedCondition(ctx, "home-ipv6", failing, errors.New("immutable field"))
	r.updatePoolsSyncedCondition(ctx, "home-ipv6", healthy, nil)

	cond := poolsSyncedCondition(t, r)
	if cond == nil {
		t.Fatal("PoolsSynced condition was never written")
	}
	if cond.Status != metav1.ConditionFalse {
		t.Fatalf("PoolsSynced = %s (%q); a healthy CIDR group cleared a broken load-balancer pool of the same name",
			cond.Status, cond.Message)
	}
	if !strings.Contains(cond.Message, "cilium-load-balancer-ip-pool") {
		t.Errorf("message %q does not say which backend is failing", cond.Message)
	}

	// The remaining backend recovering clears it.
	r.updatePoolsSyncedCondition(ctx, "home-ipv6", failing, nil)
	if cond := poolsSyncedCondition(t, r); cond.Status != metav1.ConditionTrue {
		t.Errorf("PoolsSynced = %s (%q) once every backend is in sync, want True", cond.Status, cond.Message)
	}
}

// TestPoolsSyncedClearedWhenPoolIsDeleted covers the pool disappearing outright:
// no backend object answers to the name any more, so nothing will ever reconcile
// it through the success path.
func TestPoolsSyncedClearedWhenPoolIsDeleted(t *testing.T) {
	ctx := context.Background()
	scheme := newPoolBackendTestScheme(t)

	dp := &dynamicprefixiov1alpha1.DynamicPrefix{
		ObjectMeta: metav1.ObjectMeta{Name: "home-ipv6"},
	}
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(dp).
		WithStatusSubresource(dp).
		WithTypeConverters(testTypeConverters(scheme)...).
		Build()
	r := &PoolSyncReconciler{Client: fakeClient, Scheme: scheme}

	key := poolStateKey{backend: "cilium-load-balancer-ip-pool", pool: "/gone-pool"}
	r.updatePoolsSyncedCondition(ctx, "home-ipv6", key, errors.New("webhook rejected the update"))

	// Reconciling a name that no longer resolves to any backend object.
	if _, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "gone-pool"},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	cond := poolsSyncedCondition(t, r)
	if cond == nil {
		t.Fatal("PoolsSynced condition disappeared")
	}
	if cond.Status != metav1.ConditionTrue {
		t.Errorf("PoolsSynced = %s (%q) after the pool was deleted, want True", cond.Status, cond.Message)
	}
}
