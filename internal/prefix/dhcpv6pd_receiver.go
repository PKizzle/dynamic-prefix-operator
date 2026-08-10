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
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv6"
	"github.com/insomniacslk/dhcp/dhcpv6/nclient6"
	"github.com/insomniacslk/dhcp/iana"
)

// DHCPv6PDReceiver implements a DHCPv6 Prefix Delegation client.
// It actively requests prefix delegation from an upstream DHCPv6 server
// and handles lease renewals.
type DHCPv6PDReceiver struct {
	mu                    sync.RWMutex
	iface                 string
	requestedPrefixLength int
	currentPrefix         *Prefix
	lease                 *dhcpv6Lease
	events                chan Event
	stopCh                chan struct{}
	started               bool
	ctx                   context.Context
	cancel                context.CancelFunc
	requireGlobalUnicast  bool
}

// dhcpv6Lease contains DHCPv6-PD lease information.
type dhcpv6Lease struct {
	IAID              [4]byte
	Prefix            netip.Prefix
	T1                time.Duration
	T2                time.Duration
	ValidLifetime     time.Duration
	PreferredLifetime time.Duration
	ReceivedAt        time.Time
	ServerID          dhcpv6.DUID
}

// NewDHCPv6PDReceiver creates a new DHCPv6-PD receiver for the given interface.
// The requestedPrefixLength is a hint to the server (typically 48-64).
func NewDHCPv6PDReceiver(iface string, requestedPrefixLength int) *DHCPv6PDReceiver {
	return NewDHCPv6PDReceiverWithPolicy(iface, requestedPrefixLength, true)
}

// NewDHCPv6PDReceiverWithPolicy creates a DHCPv6-PD client with an explicit
// acceptance policy for the delegated prefix.
func NewDHCPv6PDReceiverWithPolicy(iface string, requestedPrefixLength int, requireGlobalUnicast bool) *DHCPv6PDReceiver {
	if requestedPrefixLength == 0 {
		requestedPrefixLength = 56 // Common default
	}
	return &DHCPv6PDReceiver{
		iface:                 iface,
		requestedPrefixLength: requestedPrefixLength,
		requireGlobalUnicast:  requireGlobalUnicast,
		events:                make(chan Event, 10),
		stopCh:                make(chan struct{}),
	}
}

// Start begins the DHCPv6-PD client, acquiring a prefix and managing renewals.
func (r *DHCPv6PDReceiver) Start(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.started {
		return nil
	}

	r.ctx, r.cancel = context.WithCancel(ctx)
	// Fresh stop channel per run: Stop() closes this one, so reusing the
	// constructor's channel made a restart spawn goroutines that exited
	// immediately and made the following Stop panic on a double close.
	r.stopCh = make(chan struct{})
	r.started = true

	// Hand this generation its own context and stop channel. Reading the
	// fields from inside the goroutine would race with the next Start,
	// which replaces both under the lock.
	go r.runLoop(r.ctx, r.stopCh)

	return nil
}

// Stop stops the DHCPv6-PD client.
func (r *DHCPv6PDReceiver) Stop() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.started {
		return nil
	}

	r.started = false
	if r.cancel != nil {
		r.cancel()
	}
	close(r.stopCh)

	return nil
}

// Events returns the channel of prefix events.
func (r *DHCPv6PDReceiver) Events() <-chan Event {
	return r.events
}

// CurrentPrefix returns the currently delegated prefix, if any.
func (r *DHCPv6PDReceiver) CurrentPrefix() *Prefix {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.currentPrefix
}

// Source returns SourceDHCPv6PD.
func (r *DHCPv6PDReceiver) Source() Source {
	return SourceDHCPv6PD
}

const (
	// acquireRetryInterval spaces out attempts while no lease is held.
	acquireRetryInterval = 10 * time.Second
	// renewRetryInterval spaces out RENEW attempts between T1 and T2. Without
	// it the loop reruns immediately -- the lease is unchanged, so the T1 test
	// still passes -- burning a core and opening a socket per iteration for the
	// whole T1..T2 window, which is typically hours.
	renewRetryInterval = 30 * time.Second
)

// waitFor blocks for d, reporting false if the receiver was stopped instead.
// Every wait in the loop goes through this so a Stop is not held up by a sleep.
func (r *DHCPv6PDReceiver) waitFor(ctx context.Context, stopCh <-chan struct{}, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-stopCh:
		return false
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// runLoop handles prefix acquisition and renewal.
func (r *DHCPv6PDReceiver) runLoop(ctx context.Context, stopCh <-chan struct{}) {
	// Initial acquisition
	if err := r.acquirePrefix(); err != nil {
		r.sendError(fmt.Errorf("initial prefix acquisition failed: %w", err))
	}

	for {
		select {
		case <-stopCh:
			return
		case <-ctx.Done():
			return
		default:
		}

		r.mu.RLock()
		lease := r.lease
		r.mu.RUnlock()

		if lease == nil {
			// No lease, try to acquire
			if !r.waitFor(ctx, stopCh, acquireRetryInterval) {
				return
			}
			if err := r.acquirePrefix(); err != nil {
				r.sendError(fmt.Errorf("prefix acquisition failed: %w", err))
			}
			continue
		}

		// Calculate when to renew
		now := time.Now()
		elapsed := now.Sub(lease.ReceivedAt)

		// Renew at T1 (typically 50% of valid lifetime)
		if elapsed >= lease.T1 {
			if err := r.renewPrefix(); err != nil {
				r.sendError(fmt.Errorf("prefix renewal failed: %w", err))
				// If T2 has passed, try rebind
				if elapsed >= lease.T2 {
					if err := r.rebindPrefix(); err != nil {
						r.sendError(fmt.Errorf("prefix rebind failed: %w", err))
						// Lease expired, clear and reacquire
						r.mu.Lock()
						r.currentPrefix = nil
						r.lease = nil
						r.mu.Unlock()
						r.sendEvent(EventTypeExpired, nil)
						// The lease is gone; the acquisition branch above owns
						// the retry pacing from here.
						continue
					}
				} else if !r.waitFor(ctx, stopCh, renewRetryInterval) {
					// Renewal failed before T2, so the lease still stands and
					// the next iteration would re-enter this branch at once.
					// Renewals fail exactly when the WAN interface is down,
					// which is also when those failures are fastest.
					return
				}
			}
			continue
		}

		// Sleep until T1
		sleepDuration := lease.T1 - elapsed
		if sleepDuration > time.Minute {
			sleepDuration = time.Minute // Wake up periodically to check for stop
		}

		select {
		case <-stopCh:
			return
		case <-ctx.Done():
			return
		case <-time.After(sleepDuration):
		}
	}
}

// acquirePrefix performs initial prefix acquisition using SOLICIT-ADVERTISE-REQUEST-REPLY.
func (r *DHCPv6PDReceiver) acquirePrefix() error {
	ifi, err := net.InterfaceByName(r.iface)
	if err != nil {
		return fmt.Errorf("failed to get interface %s: %w", r.iface, err)
	}

	// Create a new DHCPv6 client
	client, err := nclient6.New(r.iface)
	if err != nil {
		return fmt.Errorf("failed to create DHCPv6 client: %w", err)
	}
	defer func() { _ = client.Close() }()

	// Generate IAID from interface index. The IAID is four bytes of identity
	// rather than a number, so it is written as an explicit big-endian encoding.
	var iaid [4]byte
	binary.BigEndian.PutUint32(iaid[:], uint32(ifi.Index)) // #nosec G115 -- interface indexes are small positive integers

	// Create IA_PD option with prefix hint
	iaPD := &dhcpv6.OptIAPD{
		IaId: iaid,
		Options: dhcpv6.PDOptions{
			Options: dhcpv6.Options{
				&dhcpv6.OptIAPrefix{
					PreferredLifetime: 0,
					ValidLifetime:     0,
					Prefix: &net.IPNet{
						IP:   net.IPv6zero,
						Mask: net.CIDRMask(r.requestedPrefixLength, 128),
					},
				},
			},
		},
	}

	// Build SOLICIT message with IA_PD
	solicitMods := []dhcpv6.Modifier{
		dhcpv6.WithClientID(r.generateDUID(ifi)),
		dhcpv6.WithRequestedOptions(
			dhcpv6.OptionDNSRecursiveNameServer,
		),
	}

	// Perform 4-message exchange
	ctx, cancel := context.WithTimeout(r.ctx, 30*time.Second)
	defer cancel()

	// Custom SOLICIT with IA_PD
	solicit, err := dhcpv6.NewSolicit(ifi.HardwareAddr, solicitMods...)
	if err != nil {
		return fmt.Errorf("failed to create SOLICIT: %w", err)
	}
	solicit.AddOption(iaPD)

	// Send SOLICIT and receive ADVERTISE
	advertise, err := client.SendAndRead(ctx, nclient6.AllDHCPRelayAgentsAndServers, solicit, nclient6.IsMessageType(dhcpv6.MessageTypeAdvertise))
	if err != nil {
		return fmt.Errorf("failed to receive ADVERTISE: %w", err)
	}

	// Check for IA_PD in ADVERTISE
	advIAPD := advertise.GetOneOption(dhcpv6.OptionIAPD)
	if advIAPD == nil {
		return fmt.Errorf("ADVERTISE did not contain IA_PD")
	}

	// Get Server ID
	serverID := advertise.Options.ServerID()
	if serverID == nil {
		return fmt.Errorf("ADVERTISE did not contain Server ID")
	}

	// Build REQUEST message
	request, err := dhcpv6.NewRequestFromAdvertise(advertise)
	if err != nil {
		return fmt.Errorf("failed to create REQUEST: %w", err)
	}

	// Send REQUEST and receive REPLY
	reply, err := client.SendAndRead(ctx, nclient6.AllDHCPRelayAgentsAndServers, request, nclient6.IsMessageType(dhcpv6.MessageTypeReply))
	if err != nil {
		return fmt.Errorf("failed to receive REPLY: %w", err)
	}

	// Extract IA_PD from REPLY
	return r.processIAPDReply(reply, iaid, serverID)
}

// renewPrefix sends a RENEW message to extend the lease.
func (r *DHCPv6PDReceiver) renewPrefix() error {
	r.mu.RLock()
	lease := r.lease
	r.mu.RUnlock()

	if lease == nil {
		return fmt.Errorf("no lease to renew")
	}

	ifi, err := net.InterfaceByName(r.iface)
	if err != nil {
		return fmt.Errorf("failed to get interface %s: %w", r.iface, err)
	}

	client, err := nclient6.New(r.iface)
	if err != nil {
		return fmt.Errorf("failed to create DHCPv6 client: %w", err)
	}
	defer func() { _ = client.Close() }()

	// Build RENEW message
	renew, err := dhcpv6.NewMessage()
	if err != nil {
		return fmt.Errorf("failed to create RENEW message: %w", err)
	}
	renew.MessageType = dhcpv6.MessageTypeRenew

	renew.AddOption(dhcpv6.OptClientID(r.generateDUID(ifi)))
	renew.AddOption(dhcpv6.OptServerID(lease.ServerID))

	// Add current IA_PD
	ip := lease.Prefix.Addr().AsSlice()
	bits := lease.Prefix.Bits()
	iaPD := &dhcpv6.OptIAPD{
		IaId: lease.IAID,
		Options: dhcpv6.PDOptions{
			Options: dhcpv6.Options{
				&dhcpv6.OptIAPrefix{
					PreferredLifetime: lease.PreferredLifetime,
					ValidLifetime:     lease.ValidLifetime,
					Prefix: &net.IPNet{
						IP:   ip,
						Mask: net.CIDRMask(bits, 128),
					},
				},
			},
		},
	}
	renew.AddOption(iaPD)

	// Send RENEW and receive REPLY
	ctx, cancel := context.WithTimeout(r.ctx, 30*time.Second)
	defer cancel()

	reply, err := client.SendAndRead(ctx, nclient6.AllDHCPRelayAgentsAndServers, renew, nclient6.IsMessageType(dhcpv6.MessageTypeReply))
	if err != nil {
		return fmt.Errorf("failed to receive REPLY for RENEW: %w", err)
	}

	return r.processIAPDReply(reply, lease.IAID, lease.ServerID)
}

// rebindPrefix sends a REBIND message when the server is unreachable.
func (r *DHCPv6PDReceiver) rebindPrefix() error {
	r.mu.RLock()
	lease := r.lease
	r.mu.RUnlock()

	if lease == nil {
		return fmt.Errorf("no lease to rebind")
	}

	ifi, err := net.InterfaceByName(r.iface)
	if err != nil {
		return fmt.Errorf("failed to get interface %s: %w", r.iface, err)
	}

	client, err := nclient6.New(r.iface)
	if err != nil {
		return fmt.Errorf("failed to create DHCPv6 client: %w", err)
	}
	defer func() { _ = client.Close() }()

	// Build REBIND message (no server ID)
	rebind, err := dhcpv6.NewMessage()
	if err != nil {
		return fmt.Errorf("failed to create REBIND message: %w", err)
	}
	rebind.MessageType = dhcpv6.MessageTypeRebind

	rebind.AddOption(dhcpv6.OptClientID(r.generateDUID(ifi)))

	// Add current IA_PD
	ip := lease.Prefix.Addr().AsSlice()
	bits := lease.Prefix.Bits()
	iaPD := &dhcpv6.OptIAPD{
		IaId: lease.IAID,
		Options: dhcpv6.PDOptions{
			Options: dhcpv6.Options{
				&dhcpv6.OptIAPrefix{
					PreferredLifetime: lease.PreferredLifetime,
					ValidLifetime:     lease.ValidLifetime,
					Prefix: &net.IPNet{
						IP:   ip,
						Mask: net.CIDRMask(bits, 128),
					},
				},
			},
		},
	}
	rebind.AddOption(iaPD)

	// Send REBIND and receive REPLY
	ctx, cancel := context.WithTimeout(r.ctx, 30*time.Second)
	defer cancel()

	reply, err := client.SendAndRead(ctx, nclient6.AllDHCPRelayAgentsAndServers, rebind, nclient6.IsMessageType(dhcpv6.MessageTypeReply))
	if err != nil {
		return fmt.Errorf("failed to receive REPLY for REBIND: %w", err)
	}

	// Get new server ID from reply
	serverID := reply.Options.ServerID()
	if serverID == nil {
		return fmt.Errorf("REPLY did not contain Server ID")
	}

	return r.processIAPDReply(reply, lease.IAID, serverID)
}

// processIAPDReply extracts the delegated prefix from a DHCPv6 REPLY.
func (r *DHCPv6PDReceiver) processIAPDReply(reply *dhcpv6.Message, expectedIAID [4]byte, serverID dhcpv6.DUID) error {
	// Find IA_PD option
	var iaPD *dhcpv6.OptIAPD
	for _, opt := range reply.Options.Get(dhcpv6.OptionIAPD) {
		// Comma-ok, not a bare assertion: this is remote input being typed inside
		// a goroutine with no recover, so a decoder that ever hands back a
		// generic option here would take the process down rather than skip a
		// malformed reply.
		pd, ok := opt.(*dhcpv6.OptIAPD)
		if !ok {
			continue
		}
		if pd.IaId == expectedIAID {
			iaPD = pd
			break
		}
	}

	if iaPD == nil {
		return fmt.Errorf("REPLY did not contain matching IA_PD")
	}

	// Check for status code indicating error
	if status := iaPD.Options.Status(); status != nil && status.StatusCode != iana.StatusSuccess {
		return fmt.Errorf("IA_PD status error: %s - %s", status.StatusCode, status.StatusMessage)
	}

	// Extract prefix information
	prefixes := iaPD.Options.Prefixes()
	if len(prefixes) == 0 {
		return fmt.Errorf("IA_PD did not contain any prefixes")
	}

	// Use the first valid prefix. Prefix may be nil even when the lifetimes
	// parsed fine: the wire format carries them ahead of the prefix-length
	// byte, and the decoder nils the prefix when that byte is zero. Screening
	// on the lifetime alone would dereference nil below, panicking runLoop --
	// a bare goroutine with no recover, so it takes the process down.
	var bestPrefix *dhcpv6.OptIAPrefix
	for _, p := range prefixes {
		if p.ValidLifetime > 0 && p.Prefix != nil {
			bestPrefix = p
			break
		}
	}

	if bestPrefix == nil {
		return fmt.Errorf("no valid prefix in IA_PD")
	}

	// Convert to netip.Prefix
	addr, ok := netip.AddrFromSlice(bestPrefix.Prefix.IP)
	if !ok {
		return fmt.Errorf("invalid prefix address")
	}
	// A prefix-length byte above 128 makes net.CIDRMask return nil, and Size()
	// reports (0, 0) for any non-canonical mask. Left unchecked that becomes a
	// /0 delegation covering the whole address space, which would propagate
	// into status, subnet math and the advertised pools.
	ones, bits := bestPrefix.Prefix.Mask.Size()
	if bits != net.IPv6len*8 || ones == 0 {
		return fmt.Errorf("invalid prefix length in IA_PD")
	}
	// Mask before use. The server is not obliged to send a prefix with its host
	// bits cleared, and unmasked bits would flow into status, into the
	// change-detection comparison, and into every address derived from the
	// prefix. The Router Advertisement path has always masked; this one did not.
	prefix := netip.PrefixFrom(addr, ones).Masked()

	// Delegated prefixes carry no address-class guarantee on the wire, so apply
	// the same acceptance rule the RA path uses rather than trusting the server.
	if err := ValidateDelegatedPrefix(prefix, r.requireGlobalUnicast); err != nil {
		return fmt.Errorf("rejecting delegated prefix from IA_PD: %w", err)
	}

	// Calculate T1/T2 from IA_PD or use defaults
	t1 := iaPD.T1
	t2 := iaPD.T2
	if t1 == 0 {
		t1 = bestPrefix.ValidLifetime / 2 // Default: 50%
	}
	if t2 == 0 {
		t2 = bestPrefix.ValidLifetime * 4 / 5 // Default: 80%
	}

	now := time.Now()
	newLease := &dhcpv6Lease{
		IAID:              expectedIAID,
		Prefix:            prefix,
		T1:                t1,
		T2:                t2,
		ValidLifetime:     bestPrefix.ValidLifetime,
		PreferredLifetime: bestPrefix.PreferredLifetime,
		ReceivedAt:        now,
		ServerID:          serverID,
	}

	r.mu.Lock()
	oldPrefix := r.currentPrefix
	r.currentPrefix = &Prefix{
		Network:           prefix,
		ValidLifetime:     bestPrefix.ValidLifetime,
		PreferredLifetime: bestPrefix.PreferredLifetime,
		Source:            SourceDHCPv6PD,
		ReceivedAt:        now,
	}
	r.lease = newLease
	r.mu.Unlock()

	// Determine event type
	var eventType EventType
	if oldPrefix == nil {
		eventType = EventTypeAcquired
	} else if oldPrefix.Network != prefix {
		eventType = EventTypeChanged
	} else {
		eventType = EventTypeRenewed
	}

	r.sendEvent(eventType, r.currentPrefix)
	return nil
}

// generateDUID generates a DUID-LL based on the interface's hardware address.
func (r *DHCPv6PDReceiver) generateDUID(ifi *net.Interface) dhcpv6.DUID {
	return &dhcpv6.DUIDLL{
		HWType:        iana.HWTypeEthernet,
		LinkLayerAddr: ifi.HardwareAddr,
	}
}

// sendEvent sends a prefix event.
func (r *DHCPv6PDReceiver) sendEvent(eventType EventType, prefix *Prefix) {
	select {
	case r.events <- Event{Type: eventType, Prefix: prefix}:
	default:
		// Channel full, event dropped
	}
}

// sendError sends a failed event.
func (r *DHCPv6PDReceiver) sendError(err error) {
	select {
	case r.events <- Event{Type: EventTypeFailed, Error: err}:
	default:
		// Channel full, event dropped
	}
}
