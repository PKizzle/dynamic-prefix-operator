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

import "sync"

// healthTracker remembers why acquisition last failed.
//
// Receiver failures are deliberately not delivered to the reconciler: a down
// interface reports one per second and reconcile reads the receiver's prefix
// rather than its error, so every one of those wake-ups did nothing. That left
// the failures nowhere -- a resource whose interface does not exist, or whose
// DHCPv6 client cannot bind, waited for a prefix forever with the reason only
// in the operator's log. Recording the last one lets a reconcile ask.
type healthTracker struct {
	mu  sync.RWMutex
	err error
}

func (h *healthTracker) recordFailure(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.err = err
}

// recordSuccess clears the failure. Anything acquired means the path works,
// whatever it was doing before.
func (h *healthTracker) recordSuccess() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.err = nil
}

// LastError returns the most recent acquisition failure, or nil once
// acquisition has succeeded again.
func (h *healthTracker) LastError() error {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.err
}
