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
	poolsSynced.DeletePartialMatch(prometheus.Labels{"dynamic_prefix": name})
}
