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
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	ctrl "sigs.k8s.io/controller-runtime"
)

// ciliumAPIGroup is the API group for all Cilium custom resources.
const ciliumAPIGroup = "cilium.io"

const (
	metallbAPIGroup = "metallb.io"
	calicoAPIGroup  = "projectcalico.org"
)

// preferredVersions lists API versions in order of preference (most preferred first).
// Cilium 1.16+ serves v2 as the stable API; v2alpha1 is deprecated but still
// served for backward compatibility with older clusters.
var preferredVersions = []string{"v2", "v2alpha1"}

var (
	preferredMetalLBVersions = []string{"v1beta1"}
	preferredCalicoVersions  = []string{"v3"}
)

// CiliumVersions holds the resolved GroupVersionKind for each Cilium resource
// the operator interacts with. The versions are determined at startup by
// probing the API server's discovery endpoint.
type CiliumVersions struct {
	// LoadBalancerIPPool is the resolved GVK for CiliumLoadBalancerIPPool.
	LoadBalancerIPPool schema.GroupVersionKind
	// CIDRGroup is the resolved GVK for CiliumCIDRGroup.
	CIDRGroup schema.GroupVersionKind
	// BGPAdvertisement is the resolved GVK for CiliumBGPAdvertisement.
	BGPAdvertisement schema.GroupVersionKind
}

// DiscoverCiliumVersions probes the Kubernetes API server to determine which
// Cilium API versions are available. It prefers v2 and falls back to v2alpha1.
// Returns an error if a required resource cannot be found in any known version.
func DiscoverCiliumVersions(dc discovery.DiscoveryInterface) (*CiliumVersions, error) {
	log := ctrl.Log.WithName("setup")

	available, err := discoverCiliumGroupVersions(dc)
	if err != nil {
		return nil, fmt.Errorf("failed to discover cilium.io API versions: %w", err)
	}

	// Build a resource → available-version set from the server resources.
	resourceVersions := discoverCiliumResources(dc, available)

	resolve := func(kind, plural string) (schema.GroupVersionKind, error) {
		gvk, err := resolveResource(resourceVersions, ciliumAPIGroup, kind, plural, preferredVersions)
		if err != nil {
			return schema.GroupVersionKind{}, err
		}
		log.Info("Resolved Cilium API version", "kind", kind, "version", gvk.Version)
		return gvk, nil
	}

	lbPool, err := resolve("CiliumLoadBalancerIPPool", "ciliumloadbalancerippools")
	if err != nil {
		return nil, err
	}
	cidrGroup, err := resolve("CiliumCIDRGroup", "ciliumcidrgroups")
	if err != nil {
		return nil, err
	}
	bgpAdv, err := resolve("CiliumBGPAdvertisement", "ciliumbgpadvertisements")
	if err != nil {
		return nil, err
	}

	return &CiliumVersions{
		LoadBalancerIPPool: lbPool,
		CIDRGroup:          cidrGroup,
		BGPAdvertisement:   bgpAdv,
	}, nil
}

// DiscoverPoolBackendGVKs probes the Kubernetes API server for supported pool
// backend resources. It returns only resources that are currently available and
// can be watched by PoolSyncReconciler.
func DiscoverPoolBackendGVKs(dc discovery.DiscoveryInterface) ([]schema.GroupVersionKind, error) {
	log := ctrl.Log.WithName("setup")
	definitions := []struct {
		group             string
		kind              string
		plural            string
		preferredVersions []string
	}{
		{group: ciliumAPIGroup, kind: "CiliumLoadBalancerIPPool", plural: "ciliumloadbalancerippools", preferredVersions: preferredVersions},
		{group: ciliumAPIGroup, kind: "CiliumCIDRGroup", plural: "ciliumcidrgroups", preferredVersions: preferredVersions},
		{group: metallbAPIGroup, kind: "IPAddressPool", plural: "ipaddresspools", preferredVersions: preferredMetalLBVersions},
		{group: calicoAPIGroup, kind: "IPPool", plural: "ippools", preferredVersions: preferredCalicoVersions},
	}

	resourceVersionsByGroup := make(map[string]resourceSet)
	var gvks []schema.GroupVersionKind
	for _, def := range definitions {
		resourceVersions, ok := resourceVersionsByGroup[def.group]
		if !ok {
			available, err := discoverGroupVersions(dc, def.group)
			if err != nil {
				continue
			}
			resourceVersions = discoverResources(dc, def.group, available)
			resourceVersionsByGroup[def.group] = resourceVersions
		}

		gvk, err := resolveResource(resourceVersions, def.group, def.kind, def.plural, def.preferredVersions)
		if err != nil {
			log.V(1).Info("Pool backend resource not available", "group", def.group, "kind", def.kind, "reason", err.Error())
			continue
		}
		log.Info("Resolved pool backend API version", "kind", def.kind, "group", def.group, "version", gvk.Version)
		gvks = append(gvks, gvk)
	}

	if len(gvks) == 0 {
		return nil, fmt.Errorf("no supported pool backend APIs found")
	}
	return gvks, nil
}

// ListGVK returns the List variant of a GVK (e.g. CiliumLoadBalancerIPPoolList).
func ListGVK(gvk schema.GroupVersionKind) schema.GroupVersionKind {
	return schema.GroupVersionKind{
		Group:   gvk.Group,
		Version: gvk.Version,
		Kind:    gvk.Kind + "List",
	}
}

// APIVersion returns the "group/version" string for use in unstructured objects
// (e.g. "cilium.io/v2").
func APIVersion(gvk schema.GroupVersionKind) string {
	return gvk.Group + "/" + gvk.Version
}

// discoverCiliumGroupVersions returns the list of served versions for the
// cilium.io API group.
func discoverCiliumGroupVersions(dc discovery.DiscoveryInterface) ([]string, error) {
	return discoverGroupVersions(dc, ciliumAPIGroup)
}

func discoverGroupVersions(dc discovery.DiscoveryInterface, groupName string) ([]string, error) {
	groups, err := dc.ServerGroups()
	if err != nil {
		return nil, fmt.Errorf("failed to fetch API groups: %w", err)
	}

	for _, g := range groups.Groups {
		if g.Name == groupName {
			var versions []string
			for _, v := range g.Versions {
				versions = append(versions, v.Version)
			}
			return versions, nil
		}
	}

	return nil, fmt.Errorf("%s API group not found on the cluster", groupName)
}

// resourceSet maps resource plural name to the set of versions that serve it.
type resourceSet map[string]map[string]bool

// discoverCiliumResources fetches the resources served under each available
// cilium.io version and builds a lookup table.
func discoverCiliumResources(dc discovery.DiscoveryInterface, versions []string) resourceSet {
	return discoverResources(dc, ciliumAPIGroup, versions)
}

func discoverResources(dc discovery.DiscoveryInterface, group string, versions []string) resourceSet {
	rs := make(resourceSet)

	for _, v := range versions {
		resources, err := dc.ServerResourcesForGroupVersion(group + "/" + v)
		if err != nil {
			// Version might be listed in the group but not yet available
			continue
		}
		for _, r := range resources.APIResources {
			if _, ok := rs[r.Name]; !ok {
				rs[r.Name] = make(map[string]bool)
			}
			rs[r.Name][v] = true
		}
	}

	return rs
}

func resolveResource(rs resourceSet, group, kind, plural string, preferred []string) (schema.GroupVersionKind, error) {
	for _, v := range preferred {
		if hasResource(rs, plural, v) {
			return schema.GroupVersionKind{Group: group, Version: v, Kind: kind}, nil
		}
	}
	return schema.GroupVersionKind{}, fmt.Errorf("no supported API version found for %s/%s (checked %v)", group, kind, preferred)
}

// hasResource checks whether a resource is served under a specific version.
func hasResource(rs resourceSet, plural, version string) bool {
	if versions, ok := rs[plural]; ok {
		return versions[version]
	}
	return false
}

// CiliumControllerStarter is a manager.Runnable that polls for Cilium API
// availability and registers Cilium-dependent controllers when detected.
// This allows the operator to start immediately without Cilium and
// automatically begin managing Cilium resources once Cilium is installed.
type CiliumControllerStarter struct {
	// Discovery is used to probe the API server for Cilium API groups.
	Discovery discovery.DiscoveryInterface
	// PollInterval controls how often to check for Cilium APIs.
	// Defaults to 30 seconds if zero.
	PollInterval time.Duration
	// SetupControllers is called once when Cilium APIs are detected.
	// It receives the discovered CiliumVersions and the manager to
	// register controllers with.
	SetupControllers func(versions *CiliumVersions) error
}

// Start implements manager.Runnable. It polls for Cilium APIs and registers
// controllers when they become available. Returns nil when controllers are
// registered or the context is cancelled.
func (s *CiliumControllerStarter) Start(ctx context.Context) error {
	log := ctrl.Log.WithName("setup")

	interval := s.PollInterval
	if interval == 0 {
		interval = 30 * time.Second
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	log.Info("Waiting for Cilium APIs to become available", "pollInterval", interval)

	for {
		select {
		case <-ctx.Done():
			log.Info("Stopping Cilium API discovery (context cancelled)")
			return nil
		case <-ticker.C:
			versions, err := DiscoverCiliumVersions(s.Discovery)
			if err != nil {
				log.V(1).Info("Cilium API not yet available, will retry", "reason", err.Error())
				continue
			}

			log.Info("Cilium APIs detected, registering Cilium-dependent controllers")

			if err := s.SetupControllers(versions); err != nil {
				return fmt.Errorf("failed to register Cilium controllers: %w", err)
			}

			log.Info("Cilium-dependent controllers registered successfully")
			return nil
		}
	}
}

// NeedLeaderElection implements manager.LeaderElectionRunnable. Returns true
// because the controllers this starter registers require leader election.
func (s *CiliumControllerStarter) NeedLeaderElection() bool {
	return true
}

// PoolBackendControllerStarter polls for any supported pool backend API and
// registers PoolSync once at least one backend resource is available.
type PoolBackendControllerStarter struct {
	// Discovery is used to probe the API server for supported pool backend APIs.
	Discovery discovery.DiscoveryInterface
	// PollInterval controls how often to check for backend APIs.
	// Defaults to 30 seconds if zero.
	PollInterval time.Duration
	// SetupControllers is called once when backend APIs are detected.
	SetupControllers func(gvks []schema.GroupVersionKind) error
}

// Start implements manager.Runnable. It polls for supported backend APIs and
// registers controllers when they become available. Returns nil when controllers
// are registered or the context is cancelled.
func (s *PoolBackendControllerStarter) Start(ctx context.Context) error {
	log := ctrl.Log.WithName("setup")

	interval := s.PollInterval
	if interval == 0 {
		interval = 30 * time.Second
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	log.Info("Waiting for pool backend APIs to become available", "pollInterval", interval)

	for {
		select {
		case <-ctx.Done():
			log.Info("Stopping pool backend API discovery (context cancelled)")
			return nil
		case <-ticker.C:
			gvks, err := DiscoverPoolBackendGVKs(s.Discovery)
			if err != nil {
				log.V(1).Info("Pool backend APIs not yet available, will retry", "reason", err.Error())
				continue
			}

			log.Info("Pool backend APIs detected, registering PoolSync controller", "backendCount", len(gvks))

			if err := s.SetupControllers(gvks); err != nil {
				return fmt.Errorf("failed to register PoolSync controller: %w", err)
			}

			log.Info("PoolSync controller registered successfully")
			return nil
		}
	}
}

// NeedLeaderElection implements manager.LeaderElectionRunnable. Returns true
// because PoolSync updates shared cluster resources.
func (s *PoolBackendControllerStarter) NeedLeaderElection() bool {
	return true
}
