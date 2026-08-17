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
	"sort"
	"strings"
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	dynamicprefixiov1alpha1 "github.com/pkizzle/dynamic-prefix-operator/api/v1alpha1"
	acdynamicprefixv1alpha1 "github.com/pkizzle/dynamic-prefix-operator/api/v1alpha1/applyconfiguration/api/v1alpha1"
)

// poolStateKey identifies one backend object. A reconcile.Request carries only a
// name, and several backends are cluster-scoped, so a CiliumLoadBalancerIPPool
// and a CiliumCIDRGroup can share one. Keyed by name alone their verdicts would
// overwrite each other and the condition would report whichever synced last.
type poolStateKey struct {
	backend string
	pool    string
}

func (k poolStateKey) String() string {
	return k.backend + " " + k.pool
}

// poolSyncState remembers which pools are currently failing to sync, per
// DynamicPrefix, so the PoolsSynced condition can describe the whole set rather
// than whichever pool was reconciled last.
//
// In memory on purpose: the truth is whatever the last reconcile of each pool
// found, and every pool is reconciled again after a restart or a change of
// leader, so the condition converges without needing to be persisted. It is only
// ever an aggregate of facts that are themselves recorded on the pools.
type poolSyncState struct {
	mu sync.Mutex
	// failing maps DynamicPrefix name to the set of pools currently failing.
	failing map[string]map[poolStateKey]string
}

// record notes the outcome of one pool's sync and reports the resulting
// condition for its DynamicPrefix.
func (s *poolSyncState) record(dpName string, key poolStateKey, syncErr error) (metav1.ConditionStatus, string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.failing == nil {
		s.failing = make(map[string]map[poolStateKey]string)
	}
	pools := s.failing[dpName]
	if pools == nil {
		pools = make(map[poolStateKey]string)
		s.failing[dpName] = pools
	}

	if syncErr != nil {
		pools[key] = syncErr.Error()
	} else {
		delete(pools, key)
	}

	return conditionFor(s.failing, dpName)
}

// conditionFor renders the aggregate condition for one DynamicPrefix, dropping
// the entry when nothing is failing. Callers hold s.mu.
func conditionFor(failing map[string]map[poolStateKey]string, dpName string) (metav1.ConditionStatus, string, string) {
	pools := failing[dpName]
	if len(pools) == 0 {
		delete(failing, dpName)
		return metav1.ConditionTrue, "PoolsSynced", "All referencing pools are in sync"
	}

	names := make([]string, 0, len(pools))
	for key := range pools {
		names = append(names, key.String())
	}
	sort.Strings(names)

	// One representative reason keeps the message useful when several pools fail
	// for the same cause, which is the common case.
	var first string
	for key, msg := range pools {
		if key.String() == names[0] {
			first = msg
			break
		}
	}
	return metav1.ConditionFalse, "PoolSyncFailed", fmt.Sprintf(
		"%d pool(s) are not in sync: %s (%s)", len(names), strings.Join(names, ", "), first)
}

// forget drops a DynamicPrefix's state, so a deleted resource does not keep
// entries alive for the life of the process.
func (s *poolSyncState) forget(dpName string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.failing, dpName)
}

// forgetEntries drops every failing entry the predicate matches and names the
// DynamicPrefixes whose aggregate changed as a result.
//
// A pool that fails and is then released or deleted is never reconciled through
// the success path again, so without this its entry -- and the PoolsSynced=False
// naming it -- would outlive the operator's interest in it.
func (s *poolSyncState) forgetEntries(match func(poolStateKey) bool) []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	var affected []string
	for dpName, pools := range s.failing {
		changed := false
		for key := range pools {
			if match(key) {
				delete(pools, key)
				changed = true
			}
		}
		if changed {
			affected = append(affected, dpName)
		}
	}
	sort.Strings(affected)
	return affected
}

// condition renders the aggregate for one DynamicPrefix without recording an
// outcome, for callers that have just dropped entries.
func (s *poolSyncState) condition(dpName string) (metav1.ConditionStatus, string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return conditionFor(s.failing, dpName)
}

// updatePoolsSyncedCondition reflects one pool's sync outcome in the referenced
// DynamicPrefix's PoolsSynced condition.
//
// Writes are skipped unless the condition actually changes, because this runs on
// every pool reconcile: without that check a cluster with many pools would
// rewrite one shared status object continuously.
func (r *PoolSyncReconciler) updatePoolsSyncedCondition(ctx context.Context, dpName string, key poolStateKey, syncErr error) {
	status, reason, message := r.poolState.record(dpName, key, syncErr)
	r.writePoolsSyncedCondition(ctx, dpName, status, reason, message)
}

// releasePoolsSyncedEntries drops the state of pools the operator has stopped
// managing and rewrites the conditions that named them.
func (r *PoolSyncReconciler) releasePoolsSyncedEntries(ctx context.Context, match func(poolStateKey) bool) {
	for _, dpName := range r.poolState.forgetEntries(match) {
		status, reason, message := r.poolState.condition(dpName)
		r.writePoolsSyncedCondition(ctx, dpName, status, reason, message)
	}
}

// writePoolsSyncedCondition persists one rendered condition.
//
// Applied rather than updated, under this controller's own field manager, so
// the write owns exactly the PoolsSynced entry and cannot collide with the
// other status writers -- during a rotation every referencing pool reports here
// against the same object, and full-object updates made each report a race.
func (r *PoolSyncReconciler) writePoolsSyncedCondition(
	ctx context.Context,
	dpName string,
	status metav1.ConditionStatus,
	reason, message string,
) {
	log := logf.FromContext(ctx)

	var dp dynamicprefixiov1alpha1.DynamicPrefix
	err := r.Get(ctx, types.NamespacedName{Name: dpName}, &dp)
	if err == nil {
		entry, changed := conditionApplyEntry(&dp,
			dynamicprefixiov1alpha1.ConditionTypePoolsSynced, status, reason, message)
		if !changed {
			return
		}
		ac := acdynamicprefixv1alpha1.DynamicPrefix(dpName).
			WithStatus(acdynamicprefixv1alpha1.DynamicPrefixStatus().WithConditions(entry))
		err = r.Status().Apply(ctx, ac, client.FieldOwner(fieldOwnerPoolSync), client.ForceOwnership)
	}

	switch {
	case err == nil:
	case apierrors.IsNotFound(err):
		// The DynamicPrefix is gone; the pool release path handles the rest.
		r.poolState.forget(dpName)
	default:
		// Not fatal to the sync that just succeeded or failed on its own terms:
		// the condition is a report, and the next reconcile writes it again.
		log.V(1).Info("Could not update the PoolsSynced condition",
			"dynamicPrefix", dpName, "error", err.Error())
	}
}
