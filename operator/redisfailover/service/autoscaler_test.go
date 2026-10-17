package service

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	mK8SService "github.com/saremox/redis-operator/mocks/service/k8s"
	mRedisService "github.com/saremox/redis-operator/mocks/service/redis"
	"github.com/saremox/redis-operator/service/redis"
)

func autoscalerTestRF(t *testing.T, autoscaling *redisfailoverv1.AutoscalingSettings) *redisfailoverv1.RedisFailover {
	t.Helper()
	rf := &redisfailoverv1.RedisFailover{ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "testns"}}
	rf.Spec.Sentinel.Enabled = new(true)
	rf.Spec.Redis.Exporter.Enabled = true
	rf.Spec.Sentinel.Exporter.Enabled = true
	rf.Spec.Redis.Resources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
	}
	rf.Spec.Redis.MaxMemory = &redisfailoverv1.MaxMemorySettings{}
	rf.Spec.Redis.Autoscaling = autoscaling
	require.NoError(t, rf.Validate())
	return rf
}

// Recommend changes no pod: the generated objects must be the same as
// without autoscaling, or the operator would roll the pods out.
func TestGeneratedObjectsIgnoreRecommend(t *testing.T) {
	memoryMax := resource.MustParse("2Gi")
	plain := autoscalerTestRF(t, nil)
	recommend := autoscalerTestRF(t, &redisfailoverv1.AutoscalingSettings{Mode: redisfailoverv1.AutoscalingModeRecommend, MemoryMax: &memoryMax})
	labels := map[string]string{"app": "test"}
	ownerRefs := []metav1.OwnerReference{{Name: "test"}}

	assert.Equal(t, generateRedisStatefulSet(plain, labels, ownerRefs, "pw"), generateRedisStatefulSet(recommend, labels, ownerRefs, "pw"))
	assert.Equal(t, generateSentinelDeployment(plain, labels, ownerRefs), generateSentinelDeployment(recommend, labels, ownerRefs))
	assert.Equal(t, generateRedisConfigMap(plain, labels, ownerRefs), generateRedisConfigMap(recommend, labels, ownerRefs))
	assert.Equal(t, generateRedisShutdownConfigMap(plain, labels, ownerRefs), generateRedisShutdownConfigMap(recommend, labels, ownerRefs))
	assert.Equal(t, generateRedisReadinessConfigMap(plain, labels, ownerRefs), generateRedisReadinessConfigMap(recommend, labels, ownerRefs))
	assert.Equal(t, generateSentinelConfigMap(plain, labels, ownerRefs), generateSentinelConfigMap(recommend, labels, ownerRefs))
	assert.Equal(t, generateRedisService(plain, labels, ownerRefs), generateRedisService(recommend, labels, ownerRefs))
	assert.Equal(t, generateRedisMasterService(plain, labels, ownerRefs), generateRedisMasterService(recommend, labels, ownerRefs))
	assert.Equal(t, generateRedisSlaveService(plain, labels, ownerRefs), generateRedisSlaveService(recommend, labels, ownerRefs))
	assert.Equal(t, generateSentinelService(plain, labels, ownerRefs), generateSentinelService(recommend, labels, ownerRefs))
}

func TestGenerateAutoscalerStateConfigMap(t *testing.T) {
	rf := autoscalerTestRF(t, nil)
	ownerRefs := []metav1.OwnerReference{{Name: "test", Controller: new(true)}}

	cm := GenerateAutoscalerStateConfigMap(rf, map[string]string{"app.kubernetes.io/managed-by": "redis-operator"}, ownerRefs, `{"version":1}`)

	assert.Equal(t, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "rfa-test",
			Namespace: "testns",
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "redis-operator",
				"app.kubernetes.io/name":       "test",
				"app.kubernetes.io/component":  "autoscaler",
				"app.kubernetes.io/part-of":    "redis-failover",
			},
			OwnerReferences: ownerRefs,
		},
		Data: map[string]string{"state": `{"version":1}`},
	}, cm)
}

func TestCheckerGetAutoscalerInfo(t *testing.T) {
	rf := autoscalerTestRF(t, nil)
	rf.Spec.Auth.SecretPath = "auth"
	ms := &mK8SService.Services{}
	ms.On("GetSecret", "testns", "auth").Return(&corev1.Secret{Data: map[string][]byte{"password": []byte("pw")}}, nil)
	mr := &mRedisService.Client{}
	want := &redis.AutoscalerInfo{RunID: "run1"}
	mr.On("GetAutoscalerInfo", "10.0.0.1", "6379", "pw").Once().Return(want, nil)
	checker := NewRedisFailoverChecker(ms, mr, log.Dummy, metrics.Dummy)

	got, err := checker.GetAutoscalerInfo(rf, "10.0.0.1")

	require.NoError(t, err)
	assert.Same(t, want, got)

	ms = &mK8SService.Services{}
	ms.On("GetSecret", "testns", "auth").Return(nil, assert.AnError)
	checker = NewRedisFailoverChecker(ms, mr, log.Dummy, metrics.Dummy)
	_, err = checker.GetAutoscalerInfo(rf, "10.0.0.1")
	require.ErrorIs(t, err, assert.AnError)
	mr.AssertExpectations(t)
}

func TestTemplateRedisMemoryLimit(t *testing.T) {
	ss := generateRedisStatefulSet(autoscalerTestRF(t, nil), nil, nil, "pw")
	require.Len(t, ss.Spec.Template.Spec.Containers, 2, "redis and the exporter")
	assert.Equal(t, int64(1<<30), TemplateRedisMemoryLimit(ss))

	ss.Spec.Template.Spec.Containers = slices.DeleteFunc(ss.Spec.Template.Spec.Containers, func(c corev1.Container) bool { return c.Name == redisContainerName })
	assert.Zero(t, TemplateRedisMemoryLimit(ss))
}

func TestAppliedRedisMemoryLimit(t *testing.T) {
	limit := func(size string) corev1.ResourceRequirements {
		return corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(size)}}
	}
	pod := func(status corev1.ContainerStatus) *corev1.Pod {
		status.Name = redisContainerName
		return &corev1.Pod{
			Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: "exporter", Resources: limit("64Mi")}, {Name: redisContainerName, Resources: limit("2Gi")}}},
			Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "exporter"}, status}},
		}
	}
	reported := limit("1Gi")
	running := corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	tests := map[string]struct {
		pod  *corev1.Pod
		want int64
	}{
		"kubelet report":                    {pod(corev1.ContainerStatus{Resources: &reported, State: running}), 1 << 30},
		"kubelet report, not running":       {pod(corev1.ContainerStatus{Resources: &reported}), 1 << 30},
		"no report, running":                {pod(corev1.ContainerStatus{State: running}), 2 << 30},
		"no report, not running":            {pod(corev1.ContainerStatus{}), 0},
		"no container status":               {&corev1.Pod{}, 0},
		"running without a redis container": {&corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: redisContainerName, State: running}}}}, 0},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, test.want, AppliedRedisMemoryLimit(test.pod))
		})
	}
}

func TestRequestedRedisMemoryLimit(t *testing.T) {
	limit := corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")}}
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "exporter"}, {Name: redisContainerName, Resources: limit}}}}
	assert.Equal(t, int64(2<<30), RequestedRedisMemoryLimit(pod))
	assert.Zero(t, RequestedRedisMemoryLimit(&corev1.Pod{}))
}

func TestResizeRequested(t *testing.T) {
	assert.False(t, ResizeRequested(&corev1.Pod{}))
	assert.False(t, ResizeRequested(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{resizeRequestedAnnotation: ""}}}))
	assert.True(t, ResizeRequested(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{resizeRequestedAnnotation: "2026-01-01T00:00:00Z"}}}))
}
