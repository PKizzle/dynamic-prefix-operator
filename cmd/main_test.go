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
