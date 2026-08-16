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
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv6"
	"github.com/insomniacslk/dhcp/dhcpv6/nclient6"
	"github.com/insomniacslk/dhcp/iana"
)

// The exchanges below run against a scripted server rather than a socket. Until
// the client took its transport as a dependency the only testable part of
// DHCPv6-PD was reply parsing, so the messages the operator puts on the wire --
// and the shapes of server that its message building could not survive -- went
// unexercised.

// wireTestInterface is the interface the scripted exchanges pretend to run on.
// The index matters: the IA_PD's IAID is derived from it.
var wireTestInterface = &net.Interface{
	Index:        3,
	Name:         "eth0",
	HardwareAddr: net.HardwareAddr{0x02, 0x00, 0x5e, 0x10, 0x00, 0x01},
}

func wireTestIAID() [4]byte {
	var iaid [4]byte
	binary.BigEndian.PutUint32(iaid[:], uint32(wireTestInterface.Index)) // #nosec G115 -- fixed small index
	return iaid
}

func wireTestServerID() dhcpv6.DUID {
	return &dhcpv6.DUIDLL{
		HWType:        iana.HWTypeEthernet,
		LinkLayerAddr: net.HardwareAddr{0x02, 0x00, 0x5e, 0x20, 0x00, 0x01},
	}
}

// respondFunc is the server half of one exchange.
type respondFunc func(req *dhcpv6.Message) (*dhcpv6.Message, error)

// fakeDHCPv6Client records what the receiver sends and answers from a script.
type fakeDHCPv6Client struct {
	mu      sync.Mutex
	respond respondFunc
	sent    []*dhcpv6.Message
	closed  int
}

func (c *fakeDHCPv6Client) SendAndRead(_ context.Context, _ *net.UDPAddr, msg *dhcpv6.Message, match nclient6.Matcher) (*dhcpv6.Message, error) {
	c.mu.Lock()
	c.sent = append(c.sent, msg)
	respond := c.respond
	c.mu.Unlock()

	reply, err := respond(msg)
	if err != nil {
		return nil, err
	}
	// The real client discards anything the matcher rejects and keeps waiting,
	// which surfaces to the caller as a timeout rather than a reply.
	if match != nil && !match(reply) {
		return nil, fmt.Errorf("no %s arrived before the deadline", reply.MessageType)
	}
	return reply, nil
}

func (c *fakeDHCPv6Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed++
	return nil
}

// sentOfType returns every message of a type the receiver put on the wire.
func (c *fakeDHCPv6Client) sentOfType(mt dhcpv6.MessageType) []*dhcpv6.Message {
	c.mu.Lock()
	defer c.mu.Unlock()

	var out []*dhcpv6.Message
	for _, m := range c.sent {
		if m.MessageType == mt {
			out = append(out, m)
		}
	}
	return out
}

func (c *fakeDHCPv6Client) closeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// newWireTestReceiver builds a receiver whose transport and interface lookup
// are scripted, so no socket is opened and no real interface has to exist.
func newWireTestReceiver(t *testing.T, respond respondFunc) (*DHCPv6PDReceiver, *fakeDHCPv6Client) {
	t.Helper()

	client := &fakeDHCPv6Client{respond: respond}
	r := NewDHCPv6PDReceiver(wireTestInterface.Name, 56)
	r.dial = func(string) (dhcpv6Client, error) { return client, nil }
	r.lookupInterface = func(string) (*net.Interface, error) { return wireTestInterface, nil }
	return r, client
}

// iaPrefixFor builds an IA_PREFIX through the decoder, so the tests see the
// same option shapes a real server's bytes produce.
func iaPrefixFor(cidr string, preferred, valid time.Duration) (*dhcpv6.OptIAPrefix, error) {
	p, err := netip.ParsePrefix(cidr)
	if err != nil {
		return nil, err
	}
	body := iaPrefixBytes(
		uint32(preferred.Seconds()),
		uint32(valid.Seconds()),
		uint8(p.Bits()), // #nosec G115 -- prefix lengths are 0..128
		net.IP(p.Addr().AsSlice()),
	)
	opt := &dhcpv6.OptIAPrefix{}
	if err := opt.FromBytes(body); err != nil {
		return nil, err
	}
	return opt, nil
}

// leaseLifetime is what the scripted servers delegate for. The exchanges under
// test never wait it out; only the T1/T2 split derived from it is asserted on.
const leaseLifetime = time.Hour

// iaPDFor builds the IA_PD a server sends back, echoing the IAID the client
// asked about so the reply matches the client's association.
func iaPDFor(req *dhcpv6.Message, cidr string, extra ...dhcpv6.Option) (*dhcpv6.OptIAPD, error) {
	valid := leaseLifetime
	iaid := wireTestIAID()
	if opt := req.GetOneOption(dhcpv6.OptionIAPD); opt != nil {
		if pd, ok := opt.(*dhcpv6.OptIAPD); ok {
			iaid = pd.IaId
		}
	}

	iapd := &dhcpv6.OptIAPD{IaId: iaid, T1: valid / 2, T2: valid * 4 / 5}
	if cidr != "" {
		p, err := iaPrefixFor(cidr, valid/2, valid)
		if err != nil {
			return nil, err
		}
		iapd.Options.Add(p)
	}
	for _, o := range extra {
		iapd.Options.Add(o)
	}
	return iapd, nil
}

// answer builds a server message of the given type carrying the client's
// transaction ID and the server's identity.
func answer(req *dhcpv6.Message, mt dhcpv6.MessageType, opts ...dhcpv6.Option) *dhcpv6.Message {
	msg := &dhcpv6.Message{MessageType: mt, TransactionID: req.TransactionID}
	if cid := req.GetOneOption(dhcpv6.OptionClientID); cid != nil {
		msg.AddOption(cid)
	}
	msg.AddOption(dhcpv6.OptServerID(wireTestServerID()))
	for _, o := range opts {
		msg.AddOption(o)
	}
	return msg
}

// delegating is the server most of these tests run against: it delegates one
// prefix on an hour's lease and, like a provider that hands out no WAN address,
// offers no IA_NA.
func delegating(cidr string) respondFunc {
	return func(req *dhcpv6.Message) (*dhcpv6.Message, error) {
		iapd, err := iaPDFor(req, cidr)
		if err != nil {
			return nil, err
		}
		if req.MessageType == dhcpv6.MessageTypeSolicit {
			return answer(req, dhcpv6.MessageTypeAdvertise, iapd), nil
		}
		return answer(req, dhcpv6.MessageTypeReply, iapd), nil
	}
}

func drainEvents(r *DHCPv6PDReceiver) []Event {
	var events []Event
	for {
		select {
		case e := <-r.Events():
			events = append(events, e)
		default:
			return events
		}
	}
}

func lastEvent(t *testing.T, r *DHCPv6PDReceiver) Event {
	t.Helper()
	events := drainEvents(r)
	if len(events) == 0 {
		t.Fatal("expected an event, got none")
	}
	return events[len(events)-1]
}

// A provider that delegates a prefix without also assigning an address is an
// ordinary DHCPv6 server, and it is what most ISPs running prefix delegation
// look like. Building the REQUEST out of the ADVERTISE required an IA_NA to be
// present in it, so against those servers acquisition failed on every attempt
// with "IA_NA cannot be nil", ten seconds apart, forever.
func TestDHCPv6PDAcquiresFromAServerThatOnlyDelegates(t *testing.T) {
	r, client := newWireTestReceiver(t, delegating("2001:db8:beef::/56"))

	if err := r.acquirePrefix(context.Background()); err != nil {
		t.Fatalf("acquirePrefix() = %v", err)
	}

	got := r.CurrentPrefix()
	if got == nil {
		t.Fatal("expected a delegated prefix to be recorded")
	}
	if got.Network.String() != "2001:db8:beef::/56" {
		t.Fatalf("prefix = %v, want 2001:db8:beef::/56", got.Network)
	}
	if got.Source != SourceDHCPv6PD {
		t.Fatalf("source = %v, want %v", got.Source, SourceDHCPv6PD)
	}

	if e := lastEvent(t, r); e.Type != EventTypeAcquired {
		t.Fatalf("event = %v, want %v", e.Type, EventTypeAcquired)
	}

	if n := len(client.sentOfType(dhcpv6.MessageTypeSolicit)); n != 1 {
		t.Fatalf("sent %d SOLICITs, want 1", n)
	}
	requests := client.sentOfType(dhcpv6.MessageTypeRequest)
	if len(requests) != 1 {
		t.Fatalf("sent %d REQUESTs, want 1", len(requests))
	}
	if requests[0].Options.ServerID() == nil {
		t.Error("the REQUEST must name the server whose ADVERTISE it accepts")
	}
	if requests[0].GetOneOption(dhcpv6.OptionIAPD) == nil {
		t.Error("the REQUEST must carry the IA_PD being requested")
	}
	if client.closeCount() == 0 {
		t.Error("the client socket was not closed")
	}
}

// The operator consumes a delegated prefix and nothing else. Asking for an
// address association as well makes the exchange depend on the server having
// one to give, and leaves a lease nothing ever renews or releases.
func TestDHCPv6PDSolicitAsksOnlyForAPrefix(t *testing.T) {
	r, client := newWireTestReceiver(t, delegating("2001:db8::/48"))
	r.requestedPrefixLength = 48

	if err := r.acquirePrefix(context.Background()); err != nil {
		t.Fatalf("acquirePrefix() = %v", err)
	}

	solicits := client.sentOfType(dhcpv6.MessageTypeSolicit)
	if len(solicits) != 1 {
		t.Fatalf("sent %d SOLICITs, want 1", len(solicits))
	}
	solicit := solicits[0]

	if solicit.Options.OneIANA() != nil {
		t.Error("the SOLICIT asks for an address association the operator never uses")
	}
	if solicit.Options.ClientID() == nil {
		t.Error("the SOLICIT carries no client ID")
	}

	opt := solicit.GetOneOption(dhcpv6.OptionIAPD)
	if opt == nil {
		t.Fatal("the SOLICIT carries no IA_PD")
	}
	iapd, ok := opt.(*dhcpv6.OptIAPD)
	if !ok {
		t.Fatalf("IA_PD decoded as %T", opt)
	}
	if iapd.IaId != wireTestIAID() {
		t.Errorf("IAID = %v, want %v", iapd.IaId, wireTestIAID())
	}
	hints := iapd.Options.Prefixes()
	if len(hints) != 1 {
		t.Fatalf("the SOLICIT carries %d prefix hints, want 1", len(hints))
	}
	if ones, _ := hints[0].Prefix.Mask.Size(); ones != 48 {
		t.Errorf("prefix-length hint = /%d, want /48", ones)
	}
}

func TestDHCPv6PDAcquireRejectsUnusableAdvertisements(t *testing.T) {
	tests := []struct {
		name    string
		respond respondFunc
		wantErr string
	}{
		{
			name: "advertisement without a delegation",
			respond: func(req *dhcpv6.Message) (*dhcpv6.Message, error) {
				return answer(req, dhcpv6.MessageTypeAdvertise), nil
			},
			wantErr: "ADVERTISE did not contain IA_PD",
		},
		{
			name: "advertisement without a server identity",
			respond: func(req *dhcpv6.Message) (*dhcpv6.Message, error) {
				iapd, err := iaPDFor(req, "2001:db8::/56")
				if err != nil {
					return nil, err
				}
				msg := &dhcpv6.Message{MessageType: dhcpv6.MessageTypeAdvertise, TransactionID: req.TransactionID}
				msg.AddOption(iapd)
				return msg, nil
			},
			wantErr: "ADVERTISE did not contain Server ID",
		},
		{
			name: "no server answers",
			respond: func(*dhcpv6.Message) (*dhcpv6.Message, error) {
				return nil, errors.New("timed out")
			},
			wantErr: "failed to receive ADVERTISE",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, _ := newWireTestReceiver(t, tt.respond)

			err := r.acquirePrefix(context.Background())
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %q, want it to mention %q", err, tt.wantErr)
			}
			if r.CurrentPrefix() != nil {
				t.Error("a failed acquisition must not record a prefix")
			}
		})
	}
}

// A server with nothing to delegate says so in a status code. Reporting that
// verbatim is the difference between an operator that explains itself and one
// that looks broken.
func TestDHCPv6PDAcquireSurfacesServerStatus(t *testing.T) {
	tests := []struct {
		name    string
		respond respondFunc
		wantErr string
	}{
		{
			name: "status on the association",
			respond: func(req *dhcpv6.Message) (*dhcpv6.Message, error) {
				status := &dhcpv6.OptStatusCode{StatusCode: iana.StatusNoPrefixAvail, StatusMessage: "no prefixes left"}
				iapd, err := iaPDFor(req, "", status)
				if err != nil {
					return nil, err
				}
				if req.MessageType == dhcpv6.MessageTypeSolicit {
					return answer(req, dhcpv6.MessageTypeAdvertise, iapd), nil
				}
				return answer(req, dhcpv6.MessageTypeReply, iapd), nil
			},
			wantErr: "no prefixes left",
		},
		{
			name: "status on the message",
			respond: func(req *dhcpv6.Message) (*dhcpv6.Message, error) {
				iapd, err := iaPDFor(req, "2001:db8::/56")
				if err != nil {
					return nil, err
				}
				if req.MessageType == dhcpv6.MessageTypeSolicit {
					return answer(req, dhcpv6.MessageTypeAdvertise, iapd), nil
				}
				status := &dhcpv6.OptStatusCode{StatusCode: iana.StatusUnspecFail, StatusMessage: "backend down"}
				return answer(req, dhcpv6.MessageTypeReply, iapd, status), nil
			},
			wantErr: "backend down",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, _ := newWireTestReceiver(t, tt.respond)

			err := r.acquirePrefix(context.Background())
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %q, want it to mention %q", err, tt.wantErr)
			}
			if r.CurrentPrefix() != nil {
				t.Error("a rejected REPLY must not record a prefix")
			}
		})
	}
}

func TestDHCPv6PDRenewKeepsTheSamePrefix(t *testing.T) {
	r, client := newWireTestReceiver(t, delegating("2001:db8:beef::/56"))

	if err := r.acquirePrefix(context.Background()); err != nil {
		t.Fatalf("acquirePrefix() = %v", err)
	}
	drainEvents(r)

	if err := r.renewPrefix(context.Background()); err != nil {
		t.Fatalf("renewPrefix() = %v", err)
	}

	renews := client.sentOfType(dhcpv6.MessageTypeRenew)
	if len(renews) != 1 {
		t.Fatalf("sent %d RENEWs, want 1", len(renews))
	}
	if renews[0].Options.ServerID() == nil {
		t.Error("a RENEW goes to the server holding the binding and must name it")
	}
	if e := lastEvent(t, r); e.Type != EventTypeRenewed {
		t.Fatalf("event = %v, want %v", e.Type, EventTypeRenewed)
	}
	if got := r.CurrentPrefix(); got == nil || got.Network.String() != "2001:db8:beef::/56" {
		t.Fatalf("prefix after renewal = %v, want it unchanged", got)
	}
}

// The prefix changing under a renewal is the whole reason this operator exists.
func TestDHCPv6PDRenewReportsANewPrefixAsChanged(t *testing.T) {
	delegated := "2001:db8:1::/56"
	r, _ := newWireTestReceiver(t, func(req *dhcpv6.Message) (*dhcpv6.Message, error) {
		return delegating(delegated)(req)
	})

	if err := r.acquirePrefix(context.Background()); err != nil {
		t.Fatalf("acquirePrefix() = %v", err)
	}
	drainEvents(r)

	delegated = "2001:db8:2::/56"
	if err := r.renewPrefix(context.Background()); err != nil {
		t.Fatalf("renewPrefix() = %v", err)
	}

	if e := lastEvent(t, r); e.Type != EventTypeChanged {
		t.Fatalf("event = %v, want %v", e.Type, EventTypeChanged)
	}
	if got := r.CurrentPrefix(); got == nil || got.Network.String() != "2001:db8:2::/56" {
		t.Fatalf("prefix = %v, want 2001:db8:2::/56", got)
	}
}

// REBIND is what a client falls back to when the server it renewed with has
// gone away, so it carries no server ID and takes the identity of whoever
// answers.
func TestDHCPv6PDRebindAcceptsAnotherServer(t *testing.T) {
	r, client := newWireTestReceiver(t, delegating("2001:db8:beef::/56"))
	if err := r.acquirePrefix(context.Background()); err != nil {
		t.Fatalf("acquirePrefix() = %v", err)
	}
	drainEvents(r)

	client.mu.Lock()
	client.respond = func(req *dhcpv6.Message) (*dhcpv6.Message, error) {
		if req.MessageType == dhcpv6.MessageTypeRenew {
			return nil, errors.New("timed out")
		}
		return delegating("2001:db8:beef::/56")(req)
	}
	client.mu.Unlock()

	if err := r.renewPrefix(context.Background()); err == nil {
		t.Fatal("expected the renewal to fail")
	}
	if err := r.rebindPrefix(context.Background()); err != nil {
		t.Fatalf("rebindPrefix() = %v", err)
	}

	rebinds := client.sentOfType(dhcpv6.MessageTypeRebind)
	if len(rebinds) != 1 {
		t.Fatalf("sent %d REBINDs, want 1", len(rebinds))
	}
	if rebinds[0].Options.ServerID() != nil {
		t.Error("a REBIND must not name a server; it is addressed to any of them")
	}
	if got := r.CurrentPrefix(); got == nil {
		t.Fatal("the rebound lease recorded no prefix")
	}
}
