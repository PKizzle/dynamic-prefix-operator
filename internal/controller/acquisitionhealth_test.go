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
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	dynamicprefixiov1alpha1 "github.com/pkizzle/dynamic-prefix-operator/api/v1alpha1"
	"github.com/pkizzle/dynamic-prefix-operator/internal/prefix"
)

// ailingReceiver holds no prefix and knows why.
type ailingReceiver struct {
	*prefix.MockReceiver
	err error
}

func (r *ailingReceiver) LastError() error { return r.err }

type fixedReceiverFactory struct{ receiver prefix.Receiver }

func (f *fixedReceiverFactory) CreateReceiver(dynamicprefixiov1alpha1.AcquisitionSpec) (prefix.Receiver, error) {
	return f.receiver, nil
}

func newHealthTestReconciler(t *testing.T, receiver prefix.Receiver) (*DynamicPrefixReconciler, *events.FakeRecorder, *dynamicprefixiov1alpha1.DynamicPrefix) {
	t.Helper()

	scheme := newPoolBackendTestScheme(t)
	dp := &dynamicprefixiov1alpha1.DynamicPrefix{
		ObjectMeta: metav1.ObjectMeta{Name: "home-ipv6", Finalizers: []string{finalizerName}},
		Spec: dynamicprefixiov1alpha1.DynamicPrefixSpec{
			Acquisition: dynamicprefixiov1alpha1.AcquisitionSpec{
				DHCPv6PD: &dynamicprefixiov1alpha1.DHCPv6PDSpec{Interface: "eth0"},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(dp).
		WithStatusSubresource(dp).
		WithTypeConverters(testTypeConverters(scheme)...).
		Build()
	recorder := events.NewFakeRecorder(8)

	r := &DynamicPrefixReconciler{
		Client:          fakeClient,
		Scheme:          scheme,
		Recorder:        recorder,
		ReceiverFactory: &fixedReceiverFactory{receiver: receiver},
	}
	t.Cleanup(func() { forgetPrefixMetrics(dp.Name) })
	return r, recorder, dp
}

func reconcileDynamicPrefix(t *testing.T, r *DynamicPrefixReconciler, name string) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: name},
	}); err != nil {
		t.Fatalf("Reconcile() = %v", err)
	}
}

func prefixAcquiredCondition(t *testing.T, r *DynamicPrefixReconciler, name string) *metav1.Condition {
	t.Helper()

	var dp dynamicprefixiov1alpha1.DynamicPrefix
	if err := r.Get(context.Background(), types.NamespacedName{Name: name}, &dp); err != nil {
		t.Fatalf("reading back the DynamicPrefix: %v", err)
	}
	return meta.FindStatusCondition(dp.Status.Conditions, dynamicprefixiov1alpha1.ConditionTypePrefixAcquired)
}

// A receiver that cannot acquire -- no such interface, nothing answering, no
// permission to bind its port -- left the resource reporting that it was
// waiting for an advertisement, forever, with the reason only in the operator's
// log. Failure events are deliberately not forwarded to reconcile, so the
// reason has to be pulled from the receiver instead.
func TestReconcileReportsWhyNoPrefixWasAcquired(t *testing.T) {
	receiver := &ailingReceiver{
		MockReceiver: prefix.NewMockReceiver(prefix.SourceDHCPv6PD),
		err:          errors.New("failed to create DHCPv6 client: listen udp6 [fe80::1%eth0]:546: bind: permission denied"),
	}
	r, recorder, dp := newHealthTestReconciler(t, receiver)

	reconcileDynamicPrefix(t, r, dp.Name)

	cond := prefixAcquiredCondition(t, r, dp.Name)
	if cond == nil {
		t.Fatal("no PrefixAcquired condition was written")
	}
	if cond.Status != metav1.ConditionFalse {
		t.Fatalf("PrefixAcquired = %v, want False", cond.Status)
	}
	if cond.Reason != reasonAcquisitionFailed {
		t.Errorf("reason = %q, want %q", cond.Reason, reasonAcquisitionFailed)
	}
	if !strings.Contains(cond.Message, "permission denied") {
		t.Errorf("message = %q, want it to carry the receiver's own error", cond.Message)
	}

	if got := testutil.ToFloat64(receiverHealthy.WithLabelValues(dp.Name)); got != 0 {
		t.Errorf("dynamic_prefix_receiver_healthy = %v, want 0 while acquisition is failing", got)
	}

	if !drainForWarning(recorder, "permission denied") {
		t.Error("no Warning event named the acquisition failure")
	}
}

// The failure repeats every second while the interface is down. Reconcile runs
// on its own schedule on top of that, so the event has to be tied to the
// message changing rather than to either rate.
func TestRepeatedAcquisitionFailureIsReportedOnce(t *testing.T) {
	receiver := &ailingReceiver{
		MockReceiver: prefix.NewMockReceiver(prefix.SourceDHCPv6PD),
		err:          errors.New("no server answered"),
	}
	r, recorder, dp := newHealthTestReconciler(t, receiver)

	for range 3 {
		reconcileDynamicPrefix(t, r, dp.Name)
	}

	warnings := 0
	for {
		select {
		case ev := <-recorder.Events:
			if strings.Contains(ev, corev1.EventTypeWarning) && strings.Contains(ev, "no server answered") {
				warnings++
			}
			continue
		default:
		}
		break
	}
	if warnings != 1 {
		t.Errorf("the same failure raised %d Warnings across three reconciles, want 1", warnings)
	}
}

// Waiting for a first advertisement is not a failure, and must not be dressed
// up as one.
func TestReconcileStillDistinguishesWaitingFromFailing(t *testing.T) {
	receiver := &ailingReceiver{MockReceiver: prefix.NewMockReceiver(prefix.SourceRouterAdvertisement)}
	r, recorder, dp := newHealthTestReconciler(t, receiver)

	reconcileDynamicPrefix(t, r, dp.Name)

	cond := prefixAcquiredCondition(t, r, dp.Name)
	if cond == nil {
		t.Fatal("no PrefixAcquired condition was written")
	}
	if cond.Reason != "WaitingForPrefix" {
		t.Errorf("reason = %q, want WaitingForPrefix", cond.Reason)
	}
	if drainForWarning(recorder, "") {
		t.Error("waiting for the first advertisement raised a Warning")
	}
}

// A held prefix does not mean a healthy receiver: the lease behind it may be
// one no renewal has extended for hours.
func TestFailingRenewalDegradesAResourceThatStillHoldsAPrefix(t *testing.T) {
	mock := prefix.NewMockReceiver(prefix.SourceDHCPv6PD)
	mock.SimulatePrefix(netip.MustParsePrefix("2001:db8::/56"), time.Hour)
	receiver := &ailingReceiver{MockReceiver: mock, err: errors.New("prefix renewal failed: timed out")}
	r, _, dp := newHealthTestReconciler(t, receiver)

	reconcileDynamicPrefix(t, r, dp.Name)

	var got dynamicprefixiov1alpha1.DynamicPrefix
	if err := r.Get(context.Background(), types.NamespacedName{Name: dp.Name}, &got); err != nil {
		t.Fatalf("reading back the DynamicPrefix: %v", err)
	}

	if acquired := meta.FindStatusCondition(got.Status.Conditions, dynamicprefixiov1alpha1.ConditionTypePrefixAcquired); acquired == nil || acquired.Status != metav1.ConditionTrue {
		t.Errorf("PrefixAcquired = %v, want True: the prefix is still held", acquired)
	}
	degraded := meta.FindStatusCondition(got.Status.Conditions, dynamicprefixiov1alpha1.ConditionTypeDegraded)
	if degraded == nil || degraded.Status != metav1.ConditionTrue {
		t.Fatalf("Degraded = %v, want True while renewals are failing", degraded)
	}
	if !strings.Contains(degraded.Message, "timed out") {
		t.Errorf("Degraded message = %q, want it to carry the renewal error", degraded.Message)
	}
	if value := testutil.ToFloat64(receiverHealthy.WithLabelValues(dp.Name)); value != 0 {
		t.Errorf("dynamic_prefix_receiver_healthy = %v, want 0 while renewals fail", value)
	}
}

func TestHealthyReceiverReportsHealthy(t *testing.T) {
	mock := prefix.NewMockReceiver(prefix.SourceDHCPv6PD)
	mock.SimulatePrefix(netip.MustParsePrefix("2001:db8::/56"), time.Hour)
	r, _, dp := newHealthTestReconciler(t, &ailingReceiver{MockReceiver: mock})

	reconcileDynamicPrefix(t, r, dp.Name)

	if value := testutil.ToFloat64(receiverHealthy.WithLabelValues(dp.Name)); value != 1 {
		t.Errorf("dynamic_prefix_receiver_healthy = %v, want 1", value)
	}
}

func drainForWarning(recorder *events.FakeRecorder, containing string) bool {
	found := false
	for {
		select {
		case ev := <-recorder.Events:
			if strings.Contains(ev, corev1.EventTypeWarning) && strings.Contains(ev, containing) {
				found = true
			}
			continue
		default:
		}
		return found
	}
}
