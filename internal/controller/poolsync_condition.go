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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	dynamicprefixiov1alpha1 "github.com/pkizzle/dynamic-prefix-operator/api/v1alpha1"
)

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
	// failing maps DynamicPrefix name to the set of pool keys currently failing.
	failing map[string]map[string]string
}

// record notes the outcome of one pool's sync and reports the resulting
// condition for its DynamicPrefix.
func (s *poolSyncState) record(dpName, poolKey string, syncErr error) (metav1.ConditionStatus, string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.failing == nil {
		s.failing = make(map[string]map[string]string)
	}
	pools := s.failing[dpName]
	if pools == nil {
		pools = make(map[string]string)
		s.failing[dpName] = pools
	}

	if syncErr != nil {
		pools[poolKey] = syncErr.Error()
	} else {
		delete(pools, poolKey)
	}

	if len(pools) == 0 {
		delete(s.failing, dpName)
		return metav1.ConditionTrue, "PoolsSynced", "All referencing pools are in sync"
	}

	names := make([]string, 0, len(pools))
	for name := range pools {
		names = append(names, name)
	}
	sort.Strings(names)

	// One representative reason keeps the message useful when several pools fail
	// for the same cause, which is the common case.
	return metav1.ConditionFalse, "PoolSyncFailed", fmt.Sprintf(
		"%d pool(s) are not in sync: %s (%s)", len(names), strings.Join(names, ", "), pools[names[0]])
}

// forget drops a DynamicPrefix's state, so a deleted resource does not keep
// entries alive for the life of the process.
func (s *poolSyncState) forget(dpName string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.failing, dpName)
}

// updatePoolsSyncedCondition reflects one pool's sync outcome in the referenced
// DynamicPrefix's PoolsSynced condition.
//
// The condition type has existed since the first release and was never set by
// anything, so `kubectl wait --for=condition=PoolsSynced` waited forever and the
// only sign a pool had failed was a log line and a Prometheus gauge.
//
// Writes are skipped unless the condition actually changes, because this runs on
// every pool reconcile: without that check a cluster with many pools would
// rewrite one shared status object continuously.
func (r *PoolSyncReconciler) updatePoolsSyncedCondition(ctx context.Context, dpName, poolKey string, syncErr error) {
	log := logf.FromContext(ctx)
	status, reason, message := r.poolState.record(dpName, poolKey, syncErr)

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var dp dynamicprefixiov1alpha1.DynamicPrefix
		if err := r.Get(ctx, types.NamespacedName{Name: dpName}, &dp); err != nil {
			return err
		}

		existing := meta.FindStatusCondition(dp.Status.Conditions, dynamicprefixiov1alpha1.ConditionTypePoolsSynced)
		if existing != nil && existing.Status == status && existing.Reason == reason &&
			existing.Message == message && existing.ObservedGeneration == dp.Generation {
			return nil
		}

		meta.SetStatusCondition(&dp.Status.Conditions, metav1.Condition{
			Type:               dynamicprefixiov1alpha1.ConditionTypePoolsSynced,
			Status:             status,
			ObservedGeneration: dp.Generation,
			Reason:             reason,
			Message:            message,
		})
		return r.Status().Update(ctx, &dp)
	})

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
