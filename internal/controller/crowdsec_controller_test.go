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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dynamicprefixiov1alpha1 "github.com/pkizzle/dynamic-prefix-operator/api/v1alpha1"
)

const (
	csCurrent  = "2001:db8:1:2::/64"
	csDraining = "2001:db8:1:1::/64"
	csExpired  = "2001:db8:1::/64"
	csPassword = "s3cret-password"
)

type fakeMarker struct {
	value, owner string
	active       bool
}

// fakeLAPI imitates the LAPI endpoints the operator calls, asserting the
// filters it is sent.
type fakeLAPI struct {
	t  *testing.T
	mu sync.Mutex

	markers    []fakeMarker
	token      string
	logins     int
	requests   []string // "METHOD path" in arrival order, logins excluded
	failCreate bool
	clientCNs  []string
}

const (
	fakeListRequest   = "GET /v1/alerts"
	fakeCreateRequest = "POST /v1/alerts"
	fakeDeleteRequest = "DELETE /v1/decisions"
	fakeLAPIURL       = "https://lapi:8080"
)

func (f *fakeLAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
		f.clientCNs = append(f.clientCNs, r.TLS.PeerCertificates[0].Subject.CommonName)
	}
	if r.URL.Path == "/v1/watchers/login" {
		f.login(w, r)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+f.token || f.token == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	switch r.Method + " " + r.URL.Path {
	case fakeListRequest:
		f.list(w, r)
	case fakeCreateRequest:
		f.create(w, r)
	case fakeDeleteRequest:
		f.delete(w, r)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeLAPI) login(w http.ResponseWriter, r *http.Request) {
	// LAPI answers 401 to a login whose User-Agent is not "<name>/<version>".
	if len(strings.Split(r.UserAgent(), "/")) != 2 {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	if r.TLS == nil && (body["machine_id"] != "operator" || body["password"] != csPassword) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	f.logins++
	f.token = fmt.Sprintf("tok-%d", f.logins)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"code": 200, "token": f.token, "expire": time.Now().Add(time.Hour).Format(time.RFC3339),
	})
}

func (f *fakeLAPI) list(w http.ResponseWriter, r *http.Request) {
	want := "decision_type=allow&has_active_decision=true&limit=0&origin=cscli" +
		"&scenario=dynamic-prefix%2Flan-allow&scope=Range"
	if r.URL.RawQuery != want {
		f.t.Errorf("list query = %s, want %s", r.URL.RawQuery, want)
	}
	alerts := make([]map[string]any, 0, len(f.markers))
	for _, m := range f.markers {
		duration := "87599h0m0s"
		if !m.active {
			duration = "-1h0m0s"
		}
		alert := map[string]any{"decisions": []lapiDecision{{
			Type: crowdsecDecisionAllow, Scope: crowdsecScopeRange, Value: m.value, Origin: crowdsecMarkerOrigin,
			Scenario: CrowdSecMarkerScenario, Duration: duration,
		}}}
		if m.owner != "" {
			alert["meta"] = []lapiMeta{{Key: crowdsecOwnerMetaKey, Value: m.owner}}
		}
		alerts = append(alerts, alert)
	}
	_ = json.NewEncoder(w).Encode(alerts)
}

func (f *fakeLAPI) create(w http.ResponseWriter, r *http.Request) {
	if f.failCreate {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	var alerts []struct {
		Remediation bool           `json:"remediation"`
		Decisions   []lapiDecision `json:"decisions"`
		Meta        []lapiMeta     `json:"meta"`
	}
	if err := json.NewDecoder(r.Body).Decode(&alerts); err != nil || len(alerts) != 1 ||
		len(alerts[0].Decisions) != 1 || len(alerts[0].Meta) != 1 || alerts[0].Remediation {
		f.t.Errorf("unexpected marker body: %v %+v", err, alerts)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	d := alerts[0].Decisions[0]
	if d.Type != crowdsecDecisionAllow || d.Scope != crowdsecScopeRange || d.Origin != "cscli" ||
		d.Scenario != CrowdSecMarkerScenario || d.Duration != "87600h" {
		f.t.Errorf("unexpected marker decision: %+v", d)
	}
	f.markers = append(f.markers, fakeMarker{value: d.Value, owner: alerts[0].Meta[0].Value, active: true})
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write([]byte(`["1"]`))
}

func (f *fakeLAPI) delete(w http.ResponseWriter, r *http.Request) {
	value := r.URL.Query().Get("value")
	want := "origin=cscli&scenario=dynamic-prefix%2Flan-allow&scopes=Range&type=allow&value=" +
		url.QueryEscape(value)
	if r.URL.RawQuery != want || value == "" {
		f.t.Errorf("delete query = %s, want %s", r.URL.RawQuery, want)
	}
	f.requests[len(f.requests)-1] += " " + value
	n := 0
	for i := range f.markers {
		if f.markers[i].value == value && f.markers[i].active {
			f.markers[i].active = false
			n++
		}
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"nbDeleted": fmt.Sprint(n)})
}

func (f *fakeLAPI) active() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, m := range f.markers {
		if m.active {
			out = append(out, m.value+"@"+m.owner)
		}
	}
	slices.Sort(out)
	return out
}

func (f *fakeLAPI) takeRequests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.requests
	f.requests = nil
	return out
}

type crowdsecFixture struct {
	lapi     *fakeLAPI
	server   *httptest.Server
	client   client.Client
	recorder *events.FakeRecorder
	r        *CrowdSecMarkerReconciler
}

func newCrowdSecFixture(t *testing.T, objs ...client.Object) *crowdsecFixture {
	t.Helper()
	lapi := &fakeLAPI{t: t}
	server := httptest.NewServer(lapi)
	t.Cleanup(server.Close)

	pw := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(pw, []byte(csPassword+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	api, err := NewCrowdSecLAPI(CrowdSecLAPIConfig{URL: server.URL, MachineID: "operator", PasswordFile: pw})
	if err != nil {
		t.Fatal(err)
	}

	scheme := newPoolBackendTestScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&dynamicprefixiov1alpha1.DynamicPrefix{}).
		WithTypeConverters(testTypeConverters(scheme)...).
		Build()
	recorder := events.NewFakeRecorder(32)
	return &crowdsecFixture{
		lapi: lapi, server: server, client: c, recorder: recorder,
		r: &CrowdSecMarkerReconciler{
			Client: c, Recorder: recorder, LAPI: api,
			DynamicPrefixes: []string{testDynamicPrefixName},
			MaxMarkers:      DefaultCrowdSecMaxMarkers,
		},
	}
}

func (f *crowdsecFixture) reconcile(t *testing.T) error {
	t.Helper()
	_, err := f.r.Reconcile(context.Background(),
		ctrl.Request{NamespacedName: types.NamespacedName{Name: testDynamicPrefixName}})
	return err
}

func (f *crowdsecFixture) events() []string {
	var out []string
	for {
		select {
		case e := <-f.recorder.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

func crowdsecDP(
	current string, history ...dynamicprefixiov1alpha1.PrefixHistoryEntry,
) *dynamicprefixiov1alpha1.DynamicPrefix {
	return &dynamicprefixiov1alpha1.DynamicPrefix{
		ObjectMeta: metav1.ObjectMeta{Name: testDynamicPrefixName},
		Status:     dynamicprefixiov1alpha1.DynamicPrefixStatus{CurrentPrefix: current, History: history},
	}
}

func historyEntry(p string, state dynamicprefixiov1alpha1.PrefixState) dynamicprefixiov1alpha1.PrefixHistoryEntry {
	return dynamicprefixiov1alpha1.PrefixHistoryEntry{Prefix: p, State: state}
}

func TestCrowdSecMarkersRotation(t *testing.T) {
	dp := crowdsecDP(csCurrent,
		historyEntry(csExpired, dynamicprefixiov1alpha1.PrefixStateExpired),
		historyEntry(csDraining, dynamicprefixiov1alpha1.PrefixStateDraining))
	f := newCrowdSecFixture(t, dp)
	f.lapi.markers = []fakeMarker{
		{value: csDraining, owner: testDynamicPrefixName, active: true},
		{value: csExpired, owner: testDynamicPrefixName, active: true},
		// Someone else's marker for an unrelated range is never touched.
		{value: "2001:db8:ff::/64", owner: "", active: true},
		// Expired already: not a marker any more, so the current one is recreated.
		{value: csCurrent, owner: testDynamicPrefixName, active: false},
	}

	if err := f.reconcile(t); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	want := []string{fakeListRequest, fakeCreateRequest, fakeDeleteRequest + " " + csExpired}
	if got := f.lapi.takeRequests(); !slices.Equal(got, want) {
		t.Fatalf("requests = %q, want %q (create before delete)", got, want)
	}
	wantActive := []string{csDraining + "@" + testDynamicPrefixName, csCurrent + "@" + testDynamicPrefixName,
		"2001:db8:ff::/64@"}
	slices.Sort(wantActive)
	if got := f.lapi.active(); !slices.Equal(got, wantActive) {
		t.Fatalf("active markers = %q, want %q", got, wantActive)
	}
	evs := strings.Join(f.events(), "\n")
	if !strings.Contains(evs, eventReasonCrowdSecMarkerCreated) ||
		!strings.Contains(evs, eventReasonCrowdSecMarkerRemoved) {
		t.Errorf("events = %s", evs)
	}

	var got dynamicprefixiov1alpha1.DynamicPrefix
	if err := f.client.Get(context.Background(), types.NamespacedName{Name: testDynamicPrefixName}, &got); err != nil {
		t.Fatal(err)
	}
	if c := meta.FindStatusCondition(got.Status.Conditions,
		dynamicprefixiov1alpha1.ConditionTypeCrowdSecMarkersSynced); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("condition = %+v, want True", c)
	}
	if v := testutil.ToFloat64(crowdsecMarkers.WithLabelValues(testDynamicPrefixName)); v != 2 {
		t.Errorf("marker gauge = %v, want 2", v)
	}

	// A second trigger with nothing changed writes nothing.
	if err := f.reconcile(t); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if got := f.lapi.takeRequests(); !slices.Equal(got, []string{fakeListRequest}) {
		t.Fatalf("second pass requests = %q, want only the list", got)
	}
	if f.lapi.logins != 1 {
		t.Errorf("logins = %d, want the token reused", f.lapi.logins)
	}
}

func TestCrowdSecReloginOn401(t *testing.T) {
	f := newCrowdSecFixture(t, crowdsecDP(csCurrent))
	if err := f.reconcile(t); err != nil {
		t.Fatal(err)
	}
	f.lapi.mu.Lock()
	f.lapi.token = "revoked"
	f.lapi.mu.Unlock()
	if err := f.reconcile(t); err != nil {
		t.Fatalf("reconcile after token revocation: %v", err)
	}
	if f.lapi.logins != 2 {
		t.Errorf("logins = %d, want a re-login after the 401", f.lapi.logins)
	}
}

func TestCrowdSecCreateFailureDeletesNothing(t *testing.T) {
	f := newCrowdSecFixture(t, crowdsecDP(csCurrent))
	f.lapi.markers = []fakeMarker{{value: csDraining, owner: testDynamicPrefixName, active: true}}
	f.lapi.failCreate = true
	before := testutil.ToFloat64(crowdsecSyncTotal.WithLabelValues(testDynamicPrefixName, "failed"))

	if err := f.reconcile(t); err == nil {
		t.Fatal("want an error so the workqueue retries with backoff")
	}
	if got := f.lapi.takeRequests(); !slices.Equal(got, []string{fakeListRequest, fakeCreateRequest}) {
		t.Fatalf("requests = %q: the old marker must outlive a failed create", got)
	}
	if !strings.Contains(strings.Join(f.events(), "\n"), eventReasonCrowdSecMarkerFailed) {
		t.Error("no failure event")
	}
	if after := testutil.ToFloat64(crowdsecSyncTotal.WithLabelValues(testDynamicPrefixName, "failed")); after != before+1 {
		t.Errorf("failed counter %v -> %v", before, after)
	}

	f.lapi.failCreate = false
	if err := f.reconcile(t); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if got := f.lapi.active(); !slices.Equal(got, []string{csCurrent + "@" + testDynamicPrefixName}) {
		t.Errorf("active after retry = %q", got)
	}
}

func TestCrowdSecRefusesInvalidPrefixes(t *testing.T) {
	cases := map[string]*dynamicprefixiov1alpha1.DynamicPrefix{
		"not canonical": crowdsecDP("2001:db8:1:2::1/64"),
		"too short":     crowdsecDP("2001:db8::/32"),
		"too long":      crowdsecDP("2001:db8:1:2::/80"),
		"not global":    crowdsecDP("fd00:1:2:3::/64"),
		"ipv4":          crowdsecDP("192.0.2.0/24"),
		"uppercase":     crowdsecDP("2001:DB8:1:2::/64"),
		"garbage":       crowdsecDP("not-a-prefix"),
		"too many": crowdsecDP(csCurrent,
			historyEntry("2001:db8:1:3::/64", dynamicprefixiov1alpha1.PrefixStateDraining),
			historyEntry("2001:db8:1:4::/64", dynamicprefixiov1alpha1.PrefixStateDraining),
			historyEntry("2001:db8:1:5::/64", dynamicprefixiov1alpha1.PrefixStateDraining)),
		"bad draining": crowdsecDP(csCurrent, historyEntry("::/0", dynamicprefixiov1alpha1.PrefixStateDraining)),
	}
	for name, dp := range cases {
		t.Run(name, func(t *testing.T) {
			f := newCrowdSecFixture(t, dp)
			if err := f.reconcile(t); err != nil {
				t.Fatalf("a refusal is not retried: %v", err)
			}
			if got := f.lapi.takeRequests(); len(got) != 0 || f.lapi.logins != 0 {
				t.Fatalf("LAPI was contacted: %q, logins %d", got, f.lapi.logins)
			}
			if !strings.Contains(strings.Join(f.events(), "\n"), "Warning "+eventReasonCrowdSecMarkerRefused) {
				t.Error("no refusal warning event")
			}
		})
	}
}

func TestCrowdSecWaitsForPrefixAndReadyLAPI(t *testing.T) {
	f := newCrowdSecFixture(t, crowdsecDP(""))
	if err := f.reconcile(t); err != nil || len(f.lapi.takeRequests()) != 0 {
		t.Fatalf("without a prefix nothing is sent (err %v)", err)
	}

	slice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{Namespace: "crowdsec", Name: "lapi-abc",
			Labels: map[string]string{discoveryv1.LabelServiceName: "crowdsec-service"}},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints: []discoveryv1.Endpoint{{Addresses: []string{"10.0.0.1"},
			Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(false)}}},
	}
	f = newCrowdSecFixture(t, crowdsecDP(csCurrent), slice)
	f.r.LAPIService = &types.NamespacedName{Namespace: "crowdsec", Name: "crowdsec-service"}
	if err := f.reconcile(t); err != nil || len(f.lapi.takeRequests()) != 0 {
		t.Fatalf("with no ready LAPI endpoint nothing is sent (err %v)", err)
	}
	slice.Endpoints[0].Conditions.Ready = ptr.To(true)
	if err := f.client.Update(context.Background(), slice); err != nil {
		t.Fatal(err)
	}
	if err := f.reconcile(t); err != nil {
		t.Fatal(err)
	}
	if got := f.lapi.active(); !slices.Equal(got, []string{csCurrent + "@" + testDynamicPrefixName}) {
		t.Errorf("active = %q after LAPI became ready", got)
	}
	if reqs := f.r.allDynamicPrefixes(context.Background(), slice); len(reqs) != 1 ||
		reqs[0].Name != testDynamicPrefixName {
		t.Errorf("EndpointSlice change enqueues %v", reqs)
	}
}

func TestCrowdSecDeletedDynamicPrefixExpiresItsMarkers(t *testing.T) {
	f := newCrowdSecFixture(t)
	f.lapi.markers = []fakeMarker{
		{value: csCurrent, owner: testDynamicPrefixName, active: true},
		{value: csDraining, owner: "other", active: true},
	}
	if err := f.reconcile(t); err != nil {
		t.Fatal(err)
	}
	if got := f.lapi.active(); !slices.Equal(got, []string{csDraining + "@other"}) {
		t.Errorf("active = %q", got)
	}
}

func TestCrowdSecIgnoresUnconfiguredDynamicPrefix(t *testing.T) {
	f := newCrowdSecFixture(t, crowdsecDP(csCurrent))
	f.r.DynamicPrefixes = []string{"another"}
	if err := f.reconcile(t); err != nil || len(f.lapi.takeRequests()) != 0 {
		t.Fatalf("an unconfigured DynamicPrefix is not synced (err %v)", err)
	}
}

func TestMarkerDeleteQueryNeverEmpty(t *testing.T) {
	if _, err := markerDeleteQuery(""); err == nil {
		t.Fatal("an empty value must be refused")
	}
	q, err := markerDeleteQuery(csCurrent)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"type", "scopes", "value", "origin", "scenario"} {
		if q.Get(k) == "" {
			t.Errorf("delete filter lacks %s", k)
		}
	}
}

func TestCrowdSecLAPIConfigValidate(t *testing.T) {
	ok := []CrowdSecLAPIConfig{
		{URL: fakeLAPIURL, CertFile: "c", KeyFile: "k", CAFile: "ca"},
		{URL: "http://lapi:8080", MachineID: "m", PasswordFile: "p"},
	}
	bad := []CrowdSecLAPIConfig{
		{URL: "lapi:8080", MachineID: "m", PasswordFile: "p"},
		{URL: fakeLAPIURL},
		{URL: fakeLAPIURL, CertFile: "c"},
		{URL: fakeLAPIURL, MachineID: "m"},
		{URL: fakeLAPIURL, CertFile: "c", KeyFile: "k", MachineID: "m", PasswordFile: "p"},
		{URL: "http://lapi:8080", CertFile: "c", KeyFile: "k"},
	}
	for _, c := range ok {
		if err := c.Validate(); err != nil {
			t.Errorf("%+v: %v", c, err)
		}
	}
	for _, c := range bad {
		if err := c.Validate(); err == nil {
			t.Errorf("%+v: want an error", c)
		}
	}
}

// TestCrowdSecClientCertificateRotation checks the client certificate is read
// per connection: a renewed file is presented on the next one, no restart.
func TestCrowdSecClientCertificateRotation(t *testing.T) {
	dir := t.TempDir()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, _ := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	caCert, _ := x509.ParseCertificate(caDER)
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	writePEM := func(name, typ string, der []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	issue := func(serial int64, cn string, server bool) ([]byte, []byte) {
		key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: cn, OrganizationalUnit: []string{"agent-ou"}},
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		}
		if server {
			tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
			tmpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
		}
		der, _ := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
		keyDER, _ := x509.MarshalPKCS8PrivateKey(key)
		return der, keyDER
	}
	caFile := writePEM("ca.crt", "CERTIFICATE", caDER)
	srvDER, srvKey := issue(2, "lapi", true)
	srvCert, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srvDER}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: srvKey}))
	if err != nil {
		t.Fatal(err)
	}

	lapi := &fakeLAPI{t: t}
	server := httptest.NewUnstartedServer(lapi)
	server.TLS = &tls.Config{Certificates: []tls.Certificate{srvCert}, ClientCAs: pool,
		ClientAuth: tls.RequireAndVerifyClientCert, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	t.Cleanup(server.Close)

	certDER, keyDER := issue(3, "operator-v1", false)
	certFile := writePEM("tls.crt", "CERTIFICATE", certDER)
	keyFile := writePEM("tls.key", "PRIVATE KEY", keyDER)
	api, err := NewCrowdSecLAPI(CrowdSecLAPIConfig{URL: server.URL, CertFile: certFile, KeyFile: keyFile, CAFile: caFile})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.listMarkers(context.Background()); err != nil {
		t.Fatalf("list over mTLS: %v", err)
	}

	certDER, keyDER = issue(4, "operator-v2", false)
	writePEM("tls.crt", "CERTIFICATE", certDER)
	writePEM("tls.key", "PRIVATE KEY", keyDER)
	api.http.CloseIdleConnections()
	if _, err := api.listMarkers(context.Background()); err != nil {
		t.Fatalf("list after rotation: %v", err)
	}
	lapi.mu.Lock()
	defer lapi.mu.Unlock()
	if !slices.Contains(lapi.clientCNs, "operator-v1") || lapi.clientCNs[len(lapi.clientCNs)-1] != "operator-v2" {
		t.Errorf("client certificates seen = %q, want the renewed one last", lapi.clientCNs)
	}
}
