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
	"net/netip"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dynamicprefixiov1alpha1 "github.com/pkizzle/dynamic-prefix-operator/api/v1alpha1"
)

// testStatusPrefix is the delegation the status-writer tests report on. Shared
// with the envtest half, which exercises the same scenario against a real API
// server.
const testStatusPrefix = "2001:db8:1::/56"

func mustParsePrefix(t *testing.T, s string) netip.Prefix {
	t.Helper()
	p, err := netip.ParsePrefix(s)
	if err != nil {
		t.Fatalf("parse prefix %q: %v", s, err)
	}
	return p
}

// TestStatusWritersDoNotDisturbEachOther covers the reason the writers apply
// rather than update: three controllers report into one status object, and a
// full-object write by any of them either conflicted with the others or, worse,
// resubmitted a stale copy of their fields. Each writer owns its entries under
// its own field manager, so writes interleave freely.
func TestStatusWritersDoNotDisturbEachOther(t *testing.T) {
	ctx := context.Background()
	scheme := newPoolBackendTestScheme(t)

	dp := &dynamicprefixiov1alpha1.DynamicPrefix{
		ObjectMeta: metav1.ObjectMeta{Name: "home-ipv6", Generation: 3},
		Spec: dynamicprefixiov1alpha1.DynamicPrefixSpec{
			Subnets: []dynamicprefixiov1alpha1.SubnetSpec{
				{Name: "lb", Offset: 0, PrefixLength: 64,
					BGP: &dynamicprefixiov1alpha1.SubnetBGPSpec{Advertise: true}},
			},
		},
	}
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(dp).
		WithStatusSubresource(dp).
		WithTypeConverters(testTypeConverters(scheme)...).
		Build()

	get := func(t *testing.T) *dynamicprefixiov1alpha1.DynamicPrefix {
		t.Helper()
		var out dynamicprefixiov1alpha1.DynamicPrefix
		if err := fakeClient.Get(ctx, types.NamespacedName{Name: "home-ipv6"}, &out); err != nil {
			t.Fatalf("get: %v", err)
		}
		return &out
	}

	// The prefix reconciler writes everything it owns.
	dpr := &DynamicPrefixReconciler{Client: fakeClient, Scheme: scheme}
	current := get(t)
	current.Status.CurrentPrefix = testStatusPrefix
	current.Status.PrefixSource = dynamicprefixiov1alpha1.PrefixSourceRouterAdvertisement
	subnets, err := dpr.calculateSubnets("home-ipv6", mustParsePrefix(t, testStatusPrefix), current.Spec.Subnets)
	if err != nil {
		t.Fatalf("calculateSubnets: %v", err)
	}
	current.Status.Subnets = subnets
	dpr.setCondition(current, dynamicprefixiov1alpha1.ConditionTypePrefixAcquired,
		metav1.ConditionTrue, "PrefixAcquired", "Prefix acquired")
	if err := dpr.updateStatusIfChanged(ctx, current, &dynamicprefixiov1alpha1.DynamicPrefixStatus{}); err != nil {
		t.Fatalf("prefix writer: %v", err)
	}

	// PoolSync reports a failing pool.
	psr := &PoolSyncReconciler{Client: fakeClient, Scheme: scheme}
	psr.updatePoolsSyncedCondition(ctx, "home-ipv6",
		poolStateKey{backend: "cilium-load-balancer-ip-pool", pool: "/lb-pool"},
		errors.New("webhook rejected the update"))

	// BGPSync reports its condition.
	bgp := &BGPSyncReconciler{Client: fakeClient, Scheme: scheme}
	after := get(t)
	if err := bgp.updateStatus(ctx, after, after.Spec.Subnets); err != nil {
		t.Fatalf("bgp writer: %v", err)
	}

	// All three writers' fields coexist.
	final := get(t)
	if final.Status.CurrentPrefix != testStatusPrefix {
		t.Errorf("currentPrefix = %q after the other writers ran", final.Status.CurrentPrefix)
	}
	if len(final.Status.Subnets) != 1 || final.Status.Subnets[0].BGPAdvertisement != "dp-home-ipv6-lb" {
		t.Errorf("subnets = %+v, want the owner-derived advertisement name", final.Status.Subnets)
	}
	for _, want := range []struct {
		condType string
		status   metav1.ConditionStatus
	}{
		{dynamicprefixiov1alpha1.ConditionTypePrefixAcquired, metav1.ConditionTrue},
		{dynamicprefixiov1alpha1.ConditionTypePoolsSynced, metav1.ConditionFalse},
		{dynamicprefixiov1alpha1.ConditionTypeBGPAdvertisementReady, metav1.ConditionFalse},
	} {
		cond := meta.FindStatusCondition(final.Status.Conditions, want.condType)
		if cond == nil {
			t.Errorf("condition %s was clobbered by another writer", want.condType)
			continue
		}
		if cond.Status != want.status {
			t.Errorf("condition %s = %s, want %s", want.condType, cond.Status, want.status)
		}
	}

	// A later write by one manager leaves the others' LastTransitionTime alone.
	poolCond := meta.FindStatusCondition(final.Status.Conditions, dynamicprefixiov1alpha1.ConditionTypePoolsSynced)
	prefixCond := meta.FindStatusCondition(final.Status.Conditions, dynamicprefixiov1alpha1.ConditionTypePrefixAcquired)
	psr.updatePoolsSyncedCondition(ctx, "home-ipv6",
		poolStateKey{backend: "cilium-load-balancer-ip-pool", pool: "/lb-pool"}, nil)
	final = get(t)
	newPool := meta.FindStatusCondition(final.Status.Conditions, dynamicprefixiov1alpha1.ConditionTypePoolsSynced)
	if newPool.Status != metav1.ConditionTrue {
		t.Errorf("PoolsSynced = %s after recovery, want True", newPool.Status)
	}
	if !newPool.LastTransitionTime.Equal(&poolCond.LastTransitionTime) &&
		newPool.LastTransitionTime.Before(&poolCond.LastTransitionTime) {
		t.Error("PoolsSynced transition time moved backwards")
	}
	newPrefix := meta.FindStatusCondition(final.Status.Conditions, dynamicprefixiov1alpha1.ConditionTypePrefixAcquired)
	if !newPrefix.LastTransitionTime.Equal(&prefixCond.LastTransitionTime) {
		t.Error("a PoolsSynced write disturbed PrefixAcquired's LastTransitionTime")
	}
}

// TestPrefixWriterOwnsAdvertisementNames covers the subnets list's single
// owner: the advertisement name is pure spec, so the prefix reconciler derives
// it while building the entries rather than copying it from BGPSync's writes.
func TestPrefixWriterOwnsAdvertisementNames(t *testing.T) {
	r := &DynamicPrefixReconciler{}
	subnets, err := r.calculateSubnets("home-ipv6", mustParsePrefix(t, testStatusPrefix),
		[]dynamicprefixiov1alpha1.SubnetSpec{
			{Name: "lb", Offset: 0, PrefixLength: 64,
				BGP: &dynamicprefixiov1alpha1.SubnetBGPSpec{Advertise: true}},
			{Name: "plain", Offset: 1, PrefixLength: 64},
		})
	if err != nil {
		t.Fatalf("calculateSubnets: %v", err)
	}
	if len(subnets) != 2 {
		t.Fatalf("got %d subnets, want 2", len(subnets))
	}
	if subnets[0].BGPAdvertisement != "dp-home-ipv6-lb" {
		t.Errorf("advertised subnet carries %q, want dp-home-ipv6-lb", subnets[0].BGPAdvertisement)
	}
	if subnets[1].BGPAdvertisement != "" {
		t.Errorf("non-advertised subnet carries %q, want empty", subnets[1].BGPAdvertisement)
	}
}
