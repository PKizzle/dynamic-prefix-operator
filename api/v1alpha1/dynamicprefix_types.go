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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DynamicPrefixSpec defines the desired state of DynamicPrefix
type DynamicPrefixSpec struct {
	// Acquisition defines how to receive the IPv6 prefix
	// +required
	Acquisition AcquisitionSpec `json:"acquisition"`

	// AddressRanges defines address ranges within the received prefix.
	// Use this for Mode 1 (recommended): reserve a range within your /64 that
	// your router's DHCPv6/SLAAC won't hand out. No BGP required.
	// +listType=map
	// +listMapKey=name
	// +optional
	AddressRanges []AddressRangeSpec `json:"addressRanges,omitempty"`

	// Subnets defines how to subdivide the received prefix into smaller subnets.
	// Use this for Mode 2 (advanced): carve out dedicated /64s from a larger
	// prefix. Requires BGP to announce the subnets to your router.
	// +listType=map
	// +listMapKey=name
	// +optional
	Subnets []SubnetSpec `json:"subnets,omitempty"`

	// Transition defines graceful transition settings when prefix changes
	// +optional
	Transition *TransitionSpec `json:"transition,omitempty"`
}

// AcquisitionSpec defines how to acquire/receive the IPv6 prefix
// +kubebuilder:validation:XValidation:rule="has(self.dhcpv6pd) || has(self.routerAdvertisement)",message="at least one acquisition method must be configured"
type AcquisitionSpec struct {
	// DHCPv6PD configures DHCPv6 Prefix Delegation to receive prefix from upstream router
	// +optional
	DHCPv6PD *DHCPv6PDSpec `json:"dhcpv6pd,omitempty"`

	// RouterAdvertisement configures Router Advertisement monitoring as fallback
	// +optional
	RouterAdvertisement *RouterAdvertisementSpec `json:"routerAdvertisement,omitempty"`

	// PrefixFilter constrains which acquired prefixes are accepted, independent of
	// the acquisition source.
	// +optional
	PrefixFilter *PrefixFilterSpec `json:"prefixFilter,omitempty"`
}

// PrefixFilterSpec constrains which acquired prefixes the operator will accept.
// +kubebuilder:validation:XValidation:rule="!has(self.minPrefixLength) || !has(self.maxPrefixLength) || self.minPrefixLength <= self.maxPrefixLength",message="minPrefixLength must not be greater than maxPrefixLength"
type PrefixFilterSpec struct {
	// RequireGlobalUnicast rejects any acquired prefix outside 2000::/3, notably
	// unique-local (fc00::/7) and link-local (fe80::/10).
	//
	// A delegated prefix is global unicast by definition, so this defaults to true.
	// It matters because a link can advertise several prefixes: if a Router
	// Advertisement carries no global prefix -- during upstream renegotiation, or on
	// a link where a unique-local prefix is advertised alongside -- a receiver that
	// merely prefers global unicast will accept the unique-local one and rotate it
	// in as though the delegation had changed. Every address the operator derives
	// then moves into a range that is not routable off-link, and the displaced
	// prefix ages out of history as if it were genuinely retired.
	//
	// Set to false only when the prefix being tracked is deliberately not global
	// unicast, which is unusual outside test environments.
	// +optional
	// +kubebuilder:default=true
	RequireGlobalUnicast *bool `json:"requireGlobalUnicast,omitempty"`

	// MinPrefixLength rejects any acquired prefix shorter than this, counted in
	// bits, so a larger number is a smaller prefix.
	//
	// An upstream that hands back something far larger than expected -- or an
	// advertisement from a router that should not be delegating at all -- is
	// otherwise taken at face value, and every derived range moves with it.
	// Bounding the length is the cheapest way to say what a plausible
	// delegation looks like on this link. Applies to every acquisition source.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=128
	MinPrefixLength *int `json:"minPrefixLength,omitempty"`

	// MaxPrefixLength rejects any acquired prefix longer than this, counted in
	// bits. Applies to every acquisition source.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=128
	MaxPrefixLength *int `json:"maxPrefixLength,omitempty"`
}

// PrefixLengthBounds reports the accepted prefix-length range, with zero
// meaning unbounded at that end.
func (dp *DynamicPrefix) PrefixLengthBounds() (minBits, maxBits int) {
	filter := dp.Spec.Acquisition.PrefixFilter
	if filter == nil {
		return 0, 0
	}
	if filter.MinPrefixLength != nil {
		minBits = *filter.MinPrefixLength
	}
	if filter.MaxPrefixLength != nil {
		maxBits = *filter.MaxPrefixLength
	}
	return minBits, maxBits
}

// DHCPv6PDSpec configures the DHCPv6 Prefix Delegation client
type DHCPv6PDSpec struct {
	// Interface is the network interface to receive the delegated prefix on
	// +required
	// +kubebuilder:validation:MinLength=1
	Interface string `json:"interface"`

	// RequestedPrefixLength hints the desired prefix length to request
	// +optional
	// +kubebuilder:validation:Minimum=48
	// +kubebuilder:validation:Maximum=64
	RequestedPrefixLength *int `json:"requestedPrefixLength,omitempty"`
}

// RouterAdvertisementSpec configures Router Advertisement monitoring
type RouterAdvertisementSpec struct {
	// Interface is the network interface to monitor for Router Advertisements
	// +optional
	// +kubebuilder:validation:MinLength=1
	Interface string `json:"interface,omitempty"`

	// Enabled controls whether RA monitoring is active. Defaults to true.
	//
	// A pointer, because the pair "omitempty" and "default=true" cannot express
	// false on a plain bool: the zero value is indistinguishable from unset, so a
	// Go client round-tripping this object drops `enabled: false` from the JSON
	// and the API server defaults it straight back to true. PrefixFilterSpec
	// already models an opt-out this way.
	// +optional
	// +kubebuilder:default=true
	Enabled *bool `json:"enabled,omitempty"`

	// TrustedRouters restricts accepted Router Advertisements to those sent
	// from these link-local addresses -- the fe80:: address the router uses on
	// this link, which is what an advertisement's source address carries.
	//
	// Anything on the link can send a Router Advertisement, and the RFC 4861
	// checks the receiver already applies say only that the sender is on-link
	// and one hop away, which is true of every host on the segment. Naming the
	// routers that may be believed is what turns that into a trust decision.
	// Advertisements from anywhere else are dropped, counted and reported.
	//
	// Leave empty on a link you control, where switch-side RA Guard does this
	// job, or where DHCPv6-PD is used instead.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=8
	// +kubebuilder:validation:items:MaxLength=45
	// +kubebuilder:validation:items:Pattern=`^[fF][eE][89abAB][0-9a-fA-F]:[0-9a-fA-F:]*$`
	TrustedRouters []string `json:"trustedRouters,omitempty"`
}

// RAEnabled reports whether Router Advertisement monitoring is active, applying
// the documented default for a spec that does not say.
func (s *RouterAdvertisementSpec) RAEnabled() bool {
	if s == nil {
		return false
	}
	if s.Enabled == nil {
		return true
	}
	return *s.Enabled
}

// AddressRangeSpec defines an address range within the received prefix.
// This is used for Mode 1 where you reserve a portion of your /64 that
// the router won't hand out via DHCPv6/SLAAC.
type AddressRangeSpec struct {
	// Name identifies this address range (used in annotations to reference it)
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`

	// Start is the start of the range, specified as a suffix to the prefix.
	// For example, "::f000:0:0:0" means start at prefix + 0xf000:0:0:0.
	// +required
	// +kubebuilder:validation:MinLength=2
	// +kubebuilder:validation:Pattern=`^[0-9a-fA-F:]+$`
	Start string `json:"start"`

	// End is the end of the range (inclusive), specified as a suffix.
	// For example, "::ffff:ffff:ffff:ffff" means end at prefix + 0xffff:ffff:ffff:ffff.
	// +required
	// +kubebuilder:validation:MinLength=2
	// +kubebuilder:validation:Pattern=`^[0-9a-fA-F:]+$`
	End string `json:"end"`
}

// SubnetSpec defines a subnet to be carved out of the received prefix.
// This is used for Mode 2 (advanced) where you claim a dedicated /64 from
// a larger prefix and announce it via BGP.
type SubnetSpec struct {
	// Name identifies this subnet (used in annotations to reference it)
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`

	// Offset selects the Nth subnet of PrefixLength within the received prefix.
	// For example, with a /56 base prefix and PrefixLength 64, offset 255 selects
	// the last /64 in the /56.
	// +optional
	// +kubebuilder:default=0
	// +kubebuilder:validation:Minimum=0
	Offset int64 `json:"offset,omitempty"`

	// PrefixLength is the prefix length of the subnet (e.g., 120 for a /120)
	// +required
	// +kubebuilder:validation:Minimum=48
	// +kubebuilder:validation:Maximum=128
	PrefixLength int `json:"prefixLength"`

	// BGP configures BGP advertisement for LoadBalancer IPs from this subnet.
	// Requires Cilium BGP Control Plane to be enabled and peering configured separately.
	// +optional
	BGP *SubnetBGPSpec `json:"bgp,omitempty"`
}

// SubnetBGPSpec configures BGP advertisement for a subnet.
type SubnetBGPSpec struct {
	// Advertise enables BGP advertisement of LoadBalancer IPs from this subnet.
	// When true, the operator creates a CiliumBGPAdvertisement resource that
	// causes Cilium to announce individual Service IPs (/128) to BGP peers.
	// +optional
	Advertise bool `json:"advertise,omitempty"`

	// Community is the BGP community to attach to advertisements.
	// Format: "ASN:VALUE" (e.g., "65001:100").
	// The router should be configured to filter and only accept routes with this community.
	// This provides an additional layer of security beyond prefix-length filtering.
	// +optional
	// +kubebuilder:validation:Pattern=`^\d+:\d+$`
	Community string `json:"community,omitempty"`
}

// TransitionMode defines the transition behavior mode
type TransitionMode string

const (
	// TransitionModeSimple keeps multiple blocks in pool; Services keep old IPs until block removed
	TransitionModeSimple TransitionMode = "simple"

	// TransitionModeHA keeps both old and new IPs on Service, with DNS pointing to new IP only
	TransitionModeHA TransitionMode = "ha"
)

// TransitionSpec defines settings for graceful prefix transitions
type TransitionSpec struct {
	// Mode specifies the transition behavior.
	// "simple" (default): Keep multiple blocks in pool, Services keep old IPs until block removed.
	// "ha": Keep both old and new IPs on Service, DNS points to new IP only via external-dns annotation.
	// +optional
	// +kubebuilder:validation:Enum=simple;ha
	// +kubebuilder:default=simple
	Mode TransitionMode `json:"mode,omitempty"`

	// MaxPrefixHistory is the maximum number of previous prefixes to retain in pool blocks.
	// When a new prefix is received, historical prefixes beyond this limit are dropped.
	// +optional
	// +kubebuilder:default=2
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10
	MaxPrefixHistory int `json:"maxPrefixHistory,omitempty"`
}

// DynamicPrefixStatus defines the observed state of DynamicPrefix
type DynamicPrefixStatus struct {
	// CurrentPrefix is the currently active IPv6 prefix in CIDR notation
	// +optional
	CurrentPrefix string `json:"currentPrefix,omitempty"`

	// PrefixSource indicates how the prefix was obtained
	// +optional
	PrefixSource PrefixSource `json:"prefixSource,omitempty"`

	// LeaseExpiresAt indicates when the DHCPv6 lease expires
	// +optional
	LeaseExpiresAt *metav1.Time `json:"leaseExpiresAt,omitempty"`

	// AddressRanges contains the calculated address ranges
	// +optional
	AddressRanges []AddressRangeStatus `json:"addressRanges,omitempty"`

	// Subnets contains the calculated subnet CIDRs
	// +optional
	Subnets []SubnetStatus `json:"subnets,omitempty"`

	// History contains previous prefixes
	// +optional
	History []PrefixHistoryEntry `json:"history,omitempty"`

	// Conditions represent the current state of the DynamicPrefix
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// PrefixSource indicates how a prefix was obtained
// +kubebuilder:validation:Enum=dhcpv6-pd;router-advertisement;static;unknown
type PrefixSource string

const (
	PrefixSourceDHCPv6PD            PrefixSource = "dhcpv6-pd"
	PrefixSourceRouterAdvertisement PrefixSource = "router-advertisement"
	PrefixSourceStatic              PrefixSource = "static"
	PrefixSourceUnknown             PrefixSource = "unknown"
)

// AddressRangeStatus represents the current state of an address range
type AddressRangeStatus struct {
	// Name is the address range identifier
	Name string `json:"name"`

	// Start is the first address in the range (full address)
	Start string `json:"start"`

	// End is the last address in the range (full address)
	End string `json:"end"`

	// CIDR is an approximate CIDR representation for compatibility.
	// For Cilium pools, use Start/End for precise range definition.
	// This may be a larger range if the start/end don't align to CIDR boundaries.
	CIDR string `json:"cidr,omitempty"`
}

// SubnetStatus represents the current state of a subnet
type SubnetStatus struct {
	// Name is the subnet identifier
	Name string `json:"name"`

	// CIDR is the calculated subnet in CIDR notation
	CIDR string `json:"cidr"`

	// BGPAdvertisement is the name of the managed CiliumBGPAdvertisement resource.
	// Only set when bgp.advertise is true for this subnet.
	// +optional
	BGPAdvertisement string `json:"bgpAdvertisement,omitempty"`
}

// PrefixHistoryEntry represents a historical prefix
type PrefixHistoryEntry struct {
	// Prefix is the historical prefix in CIDR notation
	Prefix string `json:"prefix"`

	// AcquiredAt is when this prefix was first acquired
	AcquiredAt metav1.Time `json:"acquiredAt"`

	// DeprecatedAt is when this prefix was replaced by a new one
	// +optional
	DeprecatedAt *metav1.Time `json:"deprecatedAt,omitempty"`

	// State indicates the current state of this historical prefix
	// +optional
	State PrefixState `json:"state,omitempty"`
}

// PrefixState indicates the state of a prefix
// +kubebuilder:validation:Enum=active;draining;expired
type PrefixState string

const (
	PrefixStateActive   PrefixState = "active"
	PrefixStateDraining PrefixState = "draining"
	PrefixStateExpired  PrefixState = "expired"
)

// Condition types for DynamicPrefix
const (
	// ConditionTypePrefixAcquired indicates whether a prefix has been acquired
	ConditionTypePrefixAcquired = "PrefixAcquired"

	// ConditionTypePoolsSynced indicates whether all referencing pools are synced
	ConditionTypePoolsSynced = "PoolsSynced"

	// ConditionTypeDegraded indicates the resource is in a degraded state
	ConditionTypeDegraded = "Degraded"

	// ConditionTypeBGPAdvertisementReady indicates whether BGP advertisements are configured
	ConditionTypeBGPAdvertisementReady = "BGPAdvertisementReady"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=dp;dprefix
// +kubebuilder:printcolumn:name="Prefix",type=string,JSONPath=`.status.currentPrefix`
// +kubebuilder:printcolumn:name="Source",type=string,JSONPath=`.status.prefixSource`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// DynamicPrefix is the Schema for the dynamicprefixes API.
// It represents a dynamically acquired IPv6 prefix that can be subdivided
// into subnets and used to populate supported pool backends and other resources.
type DynamicPrefix struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec defines the desired state of DynamicPrefix
	// +required
	Spec DynamicPrefixSpec `json:"spec"`

	// Status defines the observed state of DynamicPrefix
	// +optional
	Status DynamicPrefixStatus `json:"status,omitempty"`
}

// RequireGlobalUnicast reports whether acquired prefixes must be global unicast.
// An absent filter, or an absent field within it, means true: resources created
// before the field existed get the safe behaviour without needing to be edited.
func (dp *DynamicPrefix) RequireGlobalUnicast() bool {
	filter := dp.Spec.Acquisition.PrefixFilter
	if filter == nil || filter.RequireGlobalUnicast == nil {
		return true
	}
	return *filter.RequireGlobalUnicast
}

// +kubebuilder:object:root=true

// DynamicPrefixList contains a list of DynamicPrefix
type DynamicPrefixList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DynamicPrefix `json:"items"`
}

func init() {
	SchemeBuilder.Register(&DynamicPrefix{}, &DynamicPrefixList{})
}
