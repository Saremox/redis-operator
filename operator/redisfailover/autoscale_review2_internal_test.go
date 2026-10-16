package redisfailover

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	mK8SService "github.com/saremox/redis-operator/mocks/service/k8s"
	"github.com/saremox/redis-operator/operator/redisfailover/autoscale"
	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
)

// timeoutStep runs UpdateRedisesPods and the autoscaler for a step that did
// not end in 15 minutes, and returns the state that the autoscaler writes.
func timeoutStep(t *testing.T, it *inPlaceTest) autoscale.State {
	t.Helper()
	state := stepTo2Gi()
	state.StartedAt = time.Now().Add(-16 * time.Minute).Unix()
	h := it.handler(state)
	require.NoError(t, h.UpdateRedisesPods(it.rf))
	var written autoscale.State
	it.k8s.On("UpdateConfigMap", "testns", mock.Anything).Once().Run(func(args mock.Arguments) {
		require.NoError(t, json.Unmarshal([]byte(args.Get(1).(*corev1.ConfigMap).Data["state"]), &written))
	}).Return(nil)
	stored := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "rfa-test", ResourceVersion: "1"}}
	h.autoscaleReads.Store(failoverKey(it.rf), autoscaleRead{state: state, cm: stored, spec: 1 << 30, effective: autoscaleEffective})
	it.rf.Status.State = redisfailoverv1.HealthyState
	h.autoscale(it.rf, redisfailoverv1.AutoscalingStatus{})
	return written
}

// A missing or terminating pod comes back from the template of the
// StatefulSet. While that can be the template of the step, the timeout holds
// the step and does not revert it.
func TestAutoscaleTimeoutHoldsWithAMissingOrTerminatingPod(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		it := newInPlaceTest(t)
		it.pods = it.pods[:1]
		it.check.ExpectedCalls = slices.DeleteFunc(it.check.ExpectedCalls, func(c *mock.Call) bool {
			return c.Method == "GetRedisesIPs" || c.Method == "GetRedisesSlavesPods"
		})
		it.check.On("GetRedisesIPs", it.rf).Return([]string{"10.0.0.1"}, nil)
		it.check.On("GetRedisesSlavesPods", it.rf).Return([]string{}, nil)
		assert.Equal(t, autoscale.PhaseHeld, timeoutStep(t, it).Phase)
	})

	t.Run("terminating", func(t *testing.T) {
		it := newInPlaceTest(t)
		now := metav1.Now()
		it.pods[1].DeletionTimestamp = &now
		assert.Equal(t, autoscale.PhaseHeld, timeoutStep(t, it).Phase)
	})

	t.Run("all pods there", func(t *testing.T) {
		it := newInPlaceTest(t)
		assert.Equal(t, autoscale.PhaseReverted, timeoutStep(t, it).Phase)
	})
}

// A pod that the StatefulSet created from the template of a step after a
// revert keeps its limit. The rollout moves it to the update revision. A
// change of another field keeps it too. The documented user shrinks, a
// memoryMax below the limit of the pod or mode Off, take the user path.
func TestUpdateRedisesPodsKeepsAPodOfARevertedStep(t *testing.T) {
	tests := map[string]struct {
		change func(rf *redisfailoverv1.RedisFailover)
		kept   bool
	}{
		"reverted step":           {change: func(*redisfailoverv1.RedisFailover) {}, kept: true},
		"change of another field": {change: func(rf *redisfailoverv1.RedisFailover) { rf.Generation = 7 }, kept: true},
		"memoryMax below the step": {change: func(rf *redisfailoverv1.RedisFailover) {
			rf.Spec.Redis.Autoscaling.MemoryMax = new(resource.MustParse("1536Mi"))
		}},
		"mode Off": {change: func(rf *redisfailoverv1.RedisFailover) {
			rf.Spec.Redis.Autoscaling.Mode = redisfailoverv1.AutoscalingModeOff
		}},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			it := newInPlaceTest(t)
			it.rf.Spec.Redis.Resources = memoryLimit("1Gi")
			it.setUpdateRevision("old")
			p := &it.pods[1]
			p.Labels[appsv1.ControllerRevisionHashLabelKey] = "new"
			p.Spec.Containers[0].Resources = memoryLimit("2Gi")
			applied := memoryLimit("2Gi")
			p.Status.ContainerStatuses[0].Resources = &applied
			reverted := stepTo2Gi()
			reverted.Phase, reverted.Effective = autoscale.PhaseReverted, 0
			h := it.handler(reverted)
			h.autoscaleReads.Store(failoverKey(it.rf), autoscaleRead{state: reverted, spec: 1 << 30, effective: 1 << 30})
			mh := &recordingHealer{RedisFailoverHeal: it.healer}
			h.rfHealer = mh
			test.change(it.rf)

			require.NoError(t, h.UpdateRedisesPods(it.rf))

			if test.kept {
				assert.Empty(t, mh.deleted)
				assert.Equal(t, "old", p.Labels[appsv1.ControllerRevisionHashLabelKey])
				assert.Equal(t, "2Gi", p.Spec.Containers[0].Resources.Limits.Memory().String())
				return
			}
			// Kubernetes 1.33 and 1.34 cannot lower the limit in place.
			assert.Equal(t, []string{"rfr-test-1"}, mh.deleted)
		})
	}
}

// The limit of the last step goes to the rollout in each phase and mode with
// autoscaling. It must be above the spec limit and not above memoryMax.
func TestAutoscalerLimitsStep(t *testing.T) {
	h := NewRedisFailoverHandler(Config{}, nil, nil, nil, nil, metrics.Dummy, log.Dummy)
	tests := map[string]struct {
		mode, spec, max, phase string
		want                   int64
	}{
		"Reverted":                 {"Auto", "1Gi", "4Gi", autoscale.PhaseReverted, 2 << 30},
		"Idle":                     {"Auto", "1Gi", "4Gi", autoscale.PhaseIdle, 2 << 30},
		"Done":                     {"Auto", "1Gi", "4Gi", autoscale.PhaseDone, 2 << 30},
		"Recommend":                {"Recommend", "1Gi", "4Gi", autoscale.PhaseReverted, 2 << 30},
		"memoryMax at the step":    {"Auto", "1Gi", "2Gi", autoscale.PhaseReverted, 2 << 30},
		"memoryMax below the step": {"Auto", "1Gi", "1536Mi", autoscale.PhaseReverted, 0},
		"spec at the step":         {"Auto", "2Gi", "4Gi", autoscale.PhaseReverted, 0},
		"Off":                      {"Off", "1Gi", "4Gi", autoscale.PhaseReverted, 0},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			rf := autoscaleRolloutRF(test.mode)
			rf.Generation = 9
			rf.Spec.Redis.Autoscaling.MemoryMax = new(resource.MustParse(test.max))
			spec := resource.MustParse(test.spec)
			s := autoscale.State{Phase: test.phase, To: 2 << 30, Generation: 1}
			h.autoscaleReads.Store(failoverKey(rf), autoscaleRead{state: s, spec: spec.Value(), effective: spec.Value()})
			assert.Equal(t, rfservice.AutoscalerLimits{Step: test.want}, h.autoscalerLimits(rf))
		})
	}
}

// recordingHealer records DeletePod and passes the other calls on.
type recordingHealer struct {
	rfservice.RedisFailoverHeal
	deleted []string
}

func (h *recordingHealer) DeletePod(pod string, _ *redisfailoverv1.RedisFailover) error {
	h.deleted = append(h.deleted, pod)
	return nil
}

// A pod on the update revision that keeps the resize request, after a failed
// removal, loses it, so that it does not hold each later step.
func TestUpdateRedisesPodsClearsALeftoverResizeRequest(t *testing.T) {
	it := newInPlaceTest(t)
	for i := range it.pods {
		p := &it.pods[i]
		p.Labels[appsv1.ControllerRevisionHashLabelKey] = "new"
		p.Spec.Containers[0].Resources = memoryLimit("2Gi")
		applied := memoryLimit("2Gi")
		p.Status.ContainerStatuses[0].Resources = &applied
	}
	it.pods[1].Annotations = map[string]string{"redisfailovers.databases.spotahome.com/resize-requested-at": time.Now().UTC().Format(time.RFC3339)}
	h := it.handler(stepTo2Gi())

	require.NoError(t, h.UpdateRedisesPods(it.rf))

	v, _ := h.autoscaleRollout.Load(failoverKey(it.rf))
	assert.True(t, v.(autoscaleRollout).settled)
	assert.False(t, rfservice.ResizeRequested(&it.pods[1]))

	t.Run("a removal error holds", func(t *testing.T) {
		it := newInPlaceTest(t)
		for i := range it.pods {
			p := &it.pods[i]
			p.Labels[appsv1.ControllerRevisionHashLabelKey] = "new"
			p.Spec.Containers[0].Resources = memoryLimit("2Gi")
			applied := memoryLimit("2Gi")
			p.Status.ContainerStatuses[0].Resources = &applied
		}
		it.pods[1].Annotations = map[string]string{"redisfailovers.databases.spotahome.com/resize-requested-at": "2026-01-01T00:00:00Z"}
		it.k8s.ExpectedCalls = slices.DeleteFunc(it.k8s.ExpectedCalls, func(c *mock.Call) bool { return c.Method == "UpdatePodAnnotations" })
		it.k8s.On("UpdatePodAnnotations", "testns", mock.Anything, mock.Anything).Return(errors.New("denied"))
		h := it.handler(stepTo2Gi())
		require.NoError(t, h.UpdateRedisesPods(it.rf))
		v, _ := h.autoscaleRollout.Load(failoverKey(it.rf))
		assert.False(t, v.(autoscaleRollout).settled)
		assert.Equal(t, []string{"rfr-test-1"}, v.(autoscaleRollout).requested)
	})
}

// A redis container that restarts on a memory resize blocks a step.
func TestRecordAutoscaleRolloutFindsARestartOnResize(t *testing.T) {
	restart := []corev1.ContainerResizePolicy{{ResourceName: corev1.ResourceMemory, RestartPolicy: corev1.RestartContainer}}
	cpu := []corev1.ContainerResizePolicy{{ResourceName: corev1.ResourceCPU, RestartPolicy: corev1.RestartContainer}}
	for name, test := range map[string]struct {
		template, pod []corev1.ContainerResizePolicy
		podName       string
		want          bool
	}{
		"none":              {},
		"in the template":   {template: restart, want: true},
		"in a pod":          {pod: restart, want: true},
		"for cpu":           {template: cpu, pod: cpu},
		"another container": {pod: restart, podName: "exporter"},
	} {
		t.Run(name, func(t *testing.T) {
			rf := autoscaleRolloutRF(redisfailoverv1.AutoscalingModeAuto)
			ss := &appsv1.StatefulSet{Status: appsv1.StatefulSetStatus{UpdateRevision: "new"}}
			ss.Spec.Template.Spec.Containers = []corev1.Container{{Name: "redis", ResizePolicy: test.template}}
			pod := redisPod("rfr-test-0", "new", "2Gi")
			pod.Spec.Containers = []corev1.Container{{Name: cmpOr(test.podName, "redis"), ResizePolicy: test.pod}}
			h, mk := recordTest(rf, ss, pod)
			require.NoError(t, h.recordAutoscaleRollout(rf, false))
			v, _ := h.autoscaleRollout.Load(failoverKey(rf))
			assert.Equal(t, test.want, v.(autoscaleRollout).restartOnResize)
			mk.AssertExpectations(t)
		})
	}
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// In mode Recommend, a missing state and a status without an effective
// limit, for example after a restore of the RedisFailover, keep the limit of
// the live StatefulSet. Each read of the state reads the StatefulSet.
func TestReadAutoscaleReadsTheStatefulSetInRecommend(t *testing.T) {
	at := newAutoscaleTest(t, redisfailoverv1.AutoscalingModeRecommend)
	at.mk.On("GetConfigMap", "testns", autoscaleCM).Return(nil, notFound())
	at.onStatefulSet().Return(redisStatefulSet("1280Mi"), nil)

	for range 2 {
		read, err := at.h.readAutoscale(at.rf)
		require.NoError(t, err)
		assert.Equal(t, 1280*mib, read.effective)
	}
	at.mk.AssertNumberOfCalls(t, "GetStatefulSet", 2)

	t.Run("a read error stops the reconcile", func(t *testing.T) {
		at := newAutoscaleTest(t, redisfailoverv1.AutoscalingModeRecommend)
		at.mk.On("GetConfigMap", "testns", autoscaleCM).Return(nil, notFound())
		at.onStatefulSet().Return(nil, errors.New("timeout"))
		for range 2 {
			_, err := at.h.readAutoscale(at.rf)
			require.Error(t, err)
		}
		at.mk.AssertNumberOfCalls(t, "GetStatefulSet", 2)
	})
}

// The refusal of a resize also shows in the status message, because it holds
// a change of the spec too.
func TestResizeInPlaceShowsTheRefusalInTheMessage(t *testing.T) {
	rt := newStaleRolloutTest(t, "rfr-test-1", false)
	useAutoscaler(rt)
	rt.heal.On("ResizePodInPlace", rt.rf, "rfr-test-1", "new", autoscaleLimits).Once().Return(rfservice.ResizeResult{
		Action: rfservice.ResizeRecreate, Class: rfservice.ResizePermanent, Message: "the pod's revision is not available", AutoscalerRevision: true,
	}, nil)
	assert.Equal(t, "rollout holds pod rfr-test-1: the pod's revision is not available", rt.update(""))
}

// recordTest is a handler whose k8s mock returns ss and pods.
func recordTest(rf *redisfailoverv1.RedisFailover, ss *appsv1.StatefulSet, pods ...corev1.Pod) (*RedisFailoverHandler, *mK8SService.Services) {
	mk := &mK8SService.Services{}
	mk.On("GetStatefulSet", rf.Namespace, "rfr-"+rf.Name).Once().Return(ss, nil)
	mk.On("GetStatefulSetPods", rf.Namespace, "rfr-"+rf.Name).Once().Return(&corev1.PodList{Items: pods}, nil)
	return NewRedisFailoverHandler(Config{}, nil, nil, nil, mk, metrics.Dummy, log.Dummy), mk
}
