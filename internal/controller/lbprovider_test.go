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
	"net/netip"
	"strings"
	"testing"
)

// Existing Services carry no provider annotation, and their addresses must keep
// going where they always did.
func TestLBProviderDefaultsToCilium(t *testing.T) {
	provider, field, err := lbAddressAnnotationFor(nil)
	if err != nil {
		t.Fatalf("lbAddressAnnotationFor(nil) = %v", err)
	}
	if provider != lbProviderCilium {
		t.Errorf("provider = %q, want %q", provider, lbProviderCilium)
	}
	if field != AnnotationCiliumIPs {
		t.Errorf("field = %q, want %q", field, AnnotationCiliumIPs)
	}
}

func TestLBProviderSelection(t *testing.T) {
	tests := []struct {
		name      string
		value     string
		wantField string
		wantErr   bool
	}{
		{name: "explicit cilium", value: lbProviderCilium, wantField: AnnotationCiliumIPs},
		{name: "kube-vip", value: lbProviderKubevip, wantField: AnnotationKubevipLBIPs},
		{name: "surrounding whitespace is tolerated", value: "  kube-vip  ", wantField: AnnotationKubevipLBIPs},
		{name: "anything else is refused", value: "metallb", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, field, err := lbAddressAnnotationFor(map[string]string{AnnotationLBProvider: tt.value})
			if tt.wantErr {
				if err == nil {
					t.Fatalf("lbAddressAnnotationFor(%q) accepted an unknown provider", tt.value)
				}
				// A Service asking for something unsupported must be told which
				// providers exist, not just that it was wrong.
				if !strings.Contains(err.Error(), lbProviderKubevip) {
					t.Errorf("error = %q, want it to name the supported providers", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("lbAddressAnnotationFor(%q) = %v", tt.value, err)
			}
			if field != tt.wantField {
				t.Errorf("field = %q, want %q", field, tt.wantField)
			}
		})
	}
}

func TestWriteLBAddressesTargetsTheSelectedProvider(t *testing.T) {
	annotations := map[string]string{}
	newAnnotations := map[string]string{}

	if !writeLBAddresses(annotations, newAnnotations, AnnotationKubevipLBIPs, "2001:db8::1", ownershipRecord{}, nil) {
		t.Fatal("writing addresses reported no change")
	}
	if got := newAnnotations[AnnotationKubevipLBIPs]; got != "2001:db8::1" {
		t.Errorf("%s = %q, want the calculated address", AnnotationKubevipLBIPs, got)
	}
	if _, written := newAnnotations[AnnotationCiliumIPs]; written {
		t.Errorf("%s was written on a Service that does not use Cilium", AnnotationCiliumIPs)
	}
}

// Flipping the provider has to take the addresses with it. Left behind, they
// keep requesting a prefix that stops existing at the next rotation.
func TestWriteLBAddressesMovesEntriesOffTheOtherProvider(t *testing.T) {
	record := parseOwnershipRecord("2001:db8::1", true)
	annotations := map[string]string{
		AnnotationCiliumIPs:  "192.0.2.5,2001:db8::1",
		AnnotationManagedIPs: "2001:db8::1",
	}
	newAnnotations := map[string]string{}
	for k, v := range annotations {
		newAnnotations[k] = v
	}

	managed := []netip.Prefix{netip.MustParsePrefix("2001:db8::/64")}
	if !writeLBAddresses(annotations, newAnnotations, AnnotationKubevipLBIPs, "2001:db8::1", record, managed) {
		t.Fatal("the flip reported no change")
	}

	if got := newAnnotations[AnnotationKubevipLBIPs]; got != "2001:db8::1" {
		t.Errorf("%s = %q, want the address moved to the new provider", AnnotationKubevipLBIPs, got)
	}
	// The user's own IPv4 entry was never the operator's to remove.
	if got := newAnnotations[AnnotationCiliumIPs]; got != "192.0.2.5" {
		t.Errorf("%s = %q, want only the operator's entry taken out", AnnotationCiliumIPs, got)
	}
}

func TestWriteLBAddressesRemovesAnEmptiedAnnotation(t *testing.T) {
	record := parseOwnershipRecord("2001:db8::1", true)
	annotations := map[string]string{AnnotationCiliumIPs: "2001:db8::1"}
	newAnnotations := map[string]string{AnnotationCiliumIPs: "2001:db8::1"}

	writeLBAddresses(annotations, newAnnotations, AnnotationKubevipLBIPs, "2001:db8::1", record, nil)

	if _, present := newAnnotations[AnnotationCiliumIPs]; present {
		t.Errorf("%s = %q, want the annotation removed once nothing of the user's is left in it",
			AnnotationCiliumIPs, newAnnotations[AnnotationCiliumIPs])
	}
}
