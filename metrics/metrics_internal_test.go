package metrics

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
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
