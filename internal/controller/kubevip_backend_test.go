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

package controller

import (
	"context"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// testPoolPrefix is the delegation most of these cases start from.
const testPoolPrefix = "2001:db8:1::/64"

func kubevipConfigMap(t *testing.T, key string, data map[string]string) *unstructured.Unstructured {
	t.Helper()

	values := make(map[string]interface{}, len(data))
	for k, v := range data {
		values[k] = v
	}

	annotations := map[string]interface{}{AnnotationName: "home-ipv6"}
	if key != "" {
		annotations[AnnotationKubevipKey] = key
	}

	cm := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]interface{}{
			"name":        "kubevip",
			"namespace":   "kube-system",
			"annotations": annotations,
		},
		"data": values,
	}}
	cm.SetGroupVersionKind(KubevipConfigMapGVK)
	return cm
}

func syncKubevip(t *testing.T, cm *unstructured.Unstructured, configs []poolConfiguration, managed []netip.Prefix) (*unstructured.Unstructured, error) {
	t.Helper()

	// v1/ConfigMap is already in the scheme via the client-go types; unlike the
	// CRD backends there is nothing to register.
	scheme := newPoolBackendTestScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm).Build()

	r := &PoolSyncReconciler{Client: fakeClient, Scheme: scheme}
	backend := kubevipConfigMapBackend{resourceGVK: KubevipConfigMapGVK}
	if _, err := backend.update(context.Background(), r, cm, configs, managed); err != nil {
		return nil, err
	}

	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(KubevipConfigMapGVK)
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Namespace: "kube-system", Name: "kubevip"}, got); err != nil {
		t.Fatalf("reading the ConfigMap back: %v", err)
	}
	return got, nil
}

func dataValue(t *testing.T, cm *unstructured.Unstructured, key string) string {
	t.Helper()
	data, _, err := unstructured.NestedStringMap(cm.Object, "data")
	if err != nil {
		t.Fatalf("reading data: %v", err)
	}
	return data[key]
}

func mustPrefixes(t *testing.T, cidrs ...string) []netip.Prefix {
	t.Helper()
	prefixes := make([]netip.Prefix, 0, len(cidrs))
	for _, cidr := range cidrs {
		prefixes = append(prefixes, netip.MustParsePrefix(cidr))
	}
	return prefixes
}

func TestKubevipWritesCurrentAndHistoricalEntries(t *testing.T) {
	cm := kubevipConfigMap(t, "cidr-global", nil)
	configs := []poolConfiguration{
		{cidr: testPoolPrefix},
		{cidr: "2001:db8:2::/64"},
	}

	got, err := syncKubevip(t, cm, configs, mustPrefixes(t, testPoolPrefix, "2001:db8:2::/64"))
	if err != nil {
		t.Fatalf("update() = %v", err)
	}

	if value := dataValue(t, got, "cidr-global"); value != testPoolPrefix+",2001:db8:2::/64" {
		t.Errorf("cidr-global = %q, want the current prefix followed by the historical one", value)
	}
	record := got.GetAnnotations()[AnnotationManagedKubevipEntries]
	if record != testPoolPrefix+",2001:db8:2::/64" {
		t.Errorf("ownership record = %q, want both entries recorded in the same update", record)
	}
}

// A kube-vip pool is routinely dual-stack, and the whole IPv4 half of it is
// somebody else's business.
func TestKubevipPreservesEntriesItDoesNotOwn(t *testing.T) {
	cm := kubevipConfigMap(t, "cidr-global", map[string]string{
		"cidr-global": "192.0.2.0/24, 2001:db8:ffff::/64",
		"range-other": "192.0.2.10-192.0.2.20",
	})

	got, err := syncKubevip(t, cm, []poolConfiguration{{cidr: testPoolPrefix}}, mustPrefixes(t, testPoolPrefix))
	if err != nil {
		t.Fatalf("update() = %v", err)
	}

	value := dataValue(t, got, "cidr-global")
	for _, want := range []string{"192.0.2.0/24", "2001:db8:ffff::/64", testPoolPrefix} {
		if !strings.Contains(value, want) {
			t.Errorf("cidr-global = %q, want it to contain %q", value, want)
		}
	}
	if other := dataValue(t, got, "range-other"); other != "192.0.2.10-192.0.2.20" {
		t.Errorf("range-other = %q, want an unrelated key left alone", other)
	}
}

func TestKubevipRendersRangeKeys(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		config  poolConfiguration
		want    string
		wantErr string
	}{
		{
			name:   "an address range goes into a range key verbatim",
			key:    "range-global",
			config: poolConfiguration{useAddressRange: true, start: "2001:db8::100", end: "2001:db8::200"},
			want:   "2001:db8::100-2001:db8::200",
		},
		{
			name:   "a CIDR converts to a range exactly",
			key:    "range-global",
			config: poolConfiguration{cidr: "2001:db8::/126"},
			want:   "2001:db8::-2001:db8::3",
		},
		{
			name:   "a CIDR-aligned address range may go into a cidr key",
			key:    "cidr-global",
			config: poolConfiguration{useAddressRange: true, start: "2001:db8::", end: "2001:db8::3"},
			want:   "2001:db8::/126",
		},
		{
			// Widening it would hand kube-vip addresses the user deliberately
			// left out of the range.
			name:    "a range that is not CIDR-aligned is refused rather than widened",
			key:     "cidr-global",
			config:  poolConfiguration{useAddressRange: true, start: "2001:db8::100", end: "2001:db8::200"},
			wantErr: "not CIDR-aligned",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entries, err := kubevipEntriesFor(tt.key, []poolConfiguration{tt.config})
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("kubevipEntriesFor() = %v, want an error mentioning %q", entries, tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %q, want it to mention %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("kubevipEntriesFor() = %v", err)
			}
			if !slices.Equal(entries, []string{tt.want}) {
				t.Fatalf("entries = %v, want %v", entries, []string{tt.want})
			}
		})
	}
}

// One ConfigMap holds every pool in the cluster, keyed by name, and the
// cidr/range split changes how kube-vip allocates. Guessing would put a pool in
// somebody else's key.
func TestKubevipRequiresTheKeyToBeNamed(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		wantErr string
	}{
		{name: "missing", key: "", wantErr: AnnotationKubevipKey},
		{name: "not a pool key", key: "search-order", wantErr: "not a kube-vip pool key"},
		{name: "misspelled", key: "cidrglobal", wantErr: "not a kube-vip pool key"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cm := kubevipConfigMap(t, tt.key, nil)
			_, err := syncKubevip(t, cm, []poolConfiguration{{cidr: "2001:db8::/64"}}, nil)
			if err == nil {
				t.Fatal("expected the sync to fail")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %q, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

// Re-pointing the annotation must not strand entries under the old key: nothing
// afterwards looks at a key the annotation no longer names.
func TestKubevipMovesItsEntriesWhenTheKeyChanges(t *testing.T) {
	cm := kubevipConfigMap(t, "cidr-global", nil)
	got, err := syncKubevip(t, cm, []poolConfiguration{{cidr: testPoolPrefix}}, mustPrefixes(t, testPoolPrefix))
	if err != nil {
		t.Fatalf("first update: %v", err)
	}

	annotations := got.GetAnnotations()
	annotations[AnnotationKubevipKey] = "cidr-production"
	got.SetAnnotations(annotations)

	moved, err := syncKubevip(t, got, []poolConfiguration{{cidr: testPoolPrefix}}, mustPrefixes(t, testPoolPrefix))
	if err != nil {
		t.Fatalf("second update: %v", err)
	}

	if left := dataValue(t, moved, "cidr-global"); left != "" {
		t.Errorf("cidr-global = %q, want the entry removed from the key that is no longer managed", left)
	}
	if value := dataValue(t, moved, "cidr-production"); value != testPoolPrefix {
		t.Errorf("cidr-production = %q, want the entry written under the new key", value)
	}
}

func TestKubevipReleaseRemovesOnlyItsOwnEntries(t *testing.T) {
	cm := kubevipConfigMap(t, "cidr-global", map[string]string{
		"cidr-global": "192.0.2.0/24",
	})
	got, err := syncKubevip(t, cm, []poolConfiguration{{cidr: testPoolPrefix}}, mustPrefixes(t, testPoolPrefix))
	if err != nil {
		t.Fatalf("update: %v", err)
	}

	backend := kubevipConfigMapBackend{resourceGVK: KubevipConfigMapGVK}
	changed, err := backend.release(context.Background(), nil, got)
	if err != nil {
		t.Fatalf("release() = %v", err)
	}
	if !changed {
		t.Fatal("release() reported no change despite having written entries")
	}

	if value := dataValue(t, got, "cidr-global"); value != "192.0.2.0/24" {
		t.Errorf("cidr-global = %q, want the user's entry kept and the operator's removed", value)
	}
	if _, ok := got.GetAnnotations()[AnnotationManagedKubevipEntries]; ok {
		t.Error("the ownership record survived the release")
	}
}

// A present-but-empty key is a pool kube-vip cannot allocate from, which is not
// the same as no pool at all.
func TestKubevipRemovesAKeyItHasEmptied(t *testing.T) {
	cm := kubevipConfigMap(t, "cidr-global", nil)
	got, err := syncKubevip(t, cm, []poolConfiguration{{cidr: testPoolPrefix}}, mustPrefixes(t, testPoolPrefix))
	if err != nil {
		t.Fatalf("update: %v", err)
	}

	backend := kubevipConfigMapBackend{resourceGVK: KubevipConfigMapGVK}
	if _, err := backend.release(context.Background(), nil, got); err != nil {
		t.Fatalf("release() = %v", err)
	}

	data, _, err := unstructured.NestedStringMap(got.Object, "data")
	if err != nil {
		t.Fatalf("reading data: %v", err)
	}
	if _, present := data["cidr-global"]; present {
		t.Errorf("cidr-global is still present as %q, want the key removed once nothing is left in it", data["cidr-global"])
	}
}

// A rotation must replace the operator's entries, not accumulate them.
func TestKubevipDoesNotLeakEntriesAcrossRotations(t *testing.T) {
	cm := kubevipConfigMap(t, "cidr-global", nil)

	got, err := syncKubevip(t, cm, []poolConfiguration{{cidr: testPoolPrefix}}, mustPrefixes(t, testPoolPrefix))
	if err != nil {
		t.Fatalf("first rotation: %v", err)
	}
	got, err = syncKubevip(t, got,
		[]poolConfiguration{{cidr: "2001:db8:2::/64"}, {cidr: testPoolPrefix}},
		mustPrefixes(t, "2001:db8:2::/64", testPoolPrefix))
	if err != nil {
		t.Fatalf("second rotation: %v", err)
	}
	got, err = syncKubevip(t, got,
		[]poolConfiguration{{cidr: "2001:db8:3::/64"}, {cidr: "2001:db8:2::/64"}},
		mustPrefixes(t, "2001:db8:3::/64", "2001:db8:2::/64"))
	if err != nil {
		t.Fatalf("third rotation: %v", err)
	}

	value := dataValue(t, got, "cidr-global")
	if value != "2001:db8:3::/64,2001:db8:2::/64" {
		t.Fatalf("cidr-global = %q, want only the current prefix and the one retained in history", value)
	}
	if strings.Contains(value, testPoolPrefix) {
		t.Error("a prefix that has aged out of history was left in the pool")
	}
}

func TestKubevipRecordIsRegisteredForOwnershipChecks(t *testing.T) {
	annotations := map[string]string{AnnotationManagedKubevipEntries: "2001:db8::/64"}
	if !hasOwnershipRecord(annotations) {
		t.Error("a ConfigMap carrying only a kube-vip record is not recognised as still bound, " +
			"so the watch would stop matching it before its entries are handed back")
	}
}

func TestKubevipBackendIsSelectedForConfigMaps(t *testing.T) {
	backend := backendForGVK(KubevipConfigMapGVK)
	if backend == nil {
		t.Fatal("no backend is registered for v1/ConfigMap")
	}
	if backend.name() != "kubevip-configmap" {
		t.Errorf("backend = %q, want kubevip-configmap", backend.name())
	}
	if !backend.namespaced() {
		t.Error("the kube-vip ConfigMap is namespaced")
	}
}
