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
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"time"
)

const (
	// CrowdSecMarkerScenario names the alerts and decisions the operator writes.
	// A LAPI profile matches on it, so it is a contract with that profile.
	CrowdSecMarkerScenario = "dynamic-prefix/lan-allow"

	// crowdsecMarkerOrigin must be cscli: decisions of that origin count as
	// manual, and manual decisions are only shared with CrowdSec's central API
	// when share_manual_decisions is on. Any other origin would publish the
	// LAN prefix as a community signal.
	crowdsecMarkerOrigin = "cscli"

	// crowdsecMarkerDuration is far longer than any ban, which is what lets a
	// profile tell a marker from a ban by its remaining lifetime.
	crowdsecMarkerDuration = "87600h"

	// crowdsecOwnerMetaKey records, on each marker alert, which DynamicPrefix
	// it belongs to, so one DynamicPrefix never expires another's markers.
	crowdsecOwnerMetaKey = "dynamic_prefix"

	crowdsecDecisionAllow = "allow"
	crowdsecScopeRange    = "Range"
	crowdsecParamScenario = "scenario"
	schemeHTTPS           = "https"

	crowdsecRequestTimeout = 10 * time.Second
	// crowdsecTokenSlack renews the JWT this long before LAPI says it expires.
	crowdsecTokenSlack = time.Minute
)

// crowdsecUserAgent must be exactly "<name>/<version>": LAPI refuses a login
// whose User-Agent does not split into two parts at "/".
var crowdsecUserAgent = func() string {
	version := "unknown"
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		version = strings.ReplaceAll(info.Main.Version, "/", "_")
	}
	return "dynamic-prefix-operator/" + version
}()

// errCrowdSecUnauthorized is a 401 from LAPI: the cached token expired early,
// or the machine's credentials were revoked.
var errCrowdSecUnauthorized = errors.New("crowdsec LAPI: unauthorized")

// CrowdSecLAPIConfig configures the LAPI client. Credentials are file paths,
// read on use, so a rotated certificate or password is picked up without a
// restart.
type CrowdSecLAPIConfig struct {
	// URL is the LAPI base URL, for example https://crowdsec-service.crowdsec:8080.
	URL string
	// CertFile and KeyFile hold the TLS client certificate. LAPI registers a
	// machine named <CN>@<client IP> on first login and requires its OU to be
	// one of the configured agents_allowed_ou.
	CertFile string
	KeyFile  string
	// CAFile verifies LAPI's server certificate. Empty uses the system roots.
	CAFile string
	// MachineID and PasswordFile log in with a password instead of a client
	// certificate.
	MachineID    string
	PasswordFile string
}

// Validate rejects a configuration that cannot authenticate.
func (c CrowdSecLAPIConfig) Validate() error {
	u, err := url.Parse(c.URL)
	if err != nil || u.Host == "" || (u.Scheme != schemeHTTPS && u.Scheme != "http") {
		return fmt.Errorf("LAPI URL %q must be an absolute http(s) URL", c.URL)
	}
	clientCert := c.CertFile != "" || c.KeyFile != ""
	password := c.MachineID != "" || c.PasswordFile != ""
	switch {
	case clientCert && password:
		return errors.New("configure either a client certificate or a machine password, not both")
	case clientCert && (c.CertFile == "" || c.KeyFile == ""):
		return errors.New("a client certificate needs both a certificate and a key file")
	case password && (c.MachineID == "" || c.PasswordFile == ""):
		return errors.New("password login needs both a machine ID and a password file")
	case !clientCert && !password:
		return errors.New("no LAPI credentials configured")
	case clientCert && u.Scheme != schemeHTTPS:
		return errors.New("a client certificate needs an https LAPI URL")
	}
	return nil
}

// crowdsecMarker is one active allow decision written for a prefix.
type crowdsecMarker struct {
	Value string
	Owner string
}

// CrowdSecLAPI talks to the CrowdSec Local API as a machine (watcher).
type CrowdSecLAPI struct {
	cfg  CrowdSecLAPIConfig
	base *url.URL
	http *http.Client

	mu      sync.Mutex
	token   string
	expires time.Time
}

// NewCrowdSecLAPI returns a client for cfg, which must have passed Validate.
func NewCrowdSecLAPI(cfg CrowdSecLAPIConfig) (*CrowdSecLAPI, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	base, _ := url.Parse(strings.TrimSuffix(cfg.URL, "/"))
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	// Short, so the next request after a quiet spell dials anew and presents
	// the certificate currently on disk.
	transport.IdleConnTimeout = 30 * time.Second
	if base.Scheme == schemeHTTPS {
		dialer := &net.Dialer{Timeout: crowdsecRequestTimeout}
		transport.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			// Built per connection: cert-manager rewrites the mounted files on
			// renewal, and reading them here is what picks the new ones up.
			tlsConfig, err := cfg.tlsConfig()
			if err != nil {
				return nil, err
			}
			return (&tls.Dialer{NetDialer: dialer, Config: tlsConfig}).DialContext(ctx, network, addr)
		}
	}
	return &CrowdSecLAPI{
		cfg:  cfg,
		base: base,
		http: &http.Client{
			Timeout:   crowdsecRequestTimeout,
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

func (c CrowdSecLAPIConfig) tlsConfig() (*tls.Config, error) {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if c.CAFile != "" {
		pem, err := os.ReadFile(c.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read LAPI CA bundle: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("LAPI CA bundle %s holds no PEM certificate", c.CAFile)
		}
		tlsConfig.RootCAs = pool
	}
	if c.CertFile != "" {
		cert, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("load LAPI client certificate: %w", err)
		}
		tlsConfig.Certificates = []tls.Certificate{cert}
	}
	return tlsConfig, nil
}

// login fetches a JWT. Its errors carry the status only: the request body
// holds the password, and nothing of the exchange is worth logging.
func (c *CrowdSecLAPI) login(ctx context.Context) (string, error) {
	body := map[string]any{"scenarios": []string{}}
	if c.cfg.PasswordFile != "" {
		raw, err := os.ReadFile(c.cfg.PasswordFile)
		if err != nil {
			return "", fmt.Errorf("read LAPI password file: %w", err)
		}
		body["machine_id"] = c.cfg.MachineID
		body["password"] = strings.TrimSpace(string(raw))
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	resp, err := c.send(ctx, http.MethodPost, "/v1/watchers/login", nil, payload, "")
	if err != nil {
		return "", fmt.Errorf("LAPI login: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("LAPI login: %s", resp.Status)
	}
	var out struct {
		Token  string    `json:"token"`
		Expire time.Time `json:"expire"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out); err != nil || out.Token == "" {
		return "", errors.New("LAPI login: response carries no token")
	}
	c.mu.Lock()
	c.token, c.expires = out.Token, out.Expire.Add(-crowdsecTokenSlack)
	c.mu.Unlock()
	return out.Token, nil
}

func (c *CrowdSecLAPI) cachedToken() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token == "" || time.Now().After(c.expires) {
		return ""
	}
	return c.token
}

func (c *CrowdSecLAPI) dropToken() {
	c.mu.Lock()
	c.token = ""
	c.mu.Unlock()
}

func (c *CrowdSecLAPI) send(
	ctx context.Context, method, path string, query url.Values, body []byte, token string,
) (*http.Response, error) {
	u := *c.base
	u.Path += path
	u.RawQuery = query.Encode()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", crowdsecUserAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return c.http.Do(req)
}

// do sends an authenticated request and decodes a 2xx JSON answer into out.
// A 401 drops the cached token and retries once with a fresh login.
func (c *CrowdSecLAPI) do(ctx context.Context, method, path string, query url.Values, body []byte, out any) error {
	for attempt := 0; ; attempt++ {
		token := c.cachedToken()
		if token == "" {
			var err error
			if token, err = c.login(ctx); err != nil {
				return err
			}
		}
		err := c.doOnce(ctx, method, path, query, body, token, out)
		if errors.Is(err, errCrowdSecUnauthorized) && attempt == 0 {
			c.dropToken()
			continue
		}
		return err
	}
}

func (c *CrowdSecLAPI) doOnce(
	ctx context.Context, method, path string, query url.Values, body []byte, token string, out any,
) error {
	resp, err := c.send(ctx, method, path, query, body, token)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	limited := io.LimitReader(resp.Body, 4<<20)
	if resp.StatusCode == http.StatusUnauthorized {
		_, _ = io.Copy(io.Discard, limited)
		return errCrowdSecUnauthorized
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg, _ := io.ReadAll(io.LimitReader(limited, 512))
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(msg)))
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, limited)
		return nil
	}
	if err := json.NewDecoder(limited).Decode(out); err != nil {
		return fmt.Errorf("%s %s: decode response: %w", method, path, err)
	}
	return nil
}

type lapiDecision struct {
	Type     string `json:"type"`
	Scope    string `json:"scope"`
	Value    string `json:"value"`
	Origin   string `json:"origin"`
	Scenario string `json:"scenario"`
	Duration string `json:"duration"`
}

type lapiMeta struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type lapiAlert struct {
	Decisions []lapiDecision `json:"decisions"`
	Meta      []lapiMeta     `json:"meta"`
}

// listMarkers returns every active marker decision. LAPI filters on scenario,
// origin, type and scope as well as on active decisions, and the answer is
// checked again here, since a filter LAPI did not recognise would otherwise
// widen the list silently.
func (c *CrowdSecLAPI) listMarkers(ctx context.Context) ([]crowdsecMarker, error) {
	query := url.Values{
		crowdsecParamScenario: {CrowdSecMarkerScenario},
		"origin":              {crowdsecMarkerOrigin},
		"decision_type":       {crowdsecDecisionAllow},
		"scope":               {crowdsecScopeRange},
		"has_active_decision": {"true"},
		"limit":               {"0"},
	}
	var alerts []lapiAlert
	if err := c.do(ctx, http.MethodGet, "/v1/alerts", query, nil, &alerts); err != nil {
		return nil, err
	}
	var markers []crowdsecMarker
	for _, alert := range alerts {
		owner := ""
		for _, m := range alert.Meta {
			if m.Key == crowdsecOwnerMetaKey {
				owner = m.Value
			}
		}
		for _, d := range alert.Decisions {
			// LAPI reports the remaining lifetime; an expired decision's is negative.
			remaining, err := time.ParseDuration(d.Duration)
			if err != nil || remaining <= 0 || d.Type != crowdsecDecisionAllow || d.Scope != crowdsecScopeRange ||
				d.Origin != crowdsecMarkerOrigin || d.Scenario != CrowdSecMarkerScenario {
				continue
			}
			markers = append(markers, crowdsecMarker{Value: d.Value, Owner: owner})
		}
	}
	return markers, nil
}

// createMarker writes one allow decision for cidr, owned by dynamicPrefix.
// Decisions supplied with an alert bypass the LAPI profiles, so the marker is
// stored as given.
func (c *CrowdSecLAPI) createMarker(ctx context.Context, dynamicPrefix, cidr string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	alert := map[string]any{
		crowdsecParamScenario: CrowdSecMarkerScenario,
		"scenario_hash":       "",
		"scenario_version":    "",
		"message":             fmt.Sprintf("trusted LAN prefix %s of DynamicPrefix %s", cidr, dynamicPrefix),
		"events_count":        1,
		"events":              []any{},
		"start_at":            now,
		"stop_at":             now,
		"capacity":            0,
		"leakspeed":           "0",
		"simulated":           false,
		"remediation":         false,
		"source":              map[string]string{"scope": crowdsecScopeRange, "value": cidr},
		"meta":                []lapiMeta{{Key: crowdsecOwnerMetaKey, Value: dynamicPrefix}},
		"decisions": []lapiDecision{{
			Type:     crowdsecDecisionAllow,
			Scope:    crowdsecScopeRange,
			Value:    cidr,
			Origin:   crowdsecMarkerOrigin,
			Scenario: CrowdSecMarkerScenario,
			Duration: crowdsecMarkerDuration,
		}},
	}
	payload, err := json.Marshal([]any{alert})
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPost, "/v1/alerts", nil, payload, nil)
}

// markerDeleteQuery is the only filter the operator ever deletes with. LAPI
// expires every active decision matching all of it, so a missing parameter
// widens the match: each one is required.
func markerDeleteQuery(cidr string) (url.Values, error) {
	if cidr == "" {
		return nil, errors.New("refusing a decision delete without a value")
	}
	return url.Values{
		"type":                {crowdsecDecisionAllow},
		"scopes":              {crowdsecScopeRange}, // plural: the delete filter rejects "scope"
		"value":               {cidr},
		"origin":              {crowdsecMarkerOrigin},
		crowdsecParamScenario: {CrowdSecMarkerScenario},
	}, nil
}

// deleteMarker expires the marker decisions for cidr.
func (c *CrowdSecLAPI) deleteMarker(ctx context.Context, cidr string) error {
	query, err := markerDeleteQuery(cidr)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodDelete, "/v1/decisions", query, nil, nil)
}
