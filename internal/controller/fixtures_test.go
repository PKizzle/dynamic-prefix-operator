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

// Fixture values shared by the tests in this package.
const (
	// Keys of an unstructured object.
	keyAPIVersion  = "apiVersion"
	keyKind        = "kind"
	keyMetadata    = "metadata"
	keyName        = "name"
	keyAnnotations = "annotations"

	// Pool spec fields the backends edit.
	fieldBlocks        = "blocks"
	fieldExternalCIDRs = "externalCIDRs"
	fieldCIDR          = "cidr"
	fieldStart         = "start"
	fieldStop          = "stop"

	// API versions served in the discovery fixtures.
	testCiliumV2            = "cilium.io/v2"
	testCiliumV2alpha1      = "cilium.io/v2alpha1"
	testAppsV1              = "apps/v1"
	testKindDeployment      = "Deployment"
	testResourceDeployments = "deployments"

	// Names used across the fixtures.
	testInterface                = "eth0"
	testNamespace                = "default"
	testNamespaceNetwork         = "network"
	testServiceName              = "svc"
	testServiceWeb               = "web"
	testMyDP                     = "my-dp"
	testName                     = "test"
	testDPName                   = "test-dp"
	testDPNoBGP                  = "test-dp-no-bgp"
	testDPOrphan                 = "test-dp-orphan"
	testDPSelector               = "test-dp-selector"
	testDPStatus                 = "test-dp-status"
	testSubnetLoadBalancers      = "loadbalancers"
	testSubnetServices           = "services"
	testSubnetNoBGP              = "no-bgp"
	testSubnetWasBGP             = "was-bgp"
	testSubnetWithSelector       = "with-selector"
	testOrphanAdvertisement      = "dp-test-dp-orphan-was-bgp"
	testLBPoolName               = "lb-pool"
	testLBPoolKey                = "/lb-pool"
	testCalicoPoolName           = "calico-pool"
	testCIDRGroupName            = "cidr-group"
	testPoolDefaultIPv6          = "default-ipv6"
	testPoolHome                 = "home"
	testBackendCiliumLBIPPool    = "cilium-load-balancer-ip-pool"
	testKubevipKey               = "cidr-global"
	testAnnotationNameKey        = "dynamic-prefix.io/name"
	testLabelApp                 = "app"
	testLabelAppValue            = "nginx"
	testAdvertisementTypeService = "Service"
	testBGPCommunity             = "65001:42"
	testCiliumVersion            = "1.20.0"
	testDNSTarget                = "example.com"
	testGarbage                  = "garbage"

	// Documentation-range prefixes and addresses (RFC 3849, RFC 5737).
	testPrefixLen48        = "2001:db8::/48"
	testPrefixLen64        = "2001:db8::/64"
	testPrefix1Len48       = "2001:db8:1::/48"
	testPrefix2Len48       = "2001:db8:2::/48"
	testPrefix2Len64       = "2001:db8:2::/64"
	testPrefix3Len48       = "2001:db8:3::/48"
	testPrefix3Len64       = "2001:db8:3::/64"
	testPrefixABCDLen48    = "2001:db8:abcd::/48"
	testPrefixABCDLen64    = "2001:db8:abcd::/64"
	testPrefixABCD100Len56 = "2001:db8:abcd:100::/56"
	testPrefixABCD200Len56 = "2001:db8:abcd:200::/56"
	testAddr               = "2001:db8::1"
	testAddr2              = "2001:db8::2"
	testAddr3              = "2001:db8::3"
	testAddrFF             = "2001:db8::ff"
	testPrefix1Addr        = "2001:db8:1::1"
	testULAPrefix          = "fd00::/64"
	testULAAddr            = "fd00::1"
	testIPv4Addr           = "10.0.0.1"
	testIPv4Private        = "192.168.1.1"
	testIPv4CIDR           = "192.0.2.0/24"
	testIPv4Assigned       = "192.0.2.1"
	testIPv4Other          = "198.51.100.10"
	testSuffixOne          = "::1"
	testSuffixBracketed    = "[::1]"
	testSuffixRangeStart   = "::f000:0:0:1"
)
