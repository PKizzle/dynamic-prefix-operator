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
	"strings"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// recordFor reads an ownership record annotation, returning its value and
// whether it was present at all -- the distinction that decides between the
// record and the legacy geometric test.
func recordFor(pool *unstructured.Unstructured, key string) (string, bool) {
	value, ok := pool.GetAnnotations()[key]
	return value, ok
}

// ownedEntry is one entry the operator wants to write, paired with the key it is
// recognised by afterwards. An entry with no key is never written: it could not
// be recognised as the operator's on a later pass, so it would be preserved as a
// user's entry and a fresh copy appended on every reconcile -- exactly the
// unbounded growth the ownership record exists to stop.
type ownedEntry struct {
	value interface{}
	key   string
}

// ownedListSync describes one backend's shared-list field to syncOwnedList.
//
// Three backends write a list they share with the user -- Cilium's spec.blocks
// and spec.externalCIDRs, MetalLB's spec.addresses -- and all three follow the
// same procedure. They used to implement it three times, about eighty lines
// each, and the copies drifted: MetalLB spent a full release deciding ownership
// geometrically after the others had moved to records, and leaked one address
// per rotation for it. The procedure lives here once so a fix cannot land in two
// places out of three.
type ownedListSync struct {
	// fields is the path to the list, e.g. "spec", "blocks".
	fields []string
	// recordKey is the annotation recording what the operator wrote last.
	recordKey string
	// desired is what this pass wants to write, in order.
	desired []ownedEntry
	// keyOf identifies an entry already present in the field.
	keyOf func(existing interface{}) string
	// owned reports whether an entry already present is the operator's. It is
	// given the record, which is authoritative when present; implementations
	// fall back to the geometric test only for objects written before records
	// existed.
	owned func(existing interface{}) bool
}

// syncOwnedList reconciles one shared list field, reporting whether the object
// was written.
//
// The invariants it maintains, each of which was a bug at some point:
//
//   - Entries the operator did not write are preserved, whatever they look like.
//   - An entry the user already pinned stays theirs even where it coincides with
//     one this pass would write, and is left out of the record. Claiming it would
//     grant the operator the right to delete it once that prefix ages out,
//     turning a deliberate pin into a delayed failure.
//   - The record is written even when the list is byte-identical, because the
//     first pass after an upgrade usually produces the same list, and without
//     persisting the record the next rotation falls back to the geometric test.
func syncOwnedList(
	ctx context.Context,
	r *PoolSyncReconciler,
	pool *unstructured.Unstructured,
	spec ownedListSync,
) (bool, error) {
	logger := log.FromContext(ctx)
	path := strings.Join(spec.fields, ".")

	existing, _, err := unstructured.NestedSlice(pool.Object, spec.fields...)
	if err != nil {
		return false, fmt.Errorf("failed to read %s: %w", path, err)
	}

	preserved := make([]interface{}, 0, len(existing))
	pinned := make(map[string]struct{}, len(existing))
	for _, item := range existing {
		if spec.owned(item) {
			continue
		}
		preserved = append(preserved, item)
		if key := spec.keyOf(item); key != "" {
			pinned[key] = struct{}{}
		}
	}
	if len(preserved) > 0 {
		logger.V(1).Info("Preserving unmanaged entries", "field", path,
			"pool", pool.GetName(), "count", len(preserved))
	}

	entries := make([]interface{}, 0, len(preserved)+len(spec.desired))
	entries = append(entries, preserved...)
	managed := make([]string, 0, len(spec.desired))
	seen := make(map[string]struct{}, len(spec.desired))
	for _, entry := range spec.desired {
		if entry.key == "" {
			logger.Error(nil, "Skipping entry with no usable identity",
				"field", path, "pool", pool.GetName(), "entry", entry.value)
			continue
		}
		if _, dup := seen[entry.key]; dup {
			continue
		}
		seen[entry.key] = struct{}{}
		if _, isPinned := pinned[entry.key]; isPinned {
			continue
		}
		entries = append(entries, entry.value)
		managed = append(managed, entry.key)
	}

	recordValue, recordExists := pool.GetAnnotations()[spec.recordKey]
	managedRecord := formatOwnershipRecord(managed)
	entriesChanged := !equality.Semantic.DeepEqual(existing, entries)
	recordChanged := recordValue != managedRecord || !recordExists

	if !entriesChanged && !recordChanged {
		logger.V(2).Info("Entries unchanged, skipping update", "field", path, "pool", pool.GetName())
		return false, nil
	}

	if entriesChanged {
		if err := unstructured.SetNestedField(pool.Object, entries, spec.fields...); err != nil {
			return false, fmt.Errorf("failed to set %s: %w", path, err)
		}
	}

	setPoolAnnotation(pool, spec.recordKey, managedRecord)
	r.setLastSyncAnnotation(pool)

	return true, r.Update(ctx, pool)
}
