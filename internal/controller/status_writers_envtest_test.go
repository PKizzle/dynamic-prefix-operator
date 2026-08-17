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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	dynamicprefixiov1alpha1 "github.com/pkizzle/dynamic-prefix-operator/api/v1alpha1"
)

// The multi-writer contract against a real API server: each status writer
// applies under its own field manager, the server merges conditions per type,
// and managedFields records the three owners. The fake-client twin of this test
// lives in status_writers_test.go; this one exists because the fake's merge
// semantics come from a type converter the tests supply, and only a real
// apiserver proves the CRD schema itself carries the right listType markers.
var _ = Describe("DynamicPrefix status writers", func() {
	const dpName = "test-status-writers"
	ctx := context.Background()

	BeforeEach(func() {
		dp := &dynamicprefixiov1alpha1.DynamicPrefix{
			ObjectMeta: metav1.ObjectMeta{Name: dpName},
			Spec: dynamicprefixiov1alpha1.DynamicPrefixSpec{
				Acquisition: dynamicprefixiov1alpha1.AcquisitionSpec{
					RouterAdvertisement: &dynamicprefixiov1alpha1.RouterAdvertisementSpec{
						Interface: "eth0",
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, dp)).To(Succeed())
	})

	AfterEach(func() {
		dp := &dynamicprefixiov1alpha1.DynamicPrefix{}
		dp.Name = dpName
		_ = k8sClient.Delete(ctx, dp)
	})

	It("merges the three writers' fields under their own managers", func() {
		get := func() *dynamicprefixiov1alpha1.DynamicPrefix {
			var out dynamicprefixiov1alpha1.DynamicPrefix
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: dpName}, &out)).To(Succeed())
			return &out
		}

		// The prefix reconciler applies everything it owns.
		dpr := &DynamicPrefixReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		current := get()
		current.Status.CurrentPrefix = testStatusPrefix
		current.Status.PrefixSource = dynamicprefixiov1alpha1.PrefixSourceRouterAdvertisement
		dpr.setCondition(current, dynamicprefixiov1alpha1.ConditionTypePrefixAcquired,
			metav1.ConditionTrue, "PrefixAcquired", "Prefix acquired")
		Expect(dpr.updateStatusIfChanged(ctx, current, &dynamicprefixiov1alpha1.DynamicPrefixStatus{})).To(Succeed())

		// PoolSync and BGPSync report their conditions.
		psr := &PoolSyncReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		psr.updatePoolsSyncedCondition(ctx, dpName,
			poolStateKey{backend: "cilium-load-balancer-ip-pool", pool: "/lb-pool"},
			errors.New("webhook rejected the update"))
		bgp := &BGPSyncReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		after := get()
		Expect(bgp.updateStatus(ctx, after, nil)).To(Succeed())

		// Every writer's fields coexist on the server's copy.
		final := get()
		Expect(final.Status.CurrentPrefix).To(Equal(testStatusPrefix))
		for _, condType := range []string{
			dynamicprefixiov1alpha1.ConditionTypePrefixAcquired,
			dynamicprefixiov1alpha1.ConditionTypePoolsSynced,
			dynamicprefixiov1alpha1.ConditionTypeBGPAdvertisementReady,
		} {
			Expect(meta.FindStatusCondition(final.Status.Conditions, condType)).NotTo(BeNil(),
				"condition %s missing after the three writers ran", condType)
		}

		// managedFields names the three managers, each on the status subresource.
		owners := map[string]bool{}
		for _, mf := range final.ManagedFields {
			if mf.Subresource == "status" {
				owners[mf.Manager] = true
			}
		}
		for _, owner := range []string{fieldOwnerDynamicPrefix, fieldOwnerPoolSync, fieldOwnerBGPSync} {
			Expect(owners).To(HaveKey(owner))
		}
	})
})
