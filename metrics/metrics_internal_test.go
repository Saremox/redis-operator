package metrics

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
)

// TestStaleCheckMetricsClearedIndependentlyOfResource guards against a regression of the
// unbounded cardinality leak in redisCheck/sentinelCheck: previously, per-instance (Pod IP)
// series were only garbage collected once the *whole* RedisFailover resource stopped being
// reconciled (resourceMetricLastUpdated going stale). For an actively managed cluster that
// resource-level timestamp is refreshed on every reconcile (e.g. every 30s), so it never goes
// stale, and every historical Sentinel/Redis Pod IP (created by restarts, rollouts,
// rescheduling, ...) accumulated as a permanent time series, growing memory without bound over
// the operator's lifetime.
func TestStaleCheckMetricsClearedIndependentlyOfResource(t *testing.T) {
	reg := prometheus.NewRegistry()
	rec := NewRecorder("leak_test", reg).(recorder)

	const (
		namespace = "ns"
		resource  = "rf"
		indicator = "SOME_INDICATOR"
		instance  = "10.0.0.1"
	)

	// A stale Sentinel Pod that reported once and then disappeared (new IP on restart).
	rec.RecordSentinelCheck(namespace, resource, indicator, instance, STATUS_HEALTHY)

	mutex.Lock()
	assert.Len(t, checkMetricLastUpdated, 1, "recording a check should register it for per-instance GC tracking")
	for k, v := range checkMetricLastUpdated {
		v.lastSeen = time.Now().Add(-2 * metricsGCIntervalMinutes * time.Minute)
		checkMetricLastUpdated[k] = v
	}
	// Simulate the resource still being actively reconciled: its own tracker stays fresh even
	// though the specific Pod IP above is long gone.
	resourceMetricLastUpdated[namespace+"/redisfailover/"+resource] = time.Now()
	mutex.Unlock()

	stale := getStaleCheckMetrics()
	if assert.Len(t, stale, 1, "the stale per-instance series should be found regardless of the resource's own freshness") {
		assert.Equal(t, "sentinel", stale[0].kind)
		assert.Equal(t, namespace, stale[0].namespace)
		assert.Equal(t, resource, stale[0].resource)
		assert.Equal(t, indicator, stale[0].indicator)
		assert.Equal(t, instance, stale[0].instance)
	}

	mutex.Lock()
	assert.Empty(t, checkMetricLastUpdated, "the stale entry should be removed from the tracker once reported")
	mutex.Unlock()

	deleted := rec.sentinelCheck.DeleteLabelValues(namespace, resource, indicator, instance, STATUS_HEALTHY)
	assert.True(t, deleted, "the stale series should still be present in the vector so it can be deleted")

	// A second call with nothing newly stale should find nothing to report.
	assert.Empty(t, getStaleCheckMetrics())
}

// TestRemoveStaleMetricsDeletesStaleCheckInstances exercises the background GC loop end to end
// (like TestRemoveStaleMetrics does for the other trackers) to prove removeStaleMetrics actually
// deletes stale per-instance redisCheck/sentinelCheck series from the live Prometheus registry -
// for both the "redis" and "sentinel" kind branches - rather than just relying on
// getStaleCheckMetrics finding them.
func TestRemoveStaleMetricsDeletesStaleCheckInstances(t *testing.T) {
	reg := prometheus.NewRegistry()
	rec := NewRecorder("stale_check_gc_test", reg).(recorder)

	const (
		namespace = "ns4"
		resource  = "rf4"
	)

	rec.RecordRedisCheck(namespace, resource, REDIS_REPLICA_MISMATCH, "10.0.0.30", STATUS_UNHEALTHY)
	rec.RecordSentinelCheck(namespace, resource, SENTINEL_NOT_READY, "10.0.0.31", STATUS_HEALTHY)

	// Sanity check the metrics are present before GC runs.
	assert.Equal(t, float64(1), testutil.ToFloat64(rec.redisCheck.WithLabelValues(namespace, resource, REDIS_REPLICA_MISMATCH, "10.0.0.30", STATUS_UNHEALTHY)))
	assert.Equal(t, float64(1), testutil.ToFloat64(rec.sentinelCheck.WithLabelValues(namespace, resource, SENTINEL_NOT_READY, "10.0.0.31", STATUS_HEALTHY)))

	old := time.Now().Add(-2 * metricsGCIntervalMinutes * time.Minute)
	mutex.Lock()
	for k, v := range checkMetricLastUpdated {
		v.lastSeen = old
		checkMetricLastUpdated[k] = v
	}
	// Keep the owning resource "fresh", as an actively reconciled RedisFailover would be, so the
	// only thing that can explain the series disappearing is the dedicated per-instance sweep.
	resourceMetricLastUpdated[namespace+"/redisfailover/"+resource] = time.Now()
	mutex.Unlock()

	go removeStaleMetrics()

	assert.Eventually(t, func() bool {
		// WithLabelValues recreates the series with value 0 if it was deleted, so a value of 0
		// here indicates the GC pass ran and removed it.
		return testutil.ToFloat64(rec.redisCheck.WithLabelValues(namespace, resource, REDIS_REPLICA_MISMATCH, "10.0.0.30", STATUS_UNHEALTHY)) == 0 &&
			testutil.ToFloat64(rec.sentinelCheck.WithLabelValues(namespace, resource, SENTINEL_NOT_READY, "10.0.0.31", STATUS_HEALTHY)) == 0
	}, 3*time.Second, 50*time.Millisecond, "expected removeStaleMetrics to delete the aged out per-instance check series")
}
