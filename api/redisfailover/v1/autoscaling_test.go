package v1

import (
	"encoding/json"
	"math/rand/v2"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
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
		bootstrap     bool
		expectedError string
	}{
		{name: "defaults to Off without other fields"},
		{name: "Off ignores the memory rules", settings: AutoscalingSettings{Mode: AutoscalingModeOff}},
		{name: "Recommend", settings: recommend("2Gi"), limit: "1Gi", request: "1Gi"},
		{name: "Recommend without a request", settings: recommend("2Gi"), limit: "1Gi"},
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
		{name: "Recommend with bootstrapNode", settings: recommend("2Gi"), limit: "1Gi", bootstrap: true, expectedError: "redis.autoscaling.mode Recommend is not available with bootstrapNode, because the RedisFailover has no master to sample"},
		{name: "Off with bootstrapNode", settings: AutoscalingSettings{Mode: AutoscalingModeOff}, bootstrap: true},
		{name: "allkeys policy in customConfig", settings: recommend("2Gi"), limit: "1Gi", request: "512Mi", customConfig: []string{"maxmemory-policy ALLKEYS-LRU"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			settings := test.settings
			rf := rfWithAutoscaling(test.limit, test.request, &settings)
			if test.bootstrap {
				rf.Spec.BootstrapNode = &BootstrapSettings{Host: "10.0.0.9"}
			}
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

// autoRF is a RedisFailover that mode Auto accepts.
func autoRF() *RedisFailover {
	rf := rfWithAutoscaling("1Gi", "1Gi", &AutoscalingSettings{Mode: AutoscalingModeAuto, MemoryMax: quantity("4Gi")})
	rf.Spec.Redis.MaxMemory = &MaxMemorySettings{}
	return rf
}

func TestValidateAutoscalingAuto(t *testing.T) {
	tests := map[string]struct {
		change        func(rf *RedisFailover)
		expectedError string
	}{
		"valid":                        {change: func(*RedisFailover) {}},
		"without a request":            {change: func(rf *RedisFailover) { rf.Spec.Redis.Resources.Requests = nil }},
		"memoryMax equal to the limit": {change: func(rf *RedisFailover) { rf.Spec.Redis.Autoscaling.MemoryMax = quantity("1Gi") }},
		"cache workload": {change: func(rf *RedisFailover) {
			rf.Spec.Redis.Autoscaling.Workload = AutoscalingWorkloadCache
		}},
		"inPlaceResize Enabled": {change: func(rf *RedisFailover) { rf.Spec.Redis.InPlaceResize = "Enabled" }},
		"other customConfig": {change: func(rf *RedisFailover) {
			rf.Spec.Redis.CustomConfig = []string{"maxmemory-policy allkeys-lru", "maxmemory-samples 10"}
		}},
		"no maxMemory": {change: func(rf *RedisFailover) { rf.Spec.Redis.MaxMemory = nil },
			expectedError: "redis.autoscaling.mode Auto requires redis.maxMemory"},
		"limit below 64Mi": {change: func(rf *RedisFailover) {
			rf.Spec.Redis.Resources = corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("32Mi")}}
		}, expectedError: "redis.autoscaling.mode Auto requires a managed maxmemory: redis.maxMemory requires redis.resources.limits.memory of at least 64Mi, got 32Mi"},
		"maxmemory in customConfig": {change: func(rf *RedisFailover) { rf.Spec.Redis.CustomConfig = []string{"MAXMEMORY 100mb"} },
			expectedError: "redis.autoscaling.mode Auto requires no maxmemory in redis.customConfig"},
		"inPlaceResize Disabled": {change: func(rf *RedisFailover) { rf.Spec.Redis.InPlaceResize = "Disabled" },
			expectedError: "redis.autoscaling.mode Auto requires redis.inPlaceResize Enabled, because a step resizes the pods in place"},
		"recreateAfter": {change: func(rf *RedisFailover) {
			rf.Spec.Redis.Autoscaling.RecreateAfter = &metav1.Duration{Duration: 10 * time.Minute}
		}, expectedError: "redis.autoscaling.recreateAfter is not available with mode Auto, because a step never recreates a pod"},
		"recreateAfter 0s": {change: func(rf *RedisFailover) { rf.Spec.Redis.Autoscaling.RecreateAfter = &metav1.Duration{} },
			expectedError: "redis.autoscaling.recreateAfter is not available with mode Auto"},
		"request below the limit of a cache": {change: func(rf *RedisFailover) {
			rf.Spec.Redis.Autoscaling.Workload = AutoscalingWorkloadCache
			rf.Spec.Redis.Resources.Requests = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")}
		}, expectedError: "redis.autoscaling.mode Auto requires redis.resources.requests.memory 512Mi equal to the limit 1Gi"},
		"request above the limit": {change: func(rf *RedisFailover) {
			rf.Spec.Redis.Resources.Requests = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")}
		}, expectedError: "redis.autoscaling.mode Auto requires redis.resources.requests.memory 2Gi equal to the limit 1Gi"},
		// The rules of each mode stay.
		"no memoryMax": {change: func(rf *RedisFailover) { rf.Spec.Redis.Autoscaling.MemoryMax = nil },
			expectedError: "redis.autoscaling.memoryMax is required for mode Auto"},
		"memoryMax below the limit": {change: func(rf *RedisFailover) { rf.Spec.Redis.Autoscaling.MemoryMax = quantity("512Mi") },
			expectedError: "redis.autoscaling.memoryMax 512Mi must not be less than redis.resources.limits.memory 1Gi"},
		"no memory limit": {change: func(rf *RedisFailover) { rf.Spec.Redis.Resources = corev1.ResourceRequirements{} },
			expectedError: "redis.autoscaling requires redis.resources.limits.memory"},
		"bootstrapNode": {change: func(rf *RedisFailover) { rf.Spec.BootstrapNode = &BootstrapSettings{Host: "10.0.0.9"} },
			expectedError: "redis.autoscaling.mode Auto is not available with bootstrapNode"},
		"recreateAfter below 5m": {change: func(rf *RedisFailover) {
			rf.Spec.Redis.Autoscaling.RecreateAfter = &metav1.Duration{Duration: time.Minute}
		}, expectedError: "redis.autoscaling.recreateAfter 1m0s must be 0s or at least 5m0s"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			rf := autoRF()
			test.change(rf)
			err := rf.Validate()
			if test.expectedError != "" {
				assert.ErrorContains(t, err, test.expectedError)
				return
			}
			require.NoError(t, err)
			assert.True(t, rf.AutoscalingEnabled())
		})
	}
}

// The CRD accepts mode Auto, and its description has the requirements.
func TestCRDModeAuto(t *testing.T) {
	data, err := os.ReadFile("../../../manifests/databases.spotahome.com_redisfailovers.yaml")
	require.NoError(t, err)
	var crd map[string]any
	require.NoError(t, yaml.Unmarshal(data, &crd))
	node := crd["spec"].(map[string]any)["versions"].([]any)[0].(map[string]any)["schema"].(map[string]any)["openAPIV3Schema"]
	for _, key := range []string{"spec", "redis", "autoscaling", "mode"} {
		node = node.(map[string]any)["properties"].(map[string]any)[key]
	}
	mode := node.(map[string]any)
	assert.Equal(t, []any{"Off", "Recommend", "Auto"}, mode["enum"])
	assert.NotContains(t, mode["description"], "not available")
	assert.Contains(t, mode["description"], "Kubernetes 1.33")
}

// recreateAfterSchema returns a check of the CRD pattern and maxLength of
// recreateAfter, as the API server applies them.
func recreateAfterSchema(t *testing.T) func(string) bool {
	t.Helper()
	data, err := os.ReadFile("../../../manifests/databases.spotahome.com_redisfailovers.yaml")
	require.NoError(t, err)
	var crd map[string]any
	require.NoError(t, yaml.Unmarshal(data, &crd))
	node := crd["spec"].(map[string]any)["versions"].([]any)[0].(map[string]any)["schema"].(map[string]any)["openAPIV3Schema"]
	for _, key := range []string{"spec", "redis", "autoscaling", "recreateAfter"} {
		node = node.(map[string]any)["properties"].(map[string]any)[key]
	}
	pattern, _ := node.(map[string]any)["pattern"].(string)
	require.NotEmpty(t, pattern)
	maxLength, _ := node.(map[string]any)["maxLength"].(float64)
	require.NotZero(t, maxLength)
	re := regexp.MustCompile(pattern)
	return func(value string) bool {
		// The API server counts characters, not bytes. The fast length
		// check is first: with the race detector, the regexp is slow for
		// the long random values.
		return utf8.RuneCountInString(value) <= int(maxLength) && re.MatchString(value)
	}
}

// The API server checks recreateAfter with the schema of the CRD. The schema
// must accept each value that Validate accepts in a usual notation, and
// refuse a value that metav1.Duration cannot parse.
func TestRecreateAfterPattern(t *testing.T) {
	accepts := recreateAfterSchema(t)

	for _, value := range []string{"0", "-0", "0s", "5m", "5m0s", "600s", "1h30m", "1.5h", ".5h", "1.h", "+5m", "300000ms", "999999h", "999999h59m59s"} {
		d, err := time.ParseDuration(value)
		require.NoError(t, err, value)
		assert.True(t, accepts(value), value)
		rf := rfWithAutoscaling("", "", &AutoscalingSettings{RecreateAfter: &metav1.Duration{Duration: d}})
		require.NoError(t, rf.Validate(), value)
	}
	// The schema checks only the format. Validate refuses a short or a
	// negative wait.
	for _, value := range []string{"90s", "-5m", "1us", "300000µs", "300000μs"} {
		d, err := time.ParseDuration(value)
		require.NoError(t, err, value)
		assert.True(t, accepts(value), value)
		rf := rfWithAutoscaling("", "", &AutoscalingSettings{RecreateAfter: &metav1.Duration{Duration: d}})
		require.Error(t, rf.Validate(), value)
	}
	for _, value := range []string{"5min", "5 m", "", "m", "5", ".", ".s", "00", "5m-1s", "99999999999h", "9223372036854775808ns", "2562048h"} {
		_, err := time.ParseDuration(value)
		require.Error(t, err, value)
		assert.False(t, accepts(value), value)
	}
	// Valid, but longer than the schema allows.
	for _, value := range []string{"1000000h", "300000000µs", "999999h999999h99h"} {
		_, err := time.ParseDuration(value)
		require.NoError(t, err, value)
		assert.False(t, accepts(value), value)
	}
	// Duration.String of values up to more than 100 years, with a precision
	// of a millisecond.
	for _, d := range []time.Duration{0, time.Nanosecond, 1500 * time.Nanosecond, 1500 * time.Millisecond, 5 * time.Minute, 90 * time.Minute, -time.Minute, time.Hour + time.Millisecond, 3 * 365 * 24 * time.Hour, 99999*time.Hour + 59*time.Minute + 59999*time.Millisecond, 999999*time.Hour + 59*time.Minute + 59*time.Second} {
		assert.True(t, accepts(d.String()), d.String())
	}
}

// A value that the schema accepts, but that time.ParseDuration refuses, stops
// the decode of each list of RedisFailovers. Overflow is the risk.
func TestRecreateAfterSchemaAcceptsOnlyParsableValues(t *testing.T) {
	accepts := recreateAfterSchema(t)
	// parses requires that a value of the schema parses. It returns true
	// if the schema accepts the value.
	parses := func(value string) bool {
		if !accepts(value) {
			return false
		}
		_, err := time.ParseDuration(value)
		require.NoError(t, err, "the schema accepts %q", value)
		return true
	}
	// The largest sums that 16 characters allow.
	for _, value := range []string{"999999h999999h9h", "999999h999999.9h", "999999.999999h9h", "-999999h999999h9h", "99999h99999h999h"} {
		parses(value)
	}

	rng := rand.New(rand.NewPCG(1, 2))
	alphabet := []string{"0", "1", "5", "9", "9", "9", ".", "+", "-", "n", "s", "u", "µ", "μ", "m", "h", "h"}
	units := []string{"ns", "us", "µs", "μs", "ms", "s", "m", "h", "h", "h", "x", ""}
	digits := func(n int) string {
		var b strings.Builder
		for range n {
			b.WriteByte("0123456789999999"[rng.IntN(16)])
		}
		return b.String()
	}
	accepted := 0
	for range 300_000 {
		var b strings.Builder
		if rng.IntN(2) == 0 {
			for range 1 + rng.IntN(17) {
				b.WriteString(alphabet[rng.IntN(len(alphabet))])
			}
		} else {
			b.WriteString([]string{"", "", "+", "-"}[rng.IntN(4)])
			for range 1 + rng.IntN(4) {
				b.WriteString(digits(rng.IntN(9)))
				if rng.IntN(3) == 0 {
					b.WriteString("." + digits(rng.IntN(9)))
				}
				b.WriteString(units[rng.IntN(len(units))])
			}
		}
		if parses(b.String()) {
			accepted++
		}
	}
	assert.Greater(t, accepted, 10_000)
}
