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
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Masterminds/semver/v3"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ciliumL2AnnouncerFixedVersion is the first release carrying cilium/cilium#47579
// ("l2announcer: re-evaluate services on frontend changes"), which removes the
// need for the nudge. The fix merged to the v1.20 branch after v1.20.0 was cut,
// so v1.20.1 is the first tagged release on that branch to contain it.
//
// A single threshold covers the 1.21 line too: 1.21.0-pre.0 compares greater than
// 1.20.1 on the core version, prerelease suffix notwithstanding.
var ciliumL2AnnouncerFixedVersion = semver.MustParse("1.20.1")

// ciliumAgentLabel selects the Cilium agent DaemonSet. Cilium has set this label
// on the agent since well before any release this operator supports.
const ciliumAgentLabel = "k8s-app=cilium"

// ciliumAgentContainer is the container within that DaemonSet whose image carries
// the Cilium version.
const ciliumAgentContainer = "cilium-agent"

// l2NudgeDetector decides whether the Cilium running in this cluster still needs
// the L2 announcer nudge (see nudgeL2Announcer).
//
// Every uncertain answer resolves to "yes, nudge". The two failure directions are
// not symmetric: a wrong "already fixed" silently stops announcing rotated
// addresses, and that failure is invisible from every other angle -- pool,
// annotation, Service status and datapath frontends all look correct -- so it is
// expensive to diagnose. A wrong "still broken" costs one annotation write per
// change to a Service's address set. Guessing in the cheap direction is therefore
// the only defensible default, and it means unreadable images, unfamiliar forks,
// digest-only references and missing RBAC all degrade safely.
type l2NudgeDetector struct {
	// reader performs uncached reads. The DaemonSet is consulted rarely, and
	// caching it would mean informing on every DaemonSet in the cluster.
	reader client.Reader
	// ttl bounds how long a verdict is reused. Cilium can be upgraded underneath
	// a running operator, so the verdict has to expire on its own.
	ttl time.Duration

	mu        sync.Mutex
	checkedAt time.Time
	valid     bool
	needed    bool
	reason    string
}

func newL2NudgeDetector(reader client.Reader) *l2NudgeDetector {
	return &l2NudgeDetector{reader: reader, ttl: 5 * time.Minute}
}

// Needed reports whether the nudge should be applied, along with a short reason
// suitable for logging. It never returns an error: an undetectable version is an
// answer ("nudge"), not a failure.
func (d *l2NudgeDetector) Needed(ctx context.Context) (bool, string) {
	if d == nil {
		return true, "no detector configured"
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	if d.valid && time.Since(d.checkedAt) < d.ttl {
		return d.needed, d.reason
	}

	needed, reason := d.detect(ctx)
	d.needed, d.reason = needed, reason
	d.checkedAt, d.valid = time.Now(), true
	return needed, reason
}

func (d *l2NudgeDetector) detect(ctx context.Context) (bool, string) {
	selector, err := labels.Parse(ciliumAgentLabel)
	if err != nil {
		return true, fmt.Sprintf("could not parse agent selector: %v", err)
	}

	var daemonSets appsv1.DaemonSetList
	if err := d.reader.List(ctx, &daemonSets, &client.ListOptions{LabelSelector: selector}); err != nil {
		return true, fmt.Sprintf("could not list the Cilium DaemonSet: %v", err)
	}
	if len(daemonSets.Items) == 0 {
		return true, "no Cilium agent DaemonSet found"
	}

	image := ciliumAgentImage(&daemonSets.Items[0])
	if image == "" {
		return true, "Cilium agent DaemonSet has no container image"
	}

	version, err := parseCiliumImageVersion(image)
	if err != nil {
		return true, fmt.Sprintf("could not read a version from image %q: %v", image, err)
	}

	if version.Compare(ciliumL2AnnouncerFixedVersion) >= 0 {
		return false, fmt.Sprintf("Cilium %s includes the L2 announcer fix (>= %s)",
			version, ciliumL2AnnouncerFixedVersion)
	}
	return true, fmt.Sprintf("Cilium %s predates the L2 announcer fix (%s)",
		version, ciliumL2AnnouncerFixedVersion)
}

// ciliumAgentImage returns the image of the agent container, preferring the
// container named cilium-agent and falling back to the first one for charts that
// name it differently.
func ciliumAgentImage(ds *appsv1.DaemonSet) string {
	containers := ds.Spec.Template.Spec.Containers
	for _, c := range containers {
		if c.Name == ciliumAgentContainer {
			return c.Image
		}
	}
	if len(containers) > 0 {
		return containers[0].Image
	}
	return ""
}

// parseCiliumImageVersion extracts a version from a container image reference.
//
// A pinned image commonly carries both a tag and a digest
// ("quay.io/cilium/cilium:v1.21.0-pre.0@sha256:..."); the digest is dropped and
// the tag used. An image pinned by digest alone carries no version at all, which
// is reported as an error so the caller falls back to nudging.
func parseCiliumImageVersion(image string) (*semver.Version, error) {
	ref := image
	if at := strings.Index(ref, "@"); at >= 0 {
		ref = ref[:at]
	}

	colon := strings.LastIndex(ref, ":")
	if colon < 0 {
		return nil, fmt.Errorf("image is not tagged")
	}
	// A colon after the last slash is a tag; before it, it is a registry port.
	if slash := strings.LastIndex(ref, "/"); slash > colon {
		return nil, fmt.Errorf("image is not tagged")
	}

	tag := strings.TrimPrefix(ref[colon+1:], "v")
	if tag == "" {
		return nil, fmt.Errorf("image has an empty tag")
	}

	version, err := semver.NewVersion(tag)
	if err != nil {
		return nil, fmt.Errorf("tag %q is not a version: %w", tag, err)
	}
	return version, nil
}
