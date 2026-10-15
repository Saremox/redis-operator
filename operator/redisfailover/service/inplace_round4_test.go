package service

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	mK8SService "github.com/saremox/redis-operator/mocks/service/k8s"
	mRedisService "github.com/saremox/redis-operator/mocks/service/redis"
	"github.com/saremox/redis-operator/service/k8s"
	"github.com/saremox/redis-operator/service/redis"
)

// A pod on the update revision that a resize request makes stale, for
// example a request of a step that another operator replica marks again
// after the revert, keeps its resources. Only the request goes.
func TestResizePodInPlaceKeepsAPodOnTheUpdateRevision(t *testing.T) {
	before := resources("1", "1Gi")
	for name, test := range map[string]struct {
		requested, applied string
		kept               bool
	}{
		"requests the step":       {"1280Mi", "1Gi", true},
		"applied the step":        {"1280Mi", "1280Mi", true},
		"requests less":           {"768Mi", "1Gi", false},
		"runs with less":          {"1Gi", "768Mi", false},
		"also another cpu target": {"", "", false},
	} {
		t.Run(name, func(t *testing.T) {
			pod := stalePod(resources("2", "1280Mi"), before)
			if test.requested != "" {
				pod = stalePod(resources("1", test.requested), resources("1", test.applied))
			}
			pod.Labels[appsv1.ControllerRevisionHashLabelKey] = "new"
			pod.Annotations = map[string]string{resizeRequestedAnnotation: "2026-01-01T00:00:00Z"}
			c := resizeCase{k8s.PodResizeSupport{Supported: true}, podTemplate(before), podTemplate(before), pod}
			setup := func(ms *mK8SService.Services) {
				ms.On("UpdatePodLabels", "testns", resizePod, mock.Anything).Maybe().Return(nil)
				ms.On("ResizePod", "testns", resizePod, mock.Anything).Maybe().Return(nil)
			}
			result, ms, _, err := runResizeEffective(t, resizeRF(), c, AutoscalerLimits{Step: 1280 << 20}, setup, nil)
			require.NoError(t, err)
			if test.kept {
				assert.Equal(t, ResizeResult{Action: ResizeDone}, result)
				ms.AssertCalled(t, "UpdatePodAnnotations", "testns", resizePod, map[string]string{resizeRequestedAnnotation: ""})
				ms.AssertNotCalled(t, "ResizePod", mock.Anything, mock.Anything, mock.Anything)
				return
			}
			assert.NotEqual(t, ResizeDone, result.Action)
		})
	}
}

// A stale pod that already runs with the limit of a step or a larger one,
// for example after a resize from outside, moves to the step revision with
// its resources. A resize to the step limit would lower it: Kubernetes 1.35
// shrinks the pod, and older versions refuse the resize and hold the step.
func TestResizePodInPlaceKeepsALargerPodOfAStep(t *testing.T) {
	before, step := resources("1", "1Gi"), resources("1", "2Gi")
	limits := AutoscalerLimits{Spec: 1 << 30, Effective: 2 << 30}
	relabel := map[string]string{appsv1.ControllerRevisionHashLabelKey: "new"}
	for name, support := range map[string]k8s.PodResizeSupport{"1.34": {Supported: true}, "1.35": fullSupport} {
		t.Run(name, func(t *testing.T) {
			c := resizeCase{support, podTemplate(before), podTemplate(step), stalePod(resources("1", "3Gi"), resources("1", "3Gi"))}
			setup := func(ms *mK8SService.Services) {
				ms.On("UpdatePodLabels", "testns", resizePod, relabel).Maybe().Return(nil)
				ms.On("ResizePod", "testns", resizePod, mock.Anything).Maybe().Return(nil)
			}
			memory := func(mr *mRedisService.Client) {
				mr.On("GetMemoryInfo", mock.Anything, mock.Anything, mock.Anything).Maybe().Return(&redis.MemoryInfo{UsedMemoryRSS: 100 << 20}, nil)
			}
			result, ms, _, err := runResizeEffective(t, resizeRF(), c, limits, setup, memory)
			require.NoError(t, err)
			assert.Equal(t, ResizeResult{Action: ResizeDone, AutoscalerRevision: true}, result)
			ms.AssertCalled(t, "UpdatePodLabels", "testns", resizePod, relabel)
			ms.AssertNotCalled(t, "ResizePod", mock.Anything, mock.Anything, mock.Anything)
		})
	}
	// A user change of the template resizes the pod as before.
	c := resizeCase{fullSupport, podTemplate(before), podTemplate(step), stalePod(resources("1", "3Gi"), resources("1", "3Gi"))}
	result, ms, _, err := runResizeEffective(t, resizeRF(), c, AutoscalerLimits{}, func(ms *mK8SService.Services) {
		ms.On("ResizePod", "testns", resizePod, mock.Anything).Maybe().Return(nil)
	}, func(mr *mRedisService.Client) {
		mr.On("GetMemoryInfo", mock.Anything, mock.Anything, mock.Anything).Maybe().Return(&redis.MemoryInfo{UsedMemoryRSS: 100 << 20}, nil)
	})
	require.NoError(t, err)
	assert.NotEqual(t, ResizeDone, result.Action)
	ms.AssertNotCalled(t, "UpdatePodLabels", mock.Anything, mock.Anything, mock.Anything)
}

// Before Kubernetes 1.35, a pod that requests the step since a revert keeps
// the request when a user change gives another limit. The API server would
// refuse the lower limit in the pod spec, and the user path would recreate
// the pod. On 1.35 the pod gets the limit of the update revision.
func TestResizePodInPlaceHoldsTheRequestOfAStep(t *testing.T) {
	before, step, user := resources("1", "1Gi"), resources("1", "2Gi"), resources("1", "1536Mi")
	limits := AutoscalerLimits{Step: 2 << 30}
	t.Run("before 1.35", func(t *testing.T) {
		c := resizeCase{k8s.PodResizeSupport{Supported: true}, podTemplate(before), podTemplate(user), stalePod(step, before)}
		result, ms, _, err := runResizeEffective(t, resizeRF(), c, limits, nil, nil)
		require.NoError(t, err)
		assert.True(t, result.AutoscalerRevision, "the rollout never recreates the pod")
		assert.Equal(t, ResizeWaiting, result.Action)
		ms.AssertNotCalled(t, "ResizePod", mock.Anything, mock.Anything, mock.Anything)
		ms.AssertCalled(t, "UpdatePodAnnotations", "testns", resizePod, mock.Anything)

		// The kubelet cannot apply the step on the node.
		infeasible := stalePod(step, before, condition(corev1.PodResizePending, corev1.PodReasonInfeasible, 0))
		infeasible.Annotations = map[string]string{resizeRequestedAnnotation: "2026-01-01T00:00:00Z"}
		c.pod = infeasible
		result, _, _, err = runResizeEffective(t, resizeRF(), c, limits, nil, nil)
		require.NoError(t, err)
		assert.True(t, result.AutoscalerRevision)
		assert.Equal(t, ResizeRecreate, result.Action)
		assert.Equal(t, ResizePermanent, result.Class)

		// Without the step, the user path recreates the pod.
		c.pod = stalePod(step, before)
		result, _, _, err = runResizeEffective(t, resizeRF(), c, AutoscalerLimits{}, nil, nil)
		require.NoError(t, err)
		assert.False(t, result.AutoscalerRevision)
		assert.Equal(t, "lowering a memory limit in place needs Kubernetes 1.35", result.Message)
	})
	t.Run("1.35", func(t *testing.T) {
		var got string
		c := resizeCase{fullSupport, podTemplate(before), podTemplate(user), stalePod(step, before)}
		result, _, _, err := runResizeEffective(t, resizeRF(), c, limits, func(ms *mK8SService.Services) {
			ms.On("ResizePod", "testns", resizePod, mock.Anything).Once().Run(func(args mock.Arguments) {
				r := args.Get(2).(map[string]corev1.ResourceRequirements)[redisContainerName]
				got = r.Limits.Memory().String()
			}).Return(nil)
		}, nil)
		require.NoError(t, err)
		assert.Equal(t, ResizeWaiting, result.Action)
		assert.Equal(t, "1536Mi", got)
	})
}

// Without a known state, a pod on a revision with a larger memory limit up to
// memoryMax can be a pod of a step. It keeps its limit, and a pod above
// memoryMax or with another change takes the user path.
func TestResizePodInPlaceKeepsAPodOfAnUnknownStep(t *testing.T) {
	before := resources("1", "1Gi")
	relabel := map[string]string{appsv1.ControllerRevisionHashLabelKey: "new"}
	image := podTemplate(resources("1", "2Gi"))
	image.Spec.Containers[0].Image = "redis:8"
	for name, test := range map[string]struct {
		current corev1.PodTemplateSpec
		stepMax int64
		kept    bool
	}{
		"a larger pod":           {podTemplate(resources("1", "2Gi")), 4 << 30, true},
		"at memoryMax":           {podTemplate(resources("1", "2Gi")), 2 << 30, true},
		"above memoryMax":        {podTemplate(resources("1", "2Gi")), 1536 << 20, false},
		"a known state":          {podTemplate(resources("1", "2Gi")), 0, false},
		"also another change":    {image, 4 << 30, false},
		"a smaller pod revision": {podTemplate(resources("1", "768Mi")), 4 << 30, false},
	} {
		t.Run(name, func(t *testing.T) {
			c := resizeCase{fullSupport, test.current, podTemplate(before), podFrom(test.current)}
			setup := func(ms *mK8SService.Services) {
				ms.On("UpdatePodLabels", "testns", resizePod, relabel).Maybe().Return(nil)
				ms.On("ResizePod", "testns", resizePod, mock.Anything).Maybe().Return(nil)
			}
			memory := func(mr *mRedisService.Client) {
				mr.On("GetMemoryInfo", mock.Anything, mock.Anything, mock.Anything).Maybe().Return(&redis.MemoryInfo{UsedMemoryRSS: 100 << 20}, nil)
			}
			result, ms, _, err := runResizeEffective(t, resizeRF(), c, AutoscalerLimits{StepMax: test.stepMax}, setup, memory)
			require.NoError(t, err)
			if test.kept {
				assert.Equal(t, ResizeResult{Action: ResizeDone}, result)
				ms.AssertCalled(t, "UpdatePodLabels", "testns", resizePod, relabel)
				ms.AssertNotCalled(t, "ResizePod", mock.Anything, mock.Anything, mock.Anything)
				return
			}
			assert.NotEqual(t, ResizeDone, result.Action)
		})
	}
}

// Without a known state, a pod that requests a larger limit up to memoryMax
// keeps the request before Kubernetes 1.35.
func TestResizePodInPlaceHoldsTheRequestOfAnUnknownStep(t *testing.T) {
	before, step := resources("1", "1Gi"), resources("1", "2Gi")
	c := resizeCase{k8s.PodResizeSupport{Supported: true}, podTemplate(before), podTemplate(resources("1", "1536Mi")), stalePod(step, before)}
	result, ms, _, err := runResizeEffective(t, resizeRF(), c, AutoscalerLimits{StepMax: 4 << 30}, nil, nil)
	require.NoError(t, err)
	assert.True(t, result.AutoscalerRevision)
	ms.AssertNotCalled(t, "ResizePod", mock.Anything, mock.Anything, mock.Anything)

	result, _, _, err = runResizeEffective(t, resizeRF(), c, AutoscalerLimits{StepMax: 1536 << 20}, nil, nil)
	require.NoError(t, err)
	assert.False(t, result.AutoscalerRevision, "a request above memoryMax takes the user path")
}

// With redis.inPlaceResize Disabled, a pod of the last step after a revert
// keeps its limit. Without autoscaler limits, the rollout reads nothing.
func TestResizePodInPlaceKeepsAStepPodWithoutInPlaceResize(t *testing.T) {
	step, before := resources("1", "2Gi"), resources("1", "1Gi")
	rf := resizeRF()
	rf.Spec.Redis.InPlaceResize = "Disabled"
	relabel := map[string]string{appsv1.ControllerRevisionHashLabelKey: "new"}
	c := resizeCase{k8s.PodResizeSupport{Supported: true}, podTemplate(step), podTemplate(before), podFrom(podTemplate(step))}
	result, ms, _, err := runResizeEffective(t, rf, c, AutoscalerLimits{Step: 2 << 30}, func(ms *mK8SService.Services) {
		ms.On("UpdatePodLabels", "testns", resizePod, relabel).Once().Return(nil)
	}, nil)
	require.NoError(t, err)
	assert.Equal(t, ResizeResult{Action: ResizeDone}, result)
	ms.AssertCalled(t, "UpdatePodLabels", "testns", resizePod, relabel)

	result, ms, _, err = runResizeEffective(t, rf, c, AutoscalerLimits{}, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, ResizeResult{Action: ResizeRecreate, Class: ResizePermanent, Message: "redis.inPlaceResize is Disabled"}, result)
	ms.AssertNotCalled(t, "GetPod", mock.Anything, mock.Anything)
	ms.AssertNotCalled(t, "PodResizeSupport")
}
