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
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	"github.com/mdlayher/ndp"
	"golang.org/x/net/ipv6"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	// RFC 4861 section 10 defines MAX_RTR_SOLICITATIONS as 3 transmissions.
	defaultMaxRouterSolicitations = 3
	// RFC 4861 section 10 defines RTR_SOLICITATION_INTERVAL as 4 seconds.
	defaultRouterSolicitationInterval = 4 * time.Second
)

var allRoutersMulticast = netip.MustParseAddr("ff02::2")

type ndpConn interface {
	Close() error
	ReadFrom() (ndp.Message, *ipv6.ControlMessage, netip.Addr, error)
	SetControlMessage(cf ipv6.ControlFlags, on bool) error
	SetReadDeadline(t time.Time) error
	SetWriteDeadline(t time.Time) error
	WriteTo(m ndp.Message, cm *ipv6.ControlMessage, dst netip.Addr) error
}

type ndpListenFunc func(ifi *net.Interface, addr ndp.Addr) (ndpConn, netip.Addr, error)

func defaultNDPListen(ifi *net.Interface, addr ndp.Addr) (ndpConn, netip.Addr, error) {
	return ndp.Listen(ifi, addr)
}

// RAReceiver monitors Router Advertisements to passively detect IPv6 prefix changes.
// This is useful when another service (like Talos or systemd-networkd) is handling
// DHCPv6-PD and we just need to observe the prefix being used.
type RAReceiver struct {
	// health carries its own lock; it is read by reconcile, not by the loop.
	health                     healthTracker
	mu                         sync.RWMutex
	iface                      string
	conn                       ndpConn
	currentPrefix              *Prefix
	events                     chan Event
	stopCh                     chan struct{}
	started                    bool
	ctx                        context.Context
	cancel                     context.CancelFunc
	listen                     ndpListenFunc
	maxRouterSolicitations     int
	routerSolicitationInterval time.Duration
	policy                     RAPolicy
	// verifyHopLimit records whether the socket agreed to report each packet's
	// hop limit. Written under mu by Start and handed to that generation's
	// receive loop as an argument, since Stop does not wait for the previous
	// loop to leave ReadFrom.
	verifyHopLimit bool
	// rejected counts advertisements dropped by validation. A flood is a
	// plausible way to attack the log, so the drops are counted and summarised
	// rather than logged one for one.
	rejected uint64
	// onReject reports a drop to whatever is counting them, set at construction
	// and never reassigned. It exists so the count can reach a metric without
	// this package importing the controller's registry.
	onReject RejectionObserver
	// onHopLimit reports whether the RFC 4861 hop-limit check ended up in force,
	// for the same reason and by the same route as onReject.
	onHopLimit HopLimitObserver
	// lastReason is the most recent drop's reason, for the periodic report that
	// turns a rising counter into something a user can act on.
	lastReasonMu sync.Mutex
	lastReason   string
}

// NewRAReceiver creates a new Router Advertisement receiver for the given
// interface, accepting only global-unicast prefixes. Use NewRAReceiverWithPolicy
// to track a prefix that is deliberately not global unicast.
func NewRAReceiver(iface string) *RAReceiver {
	return NewRAReceiverWithPolicy(iface, DefaultRAPolicy())
}

// RejectionObserver is told about each dropped advertisement, by interface and
// by one of the bounded reason constants.
type RejectionObserver func(iface, reason string)

// HopLimitObserver is told, once per receiver start, whether the hop-limit
// check is in force on that interface.
type HopLimitObserver func(iface string, enabled bool)

// NewRAReceiverWithPolicy creates a Router Advertisement receiver with an explicit
// acceptance policy. The policy is fixed for the receiver's lifetime, which is why
// receivers are pooled per interface *and* policy.
func NewRAReceiverWithPolicy(iface string, policy RAPolicy, observers ...RejectionObserver) *RAReceiver {
	var onReject RejectionObserver
	if len(observers) > 0 {
		onReject = observers[0]
	}
	return &RAReceiver{
		onReject:                   onReject,
		policy:                     policy,
		iface:                      iface,
		events:                     make(chan Event, 10),
		stopCh:                     make(chan struct{}),
		listen:                     defaultNDPListen,
		maxRouterSolicitations:     defaultMaxRouterSolicitations,
		routerSolicitationInterval: defaultRouterSolicitationInterval,
	}
}

// Start begins listening for Router Advertisements on the configured interface.
func (r *RAReceiver) Start(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.started {
		return nil
	}

	log := logf.FromContext(ctx).WithName("ra-receiver")
	log.V(1).Info("Looking up interface", "name", r.iface)

	ifi, err := net.InterfaceByName(r.iface)
	if err != nil {
		return fmt.Errorf("failed to get interface %s: %w", r.iface, err)
	}

	log.V(1).Info("Found interface",
		"name", ifi.Name,
		"index", ifi.Index,
		"hwAddr", ifi.HardwareAddr.String(),
		"mtu", ifi.MTU,
		"flags", ifi.Flags.String())

	listen := r.listen
	if listen == nil {
		listen = defaultNDPListen
	}

	// Create NDP connection for listening to Router Advertisements
	conn, addr, err := listen(ifi, ndp.LinkLocal)
	if err != nil {
		return fmt.Errorf("failed to create NDP listener on %s: %w", r.iface, err)
	}

	log.V(1).Info("NDP listener started", "interface", r.iface, "localAddr", addr.String())

	// RFC 4861 section 6.1.2 requires a received Router Advertisement to carry a
	// hop limit of 255, which is what makes an RA unforgeable from off-link: a
	// router cannot forward a packet without decrementing it. Checking that needs
	// the hop limit to be delivered alongside each packet.
	//
	// If the socket will not report it the receiver still runs, because the source
	// check below is the more important of the two and a receiver that refuses to
	// start acquires no prefix at all -- but it says so, once, rather than leaving
	// the impression that both checks are in force.
	r.verifyHopLimit = true
	if err := conn.SetControlMessage(ipv6.FlagHopLimit, true); err != nil {
		r.verifyHopLimit = false
		log.Info("Could not enable hop-limit reporting; Router Advertisements will be accepted without the RFC 4861 hop-limit check",
			"interface", r.iface, "error", err.Error())
	}
	// Reported every start, not only on failure, so the series exists and can be
	// alerted on. A single log line at startup was the only previous sign that
	// one of the two anti-spoofing checks had quietly stopped applying.
	if r.onHopLimit != nil {
		r.onHopLimit(r.iface, r.verifyHopLimit)
	}

	r.conn = conn
	r.ctx, r.cancel = context.WithCancel(ctx)
	// Fresh stop channel per run, because Stop() closes this one. Reusing a
	// channel across runs would hand the new goroutines an already-closed one --
	// they exit at once, leaving a receiver that looks healthy and delivers
	// nothing -- and make the next Stop panic closing it twice.
	r.stopCh = make(chan struct{})
	r.started = true

	// Each generation's goroutines take their own context, stop channel, socket
	// and hop-limit policy; reading the fields inside them would race with the
	// next Start.
	go r.receiveLoop(r.ctx, r.stopCh, conn, r.verifyHopLimit)
	go r.sendInitialRouterSolicitations(r.ctx, r.stopCh, ifi.HardwareAddr)

	return nil
}

// Stop stops listening for Router Advertisements.
func (r *RAReceiver) Stop() error {
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

	if r.conn != nil {
		return r.conn.Close()
	}

	return nil
}

// Events returns the channel of prefix events.
func (r *RAReceiver) Events() <-chan Event {
	return r.events
}

// CurrentPrefix returns the currently observed prefix, if any.
func (r *RAReceiver) CurrentPrefix() *Prefix {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.currentPrefix
}

// LastError implements AcquisitionHealth.
func (r *RAReceiver) LastError() error { return r.health.LastError() }

// recordRejection counts a dropped advertisement and remembers why, returning
// the running total so callers can throttle their logging.
//
// The increment belongs here rather than at the call sites. It used to live at
// the receive-loop call site only, so a rejection recorded while walking the
// prefix options moved the metric and lastReason but not the counter --
// RARejections then reported a total of 0, reportRARejections returned early on
// that, and a link whose only fault was out-of-bounds prefix lengths produced
// an event stream that stayed silent forever. When both kinds occurred the
// event was worse than silent: it paired a count from one path with a reason
// from the other.
func (r *RAReceiver) recordRejection(reason string) uint64 {
	r.lastReasonMu.Lock()
	r.lastReason = reason
	r.lastReasonMu.Unlock()

	count := atomic.AddUint64(&r.rejected, 1)

	if r.onReject != nil {
		r.onReject(r.iface, reason)
	}

	return count
}

// RARejections reports how many advertisements this receiver has dropped and
// the most recent reason. Reconcile reads it to report drops on the resource:
// a link with a rogue router announces itself as a rising count here, and
// nowhere else a user would look.
func (r *RAReceiver) RARejections() (total uint64, lastReason string) {
	r.lastReasonMu.Lock()
	defer r.lastReasonMu.Unlock()
	return atomic.LoadUint64(&r.rejected), r.lastReason
}

// Source returns SourceRouterAdvertisement.
func (r *RAReceiver) Source() Source {
	return SourceRouterAdvertisement
}

// raErrorBackoff paces the receive loop after an error that returns without
// blocking, so a persistently failing socket cannot spin a core.
const raErrorBackoff = time.Second

// waitAfterError pauses for raErrorBackoff, reporting false if the receiver was
// stopped while waiting.
func (r *RAReceiver) waitAfterError(ctx context.Context, stopCh <-chan struct{}) bool {
	timer := time.NewTimer(raErrorBackoff)
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

// receiveLoop continuously reads Router Advertisements from the interface.
func (r *RAReceiver) receiveLoop(ctx context.Context, stopCh <-chan struct{}, conn ndpConn, verifyHopLimit bool) {
	log := logf.Log.WithName("ra-receiver")
	log.V(1).Info("Receive loop started", "interface", r.iface)

	iterationCount := 0
	for {
		select {
		case <-stopCh:
			log.V(1).Info("Receive loop stopping (stopCh)")
			return
		case <-ctx.Done():
			log.V(1).Info("Receive loop stopping (ctx done)")
			return
		default:
		}

		// Set read deadline to allow periodic checking of stop signal
		if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
			log.Error(err, "Failed to set read deadline")
			r.sendError(fmt.Errorf("failed to set read deadline: %w", err))
			// A sticky failure here (the interface went down) returns
			// instantly, so continuing straight away spins a core and emits an
			// error per iteration. The read below paces itself on its own
			// one-second deadline; this path has nothing to pace it.
			if !r.waitAfterError(ctx, stopCh) {
				return
			}
			continue
		}

		msg, cm, from, err := conn.ReadFrom()
		if err != nil {
			// Timeout is expected, just continue.
			// errors.As, not a bare assertion: the deadline error arrives
			// wrapped on some paths, and a missed timeout is treated as a
			// socket failure and paced with a backoff it does not need.
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				iterationCount++
				// Log every 30 seconds to show we're still alive
				if iterationCount%30 == 0 {
					log.V(1).Info("Waiting for Router Advertisements", "interface", r.iface, "iterations", iterationCount)
				}
				continue
			}
			log.Error(err, "Failed to read NDP message")
			r.sendError(fmt.Errorf("failed to read NDP message: %w", err))
			// Not a timeout, so the deadline did not pace this iteration. A
			// persistent error (ENETDOWN) would otherwise spin until the
			// receiver is stopped.
			if !r.waitAfterError(ctx, stopCh) {
				return
			}
			continue
		}

		log.V(1).Info("Received NDP message", "type", fmt.Sprintf("%T", msg), "from", from)

		ra, ok := msg.(*ndp.RouterAdvertisement)
		if !ok {
			// Not a Router Advertisement, ignore
			log.V(2).Info("Ignoring non-RA message", "type", fmt.Sprintf("%T", msg))
			continue
		}

		if reason, detail := r.rejectRouterAdvertisement(from, cm, verifyHopLimit); reason != "" {
			count := r.recordRejection(reason)
			// One line per packet would hand anyone on the link a way to fill the
			// node's disk, so the drops are counted and reported on a curve.
			if count == 1 || count%100 == 0 {
				log.Info("Ignoring Router Advertisement that failed validation",
					"interface", r.iface, "from", from, "reason", reason, "detail", detail, "rejectedTotal", count)
			} else {
				log.V(1).Info("Ignoring Router Advertisement that failed validation",
					"interface", r.iface, "from", from, "reason", reason, "detail", detail)
			}
			continue
		}

		log.V(1).Info("Received Router Advertisement", "from", from, "optionCount", len(ra.Options))
		r.handleRouterAdvertisement(ra)
	}
}

func (r *RAReceiver) sendInitialRouterSolicitations(ctx context.Context, stopCh <-chan struct{}, hwAddr net.HardwareAddr) {
	log := logf.Log.WithName("ra-receiver")
	maxSolicitations := r.maxRouterSolicitations
	if maxSolicitations <= 0 {
		maxSolicitations = defaultMaxRouterSolicitations
	}
	interval := r.routerSolicitationInterval
	if interval <= 0 {
		interval = defaultRouterSolicitationInterval
	}

	// RFC 4861 recommends a random delay before the first Router Solicitation
	// to avoid synchronized host startup bursts. This operator creates at most
	// one shared RA receiver per configured interface, and its goal is to learn
	// the current delegated prefix as soon as the pod starts, so we intentionally
	// send the first solicitation immediately and keep the standard retry limit.
	for attempt := 1; attempt <= maxSolicitations; attempt++ {
		if r.CurrentPrefix() != nil {
			log.V(1).Info("Router Solicitation loop stopping: prefix already acquired", "interface", r.iface)
			return
		}

		select {
		case <-stopCh:
			return
		case <-ctx.Done():
			return
		default:
		}

		if err := r.sendRouterSolicitation(hwAddr); err != nil {
			log.Error(err, "Failed to send Router Solicitation", "interface", r.iface, "attempt", attempt, "maxAttempts", maxSolicitations)
		} else {
			log.V(1).Info("Router Solicitation sent", "interface", r.iface, "attempt", attempt, "maxAttempts", maxSolicitations)
		}

		if attempt == maxSolicitations {
			return
		}

		timer := time.NewTimer(interval)
		select {
		case <-stopCh:
			if !timer.Stop() {
				<-timer.C
			}
			return
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return
		case <-timer.C:
		}
	}
}

func (r *RAReceiver) sendRouterSolicitation(hwAddr net.HardwareAddr) error {
	r.mu.RLock()
	conn := r.conn
	r.mu.RUnlock()

	if conn == nil {
		return fmt.Errorf("NDP connection is not initialized")
	}

	options := make([]ndp.Option, 0, 1)
	if len(hwAddr) > 0 {
		hwAddrCopy := append(net.HardwareAddr(nil), hwAddr...)
		options = append(options, &ndp.LinkLayerAddress{
			Direction: ndp.Source,
			Addr:      hwAddrCopy,
		})
	}

	if err := conn.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		return fmt.Errorf("failed to set write deadline: %w", err)
	}

	if err := conn.WriteTo(&ndp.RouterSolicitation{Options: options}, nil, allRoutersMulticast); err != nil {
		return fmt.Errorf("failed to write Router Solicitation: %w", err)
	}

	return nil
}

// Reasons an advertisement is discarded. Bounded and low-cardinality, because
// they label a metric as well as a log line.
const (
	rejectReasonNotLinkLocal    = "source-not-link-local"
	rejectReasonHopLimitUnknown = "hop-limit-unreported"
	rejectReasonForwarded       = "forwarded-hop-limit"
	rejectReasonUntrustedSource = "untrusted-source"
	rejectReasonPrefixLength    = "prefix-length-out-of-bounds"
)

// rejectRouterAdvertisement applies the receive-side checks a Router
// Advertisement has to pass, returning the reason it must be discarded and a
// description, or "" if it may be processed.
//
// The first two are what RFC 4861 section 6.1.2 requires. They are not a
// defence against anything on the link: an attacker with link access can forge
// a compliant RA, which is inherent to taking delegation from Router
// Advertisements at all. What they do is restore the floor every conforming NDP
// implementation provides, and rule out the remote attacker entirely.
//
//   - The source must be link-local. A router advertises from fe80::/10, and an
//     RA from a routable address is either misconfigured or spoofed.
//   - The hop limit must be 255. Since a forwarding router must decrement it,
//     receiving 255 proves the packet was never forwarded, i.e. originated on
//     this link.
//
// The third is the trust decision the first two cannot make. Every host on the
// segment satisfies "on-link and one hop away", so a link the operator does not
// control needs the routers that may be believed named explicitly -- the job a
// switch does with RA Guard, done here for the links where that is not
// available.
func (r *RAReceiver) rejectRouterAdvertisement(from netip.Addr, cm *ipv6.ControlMessage, verifyHopLimit bool) (reason, detail string) {
	if !isLinkLocal(from) {
		return rejectReasonNotLinkLocal, "source address is not link-local"
	}

	if !r.policy.Trusts(from) {
		return rejectReasonUntrustedSource, "source is not one of the trusted routers"
	}

	if !verifyHopLimit {
		return "", ""
	}
	if cm == nil {
		return rejectReasonHopLimitUnknown, "hop limit was not reported for this packet"
	}
	if cm.HopLimit != ndp.HopLimit {
		return rejectReasonForwarded,
			fmt.Sprintf("hop limit is %d, not %d, so the packet was forwarded", cm.HopLimit, ndp.HopLimit)
	}
	return "", ""
}

// handleRouterAdvertisement processes a received Router Advertisement.
func (r *RAReceiver) handleRouterAdvertisement(ra *ndp.RouterAdvertisement) {
	log := logf.Log.WithName("ra-receiver")
	var bestPrefix *ndp.PrefixInformation

	// Look through all options for Prefix Information
	for _, opt := range ra.Options {
		pi, ok := opt.(*ndp.PrefixInformation)
		if !ok {
			continue
		}

		log.V(2).Info("Found prefix option",
			"prefix", pi.Prefix,
			"prefixLength", pi.PrefixLength,
			"onLink", pi.OnLink,
			"autonomous", pi.AutonomousAddressConfiguration,
			"validLifetime", pi.ValidLifetime,
			"preferredLifetime", pi.PreferredLifetime)

		// Skip if not on-link
		// Note: We don't require autonomous=true because that only controls SLAAC.
		// Many ISPs (e.g., Deutsche Telekom) advertise prefixes with autonomous=false
		// when using stateful DHCPv6 for address assignment. The prefix is still valid.
		if !pi.OnLink {
			log.V(1).Info("Skipping prefix: not on-link", "prefix", pi.Prefix)
			continue
		}

		// Skip zero valid lifetime (deprecated prefix)
		if pi.ValidLifetime == 0 {
			log.V(1).Info("Skipping prefix: zero valid lifetime", "prefix", pi.Prefix)
			continue
		}

		// Screen the length here as well as at the controller's choke point: an
		// advertisement carrying an implausible prefix alongside a usable one
		// should lose to the usable one, rather than be selected and then
		// rejected downstream, which leaves the receiver holding nothing.
		if err := r.policy.checkLength(int(pi.PrefixLength)); err != nil {
			log.V(1).Info("Skipping prefix outside the configured length bounds",
				"prefix", pi.Prefix, "prefixLength", pi.PrefixLength, "reason", err.Error())
			r.recordRejection(rejectReasonPrefixLength)
			continue
		}

		// The Prefix field is already netip.Addr in mdlayher/ndp v1.1.0
		addr := pi.Prefix

		// Prefer Global Unicast Addresses over ULA and Link-Local
		if isGlobalUnicast(addr) {
			log.V(1).Info("Prefix is Global Unicast", "prefix", pi.Prefix)
			if bestPrefix == nil || !isGlobalUnicast(bestPrefix.Prefix) {
				bestPrefix = pi
			}
		} else if isULA(addr) {
			// Preferring a global prefix is not the same as requiring one. A link
			// often advertises a unique-local prefix alongside the global one, and
			// an advertisement that momentarily carries only the unique-local one
			// would otherwise be accepted as though the delegation had changed --
			// moving every derived address off-link and pushing the real prefix out
			// of history as if it had been retired. Skip it unless the tracked
			// prefix is deliberately not global unicast.
			if r.policy.RequireGlobalUnicast {
				log.V(1).Info("Ignoring unique-local prefix: requireGlobalUnicast is set",
					"prefix", pi.Prefix)
				continue
			}
			log.V(1).Info("Prefix is ULA", "prefix", pi.Prefix)
			if bestPrefix == nil {
				bestPrefix = pi
			}
		} else {
			log.V(1).Info("Prefix is neither GUA nor ULA, skipping", "prefix", pi.Prefix)
		}
	}

	if bestPrefix == nil {
		// V(1): a link that advertises a prefix this receiver does not accept
		// does so on every advertisement, so at level 0 an ordinary router
		// several times a minute -- or an attacker at will -- writes an
		// unbounded number of identical lines.
		log.V(1).Info("No suitable prefix found in Router Advertisement")
		return
	}

	// PrefixFrom neither masks nor validates. A PIO whose length exceeds 128
	// yields an invalid Prefix whose Bits() is -1, which then slips past the
	// "subnet shorter than base" guard in CalculateSubnet, and any host bits
	// present in the advertisement would leak into status and into the
	// change-detection comparison below.
	if int(bestPrefix.PrefixLength) > netip.MustParseAddr("::").BitLen() {
		log.V(1).Info("Ignoring Router Advertisement prefix with an out-of-range length",
			"prefixLength", bestPrefix.PrefixLength)
		return
	}
	prefix := netip.PrefixFrom(bestPrefix.Prefix, int(bestPrefix.PrefixLength)).Masked()
	log.V(1).Info("Selected prefix", "prefix", prefix, "validLifetime", bestPrefix.ValidLifetime)

	r.updatePrefix(prefix, bestPrefix.ValidLifetime, bestPrefix.PreferredLifetime)
}

// updatePrefix updates the current prefix and sends an event if changed.
func (r *RAReceiver) updatePrefix(prefix netip.Prefix, validLifetime, preferredLifetime time.Duration) {
	log := logf.Log.WithName("ra-receiver")
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	newPrefix := &Prefix{
		Network:           prefix,
		ValidLifetime:     validLifetime,
		PreferredLifetime: preferredLifetime,
		Source:            SourceRouterAdvertisement,
		ReceivedAt:        now,
	}

	var eventType EventType
	if r.currentPrefix == nil {
		eventType = EventTypeAcquired
	} else if r.currentPrefix.Network != prefix {
		eventType = EventTypeChanged
	} else {
		eventType = EventTypeRenewed
	}

	var previousPrefix any
	if r.currentPrefix != nil {
		previousPrefix = r.currentPrefix.Network
	}

	logInfoAtVerbosity(log, prefixEventLogVerbosity(eventType), prefixEventLogMessage(eventType),
		"prefix", prefix,
		"validLifetime", validLifetime,
		"preferredLifetime", preferredLifetime,
		"eventType", eventType,
		"previousPrefix", previousPrefix)

	r.currentPrefix = newPrefix
	r.health.recordSuccess()

	// Send event (non-blocking to avoid deadlock)
	select {
	case r.events <- Event{Type: eventType, Prefix: newPrefix}:
		log.V(2).Info("Event sent", "eventType", eventType)
	default:
		log.Info("Event channel full, event dropped", "eventType", eventType)
	}
}

// sendError sends a failed event and remembers it, since the event itself is
// not forwarded to the reconciler.
func (r *RAReceiver) sendError(err error) {
	r.health.recordFailure(err)

	select {
	case r.events <- Event{Type: EventTypeFailed, Error: err}:
	default:
		// Channel full, event dropped
	}
}

func logInfoAtVerbosity(log logr.Logger, verbosity int, msg string, keysAndValues ...any) {
	if verbosity <= 0 {
		log.Info(msg, keysAndValues...)
		return
	}

	log.V(verbosity).Info(msg, keysAndValues...)
}

func prefixEventLogMessage(eventType EventType) string {
	switch eventType {
	case EventTypeAcquired:
		return "Prefix acquired"
	case EventTypeChanged:
		return "Prefix changed"
	case EventTypeRenewed:
		return "Prefix renewed"
	default:
		return "Prefix updated"
	}
}

func prefixEventLogVerbosity(eventType EventType) int {
	switch eventType {
	case EventTypeAcquired, EventTypeChanged:
		return 0
	default:
		return 1
	}
}

// isGlobalUnicast returns true if the address is a Global Unicast Address (2000::/3).
func isGlobalUnicast(addr netip.Addr) bool {
	if !addr.Is6() {
		return false
	}
	bytes := addr.As16()
	// GUA: first 3 bits are 001 (0x20-0x3f in first byte)
	return (bytes[0] & 0xE0) == 0x20
}

// isULA returns true if the address is a Unique Local Address (fc00::/7).
func isULA(addr netip.Addr) bool {
	if !addr.Is6() {
		return false
	}
	bytes := addr.As16()
	// ULA: first 7 bits are 1111110 (0xfc or 0xfd in first byte)
	return (bytes[0] & 0xFE) == 0xFC
}

// isLinkLocal returns true if the address is a Link-Local Address (fe80::/10).
func isLinkLocal(addr netip.Addr) bool {
	return addr.IsLinkLocalUnicast()
}
