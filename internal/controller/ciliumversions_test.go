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
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery/fake"
	coretesting "k8s.io/client-go/testing"
)

func TestDiscoverCiliumVersions_PrefersV2(t *testing.T) {
	// Simulate a Cilium 1.16+ cluster with both v2 and v2alpha1
	dc := &fake.FakeDiscovery{
		Fake:               &coretesting.Fake{},
		FakedServerVersion: nil,
	}
	dc.Resources = []*metav1.APIResourceList{
		{
			GroupVersion: "cilium.io/v2",
			APIResources: []metav1.APIResource{
				{Name: "ciliumloadbalancerippools", Kind: "CiliumLoadBalancerIPPool"},
				{Name: "ciliumcidrgroups", Kind: "CiliumCIDRGroup"},
				{Name: "ciliumbgpadvertisements", Kind: "CiliumBGPAdvertisement"},
			},
		},
		{
			GroupVersion: "cilium.io/v2alpha1",
			APIResources: []metav1.APIResource{
				{Name: "ciliumloadbalancerippools", Kind: "CiliumLoadBalancerIPPool"},
				{Name: "ciliumcidrgroups", Kind: "CiliumCIDRGroup"},
				{Name: "ciliumbgpadvertisements", Kind: "CiliumBGPAdvertisement"},
			},
		},
	}

	versions, err := DiscoverCiliumVersions(dc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should prefer v2
	assertGVK(t, versions.LoadBalancerIPPool, "v2", "CiliumLoadBalancerIPPool")
	assertGVK(t, versions.CIDRGroup, "v2", "CiliumCIDRGroup")
	assertGVK(t, versions.BGPAdvertisement, "v2", "CiliumBGPAdvertisement")
}

func TestDiscoverCiliumVersions_FallsBackToV2Alpha1(t *testing.T) {
	// Simulate an older Cilium cluster with only v2alpha1
	dc := &fake.FakeDiscovery{
		Fake: &coretesting.Fake{},
	}
	dc.Resources = []*metav1.APIResourceList{
		{
			GroupVersion: "cilium.io/v2alpha1",
			APIResources: []metav1.APIResource{
				{Name: "ciliumloadbalancerippools", Kind: "CiliumLoadBalancerIPPool"},
				{Name: "ciliumcidrgroups", Kind: "CiliumCIDRGroup"},
				{Name: "ciliumbgpadvertisements", Kind: "CiliumBGPAdvertisement"},
			},
		},
	}

	versions, err := DiscoverCiliumVersions(dc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should fall back to v2alpha1
	assertGVK(t, versions.LoadBalancerIPPool, "v2alpha1", "CiliumLoadBalancerIPPool")
	assertGVK(t, versions.CIDRGroup, "v2alpha1", "CiliumCIDRGroup")
	assertGVK(t, versions.BGPAdvertisement, "v2alpha1", "CiliumBGPAdvertisement")
}

func TestDiscoverCiliumVersions_MixedVersions(t *testing.T) {
	// Some resources in v2, some only in v2alpha1
	dc := &fake.FakeDiscovery{
		Fake: &coretesting.Fake{},
	}
	dc.Resources = []*metav1.APIResourceList{
		{
			GroupVersion: "cilium.io/v2",
			APIResources: []metav1.APIResource{
				{Name: "ciliumloadbalancerippools", Kind: "CiliumLoadBalancerIPPool"},
			},
		},
		{
			GroupVersion: "cilium.io/v2alpha1",
			APIResources: []metav1.APIResource{
				{Name: "ciliumloadbalancerippools", Kind: "CiliumLoadBalancerIPPool"},
				{Name: "ciliumcidrgroups", Kind: "CiliumCIDRGroup"},
				{Name: "ciliumbgpadvertisements", Kind: "CiliumBGPAdvertisement"},
			},
		},
	}

	versions, err := DiscoverCiliumVersions(dc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// LBPool should prefer v2
	assertGVK(t, versions.LoadBalancerIPPool, "v2", "CiliumLoadBalancerIPPool")
	// Others should fall back to v2alpha1
	assertGVK(t, versions.CIDRGroup, "v2alpha1", "CiliumCIDRGroup")
	assertGVK(t, versions.BGPAdvertisement, "v2alpha1", "CiliumBGPAdvertisement")
}

func TestDiscoverCiliumVersions_NoCilium(t *testing.T) {
	// No cilium.io group at all
	dc := &fake.FakeDiscovery{
		Fake: &coretesting.Fake{},
	}
	dc.Resources = []*metav1.APIResourceList{
		{
			GroupVersion: "apps/v1",
			APIResources: []metav1.APIResource{
				{Name: "deployments", Kind: "Deployment"},
			},
		},
	}

	_, err := DiscoverCiliumVersions(dc)
	if err == nil {
		t.Fatal("expected error when cilium.io not found, got nil")
	}
}

func TestDiscoverCiliumVersions_MissingResource(t *testing.T) {
	// cilium.io group exists but missing a required resource
	dc := &fake.FakeDiscovery{
		Fake: &coretesting.Fake{},
	}
	dc.Resources = []*metav1.APIResourceList{
		{
			GroupVersion: "cilium.io/v2",
			APIResources: []metav1.APIResource{
				{Name: "ciliumloadbalancerippools", Kind: "CiliumLoadBalancerIPPool"},
				// Missing: ciliumcidrgroups, ciliumbgpadvertisements
			},
		},
	}

	_, err := DiscoverCiliumVersions(dc)
	if err == nil {
		t.Fatal("expected error when required resource is missing, got nil")
	}
}

func TestListGVK(t *testing.T) {
	gvk := schema.GroupVersionKind{Group: ciliumAPIGroup, Version: "v2", Kind: "CiliumLoadBalancerIPPool"}
	listGVK := ListGVK(gvk)

	if listGVK.Kind != "CiliumLoadBalancerIPPoolList" {
		t.Errorf("ListGVK().Kind = %q, want %q", listGVK.Kind, "CiliumLoadBalancerIPPoolList")
	}
	if listGVK.Group != ciliumAPIGroup {
		t.Errorf("ListGVK().Group = %q, want %q", listGVK.Group, ciliumAPIGroup)
	}
	if listGVK.Version != "v2" {
		t.Errorf("ListGVK().Version = %q, want %q", listGVK.Version, "v2")
	}
}

func TestAPIVersion(t *testing.T) {
	tests := []struct {
		name     string
		gvk      schema.GroupVersionKind
		expected string
	}{
		{
			name:     "v2",
			gvk:      schema.GroupVersionKind{Group: ciliumAPIGroup, Version: "v2", Kind: "CiliumLoadBalancerIPPool"},
			expected: ciliumAPIGroup + "/v2",
		},
		{
			name:     "v2alpha1",
			gvk:      schema.GroupVersionKind{Group: ciliumAPIGroup, Version: "v2alpha1", Kind: "CiliumBGPAdvertisement"},
			expected: ciliumAPIGroup + "/v2alpha1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := APIVersion(tt.gvk)
			if result != tt.expected {
				t.Errorf("APIVersion() = %q, want %q", result, tt.expected)
			}
		})
	}
}

func assertGVK(t *testing.T, gvk schema.GroupVersionKind, version, kind string) {
	t.Helper()
	if gvk.Group != ciliumAPIGroup {
		t.Errorf("GVK.Group = %q, want %q", gvk.Group, ciliumAPIGroup)
	}
	if gvk.Version != version {
		t.Errorf("GVK.Version = %q, want %q (for %s)", gvk.Version, version, kind)
	}
	if gvk.Kind != kind {
		t.Errorf("GVK.Kind = %q, want %q", gvk.Kind, kind)
	}
}

// ciliumResources returns a standard set of Cilium API resources for testing.
func ciliumResources() []*metav1.APIResourceList {
	return []*metav1.APIResourceList{
		{
			GroupVersion: "cilium.io/v2",
			APIResources: []metav1.APIResource{
				{Name: "ciliumloadbalancerippools", Kind: "CiliumLoadBalancerIPPool"},
				{Name: "ciliumcidrgroups", Kind: "CiliumCIDRGroup"},
				{Name: "ciliumbgpadvertisements", Kind: "CiliumBGPAdvertisement"},
			},
		},
	}
}

func TestCiliumControllerStarter_DetectsCiliumImmediately(t *testing.T) {
	dc := &fake.FakeDiscovery{Fake: &coretesting.Fake{}}
	dc.Resources = ciliumResources()

	var called atomic.Bool
	starter := &CiliumControllerStarter{
		Discovery:    dc,
		PollInterval: 10 * time.Millisecond,
		SetupControllers: func(versions *CiliumVersions) error {
			called.Store(true)
			if versions.LoadBalancerIPPool.Version != "v2" {
				t.Errorf("expected v2, got %s", versions.LoadBalancerIPPool.Version)
			}
			return nil
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := starter.Start(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called.Load() {
		t.Error("SetupControllers was not called")
	}
}

func TestCiliumControllerStarter_WaitsForCilium(t *testing.T) {
	dc := &fake.FakeDiscovery{Fake: &coretesting.Fake{}}
	// Start with no Cilium resources
	dc.Resources = []*metav1.APIResourceList{
		{GroupVersion: "apps/v1", APIResources: []metav1.APIResource{{Name: "deployments", Kind: "Deployment"}}},
	}

	var called atomic.Bool
	var pollCount atomic.Int32
	starter := &CiliumControllerStarter{
		Discovery:    dc,
		PollInterval: 10 * time.Millisecond,
		SetupControllers: func(versions *CiliumVersions) error {
			called.Store(true)
			return nil
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Add Cilium resources after a short delay
	go func() {
		// Wait for a few polls
		for pollCount.Load() < 3 {
			time.Sleep(5 * time.Millisecond)
			pollCount.Add(1)
		}
		dc.Resources = ciliumResources()
	}()

	err := starter.Start(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called.Load() {
		t.Error("SetupControllers was not called after Cilium became available")
	}
}

func TestCiliumControllerStarter_StopsOnContextCancel(t *testing.T) {
	dc := &fake.FakeDiscovery{Fake: &coretesting.Fake{}}
	// No Cilium resources — will never be found
	dc.Resources = []*metav1.APIResourceList{}

	var called atomic.Bool
	starter := &CiliumControllerStarter{
		Discovery:    dc,
		PollInterval: 10 * time.Millisecond,
		SetupControllers: func(versions *CiliumVersions) error {
			called.Store(true)
			return nil
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := starter.Start(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if called.Load() {
		t.Error("SetupControllers should not have been called")
	}
}

func TestCiliumControllerStarter_PropagatesSetupError(t *testing.T) {
	dc := &fake.FakeDiscovery{Fake: &coretesting.Fake{}}
	dc.Resources = ciliumResources()

	setupErr := fmt.Errorf("controller setup failed")
	starter := &CiliumControllerStarter{
		Discovery:    dc,
		PollInterval: 10 * time.Millisecond,
		SetupControllers: func(versions *CiliumVersions) error {
			return setupErr
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := starter.Start(ctx)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !contains(err.Error(), "controller setup failed") {
		t.Errorf("error %q should contain 'controller setup failed'", err.Error())
	}
}

func TestCiliumControllerStarter_DefaultPollInterval(t *testing.T) {
	starter := &CiliumControllerStarter{}
	// NeedLeaderElection should return true
	if !starter.NeedLeaderElection() {
		t.Error("NeedLeaderElection() should return true")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
