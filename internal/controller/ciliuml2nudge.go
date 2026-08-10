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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Masterminds/semver/v3"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
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

// ciliumAgentNamespace is where the agent conventionally runs. It is only used to
// disambiguate when more than one DaemonSet carries the agent label.
const ciliumAgentNamespace = "kube-system"

// ciliumRepositoryMarker must appear in an image's repository path before its tag
// is trusted to be a Cilium version. Without it, any image that happens to sit in
// the agent DaemonSet -- an injected sidecar, a renamed container -- would have
// its own version compared against the Cilium threshold.
const ciliumRepositoryMarker = "cilium"

// l2NudgeDecider answers whether the L2 announcer nudge is still required.
// ServiceSyncReconciler depends on this rather than on *l2NudgeDetector so a test
// can pin a verdict without standing up a DaemonSet.
type l2NudgeDecider interface {
	// Needed reports whether the nudge should be applied, along with a short
	// reason suitable for logging. It never returns an error: an undetectable
	// version is an answer ("nudge"), not a failure.
	Needed(ctx context.Context) (bool, string)
}

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
// digest-only references, in-flight upgrades and missing RBAC all degrade safely.
//
// "Uncertain" is meant strictly. A tag is only believed when it is a complete
// semantic version on an image whose repository names Cilium, read from the
// container that Cilium's own chart names, in a DaemonSet that is unambiguously
// the agent, whose rollout has finished. Anything else nudges.
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
	// logged is the reason last written to the log. Verdicts are re-derived every
	// few minutes and consulted on every reconcile, so only transitions are
	// logged -- but they are logged at Info, because "Cilium is fixed, standing
	// down" is the one conclusion whose silent mistake is expensive.
	logged string
}

var _ l2NudgeDecider = (*l2NudgeDetector)(nil)

func newL2NudgeDetector(reader client.Reader) *l2NudgeDetector {
	return &l2NudgeDetector{reader: reader, ttl: 5 * time.Minute}
}

// Needed implements l2NudgeDecider.
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

	if reason != d.logged {
		logf.FromContext(ctx).Info("Cilium L2 announcer nudge verdict",
			"nudge", needed, "reason", reason)
		d.logged = reason
	}
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

	agent, reason := ciliumAgentDaemonSet(daemonSets.Items)
	if agent == nil {
		return true, reason
	}

	image := ciliumAgentImage(agent)
	if image == "" {
		return true, fmt.Sprintf("DaemonSet %s/%s has no %s container",
			agent.Namespace, agent.Name, ciliumAgentContainer)
	}

	version, err := parseCiliumImageVersion(image)
	if err != nil {
		return true, fmt.Sprintf("could not read a version from image %q: %v", image, err)
	}

	if version.Compare(ciliumL2AnnouncerFixedVersion) < 0 {
		return true, fmt.Sprintf("Cilium %s predates the L2 announcer fix (%s)",
			version, ciliumL2AnnouncerFixedVersion)
	}

	// The template says the fix is present; the nodes may not agree yet. During a
	// rollout the un-upgraded pods still run the buggy announcer, so standing down
	// on the template alone would leave exactly the silent failure this detector
	// exists to avoid -- for the length of the rollout, which is when a restart is
	// most likely to be shaking addresses loose anyway.
	if rollout := ciliumRolloutIncomplete(agent); rollout != "" {
		return true, fmt.Sprintf("Cilium %s includes the L2 announcer fix but %s", version, rollout)
	}

	return false, fmt.Sprintf("Cilium %s includes the L2 announcer fix (>= %s)",
		version, ciliumL2AnnouncerFixedVersion)
}

// ciliumAgentDaemonSet picks the agent out of everything wearing the agent label.
// Ambiguity is not resolved by guessing: a stale DaemonSet left in another
// namespace must not get to decide the verdict for the whole cluster, so anything
// it cannot pin down returns a nil DaemonSet and a reason to nudge.
func ciliumAgentDaemonSet(items []appsv1.DaemonSet) (*appsv1.DaemonSet, string) {
	switch len(items) {
	case 0:
		return nil, "no Cilium agent DaemonSet found"
	case 1:
		return &items[0], ""
	}

	var candidates []*appsv1.DaemonSet
	for i := range items {
		if items[i].Namespace == ciliumAgentNamespace {
			candidates = append(candidates, &items[i])
		}
	}
	if len(candidates) == 1 {
		return candidates[0], ""
	}

	names := make([]string, 0, len(items))
	for i := range items {
		names = append(names, items[i].Namespace+"/"+items[i].Name)
	}
	sort.Strings(names)
	return nil, fmt.Sprintf("%d DaemonSets carry %s (%s); cannot tell which is the agent",
		len(items), ciliumAgentLabel, strings.Join(names, ", "))
}

// ciliumRolloutIncomplete returns a description of why the agent rollout cannot be
// considered finished, or "" when every node runs the current template.
//
// A DaemonSet whose status has not been reported at all (desired 0) is treated as
// unfinished: on a real cluster the agent runs somewhere, so a zero desired count
// means the status is not yet trustworthy rather than that there is nothing to
// wait for.
func ciliumRolloutIncomplete(ds *appsv1.DaemonSet) string {
	status := ds.Status
	if status.DesiredNumberScheduled == 0 {
		return "its rollout status is not reported yet"
	}
	if status.UpdatedNumberScheduled != status.DesiredNumberScheduled {
		return fmt.Sprintf("its rollout is still in progress (%d/%d pods updated)",
			status.UpdatedNumberScheduled, status.DesiredNumberScheduled)
	}
	if status.NumberUnavailable > 0 {
		return fmt.Sprintf("%d of its pods are unavailable", status.NumberUnavailable)
	}
	return ""
}

// ciliumAgentImage returns the image of the agent container, identified by the
// name Cilium's chart gives it.
//
// There is deliberately no fallback to "the first container": a DaemonSet whose
// agent container is named something else is an unfamiliar deployment, and
// reading an arbitrary container's tag there would compare a sidecar's version
// against the Cilium threshold. Returning "" makes that case nudge.
func ciliumAgentImage(ds *appsv1.DaemonSet) string {
	for _, c := range ds.Spec.Template.Spec.Containers {
		if c.Name == ciliumAgentContainer {
			return c.Image
		}
	}
	return ""
}

// parseCiliumImageVersion extracts a Cilium version from a container image
// reference.
//
// A pinned image commonly carries both a tag and a digest
// ("quay.io/cilium/cilium:v1.21.0-pre.0@sha256:..."); the digest is dropped and
// the tag used. An image pinned by digest alone carries no version at all, which
// is reported as an error so the caller falls back to nudging.
//
// Two rules keep a tag from being believed too readily, because every
// misreading here fails in the expensive direction:
//
//   - The repository must name Cilium. Otherwise an unrelated image's tag would
//     be compared against a Cilium version threshold.
//   - The tag must be a complete semantic version. Parsing leniently would read
//     a date-stamped nightly ("cilium:20260810") or a bare build number as an
//     enormous version that trivially clears the threshold, and silently disable
//     the nudge -- the one outcome this detector must never reach by accident.
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

	repository := ref[:colon]
	if !strings.Contains(strings.ToLower(repository), ciliumRepositoryMarker) {
		return nil, fmt.Errorf("repository %q is not a Cilium image", repository)
	}

	tag := strings.TrimPrefix(ref[colon+1:], "v")
	if tag == "" {
		return nil, fmt.Errorf("image has an empty tag")
	}

	// StrictNewVersion, not NewVersion: the lenient parser coerces "20260810" and
	// "2" into versions rather than rejecting them.
	version, err := semver.StrictNewVersion(tag)
	if err != nil {
		return nil, fmt.Errorf("tag %q is not a complete version: %w", tag, err)
	}
	return version, nil
}
