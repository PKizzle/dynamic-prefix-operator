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
	"net/netip"
	"slices"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	acmetav1 "k8s.io/client-go/applyconfigurations/meta/v1"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/source"

	dynamicprefixiov1alpha1 "github.com/pkizzle/dynamic-prefix-operator/api/v1alpha1"
	acdynamicprefixv1alpha1 "github.com/pkizzle/dynamic-prefix-operator/api/v1alpha1/applyconfiguration/api/v1alpha1"
	"github.com/pkizzle/dynamic-prefix-operator/internal/prefix"
)

const (
	finalizerName = "dynamic-prefix.io/finalizer"

	// prefixEventQueueSize bounds the wake-ups waiting to be turned into
	// reconcile requests. Reconcile always reads the receiver's current state,
	// so a queued wake-up subsumes any that follow it: the queue only has to be
	// deep enough that a burst across several DynamicPrefixes is not coalesced
	// into one, and shallow enough that a misbehaving source cannot grow it.
	prefixEventQueueSize = 32
)

// ReceiverFactory creates prefix receivers for DynamicPrefix resources
type ReceiverFactory interface {
	// CreateReceiver creates a new receiver based on the acquisition spec
	CreateReceiver(spec dynamicprefixiov1alpha1.AcquisitionSpec) (prefix.Receiver, error)
}

// DynamicPrefixReconciler reconciles a DynamicPrefix object
type DynamicPrefixReconciler struct {
	client.Client
	Scheme          *runtime.Scheme
	ReceiverFactory ReceiverFactory
	Recorder        events.EventRecorder

	// receiversMu protects the receivers map
	receiversMu sync.RWMutex
	// receivers maps DynamicPrefix name to its active receiver
	receivers map[string]prefix.Receiver
	// receiverSpecs records the acquisition spec each live receiver was built
	// from, so a spec that has since changed can be noticed. A receiver's
	// interface, source and acceptance policy are all fixed at construction --
	// receivers are even pooled per interface *and* policy for that reason -- so
	// the only way to honour an edit is to build a new one.
	receiverSpecs map[string]dynamicprefixiov1alpha1.AcquisitionSpec
	// receiverStops cancels the goroutine forwarding each receiver's events, so
	// a rebuilt or removed receiver does not leave one behind.
	receiverStops map[string]context.CancelFunc
	// rejectionReports remembers what was last reported about each resource's
	// dropped advertisements, so a link under a flood produces one event per
	// interval rather than one per reconcile.
	rejectionReports map[string]rejectionReport

	// receiverCtxMu protects receiverCtx.
	receiverCtxMu sync.RWMutex
	// receiverCtx is the long-lived context receivers are started with. It is
	// created in SetupWithManager and cancelled when the manager stops.
	// Receivers outlive the Reconcile call that starts them, so they must not be
	// tied to a per-request context.
	receiverCtx context.Context

	// prefixEvents carries a wake-up per receiver-observed prefix change into
	// the controller's work queue.
	//
	// Without it the only way a rotation reached status was the periodic
	// requeue, which is capped at five minutes -- so an ISP changing the prefix
	// could leave every derived address stale for that long, and each receiver's
	// event channel filled up and then dropped events forever, since nothing
	// ever drained it.
	prefixEvents chan event.GenericEvent
}

// NewDynamicPrefixReconciler creates a new reconciler with default configuration
func NewDynamicPrefixReconciler(c client.Client, scheme *runtime.Scheme) *DynamicPrefixReconciler {
	return &DynamicPrefixReconciler{
		Client:           c,
		Scheme:           scheme,
		receivers:        make(map[string]prefix.Receiver),
		receiverSpecs:    make(map[string]dynamicprefixiov1alpha1.AcquisitionSpec),
		receiverStops:    make(map[string]context.CancelFunc),
		rejectionReports: make(map[string]rejectionReport),
	}
}

// +kubebuilder:rbac:groups=dynamic-prefix.io,resources=dynamicprefixes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=dynamic-prefix.io,resources=dynamicprefixes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=dynamic-prefix.io,resources=dynamicprefixes/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *DynamicPrefixReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Fetch the DynamicPrefix instance
	var dp dynamicprefixiov1alpha1.DynamicPrefix
	if err := r.Get(ctx, req.NamespacedName, &dp); err != nil {
		// Only a genuine NotFound means the resource is gone. Tearing the
		// receiver down on any error would let a transient API-server blip or
		// a cache miss stop a live DHCPv6-PD/RA receiver, and the requeue would
		// then have to re-solicit a lease -- disrupting prefix acquisition for
		// a reason that has nothing to do with the resource.
		if apierrors.IsNotFound(err) {
			r.cleanupReceiver(req.Name)
			forgetPrefixMetrics(req.Name)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Handle deletion
	if !dp.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&dp, finalizerName) {
			log.Info("DynamicPrefix being deleted, cleaning up receiver")
			r.cleanupReceiver(dp.Name)

			controllerutil.RemoveFinalizer(&dp, finalizerName)
			if err := r.Update(ctx, &dp); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// Add finalizer if not present
	if !controllerutil.ContainsFinalizer(&dp, finalizerName) {
		controllerutil.AddFinalizer(&dp, finalizerName)
		if err := r.Update(ctx, &dp); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}

	originalStatus := dp.Status.DeepCopy()

	// Get or create the receiver for this DynamicPrefix
	receiver, err := r.getOrCreateReceiver(ctx, &dp)
	if err != nil {
		log.Error(err, "Failed to create receiver")
		r.setCondition(&dp, dynamicprefixiov1alpha1.ConditionTypePrefixAcquired, metav1.ConditionFalse,
			"ReceiverCreationFailed", err.Error())
		emitWarningEvent(r.Recorder, &dp, eventReasonReceiverCreationFailed, err.Error())
		if statusErr := r.updateStatusIfChanged(ctx, &dp, originalStatus); statusErr != nil {
			log.Error(statusErr, "Failed to update status")
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Ask the receiver whether it is failing. Failure events are deliberately
	// not forwarded to reconcile -- a down interface produces one a second and
	// none of them carry anything to act on -- so this is where they surface.
	acquisitionErr := receiverLastError(receiver)
	recordReceiverHealthMetric(dp.Name, acquisitionErr == nil)
	r.reportRARejections(&dp, receiver)

	// Get current prefix from receiver
	currentPrefix := receiver.CurrentPrefix()
	if currentPrefix == nil {
		if acquisitionErr != nil {
			// Trying and failing is not the same state as waiting for a first
			// advertisement, and reporting both as waiting left a resource that
			// could never acquire looking like one that simply had not yet.
			message := fmt.Sprintf("Prefix acquisition is failing: %v", acquisitionErr)
			log.Info("Prefix acquisition is failing", "error", acquisitionErr.Error())
			r.warnOnConditionChange(&dp, dynamicprefixiov1alpha1.ConditionTypePrefixAcquired,
				reasonAcquisitionFailed, message, eventReasonAcquisitionFailed)
			r.setCondition(&dp, dynamicprefixiov1alpha1.ConditionTypePrefixAcquired, metav1.ConditionFalse,
				reasonAcquisitionFailed, message)
		} else {
			log.Info("No prefix acquired yet")
			r.setCondition(&dp, dynamicprefixiov1alpha1.ConditionTypePrefixAcquired, metav1.ConditionFalse,
				reasonWaitingForPrefix, "Waiting to receive prefix from upstream")
		}
		if err := r.updateStatusIfChanged(ctx, &dp, originalStatus); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Gate every acquired prefix before it can reach status, history, subnet math
	// or any pool. The receivers apply the same rule, but a receiver is created
	// once and shared per interface, so one built under an earlier policy can
	// still be feeding prefixes acquired under the old rules; this is the single
	// choke point every source passes through.
	//
	// Rejecting is deliberately non-destructive: status keeps the last good
	// prefix, so nothing derived from it is disturbed while the upstream sorts
	// itself out.
	if err := prefixPolicyFor(&dp).Validate(currentPrefix.Network); err != nil {
		log.Info("Rejecting acquired prefix", "prefix", currentPrefix.Network, "reason", err.Error())
		r.setCondition(&dp, dynamicprefixiov1alpha1.ConditionTypePrefixAcquired, metav1.ConditionFalse,
			reasonPrefixRejected, fmt.Sprintf("Rejected acquired prefix %s: %v", currentPrefix.Network, err))
		emitWarningEvent(r.Recorder, &dp, eventReasonPrefixRejected,
			fmt.Sprintf("Rejected acquired prefix %s: %v", currentPrefix.Network, err))
		if err := r.updateStatusIfChanged(ctx, &dp, originalStatus); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Update status with current prefix
	oldPrefix := dp.Status.CurrentPrefix
	prefixChanged := oldPrefix != currentPrefix.Network.String()
	if prefixChanged {
		prefixSource := string(sourceToPrefixSource(receiver.Source()))
		log.Info("Prefix changed", "oldPrefix", oldPrefix, "newPrefix", currentPrefix.Network.String())
		recordPrefixReceivedMetric(dp.Name, prefixSource)
		if oldPrefix == "" {
			emitNormalEvent(r.Recorder, &dp, eventReasonPrefixReceived,
				fmt.Sprintf("Prefix %s acquired via %s", currentPrefix.Network, prefixSource))
		} else {
			recordPrefixChangedMetric(dp.Name)
			emitNormalEvent(r.Recorder, &dp, eventReasonPrefixChanged,
				fmt.Sprintf("Prefix changed from %s to %s via %s", oldPrefix, currentPrefix.Network, prefixSource))
		}
		r.handlePrefixChange(ctx, &dp, currentPrefix)
	}

	dp.Status.CurrentPrefix = currentPrefix.Network.String()
	dp.Status.PrefixSource = sourceToPrefixSource(receiver.Source())

	// Calculate lease expiration
	if currentPrefix.ValidLifetime > 0 {
		expiresAt := metav1.NewTime(currentPrefix.ReceivedAt.Add(currentPrefix.ValidLifetime))
		dp.Status.LeaseExpiresAt = &expiresAt
		recordPrefixLeaseExpiryMetric(dp.Name, &expiresAt.Time)
	} else {
		dp.Status.LeaseExpiresAt = nil
		recordPrefixLeaseExpiryMetric(dp.Name, nil)
	}

	// Calculate subnets (Mode 2)
	subnets, err := r.calculateSubnets(dp.Name, currentPrefix.Network, dp.Spec.Subnets)
	if err != nil {
		log.Error(err, "Failed to calculate subnets")
		r.setCondition(&dp, dynamicprefixiov1alpha1.ConditionTypeDegraded, metav1.ConditionTrue,
			"SubnetCalculationFailed", err.Error())
	} else {
		dp.Status.Subnets = subnets
	}

	// Calculate address ranges (Mode 1)
	addressRanges, err := r.calculateAddressRanges(currentPrefix.Network, dp.Spec.AddressRanges)
	if err != nil {
		log.Error(err, "Failed to calculate address ranges")
		r.setCondition(&dp, dynamicprefixiov1alpha1.ConditionTypeDegraded, metav1.ConditionTrue,
			"AddressRangeCalculationFailed", err.Error())
	} else {
		dp.Status.AddressRanges = addressRanges
		// Only set healthy if subnets also succeeded
		if subnets != nil || len(dp.Spec.Subnets) == 0 {
			r.setCondition(&dp, dynamicprefixiov1alpha1.ConditionTypeDegraded, metav1.ConditionFalse,
				"Healthy", "DynamicPrefix is operating normally")
		}
	}

	// Holding a prefix is not the same as being healthy: this is the resource
	// still serving the last delegation while every renewal since has failed.
	// Set after the calculation conditions so it is not cleared by them.
	if acquisitionErr != nil {
		r.setCondition(&dp, dynamicprefixiov1alpha1.ConditionTypeDegraded, metav1.ConditionTrue,
			reasonRenewalFailing,
			fmt.Sprintf("Still serving the last prefix; acquisition is failing: %v", acquisitionErr))
	}

	// Set prefix acquired condition
	r.setCondition(&dp, dynamicprefixiov1alpha1.ConditionTypePrefixAcquired, metav1.ConditionTrue,
		"PrefixAcquired", fmt.Sprintf("Prefix %s acquired via %s", currentPrefix.Network, receiver.Source()))

	// Update status only when it actually changed to avoid self-triggered reconcile loops.
	if err := r.updateStatusIfChanged(ctx, &dp, originalStatus); err != nil {
		return ctrl.Result{}, err
	}

	// Requeue to handle lease renewal
	requeueAfter := r.calculateRequeueTime(currentPrefix)
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// updateStatusIfChanged writes the DynamicPrefix status only when a semantic change occurred.
//
// Applied rather than updated, under this reconciler's own field manager. The
// apply carries every field this reconciler owns -- which is all of status
// except the two conditions the pool and BGP controllers report -- so those
// writers cannot be raced and their entries cannot be disturbed: conditions
// merge per type, and nothing else in status has a second writer.
func (r *DynamicPrefixReconciler) updateStatusIfChanged(
	ctx context.Context,
	dp *dynamicprefixiov1alpha1.DynamicPrefix,
	originalStatus *dynamicprefixiov1alpha1.DynamicPrefixStatus,
) error {
	desired := ownedDynamicPrefixStatus(&dp.Status)
	if equality.Semantic.DeepEqual(ownedDynamicPrefixStatus(originalStatus), desired) {
		return nil
	}
	ac := acdynamicprefixv1alpha1.DynamicPrefix(dp.Name).WithStatus(desired)
	return r.Status().Apply(ctx, ac, client.FieldOwner(fieldOwnerDynamicPrefix), client.ForceOwnership)
}

// ownedDynamicPrefixStatus projects a status onto the fields this reconciler
// owns, as the apply configuration it would write. Conditions belonging to the
// other status writers are left out: naming them here would take them over,
// and omitting them on the next apply would then delete them.
func ownedDynamicPrefixStatus(
	status *dynamicprefixiov1alpha1.DynamicPrefixStatus,
) *acdynamicprefixv1alpha1.DynamicPrefixStatusApplyConfiguration {
	ac := acdynamicprefixv1alpha1.DynamicPrefixStatus()
	if status.CurrentPrefix != "" {
		ac.WithCurrentPrefix(status.CurrentPrefix)
	}
	if status.PrefixSource != "" {
		ac.WithPrefixSource(status.PrefixSource)
	}
	if status.LeaseExpiresAt != nil {
		ac.WithLeaseExpiresAt(*status.LeaseExpiresAt)
	}
	for _, r := range status.AddressRanges {
		entry := acdynamicprefixv1alpha1.AddressRangeStatus().
			WithName(r.Name).WithStart(r.Start).WithEnd(r.End)
		if r.CIDR != "" {
			entry.WithCIDR(r.CIDR)
		}
		ac.WithAddressRanges(entry)
	}
	for _, s := range status.Subnets {
		entry := acdynamicprefixv1alpha1.SubnetStatus().WithName(s.Name).WithCIDR(s.CIDR)
		if s.BGPAdvertisement != "" {
			entry.WithBGPAdvertisement(s.BGPAdvertisement)
		}
		ac.WithSubnets(entry)
	}
	for _, h := range status.History {
		entry := acdynamicprefixv1alpha1.PrefixHistoryEntry().
			WithPrefix(h.Prefix).WithAcquiredAt(h.AcquiredAt)
		if h.DeprecatedAt != nil {
			entry.WithDeprecatedAt(*h.DeprecatedAt)
		}
		if h.State != "" {
			entry.WithState(h.State)
		}
		ac.WithHistory(entry)
	}
	for _, condType := range []string{
		dynamicprefixiov1alpha1.ConditionTypePrefixAcquired,
		dynamicprefixiov1alpha1.ConditionTypeDegraded,
	} {
		if c := meta.FindStatusCondition(status.Conditions, condType); c != nil {
			ac.WithConditions(acmetav1.Condition().
				WithType(c.Type).
				WithStatus(c.Status).
				WithObservedGeneration(c.ObservedGeneration).
				WithLastTransitionTime(c.LastTransitionTime).
				WithReason(c.Reason).
				WithMessage(c.Message))
		}
	}
	return ac
}

// getOrCreateReceiver returns an existing receiver or creates a new one
func (r *DynamicPrefixReconciler) getOrCreateReceiver(ctx context.Context, dp *dynamicprefixiov1alpha1.DynamicPrefix) (prefix.Receiver, error) {
	log := logf.FromContext(ctx)

	r.receiversMu.RLock()
	receiver, exists := r.receivers[dp.Name]
	recorded, recordedOK := r.receiverSpecs[dp.Name]
	r.receiversMu.RUnlock()

	if exists && recordedOK && equality.Semantic.DeepEqual(recorded, dp.Spec.Acquisition) {
		return receiver, nil
	}

	// Create new receiver
	r.receiversMu.Lock()
	defer r.receiversMu.Unlock()

	// The maps are only built by NewDynamicPrefixReconciler; a struct-literal
	// construction would otherwise panic on the writes below. Allocated up front
	// so every write past this point is safe, not just the last one.
	if r.receivers == nil {
		r.receivers = make(map[string]prefix.Receiver)
	}
	if r.receiverSpecs == nil {
		r.receiverSpecs = make(map[string]dynamicprefixiov1alpha1.AcquisitionSpec)
	}
	if r.receiverStops == nil {
		r.receiverStops = make(map[string]context.CancelFunc)
	}

	// Double-check after acquiring write lock
	if receiver, exists = r.receivers[dp.Name]; exists {
		recorded, recordedOK = r.receiverSpecs[dp.Name]
		if !recordedOK {
			// A live receiver whose spec was never recorded: adopt it and record
			// what the resource says now. Rebuilding on no evidence would discard
			// a working receiver -- and the prefix it has already acquired -- to
			// answer a question nothing asked. The next genuine edit is still
			// caught, because from here on there is something to compare against.
			r.receiverSpecs[dp.Name] = dp.Spec.Acquisition
			return receiver, nil
		}
		if equality.Semantic.DeepEqual(recorded, dp.Spec.Acquisition) {
			return receiver, nil
		}

		// The spec that built this receiver no longer matches the one on the
		// resource. Everything the receiver was configured with is fixed at
		// construction, so editing the interface, switching between RA and
		// DHCPv6-PD or changing the prefix filter would otherwise be accepted
		// silently and then ignored until the pod happened to restart.
		log.Info("Acquisition spec changed, rebuilding receiver", "name", dp.Name)
		if err := receiver.Stop(); err != nil {
			log.Error(err, "Failed to stop the previous receiver", "name", dp.Name)
		}
		r.stopReceiverLocked(dp.Name)
		emitNormalEvent(r.Recorder, dp, eventReasonReceiverRebuilt,
			"Acquisition settings changed; the prefix receiver was rebuilt")
	}

	if r.ReceiverFactory == nil {
		// A reconciler wired by main always has a factory. Substituting a mock
		// here would leave the operator reporting a fabricated prefix and writing
		// it into real pools, so a missing factory has to be a failure rather
		// than a fallback. Tests that want the mock set it explicitly.
		return nil, fmt.Errorf("no receiver factory configured for DynamicPrefix %s", dp.Name)
	}
	receiver, err := r.ReceiverFactory.CreateReceiver(dp.Spec.Acquisition)
	if err != nil {
		return nil, fmt.Errorf("failed to create receiver: %w", err)
	}

	// Start the receiver
	receiverCtx := r.receiverContext(ctx)
	if err := receiver.Start(receiverCtx); err != nil {
		return nil, fmt.Errorf("failed to start receiver: %w", err)
	}

	r.receivers[dp.Name] = receiver
	r.receiverSpecs[dp.Name] = dp.Spec.Acquisition

	// Drain this receiver's events into the controller's queue. Started here so
	// every receiver has exactly one reader for its lifetime: the channel is
	// small and drops on overflow, so leaving it unread turns every subsequent
	// event into a dropped one.
	if r.prefixEvents != nil {
		// The cancel is stored, not deferred: it belongs to the receiver's
		// lifetime, and stopReceiverLocked calls it when the receiver is removed
		// or rebuilt. Deferring it here would kill the forwarder immediately.
		forwardCtx, cancel := context.WithCancel(receiverCtx) // #nosec G118 -- cancel is retained in receiverStops and invoked by stopReceiverLocked
		r.receiverStops[dp.Name] = cancel
		go r.forwardPrefixEvents(forwardCtx, dp.Name, receiver.Events())
	}

	return receiver, nil
}

// forwardPrefixEvents turns receiver events into reconcile requests for one
// DynamicPrefix.
//
// Only events that can change the prefix are forwarded. Renewals move just the
// lease expiry, which the periodic requeue already refreshes. Failures are not
// forwarded either: reconcile reads the receiver's current prefix and has no
// access to the error, so a failed receive produces no state change, while the
// RA loop reports one failure per second for as long as an interface is down.
func (r *DynamicPrefixReconciler) forwardPrefixEvents(ctx context.Context, name string, received <-chan prefix.Event) {
	log := logf.Log.WithName("dynamicprefix").WithValues("name", name)

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-received:
			if !ok {
				return
			}
			switch ev.Type {
			case prefix.EventTypeAcquired, prefix.EventTypeChanged, prefix.EventTypeExpired:
			default:
				continue
			}

			select {
			case r.prefixEvents <- event.GenericEvent{
				Object: &dynamicprefixiov1alpha1.DynamicPrefix{
					ObjectMeta: metav1.ObjectMeta{Name: name},
				},
			}:
				log.V(1).Info("Woke the reconciler for a prefix event", "eventType", ev.Type)
			default:
				// The queue is shared by every DynamicPrefix and is only this full
				// when reconciles are already backed up. Each reconcile reads the
				// receiver's current state, so the pending wake-ups carry whatever
				// this one would have said.
				log.V(1).Info("Prefix event queue is full, not queueing another", "eventType", ev.Type)
			}
		}
	}
}

// stopReceiverLocked forgets a receiver and stops forwarding its events. The
// caller must hold receiversMu for writing; stopping the receiver itself is the
// caller's business, since the two call sites differ in how they report failure.
func (r *DynamicPrefixReconciler) stopReceiverLocked(name string) {
	if cancel, ok := r.receiverStops[name]; ok {
		cancel()
		delete(r.receiverStops, name)
	}
	delete(r.receivers, name)
	delete(r.receiverSpecs, name)
	delete(r.rejectionReports, name)
}

// cleanupReceiver stops and removes a receiver
func (r *DynamicPrefixReconciler) cleanupReceiver(name string) {
	r.receiversMu.Lock()
	defer r.receiversMu.Unlock()

	receiver, exists := r.receivers[name]
	if !exists {
		return
	}

	if err := receiver.Stop(); err != nil {
		logf.Log.Error(err, "Failed to stop receiver", "name", name)
	}
	r.stopReceiverLocked(name)
}

// calculateSubnets calculates subnet CIDRs from the base prefix
func (r *DynamicPrefixReconciler) calculateSubnets(
	dpName string,
	basePrefix netip.Prefix,
	specs []dynamicprefixiov1alpha1.SubnetSpec,
) ([]dynamicprefixiov1alpha1.SubnetStatus, error) {
	if len(specs) == 0 {
		return nil, nil
	}

	configs := make([]prefix.SubnetConfig, len(specs))
	for i, spec := range specs {
		configs[i] = prefix.SubnetConfig{
			Name:         spec.Name,
			Offset:       spec.Offset,
			PrefixLength: spec.PrefixLength,
		}
	}

	subnets, err := prefix.CalculateSubnets(basePrefix, configs)
	if err != nil {
		return nil, err
	}

	// BGPAdvertisement is derived here, not copied from BGPSync's writes:
	// status.subnets is an atomic list this reconciler owns, and the name is a
	// pure function of spec. Whether the advertisement is actually ready stays
	// BGPSync's to report, through its condition.
	advertised := make(map[string]string, len(specs))
	for _, spec := range specs {
		if spec.BGP != nil && spec.BGP.Advertise {
			advertised[spec.Name] = advertisementName(dpName, spec.Name)
		}
	}

	result := make([]dynamicprefixiov1alpha1.SubnetStatus, len(subnets))
	for i, s := range subnets {
		result[i] = dynamicprefixiov1alpha1.SubnetStatus{
			Name:             s.Name,
			CIDR:             s.CIDR.String(),
			BGPAdvertisement: advertised[s.Name],
		}
	}

	return result, nil
}

// calculateAddressRanges calculates address ranges from the base prefix (Mode 1)
func (r *DynamicPrefixReconciler) calculateAddressRanges(basePrefix netip.Prefix, specs []dynamicprefixiov1alpha1.AddressRangeSpec) ([]dynamicprefixiov1alpha1.AddressRangeStatus, error) {
	if len(specs) == 0 {
		return nil, nil
	}

	configs := make([]prefix.AddressRangeConfig, len(specs))
	for i, spec := range specs {
		configs[i] = prefix.AddressRangeConfig{
			Name:  spec.Name,
			Start: spec.Start,
			End:   spec.End,
		}
	}

	ranges, err := prefix.CalculateAddressRanges(basePrefix, configs)
	if err != nil {
		return nil, err
	}

	result := make([]dynamicprefixiov1alpha1.AddressRangeStatus, len(ranges))
	for i, ar := range ranges {
		// Calculate an approximate CIDR for compatibility
		cidr := prefix.RangeToCIDR(ar.Start, ar.End)
		result[i] = dynamicprefixiov1alpha1.AddressRangeStatus{
			Name:  ar.Name,
			Start: ar.Start.String(),
			End:   ar.End.String(),
			CIDR:  cidr.String(),
		}
	}

	return result, nil
}

// handlePrefixChange handles graceful prefix transitions
func (r *DynamicPrefixReconciler) handlePrefixChange(ctx context.Context, dp *dynamicprefixiov1alpha1.DynamicPrefix, newPrefix *prefix.Prefix) {
	log := logf.FromContext(ctx)
	now := metav1.Now()

	// Add old prefix to history if it exists
	if dp.Status.CurrentPrefix != "" {
		// The PrefixAcquired condition last flipped when this prefix arrived,
		// which is the closest recorded acquisition time. dp.CreationTimestamp
		// is the CR's own creation, which would stamp every history entry with
		// the same wrong instant.
		acquiredAt := dp.CreationTimestamp
		if cond := meta.FindStatusCondition(dp.Status.Conditions,
			dynamicprefixiov1alpha1.ConditionTypePrefixAcquired); cond != nil && !cond.LastTransitionTime.IsZero() {
			acquiredAt = cond.LastTransitionTime
		}

		oldEntry := dynamicprefixiov1alpha1.PrefixHistoryEntry{
			Prefix:       dp.Status.CurrentPrefix,
			AcquiredAt:   acquiredAt,
			DeprecatedAt: &now,
			State:        dynamicprefixiov1alpha1.PrefixStateDraining,
		}

		// Drop any earlier entry for this same prefix first. A prefix that flaps
		// A -> B -> A would otherwise leave A in history while A is also the
		// current prefix, and the pool builders read current and history as
		// distinct sets: Cilium and MetalLB dedupe and merely do redundant work,
		// but Calico turns the duplicate into a sibling IPPool carrying its
		// parent's CIDR, which Calico's own validation rejects -- so the sync
		// fails, retries, and fails again for as long as the flap persists.
		dp.Status.History = slices.DeleteFunc(dp.Status.History,
			func(entry dynamicprefixiov1alpha1.PrefixHistoryEntry) bool {
				return entry.Prefix == oldEntry.Prefix || entry.Prefix == newPrefix.Network.String()
			})

		dp.Status.History = append(dp.Status.History, oldEntry)

		// Limit history size
		maxHistory := 2
		if dp.Spec.Transition != nil && dp.Spec.Transition.MaxPrefixHistory > 0 {
			maxHistory = dp.Spec.Transition.MaxPrefixHistory
		}
		if len(dp.Status.History) > maxHistory {
			completed := dp.Status.History[:len(dp.Status.History)-maxHistory]
			dp.Status.History = dp.Status.History[len(dp.Status.History)-maxHistory:]
			for _, entry := range completed {
				emitNormalEvent(r.Recorder, dp, eventReasonTransitionCompleted,
					fmt.Sprintf("Completed transition for historical prefix %s after retaining %d prefix history entry(s)", entry.Prefix, maxHistory))
			}
		}

		log.Info("Added prefix to history",
			"oldPrefix", dp.Status.CurrentPrefix,
			"newPrefix", newPrefix.Network.String(),
			"state", dynamicprefixiov1alpha1.PrefixStateDraining)
		emitNormalEvent(r.Recorder, dp, eventReasonTransitionStarted,
			fmt.Sprintf("Started draining historical prefix %s after acquiring %s", dp.Status.CurrentPrefix, newPrefix.Network))
	}
}

// setCondition sets a condition on the DynamicPrefix status
func (r *DynamicPrefixReconciler) setCondition(dp *dynamicprefixiov1alpha1.DynamicPrefix, condType string, status metav1.ConditionStatus, reason, message string) {
	condition := metav1.Condition{
		Type:               condType,
		Status:             status,
		ObservedGeneration: dp.Generation,
		LastTransitionTime: metav1.Now(),
		Reason:             reason,
		Message:            message,
	}
	meta.SetStatusCondition(&dp.Status.Conditions, condition)
}

// warnOnConditionChange emits a Warning only when the condition it accompanies
// is about to say something new.
//
// The underlying failure repeats for as long as its cause lasts -- once a
// second for a down interface -- and reconcile runs on its own schedule on top
// of that. Tying the event to the message changing means one event per distinct
// failure, and the comparison is against the persisted condition, so it holds
// across operator restarts too.
func (r *DynamicPrefixReconciler) warnOnConditionChange(dp *dynamicprefixiov1alpha1.DynamicPrefix, condType, reason, message, eventReason string) {
	existing := meta.FindStatusCondition(dp.Status.Conditions, condType)
	if existing != nil && existing.Reason == reason && existing.Message == message {
		return
	}
	emitWarningEvent(r.Recorder, dp, eventReason, message)
}

// prefixPolicyFor resolves the acceptance rules a resource asks for. The
// receivers apply the same policy, but a receiver is built once and shared per
// interface, so one built under an earlier configuration can still be feeding
// prefixes acquired under the old rules.
func prefixPolicyFor(dp *dynamicprefixiov1alpha1.DynamicPrefix) prefix.Policy {
	minBits, maxBits := dp.PrefixLengthBounds()
	return prefix.Policy{
		RequireGlobalUnicast: dp.RequireGlobalUnicast(),
		MinPrefixLength:      minBits,
		MaxPrefixLength:      maxBits,
	}
}

// receiverLastError reports why a receiver is failing, for the receivers that
// can say. Ones that cannot are treated as healthy: they have no way to
// contradict it.
func receiverLastError(receiver prefix.Receiver) error {
	health, ok := receiver.(prefix.AcquisitionHealth)
	if !ok {
		return nil
	}
	return health.LastError()
}

// calculateRequeueTime determines when to requeue based on lease
func (r *DynamicPrefixReconciler) calculateRequeueTime(p *prefix.Prefix) time.Duration {
	if p.ValidLifetime == 0 {
		// No lease expiration, requeue periodically
		return 5 * time.Minute
	}

	// Requeue at 80% of remaining lease time, but not less than 1 minute
	remaining := time.Until(p.ReceivedAt.Add(p.ValidLifetime))
	requeue := time.Duration(float64(remaining) * 0.8)
	if requeue < time.Minute {
		requeue = time.Minute
	}
	if requeue > 5*time.Minute {
		requeue = 5 * time.Minute
	}
	return requeue
}

// sourceToPrefixSource converts prefix.Source to v1alpha1.PrefixSource
func sourceToPrefixSource(s prefix.Source) dynamicprefixiov1alpha1.PrefixSource {
	switch s {
	case prefix.SourceDHCPv6PD:
		return dynamicprefixiov1alpha1.PrefixSourceDHCPv6PD
	case prefix.SourceRouterAdvertisement:
		return dynamicprefixiov1alpha1.PrefixSourceRouterAdvertisement
	case prefix.SourceStatic:
		return dynamicprefixiov1alpha1.PrefixSourceStatic
	default:
		return dynamicprefixiov1alpha1.PrefixSourceUnknown
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *DynamicPrefixReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Receivers own goroutines that outlive the Reconcile call that created
	// them, so they need a context that lives as long as the manager. It is
	// created here, synchronously, rather than captured from a Runnable: a
	// Runnable is started concurrently with the controllers, so a Reconcile
	// could win the race and start a receiver with the request context instead
	// -- which is cancelled the moment that call returns, leaving a dead
	// receiver cached under a live name and the resource stuck waiting for a
	// prefix until the pod restarted.
	receiverCtx, cancelReceivers := context.WithCancel(context.Background())
	r.receiverCtxMu.Lock()
	r.receiverCtx = receiverCtx
	r.receiverCtxMu.Unlock()

	r.prefixEvents = make(chan event.GenericEvent, prefixEventQueueSize)

	if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		<-ctx.Done()
		// Stops every receiver and every event forwarder with the manager.
		cancelReceivers()
		return nil
	})); err != nil {
		cancelReceivers()
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&dynamicprefixiov1alpha1.DynamicPrefix{}).
		WatchesRawSource(source.Channel(r.prefixEvents, &handler.EnqueueRequestForObject{})).
		Named("dynamicprefix").
		Complete(r)
}

// receiverContext returns the long-lived context receivers should be started
// with, falling back to the caller's context when the manager has not handed
// one over yet (unit tests construct the reconciler directly).
func (r *DynamicPrefixReconciler) receiverContext(ctx context.Context) context.Context {
	r.receiverCtxMu.RLock()
	defer r.receiverCtxMu.RUnlock()
	if r.receiverCtx != nil {
		return r.receiverCtx
	}
	return ctx
}
