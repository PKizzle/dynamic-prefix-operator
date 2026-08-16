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
	"fmt"
	"time"

	dynamicprefixiov1alpha1 "github.com/pkizzle/dynamic-prefix-operator/api/v1alpha1"
	"github.com/pkizzle/dynamic-prefix-operator/internal/prefix"
)

// rejectionReportInterval is the shortest gap between two reports about one
// resource. A var so tests do not have to wait it out.
var rejectionReportInterval = 5 * time.Minute

// rejectionReport is what was last said about a resource's dropped
// advertisements.
type rejectionReport struct {
	total uint64
	at    time.Time
}

// reportRARejections raises an event when a resource's receiver has dropped
// advertisements since the last report.
//
// Dropped advertisements are the only outward sign of a link with something on
// it that should not be advertising -- a second router, a misconfigured host,
// or an attacker -- and the receiver's counter is not somewhere anyone looks.
// The report is bounded on both sides: nothing is said unless the count moved,
// and nothing more is said for an interval afterwards, because a flood is a
// plausible way to attack the event stream as much as the log.
func (r *DynamicPrefixReconciler) reportRARejections(dp *dynamicprefixiov1alpha1.DynamicPrefix, receiver prefix.Receiver) {
	stats, ok := receiver.(prefix.RARejectionStats)
	if !ok {
		return
	}

	total, lastReason := stats.RARejections()
	if total == 0 {
		return
	}

	now := time.Now()

	r.receiversMu.Lock()
	previous, seen := r.rejectionReports[dp.Name]
	switch {
	case total <= previous.total:
		// Nothing new. A count that went backwards means the receiver was
		// rebuilt, and the new one's drops are worth reporting from scratch.
		if total == previous.total {
			r.receiversMu.Unlock()
			return
		}
	case seen && now.Sub(previous.at) < rejectionReportInterval:
		r.receiversMu.Unlock()
		return
	}
	if r.rejectionReports == nil {
		r.rejectionReports = make(map[string]rejectionReport)
	}
	r.rejectionReports[dp.Name] = rejectionReport{total: total, at: now}
	r.receiversMu.Unlock()

	since := total - previous.total
	if total < previous.total {
		since = total
	}

	emitWarningEvent(r.Recorder, dp, eventReasonRouterAdvertisementsRejected,
		fmt.Sprintf("Dropped %d Router Advertisement(s) since the last report (%d in total; most recent reason: %s)",
			since, total, lastReason))
}
