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

// Fixture values shared by the tests in this package.
const (
	// Names used across the fixtures.
	testInterface           = "eth0"
	testName                = "test"
	testNameInvalid         = "invalid"
	testSubnetLoadBalancers = "loadbalancers"
	testSubnetServices      = "services"
	testNotAnAddress        = "not-an-address"
	testCaseGUA             = "GUA 2001:db8::1"
	testCaseLinkLocal       = "Link-local fe80::1"
	testCaseULA             = "ULA fd00::1"

	// Documentation-range prefixes and addresses (RFC 3849).
	testPrefixLen32        = "2001:db8::/32"
	testPrefixLen48        = "2001:db8::/48"
	testPrefixLen56        = "2001:db8::/56"
	testPrefixLen64        = "2001:db8::/64"
	testPrefixSubnet1Len64 = "2001:db8:0:1::/64"
	testAddr               = "2001:db8::1"
	testULAAddr            = "fd00::1"
	testRouterLinkLocal    = "fe80::1"
	testRangeBase          = "2001:db8:abcd:1::/64"
	testRangeBaseLast      = "2001:db8:abcd:1:ffff:ffff:ffff:ffff"
	testSuffixOne          = "::1"
	testRangeStartSuffix   = "::f000:0:0:0"
	testRangeEndSuffix     = "::ffff:ffff:ffff:ffff"
)
