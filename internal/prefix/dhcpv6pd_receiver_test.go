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
	"net"
	"testing"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv6"
)

// iaPrefixBytes builds the wire form of an IA_PREFIX option body so these
// tests exercise the real decoder rather than a hand-built struct: the decoder
// is what produces the awkward shapes (a nil Prefix, an out-of-range mask).
// Layout per RFC 8415 §21.22: preferred (4) | valid (4) | prefix-length (1) |
// prefix (16).
func iaPrefixBytes(preferred, valid uint32, prefixLength uint8, ip net.IP) []byte {
	b := make([]byte, 0, 25)
	be := func(v uint32) { b = append(b, byte(v>>24), byte(v>>16), byte(v>>8), byte(v)) }
	be(preferred)
	be(valid)
	b = append(b, prefixLength)
	return append(b, ip.To16()...)
}

func decodeIAPrefix(t *testing.T, body []byte) *dhcpv6.OptIAPrefix {
	t.Helper()
	opt := &dhcpv6.OptIAPrefix{}
	if err := opt.FromBytes(body); err != nil {
		t.Fatalf("decoding IA_PREFIX body: %v", err)
	}
	return opt
}

func replyWithPrefixes(iaid [4]byte, prefixes ...*dhcpv6.OptIAPrefix) *dhcpv6.Message {
	iapd := &dhcpv6.OptIAPD{IaId: iaid, T1: time.Hour, T2: 2 * time.Hour}
	for _, p := range prefixes {
		iapd.Options.Add(p)
	}
	msg := &dhcpv6.Message{MessageType: dhcpv6.MessageTypeReply}
	msg.AddOption(iapd)
	return msg
}

// A REPLY whose IA_PREFIX carries a zero prefix-length decodes to a nil
// Prefix even though the lifetimes are valid, because the lifetimes sit ahead
// of the length byte on the wire. Selecting on the lifetime alone used to
// dereference that nil in processIAPDReply; it runs in a bare goroutine with
// no recover, so the panic took the whole operator down.
func TestProcessIAPDReplyRejectsPrefixesInsteadOfPanicking(t *testing.T) {
	iaid := [4]byte{1, 2, 3, 4}
	ip := net.ParseIP("2001:db8::")

	tests := []struct {
		name         string
		prefixLength uint8
		wantErr      string
	}{
		{
			name:         "zero prefix length decodes to a nil prefix",
			prefixLength: 0,
			wantErr:      "no valid prefix in IA_PD",
		},
		{
			// net.CIDRMask returns nil above 128, and Size() then reports
			// (0, 0) -- previously accepted as a /0 over the whole space.
			name:         "prefix length above 128 yields an unusable mask",
			prefixLength: 200,
			wantErr:      "invalid prefix length in IA_PD",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opt := decodeIAPrefix(t, iaPrefixBytes(1800, 3600, tt.prefixLength, ip))
			r := NewDHCPv6PDReceiver("eth0", 56)

			err := r.processIAPDReply(replyWithPrefixes(iaid, opt), iaid, nil)

			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			if err.Error() != tt.wantErr {
				t.Fatalf("expected error %q, got %q", tt.wantErr, err)
			}
			if got := r.CurrentPrefix(); got != nil {
				t.Fatalf("a rejected REPLY must not set a prefix, got %v", got.Network)
			}
		})
	}
}

// A malformed prefix must not mask a usable one that follows it in the same
// IA_PD, and a well-formed REPLY must still be accepted.
func TestProcessIAPDReplyAcceptsValidPrefix(t *testing.T) {
	iaid := [4]byte{9, 9, 9, 9}
	unusable := decodeIAPrefix(t, iaPrefixBytes(1800, 3600, 0, net.ParseIP("2001:db8:dead::")))
	usable := decodeIAPrefix(t, iaPrefixBytes(1800, 3600, 56, net.ParseIP("2001:db8:1::")))

	r := NewDHCPv6PDReceiver("eth0", 56)
	if err := r.processIAPDReply(replyWithPrefixes(iaid, unusable, usable), iaid, nil); err != nil {
		t.Fatalf("expected the usable prefix to be accepted, got %v", err)
	}

	got := r.CurrentPrefix()
	if got == nil {
		t.Fatal("expected a prefix to be recorded")
	}
	if got.Network.String() != "2001:db8:1::/56" {
		t.Fatalf("expected 2001:db8:1::/56, got %v", got.Network)
	}
}

func TestNewDHCPv6PDReceiver(t *testing.T) {
	tests := []struct {
		name                  string
		iface                 string
		requestedPrefixLength int
		expectedPrefixLength  int
	}{
		{
			name:                  "With explicit prefix length",
			iface:                 "eth0",
			requestedPrefixLength: 48,
			expectedPrefixLength:  48,
		},
		{
			name:                  "With default prefix length",
			iface:                 "eth1",
			requestedPrefixLength: 0,
			expectedPrefixLength:  56, // Default
		},
		{
			name:                  "Custom prefix length /60",
			iface:                 "enp0s3",
			requestedPrefixLength: 60,
			expectedPrefixLength:  60,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewDHCPv6PDReceiver(tt.iface, tt.requestedPrefixLength)

			if r.iface != tt.iface {
				t.Errorf("iface = %s, want %s", r.iface, tt.iface)
			}

			if r.requestedPrefixLength != tt.expectedPrefixLength {
				t.Errorf("requestedPrefixLength = %d, want %d", r.requestedPrefixLength, tt.expectedPrefixLength)
			}

			if r.events == nil {
				t.Error("events channel should not be nil")
			}

			if r.stopCh == nil {
				t.Error("stopCh should not be nil")
			}
		})
	}
}

func TestDHCPv6PDReceiverSource(t *testing.T) {
	r := NewDHCPv6PDReceiver("eth0", 56)
	if r.Source() != SourceDHCPv6PD {
		t.Errorf("Source() = %v, want %v", r.Source(), SourceDHCPv6PD)
	}
}

func TestDHCPv6PDReceiverInitialState(t *testing.T) {
	r := NewDHCPv6PDReceiver("eth0", 56)

	if r.CurrentPrefix() != nil {
		t.Error("Expected CurrentPrefix() to be nil initially")
	}

	if r.Events() == nil {
		t.Error("Expected Events() channel to be non-nil")
	}
}

func TestDHCPv6PDReceiverEventChannel(t *testing.T) {
	r := NewDHCPv6PDReceiver("eth0", 56)

	events := r.Events()
	if cap(events) != 10 {
		t.Errorf("Events channel capacity = %d, want 10", cap(events))
	}
}

func TestDHCPv6PDReceiverStopWithoutStart(t *testing.T) {
	r := NewDHCPv6PDReceiver("eth0", 56)

	// Stop should not panic when called without Start
	err := r.Stop()
	if err != nil {
		t.Errorf("Stop() returned error: %v", err)
	}
}
