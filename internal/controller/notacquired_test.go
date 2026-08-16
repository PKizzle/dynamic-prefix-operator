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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	dynamicprefixiov1alpha1 "github.com/pkizzle/dynamic-prefix-operator/api/v1alpha1"
)

// TestPoolWaitsForFirstPrefix covers a fresh install: between creating the
// resources and the first Router Advertisement there is no prefix to write.
// Reported as a sync failure it raises a Warning on every pool, parks
// PoolsSynced at False and backs off exponentially toward a quarter of an hour,
// so the pool can stay empty long after the prefix arrives.
func TestPoolWaitsForFirstPrefix(t *testing.T) {
	ctx := context.Background()
	scheme := newPoolBackendTestScheme(t)
	scheme.AddKnownTypeWithName(DefaultCiliumLBIPPoolGVK, &unstructured.Unstructured{})

	dp := &dynamicprefixiov1alpha1.DynamicPrefix{
		ObjectMeta: metav1.ObjectMeta{Name: "home-ipv6"},
	}

	pool := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": APIVersion(DefaultCiliumLBIPPoolGVK),
			"kind":       DefaultCiliumLBIPPoolGVK.Kind,
			"metadata": map[string]interface{}{
				"name":        "lb-pool",
				"annotations": map[string]interface{}{AnnotationName: "home-ipv6"},
			},
			"spec": map[string]interface{}{"blocks": []interface{}{}},
		},
	}
	pool.SetGroupVersionKind(DefaultCiliumLBIPPoolGVK)

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(dp, pool).
		WithStatusSubresource(dp).
		Build()
	recorder := events.NewFakeRecorder(8)
	r := &PoolSyncReconciler{Client: fakeClient, Scheme: scheme, Recorder: recorder}

	res, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "lb-pool"},
	})
	if err != nil {
		t.Fatalf("waiting for the first prefix must not be an error: %v", err)
	}
	if res.RequeueAfter <= 0 {
		t.Error("expected a requeue while waiting for the first prefix")
	}
	if cond := poolsSyncedCondition(t, r); cond != nil && cond.Status == metav1.ConditionFalse {
		t.Errorf("PoolsSynced was parked at False while merely waiting: %q", cond.Message)
	}
	for {
		select {
		case ev := <-recorder.Events:
			if strings.Contains(ev, corev1.EventTypeWarning) {
				t.Errorf("waiting for the first prefix raised a Warning: %s", ev)
			}
			continue
		default:
		}
		break
	}
}

// TestSuffixServiceWaitsForFirstPrefix covers the same state on the HA-mode
// Service path, where a well-formed suffix annotation was reported as a
// malformed one until the prefix arrived.
func TestSuffixServiceWaitsForFirstPrefix(t *testing.T) {
	ctx := context.Background()
	scheme := newPoolBackendTestScheme(t)

	dp := &dynamicprefixiov1alpha1.DynamicPrefix{
		ObjectMeta: metav1.ObjectMeta{Name: "home-ipv6"},
		Spec: dynamicprefixiov1alpha1.DynamicPrefixSpec{
			Transition: &dynamicprefixiov1alpha1.TransitionSpec{
				Mode: dynamicprefixiov1alpha1.TransitionModeHA,
			},
		},
	}

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web",
			Namespace: "default",
			Annotations: map[string]string{
				AnnotationName:   "home-ipv6",
				AnnotationSuffix: "::42",
			},
		},
		Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(dp, svc).
		WithStatusSubresource(dp, svc).
		Build()
	r := &ServiceSyncReconciler{Client: fakeClient, Scheme: scheme}

	res, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "web", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("waiting for the first prefix must not be an error: %v", err)
	}
	if res.RequeueAfter <= 0 {
		t.Error("expected a requeue while waiting for the first prefix")
	}
}
