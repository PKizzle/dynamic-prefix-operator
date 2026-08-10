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
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"

	dynamicprefixiov1alpha1 "github.com/pkizzle/dynamic-prefix-operator/api/v1alpha1"
	"github.com/pkizzle/dynamic-prefix-operator/internal/prefix"
)

// countingReceiverFactory records the acquisition specs it was asked to build,
// so a test can tell a rebuilt receiver from a reused one.
type countingReceiverFactory struct {
	specs []dynamicprefixiov1alpha1.AcquisitionSpec
}

func (f *countingReceiverFactory) CreateReceiver(spec dynamicprefixiov1alpha1.AcquisitionSpec) (prefix.Receiver, error) {
	f.specs = append(f.specs, spec)
	return prefix.NewMockReceiver(prefix.SourceDHCPv6PD), nil
}

func raAcquisition(iface string) dynamicprefixiov1alpha1.AcquisitionSpec {
	return dynamicprefixiov1alpha1.AcquisitionSpec{
		RouterAdvertisement: &dynamicprefixiov1alpha1.RouterAdvertisementSpec{
			Interface: iface,
			Enabled:   true,
		},
	}
}

// TestReceiverRebuiltWhenAcquisitionChanges covers editing spec.acquisition on a
// live DynamicPrefix. Everything a receiver is configured with -- its interface,
// its source, its acceptance policy -- is fixed when it is constructed, and
// receivers were cached by name alone. An edit was therefore accepted by the API
// server and then ignored: the operator kept listening on the old interface, kept
// reporting the old interface's prefix, and only a pod restart would notice.
func TestReceiverRebuiltWhenAcquisitionChanges(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = dynamicprefixiov1alpha1.AddToScheme(scheme)

	dp := &dynamicprefixiov1alpha1.DynamicPrefix{
		ObjectMeta: metav1.ObjectMeta{Name: "home-ipv6"},
		Spec:       dynamicprefixiov1alpha1.DynamicPrefixSpec{Acquisition: raAcquisition("eth0")},
	}

	factory := &countingReceiverFactory{}
	r := NewDynamicPrefixReconciler(
		fake.NewClientBuilder().WithScheme(scheme).WithObjects(dp).Build(), scheme)
	r.ReceiverFactory = factory

	first, err := r.getOrCreateReceiver(ctx, dp)
	if err != nil {
		t.Fatalf("first getOrCreateReceiver: %v", err)
	}

	// Same spec: the receiver must be reused, or every reconcile would restart
	// acquisition and a DHCPv6-PD lease would be re-solicited constantly.
	again, err := r.getOrCreateReceiver(ctx, dp)
	if err != nil {
		t.Fatalf("second getOrCreateReceiver: %v", err)
	}
	if again != first {
		t.Error("an unchanged acquisition spec rebuilt the receiver")
	}
	if len(factory.specs) != 1 {
		t.Errorf("factory called %d times for an unchanged spec, want 1", len(factory.specs))
	}

	// The user moves the operator to another interface.
	dp.Spec.Acquisition = raAcquisition("eth1")
	rebuilt, err := r.getOrCreateReceiver(ctx, dp)
	if err != nil {
		t.Fatalf("getOrCreateReceiver after change: %v", err)
	}
	if rebuilt == first {
		t.Error("the receiver was not rebuilt after the acquisition spec changed")
	}
	if len(factory.specs) != 2 {
		t.Fatalf("factory called %d times after the spec changed, want 2", len(factory.specs))
	}
	if got := factory.specs[1].RouterAdvertisement.Interface; got != "eth1" {
		t.Errorf("the rebuilt receiver was created for interface %q, want eth1", got)
	}

	// Switching acquisition method entirely is the same story.
	dp.Spec.Acquisition = dynamicprefixiov1alpha1.AcquisitionSpec{
		DHCPv6PD: &dynamicprefixiov1alpha1.DHCPv6PDSpec{Interface: "eth1"},
	}
	if _, err := r.getOrCreateReceiver(ctx, dp); err != nil {
		t.Fatalf("getOrCreateReceiver after switching source: %v", err)
	}
	if len(factory.specs) != 3 {
		t.Errorf("factory called %d times after switching acquisition method, want 3", len(factory.specs))
	}
}

// TestPrefixFlapDoesNotDuplicateHistory covers a prefix that comes back:
// A -> B -> A. The outgoing prefix was appended to history unconditionally, so
// after the second change A was both the current prefix and a history entry.
// Pool builders read the two as distinct sets, and Calico turns the duplicate
// into a sibling IPPool carrying its parent's CIDR -- which Calico rejects as an
// overlap, so the sync fails and keeps failing.
func TestPrefixFlapDoesNotDuplicateHistory(t *testing.T) {
	ctx := context.Background()
	r := &DynamicPrefixReconciler{}

	dp := &dynamicprefixiov1alpha1.DynamicPrefix{
		ObjectMeta: metav1.ObjectMeta{Name: "home-ipv6"},
		Spec: dynamicprefixiov1alpha1.DynamicPrefixSpec{
			Transition: &dynamicprefixiov1alpha1.TransitionSpec{MaxPrefixHistory: 2},
		},
	}

	rotate := func(to string) {
		r.handlePrefixChange(ctx, dp, &prefix.Prefix{Network: netip.MustParsePrefix(to)})
		dp.Status.CurrentPrefix = to
	}

	dp.Status.CurrentPrefix = "2001:db8:a::/64"
	rotate("2001:db8:b::/64")
	rotate("2001:db8:a::/64") // the ISP hands the original prefix back

	for _, entry := range dp.Status.History {
		if entry.Prefix == dp.Status.CurrentPrefix {
			t.Errorf("history still contains the current prefix %s after a flap: %+v",
				dp.Status.CurrentPrefix, dp.Status.History)
		}
	}

	seen := make(map[string]int, len(dp.Status.History))
	for _, entry := range dp.Status.History {
		seen[entry.Prefix]++
		if seen[entry.Prefix] > 1 {
			t.Errorf("prefix %s appears %d times in history: %+v",
				entry.Prefix, seen[entry.Prefix], dp.Status.History)
		}
	}
}

// TestPrefixEventsWakeTheReconciler covers the push path. Every receiver
// populated an events channel that nothing in the controller package ever read,
// so a rotation reached status only via the periodic requeue -- capped at five
// minutes -- and each receiver's channel filled up and then dropped events
// permanently.
func TestPrefixEventsWakeTheReconciler(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	r := &DynamicPrefixReconciler{prefixEvents: make(chan event.GenericEvent, prefixEventQueueSize)}
	events := make(chan prefix.Event, 4)
	go r.forwardPrefixEvents(ctx, "home-ipv6", events)

	waitForWake := func(t *testing.T, what string) {
		t.Helper()
		select {
		case got := <-r.prefixEvents:
			if got.Object.GetName() != "home-ipv6" {
				t.Errorf("%s woke the reconciler for %q, want home-ipv6", what, got.Object.GetName())
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s did not wake the reconciler", what)
		}
	}

	for _, tc := range []struct {
		eventType prefix.EventType
		what      string
	}{
		{prefix.EventTypeAcquired, "acquiring a prefix"},
		{prefix.EventTypeChanged, "a prefix change"},
		{prefix.EventTypeExpired, "a lease expiring"},
		{prefix.EventTypeFailed, "an acquisition failure"},
	} {
		events <- prefix.Event{Type: tc.eventType}
		waitForWake(t, tc.what)
	}

	// A renewal changes only the lease expiry, which the periodic requeue
	// already refreshes; forwarding it would reconcile on every DHCPv6 renewal
	// for no change in state.
	events <- prefix.Event{Type: prefix.EventTypeRenewed}
	events <- prefix.Event{Type: prefix.EventTypeChanged}
	waitForWake(t, "a change following a renewal")
	select {
	case extra := <-r.prefixEvents:
		t.Errorf("a renewal also woke the reconciler (got %v)", extra.Object.GetName())
	default:
	}
}

// TestPrefixEventForwarderStopsWithItsReceiver covers the goroutine's lifetime: a
// rebuilt or deleted receiver must not leave a forwarder reading a channel
// nobody writes to any more.
func TestPrefixEventForwarderStopsWithItsReceiver(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := &DynamicPrefixReconciler{prefixEvents: make(chan event.GenericEvent, 1)}
	events := make(chan prefix.Event)

	done := make(chan struct{})
	go func() {
		r.forwardPrefixEvents(ctx, "home-ipv6", events)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the forwarder outlived its context")
	}
}

// TestReceiverWithoutRecordedSpecIsAdopted covers a receiver placed in the map by
// something other than getOrCreateReceiver. Its provenance is unknown, and
// discarding a live receiver -- along with the prefix it has already acquired --
// on no evidence is the wrong direction; it is adopted, and compared from then on.
func TestReceiverWithoutRecordedSpecIsAdopted(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = dynamicprefixiov1alpha1.AddToScheme(scheme)

	dp := &dynamicprefixiov1alpha1.DynamicPrefix{
		ObjectMeta: metav1.ObjectMeta{Name: "home-ipv6"},
		Spec:       dynamicprefixiov1alpha1.DynamicPrefixSpec{Acquisition: raAcquisition("eth0")},
	}

	factory := &countingReceiverFactory{}
	// Struct-literal construction, with no maps allocated: the nil-map writes
	// this used to reach are a real crash, not a hypothetical one.
	r := &DynamicPrefixReconciler{
		Client:          fake.NewClientBuilder().WithScheme(scheme).WithObjects(dp).Build(),
		Scheme:          scheme,
		ReceiverFactory: factory,
		receivers:       map[string]prefix.Receiver{"home-ipv6": prefix.NewMockReceiver(prefix.SourceRouterAdvertisement)},
	}
	injected := r.receivers["home-ipv6"]

	adopted, err := r.getOrCreateReceiver(ctx, dp)
	if err != nil {
		t.Fatalf("getOrCreateReceiver: %v", err)
	}
	if adopted != injected {
		t.Fatal("an unrecorded receiver was discarded instead of adopted")
	}
	if len(factory.specs) != 0 {
		t.Errorf("factory was called %d times while adopting, want 0", len(factory.specs))
	}

	// Having adopted it, a later edit must still be noticed.
	dp.Spec.Acquisition = raAcquisition("eth1")
	if _, err := r.getOrCreateReceiver(ctx, dp); err != nil {
		t.Fatalf("getOrCreateReceiver after change: %v", err)
	}
	if len(factory.specs) != 1 {
		t.Errorf("factory called %d times after an edit to an adopted receiver, want 1", len(factory.specs))
	}
}
