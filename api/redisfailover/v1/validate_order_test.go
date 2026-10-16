package v1

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestValidateSetsNoDefaultAfterAnError pins the order of the checks and the
// defaults: the autoscaling defaults come before the bootstrap check, and the
// other defaults after all the checks.
func TestValidateSetsNoDefaultAfterAnError(t *testing.T) {
	rf := generateRedisFailover("test", &BootstrapSettings{})
	rf.Spec.Redis.Autoscaling = &AutoscalingSettings{}
	rf.Spec.Redis.CustomConfig = []string{"tcp-keepalive 60"}

	require.EqualError(t, rf.Validate(), "BootstrapNode must include a host when provided")

	assert.Equal(t, AutoscalingModeOff, rf.Spec.Redis.Autoscaling.Mode)
	assert.Equal(t, AutoscalingWorkloadAuto, rf.Spec.Redis.Autoscaling.Workload)
	assert.Empty(t, rf.Spec.BootstrapNode.Port)
	assert.Equal(t, []string{"tcp-keepalive 60"}, rf.Spec.Redis.CustomConfig)
	assert.Empty(t, rf.Spec.Redis.Image)
	assert.Empty(t, rf.Spec.Sentinel.Image)
	assert.Zero(t, rf.Spec.Redis.Replicas)
	assert.Zero(t, rf.Spec.Redis.Port)
	assert.Zero(t, rf.Spec.Sentinel.Replicas)
	assert.Empty(t, rf.Spec.Redis.Exporter.Image)
	assert.Empty(t, rf.Spec.Sentinel.Exporter.Image)
	assert.Empty(t, rf.Spec.Sentinel.CustomConfig)
}

// TestValidateChecksInOrder pins which error Validate returns when the spec
// has more than one error.
func TestValidateChecksInOrder(t *testing.T) {
	rf := generateRedisFailover("test", &BootstrapSettings{})
	rf.Spec.Redis.CustomCommandRenames = []RedisCommandRename{{From: "CONFIG"}}
	rf.Spec.Redis.Exporter.Port = -1
	rf.Spec.Redis.Autoscaling = &AutoscalingSettings{Mode: "x"}

	require.ErrorContains(t, rf.Validate(), `customCommandRenames: "CONFIG" cannot be renamed`)
	rf.Spec.Redis.CustomCommandRenames = nil
	require.EqualError(t, rf.Validate(), "redis.exporter.port -1 must be between 1 and 65535, or 0 for the default")
	rf.Spec.Redis.Exporter.Port = 0
	require.EqualError(t, rf.Validate(), `redis.autoscaling.mode "x" must be Off, Recommend or Auto`)
	rf.Spec.Redis.Autoscaling = nil
	require.EqualError(t, rf.Validate(), "BootstrapNode must include a host when provided")
}

// TestValidateAutoscalingChecksInOrder pins which error validateAutoscaling
// returns when the settings have more than one error.
func TestValidateAutoscalingChecksInOrder(t *testing.T) {
	a := &AutoscalingSettings{Mode: "On", Workload: "Queue", RecreateAfter: &metav1.Duration{Duration: time.Minute}}
	rf := rfWithAutoscaling("", "512Mi", a)
	rf.Spec.BootstrapNode = &BootstrapSettings{Host: "10.0.0.9"}

	require.EqualError(t, rf.validateAutoscaling(), `redis.autoscaling.mode "On" must be Off, Recommend or Auto`)
	a.Mode = AutoscalingModeAuto
	require.EqualError(t, rf.validateAutoscaling(), `redis.autoscaling.workload "Queue" must be Auto, Store or Cache`)
	a.Workload = AutoscalingWorkloadStore
	require.EqualError(t, rf.validateAutoscaling(), "redis.autoscaling.recreateAfter 1m0s must be 0s or at least 5m0s")
	a.RecreateAfter.Duration = 5 * time.Minute
	require.EqualError(t, rf.validateAutoscaling(), "redis.autoscaling.mode Auto is not available with bootstrapNode, because the RedisFailover has no master to sample")
	rf.Spec.BootstrapNode = nil
	require.EqualError(t, rf.validateAutoscaling(), "redis.autoscaling requires redis.resources.limits.memory")
	rf.Spec.Redis.Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}
	require.EqualError(t, rf.validateAutoscaling(), "redis.autoscaling.memoryMax is required for mode Auto")
	a.MemoryMax = quantity("512Mi")
	require.EqualError(t, rf.validateAutoscaling(), "redis.autoscaling.memoryMax 512Mi must not be less than redis.resources.limits.memory 1Gi")
	a.MemoryMax = quantity("2Gi")
	require.EqualError(t, rf.validateAutoscaling(), "redis.autoscaling.mode Auto requires redis.maxMemory")
	rf.Spec.Redis.MaxMemory = &MaxMemorySettings{}
	require.EqualError(t, rf.validateAutoscaling(), "redis.autoscaling.recreateAfter is not available with mode Auto, because a step never recreates a pod")
	a.RecreateAfter = nil
	require.EqualError(t, rf.validateAutoscaling(), "redis.autoscaling.mode Auto requires redis.resources.requests.memory 512Mi equal to the limit 1Gi")
	a.Mode = AutoscalingModeRecommend
	require.EqualError(t, rf.validateAutoscaling(), "redis.autoscaling requires redis.resources.requests.memory 512Mi equal to the limit 1Gi for a store")
	a.Mode = AutoscalingModeOff
	a.RecreateAfter = &metav1.Duration{Duration: time.Minute}
	require.EqualError(t, rf.validateAutoscaling(), "redis.autoscaling.recreateAfter 1m0s must be 0s or at least 5m0s")
	a.RecreateAfter = nil
	require.NoError(t, rf.validateAutoscaling())
}
