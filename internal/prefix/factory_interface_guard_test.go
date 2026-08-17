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

package prefix

import (
	"errors"
	"strings"
	"testing"

	"k8s.io/utils/ptr"

	dynamicprefixiov1alpha1 "github.com/pkizzle/dynamic-prefix-operator/api/v1alpha1"
)

func pdSpec(iface string) dynamicprefixiov1alpha1.AcquisitionSpec {
	return dynamicprefixiov1alpha1.AcquisitionSpec{
		DHCPv6PD: &dynamicprefixiov1alpha1.DHCPv6PDSpec{Interface: iface},
	}
}

// Two DHCPv6-PD clients on one interface present the same DUID and the same
// interface-derived IAID, so the server sees a single client contradicting
// itself: each REQUEST and RENEW rewrites the other's binding, and the two
// resources take turns holding a lease that keeps being reassigned. Nothing
// about that is visible from either resource -- both look like they are simply
// failing to renew -- which is why it is refused at construction instead.
func TestFactoryRefusesASecondDHCPv6PDReceiverOnOneInterface(t *testing.T) {
	factory := NewReceiverFactory()

	first, err := factory.CreateReceiver(pdSpec("eth0"))
	if err != nil {
		t.Fatalf("first receiver: %v", err)
	}

	_, err = factory.CreateReceiver(pdSpec("eth0"))
	if err == nil {
		t.Fatal("expected the second receiver on eth0 to be refused")
	}
	var busy *InterfaceBusyError
	if !errors.As(err, &busy) {
		t.Fatalf("error = %v (%T), want an *InterfaceBusyError", err, err)
	}
	if busy.Interface != "eth0" {
		t.Errorf("busy interface = %q, want eth0", busy.Interface)
	}
	// The message is what a user sees on the resource, so it has to say what to
	// do rather than only what went wrong.
	if !strings.Contains(err.Error(), "DynamicPrefix") {
		t.Errorf("error = %q, want it to point at the one-resource-per-delegation model", err)
	}

	// A different interface is a different delegation and must still work.
	if _, err := factory.CreateReceiver(pdSpec("eth1")); err != nil {
		t.Fatalf("receiver on a second interface: %v", err)
	}

	// Releasing lets the interface be claimed again, which is what a spec edit
	// does: the old receiver is stopped before the new one is built.
	if err := first.Stop(); err != nil {
		t.Fatalf("Stop() = %v", err)
	}
	if _, err := factory.CreateReceiver(pdSpec("eth0")); err != nil {
		t.Fatalf("receiver after the first was stopped: %v", err)
	}
}

// A composite runs a DHCPv6-PD client too, so it claims the interface on the
// same terms.
func TestFactoryCountsCompositeReceiversAgainstTheInterface(t *testing.T) {
	factory := NewReceiverFactory()

	composite := dynamicprefixiov1alpha1.AcquisitionSpec{
		DHCPv6PD:            &dynamicprefixiov1alpha1.DHCPv6PDSpec{Interface: "eth0"},
		RouterAdvertisement: &dynamicprefixiov1alpha1.RouterAdvertisementSpec{Interface: "eth0", Enabled: ptr.To(true)},
	}

	receiver, err := factory.CreateReceiver(composite)
	if err != nil {
		t.Fatalf("composite receiver: %v", err)
	}

	var busy *InterfaceBusyError
	if _, err := factory.CreateReceiver(pdSpec("eth0")); !errors.As(err, &busy) {
		t.Fatalf("error = %v, want the interface to be reported busy", err)
	}

	if err := receiver.Stop(); err != nil {
		t.Fatalf("Stop() = %v", err)
	}
	if _, err := factory.CreateReceiver(pdSpec("eth0")); err != nil {
		t.Fatalf("receiver after the composite was stopped: %v", err)
	}
}

// The composite claims the interface for its DHCPv6-PD half before it builds
// its RA half. If that second half fails the claim has to go back, or a
// misconfigured resource makes the interface unusable until the operator is
// restarted -- and the resource that broke it would be told the interface is
// busy, which is true only because of itself.
func TestFactoryReleasesTheInterfaceWhenTheCompositeFallbackFails(t *testing.T) {
	factory := NewReceiverFactory()

	broken := dynamicprefixiov1alpha1.AcquisitionSpec{
		DHCPv6PD:            &dynamicprefixiov1alpha1.DHCPv6PDSpec{Interface: "eth0"},
		RouterAdvertisement: &dynamicprefixiov1alpha1.RouterAdvertisementSpec{Enabled: ptr.To(true)},
	}
	if _, err := factory.CreateReceiver(broken); err == nil {
		t.Fatal("expected the composite to fail without an RA interface")
	}

	if _, err := factory.CreateReceiver(pdSpec("eth0")); err != nil {
		t.Fatalf("eth0 stayed claimed after a failed composite: %v", err)
	}
}
