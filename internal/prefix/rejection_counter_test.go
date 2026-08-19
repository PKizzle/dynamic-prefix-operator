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
	"net/netip"
	"testing"
	"time"

	"github.com/mdlayher/ndp"
)

// oversizedPrefixAdvertisement carries a single prefix that is longer than any
// sane bound, so the only thing that can reject it is the length check inside
// the prefix walk -- the path that used to move the metric without moving the
// counter.
func oversizedPrefixAdvertisement() *ndp.RouterAdvertisement {
	return &ndp.RouterAdvertisement{Options: []ndp.Option{
		&ndp.PrefixInformation{
			Prefix:                         netip.MustParseAddr("2001:db8::"),
			PrefixLength:                   120,
			OnLink:                         true,
			AutonomousAddressConfiguration: true,
			ValidLifetime:                  2 * time.Hour,
			PreferredLifetime:              time.Hour,
		},
	}}
}

// A rejection recorded while walking the prefix options must reach the counter
// that RARejections reports, not just the metric.
//
// It used to reach only the metric. reportRARejections returns early on a total
// of 0, so a link whose only fault was out-of-bounds prefix lengths raised the
// metric while the RouterAdvertisementsRejected event stayed silent forever --
// and when both kinds of rejection happened, the event paired a count from one
// path with a reason from the other.
func TestPrefixLengthRejectionCountsTowardsRARejections(t *testing.T) {
	var observed []string
	r := NewRAReceiverWithPolicy("eth0", RAPolicy{
		Policy: Policy{MaxPrefixLength: 64},
	}, func(_, reason string) {
		observed = append(observed, reason)
	})

	r.handleRouterAdvertisement(oversizedPrefixAdvertisement())

	total, lastReason := r.RARejections()
	if total != 1 {
		t.Errorf("RARejections() total = %d, want 1: a prefix rejected on length is still a rejection, "+
			"and the event that reports drops never fires while this reads 0", total)
	}
	if lastReason != rejectReasonPrefixLength {
		t.Errorf("RARejections() lastReason = %q, want %q", lastReason, rejectReasonPrefixLength)
	}
	if len(observed) != 1 || observed[0] != rejectReasonPrefixLength {
		t.Errorf("observer saw %v, want exactly one %q: the metric and the counter must move together",
			observed, rejectReasonPrefixLength)
	}
	if got := r.CurrentPrefix(); got != nil {
		t.Errorf("CurrentPrefix() = %v, want nil: the only advertised prefix was out of bounds", got.Network)
	}
}
