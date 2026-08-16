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
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pkizzle/dynamic-prefix-operator/internal/prefix"
)

// rejectingReceiver stands in for an RA receiver on a link where something is
// advertising that should not be.
type rejectingReceiver struct {
	*prefix.MockReceiver
	rejected atomic.Uint64
}

func (r *rejectingReceiver) RARejections() (uint64, string) {
	return r.rejected.Load(), "untrusted-source"
}

// Dropped advertisements are how a rogue router announces itself, and the
// receiver's counter is not somewhere anyone looks. They have to reach the
// resource -- but a flood is as plausible an attack on the event stream as on
// the log, so a link dropping thousands a second must still produce a bounded
// number of events.
func TestRejectedAdvertisementsAreReportedAndThenRateLimited(t *testing.T) {
	receiver := &rejectingReceiver{MockReceiver: prefix.NewMockReceiver(prefix.SourceRouterAdvertisement)}
	r, recorder, dp := newHealthTestReconciler(t, receiver)

	// Nothing dropped yet: nothing to say.
	reconcileDynamicPrefix(t, r, dp.Name)
	if drainForWarning(recorder, "Router Advertisement") {
		t.Fatal("a receiver that has dropped nothing raised a rejection event")
	}

	receiver.rejected.Store(7)
	reconcileDynamicPrefix(t, r, dp.Name)
	if !drainForWarning(recorder, "Dropped 7 Router Advertisement") {
		t.Error("the first drops were not reported")
	}

	// Still climbing, but within the interval: silence.
	receiver.rejected.Store(5000)
	reconcileDynamicPrefix(t, r, dp.Name)
	reconcileDynamicPrefix(t, r, dp.Name)
	if drainForWarning(recorder, "Router Advertisement") {
		t.Error("a rising count inside the reporting interval produced another event")
	}

	// Once the interval has passed, the total since the last report is named.
	r.receiversMu.Lock()
	previous := r.rejectionReports[dp.Name]
	previous.at = previous.at.Add(-2 * rejectionReportInterval)
	r.rejectionReports[dp.Name] = previous
	r.receiversMu.Unlock()

	// The report covers everything since the last one, including the drops the
	// interval suppressed -- that window is exactly what the reader missed.
	receiver.rejected.Store(5100)
	reconcileDynamicPrefix(t, r, dp.Name)
	if !drainForWarning(recorder, "Dropped 5093 Router Advertisement") {
		t.Error("the report after the interval did not cover the suppressed window")
	}
}

// A rebuilt receiver starts counting from zero. Comparing against the previous
// receiver's total would suppress every report the new one has to make.
func TestRejectionReportSurvivesAReceiverRebuild(t *testing.T) {
	receiver := &rejectingReceiver{MockReceiver: prefix.NewMockReceiver(prefix.SourceRouterAdvertisement)}
	r, recorder, dp := newHealthTestReconciler(t, receiver)

	receiver.rejected.Store(400)
	reconcileDynamicPrefix(t, r, dp.Name)
	if !drainForWarning(recorder, "Dropped 400") {
		t.Fatal("the first report did not arrive")
	}

	// The counter going backwards is a new receiver, not fewer drops.
	receiver.rejected.Store(3)
	r.receiversMu.Lock()
	previous := r.rejectionReports[dp.Name]
	previous.at = previous.at.Add(-2 * rejectionReportInterval)
	r.rejectionReports[dp.Name] = previous
	r.receiversMu.Unlock()

	reconcileDynamicPrefix(t, r, dp.Name)
	if !drainForWarning(recorder, "Dropped 3 Router Advertisement") {
		t.Error("a rebuilt receiver's drops went unreported")
	}
}

func TestRejectionReportIntervalIsSane(t *testing.T) {
	if rejectionReportInterval < time.Minute {
		t.Errorf("reporting interval = %v, want at least a minute so a flood cannot flood the events too",
			rejectionReportInterval)
	}
}

func TestRejectionMessageNamesTheReason(t *testing.T) {
	receiver := &rejectingReceiver{MockReceiver: prefix.NewMockReceiver(prefix.SourceRouterAdvertisement)}
	r, recorder, dp := newHealthTestReconciler(t, receiver)

	receiver.rejected.Store(1)
	reconcileDynamicPrefix(t, r, dp.Name)

	var message string
	for {
		select {
		case ev := <-recorder.Events:
			if strings.Contains(ev, eventReasonRouterAdvertisementsRejected) {
				message = ev
			}
			continue
		default:
		}
		break
	}
	if message == "" {
		t.Fatal("no rejection event was raised")
	}
	if !strings.Contains(message, "untrusted-source") {
		t.Errorf("event = %q, want it to name why the advertisements were dropped", message)
	}
}
