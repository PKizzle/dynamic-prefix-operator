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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

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
