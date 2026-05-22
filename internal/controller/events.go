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
	"k8s.io/client-go/tools/record"
)

const (
	eventReasonPrefixReceived         = "PrefixReceived"
	eventReasonPrefixChanged          = "PrefixChanged"
	eventReasonPoolUpdated            = "PoolUpdated"
	eventReasonTransitionStarted      = "TransitionStarted"
	eventReasonTransitionCompleted    = "TransitionCompleted"
	eventReasonReceiverCreationFailed = "ReceiverCreationFailed"
)

func emitNormalEvent(recorder record.EventRecorder, object runtime.Object, reason, message string) {
	emitEvent(recorder, object, corev1.EventTypeNormal, reason, message)
}

func emitWarningEvent(recorder record.EventRecorder, object runtime.Object, reason, message string) {
	emitEvent(recorder, object, corev1.EventTypeWarning, reason, message)
}

func emitEvent(recorder record.EventRecorder, object runtime.Object, eventType, reason, message string) {
	if recorder == nil {
		return
	}
	recorder.Event(object, eventType, reason, message)
}
