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
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestParseCiliumImageVersion(t *testing.T) {
	tests := []struct {
		name    string
		image   string
		want    string
		wantErr bool
	}{
		{
			name:  "tag and digest, as a pinned chart writes it",
			image: "quay.io/cilium/cilium:v1.21.0-pre.0@sha256:86081047ee204fd4a720fdd8a6620efa0e13929f6c5376fb7ac73aea08de0f48",
			want:  "1.21.0-pre.0",
		},
		{
			name:  "plain tag",
			image: "quay.io/cilium/cilium:v1.20.0",
			want:  "1.20.0",
		},
		{
			name:  "tag without the v prefix",
			image: "quay.io/cilium/cilium:1.20.0",
			want:  "1.20.0",
		},
		{
			name:  "registry with a port",
			image: "registry.internal:5000/cilium/cilium:v1.20.0",
			want:  "1.20.0",
		},
		{
			name:    "digest only carries no version",
			image:   "quay.io/cilium/cilium@sha256:86081047ee204fd4a720fdd8a6620efa0e13929f6c5376fb7ac73aea08de0f48",
			wantErr: true,
		},
		{
			name:    "untagged",
			image:   "quay.io/cilium/cilium",
			wantErr: true,
		},
		{
			name:    "registry port must not be mistaken for a tag",
			image:   "registry.internal:5000/cilium/cilium",
			wantErr: true,
		},
		{
			name:    "non-version tag",
			image:   "quay.io/cilium/cilium:latest",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseCiliumImageVersion(tt.image)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseCiliumImageVersion(%q) = %v, want an error", tt.image, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseCiliumImageVersion(%q) error = %v", tt.image, err)
			}
			if got.String() != tt.want {
				t.Errorf("parseCiliumImageVersion(%q) = %q, want %q", tt.image, got, tt.want)
			}
		})
	}
}

// TestL2NudgeDetector_Needed pins the verdict for each Cilium version, and the
// rule that every uncertain case resolves to "nudge". Getting that direction
// wrong would silently stop announcing rotated addresses.
func TestL2NudgeDetector_Needed(t *testing.T) {
	daemonSet := func(image string) *appsv1.DaemonSet {
		return &appsv1.DaemonSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "cilium",
				Namespace: "kube-system",
				Labels:    map[string]string{"k8s-app": "cilium"},
			},
			Spec: appsv1.DaemonSetSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{Name: "config", Image: "quay.io/cilium/cilium:v0.0.1"},
							{Name: ciliumAgentContainer, Image: image},
						},
					},
				},
			},
		}
	}

	tests := []struct {
		name       string
		daemonSet  *appsv1.DaemonSet
		wantNeeded bool
	}{
		{
			name:       "1.20.0 predates the fix",
			daemonSet:  daemonSet("quay.io/cilium/cilium:v1.20.0"),
			wantNeeded: true,
		},
		{
			name:       "1.19.9 never received the backport",
			daemonSet:  daemonSet("quay.io/cilium/cilium:v1.19.9"),
			wantNeeded: true,
		},
		{
			name:       "1.20.1 is the first release carrying the fix",
			daemonSet:  daemonSet("quay.io/cilium/cilium:v1.20.1"),
			wantNeeded: false,
		},
		{
			name:       "1.21.0-pre.0 carries the fix despite the prerelease suffix",
			daemonSet:  daemonSet("quay.io/cilium/cilium:v1.21.0-pre.0@sha256:abc"),
			wantNeeded: false,
		},
		{
			name:       "1.21.0 carries the fix",
			daemonSet:  daemonSet("quay.io/cilium/cilium:v1.21.0"),
			wantNeeded: false,
		},
		{
			name:       "an unreadable version falls back to nudging",
			daemonSet:  daemonSet("quay.io/cilium/cilium:latest"),
			wantNeeded: true,
		},
		{
			name:       "a digest-only pin falls back to nudging",
			daemonSet:  daemonSet("quay.io/cilium/cilium@sha256:abc"),
			wantNeeded: true,
		},
		{
			name:       "no Cilium DaemonSet at all falls back to nudging",
			daemonSet:  nil,
			wantNeeded: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			_ = clientgoscheme.AddToScheme(scheme)

			builder := fake.NewClientBuilder().WithScheme(scheme)
			if tt.daemonSet != nil {
				builder = builder.WithObjects(tt.daemonSet)
			}
			detector := newL2NudgeDetector(builder.Build())

			needed, reason := detector.Needed(context.Background())
			if needed != tt.wantNeeded {
				t.Errorf("Needed() = %v (%s), want %v", needed, reason, tt.wantNeeded)
			}
			if reason == "" {
				t.Error("Needed() returned an empty reason")
			}
		})
	}
}

// TestL2NudgeDetector_NilDetectorNudges covers the zero value: a reconciler built
// without SetupWithManager must still nudge rather than silently skip.
func TestL2NudgeDetector_NilDetectorNudges(t *testing.T) {
	var detector *l2NudgeDetector
	needed, reason := detector.Needed(context.Background())
	if !needed {
		t.Errorf("nil detector said the nudge is unnecessary (%s)", reason)
	}
}

// TestL2NudgeDetector_VerdictExpires covers Cilium being upgraded underneath a
// running operator: the verdict is cached, but must not be cached forever.
func TestL2NudgeDetector_VerdictExpires(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	detector := newL2NudgeDetector(fakeClient)
	if needed, _ := detector.Needed(context.Background()); !needed {
		t.Fatal("expected the nudge to be needed with no DaemonSet present")
	}
	if !detector.valid {
		t.Fatal("expected the verdict to be cached")
	}

	// A fresh verdict is reused ...
	detector.needed = false
	if needed, _ := detector.Needed(context.Background()); needed {
		t.Error("expected the cached verdict to be reused within the TTL")
	}

	// ... but an expired one is recomputed.
	detector.checkedAt = time.Now().Add(-2 * detector.ttl)
	if needed, _ := detector.Needed(context.Background()); !needed {
		t.Error("expected an expired verdict to be recomputed")
	}
}
