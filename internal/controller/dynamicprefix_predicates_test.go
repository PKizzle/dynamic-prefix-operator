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
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dynamicprefixiov1alpha1 "github.com/pkizzle/dynamic-prefix-operator/api/v1alpha1"
)

func TestDynamicPrefixAffectsDependents(t *testing.T) {
	base := func() *dynamicprefixiov1alpha1.DynamicPrefix {
		return &dynamicprefixiov1alpha1.DynamicPrefix{
			Spec: dynamicprefixiov1alpha1.DynamicPrefixSpec{
				Acquisition: dynamicprefixiov1alpha1.AcquisitionSpec{
					RouterAdvertisement: &dynamicprefixiov1alpha1.RouterAdvertisementSpec{Interface: "eth0", Enabled: true},
				},
				AddressRanges: []dynamicprefixiov1alpha1.AddressRangeSpec{
					{Name: "loadbalancers", Start: "::f000:0:0:0", End: "::ffff:ffff:ffff:ffff"},
				},
				Subnets: []dynamicprefixiov1alpha1.SubnetSpec{
					{Name: "services", Offset: 0, PrefixLength: 64},
				},
				Transition: &dynamicprefixiov1alpha1.TransitionSpec{Mode: dynamicprefixiov1alpha1.TransitionModeHA, MaxPrefixHistory: 2},
			},
			Status: dynamicprefixiov1alpha1.DynamicPrefixStatus{
				CurrentPrefix: "2001:db8::/64",
				AddressRanges: []dynamicprefixiov1alpha1.AddressRangeStatus{
					{Name: "loadbalancers", Start: "2001:db8::f000:0:0:0", End: "2001:db8::ffff:ffff:ffff:ffff", CIDR: "2001:db8::/64"},
				},
				Subnets: []dynamicprefixiov1alpha1.SubnetStatus{
					{Name: "services", CIDR: "2001:db8::/64"},
				},
			},
		}
	}

	tests := []struct {
		name   string
		mutate func(*dynamicprefixiov1alpha1.DynamicPrefix)
		want   bool
	}{
		{
			name: "lease expiry only ignored",
			mutate: func(dp *dynamicprefixiov1alpha1.DynamicPrefix) {
				expiresAt := metav1.NewTime(time.Now().Add(2 * time.Hour))
				dp.Status.LeaseExpiresAt = &expiresAt
			},
		},
		{
			name: "condition only ignored",
			mutate: func(dp *dynamicprefixiov1alpha1.DynamicPrefix) {
				dp.Status.Conditions = []metav1.Condition{{Type: dynamicprefixiov1alpha1.ConditionTypePrefixAcquired, Status: metav1.ConditionTrue, Reason: "PrefixAcquired", Message: "ok", LastTransitionTime: metav1.Now()}}
			},
		},
		{
			name: "bgp advertisement status only ignored",
			mutate: func(dp *dynamicprefixiov1alpha1.DynamicPrefix) {
				dp.Status.Subnets[0].BGPAdvertisement = "dp-home-services"
			},
		},
		{
			name: "current prefix change affects dependents",
			mutate: func(dp *dynamicprefixiov1alpha1.DynamicPrefix) {
				dp.Status.CurrentPrefix = "2001:db8:1::/64"
			},
			want: true,
		},
		{
			name: "address range change affects dependents",
			mutate: func(dp *dynamicprefixiov1alpha1.DynamicPrefix) {
				dp.Status.AddressRanges[0].Start = "2001:db8::f100:0:0:0"
			},
			want: true,
		},
		{
			name: "subnet cidr change affects dependents",
			mutate: func(dp *dynamicprefixiov1alpha1.DynamicPrefix) {
				dp.Status.Subnets[0].CIDR = "2001:db8:1::/64"
			},
			want: true,
		},
		{
			name: "history change affects dependents",
			mutate: func(dp *dynamicprefixiov1alpha1.DynamicPrefix) {
				dp.Status.History = []dynamicprefixiov1alpha1.PrefixHistoryEntry{{Prefix: "2001:db8:ffff::/64", State: dynamicprefixiov1alpha1.PrefixStateDraining}}
			},
			want: true,
		},
		{
			name: "spec change affects dependents",
			mutate: func(dp *dynamicprefixiov1alpha1.DynamicPrefix) {
				dp.Spec.Transition.MaxPrefixHistory = 3
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldDP := base()
			newDP := oldDP.DeepCopy()
			tt.mutate(newDP)

			if got := dynamicPrefixAffectsDependents(oldDP, newDP); got != tt.want {
				t.Fatalf("dynamicPrefixAffectsDependents() = %v, want %v", got, tt.want)
			}
		})
	}
}
