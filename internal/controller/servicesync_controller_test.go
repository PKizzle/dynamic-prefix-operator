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
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	dynamicprefixiov1alpha1 "github.com/jr42/dynamic-prefix-operator/api/v1alpha1"
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
			svc := &corev1.Service{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: serviceName, Namespace: serviceNS}, svc)).To(Succeed())

			annotations := svc.GetAnnotations()
			annotations[AnnotationCiliumIPs] = "198.51.100.10," + currentIP
			svc.SetAnnotations(annotations)
			Expect(k8sClient.Update(ctx, svc)).To(Succeed())

			reconciler := &ServiceSyncReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: serviceName, Namespace: serviceNS}})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: serviceName, Namespace: serviceNS}, svc)).To(Succeed())
			ipsAnnotation := svc.GetAnnotations()[AnnotationCiliumIPs]

			Expect(ipsAnnotation).To(HavePrefix("198.51.100.10,"))
			Expect(ipsAnnotation).To(ContainSubstring(currentIP))
			Expect(ipsAnnotation).To(ContainSubstring(historicalIP))
		})

		It("should preserve hostname in external-dns target annotation", func() {
			svc := &corev1.Service{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: serviceName, Namespace: serviceNS}, svc)).To(Succeed())

			annotations := svc.GetAnnotations()
			annotations[AnnotationCiliumIPs] = "198.51.100.10," + currentIP
			annotations[AnnotationExternalDNSTarget] = "example.com," + currentIP
			svc.SetAnnotations(annotations)
			Expect(k8sClient.Update(ctx, svc)).To(Succeed())

			reconciler := &ServiceSyncReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: serviceName, Namespace: serviceNS}})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: serviceName, Namespace: serviceNS}, svc)).To(Succeed())
			dnsTarget := svc.GetAnnotations()[AnnotationExternalDNSTarget]

			Expect(dnsTarget).To(HavePrefix("example.com,"))
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
		{name: "empty string", input: "", prefixes: managedPrefixes, expected: nil},
		{name: "managed IPv6 only", input: "2001:db8:1::1,2001:db8:2::2", prefixes: managedPrefixes, expected: nil},
		{name: "IPv4 only", input: "192.168.1.1,10.0.0.1", prefixes: managedPrefixes, expected: []string{"192.168.1.1", "10.0.0.1"}},
		{name: "dual-stack", input: "198.51.100.10,2001:db8:1::ffff:0:2", prefixes: managedPrefixes, expected: []string{"198.51.100.10"}},
		{name: "static IPv6 outside managed prefix", input: "fd00::1,2001:db8:1::1", prefixes: managedPrefixes, expected: []string{"fd00::1"}},
		{name: "mixed values", input: "198.51.100.10,fd00::1,2001:db8:1::ffff:0:2,2001:db8:2::ffff:0:2", prefixes: managedPrefixes, expected: []string{"198.51.100.10", "fd00::1"}},
		{name: "no managed prefixes", input: "192.168.1.1,2001:db8:1::1,fd00::1", prefixes: nil, expected: []string{"192.168.1.1", "2001:db8:1::1", "fd00::1"}},
		{name: "hostname preserved", input: "example.com,2001:db8:1::1", prefixes: managedPrefixes, expected: []string{"example.com"}},
		{name: "spaces around entries", input: " 198.51.100.10 , 2001:db8:1::1 ", prefixes: managedPrefixes, expected: []string{"198.51.100.10"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := extractUnmanagedIPs(tt.input, tt.prefixes)
			if len(result) != len(tt.expected) {
				t.Fatalf("extractUnmanagedIPs(%q) returned %d items, want %d: %v", tt.input, len(result), len(tt.expected), result)
			}
			for i, v := range result {
				if v != tt.expected[i] {
					t.Errorf("extractUnmanagedIPs(%q)[%d] = %q, want %q", tt.input, i, v, tt.expected[i])
				}
			}
		})
	}
}

func TestExtractUnmanagedIPs_MalformedInput(t *testing.T) {
	managed := []netip.Prefix{netip.MustParsePrefix("2001:db8::/32")}

	tests := []struct {
		name     string
		input    string
		expected []string
	}{
		{name: "multiple commas", input: ",,,", expected: nil},
		{name: "garbage entry preserved", input: "not-an-ip", expected: []string{"not-an-ip"}},
		{name: "garbage mixed with valid IPs", input: "192.168.1.1,garbage,2001:db8::1", expected: []string{"192.168.1.1", "garbage"}},
		{name: "CIDR notation preserved as-is", input: "10.0.0.0/24,2001:db8::1", expected: []string{"10.0.0.0/24"}},
		{name: "IP with port preserved", input: "192.168.1.1:8080", expected: []string{"192.168.1.1:8080"}},
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.constant != tt.expected {
				t.Errorf("%s = %q, want %q", tt.name, tt.constant, tt.expected)
			}
		})
	}
}
