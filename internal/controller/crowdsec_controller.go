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
	"fmt"
	"net/netip"
	"slices"
	"strings"

	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	dynamicprefixiov1alpha1 "github.com/pkizzle/dynamic-prefix-operator/api/v1alpha1"
	acdynamicprefixv1alpha1 "github.com/pkizzle/dynamic-prefix-operator/api/v1alpha1/applyconfiguration/api/v1alpha1"
)

const (
	eventReasonCrowdSecMarkerCreated = "CrowdSecMarkerCreated"
	eventReasonCrowdSecMarkerRemoved = "CrowdSecMarkerRemoved"
	eventReasonCrowdSecMarkerFailed  = "CrowdSecMarkerFailed"
	eventReasonCrowdSecMarkerRefused = "CrowdSecMarkerRefused"

	fieldOwnerCrowdSec = "dynamic-prefix-operator/crowdsec"

	// DefaultCrowdSecMaxMarkers bounds the markers kept per DynamicPrefix: the
	// current prefix and a couple still draining.
	DefaultCrowdSecMaxMarkers = 3

	crowdsecMinPrefixLen = 48
	crowdsecMaxPrefixLen = 64
)

// globalUnicast is 2000::/3. A marker outside it would trust addresses that
// can never be a delegated LAN prefix.
var globalUnicast = netip.MustParsePrefix("2000::/3")

// crowdsecMarkerAPI is the slice of the LAPI client the reconciler uses.
type crowdsecMarkerAPI interface {
	listMarkers(ctx context.Context) ([]crowdsecMarker, error)
	createMarker(ctx context.Context, dynamicPrefix, cidr string) error
	deleteMarker(ctx context.Context, cidr string) error
}

// CrowdSecMarkerReconciler keeps one CrowdSec LAPI allow decision ("marker")
// per trusted prefix of each configured DynamicPrefix: the current prefix and
// the ones still draining. A LAPI profile that breaks on alerts inside a
// marker then stops CrowdSec banning the LAN while its WAF keeps inspecting.
//
// It is event-driven only: a DynamicPrefix change, the start of leadership
// (the initial list), the LAPI Service's EndpointSlices changing, and
// workqueue backoff after a failure. Like every controller it runs on the
// leader only, so a single replica writes.
type CrowdSecMarkerReconciler struct {
	client.Client
	Recorder events.EventRecorder
	LAPI     crowdsecMarkerAPI
	// DynamicPrefixes names the DynamicPrefixes whose prefixes are trusted.
	DynamicPrefixes []string
	// MaxMarkers refuses a sync that would keep more markers than this for
	// one DynamicPrefix.
	MaxMarkers int
	// LAPIService, when set, gates syncs on the Service having a ready
	// endpoint and re-triggers them when its EndpointSlices change.
	LAPIService *types.NamespacedName
}

// crowdsecTrustedPrefixes is the current prefix followed by the prefixes still
// draining, newest first.
func crowdsecTrustedPrefixes(dp *dynamicprefixiov1alpha1.DynamicPrefix) []string {
	if dp.Status.CurrentPrefix == "" {
		return nil
	}
	prefixes := []string{dp.Status.CurrentPrefix}
	for i := len(dp.Status.History) - 1; i >= 0; i-- {
		entry := dp.Status.History[i]
		if entry.State == dynamicprefixiov1alpha1.PrefixStateExpired || slices.Contains(prefixes, entry.Prefix) {
			continue
		}
		prefixes = append(prefixes, entry.Prefix)
	}
	return prefixes
}

// validateMarkerPrefixes is the last check before anything is written to LAPI,
// where a wrong range would stop CrowdSec banning whatever it covers.
func validateMarkerPrefixes(prefixes []string, maxMarkers int) error {
	if len(prefixes) > maxMarkers {
		return fmt.Errorf("%d trusted prefixes exceed the maximum of %d markers", len(prefixes), maxMarkers)
	}
	for _, p := range prefixes {
		pfx, err := netip.ParsePrefix(p)
		switch {
		case err != nil:
			return fmt.Errorf("prefix %q does not parse: %w", p, err)
		case !pfx.Addr().Is6() || pfx.Addr().Is4In6():
			return fmt.Errorf("prefix %q is not IPv6", p)
		case !globalUnicast.Contains(pfx.Addr()):
			return fmt.Errorf("prefix %q is outside global unicast %s", p, globalUnicast)
		case pfx.Bits() < crowdsecMinPrefixLen || pfx.Bits() > crowdsecMaxPrefixLen:
			return fmt.Errorf("prefix %q is not between /%d and /%d", p, crowdsecMinPrefixLen, crowdsecMaxPrefixLen)
		case pfx.Masked().String() != p:
			return fmt.Errorf("prefix %q is not in canonical form %s", p, pfx.Masked())
		}
	}
	return nil
}

// Reconcile brings the markers owned by one DynamicPrefix in line with its
// trusted prefixes: missing ones are created first, then obsolete ones expired,
// so a rotation never leaves a moment with neither prefix trusted.
func (r *CrowdSecMarkerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	name := req.Name
	if !slices.Contains(r.DynamicPrefixes, name) {
		return ctrl.Result{}, nil
	}

	var dp dynamicprefixiov1alpha1.DynamicPrefix
	found := true
	if err := r.Get(ctx, req.NamespacedName, &dp); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		// Deleted: its markers are expired, and nothing is left to report on.
		found = false
	}

	var desired []string
	if found {
		desired = crowdsecTrustedPrefixes(&dp)
		if len(desired) == 0 {
			// No prefix yet, or lost: the last markers stay the better answer
			// until a new prefix arrives, which is a watched status change.
			return ctrl.Result{}, nil
		}
		if err := validateMarkerPrefixes(desired, r.MaxMarkers); err != nil {
			msg := "Refusing to write CrowdSec markers: " + err.Error()
			r.report(ctx, &dp, "refused", eventReasonCrowdSecMarkerRefused, msg)
			// Only a change to the DynamicPrefix can fix this, and that is watched.
			return ctrl.Result{}, nil //nolint:nilerr // a refusal is not retried

		}
	}

	if ready, err := r.lapiReady(ctx); err != nil {
		return ctrl.Result{}, err
	} else if !ready {
		log.V(1).Info("CrowdSec LAPI Service has no ready endpoint; waiting for one", "dynamicPrefix", name)
		return ctrl.Result{}, nil
	}

	if err := r.sync(ctx, name, desired, &dp, found); err != nil {
		if found {
			r.report(ctx, &dp, "failed", eventReasonCrowdSecMarkerFailed, "CrowdSec marker sync failed: "+err.Error())
		} else {
			recordCrowdSecSyncMetric(name, "failed")
		}
		// Retried with the workqueue's exponential backoff.
		return ctrl.Result{}, err
	}
	recordCrowdSecSyncMetric(name, "success")
	if found {
		crowdsecMarkers.WithLabelValues(name).Set(float64(len(desired)))
		r.writeCondition(ctx, &dp, metav1.ConditionTrue, "MarkersSynced",
			"CrowdSec markers cover "+strings.Join(desired, ", "))
	} else {
		forgetCrowdSecMetrics(name)
	}
	return ctrl.Result{}, nil
}

func (r *CrowdSecMarkerReconciler) sync(
	ctx context.Context, name string, desired []string, dp *dynamicprefixiov1alpha1.DynamicPrefix, found bool,
) error {
	log := logf.FromContext(ctx)
	markers, err := r.LAPI.listMarkers(ctx)
	if err != nil {
		return err
	}
	var present, owned []string
	for _, m := range markers {
		present = append(present, m.Value)
		if m.Owner == name && !slices.Contains(owned, m.Value) {
			owned = append(owned, m.Value)
		}
	}

	for _, p := range desired {
		if slices.Contains(present, p) {
			continue
		}
		if err := r.LAPI.createMarker(ctx, name, p); err != nil {
			// Nothing is expired after a failed create.
			return fmt.Errorf("create marker for %s: %w", p, err)
		}
		log.Info("Created CrowdSec marker", "dynamicPrefix", name, "prefix", p)
		emitNormalEvent(r.Recorder, dp, eventReasonCrowdSecMarkerCreated, "Created CrowdSec allow marker for "+p)
	}

	for _, p := range owned {
		if slices.Contains(desired, p) {
			continue
		}
		// shortcut: the delete expires every marker with this value, including
		// one another DynamicPrefix owns for the same prefix; two DynamicPrefixes
		// sharing a prefix would need a per-owner decision lookup.
		if err := r.LAPI.deleteMarker(ctx, p); err != nil {
			return fmt.Errorf("expire marker for %s: %w", p, err)
		}
		log.Info("Expired CrowdSec marker", "dynamicPrefix", name, "prefix", p)
		if found {
			emitNormalEvent(r.Recorder, dp, eventReasonCrowdSecMarkerRemoved, "Expired CrowdSec allow marker for "+p)
		}
	}
	return nil
}

// lapiReady reports whether the configured LAPI Service has a ready endpoint.
// Without a configured Service it is always true, and failures surface as
// request errors instead.
func (r *CrowdSecMarkerReconciler) lapiReady(ctx context.Context) (bool, error) {
	if r.LAPIService == nil {
		return true, nil
	}
	var sliceList discoveryv1.EndpointSliceList
	if err := r.List(ctx, &sliceList, client.InNamespace(r.LAPIService.Namespace),
		client.MatchingLabels{discoveryv1.LabelServiceName: r.LAPIService.Name}); err != nil {
		return false, fmt.Errorf("list EndpointSlices of %s: %w", r.LAPIService, err)
	}
	for _, slice := range sliceList.Items {
		for _, ep := range slice.Endpoints {
			// A nil Ready is "unknown", which the EndpointSlice API says to treat as ready.
			if ep.Conditions.Ready == nil || *ep.Conditions.Ready {
				return true, nil
			}
		}
	}
	return false, nil
}

func (r *CrowdSecMarkerReconciler) report(
	ctx context.Context, dp *dynamicprefixiov1alpha1.DynamicPrefix, result, reason, msg string,
) {
	logf.FromContext(ctx).Info(msg, "dynamicPrefix", dp.Name)
	emitWarningEvent(r.Recorder, dp, reason, msg)
	recordCrowdSecSyncMetric(dp.Name, result)
	r.writeCondition(ctx, dp, metav1.ConditionFalse, reason, msg)
}

// writeCondition applies the CrowdSecMarkersSynced condition under this
// controller's own field manager. Condition-only changes do not pass the
// DynamicPrefix predicate, so the write does not trigger another sync.
func (r *CrowdSecMarkerReconciler) writeCondition(
	ctx context.Context, dp *dynamicprefixiov1alpha1.DynamicPrefix,
	status metav1.ConditionStatus, reason, message string,
) {
	entry, changed := conditionApplyEntry(dp,
		dynamicprefixiov1alpha1.ConditionTypeCrowdSecMarkersSynced, status, reason, message)
	if !changed {
		return
	}
	ac := acdynamicprefixv1alpha1.DynamicPrefix(dp.Name).
		WithStatus(acdynamicprefixv1alpha1.DynamicPrefixStatus().WithConditions(entry))
	if err := r.Status().Apply(ctx, ac, client.FieldOwner(fieldOwnerCrowdSec), client.ForceOwnership); err != nil {
		// A report only; the next sync writes it again.
		logf.FromContext(ctx).V(1).Info("Could not update the CrowdSecMarkersSynced condition",
			"dynamicPrefix", dp.Name, "error", err.Error())
	}
}

// SetupWithManager registers the controller. The manager's cache must be
// scoped to the LAPI Service's EndpointSlices, which is what lets a namespaced
// Role cover the watch.
func (r *CrowdSecMarkerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	configured := predicate.NewPredicateFuncs(func(obj client.Object) bool {
		return slices.Contains(r.DynamicPrefixes, obj.GetName())
	})
	b := ctrl.NewControllerManagedBy(mgr).
		Named("crowdsecmarker").
		For(&dynamicprefixiov1alpha1.DynamicPrefix{},
			builder.WithPredicates(configured, dynamicPrefixDependentChangePredicate()))
	if r.LAPIService != nil {
		b = b.Watches(&discoveryv1.EndpointSlice{},
			handler.EnqueueRequestsFromMapFunc(r.allDynamicPrefixes),
			builder.WithPredicates(predicate.NewPredicateFuncs(func(obj client.Object) bool {
				return obj.GetNamespace() == r.LAPIService.Namespace &&
					obj.GetLabels()[discoveryv1.LabelServiceName] == r.LAPIService.Name
			})))
	}
	return b.Complete(r)
}

func (r *CrowdSecMarkerReconciler) allDynamicPrefixes(context.Context, client.Object) []reconcile.Request {
	requests := make([]reconcile.Request, 0, len(r.DynamicPrefixes))
	for _, name := range r.DynamicPrefixes {
		requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Name: name}})
	}
	return requests
}
