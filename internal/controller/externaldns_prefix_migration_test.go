package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// external-dns v0.22 changed its default annotation prefix and provides no
// fallback, so a cluster mid-migration has to carry both spellings at once and
// then drop the old one. These tests cover that transition, because getting it
// wrong is silent: the target simply stops being read, and under --policy=sync an
// unread target is a deleted record rather than an error.

// TestExternalDNSTargetWrittenUnderEveryConfiguredKey is the migration's steady
// state -- both spellings present and equal.
func TestExternalDNSTargetWrittenUnderEveryConfiguredKey(t *testing.T) {
	ctx := context.Background()
	const maxHistory = 2
	r, dp, key := newOwnershipTestFixture(t, maxHistory)
	r.ExternalDNSTargetAnnotationKeys = []string{AnnotationExternalDNSTarget, AnnotationExternalDNSTargetNew}

	rotatePrefix(dp, 3, maxHistory)
	if err := r.Status().Update(ctx, dp); err != nil {
		t.Fatalf("status update: %v", err)
	}
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var svc corev1.Service
	if err := r.Get(ctx, key, &svc); err != nil {
		t.Fatalf("get: %v", err)
	}
	old, newKey := svc.Annotations[AnnotationExternalDNSTarget], svc.Annotations[AnnotationExternalDNSTargetNew]
	if old == "" {
		t.Fatal("expected a target under the alpha key")
	}
	if old != newKey {
		t.Errorf("keys disagree: alpha=%q new=%q; both must hold the same target or one consumer resolves a stale address", old, newKey)
	}
}

// TestExternalDNSTargetDefaultsToBothKeys pins the default, since it is what an
// operator upgraded without any configuration change will do.
func TestExternalDNSTargetDefaultsToBothKeys(t *testing.T) {
	ctx := context.Background()
	const maxHistory = 2
	r, dp, key := newOwnershipTestFixture(t, maxHistory)
	// deliberately leave ExternalDNSTargetAnnotationKeys unset

	rotatePrefix(dp, 3, maxHistory)
	if err := r.Status().Update(ctx, dp); err != nil {
		t.Fatalf("status update: %v", err)
	}
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var svc corev1.Service
	if err := r.Get(ctx, key, &svc); err != nil {
		t.Fatalf("get: %v", err)
	}
	for _, k := range DefaultExternalDNSTargetAnnotationKeys {
		if svc.Annotations[k] == "" {
			t.Errorf("default config left %q unset; the default must be safe on every external-dns version", k)
		}
	}
}

// TestDeConfiguredExternalDNSTargetKeyIsReleased is how the migration ends:
// narrowing the configured list must clean the old key off Services, rather than
// leaving it behind frozen at whatever address it last held.
func TestDeConfiguredExternalDNSTargetKeyIsReleased(t *testing.T) {
	ctx := context.Background()
	const maxHistory = 2
	r, dp, key := newOwnershipTestFixture(t, maxHistory)
	r.ExternalDNSTargetAnnotationKeys = []string{AnnotationExternalDNSTarget, AnnotationExternalDNSTargetNew}

	rotatePrefix(dp, 3, maxHistory)
	if err := r.Status().Update(ctx, dp); err != nil {
		t.Fatalf("status update: %v", err)
	}
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// Finish the migration: only the new key from here on.
	r.ExternalDNSTargetAnnotationKeys = []string{AnnotationExternalDNSTargetNew}
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile after narrowing: %v", err)
	}

	var svc corev1.Service
	if err := r.Get(ctx, key, &svc); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got, ok := svc.Annotations[AnnotationExternalDNSTarget]; ok {
		t.Errorf("alpha key still present as %q; a de-configured key must be released, not abandoned", got)
	}
	if svc.Annotations[AnnotationExternalDNSTargetNew] == "" {
		t.Error("new key lost while releasing the old one")
	}
}

// TestReleasingADeConfiguredKeyPreservesUnownedEntries -- the operator gives back
// only what it wrote. A hostname a human put in the old key must not be swept away
// by the migration.
func TestReleasingADeConfiguredKeyPreservesUnownedEntries(t *testing.T) {
	ctx := context.Background()
	const maxHistory = 2
	r, dp, key := newOwnershipTestFixture(t, maxHistory)
	r.ExternalDNSTargetAnnotationKeys = []string{AnnotationExternalDNSTarget, AnnotationExternalDNSTargetNew}

	rotatePrefix(dp, 3, maxHistory)
	if err := r.Status().Update(ctx, dp); err != nil {
		t.Fatalf("status update: %v", err)
	}
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var svc corev1.Service
	if err := r.Get(ctx, key, &svc); err != nil {
		t.Fatalf("get: %v", err)
	}
	annotations := svc.GetAnnotations()
	annotations[AnnotationExternalDNSTarget] = "example.com," + annotations[AnnotationExternalDNSTarget]
	svc.SetAnnotations(annotations)
	if err := r.Update(ctx, &svc); err != nil {
		t.Fatalf("pin a hostname: %v", err)
	}

	r.ExternalDNSTargetAnnotationKeys = []string{AnnotationExternalDNSTargetNew}
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile after narrowing: %v", err)
	}

	if err := r.Get(ctx, key, &svc); err != nil {
		t.Fatalf("get after narrowing: %v", err)
	}
	if got := svc.Annotations[AnnotationExternalDNSTarget]; got != "example.com" {
		t.Errorf("alpha key = %q, want the user's hostname to survive the release", got)
	}
}

// TestOptOutReleasesEveryKnownExternalDNSTargetKey -- opting out has to hand back
// whatever the operator has ever written, including a spelling left over from a
// migration. Missing one leaves an address that stops resolving at the next
// rotation, with nothing pointing at the cause.
func TestOptOutReleasesEveryKnownExternalDNSTargetKey(t *testing.T) {
	ctx := context.Background()
	const maxHistory = 2
	r, dp, key := newOwnershipTestFixture(t, maxHistory)
	r.ExternalDNSTargetAnnotationKeys = []string{AnnotationExternalDNSTarget, AnnotationExternalDNSTargetNew}

	rotatePrefix(dp, 3, maxHistory)
	if err := r.Status().Update(ctx, dp); err != nil {
		t.Fatalf("status update: %v", err)
	}
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var svc corev1.Service
	if err := r.Get(ctx, key, &svc); err != nil {
		t.Fatalf("get: %v", err)
	}
	annotations := svc.GetAnnotations()
	annotations[AnnotationSkipExternalDNSUpdate] = AnnotationValueTrue
	svc.SetAnnotations(annotations)
	if err := r.Update(ctx, &svc); err != nil {
		t.Fatalf("opt out: %v", err)
	}

	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile after opt-out: %v", err)
	}

	if err := r.Get(ctx, key, &svc); err != nil {
		t.Fatalf("get after opt-out: %v", err)
	}
	for _, k := range knownExternalDNSTargetAnnotationKeys {
		if got, ok := svc.Annotations[k]; ok {
			t.Errorf("%s still present as %q after opting out", k, got)
		}
	}
}
