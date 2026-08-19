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
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	prefixReceivedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "dynamic_prefix_received_total",
			Help: "Total number of newly acquired dynamic prefixes.",
		},
		[]string{"name", "source"},
	)

	prefixChangesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "dynamic_prefix_changes_total",
			Help: "Total number of dynamic prefix changes.",
		},
		[]string{"name"},
	)

	prefixLeaseExpirySeconds = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "dynamic_prefix_lease_expiry_seconds",
			Help: "Unix timestamp, in seconds, when the current dynamic prefix lease expires. Zero means no lease expiry is known.",
		},
		[]string{"name"},
	)

	receiverHealthy = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "dynamic_prefix_receiver_healthy",
			Help: "Whether the prefix receiver's last acquisition attempt succeeded. A value of 0 means acquisition or renewal is currently failing.",
		},
		[]string{"name"},
	)

	raRejectedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "dynamic_prefix_rejected_router_advertisements_total",
			Help: "Router Advertisements dropped by validation, by interface and reason.",
		},
		[]string{"interface", "reason"},
	)

	// One of the two defences against a forged Router Advertisement, reported
	// so its absence is visible. The other is the trusted-router source check,
	// and that one is spoofable by anything on the link, so knowing whether
	// this one is actually in force matters. It can only be off when the socket
	// refuses to report hop limits, which is a property of the node, not of the
	// traffic -- a startup log line was the only sign before.
	raHopLimitCheckEnabled = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "dynamic_prefix_ra_hop_limit_check_enabled",
			Help: "Whether the RFC 4861 hop-limit check is in force for Router Advertisements on this interface. A value of 0 means the socket would not report hop limits, so off-link advertisements are no longer excluded by that check.",
		},
		[]string{"interface"},
	)

	poolsSynced = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "dynamic_prefix_pools_synced",
			Help: "Pool sync state by backend and pool. A value of 1 indicates the pool is currently in sync.",
		},
		[]string{"backend", "dynamic_prefix", "pool"},
	)
)

func init() {
	metrics.Registry.MustRegister(
		prefixReceivedTotal,
		prefixChangesTotal,
		prefixLeaseExpirySeconds,
		receiverHealthy,
		raRejectedTotal,
		raHopLimitCheckEnabled,
		poolsSynced,
	)
}

func recordPrefixReceivedMetric(name, source string) {
	prefixReceivedTotal.WithLabelValues(name, source).Inc()
}

func recordPrefixChangedMetric(name string) {
	prefixChangesTotal.WithLabelValues(name).Inc()
}

func recordPrefixLeaseExpiryMetric(name string, expiresAt *time.Time) {
	if expiresAt == nil {
		prefixLeaseExpirySeconds.WithLabelValues(name).Set(0)
		return
	}
	prefixLeaseExpirySeconds.WithLabelValues(name).Set(float64(expiresAt.Unix()))
}

// recordReceiverHealthMetric reports whether acquisition is currently working.
// A resource can hold a prefix and still be unhealthy: the lease it holds may
// be one no renewal has extended for hours.
func recordReceiverHealthMetric(name string, healthy bool) {
	value := 0.0
	if healthy {
		value = 1
	}
	receiverHealthy.WithLabelValues(name).Set(value)
}

// RecordRARejection counts one dropped Router Advertisement. Exported because
// the prefix package reports drops through it without importing this registry.
// The reason is one of a small fixed set, so the label cannot be grown by
// anything arriving on the link.
func RecordRARejection(iface, reason string) {
	raRejectedTotal.WithLabelValues(iface, reason).Inc()
}

// RecordRAHopLimitCheck reports whether the hop-limit check is in force on an
// interface. Exported for the same reason as RecordRARejection: the prefix
// package reports through it without importing this registry.
func RecordRAHopLimitCheck(iface string, enabled bool) {
	value := 0.0
	if enabled {
		value = 1
	}
	raHopLimitCheckEnabled.WithLabelValues(iface).Set(value)
}

func recordPoolSyncedMetric(backend, dynamicPrefix, pool string) {
	poolsSynced.WithLabelValues(backend, dynamicPrefix, pool).Set(1)
}

// recordPoolSyncFailedMetric marks a pool as out of sync. Without it the gauge
// was only ever set to 1, so it could never report the state its own help text
// describes and an alert on `== 0` could never fire.
func recordPoolSyncFailedMetric(backend, dynamicPrefix, pool string) {
	poolsSynced.WithLabelValues(backend, dynamicPrefix, pool).Set(0)
}

// forgetPoolMetrics drops the series for a pool the operator no longer manages.
//
// A gauge keeps reporting its last value forever, so a released or deleted pool
// would go on claiming to be in sync -- or, worse, out of sync -- indefinitely,
// and in a cluster that churns pools the label set grows for the life of the
// process.
//
// Matched on backend and pool rather than on all three labels: the release path
// runs after the dynamic-prefix.io/name annotation is gone, so the DynamicPrefix
// the series was recorded under is no longer knowable there.
func forgetPoolMetrics(backend, pool string) {
	poolsSynced.DeletePartialMatch(prometheus.Labels{"backend": backend, "pool": pool})
}

// forgetPrefixMetrics drops the series for a DynamicPrefix that has been
// deleted. The counters are cumulative and keep their meaning across a
// resource's life, but the lease-expiry gauge would otherwise report an expiry
// for a resource that no longer exists.
func forgetPrefixMetrics(name string) {
	prefixLeaseExpirySeconds.DeleteLabelValues(name)
	receiverHealthy.DeleteLabelValues(name)
	poolsSynced.DeletePartialMatch(prometheus.Labels{"dynamic_prefix": name})
}
