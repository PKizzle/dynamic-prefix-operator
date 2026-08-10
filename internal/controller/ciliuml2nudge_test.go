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
	"errors"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// stubL2NudgeDecider pins a verdict so the write path can be tested without a
// DaemonSet. The production decider is exercised separately above.
type stubL2NudgeDecider struct {
	needed bool
	calls  int
}

func (s *stubL2NudgeDecider) Needed(context.Context) (bool, string) {
	s.calls++
	return s.needed, "stub verdict"
}

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
		{
			// The lenient semver parser reads this as 20260810.0.0, which clears
			// the threshold and silently disables the nudge.
			name:    "date-stamped nightly is not a version",
			image:   "registry.internal/cilium/cilium:20260810",
			wantErr: true,
		},
		{
			name:    "bare build number is not a version",
			image:   "registry.internal/cilium/cilium:2",
			wantErr: true,
		},
		{
			name:    "major.minor alone is ambiguous",
			image:   "quay.io/cilium/cilium:v1.20",
			wantErr: true,
		},
		{
			// A sidecar's tag must never be compared against a Cilium threshold.
			name:    "repository does not name Cilium",
			image:   "registry.internal/vendor/service-mesh-agent:v1.23.4",
			wantErr: true,
		},
		{
			name:  "enterprise fork keeps its prerelease suffix",
			image: "quay.io/isovalent/cilium:v1.21.0-cee.1",
			want:  "1.21.0-cee.1",
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
	// rolledOut is the steady state: every node runs the current template. A
	// DaemonSet built without it is mid-rollout as far as the detector cares.
	rolledOut := appsv1.DaemonSetStatus{
		DesiredNumberScheduled: 3,
		UpdatedNumberScheduled: 3,
		NumberReady:            3,
	}

	daemonSetIn := func(namespace, name string, containers []corev1.Container, status appsv1.DaemonSetStatus) *appsv1.DaemonSet {
		return &appsv1.DaemonSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
				Labels:    map[string]string{"k8s-app": "cilium"},
			},
			Spec: appsv1.DaemonSetSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{Containers: containers},
				},
			},
			Status: status,
		}
	}
	agentContainers := func(image string) []corev1.Container {
		return []corev1.Container{
			{Name: "config", Image: "quay.io/cilium/cilium:v0.0.1"},
			{Name: ciliumAgentContainer, Image: image},
		}
	}
	daemonSet := func(image string) *appsv1.DaemonSet {
		return daemonSetIn("kube-system", "cilium", agentContainers(image), rolledOut)
	}

	tests := []struct {
		name       string
		daemonSet  *appsv1.DaemonSet
		extra      *appsv1.DaemonSet
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
		{
			name:       "a date-stamped nightly must not read as a huge version",
			daemonSet:  daemonSet("registry.internal/cilium/cilium:20260810"),
			wantNeeded: true,
		},
		{
			name: "an unfamiliar agent container name falls back to nudging",
			daemonSet: daemonSetIn("kube-system", "cilium", []corev1.Container{
				{Name: "agent", Image: "quay.io/cilium/cilium:v1.21.0"},
			}, rolledOut),
			wantNeeded: true,
		},
		{
			name: "a sidecar's version must not decide the verdict",
			daemonSet: daemonSetIn("kube-system", "cilium", []corev1.Container{
				{Name: "istio-proxy", Image: "docker.io/istio/proxyv2:v1.23.4"},
			}, rolledOut),
			wantNeeded: true,
		},
		{
			name:       "a DaemonSet with no containers falls back to nudging",
			daemonSet:  daemonSetIn("kube-system", "cilium", nil, rolledOut),
			wantNeeded: true,
		},
		{
			name: "an in-flight rollout is not yet fixed",
			daemonSet: daemonSetIn("kube-system", "cilium", agentContainers("quay.io/cilium/cilium:v1.21.0"),
				appsv1.DaemonSetStatus{DesiredNumberScheduled: 3, UpdatedNumberScheduled: 1, NumberReady: 3}),
			wantNeeded: true,
		},
		{
			name: "unavailable pods may still run the old announcer",
			daemonSet: daemonSetIn("kube-system", "cilium", agentContainers("quay.io/cilium/cilium:v1.21.0"),
				appsv1.DaemonSetStatus{DesiredNumberScheduled: 3, UpdatedNumberScheduled: 3, NumberUnavailable: 1}),
			wantNeeded: true,
		},
		{
			name: "an unreported rollout status is not trusted",
			daemonSet: daemonSetIn("kube-system", "cilium", agentContainers("quay.io/cilium/cilium:v1.21.0"),
				appsv1.DaemonSetStatus{}),
			wantNeeded: true,
		},
		{
			// A stale DaemonSet elsewhere must not decide for the cluster; the
			// one in kube-system is the agent.
			name:       "a second labelled DaemonSet outside kube-system is ignored",
			daemonSet:  daemonSet("quay.io/cilium/cilium:v1.21.0"),
			extra:      daemonSetIn("staging", "cilium", agentContainers("quay.io/cilium/cilium:v1.20.0"), rolledOut),
			wantNeeded: false,
		},
		{
			name:       "two labelled DaemonSets in kube-system are ambiguous",
			daemonSet:  daemonSet("quay.io/cilium/cilium:v1.21.0"),
			extra:      daemonSetIn("kube-system", "cilium-old", agentContainers("quay.io/cilium/cilium:v1.21.0"), rolledOut),
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
			if tt.extra != nil {
				builder = builder.WithObjects(tt.extra)
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

// TestNudgeL2Announcer_VersionGate connects the verdict to the write path. Every
// other test in this file stops at the verdict, and the reconciler's own tests
// leave the decider nil -- so without this one, "a fixed Cilium is not nudged"
// is asserted nowhere.
func TestNudgeL2Announcer_VersionGate(t *testing.T) {
	const assigned = "2001:db8::1"

	service := func(annotations map[string]string) *corev1.Service {
		return &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default", Annotations: annotations},
			Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer},
			Status: corev1.ServiceStatus{
				LoadBalancer: corev1.LoadBalancerStatus{
					Ingress: []corev1.LoadBalancerIngress{{IP: assigned}},
				},
			},
		}
	}

	tests := []struct {
		name        string
		decider     l2NudgeDecider
		annotations map[string]string
		wantNudge   bool
		wantCalls   int
	}{
		{
			name:      "a Cilium that still needs the nudge gets one",
			decider:   &stubL2NudgeDecider{needed: true},
			wantNudge: true,
			wantCalls: 1,
		},
		{
			name:      "a fixed Cilium is left alone",
			decider:   &stubL2NudgeDecider{needed: false},
			wantNudge: false,
			wantCalls: 1,
		},
		{
			name:      "no decider nudges, because uncertainty is cheap in that direction",
			decider:   nil,
			wantNudge: true,
		},
		{
			name:        "the force annotation overrides a fixed verdict without consulting it",
			decider:     &stubL2NudgeDecider{needed: false},
			annotations: map[string]string{AnnotationForceL2Nudge: AnnotationValueTrue},
			wantNudge:   true,
			wantCalls:   0,
		},
		{
			name:        "an explicit opt-out wins over the force annotation",
			decider:     &stubL2NudgeDecider{needed: true},
			annotations: map[string]string{AnnotationSkipL2Nudge: AnnotationValueTrue, AnnotationForceL2Nudge: AnnotationValueTrue},
			wantNudge:   false,
			wantCalls:   0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			_ = clientgoscheme.AddToScheme(scheme)

			svc := service(tt.annotations)
			fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(svc).Build()
			r := &ServiceSyncReconciler{Client: fakeClient, Scheme: scheme, l2Nudge: tt.decider}

			if _, err := r.nudgeL2Announcer(context.Background(), svc, assigned); err != nil {
				t.Fatalf("nudgeL2Announcer() error = %v", err)
			}

			var got corev1.Service
			if err := fakeClient.Get(context.Background(),
				types.NamespacedName{Name: "web", Namespace: "default"}, &got); err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			_, nudged := got.Annotations[AnnotationL2Nudge]
			if nudged != tt.wantNudge {
				t.Errorf("nudge annotation present = %v, want %v", nudged, tt.wantNudge)
			}
			if stub, ok := tt.decider.(*stubL2NudgeDecider); ok && stub.calls != tt.wantCalls {
				t.Errorf("decider consulted %d times, want %d", stub.calls, tt.wantCalls)
			}
		})
	}
}

// TestL2NudgeDetector_ListErrorNudges covers the path the documentation leans on
// hardest: RBAC for apps/daemonsets is optional, so being refused the read must
// resolve to nudging rather than to standing down.
func TestL2NudgeDetector_ListErrorNudges(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)

	forbidden := fake.NewClientBuilder().
		WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
				return apierrors.NewForbidden(schema.GroupResource{Group: "apps", Resource: "daemonsets"},
					"", errors.New("no RBAC for daemonsets"))
			},
		}).
		Build()

	needed, reason := newL2NudgeDetector(forbidden).Needed(context.Background())
	if !needed {
		t.Errorf("a refused DaemonSet read said the nudge is unnecessary (%s)", reason)
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
