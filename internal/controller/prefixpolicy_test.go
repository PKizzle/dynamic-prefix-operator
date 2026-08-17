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
	"net/netip"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	dynamicprefixiov1alpha1 "github.com/pkizzle/dynamic-prefix-operator/api/v1alpha1"
	"github.com/pkizzle/dynamic-prefix-operator/internal/prefix"
)

// The receivers apply the configured bounds themselves, but a receiver is built
// once and shared per interface, so one built under an earlier configuration
// can still be feeding prefixes acquired under the old rules. The controller's
// choke point is what every source passes through, and it has to apply the same
// rules the receivers do.
func TestPrefixOutsideTheConfiguredLengthBoundsIsRejected(t *testing.T) {
	mock := prefix.NewMockReceiver(prefix.SourceDHCPv6PD)
	mock.SimulatePrefix(netip.MustParsePrefix("2001:db8::/56"), time.Hour)

	r, _, dp := newHealthTestReconciler(t, mock)

	var live dynamicprefixiov1alpha1.DynamicPrefix
	if err := r.Get(context.Background(), types.NamespacedName{Name: dp.Name}, &live); err != nil {
		t.Fatalf("reading the DynamicPrefix: %v", err)
	}
	live.Spec.Acquisition.PrefixFilter = &dynamicprefixiov1alpha1.PrefixFilterSpec{
		MinPrefixLength: ptr.To(60),
	}
	if err := r.Update(context.Background(), &live); err != nil {
		t.Fatalf("setting the bounds: %v", err)
	}

	reconcileDynamicPrefix(t, r, dp.Name)

	cond := prefixAcquiredCondition(t, r, dp.Name)
	if cond == nil {
		t.Fatal("no PrefixAcquired condition was written")
	}
	if cond.Reason != reasonPrefixRejected {
		t.Fatalf("reason = %q, want %q", cond.Reason, reasonPrefixRejected)
	}
	if !strings.Contains(cond.Message, "minimum") {
		t.Errorf("message = %q, want it to explain which bound was missed", cond.Message)
	}

	// Rejection is deliberately non-destructive: nothing derived from a prefix
	// the operator will not accept may reach status.
	var got dynamicprefixiov1alpha1.DynamicPrefix
	if err := r.Get(context.Background(), types.NamespacedName{Name: dp.Name}, &got); err != nil {
		t.Fatalf("reading the DynamicPrefix back: %v", err)
	}
	if got.Status.CurrentPrefix != "" {
		t.Errorf("status.currentPrefix = %q, want the rejected prefix not to have been recorded", got.Status.CurrentPrefix)
	}
}

func TestPrefixInsideTheConfiguredLengthBoundsIsAccepted(t *testing.T) {
	mock := prefix.NewMockReceiver(prefix.SourceDHCPv6PD)
	mock.SimulatePrefix(netip.MustParsePrefix("2001:db8::/56"), time.Hour)

	r, _, dp := newHealthTestReconciler(t, mock)

	var live dynamicprefixiov1alpha1.DynamicPrefix
	if err := r.Get(context.Background(), types.NamespacedName{Name: dp.Name}, &live); err != nil {
		t.Fatalf("reading the DynamicPrefix: %v", err)
	}
	live.Spec.Acquisition.PrefixFilter = &dynamicprefixiov1alpha1.PrefixFilterSpec{
		MinPrefixLength: ptr.To(48),
		MaxPrefixLength: ptr.To(64),
	}
	if err := r.Update(context.Background(), &live); err != nil {
		t.Fatalf("setting the bounds: %v", err)
	}

	reconcileDynamicPrefix(t, r, dp.Name)

	var got dynamicprefixiov1alpha1.DynamicPrefix
	if err := r.Get(context.Background(), types.NamespacedName{Name: dp.Name}, &got); err != nil {
		t.Fatalf("reading the DynamicPrefix back: %v", err)
	}
	if got.Status.CurrentPrefix != "2001:db8::/56" {
		t.Errorf("status.currentPrefix = %q, want the accepted prefix", got.Status.CurrentPrefix)
	}
}
