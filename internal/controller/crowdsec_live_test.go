/*
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

package controller

import (
	"context"
	"os"
	"slices"
	"testing"
)

// TestCrowdSecLAPILive runs the marker calls against a real LAPI. Skipped
// unless CROWDSEC_LAPI_URL is set; the remaining CROWDSEC_* variables carry
// the credentials (see CrowdSecLAPIConfig). Example, with a throwaway
// container:
//
//	docker run -d --name cs-test --cpus 1 -p 127.0.0.1:18080:8080 \
//	  -e DISABLE_ONLINE_API=true crowdsecurity/crowdsec:v1.8.2
//	docker exec cs-test cscli machines add operator --password <pw> -f /dev/null
//	CROWDSEC_LAPI_URL=http://127.0.0.1:18080 CROWDSEC_MACHINE_ID=operator \
//	  CROWDSEC_PASSWORD_FILE=<file> go test ./internal/controller -run Live
func TestCrowdSecLAPILive(t *testing.T) {
	cfg := CrowdSecLAPIConfig{
		URL:          os.Getenv("CROWDSEC_LAPI_URL"),
		CertFile:     os.Getenv("CROWDSEC_CERT_FILE"),
		KeyFile:      os.Getenv("CROWDSEC_KEY_FILE"),
		CAFile:       os.Getenv("CROWDSEC_CA_FILE"),
		MachineID:    os.Getenv("CROWDSEC_MACHINE_ID"),
		PasswordFile: os.Getenv("CROWDSEC_PASSWORD_FILE"),
	}
	if cfg.URL == "" {
		t.Skip("CROWDSEC_LAPI_URL not set")
	}
	api, err := NewCrowdSecLAPI(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	const owner, a, b = "live-test", "2001:db8:77:1::/64", "2001:db8:77:2::/64"
	values := func() []string {
		t.Helper()
		markers, err := api.listMarkers(ctx)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		var out []string
		for _, m := range markers {
			if m.Owner == owner {
				out = append(out, m.Value)
			}
		}
		slices.Sort(out)
		return out
	}
	t.Cleanup(func() {
		_ = api.deleteMarker(ctx, a)
		_ = api.deleteMarker(ctx, b)
	})

	for _, p := range []string{a, b} {
		if err := api.createMarker(ctx, owner, p); err != nil {
			t.Fatalf("create %s: %v", p, err)
		}
	}
	if got := values(); !slices.Equal(got, []string{a, b}) {
		t.Fatalf("after create: %q", got)
	}
	if err := api.deleteMarker(ctx, a); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got := values(); !slices.Equal(got, []string{b}) {
		t.Fatalf("after delete: %q", got)
	}
}
