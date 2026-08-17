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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	acmetav1 "k8s.io/client-go/applyconfigurations/meta/v1"

	dynamicprefixiov1alpha1 "github.com/pkizzle/dynamic-prefix-operator/api/v1alpha1"
)

// Field manager names for the DynamicPrefix status writers. Three controllers
// write disjoint parts of one status object; giving each its own manager is
// what lets Server-Side Apply merge their writes instead of making them race
// for the whole object. The names appear in managedFields, so they are part of
// the operator's observable surface -- change them and the next apply
// force-takes the fields from the old name.
const (
	fieldOwnerDynamicPrefix = "dynamic-prefix-operator/dynamicprefix"
	fieldOwnerBGPSync       = "dynamic-prefix-operator/bgpsync"
	fieldOwnerPoolSync      = "dynamic-prefix-operator/poolsync"
)

// conditionApplyEntry builds the apply-configuration fragment for one condition,
// deciding lastTransitionTime the way meta.SetStatusCondition would: carried
// over while the status value is unchanged, now() on a transition. Apply
// requires the writer to state the whole entry, including the timestamp, so
// this has to be computed against the current object rather than left to the
// server.
//
// The second return reports whether the entry differs from what the object
// already carries. Apply has no conflict to save on, but a no-op write is still
// a write: this runs on every reconcile, and skipping unchanged entries is what
// keeps a large cluster from patching one shared object continuously.
func conditionApplyEntry(
	dp *dynamicprefixiov1alpha1.DynamicPrefix,
	condType string,
	status metav1.ConditionStatus,
	reason, message string,
) (*acmetav1.ConditionApplyConfiguration, bool) {
	transitionTime := metav1.Now()
	changed := true
	if existing := meta.FindStatusCondition(dp.Status.Conditions, condType); existing != nil {
		if existing.Status == status {
			transitionTime = existing.LastTransitionTime
		}
		changed = existing.Status != status ||
			existing.Reason != reason ||
			existing.Message != message ||
			existing.ObservedGeneration != dp.Generation
	}

	entry := acmetav1.Condition().
		WithType(condType).
		WithStatus(status).
		WithObservedGeneration(dp.Generation).
		WithLastTransitionTime(transitionTime).
		WithReason(reason).
		WithMessage(message)
	return entry, changed
}
