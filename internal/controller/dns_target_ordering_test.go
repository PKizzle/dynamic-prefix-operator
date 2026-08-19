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
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// assignAddresses puts addresses into the Service's LoadBalancer status, the way
// LB-IPAM does once it has actually handed them out.
func assignAddresses(t *testing.T, r *ServiceSyncReconciler, key types.NamespacedName, addrs ...string) {
	t.Helper()
	ctx := context.Background()

	var svc corev1.Service
	if err := r.Get(ctx, key, &svc); err != nil {
		t.Fatalf("get service: %v", err)
	}
	ingress := make([]corev1.LoadBalancerIngress, 0, len(addrs))
	for _, a := range addrs {
		ingress = append(ingress, corev1.LoadBalancerIngress{IP: a})
	}
	svc.Status.LoadBalancer.Ingress = ingress
	if err := r.Status().Update(ctx, &svc); err != nil {
		t.Fatalf("status update: %v", err)
	}
}

// During a rotation the external-dns target must not move to the new address
// until that address is actually assigned.
//
// It used to move in the same pass that requested the address, so every
// rotation published a name that resolved to an address nothing answered on
// yet. Holding the previous target through the window is the better failure:
// it keeps pointing somewhere that works.
func TestExternalDNSTargetWaitsForTheAddressToBeAssigned(t *testing.T) {
	ctx := context.Background()
	const maxHistory = 2
	r, dp, key := newOwnershipTestFixture(t, maxHistory)

	// Generation 0 is live and assigned: steady state.
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatalf("initial reconcile: %v", err)
	}
	assignAddresses(t, r, key, genAddr(0))
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile after assignment: %v", err)
	}

	var svc corev1.Service
	if err := r.Get(ctx, key, &svc); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got, want := svc.Annotations[AnnotationExternalDNSTarget], genAddr(0); got != want {
		t.Fatalf("external-dns target = %q, want %q once the address is assigned", got, want)
	}

	// The prefix rotates. LB-IPAM has not caught up: status still holds the old
	// address only.
	rotatePrefix(dp, 1, maxHistory)
	if err := r.Status().Update(ctx, dp); err != nil {
		t.Fatalf("status update: %v", err)
	}
	result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
	if err != nil {
		t.Fatalf("reconcile during rotation: %v", err)
	}

	if err := r.Get(ctx, key, &svc); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got, want := svc.Annotations[AnnotationExternalDNSTarget], genAddr(0); got != want {
		t.Errorf("external-dns target = %q, want the previous address %q: the new one is not "+
			"assigned yet, so publishing it resolves a name nothing answers on", got, want)
	}
	if result.RequeueAfter == 0 && !result.Requeue { //nolint:staticcheck // Requeue is what the older API sets
		t.Error("reconcile did not requeue while the target was withheld, so nothing would " +
			"retry if the assignment event had already passed")
	}

	// LB-IPAM assigns the new address; now the target may move.
	assignAddresses(t, r, key, genAddr(1))
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile after new assignment: %v", err)
	}
	if err := r.Get(ctx, key, &svc); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got, want := svc.Annotations[AnnotationExternalDNSTarget], genAddr(1); got != want {
		t.Errorf("external-dns target = %q, want %q once the new address is assigned", got, want)
	}
}
