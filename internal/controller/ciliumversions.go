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

// preferredVersions lists API versions in order of preference (most preferred first).
// Cilium 1.16+ serves v2 as the stable API; v2alpha1 is deprecated but still
// served for backward compatibility with older clusters.
var preferredVersions = []string{"v2", "v2alpha1"}

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
		for _, v := range preferredVersions {
			if hasResource(resourceVersions, plural, v) {
				gvk := schema.GroupVersionKind{Group: ciliumAPIGroup, Version: v, Kind: kind}
				log.Info("Resolved Cilium API version", "kind", kind, "version", v)
				return gvk, nil
			}
		}
		return schema.GroupVersionKind{}, fmt.Errorf("no supported API version found for %s/%s (checked %v)", ciliumAPIGroup, kind, preferredVersions)
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
	groups, err := dc.ServerGroups()
	if err != nil {
		return nil, fmt.Errorf("failed to fetch API groups: %w", err)
	}

	for _, g := range groups.Groups {
		if g.Name == ciliumAPIGroup {
			var versions []string
			for _, v := range g.Versions {
				versions = append(versions, v.Version)
			}
			return versions, nil
		}
	}

	return nil, fmt.Errorf("%s API group not found on the cluster — is Cilium installed?", ciliumAPIGroup)
}

// resourceSet maps resource plural name to the set of versions that serve it.
type resourceSet map[string]map[string]bool

// discoverCiliumResources fetches the resources served under each available
// cilium.io version and builds a lookup table.
func discoverCiliumResources(dc discovery.DiscoveryInterface, versions []string) resourceSet {
	rs := make(resourceSet)

	for _, v := range versions {
		resources, err := dc.ServerResourcesForGroupVersion(ciliumAPIGroup + "/" + v)
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
