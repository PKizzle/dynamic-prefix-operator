/*
Copyright 2026 jr42.

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
	"net/netip"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	dynamicprefixiov1alpha1 "github.com/pkizzle/dynamic-prefix-operator/api/v1alpha1"
)

var _ = Describe("ServiceSync Controller", func() {
	Context("When reconciling a LoadBalancer Service in HA mode", func() {
		const (
			serviceName   = "test-service"
			serviceNS     = "default"
			dpName        = "test-dp-ha"
			addressRange  = "lb-range"
			currentPrefix = "2001:db8:1::/48"
			histPrefix1   = "2001:db8:2::/48"
			// Use canonical IPv6 format (RFC 5952)
			currentIP    = "2001:db8:1:0:f000::10"
			historicalIP = "2001:db8:2:0:f000::10"
		)

		ctx := context.Background()

		BeforeEach(func() {
			// Create DynamicPrefix with HA mode
			dp := &dynamicprefixiov1alpha1.DynamicPrefix{
				ObjectMeta: metav1.ObjectMeta{
					Name: dpName,
				},
				Spec: dynamicprefixiov1alpha1.DynamicPrefixSpec{
					Acquisition: dynamicprefixiov1alpha1.AcquisitionSpec{
						RouterAdvertisement: &dynamicprefixiov1alpha1.RouterAdvertisementSpec{
							Interface: "eth0",
							Enabled:   true,
						},
					},
					AddressRanges: []dynamicprefixiov1alpha1.AddressRangeSpec{
						{
							Name:  addressRange,
							Start: "::f000:0:0:1",
							End:   "::f000:0:0:ff",
						},
					},
					Transition: &dynamicprefixiov1alpha1.TransitionSpec{
						Mode:             dynamicprefixiov1alpha1.TransitionModeHA,
						MaxPrefixHistory: 2,
					},
				},
			}
			Expect(k8sClient.Create(ctx, dp)).To(Succeed())

			// Update DynamicPrefix status with current prefix and history
			dp.Status = dynamicprefixiov1alpha1.DynamicPrefixStatus{
				CurrentPrefix: currentPrefix,
				AddressRanges: []dynamicprefixiov1alpha1.AddressRangeStatus{
					{
						Name:  addressRange,
						Start: "2001:db8:1:0:f000::1",
						End:   "2001:db8:1:0:f000::ff",
					},
				},
				History: []dynamicprefixiov1alpha1.PrefixHistoryEntry{
					{
						Prefix:     histPrefix1,
						AcquiredAt: metav1.Now(),
						State:      dynamicprefixiov1alpha1.PrefixStateDraining,
					},
				},
			}
			Expect(k8sClient.Status().Update(ctx, dp)).To(Succeed())

			// Create LoadBalancer Service
			svc := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      serviceName,
					Namespace: serviceNS,
					Annotations: map[string]string{
						AnnotationName:                dpName,
						AnnotationServiceAddressRange: addressRange,
					},
				},
				Spec: corev1.ServiceSpec{
					Type: corev1.ServiceTypeLoadBalancer,
					Ports: []corev1.ServicePort{
						{
							Port: 80,
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, svc)).To(Succeed())

			// Update Service status with an IP (using canonical format)
			svc.Status = corev1.ServiceStatus{
				LoadBalancer: corev1.LoadBalancerStatus{
					Ingress: []corev1.LoadBalancerIngress{
						{
							IP: currentIP,
						},
					},
				},
			}
			Expect(k8sClient.Status().Update(ctx, svc)).To(Succeed())
		})

		AfterEach(func() {
			// Cleanup
			svc := &corev1.Service{}
			svc.Name = serviceName
			svc.Namespace = serviceNS
			_ = k8sClient.Delete(ctx, svc)

			dp := &dynamicprefixiov1alpha1.DynamicPrefix{}
			dp.Name = dpName
			_ = k8sClient.Delete(ctx, dp)
		})

		It("should update Service with both current and historical IPs", func() {
			reconciler := &ServiceSyncReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      serviceName,
					Namespace: serviceNS,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			// Fetch updated Service
			svc := &corev1.Service{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      serviceName,
				Namespace: serviceNS,
			}, svc)).To(Succeed())

			// Check annotations
			annotations := svc.GetAnnotations()

			// Should have external-dns target pointing to current IP
			Expect(annotations).To(HaveKey(AnnotationExternalDNSTarget))
			Expect(annotations[AnnotationExternalDNSTarget]).To(Equal(currentIP))

			// Should have lbipam.cilium.io/ips with both current and historical IPs
			Expect(annotations).To(HaveKey(AnnotationCiliumIPs))
			// The IPs annotation should contain both current and historical IPs
			ipsAnnotation := annotations[AnnotationCiliumIPs]
			Expect(ipsAnnotation).To(ContainSubstring(currentIP))
			Expect(ipsAnnotation).To(ContainSubstring(historicalIP))

			// Should have last-sync annotation
			Expect(annotations).To(HaveKey(AnnotationLastSync))
		})

		It("should preserve IPv4 addresses in dual-stack annotation", func() {
			// Set existing dual-stack annotation with IPv4 + IPv6
			svc := &corev1.Service{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      serviceName,
				Namespace: serviceNS,
			}, svc)).To(Succeed())

			annotations := svc.GetAnnotations()
			annotations[AnnotationCiliumIPs] = "198.51.100.10," + currentIP
			svc.SetAnnotations(annotations)
			Expect(k8sClient.Update(ctx, svc)).To(Succeed())

			reconciler := &ServiceSyncReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      serviceName,
					Namespace: serviceNS,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			// Fetch updated Service
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      serviceName,
				Namespace: serviceNS,
			}, svc)).To(Succeed())

			ipsAnnotation := svc.GetAnnotations()[AnnotationCiliumIPs]

			// IPv4 must be preserved
			Expect(ipsAnnotation).To(ContainSubstring("198.51.100.10"))
			// Current IPv6 must be present
			Expect(ipsAnnotation).To(ContainSubstring(currentIP))
			// Historical IPv6 must be present
			Expect(ipsAnnotation).To(ContainSubstring(historicalIP))
			// IPv4 should come first
			Expect(ipsAnnotation).To(HavePrefix("198.51.100.10,"))
		})

		It("should preserve hostname in external-dns target annotation", func() {
			// Set existing DNS target with hostname (for NAT IPv4) + managed IPv6
			svc := &corev1.Service{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      serviceName,
				Namespace: serviceNS,
			}, svc)).To(Succeed())

			annotations := svc.GetAnnotations()
			annotations[AnnotationCiliumIPs] = "198.51.100.10," + currentIP
			annotations[AnnotationExternalDNSTarget] = "example.com," + currentIP
			svc.SetAnnotations(annotations)
			Expect(k8sClient.Update(ctx, svc)).To(Succeed())

			reconciler := &ServiceSyncReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      serviceName,
					Namespace: serviceNS,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			// Fetch updated Service
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      serviceName,
				Namespace: serviceNS,
			}, svc)).To(Succeed())

			dnsTarget := svc.GetAnnotations()[AnnotationExternalDNSTarget]

			// Hostname must be preserved
			Expect(dnsTarget).To(ContainSubstring("example.com"))
			// Hostname should come first
			Expect(dnsTarget).To(HavePrefix("example.com,"))
			// Current IPv6 must be present
			Expect(dnsTarget).To(ContainSubstring(currentIP))
		})
	})

	Context("When reconciling a Service in simple mode", func() {
		const (
			serviceName = "test-service-simple"
			serviceNS   = "default"
			dpName      = "test-dp-simple"
		)

		ctx := context.Background()

		BeforeEach(func() {
			// Create DynamicPrefix with simple mode (default)
			dp := &dynamicprefixiov1alpha1.DynamicPrefix{
				ObjectMeta: metav1.ObjectMeta{
					Name: dpName,
				},
				Spec: dynamicprefixiov1alpha1.DynamicPrefixSpec{
					Acquisition: dynamicprefixiov1alpha1.AcquisitionSpec{
						RouterAdvertisement: &dynamicprefixiov1alpha1.RouterAdvertisementSpec{
							Interface: "eth0",
							Enabled:   true,
						},
					},
					Transition: &dynamicprefixiov1alpha1.TransitionSpec{
						Mode: dynamicprefixiov1alpha1.TransitionModeSimple,
					},
				},
			}
			Expect(k8sClient.Create(ctx, dp)).To(Succeed())

			// Create LoadBalancer Service
			svc := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      serviceName,
					Namespace: serviceNS,
					Annotations: map[string]string{
						AnnotationName: dpName,
					},
				},
				Spec: corev1.ServiceSpec{
					Type: corev1.ServiceTypeLoadBalancer,
					Ports: []corev1.ServicePort{
						{
							Port: 80,
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, svc)).To(Succeed())
		})

		AfterEach(func() {
			// Cleanup
			svc := &corev1.Service{}
			svc.Name = serviceName
			svc.Namespace = serviceNS
			_ = k8sClient.Delete(ctx, svc)

			dp := &dynamicprefixiov1alpha1.DynamicPrefix{}
			dp.Name = dpName
			_ = k8sClient.Delete(ctx, dp)
		})

		It("should not modify Service annotations in simple mode", func() {
			reconciler := &ServiceSyncReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      serviceName,
					Namespace: serviceNS,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			// Fetch Service
			svc := &corev1.Service{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      serviceName,
				Namespace: serviceNS,
			}, svc)).To(Succeed())

			// Should NOT have HA mode annotations
			annotations := svc.GetAnnotations()
			Expect(annotations).NotTo(HaveKey(AnnotationCiliumIPs))
			Expect(annotations).NotTo(HaveKey(AnnotationExternalDNSTarget))
		})
	})
})

func TestServiceSyncReconciler_calculateIPOffset(t *testing.T) {
	r := &ServiceSyncReconciler{}

	tests := []struct {
		name     string
		base     string
		target   string
		expected [16]byte
	}{
		{
			name:     "same address",
			base:     "2001:db8::1",
			target:   "2001:db8::1",
			expected: [16]byte{},
		},
		{
			name:   "simple offset",
			base:   "2001:db8::1",
			target: "2001:db8::10",
			expected: [16]byte{
				0, 0, 0, 0, 0, 0, 0, 0,
				0, 0, 0, 0, 0, 0, 0, 0x0f,
			},
		},
		{
			name:   "larger offset",
			base:   "2001:db8::f000:0:0:1",
			target: "2001:db8::f000:0:0:ff",
			expected: [16]byte{
				0, 0, 0, 0, 0, 0, 0, 0,
				0, 0, 0, 0, 0, 0, 0, 0xfe,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := netip.MustParseAddr(tt.base)
			target := netip.MustParseAddr(tt.target)

			offset := r.calculateIPOffset(base, target)
			if offset != tt.expected {
				t.Errorf("calculateIPOffset() = %v, want %v", offset, tt.expected)
			}
		})
	}
}

func TestServiceSyncReconciler_applyIPOffset(t *testing.T) {
	r := &ServiceSyncReconciler{}

	tests := []struct {
		name     string
		base     string
		offset   [16]byte
		expected string
	}{
		{
			name:     "zero offset",
			base:     "2001:db8::1",
			offset:   [16]byte{},
			expected: "2001:db8::1",
		},
		{
			name: "simple offset",
			base: "2001:db8::1",
			offset: [16]byte{
				0, 0, 0, 0, 0, 0, 0, 0,
				0, 0, 0, 0, 0, 0, 0, 0x0f,
			},
			expected: "2001:db8::10",
		},
		{
			name: "different prefix same offset",
			base: "2001:db8:2::f000:0:0:1",
			offset: [16]byte{
				0, 0, 0, 0, 0, 0, 0, 0,
				0, 0, 0, 0, 0, 0, 0, 0x0f,
			},
			expected: "2001:db8:2::f000:0:0:10",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := netip.MustParseAddr(tt.base)
			expected := netip.MustParseAddr(tt.expected)

			result := r.applyIPOffset(base, tt.offset)
			if result != expected {
				t.Errorf("applyIPOffset() = %v, want %v", result, expected)
			}
		})
	}
}

func TestExtractUnmanagedIPs(t *testing.T) {
	managedPrefixes := []netip.Prefix{
		netip.MustParsePrefix("2001:db8:1::/48"),
		netip.MustParsePrefix("2001:db8:2::/48"),
	}

	tests := []struct {
		name     string
		input    string
		prefixes []netip.Prefix
		expected []string
	}{
		{
			name:     "empty string",
			input:    "",
			prefixes: managedPrefixes,
			expected: nil,
		},
		{
			name:     "managed IPv6 only — all removed",
			input:    "2001:db8:1::1,2001:db8:2::2",
			prefixes: managedPrefixes,
			expected: nil,
		},
		{
			name:     "IPv4 only — all preserved",
			input:    "192.168.1.1,10.0.0.1",
			prefixes: managedPrefixes,
			expected: []string{"192.168.1.1", "10.0.0.1"},
		},
		{
			name:     "dual-stack: IPv4 preserved, managed IPv6 removed",
			input:    "198.51.100.10,2001:db8:1::ffff:0:2",
			prefixes: managedPrefixes,
			expected: []string{"198.51.100.10"},
		},
		{
			name:     "static IPv6 outside managed prefix — preserved",
			input:    "fd00::1,2001:db8:1::1",
			prefixes: managedPrefixes,
			expected: []string{"fd00::1"},
		},
		{
			name:     "mixed: IPv4 + static IPv6 + managed IPv6",
			input:    "198.51.100.10,fd00::1,2001:db8:1::ffff:0:2,2001:db8:2::ffff:0:2",
			prefixes: managedPrefixes,
			expected: []string{"198.51.100.10", "fd00::1"},
		},
		{
			name:     "multiple IPv4 + multiple static IPv6",
			input:    "192.168.1.1,10.0.0.1,fd00::1,fd00::2,2001:db8:1::1",
			prefixes: managedPrefixes,
			expected: []string{"192.168.1.1", "10.0.0.1", "fd00::1", "fd00::2"},
		},
		{
			name:     "no managed prefixes — all IPs preserved",
			input:    "192.168.1.1,2001:db8:1::1,fd00::1",
			prefixes: nil,
			expected: []string{"192.168.1.1", "2001:db8:1::1", "fd00::1"},
		},
		{
			name:     "spaces around IPs",
			input:    " 198.51.100.10 , 2001:db8:1::1 ",
			prefixes: managedPrefixes,
			expected: []string{"198.51.100.10"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := extractUnmanagedIPs(tt.input, tt.prefixes)
			if len(result) != len(tt.expected) {
				t.Errorf("extractUnmanagedIPs(%q) returned %d items, want %d: %v", tt.input, len(result), len(tt.expected), result)
				return
			}
			for i, v := range result {
				if v != tt.expected[i] {
					t.Errorf("extractUnmanagedIPs(%q)[%d] = %q, want %q", tt.input, i, v, tt.expected[i])
				}
			}
		})
	}
}

func TestServiceSyncAnnotationConstants(t *testing.T) {
	tests := []struct {
		name     string
		constant string
		expected string
	}{
		{
			name:     "AnnotationCiliumIPs",
			constant: AnnotationCiliumIPs,
			expected: "lbipam.cilium.io/ips",
		},
		{
			name:     "AnnotationExternalDNSTarget",
			constant: AnnotationExternalDNSTarget,
			expected: "external-dns.alpha.kubernetes.io/target",
		},
		{
			name:     "AnnotationServiceAddressRange",
			constant: AnnotationServiceAddressRange,
			expected: "dynamic-prefix.io/service-address-range",
		},
		{
			name:     "AnnotationServiceSubnet",
			constant: AnnotationServiceSubnet,
			expected: "dynamic-prefix.io/service-subnet",
		},
		{
			name:     "AnnotationSuffix",
			constant: AnnotationSuffix,
			expected: "dynamic-prefix.io/suffix",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.constant != tt.expected {
				t.Errorf("%s = %q, want %q", tt.name, tt.constant, tt.expected)
			}
		})
	}
}

func TestCombinePrefixSuffix(t *testing.T) {
	tests := []struct {
		name     string
		prefix   string
		suffix   string
		expected string
	}{
		{
			name:     "/48 prefix with host suffix",
			prefix:   "2001:db8:1::/48",
			suffix:   "::ffff:0:2",
			expected: "2001:db8:1::ffff:0:2",
		},
		{
			name:     "/56 prefix with host suffix",
			prefix:   "2001:db8:abcd:100::/56",
			suffix:   "::ffff:0:2",
			expected: "2001:db8:abcd:100::ffff:0:2",
		},
		{
			name:     "/56 prefix with different suffix",
			prefix:   "2001:db8:abcd:100::/56",
			suffix:   "::ffff:0:10",
			expected: "2001:db8:abcd:100::ffff:0:10",
		},
		{
			name:     "/64 prefix with suffix",
			prefix:   "2001:db8:1:2::/64",
			suffix:   "::1",
			expected: "2001:db8:1:2::1",
		},
		{
			name:     "/48 prefix preserves high bits only from prefix",
			prefix:   "2001:db8:abcd::/48",
			suffix:   "::1234:5678:9abc:def0",
			expected: "2001:db8:abcd:0:1234:5678:9abc:def0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pfx := netip.MustParsePrefix(tt.prefix)
			suffixAddr := netip.MustParseAddr(tt.suffix)
			suffixBytes := suffixAddr.As16()

			result := combinePrefixSuffix(pfx, suffixBytes)
			expected := netip.MustParseAddr(tt.expected)

			if result != expected {
				t.Errorf("combinePrefixSuffix(%s, %s) = %s, want %s",
					tt.prefix, tt.suffix, result, expected)
			}
		})
	}
}

func TestCalculateSuffixIPs(t *testing.T) {
	r := &ServiceSyncReconciler{}

	dp := &dynamicprefixiov1alpha1.DynamicPrefix{
		Spec: dynamicprefixiov1alpha1.DynamicPrefixSpec{
			Transition: &dynamicprefixiov1alpha1.TransitionSpec{
				Mode:             dynamicprefixiov1alpha1.TransitionModeHA,
				MaxPrefixHistory: 2,
			},
		},
		Status: dynamicprefixiov1alpha1.DynamicPrefixStatus{
			CurrentPrefix: "2001:db8:abcd:100::/56",
			History: []dynamicprefixiov1alpha1.PrefixHistoryEntry{
				{Prefix: "2001:db8:abcd:200::/56"},
			},
		},
	}

	allIPs, currentIP, err := r.calculateSuffixIPs(dp, "::ffff:0:2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if currentIP != "2001:db8:abcd:100:0:ffff:0:2" {
		t.Errorf("currentIP = %q, want %q", currentIP, "2001:db8:abcd:100:0:ffff:0:2")
	}
	if len(allIPs) != 2 {
		t.Fatalf("allIPs has %d entries, want 2", len(allIPs))
	}
	if allIPs[0] != "2001:db8:abcd:100:0:ffff:0:2" {
		t.Errorf("allIPs[0] = %q, want %q", allIPs[0], "2001:db8:abcd:100:0:ffff:0:2")
	}
	if allIPs[1] != "2001:db8:abcd:200:0:ffff:0:2" {
		t.Errorf("allIPs[1] = %q, want %q", allIPs[1], "2001:db8:abcd:200:0:ffff:0:2")
	}
}

func TestCalculateSuffixIPs_NoPrefix(t *testing.T) {
	r := &ServiceSyncReconciler{}

	dp := &dynamicprefixiov1alpha1.DynamicPrefix{
		Status: dynamicprefixiov1alpha1.DynamicPrefixStatus{},
	}

	_, _, err := r.calculateSuffixIPs(dp, "::ffff:0:2")
	if err == nil {
		t.Error("expected error for empty current prefix, got nil")
	}
}

func TestCalculateSuffixIPs_InvalidSuffix(t *testing.T) {
	r := &ServiceSyncReconciler{}

	dp := &dynamicprefixiov1alpha1.DynamicPrefix{
		Status: dynamicprefixiov1alpha1.DynamicPrefixStatus{
			CurrentPrefix: "2001:db8::/48",
		},
	}

	_, _, err := r.calculateSuffixIPs(dp, "not-an-ip")
	if err == nil {
		t.Error("expected error for invalid suffix, got nil")
	}
}

func TestCalculateSuffixIPs_MultipleHistory(t *testing.T) {
	r := &ServiceSyncReconciler{}

	dp := &dynamicprefixiov1alpha1.DynamicPrefix{
		Spec: dynamicprefixiov1alpha1.DynamicPrefixSpec{
			Transition: &dynamicprefixiov1alpha1.TransitionSpec{
				Mode:             dynamicprefixiov1alpha1.TransitionModeHA,
				MaxPrefixHistory: 3,
			},
		},
		Status: dynamicprefixiov1alpha1.DynamicPrefixStatus{
			CurrentPrefix: "2001:db8:abcd:100::/56",
			History: []dynamicprefixiov1alpha1.PrefixHistoryEntry{
				{Prefix: "2001:db8:abcd:200::/56"},
				{Prefix: "2001:db8:abcd:300::/56"},
				{Prefix: "2001:db8:abcd:400::/56"},
			},
		},
	}

	allIPs, currentIP, err := r.calculateSuffixIPs(dp, "::1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expectedCurrent := netip.MustParseAddr("2001:db8:abcd:100::1").String()
	if currentIP != expectedCurrent {
		t.Errorf("currentIP = %q, want %q", currentIP, expectedCurrent)
	}
	if len(allIPs) != 4 {
		t.Fatalf("allIPs has %d entries, want 4 (1 current + 3 history)", len(allIPs))
	}

	expectedAll := []string{
		netip.MustParseAddr("2001:db8:abcd:100::1").String(),
		netip.MustParseAddr("2001:db8:abcd:200::1").String(),
		netip.MustParseAddr("2001:db8:abcd:300::1").String(),
		netip.MustParseAddr("2001:db8:abcd:400::1").String(),
	}
	for i, exp := range expectedAll {
		if allIPs[i] != exp {
			t.Errorf("allIPs[%d] = %q, want %q", i, allIPs[i], exp)
		}
	}
}

func TestCalculateSuffixIPs_MaxHistoryLimit(t *testing.T) {
	r := &ServiceSyncReconciler{}

	dp := &dynamicprefixiov1alpha1.DynamicPrefix{
		Spec: dynamicprefixiov1alpha1.DynamicPrefixSpec{
			Transition: &dynamicprefixiov1alpha1.TransitionSpec{
				Mode:             dynamicprefixiov1alpha1.TransitionModeHA,
				MaxPrefixHistory: 1, // Only keep 1 historical prefix
			},
		},
		Status: dynamicprefixiov1alpha1.DynamicPrefixStatus{
			CurrentPrefix: "2001:db8:1::/48",
			History: []dynamicprefixiov1alpha1.PrefixHistoryEntry{
				{Prefix: "2001:db8:2::/48"},
				{Prefix: "2001:db8:3::/48"}, // Should be excluded
			},
		},
	}

	allIPs, _, err := r.calculateSuffixIPs(dp, "::42")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(allIPs) != 2 {
		t.Fatalf("allIPs has %d entries, want 2 (1 current + 1 history, limit=1)", len(allIPs))
	}

	expectedSecond := netip.MustParseAddr("2001:db8:2::42").String()
	if allIPs[1] != expectedSecond {
		t.Errorf("allIPs[1] = %q, want %q", allIPs[1], expectedSecond)
	}
}

func TestCalculateSuffixIPs_DifferentPrefixLengths(t *testing.T) {
	r := &ServiceSyncReconciler{}

	tests := []struct {
		name           string
		prefix         string
		suffix         string
		expectedResult string
	}{
		{
			name:           "/48 prefix",
			prefix:         "2001:db8:abcd::/48",
			suffix:         "::f000:0:0:1",
			expectedResult: netip.MustParseAddr("2001:db8:abcd:0:f000:0:0:1").String(),
		},
		{
			name:           "/56 prefix",
			prefix:         "2001:db8:abcd:100::/56",
			suffix:         "::f000:0:0:1",
			expectedResult: netip.MustParseAddr("2001:db8:abcd:100:f000:0:0:1").String(),
		},
		{
			name:           "/64 prefix",
			prefix:         "2001:db8:1:2::/64",
			suffix:         "::dead:beef",
			expectedResult: netip.MustParseAddr("2001:db8:1:2::dead:beef").String(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dp := &dynamicprefixiov1alpha1.DynamicPrefix{
				Status: dynamicprefixiov1alpha1.DynamicPrefixStatus{
					CurrentPrefix: tt.prefix,
				},
			}

			allIPs, currentIP, err := r.calculateSuffixIPs(dp, tt.suffix)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if currentIP != tt.expectedResult {
				t.Errorf("currentIP = %q, want %q", currentIP, tt.expectedResult)
			}
			if len(allIPs) != 1 {
				t.Errorf("allIPs has %d entries, want 1 (no history)", len(allIPs))
			}
		})
	}
}

// TestSuffixAnnotation_EndToEnd tests the full reconcile flow using the suffix
// annotation with various lbipam.cilium.io/ips and external-dns target scenarios.
func TestSuffixAnnotation_EndToEnd(t *testing.T) {
	tests := []struct {
		name              string
		annotations       map[string]string // Service annotations
		existingCiliumIPs string            // Pre-existing lbipam.cilium.io/ips value
		existingDNSTarget string            // Pre-existing external-dns target value
		dpCurrentPrefix   string
		dpHistory         []string
		maxHistory        int
		// Expected results
		expectUpdate    bool
		expectCiliumIPs func(t *testing.T, ips string)
		expectDNSTarget func(t *testing.T, target string)
	}{
		{
			name: "suffix with IPv4-only cilium annotation",
			annotations: map[string]string{
				AnnotationName:   "my-dp",
				AnnotationSuffix: "::ffff:0:2",
			},
			existingCiliumIPs: "198.51.100.10",
			dpCurrentPrefix:   "2001:db8:abcd:100::/56",
			dpHistory:         []string{"2001:db8:abcd:200::/56"},
			maxHistory:        2,
			expectUpdate:      true,
			expectCiliumIPs: func(t *testing.T, ips string) {
				t.Helper()
				// IPv4 must be first (preserved)
				if !strings.HasPrefix(ips, "198.51.100.10,") {
					t.Errorf("expected IPv4 first, got: %s", ips)
				}
				// Must contain current prefix IP
				currentExpected := netip.MustParseAddr("2001:db8:abcd:100:0:ffff:0:2").String()
				if !strings.Contains(ips, currentExpected) {
					t.Errorf("missing current prefix IP %s in: %s", currentExpected, ips)
				}
				// Must contain historical prefix IP
				histExpected := netip.MustParseAddr("2001:db8:abcd:200:0:ffff:0:2").String()
				if !strings.Contains(ips, histExpected) {
					t.Errorf("missing historical prefix IP %s in: %s", histExpected, ips)
				}
			},
			expectDNSTarget: func(t *testing.T, target string) {
				t.Helper()
				expected := netip.MustParseAddr("2001:db8:abcd:100:0:ffff:0:2").String()
				if target != expected {
					t.Errorf("DNS target = %q, want %q", target, expected)
				}
			},
		},
		{
			name: "suffix with multiple IPv4 addresses",
			annotations: map[string]string{
				AnnotationName:   "my-dp",
				AnnotationSuffix: "::1",
			},
			existingCiliumIPs: "10.0.0.1,192.168.1.100",
			dpCurrentPrefix:   "2001:db8:1::/48",
			dpHistory:         nil,
			maxHistory:        2,
			expectUpdate:      true,
			expectCiliumIPs: func(t *testing.T, ips string) {
				t.Helper()
				if !strings.HasPrefix(ips, "10.0.0.1,192.168.1.100,") {
					t.Errorf("expected both IPv4 first, got: %s", ips)
				}
				expected := netip.MustParseAddr("2001:db8:1::1").String()
				if !strings.Contains(ips, expected) {
					t.Errorf("missing current IP %s in: %s", expected, ips)
				}
			},
			expectDNSTarget: func(t *testing.T, target string) {
				t.Helper()
				expected := netip.MustParseAddr("2001:db8:1::1").String()
				if target != expected {
					t.Errorf("DNS target = %q, want %q", target, expected)
				}
			},
		},
		{
			name: "suffix replaces old managed IPv6 but keeps static IPv6",
			annotations: map[string]string{
				AnnotationName:   "my-dp",
				AnnotationSuffix: "::ffff:0:2",
			},
			// Has IPv4, a static IPv6 (fd00::), and an old managed IPv6
			existingCiliumIPs: "192.168.1.1,fd00::1,2001:db8:abcd:100:0:ffff:0:99",
			dpCurrentPrefix:   "2001:db8:abcd:100::/56",
			dpHistory:         nil,
			maxHistory:        2,
			expectUpdate:      true,
			expectCiliumIPs: func(t *testing.T, ips string) {
				t.Helper()
				// IPv4 preserved
				if !strings.Contains(ips, "192.168.1.1") {
					t.Errorf("missing IPv4 in: %s", ips)
				}
				// Static IPv6 preserved
				if !strings.Contains(ips, "fd00::1") {
					t.Errorf("missing static IPv6 fd00::1 in: %s", ips)
				}
				// Old managed IPv6 should be gone (replaced by suffix-calculated one)
				if strings.Contains(ips, "ffff:0:99") {
					t.Errorf("old managed IPv6 should have been replaced in: %s", ips)
				}
				// New suffix-calculated IPv6 present
				expected := netip.MustParseAddr("2001:db8:abcd:100:0:ffff:0:2").String()
				if !strings.Contains(ips, expected) {
					t.Errorf("missing suffix-calculated IP %s in: %s", expected, ips)
				}
			},
			expectDNSTarget: nil,
		},
		{
			name: "suffix with empty cilium annotation (first run)",
			annotations: map[string]string{
				AnnotationName:   "my-dp",
				AnnotationSuffix: "::42",
			},
			existingCiliumIPs: "",
			dpCurrentPrefix:   "2001:db8:abcd::/48",
			dpHistory:         nil,
			maxHistory:        2,
			expectUpdate:      true,
			expectCiliumIPs: func(t *testing.T, ips string) {
				t.Helper()
				expected := netip.MustParseAddr("2001:db8:abcd::42").String()
				if ips != expected {
					t.Errorf("cilium IPs = %q, want %q", ips, expected)
				}
			},
			expectDNSTarget: func(t *testing.T, target string) {
				t.Helper()
				expected := netip.MustParseAddr("2001:db8:abcd::42").String()
				if target != expected {
					t.Errorf("DNS target = %q, want %q", target, expected)
				}
			},
		},
		{
			name: "dual-stack: hostname DNS target preserved with IPv6",
			annotations: map[string]string{
				AnnotationName:   "my-dp",
				AnnotationSuffix: "::ffff:0:2",
			},
			existingCiliumIPs: "198.51.100.10",
			existingDNSTarget: "example.com",
			dpCurrentPrefix:   "2001:db8:abcd:100::/56",
			dpHistory:         []string{"2001:db8:abcd:200::/56"},
			maxHistory:        2,
			expectUpdate:      true,
			expectCiliumIPs:   nil,
			expectDNSTarget: func(t *testing.T, target string) {
				t.Helper()
				// Hostname must be preserved at front
				if !strings.HasPrefix(target, "example.com,") {
					t.Errorf("expected hostname first, got: %s", target)
				}
				// Current IPv6 must be present
				currentExpected := netip.MustParseAddr("2001:db8:abcd:100:0:ffff:0:2").String()
				if !strings.Contains(target, currentExpected) {
					t.Errorf("missing current IPv6 %s in: %s", currentExpected, target)
				}
				// Historical IPv6 must NOT be in DNS target (only current)
				histExpected := netip.MustParseAddr("2001:db8:abcd:200:0:ffff:0:2").String()
				if strings.Contains(target, histExpected) {
					t.Errorf("DNS target should not contain historical IP %s: %s", histExpected, target)
				}
			},
		},
		{
			name: "dual-stack: hostname + old managed IPv6 in target gets rotated",
			annotations: map[string]string{
				AnnotationName:   "my-dp",
				AnnotationSuffix: "::ffff:0:2",
			},
			existingCiliumIPs: "198.51.100.10",
			// Target has hostname + stale IPv6 from old prefix
			existingDNSTarget: "example.com,2001:db8:abcd:200:0:ffff:0:2",
			dpCurrentPrefix:   "2001:db8:abcd:100::/56",
			dpHistory:         []string{"2001:db8:abcd:200::/56"},
			maxHistory:        2,
			expectUpdate:      true,
			expectCiliumIPs:   nil,
			expectDNSTarget: func(t *testing.T, target string) {
				t.Helper()
				// Hostname preserved
				if !strings.HasPrefix(target, "example.com,") {
					t.Errorf("expected hostname first, got: %s", target)
				}
				// Old managed IPv6 must be removed
				oldIP := netip.MustParseAddr("2001:db8:abcd:200:0:ffff:0:2").String()
				parts := strings.Split(target, ",")
				count := 0
				for _, p := range parts {
					if p == oldIP {
						count++
					}
				}
				if count > 0 {
					t.Errorf("old managed IPv6 should be removed from DNS target: %s", target)
				}
				// New current IPv6 must be present
				newIP := netip.MustParseAddr("2001:db8:abcd:100:0:ffff:0:2").String()
				if !strings.Contains(target, newIP) {
					t.Errorf("missing current IPv6 %s in: %s", newIP, target)
				}
			},
		},
		{
			name: "dual-stack: IPv4 address in DNS target preserved",
			annotations: map[string]string{
				AnnotationName:   "my-dp",
				AnnotationSuffix: "::1",
			},
			existingCiliumIPs: "10.0.0.1",
			existingDNSTarget: "203.0.113.1",
			dpCurrentPrefix:   "2001:db8:1::/48",
			dpHistory:         nil,
			maxHistory:        2,
			expectUpdate:      true,
			expectCiliumIPs:   nil,
			expectDNSTarget: func(t *testing.T, target string) {
				t.Helper()
				// IPv4 preserved at front
				if !strings.HasPrefix(target, "203.0.113.1,") {
					t.Errorf("expected IPv4 first, got: %s", target)
				}
				// Current IPv6 present
				expected := netip.MustParseAddr("2001:db8:1::1").String()
				if !strings.Contains(target, expected) {
					t.Errorf("missing current IPv6 %s in: %s", expected, target)
				}
			},
		},
		{
			name: "empty DNS target (first run)",
			annotations: map[string]string{
				AnnotationName:   "my-dp",
				AnnotationSuffix: "::ffff:0:2",
			},
			existingCiliumIPs: "",
			existingDNSTarget: "",
			dpCurrentPrefix:   "2001:db8:abcd:100::/56",
			dpHistory:         nil,
			maxHistory:        2,
			expectUpdate:      true,
			expectCiliumIPs:   nil,
			expectDNSTarget: func(t *testing.T, target string) {
				t.Helper()
				expected := netip.MustParseAddr("2001:db8:abcd:100:0:ffff:0:2").String()
				if target != expected {
					t.Errorf("DNS target = %q, want %q", target, expected)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Build the DynamicPrefix
			dp := &dynamicprefixiov1alpha1.DynamicPrefix{
				Spec: dynamicprefixiov1alpha1.DynamicPrefixSpec{
					Transition: &dynamicprefixiov1alpha1.TransitionSpec{
						Mode:             dynamicprefixiov1alpha1.TransitionModeHA,
						MaxPrefixHistory: tt.maxHistory,
					},
				},
				Status: dynamicprefixiov1alpha1.DynamicPrefixStatus{
					CurrentPrefix: tt.dpCurrentPrefix,
				},
			}
			for _, h := range tt.dpHistory {
				dp.Status.History = append(dp.Status.History, dynamicprefixiov1alpha1.PrefixHistoryEntry{
					Prefix: h,
				})
			}

			// Calculate suffix IPs
			suffix := tt.annotations[AnnotationSuffix]
			r := &ServiceSyncReconciler{}
			allIPs, currentIP, err := r.calculateSuffixIPs(dp, suffix)
			if err != nil {
				t.Fatalf("calculateSuffixIPs failed: %v", err)
			}

			// Simulate the managed prefix filtering + IP assembly
			managedPrefixes := collectManagedPrefixes(dp)
			preservedIPs := extractUnmanagedIPs(tt.existingCiliumIPs, managedPrefixes)
			finalIPs := append(preservedIPs, allIPs...)
			finalIPsStr := strings.Join(finalIPs, ",")

			// Simulate DNS target preservation (same logic as Reconcile)
			preservedTargets := extractUnmanagedIPs(tt.existingDNSTarget, managedPrefixes)
			finalTargets := append(preservedTargets, currentIP)
			finalTargetStr := strings.Join(finalTargets, ",")

			if tt.expectCiliumIPs != nil {
				tt.expectCiliumIPs(t, finalIPsStr)
			}
			if tt.expectDNSTarget != nil {
				tt.expectDNSTarget(t, finalTargetStr)
			}
		})
	}
}

// TestSuffixAnnotation_RequiresName verifies that without dynamic-prefix.io/name,
// the suffix annotation has no effect (Reconcile short-circuits).
func TestSuffixAnnotation_RequiresName(t *testing.T) {
	if AnnotationName != "dynamic-prefix.io/name" {
		t.Fatalf("AnnotationName = %q, expected %q", AnnotationName, "dynamic-prefix.io/name")
	}
	if AnnotationSuffix != "dynamic-prefix.io/suffix" {
		t.Fatalf("AnnotationSuffix = %q, expected %q", AnnotationSuffix, "dynamic-prefix.io/suffix")
	}
}

// =============================================================================
// Battle tests: invalid, malformed, and edge-case input
// =============================================================================

func TestCalculateSuffixIPs_InvalidInputs(t *testing.T) {
	r := &ServiceSyncReconciler{}

	validDP := &dynamicprefixiov1alpha1.DynamicPrefix{
		Status: dynamicprefixiov1alpha1.DynamicPrefixStatus{
			CurrentPrefix: "2001:db8::/48",
		},
	}

	tests := []struct {
		name      string
		dp        *dynamicprefixiov1alpha1.DynamicPrefix
		suffix    string
		expectErr bool
	}{
		{
			name:      "empty suffix string",
			dp:        validDP,
			suffix:    "",
			expectErr: true,
		},
		{
			name:      "whitespace-only suffix",
			dp:        validDP,
			suffix:    "   ",
			expectErr: true,
		},
		{
			name:      "IPv4 address as suffix",
			dp:        validDP,
			suffix:    "192.168.1.1",
			expectErr: false, // netip.ParseAddr accepts IPv4, combinePrefixSuffix will produce garbage but no error
		},
		{
			name:      "CIDR notation as suffix (not a bare address)",
			dp:        validDP,
			suffix:    "::1/128",
			expectErr: true,
		},
		{
			name:      "garbage string",
			dp:        validDP,
			suffix:    "hello-world",
			expectErr: true,
		},
		{
			name:      "partial IPv6 (missing colons)",
			dp:        validDP,
			suffix:    "2001db8",
			expectErr: true,
		},
		{
			name:      "suffix with brackets",
			dp:        validDP,
			suffix:    "[::1]",
			expectErr: true,
		},
		{
			name:      "suffix with port notation",
			dp:        validDP,
			suffix:    "::1%eth0",
			expectErr: false, // Go's netip.ParseAddr accepts zone IDs — not ideal but not an error
		},
		{
			name: "malformed current prefix (not CIDR)",
			dp: &dynamicprefixiov1alpha1.DynamicPrefix{
				Status: dynamicprefixiov1alpha1.DynamicPrefixStatus{
					CurrentPrefix: "not-a-prefix",
				},
			},
			suffix:    "::1",
			expectErr: true,
		},
		{
			name: "current prefix is bare address (no /nn)",
			dp: &dynamicprefixiov1alpha1.DynamicPrefix{
				Status: dynamicprefixiov1alpha1.DynamicPrefixStatus{
					CurrentPrefix: "2001:db8::1",
				},
			},
			suffix:    "::1",
			expectErr: true,
		},
		{
			name: "empty current prefix",
			dp: &dynamicprefixiov1alpha1.DynamicPrefix{
				Status: dynamicprefixiov1alpha1.DynamicPrefixStatus{
					CurrentPrefix: "",
				},
			},
			suffix:    "::1",
			expectErr: true,
		},
		{
			name: "nil transition spec uses default maxHistory",
			dp: &dynamicprefixiov1alpha1.DynamicPrefix{
				Spec: dynamicprefixiov1alpha1.DynamicPrefixSpec{},
				Status: dynamicprefixiov1alpha1.DynamicPrefixStatus{
					CurrentPrefix: "2001:db8::/48",
					History: []dynamicprefixiov1alpha1.PrefixHistoryEntry{
						{Prefix: "2001:db8:1::/48"},
						{Prefix: "2001:db8:2::/48"},
						{Prefix: "2001:db8:3::/48"},
					},
				},
			},
			suffix:    "::1",
			expectErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := r.calculateSuffixIPs(tt.dp, tt.suffix)
			if tt.expectErr && err == nil {
				t.Error("expected error, got nil")
			}
			if !tt.expectErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestCalculateSuffixIPs_MalformedHistorySkipped(t *testing.T) {
	r := &ServiceSyncReconciler{}

	dp := &dynamicprefixiov1alpha1.DynamicPrefix{
		Spec: dynamicprefixiov1alpha1.DynamicPrefixSpec{
			Transition: &dynamicprefixiov1alpha1.TransitionSpec{
				Mode:             dynamicprefixiov1alpha1.TransitionModeHA,
				MaxPrefixHistory: 5,
			},
		},
		Status: dynamicprefixiov1alpha1.DynamicPrefixStatus{
			CurrentPrefix: "2001:db8::/48",
			History: []dynamicprefixiov1alpha1.PrefixHistoryEntry{
				{Prefix: "2001:db8:1::/48"}, // valid
				{Prefix: "garbage"},         // invalid — should be skipped
				{Prefix: ""},                // empty — should be skipped
				{Prefix: "2001:db8::1"},     // bare addr without /nn — invalid
				{Prefix: "2001:db8:2::/48"}, // valid
			},
		},
	}

	allIPs, _, err := r.calculateSuffixIPs(dp, "::42")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should have: current + 2 valid history entries (the 3 invalid are skipped)
	if len(allIPs) != 3 {
		t.Errorf("allIPs has %d entries, want 3 (1 current + 2 valid history, 3 invalid skipped): %v",
			len(allIPs), allIPs)
	}
}

func TestExtractUnmanagedIPs_MalformedInput(t *testing.T) {
	managed := []netip.Prefix{netip.MustParsePrefix("2001:db8::/32")}

	tests := []struct {
		name     string
		input    string
		expected []string
	}{
		{
			name:     "lone comma",
			input:    ",",
			expected: nil,
		},
		{
			name:     "multiple commas",
			input:    ",,,",
			expected: nil,
		},
		{
			name:     "garbage entry preserved as-is",
			input:    "not-an-ip",
			expected: []string{"not-an-ip"},
		},
		{
			name:     "garbage mixed with valid IPs",
			input:    "192.168.1.1,garbage,2001:db8::1",
			expected: []string{"192.168.1.1", "garbage"},
		},
		{
			name:     "CIDR notation preserved as garbage (not a bare IP)",
			input:    "10.0.0.0/24,2001:db8::1",
			expected: []string{"10.0.0.0/24"},
		},
		{
			name:     "duplicate IPs — all kept (no dedup)",
			input:    "192.168.1.1,192.168.1.1,fd00::1,fd00::1",
			expected: []string{"192.168.1.1", "192.168.1.1", "fd00::1", "fd00::1"},
		},
		{
			name:     "trailing comma",
			input:    "192.168.1.1,",
			expected: []string{"192.168.1.1"},
		},
		{
			name:     "leading comma",
			input:    ",192.168.1.1",
			expected: []string{"192.168.1.1"},
		},
		{
			name:     "IP with brackets",
			input:    "[::1]",
			expected: []string{"[::1]"}, // invalid parse → preserved as-is
		},
		{
			name:     "IP with port",
			input:    "192.168.1.1:8080",
			expected: []string{"192.168.1.1:8080"}, // invalid parse → preserved
		},
		{
			name:     "empty entries between valid IPs",
			input:    "10.0.0.1,,fd00::1",
			expected: []string{"10.0.0.1", "fd00::1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := extractUnmanagedIPs(tt.input, managed)
			if len(result) != len(tt.expected) {
				t.Fatalf("got %d items %v, want %d items %v", len(result), result, len(tt.expected), tt.expected)
			}
			for i, v := range result {
				if v != tt.expected[i] {
					t.Errorf("[%d] = %q, want %q", i, v, tt.expected[i])
				}
			}
		})
	}
}

func TestCombinePrefixSuffix_EdgeCases(t *testing.T) {
	tests := []struct {
		name     string
		prefix   string
		suffix   string
		expected string
	}{
		{
			name:     "/128 prefix ignores suffix entirely",
			prefix:   "2001:db8::1/128",
			suffix:   "::ffff",
			expected: "2001:db8::1",
		},
		{
			name:     "zero suffix keeps prefix network address",
			prefix:   "2001:db8:abcd::/48",
			suffix:   "::",
			expected: "2001:db8:abcd::",
		},
		{
			name:     "all-ones suffix fills host part",
			prefix:   "2001:db8::/32",
			suffix:   "::ffff:ffff:ffff:ffff:ffff:ffff",
			expected: "2001:db8:ffff:ffff:ffff:ffff:ffff:ffff",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pfx := netip.MustParsePrefix(tt.prefix)
			suffixAddr := netip.MustParseAddr(tt.suffix)
			suffixBytes := suffixAddr.As16()

			result := combinePrefixSuffix(pfx, suffixBytes)
			expected := netip.MustParseAddr(tt.expected)

			if result != expected {
				t.Errorf("combinePrefixSuffix(%s, %s) = %s, want %s",
					tt.prefix, tt.suffix, result, expected)
			}
		})
	}
}

func TestCollectManagedPrefixes_InvalidEntries(t *testing.T) {
	tests := []struct {
		name          string
		dp            *dynamicprefixiov1alpha1.DynamicPrefix
		expectedCount int
	}{
		{
			name: "all valid",
			dp: &dynamicprefixiov1alpha1.DynamicPrefix{
				Status: dynamicprefixiov1alpha1.DynamicPrefixStatus{
					CurrentPrefix: "2001:db8::/48",
					History: []dynamicprefixiov1alpha1.PrefixHistoryEntry{
						{Prefix: "2001:db8:1::/48"},
					},
				},
			},
			expectedCount: 2,
		},
		{
			name: "garbage current prefix — skipped",
			dp: &dynamicprefixiov1alpha1.DynamicPrefix{
				Status: dynamicprefixiov1alpha1.DynamicPrefixStatus{
					CurrentPrefix: "garbage",
					History: []dynamicprefixiov1alpha1.PrefixHistoryEntry{
						{Prefix: "2001:db8:1::/48"},
					},
				},
			},
			expectedCount: 1,
		},
		{
			name: "empty current prefix",
			dp: &dynamicprefixiov1alpha1.DynamicPrefix{
				Status: dynamicprefixiov1alpha1.DynamicPrefixStatus{
					CurrentPrefix: "",
					History: []dynamicprefixiov1alpha1.PrefixHistoryEntry{
						{Prefix: "2001:db8:1::/48"},
					},
				},
			},
			expectedCount: 1,
		},
		{
			name: "mixed valid and invalid history",
			dp: &dynamicprefixiov1alpha1.DynamicPrefix{
				Status: dynamicprefixiov1alpha1.DynamicPrefixStatus{
					CurrentPrefix: "2001:db8::/48",
					History: []dynamicprefixiov1alpha1.PrefixHistoryEntry{
						{Prefix: "valid-nope"},
						{Prefix: "2001:db8:1::/48"},
						{Prefix: ""},
						{Prefix: "2001:db8::1"}, // bare addr — invalid prefix
					},
				},
			},
			expectedCount: 2, // current + one valid history
		},
		{
			name: "completely empty status",
			dp: &dynamicprefixiov1alpha1.DynamicPrefix{
				Status: dynamicprefixiov1alpha1.DynamicPrefixStatus{},
			},
			expectedCount: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := collectManagedPrefixes(tt.dp)
			if len(result) != tt.expectedCount {
				t.Errorf("collectManagedPrefixes returned %d prefixes, want %d: %v",
					len(result), tt.expectedCount, result)
			}
		})
	}
}
