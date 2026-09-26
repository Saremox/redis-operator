package v1

import (
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func rfWithMemoryLimit(limit string, mm *MaxMemorySettings) *RedisFailover {
	rf := &RedisFailover{ObjectMeta: metav1.ObjectMeta{Name: "test"}}
	if limit != "" {
		rf.Spec.Redis.Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(limit)}
	}
	rf.Spec.Redis.MaxMemory = mm
	return rf
}

func TestMaxMemoryFor(t *testing.T) {
	tests := []struct {
		limit    string
		percent  int32
		expected string
	}{
		{"64Mi", 75, "32Mi"},
		{"96Mi", 75, "64Mi"},
		{"128Mi", 75, "96Mi"},
		{"1Gi", 75, "768Mi"},
		{"128Mi", 90, "96Mi"},
		{"1Gi", 50, "512Mi"},
	}
	for _, test := range tests {
		t.Run(test.limit, func(t *testing.T) {
			rf := rfWithMemoryLimit(test.limit, &MaxMemorySettings{Percent: test.percent})
			expected := resource.MustParse(test.expected)
			limit := resource.MustParse(test.limit)
			assert.Equal(t, expected.Value(), rf.MaxMemoryFor(limit.Value()))
		})
	}
	assert.Zero(t, rfWithMemoryLimit("1Gi", nil).MaxMemoryFor(1<<30))
}

func TestCustomConfigSets(t *testing.T) {
	rf := &RedisFailover{}
	rf.Spec.Redis.CustomConfig = []string{"maxmemory-samples 10", "MAXMEMORY 100mb", " maxmemory-policy allkeys-lru"}
	assert.True(t, rf.CustomConfigSets("maxmemory"))
	// Skipped by SetCustomRedisConfig, so not an override.
	assert.False(t, rf.CustomConfigSets("maxmemory-policy"))
}

func TestValidateMaxMemory(t *testing.T) {
	tests := []struct {
		name          string
		settings      MaxMemorySettings
		customConfig  []string
		expectedError string
	}{
		{name: "defaults"},
		{name: "replicas enforcing maxmemory", customConfig: []string{"slave-ignore-maxmemory NO"}, expectedError: `redis.maxMemory does not support "slave-ignore-maxmemory NO" in customConfig`},
		{name: "replicas ignoring maxmemory", customConfig: []string{"replica-ignore-maxmemory yes"}},
		{name: "percent too low", settings: MaxMemorySettings{Percent: 5}, expectedError: "redis.maxMemory.percent 5 must be between 10 and 95"},
		{name: "percent too high", settings: MaxMemorySettings{Percent: 96}, expectedError: "redis.maxMemory.percent 96 must be between 10 and 95"},
		{name: "unknown policy", settings: MaxMemorySettings{Policy: "lru"}, expectedError: `redis.maxMemory.policy "lru" must be one of`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			settings := test.settings
			// Requirements for managing maxmemory are not validated here.
			rf := rfWithMemoryLimit("", &settings)
			rf.Spec.Redis.CustomConfig = test.customConfig
			err := rf.Validate()
			if test.expectedError != "" {
				assert.ErrorContains(t, err, test.expectedError)
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, MaxMemorySettings{Percent: 75, Policy: "noeviction"}, *rf.Spec.Redis.MaxMemory)
		})
	}
}

func TestManagedMaxMemoryError(t *testing.T) {
	tests := []struct {
		name          string
		limit         string
		expectedError string
	}{
		{name: "minimum limit", limit: "64Mi"},
		{name: "no limit", expectedError: "redis.maxMemory requires redis.resources.limits.memory"},
		{name: "limit too small", limit: "63Mi", expectedError: "redis.maxMemory requires redis.resources.limits.memory of at least 64Mi, got 63Mi"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rf := rfWithMemoryLimit(test.limit, &MaxMemorySettings{})
			err := rf.ManagedMaxMemoryError()
			if test.expectedError != "" {
				assert.EqualError(t, err, test.expectedError)
				return
			}
			assert.NoError(t, err)
		})
	}
}
