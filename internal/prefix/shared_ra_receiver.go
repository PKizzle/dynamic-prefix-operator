/*
Copyright 2026 jr42.

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

type newReceiverFunc func(iface string, requireGlobalUnicast bool) Receiver

type sharedRAReceiverPool struct {
	mu          sync.Mutex
	entries     map[string]*sharedRAReceiverEntry
	newReceiver newReceiverFunc
}

func newSharedRAReceiverPool(newReceiver newReceiverFunc) *sharedRAReceiverPool {
	return &sharedRAReceiverPool{
		entries:     make(map[string]*sharedRAReceiverEntry),
		newReceiver: newReceiver,
	}
}

// poolKey identifies a shareable receiver. Receivers are shared per interface,
// but the acceptance policy is baked into the receiver, so two DynamicPrefixes
// watching the same interface with different policies must not share one: the
// stricter subscriber would otherwise see prefixes it rejects. Keying on both
// gives them separate receivers, which is correct and rare enough not to matter.
func poolKey(iface string, requireGlobalUnicast bool) string {
	if requireGlobalUnicast {
		return iface + "|gua"
	}
	return iface + "|any"
}

func (p *sharedRAReceiverPool) subscribe(iface string, requireGlobalUnicast bool) Receiver {
	p.mu.Lock()
	defer p.mu.Unlock()

	key := poolKey(iface, requireGlobalUnicast)
	entry := p.entries[key]
	if entry == nil {
		entry = &sharedRAReceiverEntry{
			iface:       iface,
			key:         key,
			receiver:    p.newReceiver(iface, requireGlobalUnicast),
			subscribers: make(map[*sharedRAReceiverSubscription]struct{}),
		}
		p.entries[key] = entry
	}

	return &sharedRAReceiverSubscription{
		pool:   p,
		entry:  entry,
		iface:  iface,
		key:    key,
		events: make(chan Event, 10),
	}
}

func (p *sharedRAReceiverPool) unsubscribe(key string, entry *sharedRAReceiverEntry, sub *sharedRAReceiverSubscription) error {
	p.mu.Lock()
	// The entry may already have been replaced in the map: a caller holding a
	// pointer from an earlier subscribe can re-arm an entry that was stopped
	// and removed, which spawns a fresh fanOut goroutine and NDP listener.
	// Returning early for such an entry left both running for the life of the
	// process. Release it either way; only drop the map slot when this really
	// is the entry the map still points at, so a newer one is not evicted.
	current := p.entries[key] == entry

	shouldStop := entry.removeSubscriber(sub)
	if shouldStop && current {
		delete(p.entries, key)
	}
	p.mu.Unlock()

	if shouldStop {
		return entry.stop()
	}
	return nil
}

type sharedRAReceiverEntry struct {
	mu          sync.RWMutex
	iface       string
	key         string
	receiver    Receiver
	subscribers map[*sharedRAReceiverSubscription]struct{}
	started     bool
	ctx         context.Context
	cancel      context.CancelFunc
	stopCh      chan struct{}
}

func (e *sharedRAReceiverEntry) addSubscriber(ctx context.Context, sub *sharedRAReceiverSubscription) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if _, ok := e.subscribers[sub]; ok {
		return nil
	}

	if !e.started {
		e.ctx, e.cancel = context.WithCancel(ctx)
		e.stopCh = make(chan struct{})
		if err := e.receiver.Start(e.ctx); err != nil {
			e.cancel()
			e.ctx = nil
			e.cancel = nil
			e.stopCh = nil
			return err
		}
		e.started = true
		go e.fanOut(e.stopCh)
	}

	e.subscribers[sub] = struct{}{}
	return nil
}

func (e *sharedRAReceiverEntry) removeSubscriber(sub *sharedRAReceiverSubscription) bool {
	e.mu.Lock()
	defer e.mu.Unlock()

	delete(e.subscribers, sub)
	return len(e.subscribers) == 0 && e.started
}

func (e *sharedRAReceiverEntry) stop() error {
	e.mu.Lock()
	if !e.started {
		e.mu.Unlock()
		return nil
	}

	stopCh := e.stopCh
	cancel := e.cancel
	e.started = false
	e.ctx = nil
	e.cancel = nil
	e.stopCh = nil
	e.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if stopCh != nil {
		close(stopCh)
	}

	return e.receiver.Stop()
}

func (e *sharedRAReceiverEntry) fanOut(stopCh <-chan struct{}) {
	events := e.receiver.Events()
	for {
		select {
		case <-stopCh:
			return
		case event, ok := <-events:
			if !ok {
				return
			}
			e.broadcast(event)
		}
	}
}

func (e *sharedRAReceiverEntry) broadcast(event Event) {
	e.mu.RLock()
	subscribers := make([]*sharedRAReceiverSubscription, 0, len(e.subscribers))
	for sub := range e.subscribers {
		subscribers = append(subscribers, sub)
	}
	e.mu.RUnlock()

	for _, sub := range subscribers {
		sub.send(event)
	}
}

type sharedRAReceiverSubscription struct {
	mu      sync.Mutex
	pool    *sharedRAReceiverPool
	entry   *sharedRAReceiverEntry
	iface   string
	key     string
	events  chan Event
	started bool
}

func (s *sharedRAReceiverSubscription) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return nil
	}
	s.started = true
	s.mu.Unlock()

	if err := s.entry.addSubscriber(ctx, s); err != nil {
		s.mu.Lock()
		s.started = false
		s.mu.Unlock()
		return err
	}
	return nil
}

func (s *sharedRAReceiverSubscription) Stop() error {
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return nil
	}
	s.started = false
	s.mu.Unlock()

	return s.pool.unsubscribe(s.key, s.entry, s)
}

func (s *sharedRAReceiverSubscription) Events() <-chan Event {
	return s.events
}

func (s *sharedRAReceiverSubscription) CurrentPrefix() *Prefix {
	return s.entry.receiver.CurrentPrefix()
}

func (s *sharedRAReceiverSubscription) Source() Source {
	return s.entry.receiver.Source()
}

func (s *sharedRAReceiverSubscription) send(event Event) {
	select {
	case s.events <- event:
	default:
		// Slow subscribers should not block delivery to other DynamicPrefix resources.
	}
}
