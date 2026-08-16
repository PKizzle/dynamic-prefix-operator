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
	"context"
	"net/netip"
	"time"
)

// Source indicates how a prefix was obtained
type Source string

const (
	SourceDHCPv6PD            Source = "dhcpv6-pd"
	SourceRouterAdvertisement Source = "router-advertisement"
	SourceStatic              Source = "static"
	SourceUnknown             Source = "unknown"
)

// Prefix represents an acquired IPv6 prefix with metadata
type Prefix struct {
	// Network is the IPv6 prefix
	Network netip.Prefix

	// ValidLifetime is how long this prefix is valid
	ValidLifetime time.Duration

	// PreferredLifetime is how long this prefix is preferred
	PreferredLifetime time.Duration

	// Source indicates how this prefix was obtained
	Source Source

	// ReceivedAt is when this prefix was received
	ReceivedAt time.Time
}

// Event represents a prefix-related event
type Event struct {
	// Type indicates what happened
	Type EventType

	// Prefix is the prefix involved (may be nil for some events)
	Prefix *Prefix

	// Error contains any error (for failure events)
	Error error
}

// EventType indicates the type of prefix event
type EventType string

const (
	EventTypeAcquired EventType = "acquired"
	EventTypeRenewed  EventType = "renewed"
	EventTypeChanged  EventType = "changed"
	EventTypeExpired  EventType = "expired"
	EventTypeFailed   EventType = "failed"
)

// AcquisitionHealth is implemented by receivers that can say why acquisition
// is failing. Reconcile asks rather than being told: failure events are not
// forwarded to it, because a down interface produces one every second and none
// of them carry anything reconcile acts on. Without this a resource that can
// never acquire -- no such interface, no server answering, no permission to
// bind -- is indistinguishable from one still waiting for its first
// advertisement.
type AcquisitionHealth interface {
	// LastError returns the most recent acquisition failure, or nil if the
	// last attempt succeeded.
	LastError() error
}

// RARejectionStats is implemented by receivers that validate Router
// Advertisements, so the drops can be reported on the resource. A rising count
// is the only outward sign that something on the link is advertising when it
// should not be.
type RARejectionStats interface {
	// RARejections returns how many advertisements have been dropped and the
	// most recent reason.
	RARejections() (total uint64, lastReason string)
}

// Receiver is the interface for prefix acquisition implementations
type Receiver interface {
	// Start begins receiving prefixes
	Start(ctx context.Context) error

	// Stop stops receiving prefixes
	Stop() error

	// Events returns a channel of prefix events
	Events() <-chan Event

	// CurrentPrefix returns the current prefix, if any
	CurrentPrefix() *Prefix

	// Source returns the type of this receiver
	Source() Source
}
