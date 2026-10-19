package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/stretchr/testify/assert"
)

func TestPrometheusMetrics(t *testing.T) {

	tests := []struct {
		name       string
		addMetrics func(rec Recorder)
		expMetrics []string
		expCode    int
	}{
		{
			name: "Setting OK should give an OK",
			addMetrics: func(rec Recorder) {
				rec.SetClusterOK("testns", "test")
			},
			expMetrics: []string{
				`my_metrics_controller_cluster_ok{name="test",namespace="testns"} 1`,
			},
			expCode: http.StatusOK,
		},
		{
			name: "Setting Error should give an Error",
			addMetrics: func(rec Recorder) {
				rec.SetClusterError("testns", "test")
			},
			expMetrics: []string{
				`my_metrics_controller_cluster_ok{name="test",namespace="testns"} 0`,
			},
			expCode: http.StatusOK,
		},
		{
			name: "Setting Error after ok should give an Error",
			addMetrics: func(rec Recorder) {
				rec.SetClusterOK("testns", "test")
				rec.SetClusterError("testns", "test")
			},
			expMetrics: []string{
				`my_metrics_controller_cluster_ok{name="test",namespace="testns"} 0`,
			},
			expCode: http.StatusOK,
		},
		{
			name: "Setting OK after Error should give an OK",
			addMetrics: func(rec Recorder) {
				rec.SetClusterError("testns", "test")
				rec.SetClusterOK("testns", "test")
			},
			expMetrics: []string{
				`my_metrics_controller_cluster_ok{name="test",namespace="testns"} 1`,
			},
			expCode: http.StatusOK,
		},
		{
			name: "Multiple clusters should appear",
			addMetrics: func(rec Recorder) {
				rec.SetClusterOK("testns", "test")
				rec.SetClusterOK("testns", "test2")
			},
			expMetrics: []string{
				`my_metrics_controller_cluster_ok{name="test",namespace="testns"} 1`,
				`my_metrics_controller_cluster_ok{name="test2",namespace="testns"} 1`,
			},
			expCode: http.StatusOK,
		},
		{
			name: "Same name on different namespaces should appear",
			addMetrics: func(rec Recorder) {
				rec.SetClusterOK("testns1", "test")
				rec.SetClusterOK("testns2", "test")
			},
			expMetrics: []string{
				`my_metrics_controller_cluster_ok{name="test",namespace="testns1"} 1`,
				`my_metrics_controller_cluster_ok{name="test",namespace="testns2"} 1`,
			},
			expCode: http.StatusOK,
		},
		{
			name: "Deleting a cluster should remove it",
			addMetrics: func(rec Recorder) {
				rec.SetClusterOK("testns1", "test")
				rec.DeleteCluster("testns1", "test")
			},
			expMetrics: []string{},
			expCode:    http.StatusOK,
		},
		{
			name: "Deleting a cluster should remove only the desired one",
			addMetrics: func(rec Recorder) {
				rec.SetClusterOK("testns1", "test")
				rec.SetClusterOK("testns2", "test")
				rec.DeleteCluster("testns1", "test")
			},
			expMetrics: []string{
				`my_metrics_controller_cluster_ok{name="test",namespace="testns2"} 1`,
			},
			expCode: http.StatusOK,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)

			// Create the muxer for testing.
			reg := prometheus.NewRegistry()
			rec := NewRecorder("my_metrics", reg)

			// Add metrics to prometheus.
			test.addMetrics(rec)

			// Make the request to the metrics.
			h := promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", http.NoBody))

			resp := w.Result()
			if assert.Equal(test.expCode, resp.StatusCode) {
				body, _ := io.ReadAll(resp.Body)
				// Check all the metrics are present.
				for _, expMetric := range test.expMetrics {
					assert.Contains(string(body), expMetric)
				}
			}
		})
	}
}

// TestRecorderRecordMethods exercises every Record* method of the real
// Recorder implementation and asserts the underlying prometheus metric was
// actually mutated (and not just that the call did not panic).
func TestRecorderRecordMethods(t *testing.T) {
	reg := prometheus.NewRegistry()
	rec := NewRecorder("record_test", reg)
	concrete, ok := rec.(recorder)
	if !ok {
		t.Fatalf("expected NewRecorder to return a recorder, got %T", rec)
	}

	t.Run("RecordEnsureOperation", func(t *testing.T) {
		rec.RecordEnsureOperation("ns", "obj", KIND_REDIS, "res", SUCCESS)
		val := testutil.ToFloat64(concrete.ensureResource.WithLabelValues("ns", "obj", KIND_REDIS, "res", SUCCESS))
		assert.InDelta(t, float64(1), val, 0)
	})

	t.Run("RecordRedisCheck", func(t *testing.T) {
		rec.RecordRedisCheck("ns", "res", REDIS_REPLICA_MISMATCH, "inst", FAIL)
		val := testutil.ToFloat64(concrete.redisCheck.WithLabelValues("ns", "res", REDIS_REPLICA_MISMATCH, "inst", FAIL))
		assert.InDelta(t, float64(1), val, 0)
	})

	t.Run("RecordSentinelCheck", func(t *testing.T) {
		rec.RecordSentinelCheck("ns", "res", SENTINEL_NOT_READY, "inst", FAIL)
		val := testutil.ToFloat64(concrete.sentinelCheck.WithLabelValues("ns", "res", SENTINEL_NOT_READY, "inst", FAIL))
		assert.InDelta(t, float64(1), val, 0)
	})

	t.Run("RecordK8sOperation", func(t *testing.T) {
		rec.RecordK8sOperation("ns", "Pod", "name", "create", SUCCESS, "")
		val := testutil.ToFloat64(concrete.k8sServiceOperations.WithLabelValues("ns", "Pod", "name", "create", SUCCESS, ""))
		assert.InDelta(t, float64(1), val, 0)
	})

	t.Run("RecordRedisOperation", func(t *testing.T) {
		rec.RecordRedisOperation("redis", "10.0.0.5", GET_SLAVE_OF, SUCCESS, "")
		val := testutil.ToFloat64(concrete.redisOperations.WithLabelValues("redis", NOT_APPLICABLE, "10.0.0.5", GET_SLAVE_OF, SUCCESS, ""))
		assert.InDelta(t, float64(1), val, 0)
	})
}

// TestUpdateTrackers verifies the low level tracker helpers record a fresh
// timestamp for the key they are given.
func TestUpdateTrackers(t *testing.T) {
	before := time.Now()

	updateResourceMetricLastUpdatedTracker("nsX", "kindX", "nameX")
	updateInstanceMetricLastUpdatedTracker(podKey{namespace: "nsX", name: "podX"})

	mutex.Lock()
	resourceTS, resourceOK := resourceMetricLastUpdated["nsX/kindX/nameX"]
	instanceTS, instanceOK := instanceMetricLastUpdated[podKey{namespace: "nsX", name: "podX"}]
	mutex.Unlock()

	assert.True(t, resourceOK, "expected resource tracker entry to be present")
	assert.False(t, resourceTS.Before(before), "expected resource tracker timestamp to be refreshed")

	assert.True(t, instanceOK, "expected instance tracker entry to be present")
	assert.False(t, instanceTS.Before(before), "expected instance tracker timestamp to be refreshed")
}

// TestGetLabelsOfStaleMetrics verifies that entries older than the GC
// interval are reported as stale and removed from the tracking maps, while
// fresh entries are left untouched.
func TestGetLabelsOfStaleMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	rec := NewRecorder("stale_labels_test", reg)

	rec.RecordEnsureOperation("ns1", "obj1", KIND_REDIS, "res1", SUCCESS)
	rec.RecordRedisCheck("ns1", "res1", REDIS_REPLICA_MISMATCH, "inst1", FAIL)
	rec.RecordRedisOperation("redis", "10.0.0.10", GET_SLAVE_OF, SUCCESS, "")

	// A fresh entry that should NOT be reported as stale.
	rec.RecordEnsureOperation("ns2", "obj2", KIND_REDIS, "res2", SUCCESS)

	staleResourceKey := "ns1/" + KIND_REDIS + "/obj1"
	staleCheckKey := "ns1/redisfailover/res1"
	freshResourceKey := "ns2/" + KIND_REDIS + "/obj2"

	// Age out the entries we want reported as stale.
	old := time.Now().Add(-2 * metricsGCIntervalMinutes * time.Minute)
	mutex.Lock()
	resourceMetricLastUpdated[staleResourceKey] = old
	resourceMetricLastUpdated[staleCheckKey] = old
	instanceMetricLastUpdated[podKey{namespace: NOT_APPLICABLE, name: "10.0.0.10"}] = old
	mutex.Unlock()

	kubeLabels, customLabels, podLabels := getLabelsOfStaleMetrics()

	assert.Contains(t, kubeLabels, prometheus.Labels{"namespace": "ns1", "kind": KIND_REDIS, "name": "obj1"})
	assert.Contains(t, customLabels, prometheus.Labels{"namespace": "ns1", "resource": "obj1"})
	assert.Contains(t, kubeLabels, prometheus.Labels{"namespace": "ns1", "kind": "redisfailover", "name": "res1"})
	assert.Contains(t, podLabels, prometheus.Labels{"namespace": NOT_APPLICABLE, "pod_name": "10.0.0.10"})

	// The fresh entry must not show up as stale.
	for _, l := range kubeLabels {
		assert.NotEqual(t, prometheus.Labels{"namespace": "ns2", "kind": KIND_REDIS, "name": "obj2"}, l)
	}

	// Stale entries are removed from the tracking maps once reported.
	mutex.Lock()
	_, stillTrackedResource := resourceMetricLastUpdated[staleResourceKey]
	_, stillTrackedCheck := resourceMetricLastUpdated[staleCheckKey]
	_, stillTrackedInstance := instanceMetricLastUpdated[podKey{namespace: NOT_APPLICABLE, name: "10.0.0.10"}]
	_, freshStillTracked := resourceMetricLastUpdated[freshResourceKey]
	mutex.Unlock()

	assert.False(t, stillTrackedResource, "stale resource entry should have been removed from the tracker")
	assert.False(t, stillTrackedCheck, "stale check entry should have been removed from the tracker")
	assert.False(t, stillTrackedInstance, "stale instance entry should have been removed from the tracker")
	assert.True(t, freshStillTracked, "fresh entry should still be tracked")

	// Clean up so the fresh entry does not leak staleness into other tests.
	mutex.Lock()
	delete(resourceMetricLastUpdated, freshResourceKey)
	mutex.Unlock()
}

// TestRemoveStaleMetrics exercises the background GC loop directly: it ages
// out a tracked entry, runs one iteration of removeStaleMetrics in a
// goroutine (the function loops forever, sleeping between iterations) and
// asserts the corresponding prometheus series was actually deleted from the
// registry.
func TestRemoveStaleMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	rec := NewRecorder("stale_gc_test", reg)
	concrete, ok := rec.(recorder)
	if !ok {
		t.Fatalf("expected NewRecorder to return a recorder, got %T", rec)
	}

	rec.RecordK8sOperation("ns3", "Pod", "obj3", "create", SUCCESS, "")
	rec.RecordRedisOperation("redis", "10.0.0.20", GET_SLAVE_OF, SUCCESS, "")

	// Sanity check the metrics are present before GC runs.
	assert.InDelta(t, float64(1), testutil.ToFloat64(concrete.k8sServiceOperations.WithLabelValues("ns3", "Pod", "obj3", "create", SUCCESS, "")), 0)

	old := time.Now().Add(-2 * metricsGCIntervalMinutes * time.Minute)
	mutex.Lock()
	resourceMetricLastUpdated["ns3/Pod/obj3"] = old
	instanceMetricLastUpdated[podKey{namespace: NOT_APPLICABLE, name: "10.0.0.20"}] = old
	mutex.Unlock()

	go removeStaleMetrics()

	assert.Eventually(t, func() bool {
		// WithLabelValues recreates the series with value 0 if it was
		// deleted, so a value of 0 here indicates the GC pass ran and
		// removed it.
		return testutil.ToFloat64(concrete.k8sServiceOperations.WithLabelValues("ns3", "Pod", "obj3", "create", SUCCESS, "")) == 0 &&
			testutil.ToFloat64(concrete.redisOperations.WithLabelValues("redis", NOT_APPLICABLE, "10.0.0.20", GET_SLAVE_OF, SUCCESS, "")) == 0
	}, 3*time.Second, 50*time.Millisecond, "expected removeStaleMetrics to delete the aged out series")
}

// TestDummyRecorder exercises every method of the no-op Dummy recorder used
// as a test double elsewhere in the codebase. None of them are expected to
// do anything observable, so this only asserts they do not panic.
func TestDummyRecorder(t *testing.T) {
	assert.NotPanics(t, func() {
		Dummy.SetClusterOK("ns", "name")
		Dummy.SetClusterError("ns", "name")
		Dummy.DeleteCluster("ns", "name")
		Dummy.RecordEnsureOperation("ns", "obj", KIND_REDIS, "res", SUCCESS)
		Dummy.RecordRedisCheck("ns", "res", REDIS_REPLICA_MISMATCH, "inst", FAIL)
		Dummy.RecordSentinelCheck("ns", "res", SENTINEL_NOT_READY, "inst", FAIL)
		Dummy.RecordK8sOperation("ns", "Pod", "name", "create", SUCCESS, "")
		Dummy.RecordRedisOperation("redis", "10.0.0.5", GET_SLAVE_OF, SUCCESS, "")
		Dummy.SetPodIP(KIND_REDIS, "ns", "pod", "10.0.0.5")
		Dummy.SetAutoscaler("ns", "name", 1, 0.5, 2, 1)
		Dummy.SetAutoscalerPhase("ns", "name", 1)
		Dummy.CountAutoscalerStep("ns", "name", AutoscalerStepDone)
		Dummy.DeleteAutoscaler("ns", "name")
		Dummy.SetSentinelUnknownAddrs("ns", "name", 1, true)
	})
}

func TestSentinelUnknownAddrsMetrics(t *testing.T) {
	rec := NewRecorder("unknown_test", prometheus.NewRegistry()).(recorder)
	rec.SetSentinelUnknownAddrs("ns", "a", 2, true)
	rec.SetSentinelUnknownAddrs("ns", "b", 0, false)

	assert.InDelta(t, 2.0, testutil.ToFloat64(rec.unknownPeers.WithLabelValues("ns", "a")), 0)
	assert.InDelta(t, 1.0, testutil.ToFloat64(rec.unknownMaster.WithLabelValues("ns", "a")), 0)
	assert.InDelta(t, 0.0, testutil.ToFloat64(rec.unknownPeers.WithLabelValues("ns", "b")), 0)
	assert.InDelta(t, 0.0, testutil.ToFloat64(rec.unknownMaster.WithLabelValues("ns", "b")), 0)

	rec.DeleteCluster("ns", "a")
	assert.Equal(t, 1, testutil.CollectAndCount(rec.unknownPeers))
	assert.Equal(t, 1, testutil.CollectAndCount(rec.unknownMaster))
}

func TestAutoscalerMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	rec := NewRecorder("autoscaler_test", reg).(recorder)
	rec.SetAutoscaler("ns", "a", 1342177280, 0.912, 2, 1073741824)
	rec.SetAutoscaler("ns", "b", 0, 0.5, 0, 1)
	rec.SetAutoscaler("ns", "c", 0, 0.5, 0, 1)
	for _, name := range []string{"a", "b", "c"} {
		rec.SetAutoscalerPhase("ns", name, 4)
		for _, result := range []string{AutoscalerStepDone, AutoscalerStepReverted, AutoscalerStepHeld} {
			rec.CountAutoscalerStep("ns", name, result)
		}
	}
	rec.CountAutoscalerStep("ns", "a", AutoscalerStepDone)

	assert.InDelta(t, 1342177280.0, testutil.ToFloat64(rec.recommendedMemory.WithLabelValues("ns", "a")), 0)
	assert.InDelta(t, 0.912, testutil.ToFloat64(rec.memoryUsedRatio.WithLabelValues("ns", "a")), 0)
	assert.InDelta(t, 2.0, testutil.ToFloat64(rec.highWindows.WithLabelValues("ns", "a")), 0)
	assert.InDelta(t, 1073741824.0, testutil.ToFloat64(rec.effectiveMemory.WithLabelValues("ns", "a")), 0)
	assert.InDelta(t, 4.0, testutil.ToFloat64(rec.stepPhase.WithLabelValues("ns", "a")), 0)
	assert.InDelta(t, 2.0, testutil.ToFloat64(rec.steps.WithLabelValues("ns", "a", AutoscalerStepDone)), 0)

	// DeleteAutoscaler is for a mode change to Off, and DeleteCluster for a
	// deleted RedisFailover.
	rec.DeleteAutoscaler("ns", "b")
	rec.SetClusterOK("ns", "c")
	rec.DeleteCluster("ns", "c")
	for _, gauge := range []*prometheus.GaugeVec{rec.recommendedMemory, rec.memoryUsedRatio, rec.highWindows, rec.effectiveMemory, rec.stepPhase} {
		assert.Equal(t, 1, testutil.CollectAndCount(gauge))
	}
	assert.Equal(t, 3, testutil.CollectAndCount(rec.steps), "the three results of a")
	assert.Zero(t, testutil.CollectAndCount(rec.clusterOK))
}
