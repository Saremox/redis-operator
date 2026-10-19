package metrics

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	resetTrackers(t)

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
	assert.InDelta(t, float64(1), testutil.ToFloat64(rec.redisCheck.WithLabelValues(namespace, resource, REDIS_REPLICA_MISMATCH, "10.0.0.30", STATUS_UNHEALTHY)), 0)
	assert.InDelta(t, float64(1), testutil.ToFloat64(rec.sentinelCheck.WithLabelValues(namespace, resource, SENTINEL_NOT_READY, "10.0.0.31", STATUS_HEALTHY)), 0)

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

// resetTrackers empties the GC trackers for one test and puts the old ones
// back when the test ends, because they are package state that the tests
// share and count.
func resetTrackers(t *testing.T) {
	t.Helper()
	mutex.Lock()
	resources, checks := resourceMetricLastUpdated, checkMetricLastUpdated
	resourceMetricLastUpdated = map[string]time.Time{}
	checkMetricLastUpdated = map[string]checkMetricInfo{}
	mutex.Unlock()
	t.Cleanup(func() {
		mutex.Lock()
		resourceMetricLastUpdated, checkMetricLastUpdated = resources, checks
		mutex.Unlock()
	})
}

// An instance can have a HEALTHY and an UNHEALTHY series. The tracker keeps
// one entry for both, so one GC pass must delete both series.
func TestRemoveStaleMetricsDeletesBothStatusesOfAnInstance(t *testing.T) {
	resetTrackers(t)
	reg := prometheus.NewRegistry()
	rec := NewRecorder("stale_check_status_test", reg).(recorder)

	const (
		namespace = "ns5"
		resource  = "rf5"
	)

	for _, status := range []string{STATUS_HEALTHY, STATUS_UNHEALTHY} {
		rec.RecordRedisCheck(namespace, resource, REDIS_REPLICA_MISMATCH, "10.0.0.40", status)
		rec.RecordSentinelCheck(namespace, resource, SENTINEL_NOT_READY, "10.0.0.41", status)
	}
	assert.Equal(t, 2, testutil.CollectAndCount(rec.redisCheck))
	assert.Equal(t, 2, testutil.CollectAndCount(rec.sentinelCheck))

	old := time.Now().Add(-2 * metricsGCIntervalMinutes * time.Minute)
	mutex.Lock()
	for k, v := range checkMetricLastUpdated {
		v.lastSeen = old
		checkMetricLastUpdated[k] = v
	}
	resourceMetricLastUpdated[namespace+"/redisfailover/"+resource] = time.Now()
	mutex.Unlock()

	go removeStaleMetrics()

	assert.Eventually(t, func() bool {
		return testutil.CollectAndCount(rec.redisCheck) == 0 && testutil.CollectAndCount(rec.sentinelCheck) == 0
	}, 3*time.Second, 50*time.Millisecond, "expected removeStaleMetrics to delete the HEALTHY and the UNHEALTHY series")
}

// The GC uses the labels of a stale RedisFailover for each recorder, so the
// first recorder must not change them for the next one.
func TestRemoveStaleMetricsDeletesStaleResourceInEachRecorder(t *testing.T) {
	resetTrackers(t)
	const (
		namespace = "ns6"
		resource  = "rf6"
	)
	recs := []recorder{
		NewRecorder("stale_resource_test_a", prometheus.NewRegistry()).(recorder),
		NewRecorder("stale_resource_test_b", prometheus.NewRegistry()).(recorder),
	}
	for _, rec := range recs {
		rec.RecordRedisCheck(namespace, resource, REDIS_REPLICA_MISMATCH, "10.0.0.50", STATUS_UNHEALTHY)
		rec.RecordSentinelCheck(namespace, resource, SENTINEL_NOT_READY, "10.0.0.51", STATUS_UNHEALTHY)
		rec.SetClusterError(namespace, resource)
		rec.SetAutoscaler(namespace, resource, 1, 0.5, 1, 1)
		rec.SetAutoscalerPhase(namespace, resource, 1)
		rec.CountAutoscalerStep(namespace, resource, AutoscalerStepHeld)
		rec.SetSentinelUnknownAddrs(namespace, resource, 1, true)
	}

	mutex.Lock()
	resourceMetricLastUpdated[namespace+"/redisfailover/"+resource] = time.Now().Add(-2 * metricsGCIntervalMinutes * time.Minute)
	mutex.Unlock()

	go removeStaleMetrics()

	assert.Eventually(t, func() bool {
		for _, rec := range recs {
			if testutil.CollectAndCount(rec.redisCheck)+testutil.CollectAndCount(rec.sentinelCheck)+testutil.CollectAndCount(rec.clusterOK)+
				testutil.CollectAndCount(rec.recommendedMemory)+testutil.CollectAndCount(rec.memoryUsedRatio)+testutil.CollectAndCount(rec.highWindows)+
				testutil.CollectAndCount(rec.effectiveMemory)+testutil.CollectAndCount(rec.stepPhase)+testutil.CollectAndCount(rec.steps)+
				testutil.CollectAndCount(rec.unknownPeers)+testutil.CollectAndCount(rec.unknownMaster) != 0 {
				return false
			}
		}
		return true
	}, 3*time.Second, 50*time.Millisecond, "expected removeStaleMetrics to delete the series of the stale RedisFailover in each recorder")
}

// resetPods empties the IP map and the redisOperations tracker for one test.
func resetPods(t *testing.T) {
	t.Helper()
	mutex.Lock()
	pods, instances := podsByIP, instanceMetricLastUpdated
	podsByIP = map[podIPKey]map[podKey]time.Time{}
	instanceMetricLastUpdated = map[podKey]time.Time{}
	mutex.Unlock()
	t.Cleanup(func() {
		mutex.Lock()
		podsByIP, instanceMetricLastUpdated = pods, instances
		mutex.Unlock()
	})
}

// agePod makes the pod of an IP stale for the GC. It does not fail with the
// mutex held, because the cleanup of resetPods needs the mutex.
func agePod(kind, ip string, pod podKey) {
	mutex.Lock()
	defer mutex.Unlock()
	if pods := podsByIP[podIPKey{kind: kind, ip: ip}]; pods != nil {
		pods[pod] = time.Now().Add(-2 * metricsGCIntervalMinutes * time.Minute)
	}
}

// A pod that Kubernetes creates again gets a different IP and keeps its name. The
// series of the pod must not change.
func TestPodSeriesKeepTheNameAcrossIPs(t *testing.T) {
	resetTrackers(t)
	resetPods(t)
	rec := NewRecorder("pod_name_test", prometheus.NewRegistry()).(recorder)

	for _, ip := range []string{"10.1.0.1", "10.1.0.2"} {
		rec.SetPodIP(KIND_REDIS, "ns7", "rfr-rf7-0", ip)
		rec.SetPodIP(KIND_SENTINEL, "ns7", "rfs-rf7-0", ip)
		rec.RecordRedisOperation(KIND_REDIS, ip, IS_MASTER, SUCCESS, NOT_APPLICABLE)
		rec.RecordRedisCheck("ns7", "rf7", SLAVE_WRONG_MASTER, ip, STATUS_HEALTHY)
		rec.RecordSentinelCheck("ns7", "rf7", SENTINEL_WRONG_MASTER, ip, STATUS_HEALTHY)
	}

	assert.Equal(t, 1, testutil.CollectAndCount(rec.redisOperations))
	assert.InDelta(t, 2, testutil.ToFloat64(rec.redisOperations.WithLabelValues(KIND_REDIS, "ns7", "rfr-rf7-0", IS_MASTER, SUCCESS, NOT_APPLICABLE)), 0)
	assert.Equal(t, 1, testutil.CollectAndCount(rec.redisCheck))
	assert.InDelta(t, 2, testutil.ToFloat64(rec.redisCheck.WithLabelValues("ns7", "rf7", SLAVE_WRONG_MASTER, "rfr-rf7-0", STATUS_HEALTHY)), 0)
	assert.Equal(t, 1, testutil.CollectAndCount(rec.sentinelCheck))
	assert.InDelta(t, 2, testutil.ToFloat64(rec.sentinelCheck.WithLabelValues("ns7", "rf7", SENTINEL_WRONG_MASTER, "rfs-rf7-0", STATUS_HEALTHY)), 0)

	// The label name pod_name does not collide with the target labels pod and
	// instance.
	podName := prometheus.Labels{"pod_name": "rfr-rf7-0"}
	assert.Equal(t, 1, rec.redisOperations.DeletePartialMatch(podName))
	assert.Equal(t, 1, rec.redisCheck.DeletePartialMatch(podName))
	assert.Equal(t, 1, rec.sentinelCheck.DeletePartialMatch(prometheus.Labels{"pod_name": "rfs-rf7-0"}))
}

// Two RedisFailovers with the same name in two namespaces have pods with the
// same name. Their series must stay apart.
func TestRedisOperationSeriesOfPodsInTwoNamespaces(t *testing.T) {
	resetPods(t)
	rec := NewRecorder("pod_namespace_test", prometheus.NewRegistry()).(recorder)

	rec.SetPodIP(KIND_REDIS, "ns8a", "rfr-rf8-0", "10.1.0.3")
	rec.SetPodIP(KIND_REDIS, "ns8b", "rfr-rf8-0", "10.1.0.4")
	rec.RecordRedisOperation(KIND_REDIS, "10.1.0.3", IS_MASTER, SUCCESS, NOT_APPLICABLE)
	rec.RecordRedisOperation(KIND_REDIS, "10.1.0.4", IS_MASTER, SUCCESS, NOT_APPLICABLE)

	assert.Equal(t, 2, testutil.CollectAndCount(rec.redisOperations))
	assert.InDelta(t, 1, testutil.ToFloat64(rec.redisOperations.WithLabelValues(KIND_REDIS, "ns8a", "rfr-rf8-0", IS_MASTER, SUCCESS, NOT_APPLICABLE)), 0)
	assert.InDelta(t, 1, testutil.ToFloat64(rec.redisOperations.WithLabelValues(KIND_REDIS, "ns8b", "rfr-rf8-0", IS_MASTER, SUCCESS, NOT_APPLICABLE)), 0)
}

// The checks of the RedisFailover use NOT_APPLICABLE as instance, and an IP
// without SetPodIP has no pod name.
func TestPodSeriesWithoutAPodName(t *testing.T) {
	resetTrackers(t)
	resetPods(t)
	rec := NewRecorder("pod_unknown_test", prometheus.NewRegistry()).(recorder)

	rec.RecordRedisCheck("ns9", "rf9", NO_MASTER, NOT_APPLICABLE, STATUS_HEALTHY)
	rec.RecordSentinelCheck("ns9", "rf9", SENTINEL_WRONG_MASTER, "10.1.0.5", STATUS_HEALTHY)
	rec.RecordRedisOperation(KIND_SENTINEL, "10.1.0.5", GET_SENTINEL_MONITOR, SUCCESS, NOT_APPLICABLE)

	assert.InDelta(t, 1, testutil.ToFloat64(rec.redisCheck.WithLabelValues("ns9", "rf9", NO_MASTER, NOT_APPLICABLE, STATUS_HEALTHY)), 0)
	assert.InDelta(t, 1, testutil.ToFloat64(rec.sentinelCheck.WithLabelValues("ns9", "rf9", SENTINEL_WRONG_MASTER, "10.1.0.5", STATUS_HEALTHY)), 0)
	assert.InDelta(t, 1, testutil.ToFloat64(rec.redisOperations.WithLabelValues(KIND_SENTINEL, NOT_APPLICABLE, "10.1.0.5", GET_SENTINEL_MONITOR, SUCCESS, NOT_APPLICABLE)), 0)
}

// The GC deletes the redisOperations series of a removed pod by its namespace
// and pod name. It forgets an IP that no pod list showed for
// metricsGCIntervalMinutes.
func TestRemoveStaleMetricsDeletesRemovedPod(t *testing.T) {
	resetTrackers(t)
	resetPods(t)
	rec := NewRecorder("stale_pod_test", prometheus.NewRegistry()).(recorder)

	rec.SetPodIP(KIND_REDIS, "ns10", "rfr-rf10-2", "10.1.0.6")
	rec.SetPodIP(KIND_REDIS, "ns10", "rfr-rf10-0", "10.1.0.7")
	rec.RecordRedisOperation(KIND_REDIS, "10.1.0.6", IS_MASTER, SUCCESS, NOT_APPLICABLE)
	rec.RecordRedisOperation(KIND_REDIS, "10.1.0.7", IS_MASTER, SUCCESS, NOT_APPLICABLE)

	old := time.Now().Add(-2 * metricsGCIntervalMinutes * time.Minute)
	agePod(KIND_REDIS, "10.1.0.6", podKey{namespace: "ns10", name: "rfr-rf10-2"})
	mutex.Lock()
	instanceMetricLastUpdated[podKey{namespace: "ns10", name: "rfr-rf10-2"}] = old
	mutex.Unlock()

	go removeStaleMetrics()

	assert.Eventually(t, func() bool {
		return testutil.CollectAndCount(rec.redisOperations) == 1
	}, 3*time.Second, 50*time.Millisecond, "expected removeStaleMetrics to delete the series of the removed pod")
	assert.InDelta(t, 1, testutil.ToFloat64(rec.redisOperations.WithLabelValues(KIND_REDIS, "ns10", "rfr-rf10-0", IS_MASTER, SUCCESS, NOT_APPLICABLE)), 0)
	mutex.Lock()
	_, staleIP := podsByIP[podIPKey{kind: KIND_REDIS, ip: "10.1.0.6"}]
	_, freshIP := podsByIP[podIPKey{kind: KIND_REDIS, ip: "10.1.0.7"}]
	mutex.Unlock()
	assert.False(t, staleIP, "expected the GC to forget the IP of the removed pod")
	assert.True(t, freshIP, "expected the GC to keep the IP of a listed pod")
}

// With the host network, two redis pods of two RedisFailovers on one node have
// the IP of the node. The series must not carry the name of the wrong pod.
func TestPodSeriesOfAnIPOfTwoPods(t *testing.T) {
	resetTrackers(t)
	resetPods(t)
	rec := NewRecorder("pod_shared_ip_test", prometheus.NewRegistry()).(recorder)

	rec.SetPodIP(KIND_REDIS, "ns11a", "rfr-rf11-0", "10.1.0.8")
	rec.SetPodIP(KIND_REDIS, "ns11b", "rfr-rf11-0", "10.1.0.8")
	rec.RecordRedisOperation(KIND_REDIS, "10.1.0.8", IS_MASTER, SUCCESS, NOT_APPLICABLE)
	rec.RecordRedisCheck("ns11a", "rf11", SLAVE_WRONG_MASTER, "10.1.0.8", STATUS_HEALTHY)

	assert.InDelta(t, 1, testutil.ToFloat64(rec.redisOperations.WithLabelValues(KIND_REDIS, NOT_APPLICABLE, "10.1.0.8", IS_MASTER, SUCCESS, NOT_APPLICABLE)), 0)
	assert.Equal(t, 1, testutil.CollectAndCount(rec.redisOperations))
	assert.InDelta(t, 1, testutil.ToFloat64(rec.redisCheck.WithLabelValues("ns11a", "rf11", SLAVE_WRONG_MASTER, "10.1.0.8", STATUS_HEALTHY)), 0)
	assert.Equal(t, 1, testutil.CollectAndCount(rec.redisCheck))
}

// With the host network, a redis pod and a sentinel pod on one node have the
// IP of the node. The kind of the command tells the pods apart.
func TestPodSeriesOfARedisAndASentinelPodWithOneIP(t *testing.T) {
	resetTrackers(t)
	resetPods(t)
	rec := NewRecorder("pod_kind_ip_test", prometheus.NewRegistry()).(recorder)

	rec.SetPodIP(KIND_REDIS, "ns12", "rfr-rf12-0", "10.1.0.9")
	rec.SetPodIP(KIND_SENTINEL, "ns12", "rfs-rf12-abc-1", "10.1.0.9")
	rec.RecordRedisOperation(KIND_REDIS, "10.1.0.9", IS_MASTER, SUCCESS, NOT_APPLICABLE)
	rec.RecordRedisOperation(KIND_SENTINEL, "10.1.0.9", GET_SENTINEL_MONITOR, SUCCESS, NOT_APPLICABLE)
	rec.RecordRedisCheck("ns12", "rf12", SLAVE_WRONG_MASTER, "10.1.0.9", STATUS_HEALTHY)
	rec.RecordSentinelCheck("ns12", "rf12", SENTINEL_WRONG_MASTER, "10.1.0.9", STATUS_HEALTHY)

	assert.InDelta(t, 1, testutil.ToFloat64(rec.redisOperations.WithLabelValues(KIND_REDIS, "ns12", "rfr-rf12-0", IS_MASTER, SUCCESS, NOT_APPLICABLE)), 0)
	assert.InDelta(t, 1, testutil.ToFloat64(rec.redisOperations.WithLabelValues(KIND_SENTINEL, "ns12", "rfs-rf12-abc-1", GET_SENTINEL_MONITOR, SUCCESS, NOT_APPLICABLE)), 0)
	assert.Equal(t, 2, testutil.CollectAndCount(rec.redisOperations))
	assert.InDelta(t, 1, testutil.ToFloat64(rec.redisCheck.WithLabelValues("ns12", "rf12", SLAVE_WRONG_MASTER, "rfr-rf12-0", STATUS_HEALTHY)), 0)
	assert.InDelta(t, 1, testutil.ToFloat64(rec.sentinelCheck.WithLabelValues("ns12", "rf12", SENTINEL_WRONG_MASTER, "rfs-rf12-abc-1", STATUS_HEALTHY)), 0)
}

// When the pod lists show only one of the two pods of an IP, the GC forgets
// the other pod, and the series carry the pod name again.
func TestPodIPOfTwoPodsGetsANameWhenOnePodIsNotListed(t *testing.T) {
	resetTrackers(t)
	resetPods(t)

	rec := NewRecorder("pod_shared_ip_gc_test", prometheus.NewRegistry()).(recorder)
	rec.SetPodIP(KIND_REDIS, "ns13a", "rfr-rf13-0", "10.1.0.10")
	rec.SetPodIP(KIND_REDIS, "ns13b", "rfr-rf13-0", "10.1.0.10")
	require.Equal(t, podKey{namespace: NOT_APPLICABLE, name: "10.1.0.10"}, podOf(KIND_REDIS, "10.1.0.10"))

	agePod(KIND_REDIS, "10.1.0.10", podKey{namespace: "ns13a", name: "rfr-rf13-0"})
	getLabelsOfStaleMetrics()

	assert.Equal(t, podKey{namespace: "ns13b", name: "rfr-rf13-0"}, podOf(KIND_REDIS, "10.1.0.10"))
}
