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
	"k8s.io/apimachinery/pkg/api/equality"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	dynamicprefixiov1alpha1 "github.com/pkizzle/dynamic-prefix-operator/api/v1alpha1"
)

type dependentSubnetStatus struct {
	Name string
	CIDR string
}

type dependentDynamicPrefixStatus struct {
	CurrentPrefix string
	AddressRanges []dynamicprefixiov1alpha1.AddressRangeStatus
	Subnets       []dependentSubnetStatus
	History       []dynamicprefixiov1alpha1.PrefixHistoryEntry
}

// dynamicPrefixDependentChangePredicate returns true only when a DynamicPrefix
// change can alter dependent pool, Service, or BGP resources. Lease expiry and
// condition-only status updates are intentionally ignored to avoid fanning out
// no-op reconciles on every prefix renewal or health condition refresh.
func dynamicPrefixDependentChangePredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(event.CreateEvent) bool { return true },
		DeleteFunc: func(event.DeleteEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldDP, oldOK := e.ObjectOld.(*dynamicprefixiov1alpha1.DynamicPrefix)
			newDP, newOK := e.ObjectNew.(*dynamicprefixiov1alpha1.DynamicPrefix)
			if !oldOK || !newOK {
				return true
			}
			return dynamicPrefixAffectsDependents(oldDP, newDP)
		},
		GenericFunc: func(event.GenericEvent) bool { return true },
	}
}

func dynamicPrefixAffectsDependents(oldDP, newDP *dynamicprefixiov1alpha1.DynamicPrefix) bool {
	if !equality.Semantic.DeepEqual(oldDP.Spec, newDP.Spec) {
		return true
	}

	return !equality.Semantic.DeepEqual(
		dependentStatus(oldDP.Status),
		dependentStatus(newDP.Status),
	)
}

func dependentStatus(status dynamicprefixiov1alpha1.DynamicPrefixStatus) dependentDynamicPrefixStatus {
	subnets := make([]dependentSubnetStatus, 0, len(status.Subnets))
	for _, subnet := range status.Subnets {
		subnets = append(subnets, dependentSubnetStatus{
			Name: subnet.Name,
			CIDR: subnet.CIDR,
		})
	}

	return dependentDynamicPrefixStatus{
		CurrentPrefix: status.CurrentPrefix,
		AddressRanges: status.AddressRanges,
		Subnets:       subnets,
		History:       status.History,
	}
}
