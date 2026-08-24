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

package main

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

func TestDefaultZapOptionsProduceStructuredJSONLogs(t *testing.T) {
	opts := defaultZapOptions()
	if opts.Development {
		t.Fatal("default zap options should use production mode")
	}

	var buf bytes.Buffer
	logger := zap.New(zap.UseFlagOptions(&opts), zap.WriteTo(&buf))
	logger.WithName("setup").Info("test message")

	raw := strings.TrimSpace(buf.String())
	if raw == "" {
		t.Fatal("expected logger output")
	}

	var entry struct {
		Level   string `json:"level"`
		Logger  string `json:"logger"`
		Message string `json:"msg"`
	}
	if err := json.Unmarshal([]byte(raw), &entry); err != nil {
		t.Fatalf("expected JSON log output, got %q: %v", raw, err)
	}

	if entry.Level != "info" {
		t.Fatalf("level = %q, want %q", entry.Level, "info")
	}
	if entry.Logger != "setup" {
		t.Fatalf("logger = %q, want %q", entry.Logger, "setup")
	}
	if entry.Message != "test message" {
		t.Fatalf("message = %q, want %q", entry.Message, "test message")
	}
}

func TestParseAnnotationKeyListAcceptsWellFormedKeys(t *testing.T) {
	keys, err := parseAnnotationKeyList(
		" external-dns.alpha.kubernetes.io/target , external-dns.kubernetes.io/target ,,external-dns.kubernetes.io/target")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"external-dns.alpha.kubernetes.io/target", "external-dns.kubernetes.io/target"}
	if !slices.Equal(keys, want) {
		t.Fatalf("keys = %q, want %q", keys, want)
	}
}

// A chart that quotes only the flag's value renders `- --flag="a,b"` as a YAML plain
// scalar, so the process receives the quotes as part of the value. Writing the keys that
// come out of that is an API-server rejection at rotation time, hours later; refusing them
// at startup is the difference between a pod that will not start and a name that silently
// stops resolving.
func TestParseAnnotationKeyListRejectsKeysWithQuotesAttached(t *testing.T) {
	if _, err := parseAnnotationKeyList(
		`"external-dns.alpha.kubernetes.io/target,external-dns.kubernetes.io/target"`); err == nil {
		t.Fatal("expected quoted flag value to be rejected")
	}
}

func TestParseAnnotationKeyListRejectsUnusableValues(t *testing.T) {
	for _, raw := range []string{"", " , ", "not a key", "external-dns.kubernetes.io/", "/target"} {
		if _, err := parseAnnotationKeyList(raw); err == nil {
			t.Fatalf("expected %q to be rejected", raw)
		}
	}
}
