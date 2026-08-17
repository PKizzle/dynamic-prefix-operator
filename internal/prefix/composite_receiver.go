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
	"sync"
)

// CompositeReceiver runs DHCPv6-PD as primary with RA as fallback.
// It prefers the DHCPv6-PD prefix when available, switching to RA
// after consecutive DHCPv6-PD failures.
type CompositeReceiver struct {
	mu                  sync.RWMutex
	primary             Receiver // DHCPv6-PD
	fallback            Receiver // RA
	active              Receiver
	events              chan Event
	stopCh              chan struct{}
	started             bool
	consecutiveFailures int
	maxFailures         int
	ctx                 context.Context
	cancel              context.CancelFunc
}

// NewCompositeReceiver creates a new composite receiver with the given primary and fallback receivers.
func NewCompositeReceiver(primary, fallback Receiver) *CompositeReceiver {
	return &CompositeReceiver{
		primary:     primary,
		fallback:    fallback,
		active:      primary, // Start with primary
		events:      make(chan Event, 10),
		stopCh:      make(chan struct{}),
		maxFailures: 3, // Switch to fallback after 3 consecutive failures
	}
}

// Start begins both receivers and merges their events.
func (c *CompositeReceiver) Start(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.started {
		return nil
	}

	c.ctx, c.cancel = context.WithCancel(ctx)

	// Start primary receiver. Both failure paths must cancel: the parent is the
	// manager context, which lives for the whole process and retains every
	// child it is given, and a failing interface is retried every 30s -- so an
	// uncancelled attempt leaves a cancelCtx behind for good, once per retry.
	if err := c.primary.Start(c.ctx); err != nil {
		c.cancel()
		return err
	}

	// Start fallback receiver
	if err := c.fallback.Start(c.ctx); err != nil {
		_ = c.primary.Stop()
		c.cancel()
		return err
	}

	// Fresh stop channel per run; the constructor's is closed by Stop().
	c.stopCh = make(chan struct{})
	c.started = true

	// Hand this generation its own context and stop channel. Reading the
	// fields from inside the goroutine would race with the next Start, which
	// replaces both under the lock -- the same defect c03e9a7 fixed in the RA
	// receive loop, in the receiver that wraps it.
	go c.mergeEvents(c.ctx, c.stopCh)

	return nil
}

// Stop stops both receivers.
func (c *CompositeReceiver) Stop() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.started {
		// A composite that was built and never started still holds whatever its
		// receivers took at construction -- the DHCPv6-PD half claims its
		// interface there -- so the stop is forwarded regardless. Stopping a
		// receiver that never ran is a no-op beyond that release.
		_ = c.primary.Stop()
		_ = c.fallback.Stop()
		return nil
	}

	c.started = false
	if c.cancel != nil {
		c.cancel()
	}
	close(c.stopCh)

	// Stop both receivers
	var primaryErr, fallbackErr error
	primaryErr = c.primary.Stop()
	fallbackErr = c.fallback.Stop()

	if primaryErr != nil {
		return primaryErr
	}
	return fallbackErr
}

// Events returns the merged event channel.
func (c *CompositeReceiver) Events() <-chan Event {
	return c.events
}

// CurrentPrefix returns the current prefix from the active receiver.
func (c *CompositeReceiver) CurrentPrefix() *Prefix {
	c.mu.RLock()
	defer c.mu.RUnlock()

	// Prefer primary if it has a prefix
	if prefix := c.primary.CurrentPrefix(); prefix != nil {
		return prefix
	}
	return c.fallback.CurrentPrefix()
}

// LastError reports the primary's failure in preference to the fallback's. A
// composite serving an RA-derived prefix because DHCPv6-PD keeps failing is
// working, but not the way it was configured to, and the primary's error is
// what explains that.
func (c *CompositeReceiver) LastError() error {
	if h, ok := c.primary.(AcquisitionHealth); ok {
		if err := h.LastError(); err != nil {
			return err
		}
	}
	if h, ok := c.fallback.(AcquisitionHealth); ok {
		return h.LastError()
	}
	return nil
}

// RARejections reports the fallback's drops: the Router Advertisement half is
// the only one that validates advertisements.
func (c *CompositeReceiver) RARejections() (uint64, string) {
	if stats, ok := c.fallback.(RARejectionStats); ok {
		return stats.RARejections()
	}
	return 0, ""
}

// Source returns the source of the active receiver.
func (c *CompositeReceiver) Source() Source {
	c.mu.RLock()
	defer c.mu.RUnlock()

	// Return source based on which receiver has the current prefix
	if c.primary.CurrentPrefix() != nil {
		return c.primary.Source()
	}
	if c.fallback.CurrentPrefix() != nil {
		return c.fallback.Source()
	}
	return c.primary.Source() // Default to primary
}

// mergeEvents reads from both receivers' event channels and forwards to the
// composite channel until this generation's context or stop channel closes.
func (c *CompositeReceiver) mergeEvents(ctx context.Context, stopCh <-chan struct{}) {
	primaryEvents := c.primary.Events()
	fallbackEvents := c.fallback.Events()

	for {
		select {
		case <-stopCh:
			return
		case <-ctx.Done():
			return

		case event, ok := <-primaryEvents:
			if !ok {
				continue
			}
			c.handlePrimaryEvent(event)

		case event, ok := <-fallbackEvents:
			if !ok {
				continue
			}
			c.handleFallbackEvent(event)
		}
	}
}

// handlePrimaryEvent processes an event from the primary (DHCPv6-PD) receiver.
func (c *CompositeReceiver) handlePrimaryEvent(event Event) {
	c.mu.Lock()
	defer c.mu.Unlock()

	switch event.Type {
	case EventTypeFailed:
		c.consecutiveFailures++
		if c.consecutiveFailures >= c.maxFailures {
			// Switch to fallback
			c.active = c.fallback
			// If fallback has a prefix, emit it
			if fallbackPrefix := c.fallback.CurrentPrefix(); fallbackPrefix != nil {
				c.sendEvent(Event{Type: EventTypeAcquired, Prefix: fallbackPrefix})
			}
		}
		// Always forward the failure event
		c.sendEvent(event)

	case EventTypeAcquired, EventTypeRenewed, EventTypeChanged:
		// Primary succeeded, reset failure count
		c.consecutiveFailures = 0
		c.active = c.primary
		// Forward the event
		c.sendEvent(event)

	case EventTypeExpired:
		// Primary expired, switch to fallback if available
		if fallbackPrefix := c.fallback.CurrentPrefix(); fallbackPrefix != nil {
			c.active = c.fallback
			c.sendEvent(Event{Type: EventTypeAcquired, Prefix: fallbackPrefix})
		} else {
			c.sendEvent(event)
		}
	}
}

// handleFallbackEvent processes an event from the fallback (RA) receiver.
func (c *CompositeReceiver) handleFallbackEvent(event Event) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Only forward fallback events if we're using the fallback
	if c.active != c.fallback {
		// But track the prefix in case we need to switch
		return
	}

	// Forward the event
	c.sendEvent(event)
}

// sendEvent sends an event to the events channel (must be called with lock held).
func (c *CompositeReceiver) sendEvent(event Event) {
	select {
	case c.events <- event:
	default:
		// Channel full, event dropped
	}
}

// IsUsingFallback returns true if the composite receiver is currently using the fallback.
func (c *CompositeReceiver) IsUsingFallback() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.active == c.fallback
}
