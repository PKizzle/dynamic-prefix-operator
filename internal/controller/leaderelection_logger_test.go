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
	"testing"
	"time"

	"github.com/go-logr/logr"
)

func TestLeaderElectionStatusLogger_LogsWaitingAndAcquired(t *testing.T) {
	elected := make(chan struct{})
	messages := make(chan string, 2)

	logger := &leaderElectionStatusLogger{
		log:                   logr.Discard(),
		leaderElectionEnabled: true,
		leaderElectionID:      "lease-id",
		elected:               elected,
		logFn: func(msg string, _ ...any) {
			messages <- msg
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- logger.Start(ctx)
	}()

	assertMessage(t, messages, leaderElectionWaitingMessage)
	close(elected)
	assertMessage(t, messages, leaderElectionAcquiredMessage)

	cancel()
	assertNilError(t, done)
}

func TestLeaderElectionStatusLogger_LogsDisabledMode(t *testing.T) {
	messages := make(chan string, 1)

	logger := &leaderElectionStatusLogger{
		log:                   logr.Discard(),
		leaderElectionEnabled: false,
		logFn: func(msg string, _ ...any) {
			messages <- msg
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- logger.Start(ctx)
	}()

	assertMessage(t, messages, leaderElectionDisabledMessage)

	cancel()
	assertNilError(t, done)
}

func TestLeaderElectionStatusLogger_LogsStopBeforeLeadership(t *testing.T) {
	messages := make(chan string, 2)

	logger := &leaderElectionStatusLogger{
		log:                   logr.Discard(),
		leaderElectionEnabled: true,
		leaderElectionID:      "lease-id",
		elected:               make(chan struct{}),
		logFn: func(msg string, _ ...any) {
			messages <- msg
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- logger.Start(ctx)
	}()

	assertMessage(t, messages, leaderElectionWaitingMessage)

	cancel()
	assertMessage(t, messages, leaderElectionStoppedMessage)
	assertNilError(t, done)
}

func assertMessage(t *testing.T, messages <-chan string, want string) {
	t.Helper()

	select {
	case got := <-messages:
		if got != want {
			t.Fatalf("message = %q, want %q", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for message %q", want)
	}
}

func assertNilError(t *testing.T, done <-chan error) {
	t.Helper()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start() error = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for logger to stop")
	}
}
