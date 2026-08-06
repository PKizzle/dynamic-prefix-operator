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

// DefaultReceiverFactory is the default implementation of ReceiverFactory.
type DefaultReceiverFactory struct {
	mu            sync.Mutex
	raReceivers   *sharedRAReceiverPool
	newRAReceiver newReceiverFunc
}

// NewReceiverFactory creates a new DefaultReceiverFactory.
func NewReceiverFactory() *DefaultReceiverFactory {
	return &DefaultReceiverFactory{
		newRAReceiver: func(iface string, requireGlobalUnicast bool) Receiver {
			return NewRAReceiverWithPolicy(iface, requireGlobalUnicast)
		},
	}
}

// requireGlobalUnicastFromSpec resolves the acceptance policy. An absent filter,
// or an absent field within it, means true, so specs written before the field
// existed keep the safe behaviour without being edited.
func requireGlobalUnicastFromSpec(spec dynamicprefixiov1alpha1.AcquisitionSpec) bool {
	if spec.PrefixFilter == nil || spec.PrefixFilter.RequireGlobalUnicast == nil {
		return true
	}
	return *spec.PrefixFilter.RequireGlobalUnicast
}

// CreateReceiver creates a Receiver based on the AcquisitionSpec.
// Decision logic:
// 1. If only DHCPv6PD configured → DHCPv6PDReceiver
// 2. If only RouterAdvertisement configured → RAReceiver
// 3. If both configured → CompositeReceiver (DHCPv6-PD primary, RA fallback)
func (f *DefaultReceiverFactory) CreateReceiver(spec dynamicprefixiov1alpha1.AcquisitionSpec) (Receiver, error) {
	hasDHCPv6 := spec.DHCPv6PD != nil
	hasRA := spec.RouterAdvertisement != nil && spec.RouterAdvertisement.Enabled

	requireGUA := requireGlobalUnicastFromSpec(spec)

	switch {
	case hasDHCPv6 && hasRA:
		// Both configured - use composite receiver
		return f.createCompositeReceiver(spec)
	case hasDHCPv6:
		// Only DHCPv6-PD configured
		return f.createDHCPv6PDReceiver(spec.DHCPv6PD, requireGUA)
	case hasRA:
		// Only RA configured
		return f.createRAReceiver(spec.RouterAdvertisement, requireGUA)
	default:
		return nil, fmt.Errorf("no acquisition method configured")
	}
}

// createDHCPv6PDReceiver creates a DHCPv6-PD receiver from the spec.
func (f *DefaultReceiverFactory) createDHCPv6PDReceiver(spec *dynamicprefixiov1alpha1.DHCPv6PDSpec, requireGlobalUnicast bool) (*DHCPv6PDReceiver, error) {
	if spec.Interface == "" {
		return nil, fmt.Errorf("DHCPv6-PD interface is required")
	}

	prefixLength := 56 // Default
	if spec.RequestedPrefixLength != nil {
		prefixLength = *spec.RequestedPrefixLength
	}

	return NewDHCPv6PDReceiverWithPolicy(spec.Interface, prefixLength, requireGlobalUnicast), nil
}

// createRAReceiver creates a Router Advertisement receiver from the spec.
func (f *DefaultReceiverFactory) createRAReceiver(spec *dynamicprefixiov1alpha1.RouterAdvertisementSpec, requireGlobalUnicast bool) (Receiver, error) {
	if spec.Interface == "" {
		return nil, fmt.Errorf("router advertisement interface is required")
	}

	return f.sharedRAPool().subscribe(spec.Interface, requireGlobalUnicast), nil
}

// createCompositeReceiver creates a composite receiver with DHCPv6-PD as primary and RA as fallback.
func (f *DefaultReceiverFactory) createCompositeReceiver(spec dynamicprefixiov1alpha1.AcquisitionSpec) (*CompositeReceiver, error) {
	requireGUA := requireGlobalUnicastFromSpec(spec)

	primary, err := f.createDHCPv6PDReceiver(spec.DHCPv6PD, requireGUA)
	if err != nil {
		return nil, fmt.Errorf("failed to create primary DHCPv6-PD receiver: %w", err)
	}

	fallback, err := f.createRAReceiver(spec.RouterAdvertisement, requireGUA)
	if err != nil {
		return nil, fmt.Errorf("failed to create fallback RA receiver: %w", err)
	}

	return NewCompositeReceiver(primary, fallback), nil
}

func (f *DefaultReceiverFactory) sharedRAPool() *sharedRAReceiverPool {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.newRAReceiver == nil {
		f.newRAReceiver = func(iface string, requireGlobalUnicast bool) Receiver {
			return NewRAReceiverWithPolicy(iface, requireGlobalUnicast)
		}
	}
	if f.raReceivers == nil {
		f.raReceivers = newSharedRAReceiverPool(f.newRAReceiver)
	}

	return f.raReceivers
}
