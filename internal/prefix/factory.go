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
	"fmt"
	"sync"

	dynamicprefixiov1alpha1 "github.com/pkizzle/dynamic-prefix-operator/api/v1alpha1"
)

// ReceiverFactory creates Receiver instances based on AcquisitionSpec.
type ReceiverFactory interface {
	// CreateReceiver creates a Receiver based on the given acquisition spec.
	CreateReceiver(spec dynamicprefixiov1alpha1.AcquisitionSpec) (Receiver, error)
}

// InterfaceBusyError reports a second DHCPv6-PD client asked for on an
// interface that already has one.
type InterfaceBusyError struct {
	Interface string
}

func (e *InterfaceBusyError) Error() string {
	return fmt.Sprintf(
		"interface %s already has a DHCPv6-PD client: two clients on one interface send the same DUID and IAID, "+
			"so each would overwrite the other's lease. Model one delegation as one DynamicPrefix and give it "+
			"several addressRanges or subnets instead", e.Interface)
}

// DefaultReceiverFactory is the default implementation of ReceiverFactory.
type DefaultReceiverFactory struct {
	mu            sync.Mutex
	raReceivers   *sharedRAReceiverPool
	newRAReceiver newReceiverFunc
	// onRAReject is handed to every Router Advertisement receiver this factory
	// builds, so drops on any interface reach one counter.
	onRAReject RejectionObserver
	// pdInterfaces records which interfaces already run a DHCPv6-PD client.
	// Router Advertisement receivers are shared per interface and policy, which
	// is safe because listening twice costs nothing; a DHCPv6 client holds a
	// lease, and two of them on one interface fight over it.
	pdInterfaces map[string]struct{}
}

// FactoryOption configures a DefaultReceiverFactory at construction.
type FactoryOption func(*DefaultReceiverFactory)

// WithRARejectionObserver reports every dropped Router Advertisement to obs.
// It exists so the drops can reach a metric without this package importing the
// controller's registry.
func WithRARejectionObserver(obs RejectionObserver) FactoryOption {
	return func(f *DefaultReceiverFactory) { f.onRAReject = obs }
}

// NewReceiverFactory creates a new DefaultReceiverFactory.
func NewReceiverFactory(opts ...FactoryOption) *DefaultReceiverFactory {
	f := &DefaultReceiverFactory{}
	for _, opt := range opts {
		opt(f)
	}
	f.newRAReceiver = func(iface string, policy RAPolicy) Receiver {
		return NewRAReceiverWithPolicy(iface, policy, f.onRAReject)
	}
	return f
}

// policyFromSpec resolves the source-agnostic acceptance policy. An absent
// filter, or an absent field within it, keeps the safe default, so specs
// written before a field existed do not have to be edited.
func policyFromSpec(spec dynamicprefixiov1alpha1.AcquisitionSpec) Policy {
	policy := DefaultPolicy()

	filter := spec.PrefixFilter
	if filter == nil {
		return policy
	}
	if filter.RequireGlobalUnicast != nil {
		policy.RequireGlobalUnicast = *filter.RequireGlobalUnicast
	}
	if filter.MinPrefixLength != nil {
		policy.MinPrefixLength = *filter.MinPrefixLength
	}
	if filter.MaxPrefixLength != nil {
		policy.MaxPrefixLength = *filter.MaxPrefixLength
	}
	return policy
}

// raPolicyFromSpec adds the Router-Advertisement-only rules. A trusted-router
// list that cannot be parsed fails receiver creation rather than being ignored:
// silently believing every router on a link that asked for an allowlist is the
// one outcome nobody configuring this wants.
func raPolicyFromSpec(spec dynamicprefixiov1alpha1.AcquisitionSpec) (RAPolicy, error) {
	policy := RAPolicy{Policy: policyFromSpec(spec)}
	if spec.RouterAdvertisement == nil {
		return policy, nil
	}

	routers, err := ParseTrustedRouters(spec.RouterAdvertisement.TrustedRouters)
	if err != nil {
		return RAPolicy{}, err
	}
	policy.TrustedRouters = routers
	return policy, nil
}

// CreateReceiver creates a Receiver based on the AcquisitionSpec.
// Decision logic:
// 1. If only DHCPv6PD configured → DHCPv6PDReceiver
// 2. If only RouterAdvertisement configured → RAReceiver
// 3. If both configured → CompositeReceiver (DHCPv6-PD primary, RA fallback)
func (f *DefaultReceiverFactory) CreateReceiver(spec dynamicprefixiov1alpha1.AcquisitionSpec) (Receiver, error) {
	hasDHCPv6 := spec.DHCPv6PD != nil
	hasRA := spec.RouterAdvertisement.RAEnabled()

	switch {
	case hasDHCPv6 && hasRA:
		// Both configured - use composite receiver
		return f.createCompositeReceiver(spec)
	case hasDHCPv6:
		// Only DHCPv6-PD configured
		return f.createDHCPv6PDReceiver(spec.DHCPv6PD, policyFromSpec(spec))
	case hasRA:
		// Only RA configured
		raPolicy, err := raPolicyFromSpec(spec)
		if err != nil {
			return nil, err
		}
		return f.createRAReceiver(spec.RouterAdvertisement, raPolicy)
	default:
		return nil, fmt.Errorf("no acquisition method configured")
	}
}

// claimPDInterface reserves an interface for one DHCPv6-PD client, returning
// the function that hands it back.
func (f *DefaultReceiverFactory) claimPDInterface(iface string) (func(), error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if _, taken := f.pdInterfaces[iface]; taken {
		return nil, &InterfaceBusyError{Interface: iface}
	}
	if f.pdInterfaces == nil {
		f.pdInterfaces = make(map[string]struct{})
	}
	f.pdInterfaces[iface] = struct{}{}

	return func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		delete(f.pdInterfaces, iface)
	}, nil
}

// createDHCPv6PDReceiver creates a DHCPv6-PD receiver from the spec.
func (f *DefaultReceiverFactory) createDHCPv6PDReceiver(spec *dynamicprefixiov1alpha1.DHCPv6PDSpec, policy Policy) (*DHCPv6PDReceiver, error) {
	if spec.Interface == "" {
		return nil, fmt.Errorf("DHCPv6-PD interface is required")
	}

	prefixLength := 56 // Default
	if spec.RequestedPrefixLength != nil {
		prefixLength = *spec.RequestedPrefixLength
	}

	release, err := f.claimPDInterface(spec.Interface)
	if err != nil {
		return nil, err
	}

	receiver := NewDHCPv6PDReceiverWithPolicy(spec.Interface, prefixLength, policy)
	receiver.releaseInterface = release
	return receiver, nil
}

// createRAReceiver creates a Router Advertisement receiver from the spec.
func (f *DefaultReceiverFactory) createRAReceiver(spec *dynamicprefixiov1alpha1.RouterAdvertisementSpec, policy RAPolicy) (Receiver, error) {
	if spec.Interface == "" {
		return nil, fmt.Errorf("router advertisement interface is required")
	}

	return f.sharedRAPool().subscribe(spec.Interface, policy), nil
}

// createCompositeReceiver creates a composite receiver with DHCPv6-PD as primary and RA as fallback.
func (f *DefaultReceiverFactory) createCompositeReceiver(spec dynamicprefixiov1alpha1.AcquisitionSpec) (*CompositeReceiver, error) {
	raPolicy, err := raPolicyFromSpec(spec)
	if err != nil {
		return nil, err
	}

	primary, err := f.createDHCPv6PDReceiver(spec.DHCPv6PD, raPolicy.Policy)
	if err != nil {
		return nil, fmt.Errorf("failed to create primary DHCPv6-PD receiver: %w", err)
	}

	fallback, err := f.createRAReceiver(spec.RouterAdvertisement, raPolicy)
	if err != nil {
		// The primary already claimed the interface, and nothing will ever stop
		// a receiver that was never returned to the caller. Left behind, the
		// claim would outlive the misconfiguration that caused it and report
		// the interface busy to the very resource that is being corrected.
		primary.releaseClaimedInterface()
		return nil, fmt.Errorf("failed to create fallback RA receiver: %w", err)
	}

	return NewCompositeReceiver(primary, fallback), nil
}

func (f *DefaultReceiverFactory) sharedRAPool() *sharedRAReceiverPool {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.newRAReceiver == nil {
		f.newRAReceiver = func(iface string, policy RAPolicy) Receiver {
			return NewRAReceiverWithPolicy(iface, policy, f.onRAReject)
		}
	}
	if f.raReceivers == nil {
		f.raReceivers = newSharedRAReceiverPool(f.newRAReceiver)
	}

	return f.raReceivers
}
