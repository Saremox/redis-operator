package service

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	mK8SService "github.com/saremox/redis-operator/mocks/service/k8s"
	"github.com/saremox/redis-operator/service/k8s"
)

// A pod that the StatefulSet created from the template of a step keeps its
// limit after a revert. Without the step, or with another change, it takes
// the user path.
func TestResizePodInPlaceKeepsTheLimitOfAStep(t *testing.T) {
	step, before := resources("1", "2Gi"), resources("1", "1Gi")
	image := podTemplate(before)
	image.Spec.Containers[0].Image = "redis:8"
	relabel := map[string]string{appsv1.ControllerRevisionHashLabelKey: "new"}
	tests := map[string]struct {
		update corev1.PodTemplateSpec
		limits AutoscalerLimits
		kept   bool
	}{
		"revert of the step": {podTemplate(before), AutoscalerLimits{Step: 2 << 30}, true},
		"no step":            {podTemplate(before), AutoscalerLimits{}, false},
		"another step limit": {podTemplate(before), AutoscalerLimits{Step: 3 << 30}, false},
		"also an image":      {image, AutoscalerLimits{Step: 2 << 30}, false},
		"a larger template":  {podTemplate(resources("1", "4Gi")), AutoscalerLimits{Step: 2 << 30}, false},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			c := resizeCase{k8s.PodResizeSupport{Supported: true}, podTemplate(step), test.update, podFrom(podTemplate(step))}
			setup := func(ms *mK8SService.Services) {
				ms.On("UpdatePodLabels", "testns", resizePod, relabel).Maybe().Return(nil)
				ms.On("ResizePod", "testns", resizePod, mock.Anything).Maybe().Return(nil)
			}
			result, ms, _, err := runResizeEffective(t, resizeRF(), c, test.limits, setup, nil)
			require.NoError(t, err)
			if test.kept {
				assert.Equal(t, ResizeResult{Action: ResizeDone}, result)
				ms.AssertCalled(t, "UpdatePodLabels", "testns", resizePod, relabel)
				ms.AssertCalled(t, "UpdatePodAnnotations", "testns", resizePod, map[string]string{resizeRequestedAnnotation: ""})
				ms.AssertNotCalled(t, "ResizePod", mock.Anything, mock.Anything, mock.Anything)
				return
			}
			assert.NotEqual(t, ResizeDone, result.Action)
			ms.AssertNotCalled(t, "UpdatePodLabels", mock.Anything, mock.Anything, mock.Anything)
		})
	}

	fail := errors.New("boom")
	revert := resizeCase{fullSupport, podTemplate(step), podTemplate(before), podFrom(podTemplate(step))}
	t.Run("label error", func(t *testing.T) {
		_, _, _, err := runResizeEffective(t, resizeRF(), revert, AutoscalerLimits{Step: 2 << 30}, func(ms *mK8SService.Services) {
			ms.On("UpdatePodLabels", "testns", resizePod, mock.Anything).Return(fail)
		}, nil)
		require.ErrorIs(t, err, fail)
	})

	t.Run("annotation error", func(t *testing.T) {
		_, _, _, err := runResizeEffective(t, resizeRF(), revert, AutoscalerLimits{Step: 2 << 30}, func(ms *mK8SService.Services) {
			ms.On("UpdatePodLabels", "testns", resizePod, mock.Anything).Return(nil)
			ms.ExpectedCalls = slices.DeleteFunc(ms.ExpectedCalls, func(c *mock.Call) bool { return c.Method == "UpdatePodAnnotations" })
			ms.On("UpdatePodAnnotations", "testns", resizePod, mock.Anything).Return(fail)
		}, nil)
		require.ErrorIs(t, err, fail)
	})

	t.Run("a deferred resize of a step before a revert waits", func(t *testing.T) {
		old, step := resources("1", "1Gi"), resources("1", "2Gi")
		deferred := stalePod(step, old, condition(corev1.PodResizePending, corev1.PodReasonDeferred, 10*time.Minute))
		c := resizeCase{fullSupport, podTemplate(old), podTemplate(step), deferred}
		result, ms, _, err := runResizeEffective(t, resizeRF(), c, AutoscalerLimits{Spec: 1 << 30, Effective: 2 << 30}, nil, nil)
		require.NoError(t, err)
		assert.Equal(t, ResizeWaiting, result.Action)
		assert.Equal(t, ResizeTransient, result.Class)
		ms.AssertCalled(t, "UpdatePodAnnotations", "testns", resizePod, mock.MatchedBy(func(a map[string]string) bool { return a[resizeRequestedAnnotation] != "" }))

		// A user revision keeps the timeout of the condition.
		result, _, _, err = runResizeEffective(t, resizeRF(), c, AutoscalerLimits{}, nil, nil)
		require.NoError(t, err)
		assert.Equal(t, ResizeRecreate, result.Action)

		// An infeasible resize needs no wait.
		c.pod = stalePod(step, old, condition(corev1.PodResizePending, corev1.PodReasonInfeasible, 0))
		result, _, _, err = runResizeEffective(t, resizeRF(), c, AutoscalerLimits{Spec: 1 << 30, Effective: 2 << 30}, nil, nil)
		require.NoError(t, err)
		assert.Equal(t, ResizePermanent, result.Class)

		fail := errors.New("boom")
		_, _, _, err = runResizeEffective(t, resizeRF(), resizeCase{fullSupport, podTemplate(old), podTemplate(step), deferred}, AutoscalerLimits{Spec: 1 << 30, Effective: 2 << 30}, func(ms *mK8SService.Services) {
			ms.ExpectedCalls = slices.DeleteFunc(ms.ExpectedCalls, func(c *mock.Call) bool { return c.Method == "UpdatePodAnnotations" })
			ms.On("UpdatePodAnnotations", "testns", resizePod, mock.Anything).Return(fail)
		}, nil)
		require.ErrorIs(t, err, fail)
	})

	t.Run("revision not available with a step", func(t *testing.T) {
		result, _, _, err := runResizeEffective(t, resizeRF(), revert, AutoscalerLimits{Step: 2 << 30}, revisionNotFound("old"), nil)
		require.NoError(t, err)
		assert.True(t, result.AutoscalerRevision)
	})
}
