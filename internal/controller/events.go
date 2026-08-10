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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
)

const (
	eventReasonPrefixReceived         = "PrefixReceived"
	eventReasonPrefixChanged          = "PrefixChanged"
	eventReasonPoolUpdated            = "PoolUpdated"
	eventReasonPoolReleased           = "PoolReleased"
	eventReasonPoolSyncFailed         = "PoolSyncFailed"
	eventReasonServiceReleased        = "ServiceReleased"
	eventReasonTransitionStarted      = "TransitionStarted"
	eventReasonTransitionCompleted    = "TransitionCompleted"
	eventReasonReceiverCreationFailed = "ReceiverCreationFailed"
	eventReasonReceiverRebuilt        = "ReceiverRebuilt"
	eventReasonPrefixRejected         = "PrefixRejected"
)

// reasonPrefixRejected is the status-condition reason matching
// eventReasonPrefixRejected.
const reasonPrefixRejected = "PrefixRejected"

func emitNormalEvent(recorder events.EventRecorder, object runtime.Object, reason, message string) {
	emitEvent(recorder, object, corev1.EventTypeNormal, reason, message)
}

func emitWarningEvent(recorder events.EventRecorder, object runtime.Object, reason, message string) {
	emitEvent(recorder, object, corev1.EventTypeWarning, reason, message)
}

func emitEvent(recorder events.EventRecorder, object runtime.Object, eventType, reason, message string) {
	if recorder == nil {
		return
	}
	// events.k8s.io/v1 splits the old Reason into `reason` (why) and `action`
	// (what was done), and requires both. Every reason this operator emits is
	// already phrased as the action itself -- PrefixReceived, PoolUpdated,
	// TransitionStarted -- so reusing it keeps the two fields consistent
	// instead of inventing a second vocabulary that has to be kept in sync.
	//
	// `related` is nil: these events concern one object, with no secondary
	// object to point at.
	//
	// The message is passed as the format string with no arguments, matching
	// the previous Event() call; callers pre-format with fmt.Sprintf.
	recorder.Eventf(object, nil, eventType, reason, reason, "%s", message)
}
