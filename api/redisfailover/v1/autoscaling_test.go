package v1

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func rfWithAutoscaling(limit, request string, a *AutoscalingSettings) *RedisFailover {
	rf := rfWithMemoryLimit(limit, nil)
	if request != "" {
		rf.Spec.Redis.Resources.Requests = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(request)}
	}
	rf.Spec.Redis.Autoscaling = a
	return rf
}

func quantity(s string) *resource.Quantity {
	q := resource.MustParse(s)
	return &q
}

func TestValidateAutoscaling(t *testing.T) {
	recommend := func(memoryMax string) AutoscalingSettings {
		a := AutoscalingSettings{Mode: AutoscalingModeRecommend}
		if memoryMax != "" {
			a.MemoryMax = quantity(memoryMax)
		}
		return a
	}
	withRecreate := func(a AutoscalingSettings, d time.Duration) AutoscalingSettings {
		a.RecreateAfter = &metav1.Duration{Duration: d}
		return a
	}
	withWorkload := func(a AutoscalingSettings, w string) AutoscalingSettings {
		a.Workload = w
		return a
	}
	tests := []struct {
		name          string
		settings      AutoscalingSettings
		limit         string
		request       string
		customConfig  []string
		maxMemory     *MaxMemorySettings
		expectedError string
	}{
		{name: "defaults to Off without other fields"},
		{name: "Off ignores the memory rules", settings: AutoscalingSettings{Mode: AutoscalingModeOff}},
		{name: "Recommend", settings: recommend("2Gi"), limit: "1Gi", request: "1Gi"},
		{name: "Recommend without a request", settings: recommend("2Gi"), limit: "1Gi"},
		{name: "Auto is refused", settings: AutoscalingSettings{Mode: AutoscalingModeAuto, MemoryMax: quantity("2Gi")}, limit: "1Gi", expectedError: "redis.autoscaling.mode Auto is not available, use Off or Recommend"},
		{name: "unknown mode", settings: AutoscalingSettings{Mode: "On"}, expectedError: `redis.autoscaling.mode "On" must be Off, Recommend or Auto`},
		{name: "unknown workload", settings: AutoscalingSettings{Workload: "Queue"}, expectedError: `redis.autoscaling.workload "Queue" must be Auto, Store or Cache`},
		{name: "recreateAfter 0s", settings: withRecreate(recommend("2Gi"), 0), limit: "1Gi"},
		{name: "recreateAfter 5m", settings: withRecreate(recommend("2Gi"), 5*time.Minute), limit: "1Gi"},
		{name: "recreateAfter below 5m", settings: withRecreate(AutoscalingSettings{}, 4*time.Minute), expectedError: "redis.autoscaling.recreateAfter 4m0s must be 0s or at least 5m0s"},
		{name: "recreateAfter negative", settings: withRecreate(AutoscalingSettings{}, -time.Minute), expectedError: "redis.autoscaling.recreateAfter -1m0s must be 0s or at least 5m0s"},
		{name: "no memory limit", settings: recommend("2Gi"), expectedError: "redis.autoscaling requires redis.resources.limits.memory"},
		{name: "no memoryMax", settings: recommend(""), limit: "1Gi", expectedError: "redis.autoscaling.memoryMax is required for mode Recommend"},
		{name: "memoryMax equal to the limit", settings: recommend("1Gi"), limit: "1Gi"},
		{name: "memoryMax below the limit", settings: recommend("1023Mi"), limit: "1Gi", expectedError: "redis.autoscaling.memoryMax 1023Mi must not be less than redis.resources.limits.memory 1Gi"},
		{name: "store with a smaller request", settings: recommend("2Gi"), limit: "1Gi", request: "512Mi", expectedError: "redis.autoscaling requires redis.resources.requests.memory 512Mi equal to the limit 1Gi for a store"},
		{name: "volatile policy is a store", settings: recommend("2Gi"), limit: "1Gi", request: "512Mi", maxMemory: &MaxMemorySettings{Policy: "volatile-lru"}, expectedError: "for a store"},
		{name: "explicit store with allkeys policy", settings: withWorkload(recommend("2Gi"), AutoscalingWorkloadStore), limit: "1Gi", request: "512Mi", maxMemory: &MaxMemorySettings{Policy: "allkeys-lru"}, expectedError: "for a store"},
		{name: "cache workload with a smaller request", settings: withWorkload(recommend("2Gi"), AutoscalingWorkloadCache), limit: "1Gi", request: "512Mi"},
		{name: "allkeys policy with a smaller request", settings: recommend("2Gi"), limit: "1Gi", request: "512Mi", maxMemory: &MaxMemorySettings{Policy: "allkeys-lfu"}},
		{name: "allkeys policy in customConfig", settings: recommend("2Gi"), limit: "1Gi", request: "512Mi", customConfig: []string{"maxmemory-policy ALLKEYS-LRU"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			settings := test.settings
			rf := rfWithAutoscaling(test.limit, test.request, &settings)
			rf.Spec.Redis.MaxMemory = test.maxMemory
			rf.Spec.Redis.CustomConfig = test.customConfig
			err := rf.Validate()
			if test.expectedError != "" {
				assert.ErrorContains(t, err, test.expectedError)
				return
			}
			assert.NoError(t, err)
		})
	}
}

func TestValidateAutoscalingDefaults(t *testing.T) {
	rf := rfWithAutoscaling("", "", &AutoscalingSettings{})
	require.NoError(t, rf.Validate())
	assert.Equal(t, AutoscalingSettings{Mode: AutoscalingModeOff, Workload: AutoscalingWorkloadAuto}, *rf.Spec.Redis.Autoscaling)
	assert.False(t, rf.AutoscalingEnabled())

	rf = rfWithAutoscaling("1Gi", "", &AutoscalingSettings{Mode: AutoscalingModeRecommend, MemoryMax: quantity("2Gi")})
	require.NoError(t, rf.Validate())
	assert.True(t, rf.AutoscalingEnabled())

	rf = rfWithAutoscaling("", "", nil)
	require.NoError(t, rf.Validate())
	assert.Nil(t, rf.Spec.Redis.Autoscaling)
	assert.False(t, rf.AutoscalingEnabled())
}

func TestMaxMemoryPolicy(t *testing.T) {
	rf := &RedisFailover{}
	assert.Equal(t, "noeviction", rf.MaxMemoryPolicy())
	rf.Spec.Redis.MaxMemory = &MaxMemorySettings{Policy: "volatile-ttl"}
	assert.Equal(t, "volatile-ttl", rf.MaxMemoryPolicy())
	// A leading space is not an override, as for CustomConfigSets.
	rf.Spec.Redis.CustomConfig = []string{" maxmemory-policy allkeys-lru"}
	assert.Equal(t, "volatile-ttl", rf.MaxMemoryPolicy())
	rf.Spec.Redis.CustomConfig = []string{"MAXMEMORY-POLICY  Allkeys-LFU "}
	assert.Equal(t, "allkeys-lfu", rf.MaxMemoryPolicy())
}

// A status without autoscaling must serialize as before the field existed,
// and the status must stay comparable with ==.
func TestStatusWithoutAutoscaling(t *testing.T) {
	status := RedisFailoverStatus{State: HealthyState, LastChanged: "2026-01-01T00:00:00Z", Message: "m"}
	got, err := json.Marshal(status)
	require.NoError(t, err)
	assert.JSONEq(t, `{"state":"Healthy","lastChanged":"2026-01-01T00:00:00Z","message":"m"}`, string(got))
	assert.NotContains(t, string(got), "autoscaling")

	other := status
	other.Autoscaling.HighWindows = 1
	assert.False(t, status == other)
	other.Autoscaling.HighWindows = 0
	assert.True(t, status == other)

	other.Autoscaling = AutoscalingStatus{Workload: "Store", RecommendedMemory: "1280Mi", Reason: "r", EvaluatedAt: "2026-01-01T00:05:00Z", HighWindows: 3}
	got, err = json.Marshal(other)
	require.NoError(t, err)
	var back RedisFailoverStatus
	require.NoError(t, json.Unmarshal(got, &back))
	assert.Equal(t, other, back)
}
