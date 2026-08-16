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
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv6"
	"github.com/insomniacslk/dhcp/dhcpv6/nclient6"
	"github.com/insomniacslk/dhcp/iana"
)

// dhcpv6Client is the transport half of an exchange: send one message, wait for
// the matching answer. nclient6.Client satisfies it. Taking it as a dependency
// is what lets the tests script a server, so the messages this client puts on
// the wire are checked rather than assumed.
type dhcpv6Client interface {
	SendAndRead(ctx context.Context, dest *net.UDPAddr, msg *dhcpv6.Message, match nclient6.Matcher) (*dhcpv6.Message, error)
	Close() error
}

// dhcpv6DialFunc opens a client bound to an interface.
type dhcpv6DialFunc func(iface string) (dhcpv6Client, error)

func defaultDHCPv6Dial(iface string) (dhcpv6Client, error) {
	return nclient6.New(iface)
}

// DHCPv6PDReceiver implements a DHCPv6 Prefix Delegation client.
// It actively requests prefix delegation from an upstream DHCPv6 server
// and handles lease renewals.
type DHCPv6PDReceiver struct {
	// health carries its own lock; it is read by reconcile, not by the loop.
	health                healthTracker
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
	// dial and lookupInterface are fixed at construction and never reassigned,
	// so the exchange goroutine can read them without the lock.
	dial            dhcpv6DialFunc
	lookupInterface func(name string) (*net.Interface, error)
	// acquireBackoffMin and acquireBackoffMax bound the retry pace and are
	// likewise fixed at construction.
	acquireBackoffMin time.Duration
	acquireBackoffMax time.Duration
	// releaseInterface hands this receiver's interface back to the factory that
	// claimed it, so the next receiver built for that interface can have it. It
	// is nil for receivers built directly, which claim nothing.
	releaseInterface func()
	releaseOnce      sync.Once
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
		dial:                  defaultDHCPv6Dial,
		lookupInterface:       net.InterfaceByName,
		acquireBackoffMin:     defaultAcquireBackoffMin,
		acquireBackoffMax:     defaultAcquireBackoffMax,
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

// releaseClaimedInterface hands the interface back to the factory. Safe to call
// more than once, and on a receiver that never claimed one.
//
// Deliberately outside the started check in Stop: a receiver can be built and
// then discarded without ever running -- a spec that fails validation on the
// way to being started, say -- and a claim left behind would make the interface
// unusable for the rest of the process.
func (r *DHCPv6PDReceiver) releaseClaimedInterface() {
	if r.releaseInterface == nil {
		return
	}
	r.releaseOnce.Do(r.releaseInterface)
}

// Stop stops the DHCPv6-PD client.
func (r *DHCPv6PDReceiver) Stop() error {
	r.releaseClaimedInterface()

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

// LastError implements AcquisitionHealth.
func (r *DHCPv6PDReceiver) LastError() error { return r.health.LastError() }

// Source returns SourceDHCPv6PD.
func (r *DHCPv6PDReceiver) Source() Source {
	return SourceDHCPv6PD
}

const (
	// defaultAcquireBackoffMin is the first pause after a failed acquisition.
	defaultAcquireBackoffMin = 10 * time.Second
	// defaultAcquireBackoffMax caps it. An uplink that has been down for an hour
	// is not about to come back because it was asked a 360th time, but the delay
	// still has to be short enough that a rotation is picked up promptly.
	defaultAcquireBackoffMax = 5 * time.Minute
	// renewRetryInterval spaces out RENEW attempts between T1 and T2. Without
	// it the loop reruns immediately -- the lease is unchanged, so the T1 test
	// still passes -- burning a core and opening a socket per iteration for the
	// whole T1..T2 window, which is typically hours.
	renewRetryInterval = 30 * time.Second
)

// nextBackoff doubles a delay without passing a ceiling. Doubling near the end
// of the duration range wraps negative, and a negative timer fires at once --
// the busy loop the ceiling exists to prevent -- so the overflow is checked.
func nextBackoff(cur, max time.Duration) time.Duration {
	if cur >= max || cur > max/2 {
		return max
	}
	return cur * 2
}

// jittered spreads a delay by up to a fifth either way, so a fleet of operators
// that lost the same uplink does not solicit in lockstep when it returns.
func jittered(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	spread := int64(d) / 5
	if spread == 0 {
		return d
	}
	// #nosec G404 -- spreading retry timing, not generating a secret
	return d + time.Duration(rand.Int64N(2*spread+1)-spread)
}

// acquireBackoff paces attempts while no lease is held.
type acquireBackoff struct {
	min, max, cur time.Duration
}

func newAcquireBackoff(minDelay, maxDelay time.Duration) acquireBackoff {
	return acquireBackoff{min: minDelay, max: maxDelay, cur: minDelay}
}

// current is the delay before the next attempt, before jitter.
func (b *acquireBackoff) current() time.Duration { return b.cur }

// wait is how long to actually sleep for.
func (b *acquireBackoff) wait() time.Duration { return jittered(b.cur) }

func (b *acquireBackoff) failed() { b.cur = nextBackoff(b.cur, b.max) }

// succeeded returns to the shortest delay, so the rotation after a recovery is
// noticed as quickly as the first one would have been.
func (b *acquireBackoff) succeeded() { b.cur = b.min }

// dropLease gives up the delegation and says so. The prefix goes with it: a
// lease the server will not extend is one whose prefix is no longer routed,
// and reporting it as still held would keep it in the pools.
func (r *DHCPv6PDReceiver) dropLease() {
	r.mu.Lock()
	r.currentPrefix = nil
	r.lease = nil
	r.mu.Unlock()

	r.sendEvent(EventTypeExpired, nil)
}

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
	// The pace belongs to this generation of the loop, like its context and
	// stop channel: a restart is a fresh start, not a continuation of whatever
	// the previous one had backed off to.
	backoff := newAcquireBackoff(r.acquireBackoffMin, r.acquireBackoffMax)

	// Initial acquisition
	if err := r.acquirePrefix(ctx); err != nil {
		r.sendError(fmt.Errorf("initial prefix acquisition failed: %w", err))
		backoff.failed()
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
			if !r.waitFor(ctx, stopCh, backoff.wait()) {
				return
			}
			if err := r.acquirePrefix(ctx); err != nil {
				r.sendError(fmt.Errorf("prefix acquisition failed: %w", err))
				backoff.failed()
			} else {
				backoff.succeeded()
			}
			continue
		}

		// Calculate when to renew
		now := time.Now()
		elapsed := now.Sub(lease.ReceivedAt)

		// Renew at T1 (typically 50% of valid lifetime)
		if elapsed >= lease.T1 {
			if err := r.renewPrefix(ctx); err != nil {
				r.sendError(fmt.Errorf("prefix renewal failed: %w", err))
				switch {
				case errors.Is(err, errNoBinding):
					// The server has disowned this delegation, so it will
					// refuse the rebind at T2 for the same reason. Holding the
					// prefix until then means advertising one nothing upstream
					// routes; the acquisition branch re-solicits at once.
					r.dropLease()
					continue
				case elapsed >= lease.T2:
					// If T2 has passed, try rebind
					if err := r.rebindPrefix(ctx); err != nil {
						r.sendError(fmt.Errorf("prefix rebind failed: %w", err))
						// The lease is gone; the acquisition branch above owns
						// the retry pacing from here.
						r.dropLease()
						continue
					}
				case !r.waitFor(ctx, stopCh, renewRetryInterval):
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

// exchangeTimeout bounds one send-and-wait. nclient6 retransmits within it.
const exchangeTimeout = 30 * time.Second

// openClient dials the configured interface, returning the interface too: its
// hardware address is what the client's DUID is built from.
func (r *DHCPv6PDReceiver) openClient() (dhcpv6Client, *net.Interface, error) {
	ifi, err := r.lookupInterface(r.iface)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get interface %s: %w", r.iface, err)
	}

	client, err := r.dial(r.iface)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create DHCPv6 client: %w", err)
	}
	return client, ifi, nil
}

// iaidForInterface derives the association's identifier from the interface
// index. The IAID is four bytes of identity rather than a number, so it is
// written as an explicit big-endian encoding.
func iaidForInterface(ifi *net.Interface) [4]byte {
	var iaid [4]byte
	binary.BigEndian.PutUint32(iaid[:], uint32(ifi.Index)) // #nosec G115 -- interface indexes are small positive integers
	return iaid
}

// newPDMessage starts a message carrying the identity every exchange needs.
func (r *DHCPv6PDReceiver) newPDMessage(mt dhcpv6.MessageType, ifi *net.Interface) (*dhcpv6.Message, error) {
	msg, err := dhcpv6.NewMessage()
	if err != nil {
		return nil, fmt.Errorf("failed to create %s: %w", mt, err)
	}
	msg.MessageType = mt
	msg.AddOption(dhcpv6.OptClientID(r.generateDUID(ifi)))
	msg.AddOption(dhcpv6.OptElapsedTime(0))
	return msg, nil
}

// iaPDHint asks for a prefix of the configured length without naming one --
// the form RFC 8415 §21.21 gives a client that holds no prefix yet.
func (r *DHCPv6PDReceiver) iaPDHint(iaid [4]byte) *dhcpv6.OptIAPD {
	return &dhcpv6.OptIAPD{
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
}

// iaPDForLease restates the delegation being renewed or rebound.
func iaPDForLease(lease *dhcpv6Lease) *dhcpv6.OptIAPD {
	return &dhcpv6.OptIAPD{
		IaId: lease.IAID,
		Options: dhcpv6.PDOptions{
			Options: dhcpv6.Options{
				&dhcpv6.OptIAPrefix{
					PreferredLifetime: lease.PreferredLifetime,
					ValidLifetime:     lease.ValidLifetime,
					Prefix: &net.IPNet{
						IP:   lease.Prefix.Addr().AsSlice(),
						Mask: net.CIDRMask(lease.Prefix.Bits(), 128),
					},
				},
			},
		},
	}
}

// errNoBinding marks the one refusal the client can act on rather than just
// report: the server does not know about this delegation, so no amount of
// renewing or rebinding will bring it back and only a new SOLICIT will.
var errNoBinding = errors.New("the server holds no binding for this delegation")

// statusError reports a server's refusal in the server's own words, which is
// almost always more specific than anything the client could infer.
func statusError(status *dhcpv6.OptStatusCode) error {
	if status == nil || status.StatusCode == iana.StatusSuccess {
		return nil
	}
	if status.StatusCode == iana.StatusNoBinding {
		return fmt.Errorf("%w: %s", errNoBinding, status.StatusMessage)
	}
	return fmt.Errorf("server reported %s: %s", status.StatusCode, status.StatusMessage)
}

// acquirePrefix performs initial prefix acquisition using SOLICIT-ADVERTISE-REQUEST-REPLY.
func (r *DHCPv6PDReceiver) acquirePrefix(ctx context.Context) error {
	client, ifi, err := r.openClient()
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	iaid := iaidForInterface(ifi)

	// This client delegates and nothing else, so it asks for a prefix and not
	// for an address as well. Soliciting an IA_NA it never reads would leave a
	// lease nothing renews or releases, and -- because the helper that built
	// the REQUEST out of the ADVERTISE required one to come back -- made every
	// exchange with a delegation-only server fail outright.
	solicit, err := r.newPDMessage(dhcpv6.MessageTypeSolicit, ifi)
	if err != nil {
		return err
	}
	solicit.AddOption(r.iaPDHint(iaid))

	ctx, cancel := context.WithTimeout(ctx, exchangeTimeout)
	defer cancel()

	advertise, err := client.SendAndRead(ctx, nclient6.AllDHCPRelayAgentsAndServers, solicit, nclient6.IsMessageType(dhcpv6.MessageTypeAdvertise))
	if err != nil {
		return fmt.Errorf("failed to receive ADVERTISE: %w", err)
	}

	advIAPD := advertise.GetOneOption(dhcpv6.OptionIAPD)
	if advIAPD == nil {
		return fmt.Errorf("ADVERTISE did not contain IA_PD")
	}
	// A server with nothing to delegate says so here. Requesting anyway costs a
	// round trip to be told the same thing.
	if pd, ok := advIAPD.(*dhcpv6.OptIAPD); ok {
		if err := statusError(pd.Options.Status()); err != nil {
			return fmt.Errorf("ADVERTISE offered no delegation: %w", err)
		}
	}

	serverID := advertise.Options.ServerID()
	if serverID == nil {
		return fmt.Errorf("ADVERTISE did not contain Server ID")
	}

	// Accept the advertised delegation from the server that offered it.
	request, err := r.newPDMessage(dhcpv6.MessageTypeRequest, ifi)
	if err != nil {
		return err
	}
	request.AddOption(dhcpv6.OptServerID(serverID))
	request.AddOption(advIAPD)

	reply, err := client.SendAndRead(ctx, nclient6.AllDHCPRelayAgentsAndServers, request, nclient6.IsMessageType(dhcpv6.MessageTypeReply))
	if err != nil {
		return fmt.Errorf("failed to receive REPLY: %w", err)
	}

	// Extract IA_PD from REPLY
	return r.processIAPDReply(reply, iaid, serverID)
}

// renewPrefix sends a RENEW message to extend the lease.
func (r *DHCPv6PDReceiver) renewPrefix(ctx context.Context) error {
	r.mu.RLock()
	lease := r.lease
	r.mu.RUnlock()

	if lease == nil {
		return fmt.Errorf("no lease to renew")
	}

	client, ifi, err := r.openClient()
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	// A RENEW goes to the server holding the binding, so it names it.
	renew, err := r.newPDMessage(dhcpv6.MessageTypeRenew, ifi)
	if err != nil {
		return err
	}
	renew.AddOption(dhcpv6.OptServerID(lease.ServerID))
	renew.AddOption(iaPDForLease(lease))

	ctx, cancel := context.WithTimeout(ctx, exchangeTimeout)
	defer cancel()

	reply, err := client.SendAndRead(ctx, nclient6.AllDHCPRelayAgentsAndServers, renew, nclient6.IsMessageType(dhcpv6.MessageTypeReply))
	if err != nil {
		return fmt.Errorf("failed to receive REPLY for RENEW: %w", err)
	}

	return r.processIAPDReply(reply, lease.IAID, lease.ServerID)
}

// rebindPrefix sends a REBIND message when the server is unreachable.
func (r *DHCPv6PDReceiver) rebindPrefix(ctx context.Context) error {
	r.mu.RLock()
	lease := r.lease
	r.mu.RUnlock()

	if lease == nil {
		return fmt.Errorf("no lease to rebind")
	}

	client, ifi, err := r.openClient()
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	// A REBIND names no server: it is addressed to whichever one still holds
	// the binding, and the answer decides who that is.
	rebind, err := r.newPDMessage(dhcpv6.MessageTypeRebind, ifi)
	if err != nil {
		return err
	}
	rebind.AddOption(iaPDForLease(lease))

	ctx, cancel := context.WithTimeout(ctx, exchangeTimeout)
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
	// A refusal that applies to the whole message carries no IA_PD to hang a
	// status on, so checking only the association's status reported the reply
	// as missing a delegation rather than as the refusal it was.
	if err := statusError(reply.Options.Status()); err != nil {
		return fmt.Errorf("REPLY was refused: %w", err)
	}

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
	if err := statusError(iaPD.Options.Status()); err != nil {
		return fmt.Errorf("IA_PD was refused: %w", err)
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
	r.health.recordSuccess()

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

// sendError sends a failed event and remembers it, since the event itself is
// not forwarded to the reconciler.
func (r *DHCPv6PDReceiver) sendError(err error) {
	r.health.recordFailure(err)

	select {
	case r.events <- Event{Type: EventTypeFailed, Error: err}:
	default:
		// Channel full, event dropped
	}
}
