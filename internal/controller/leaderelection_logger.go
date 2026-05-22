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
	"context"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

const (
	leaderElectionWaitingMessage  = "Leader election enabled; this replica will stay on standby until it acquires the leader lease"
	leaderElectionAcquiredMessage = "Leader lease acquired; controllers are now active on this replica"
	leaderElectionStoppedMessage  = "Manager stopped before acquiring the leader lease"
	leaderElectionDisabledMessage = "Leader election disabled; controllers are active on this replica"
)

// NewLeaderElectionStatusLogger creates a non-leader-gated manager runnable that
// explains whether the current replica is waiting on the leader lease or is
// already active because leader election is disabled.
func NewLeaderElectionStatusLogger(
	log logr.Logger,
	leaderElectionEnabled bool,
	leaderElectionID string,
	elected <-chan struct{},
) manager.Runnable {
	return &leaderElectionStatusLogger{
		log:                   log,
		leaderElectionEnabled: leaderElectionEnabled,
		leaderElectionID:      leaderElectionID,
		elected:               elected,
	}
}

type leaderElectionStatusLogger struct {
	log                   logr.Logger
	leaderElectionEnabled bool
	leaderElectionID      string
	elected               <-chan struct{}
	logFn                 func(msg string, keysAndValues ...any)
}

// NeedLeaderElection returns false so the standby/leader transition is logged
// even before this replica acquires the lease.
func (l *leaderElectionStatusLogger) NeedLeaderElection() bool {
	return false
}

// Start logs the standby/active state for the current replica and then blocks
// until the manager context is cancelled.
func (l *leaderElectionStatusLogger) Start(ctx context.Context) error {
	if !l.leaderElectionEnabled {
		l.info(leaderElectionDisabledMessage)
		<-ctx.Done()
		return nil
	}

	l.info(leaderElectionWaitingMessage, "leaderElectionID", l.leaderElectionID)

	select {
	case <-l.elected:
		l.info(leaderElectionAcquiredMessage, "leaderElectionID", l.leaderElectionID)
	case <-ctx.Done():
		l.info(leaderElectionStoppedMessage, "leaderElectionID", l.leaderElectionID)
		return nil
	}

	<-ctx.Done()
	return nil
}

func (l *leaderElectionStatusLogger) info(msg string, keysAndValues ...any) {
	if l.logFn != nil {
		l.logFn(msg, keysAndValues...)
		return
	}

	l.log.Info(msg, keysAndValues...)
}
