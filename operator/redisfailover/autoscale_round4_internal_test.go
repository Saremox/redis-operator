package redisfailover

import (
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/operator/redisfailover/autoscale"
	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
	"github.com/saremox/redis-operator/service/k8s"
)

// ensuredStatefulSet is a redis StatefulSet that Ensure wrote with the spec
// limit spec and the template limit limit.
func ensuredStatefulSet(limit, spec string) *appsv1.StatefulSet {
	ss := redisStatefulSet(limit)
	ss.Annotations = map[string]string{rfservice.SpecMemoryLimitAnnotation: spec}
	return ss
}

// The limit of the live template is a floor. Only a change of the spec limit
// since the last Ensure, a lower memoryMax, or the revert of the step of the
// template gives a lower limit. A state below the floor is lost.
func TestReadAutoscaleKeepsTheLiveLimit(t *testing.T) {
	done := autoscale.State{Version: autoscale.StateVersion, Phase: autoscale.PhaseDone, From: 1024 * mib, To: 1280 * mib, Effective: 1280 * mib, SpecLimit: 1024 * mib, LastSample: autoscaleT0.Unix()}
	before := autoscale.State{Version: autoscale.StateVersion, Phase: autoscale.PhaseIdle, LastSample: autoscaleT0.Unix()}
	reverted := done
	reverted.Phase, reverted.Effective = autoscale.PhaseReverted, 0
	tests := map[string]struct {
		state     autoscale.State
		ss        *appsv1.StatefulSet
		max, spec string
		effective int64
		lost      bool
	}{
		"the state of the template":        {state: done, ss: ensuredStatefulSet("1280Mi", "1Gi"), effective: 1280 * mib},
		"a restored state before the step": {state: before, ss: ensuredStatefulSet("1280Mi", "1Gi"), effective: 1280 * mib, lost: true},
		"a lower spec limit":               {state: before, ss: ensuredStatefulSet("1280Mi", "1280Mi"), effective: 1024 * mib},
		"a higher spec limit":              {state: before, ss: ensuredStatefulSet("1536Mi", "768Mi"), effective: 1536 * mib, lost: true},
		"a lower spec limit below a step":  {state: before, ss: ensuredStatefulSet("1536Mi", "1280Mi"), effective: 1536 * mib, lost: true},
		"a lower spec limit at a step":     {state: before, ss: ensuredStatefulSet("1280Mi", "1280Mi"), spec: "768Mi", effective: 768 * mib},
		"a template without the spec":      {state: before, ss: redisStatefulSet("1280Mi"), effective: 1280 * mib, lost: true},
		"the revert of the template step":  {state: reverted, ss: ensuredStatefulSet("1280Mi", "1Gi"), effective: 1024 * mib},
		"a revert of another step":         {state: func() autoscale.State { s := reverted; s.To = 1536 * mib; return s }(), ss: ensuredStatefulSet("1280Mi", "1Gi"), effective: 1280 * mib, lost: true},
		"a lower memoryMax":                {state: before, ss: ensuredStatefulSet("1280Mi", "1Gi"), max: "1152Mi", effective: 1152 * mib, lost: true},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			at := newAutoscaleTest(t, redisfailoverv1.AutoscalingModeRecommend)
			at.rf.Spec.Redis.Autoscaling.Mode = redisfailoverv1.AutoscalingModeAuto
			if test.max != "" {
				at.rf.Spec.Redis.Autoscaling.MemoryMax = ptrQuantity(test.max)
			}
			if test.spec != "" {
				at.rf.Spec.Redis.Resources.Limits[corev1.ResourceMemory] = resource.MustParse(test.spec)
			}
			at.mk.On("GetConfigMap", "testns", autoscaleCM).Return(at.stateConfigMap(encodeState(t, test.state)), nil)
			at.mk.On("GetStatefulSetPods", "testns", "rfr-test").Maybe().Return(&corev1.PodList{}, nil)
			at.onStatefulSet().Return(test.ss, nil)

			read, err := at.h.readAutoscale(at.rf)

			require.NoError(t, err)
			assert.Equal(t, test.effective, read.effective)
			assert.Equal(t, test.lost, read.lost)
			if test.lost {
				assert.Zero(t, read.state.Version, "a state below the floor is lost")
			}
		})
	}
}

func ptrQuantity(q string) *resource.Quantity {
	v := resource.MustParse(q)
	return &v
}

// A restored older state freezes the live limit, and a step waits for a
// change of the spec.
func TestAutoscaleFreezesARestoredOlderState(t *testing.T) {
	at := newAutoscaleTest(t, redisfailoverv1.AutoscalingModeRecommend)
	at.rf.Spec.Redis.Autoscaling.Mode = redisfailoverv1.AutoscalingModeAuto
	f := at.fakeStateStore(t)
	f.stored = at.stateConfigMap(encodeState(t, autoscale.State{Version: autoscale.StateVersion, Phase: autoscale.PhaseIdle, LastSample: autoscaleT0.Unix()}))
	at.onStatefulSet().Return(ensuredStatefulSet("1280Mi", "1Gi"), nil)
	at.mrfc.On("GetAutoscalerInfo", mock.Anything, autoscaleMaster).Return(masterInfo(500), nil)
	at.mrec.On("SetAutoscaler", "testns", "test", mock.Anything, mock.Anything, mock.Anything, 1280*mib)
	at.now = autoscaleT0.Add(time.Minute)

	rf := at.handle(t, redisfailoverv1.AutoscalingStatus{Phase: autoscale.PhaseDone})

	s := decodeState(t, f.stored)
	assert.True(t, s.Frozen)
	assert.Equal(t, 1280*mib, s.Effective)
	assert.Equal(t, "1280Mi", rf.Status.Autoscaling.EffectiveMemory)
	assert.Equal(t, autoscale.ReasonFrozen, rf.Status.Autoscaling.Reason)
}

// Without the StatefulSet, for example after a delete with
// --cascade=orphan, the largest redis pod of the RedisFailover gives the
// floor.
func TestReadAutoscaleKeepsTheLimitOfOrphanedPods(t *testing.T) {
	at := newAutoscaleTest(t, redisfailoverv1.AutoscalingModeRecommend)
	at.mk.On("GetConfigMap", "testns", autoscaleCM).Return(nil, notFound())
	at.onStatefulSet().Return(nil, apierrors.NewNotFound(schema.GroupResource{Resource: "statefulsets"}, "rfr-test"))
	pod := func(name, rf, limit string) corev1.Pod {
		p := redisPod(name, "old", limit)
		p.Labels["app.kubernetes.io/name"] = rf
		p.Labels["app.kubernetes.io/component"] = "redis"
		p.Labels["app.kubernetes.io/part-of"] = "redis-failover"
		return p
	}
	other := pod("rfr-other-0", "other", "4Gi")
	at.mk.On("ListPods", "testns").Return(&corev1.PodList{Items: []corev1.Pod{pod("rfr-test-0", "test", "1280Mi"), pod("rfr-test-1", "test", "1Gi"), other}}, nil)

	read, err := at.h.readAutoscale(at.rf)

	require.NoError(t, err)
	assert.Equal(t, 1280*mib, read.effective)
	assert.True(t, rfservice.IsRedisPod(at.rf, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: pod("rfr-test-0", "test", "1Gi").Labels}}))
}

// An in-memory sample of a state without a ConfigMap is not used after the
// ConfigMap is lost, because its limit can be from before a step.
func TestAutoscaleUsesNoSampleOfALostState(t *testing.T) {
	at := newAutoscaleTest(t, redisfailoverv1.AutoscalingModeRecommend)
	at.rf.Spec.Redis.Autoscaling.Mode = redisfailoverv1.AutoscalingModeAuto
	f := at.fakeStateStore(t)
	at.onStatefulSet().Return(ensuredStatefulSet("1280Mi", "1Gi"), nil)
	at.mrec.On("SetAutoscaler", "testns", "test", mock.Anything, mock.Anything, mock.Anything, 1280*mib)
	sampled := autoscale.State{Version: autoscale.StateVersion, Phase: autoscale.PhaseIdle, LastSample: autoscaleT0.Unix()}
	at.h.autoscaleSamples.Store(failoverKey(at.rf), autoscaleSample{at: autoscaleT0, state: sampled, sampled: true})

	rf := at.handle(t, redisfailoverv1.AutoscalingStatus{Phase: autoscale.PhaseApplying, EffectiveMemory: "1280Mi"})

	s := decodeState(t, f.stored)
	assert.True(t, s.Frozen)
	assert.Equal(t, 1280*mib, s.Effective)
	assert.Equal(t, "1280Mi", rf.Status.Autoscaling.EffectiveMemory)
}

// A spec limit at the limit of a done step ends the step. A later lower spec
// limit then changes the template, and the pods take the user path.
func TestAutoscalePinEndsTheStep(t *testing.T) {
	at := newAutoscaleTest(t, redisfailoverv1.AutoscalingModeRecommend)
	at.rf.Spec.Redis.Autoscaling.Mode = redisfailoverv1.AutoscalingModeAuto
	f := at.fakeStateStore(t)
	done := autoscale.State{Version: autoscale.StateVersion, Phase: autoscale.PhaseDone, From: 1024 * mib, To: 1280 * mib, Effective: 1280 * mib, SpecLimit: 1024 * mib, DoneAt: autoscaleT0.Add(-time.Hour).Unix(), LastSample: autoscaleT0.Unix()}
	f.stored = at.stateConfigMap(encodeState(t, done))
	at.mrfc.On("GetAutoscalerInfo", mock.Anything, autoscaleMaster).Return(masterInfo(500), nil)
	at.mrec.On("SetAutoscaler", "testns", "test", mock.Anything, mock.Anything, mock.Anything, mock.Anything)

	// The pin.
	at.rf.Generation = 2
	at.rf.Spec.Redis.Resources.Limits[corev1.ResourceMemory] = resource.MustParse("1280Mi")
	at.onStatefulSet().Return(ensuredStatefulSet("1280Mi", "1Gi"), nil)
	at.now = autoscaleT0.Add(time.Minute)
	at.handle(t, redisfailoverv1.AutoscalingStatus{Phase: autoscale.PhaseDone, EffectiveMemory: "1280Mi"})
	s := decodeState(t, f.stored)
	assert.Zero(t, s.To)
	assert.Zero(t, s.Effective)

	// The lower spec limit.
	at.rf.Generation = 3
	at.rf.Spec.Redis.Resources.Limits[corev1.ResourceMemory] = resource.MustParse("1Gi")
	at.onStatefulSet().Return(ensuredStatefulSet("1280Mi", "1280Mi"), nil)
	read, err := at.h.readAutoscale(at.rf)
	require.NoError(t, err)
	assert.Equal(t, 1024*mib, read.effective)
	at.h.autoscaleReads.Store(failoverKey(at.rf), read)
	assert.Zero(t, at.h.autoscalerLimits(at.rf), "no limit keeps the pods of the step")
}

// After a revert, the spec of a pod can still request the step. Before
// Kubernetes 1.35, the API server refuses a lower limit in the pod spec, so a
// user change of the limit keeps the pod, and the rollout does not recreate
// it.
func TestRolloutKeepsAPodThatRequestsTheStepAfterARevert(t *testing.T) {
	it := newInPlaceTest(t)
	it.rf.Spec.Redis.Resources = memoryLimit("1536Mi")
	it.pods[1].Spec.Containers[0].Resources = memoryLimit("2Gi")
	it.k8s.On("GetControllerRevision", "testns", "mid").Return(controllerRevision(t, "mid", "1536Mi"), nil)
	it.setUpdateRevision("mid")
	it.k8s.ExpectedCalls = slices.DeleteFunc(it.k8s.ExpectedCalls, func(c *mock.Call) bool { return c.Method == "PodResizeSupport" })
	it.k8s.On("PodResizeSupport").Return(k8s.PodResizeSupport{Supported: true}, nil)
	s := stepTo2Gi()
	s.Phase, s.Effective, s.RevertApplied, s.Reverts, s.RetryAt = autoscale.PhaseReverted, 0, true, 1, time.Now().Add(time.Hour).Unix()
	h := it.handler(s)
	h.autoscaleReads.Store(failoverKey(it.rf), autoscaleRead{state: s, spec: 1536 << 20, effective: 1536 << 20})

	require.NoError(t, h.UpdateRedisesPods(it.rf))

	it.k8s.AssertNotCalled(t, "ResizePod", mock.Anything, mock.Anything, mock.Anything)
	it.k8s.AssertNotCalled(t, "DeletePod", mock.Anything, mock.Anything)
	it.heal.AssertNotCalled(t, "DeletePod", mock.Anything, mock.Anything)
}

// A change of the spec turns a frozen limit into a held step. Mode Recommend
// then also reads the rollout, so the step ends at once when each pod runs
// with the limit.
func TestAutoscaleSteppingAtTheEndOfAFreeze(t *testing.T) {
	h := NewRedisFailoverHandler(Config{}, nil, nil, nil, nil, nil, nil)
	rf := autoscaleRolloutRF(redisfailoverv1.AutoscalingModeRecommend)
	rf.Generation = 2
	frozen := autoscale.State{Version: autoscale.StateVersion, Frozen: true, Generation: 1, Effective: 1280 * mib}
	h.autoscaleReads.Store(failoverKey(rf), autoscaleRead{state: frozen})
	assert.True(t, h.autoscaleStepping(rf))
	frozen.Generation = 2
	h.autoscaleReads.Store(failoverKey(rf), autoscaleRead{state: frozen})
	assert.False(t, h.autoscaleStepping(rf))
}
