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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	mRFService "github.com/saremox/redis-operator/mocks/operator/redisfailover/service"
	mK8SService "github.com/saremox/redis-operator/mocks/service/k8s"
	"github.com/saremox/redis-operator/operator/redisfailover/autoscale"
	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
)

const autoscaleEffective = int64(2 << 30)

var autoscaleLimits = rfservice.AutoscalerLimits{Spec: 1 << 30, Effective: autoscaleEffective}

// useAutoscaler gives rt mode Auto with the effective limit autoscaleEffective.
func useAutoscaler(rt *rolloutTest) {
	rt.rf.Spec.Redis.Autoscaling = &redisfailoverv1.AutoscalingSettings{Mode: redisfailoverv1.AutoscalingModeAuto}
	rt.handler.autoscaleReads.Store(failoverKey(rt.rf), autoscaleRead{spec: 1 << 30, effective: autoscaleEffective})
	rt.k8s.On("GetStatefulSet", "testns", "rfr-test").Maybe().Return(&appsv1.StatefulSet{Status: appsv1.StatefulSetStatus{UpdateRevision: "new"}}, nil)
}

// newStaleRolloutTest is a rolloutTest with synced, ready pods, where only
// the pod stale is on the old revision.
func newStaleRolloutTest(t *testing.T, stale string, sentinel bool) *rolloutTest {
	t.Helper()
	rt := newRolloutTest(t)
	rt.synced = true
	rt.pods[1].Status.Conditions = podReady
	if stale == "rfr-test-1" {
		rt.pods[0].Labels[appsv1.ControllerRevisionHashLabelKey] = "new"
		rt.pods[1].Labels[appsv1.ControllerRevisionHashLabelKey] = "old"
	}
	if sentinel {
		rt.rf.Spec.Sentinel.Enabled = new(true)
	}
	return rt
}

// A permanent refusal of a resize to an autoscaler revision deletes no pod
// and hands no master over. The same refusal of a user revision recreates
// the pod as before.
func TestUpdateRedisesPodsKeepsAPodOfAnAutoscalerRevision(t *testing.T) {
	refusals := map[string]rfservice.ResizeResult{
		"infeasible": {Action: rfservice.ResizeRecreate, Class: rfservice.ResizePermanent, Message: "resize infeasible: no node"},
		"quota":      {Action: rfservice.ResizeWaiting, Class: rfservice.ResizePermanent, Message: "resize of pod is blocked by a ResourceQuota or LimitRange: exceeded quota"},
	}
	paths := map[string]struct {
		pod      string
		sentinel bool
	}{
		"replica":                 {"rfr-test-1", false},
		"master":                  {"rfr-test-0", false},
		"master in Sentinel mode": {"rfr-test-0", true},
	}
	for name, refusal := range refusals {
		for path, p := range paths {
			t.Run(name+", "+path+", autoscaler revision", func(t *testing.T) {
				rt := newStaleRolloutTest(t, p.pod, p.sentinel)
				useAutoscaler(rt)
				result := refusal
				result.AutoscalerRevision = true
				rt.heal.On("ResizePodInPlace", rt.rf, p.pod, "new", autoscaleLimits).Once().Return(result, nil)

				rt.update("")
				rt.heal.AssertExpectations(t)
				rt.heal.AssertNotCalled(t, "DeletePod", mock.Anything, mock.Anything)
				rt.heal.AssertNotCalled(t, "HandOverMaster", mock.Anything, mock.Anything, mock.Anything)
				rt.check.AssertNotCalled(t, "GetBestReplicaForPromotion", mock.Anything)
				rt.check.AssertNotCalled(t, "GetSentinelsIPs", mock.Anything)
				v, ok := rt.handler.autoscaleRollout.Load(failoverKey(rt.rf))
				assert.True(t, ok)
				assert.Equal(t, &resizeRefusal{pod: p.pod, class: rfservice.ResizePermanent, message: refusal.Message}, v.(autoscaleRollout).refusal)
			})

			t.Run(name+", "+path+", user revision", func(t *testing.T) {
				rt := newStaleRolloutTest(t, p.pod, p.sentinel)
				rt.heal.On("ResizePodInPlace", rt.rf, p.pod, "new", rfservice.AutoscalerLimits{}).Once().Return(refusal, nil)
				if refusal.Action == rfservice.ResizeRecreate {
					switch {
					case p.pod == "rfr-test-1" || p.sentinel:
						if p.sentinel {
							rt.check.On("GetSentinelsIPs", rt.rf).Once().Return([]string{"10.0.1.1"}, nil)
							rt.check.On("CheckSentinelSlavesNumberQuorumInMemory", "10.0.1.1", rt.rf).Once().Return(nil)
						}
						rt.heal.On("DeletePod", p.pod, rt.rf).Once().Return(nil)
					default:
						rt.check.On("GetBestReplicaForPromotion", rt.rf).Once().Return(&rfservice.ReplicaInfo{IP: "10.0.0.2", PodName: "rfr-test-1", Synced: true}, nil)
						rt.heal.On("HandOverMaster", "10.0.0.1", "10.0.0.2", rt.rf).Once().Return(rfservice.HandoverDone, nil)
						rt.heal.On("PromoteBestReplica", "10.0.0.2", rt.rf).Once().Return(nil)
					}
				}

				rt.update("")
				rt.heal.AssertExpectations(t)
				if refusal.Action == rfservice.ResizeWaiting {
					rt.heal.AssertNotCalled(t, "DeletePod", mock.Anything, mock.Anything)
				}
				_, ok := rt.handler.autoscaleRollout.Load(failoverKey(rt.rf))
				assert.False(t, ok)
			})
		}
	}
}

// A transient outcome waits, and an outcome of another class recreates the
// pod, also on an autoscaler revision.
func TestUpdateRedisesPodsActsOnOtherClassesOfAnAutoscalerRevision(t *testing.T) {
	rt := newStaleRolloutTest(t, "rfr-test-1", false)
	useAutoscaler(rt)
	rt.heal.On("ResizePodInPlace", rt.rf, "rfr-test-1", "new", autoscaleLimits).Once().Return(rfservice.ResizeResult{Action: rfservice.ResizeWaiting, Class: rfservice.ResizeTransient, Message: "resizing pod rfr-test-1 in place", AutoscalerRevision: true}, nil)
	assert.Equal(t, "resizing pod rfr-test-1 in place", rt.update(""))
	rt.heal.AssertNotCalled(t, "DeletePod", mock.Anything, mock.Anything)

	rt.heal.On("ResizePodInPlace", rt.rf, "rfr-test-1", "new", autoscaleLimits).Once().Return(rfservice.ResizeResult{Action: rfservice.ResizeRecreate, Class: rfservice.ResizeUserRevision}, nil)
	rt.heal.On("DeletePod", "rfr-test-1", rt.rf).Once().Return(nil)
	rt.update("")
	rt.heal.AssertExpectations(t)
	v, _ := rt.handler.autoscaleRollout.Load(failoverKey(rt.rf))
	assert.Nil(t, v.(autoscaleRollout).refusal)
}

func TestAutoscalerLimits(t *testing.T) {
	h := NewRedisFailoverHandler(Config{}, nil, nil, nil, nil, metrics.Dummy, log.Dummy)
	rf := autoscaleRolloutRF(redisfailoverv1.AutoscalingModeAuto)
	assert.Zero(t, h.autoscalerLimits(rf))
	h.autoscaleReads.Store(failoverKey(rf), autoscaleRead{spec: 1 << 30, effective: 1 << 30})
	assert.Zero(t, h.autoscalerLimits(rf))
	h.autoscaleReads.Store(failoverKey(rf), autoscaleRead{spec: 1 << 30, effective: autoscaleEffective})
	assert.Equal(t, autoscaleLimits, h.autoscalerLimits(rf))

	// Without autoscaling, each revision takes the user path.
	for _, mode := range []string{"", redisfailoverv1.AutoscalingModeOff} {
		rf := autoscaleRolloutRF(mode)
		h.autoscaleReads.Store(failoverKey(rf), autoscaleRead{spec: 1 << 30, effective: autoscaleEffective})
		assert.Zero(t, h.autoscalerLimits(rf), mode)
	}

	// A frozen effective limit of mode Recommend can come from a step, also
	// after the loss of the state.
	rf = autoscaleRolloutRF(redisfailoverv1.AutoscalingModeRecommend)
	h.autoscaleReads.Store(failoverKey(rf), autoscaleRead{spec: 1 << 30, effective: autoscaleEffective})
	assert.Equal(t, autoscaleLimits, h.autoscalerLimits(rf))

	// A blocked, lost or frozen state can hide a step, also at the spec
	// limit, and a pod of a step up to memoryMax. A lower spec limit of the
	// user gives the larger pods the user path.
	rf.Spec.Redis.Autoscaling.MemoryMax = new(resource.MustParse("4Gi"))
	maxLimit := rf.Spec.Redis.Autoscaling.MemoryMax.Value()
	for name, read := range map[string]autoscaleRead{
		"blocked": {blocked: autoscale.ReasonNewerState},
		"lost":    {lost: true},
		"frozen":  {state: autoscale.State{Version: autoscale.StateVersion, Frozen: true}},
	} {
		read.spec, read.effective = 1<<30, 1<<30
		h.autoscaleReads.Store(failoverKey(rf), read)
		assert.Equal(t, rfservice.AutoscalerLimits{Effective: 1 << 30, StepMax: maxLimit}, h.autoscalerLimits(rf), name)
		read.userLowered = true
		h.autoscaleReads.Store(failoverKey(rf), read)
		assert.Equal(t, rfservice.AutoscalerLimits{Effective: 1 << 30}, h.autoscalerLimits(rf), name)
	}

	// A spec limit at the target of an unfinished step keeps the step.
	for _, phase := range []string{autoscale.PhaseApplying, autoscale.PhaseHeld} {
		s := autoscale.State{Phase: phase, From: 1 << 30, To: 2 << 30}
		h.autoscaleReads.Store(failoverKey(rf), autoscaleRead{state: s, spec: 2 << 30, effective: 2 << 30})
		assert.Equal(t, rfservice.AutoscalerLimits{Spec: 1 << 30, Effective: 2 << 30}, h.autoscalerLimits(rf), phase)
	}
	for _, phase := range []string{autoscale.PhaseDone, autoscale.PhaseReverted, autoscale.PhaseIdle} {
		s := autoscale.State{Phase: phase, From: 1 << 30, To: 2 << 30}
		h.autoscaleReads.Store(failoverKey(rf), autoscaleRead{state: s, spec: 2 << 30, effective: 2 << 30})
		assert.Zero(t, h.autoscalerLimits(rf), phase)
	}
}

// redisPod is a ready redis pod on revision rev that runs with the memory
// limit applied.
func redisPod(name, rev, applied string) corev1.Pod {
	limits := corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(applied)}
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{appsv1.ControllerRevisionHashLabelKey: rev}},
		Status: corev1.PodStatus{
			Conditions:        podReady,
			ContainerStatuses: []corev1.ContainerStatus{{Name: "redis", Resources: &corev1.ResourceRequirements{Limits: limits}}},
		},
	}
}

func TestRecordAutoscaleRollout(t *testing.T) {
	type want struct{ settled, applied, anyApplied bool }
	pods := func(change func(pods []corev1.Pod) []corev1.Pod) []corev1.Pod {
		p := []corev1.Pod{redisPod("rfr-test-0", "new", "2Gi"), redisPod("rfr-test-1", "new", "2Gi")}
		if change != nil {
			p = change(p)
		}
		return p
	}
	refusal := &resizeRefusal{pod: "rfr-test-1", class: rfservice.ResizePermanent, message: "resize infeasible"}
	tests := map[string]struct {
		pods     []corev1.Pod
		waits    bool
		observed int64
		refusal  *resizeRefusal
		want     want
	}{
		"settled": {pods: pods(nil), observed: 2, want: want{true, true, true}},
		"a larger pod": {pods: pods(func(p []corev1.Pod) []corev1.Pod {
			p[0] = redisPod("rfr-test-0", "new", "3Gi")
			return p
		}), observed: 2, want: want{true, true, true}},
		"a wait":                   {pods: pods(nil), waits: true, observed: 2, want: want{false, true, true}},
		"template not observed":    {pods: pods(nil), observed: 1, want: want{false, false, true}},
		"a refusal":                {pods: pods(nil), observed: 2, refusal: refusal, want: want{false, true, true}},
		"a pod missing":            {pods: pods(func(p []corev1.Pod) []corev1.Pod { return p[:1] }), observed: 2, want: want{false, false, true}},
		"a stale pod, not applied": {pods: pods(func(p []corev1.Pod) []corev1.Pod { p[1] = redisPod("rfr-test-1", "old", "1Gi"); return p }), observed: 2, want: want{false, false, true}},
		"a stale pod, applied": {pods: pods(func(p []corev1.Pod) []corev1.Pod {
			p[1].Labels[appsv1.ControllerRevisionHashLabelKey] = "old"
			return p
		}), observed: 2, want: want{false, false, true}},
		"a pod of the step that starts": {pods: pods(func(p []corev1.Pod) []corev1.Pod {
			p[0] = redisPod("rfr-test-0", "old", "1Gi")
			p[1] = startingPod("new", "2Gi")
			return p
		}), observed: 2, want: want{false, false, true}},
		"a pod of the step that no node can hold": {pods: pods(func(p []corev1.Pod) []corev1.Pod {
			p[0] = redisPod("rfr-test-0", "old", "1Gi")
			p[1] = startingPod("new", "2Gi")
			p[1].Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable}}
			return p
		}), observed: 2, want: want{false, false, false}},
		"a pod of the template before that starts": {pods: pods(func(p []corev1.Pod) []corev1.Pod {
			p[0] = redisPod("rfr-test-0", "old", "1Gi")
			p[1] = startingPod("old", "1Gi")
			return p
		}), observed: 2, want: want{false, false, false}},
		"no pod applied": {pods: pods(func(_ []corev1.Pod) []corev1.Pod {
			return []corev1.Pod{redisPod("rfr-test-0", "old", "1Gi"), redisPod("rfr-test-1", "old", "1Gi")}
		}), observed: 2, want: want{false, false, false}},
		"a pod not ready":   {pods: pods(func(p []corev1.Pod) []corev1.Pod { p[1].Status.Conditions = nil; return p }), observed: 2, want: want{false, true, true}},
		"a terminating pod": {pods: pods(func(p []corev1.Pod) []corev1.Pod { p[1].DeletionTimestamp = &metav1.Time{Time: time.Now()}; return p }), observed: 2, want: want{false, false, true}},
		"a resize not applied yet": {pods: pods(func(p []corev1.Pod) []corev1.Pod {
			p[1] = redisPod("rfr-test-1", "old", "1Gi")
			p[1].Annotations = map[string]string{"redisfailovers.databases.spotahome.com/resize-requested-at": "2026-01-01T00:00:00Z"}
			return p
		}), observed: 2, want: want{false, false, true}},
		"a pending pod of the update": {pods: pods(func(p []corev1.Pod) []corev1.Pod { p[1].Status = corev1.PodStatus{Phase: corev1.PodPending}; return p }), observed: 2, want: want{false, false, true}},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			rf := autoscaleRolloutRF(redisfailoverv1.AutoscalingModeAuto)
			mk := &mK8SService.Services{}
			mk.On("GetStatefulSet", "testns", "rfr-test").Once().Return(&appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{Generation: 2},
				Status:     appsv1.StatefulSetStatus{ObservedGeneration: test.observed, UpdateRevision: "new"},
			}, nil)
			mk.On("GetStatefulSetPods", "testns", "rfr-test").Once().Return(&corev1.PodList{Items: test.pods}, nil)
			h := NewRedisFailoverHandler(Config{}, nil, nil, nil, mk, metrics.Dummy, log.Dummy)
			if test.refusal != nil {
				h.autoscaleRollout.Store(failoverKey(rf), autoscaleRollout{refusal: test.refusal})
			}

			require.NoError(t, h.recordAutoscaleRollout(rf, test.waits))
			mk.AssertExpectations(t)
			v, _ := h.autoscaleRollout.Load(failoverKey(rf))
			got := v.(autoscaleRollout)
			assert.Equal(t, test.want, want{got.settled, got.applied, got.anyApplied})
			assert.Equal(t, test.refusal, got.refusal)
		})
	}
}

// startingPod is a Pending redis pod on revision rev with the memory limit
// requested.
func startingPod(rev, limit string) corev1.Pod {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "rfr-test-1", Labels: map[string]string{appsv1.ControllerRevisionHashLabelKey: rev}},
		Status:     corev1.PodStatus{Phase: corev1.PodPending},
	}
	pod.Spec.Containers = []corev1.Container{{Name: "redis", Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(limit)}}}}
	return pod
}

func autoscaleRolloutRF(mode string) *redisfailoverv1.RedisFailover {
	rf := &redisfailoverv1.RedisFailover{ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "testns"}}
	rf.Spec.Redis.Replicas = 2
	rf.Spec.Redis.Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")}
	if mode != "" {
		rf.Spec.Redis.Autoscaling = &redisfailoverv1.AutoscalingSettings{Mode: mode}
	}
	return rf
}

// Only mode Auto reads the StatefulSet and the pods for the rollout result.
func TestRecordAutoscaleRolloutOnlyInModeAuto(t *testing.T) {
	for _, mode := range []string{"", redisfailoverv1.AutoscalingModeOff, redisfailoverv1.AutoscalingModeRecommend} {
		t.Run(mode, func(t *testing.T) {
			rf := autoscaleRolloutRF(mode)
			h := NewRedisFailoverHandler(Config{}, nil, nil, nil, &mK8SService.Services{}, metrics.Dummy, log.Dummy)
			require.NoError(t, h.recordAutoscaleRollout(rf, false))
			_, ok := h.autoscaleRollout.Load(failoverKey(rf))
			assert.False(t, ok)
		})
	}

	fail := errors.New("boom")
	for name, setup := range map[string]func(mk *mK8SService.Services){
		"StatefulSet": func(mk *mK8SService.Services) {
			mk.On("GetStatefulSet", "testns", "rfr-test").Return(nil, fail)
		},
		"pods": func(mk *mK8SService.Services) {
			mk.On("GetStatefulSet", "testns", "rfr-test").Return(&appsv1.StatefulSet{}, nil)
			mk.On("GetStatefulSetPods", "testns", "rfr-test").Return(nil, fail)
		},
	} {
		t.Run("error reading the "+name, func(t *testing.T) {
			mk := &mK8SService.Services{}
			setup(mk)
			h := NewRedisFailoverHandler(Config{}, nil, nil, nil, mk, metrics.Dummy, log.Dummy)
			assert.ErrorIs(t, h.recordAutoscaleRollout(autoscaleRolloutRF(redisfailoverv1.AutoscalingModeAuto), false), fail)
		})
	}
}

// UpdateRedisesPods sets the rollout result in each call, also after a
// refusal, and fails when it cannot read it.
func TestUpdateRedisesPodsRecordsTheAutoscaleRollout(t *testing.T) {
	rt := newStaleRolloutTest(t, "rfr-test-1", false)
	rt.rf.Spec.Redis.Autoscaling = &redisfailoverv1.AutoscalingSettings{Mode: redisfailoverv1.AutoscalingModeAuto}
	rt.rf.Spec.Redis.Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: *resource.NewQuantity(autoscaleEffective, resource.BinarySI)}
	rt.handler.autoscaleReads.Store(failoverKey(rt.rf), autoscaleRead{spec: 1 << 30, effective: autoscaleEffective})
	ss := &appsv1.StatefulSet{Status: appsv1.StatefulSetStatus{UpdateRevision: "new"}}
	var ssErr error
	rt.k8s.On("GetStatefulSet", "testns", "rfr-test").Return(func(string, string) (*appsv1.StatefulSet, error) { return ss, ssErr })
	rt.pods[0] = redisPod("rfr-test-0", "new", "2Gi")
	rt.pods[0].Status.PodIP = "10.0.0.1"
	rt.pods[1].Labels[appsv1.ControllerRevisionHashLabelKey] = "old"
	rt.heal.On("ResizePodInPlace", rt.rf, "rfr-test-1", "new", autoscaleLimits).Once().Return(rfservice.ResizeResult{Action: rfservice.ResizeRecreate, Class: rfservice.ResizePermanent, Message: "resize infeasible", AutoscalerRevision: true}, nil)

	rt.update("")
	v, _ := rt.handler.autoscaleRollout.Load(failoverKey(rt.rf))
	assert.Equal(t, autoscaleRollout{anyApplied: true, refusal: &resizeRefusal{pod: "rfr-test-1", class: rfservice.ResizePermanent, message: "resize infeasible"}, complete: true, updateRevision: "new"}, v)

	// The next reconcile starts without the refusal.
	rt.handler.autoscaleRollout.Delete(failoverKey(rt.rf))
	rt.pods[1] = redisPod("rfr-test-1", "new", "2Gi")
	rt.pods[1].Status.PodIP = "10.0.0.2"
	rt.update("")
	v, _ = rt.handler.autoscaleRollout.Load(failoverKey(rt.rf))
	assert.Equal(t, autoscaleRollout{settled: true, applied: true, anyApplied: true, complete: true, updateRevision: "new"}, v)

	ssErr = errors.New("boom")
	rt.handler.autoscaleRollout.Delete(failoverKey(rt.rf))
	rt.rf.Status = redisfailoverv1.RedisFailoverStatus{}
	require.ErrorIs(t, rt.handler.UpdateRedisesPods(rt.rf), ssErr)
	_, ok := rt.handler.autoscaleRollout.Load(failoverKey(rt.rf))
	assert.False(t, ok)
}

// The mocks fail a call that the test does not expect. In mode Recommend
// with the spec limit, the rollout takes the user path and makes the API
// calls of an instance without autoscaling.
func TestUpdateRedisesPodsInModeRecommend(t *testing.T) {
	rt := newStaleRolloutTest(t, "rfr-test-1", false)
	rt.rf.Spec.Redis.Autoscaling = &redisfailoverv1.AutoscalingSettings{Mode: redisfailoverv1.AutoscalingModeRecommend}
	rt.handler.autoscaleReads.Store(failoverKey(rt.rf), autoscaleRead{spec: 1 << 30, effective: 1 << 30})
	rt.heal.On("ResizePodInPlace", rt.rf, "rfr-test-1", "new", rfservice.AutoscalerLimits{}).Once().Return(rfservice.ResizeResult{Action: rfservice.ResizeRecreate, Class: rfservice.ResizePermanent}, nil)
	rt.heal.On("DeletePod", "rfr-test-1", rt.rf).Once().Return(nil)
	rt.update("")
	rt.heal.AssertExpectations(t)
	_, ok := rt.handler.autoscaleRollout.Load(failoverKey(rt.rf))
	assert.False(t, ok)
}

// In mode Recommend, a frozen effective limit can come from a step, also
// after the loss of the state. A pod that refuses it keeps its revision.
func TestUpdateRedisesPodsInModeRecommendKeepsAFrozenStep(t *testing.T) {
	rt := newStaleRolloutTest(t, "rfr-test-1", false)
	rt.rf.Spec.Redis.Autoscaling = &redisfailoverv1.AutoscalingSettings{Mode: redisfailoverv1.AutoscalingModeRecommend}
	rt.handler.autoscaleReads.Store(failoverKey(rt.rf), autoscaleRead{spec: 1 << 30, effective: autoscaleEffective})
	rt.heal.On("ResizePodInPlace", rt.rf, "rfr-test-1", "new", autoscaleLimits).Once().Return(rfservice.ResizeResult{
		Action: rfservice.ResizeRecreate, Class: rfservice.ResizePermanent, Message: "resize infeasible", AutoscalerRevision: true,
	}, nil)
	rt.update("")
	rt.heal.AssertExpectations(t)
	rt.heal.AssertNotCalled(t, "DeletePod", mock.Anything, mock.Anything)
	assert.Equal(t, "rollout holds pod rfr-test-1: resize infeasible", rt.rf.Status.Message)
}

// A reconcile that ends before UpdateRedisesPods leaves no rollout result of
// an earlier reconcile.
func TestCheckAndHealDropsTheAutoscaleRollout(t *testing.T) {
	rf := autoscaleRolloutRF("")
	rf.Spec.Auth.SecretPath = "auth"
	fail := errors.New("boom")
	mk := &mK8SService.Services{}
	mk.On("GetSecret", "testns", "auth").Return(nil, fail)
	mk.On("UpdateRedisFailoverStatus", mock.Anything, "testns", rf, mock.Anything).Maybe().Return()
	h := NewRedisFailoverHandler(Config{}, nil, nil, nil, mk, metrics.Dummy, log.Dummy)
	h.autoscaleRollout.Store(failoverKey(rf), autoscaleRollout{settled: true})

	require.ErrorIs(t, h.CheckAndHeal(rf), fail)
	_, ok := h.autoscaleRollout.Load(failoverKey(rf))
	assert.False(t, ok)
}

// The rollout result holds what a revert and the checks before a step need.
func TestRecordAutoscaleRolloutForTheStep(t *testing.T) {
	rf := autoscaleRolloutRF(redisfailoverv1.AutoscalingModeAuto)
	pods := []corev1.Pod{redisPod("rfr-test-0", "new", "2Gi"), redisPod("rfr-test-1", "new", "1Gi")}
	pods[1].Annotations = map[string]string{"redisfailovers.databases.spotahome.com/resize-requested-at": "2026-01-01T00:00:00Z"}
	pods[0].Annotations = map[string]string{"vpaObservedContainers": "redis"}
	mk := &mK8SService.Services{}
	mk.On("GetStatefulSet", "testns", "rfr-test").Return(&appsv1.StatefulSet{Status: appsv1.StatefulSetStatus{UpdateRevision: "new"}}, nil)
	mk.On("GetStatefulSetPods", "testns", "rfr-test").Return(&corev1.PodList{Items: pods}, nil)
	h := NewRedisFailoverHandler(Config{}, nil, nil, nil, mk, metrics.Dummy, log.Dummy)

	require.NoError(t, h.recordAutoscaleRollout(rf, false))

	v, _ := h.autoscaleRollout.Load(failoverKey(rf))
	got := v.(autoscaleRollout)
	assert.True(t, got.complete)
	assert.Equal(t, []string{"rfr-test-1"}, got.requested)
	assert.True(t, got.vpa)
	assert.Equal(t, "new", got.updateRevision)

	pods[0].Annotations = map[string]string{"vpaUpdates": "Pod resources updated by redis: container 0: memory request"}
	require.NoError(t, h.recordAutoscaleRollout(rf, false))
	v, _ = h.autoscaleRollout.Load(failoverKey(rf))
	assert.True(t, v.(autoscaleRollout).vpa)

	pods[0].Annotations = nil
	require.NoError(t, h.recordAutoscaleRollout(rf, false))
	v, _ = h.autoscaleRollout.Load(failoverKey(rf))
	assert.False(t, v.(autoscaleRollout).vpa)
}

// A replica that is not synced holds a step, also without a pending rollout.
func TestUpdateRedisesPodsRecordsAnUnsyncedReplica(t *testing.T) {
	rt := newRolloutTest(t)
	useAutoscaler(rt)
	rt.rf.Spec.Redis.Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: *resource.NewQuantity(autoscaleEffective, resource.BinarySI)}
	rt.pods[0] = redisPod("rfr-test-0", "new", "2Gi")
	rt.pods[0].Status.PodIP = "10.0.0.1"
	rt.pods[1] = redisPod("rfr-test-1", "new", "2Gi")
	rt.pods[1].Status.PodIP = "10.0.0.2"

	rt.update("")
	v, _ := rt.handler.autoscaleRollout.Load(failoverKey(rt.rf))
	got := v.(autoscaleRollout)
	assert.True(t, got.complete)
	assert.False(t, got.settled)
	assert.True(t, got.applied)

	rt.synced = true
	rt.handler.autoscaleRollout.Delete(failoverKey(rt.rf))
	rt.update("")
	v, _ = rt.handler.autoscaleRollout.Load(failoverKey(rt.rf))
	assert.True(t, v.(autoscaleRollout).settled)
}

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

// stuckStepTest is an inPlaceTest where the replica rfr-test-1 requests the
// 2Gi of the step on the revision "old", and the node cannot hold it.
func stuckStepTest(t *testing.T, mode string) *inPlaceTest {
	t.Helper()
	it := newInPlaceTest(t)
	it.rf.Spec.Redis.Autoscaling.Mode = mode
	pod := &it.pods[1]
	pod.Spec.Containers[0].Resources = memoryLimit("2Gi")
	pod.Annotations = map[string]string{"redisfailovers.databases.spotahome.com/resize-requested-at": "2026-01-01T00:00:00Z"}
	pod.Status.Conditions = append(pod.Status.Conditions, corev1.PodCondition{
		Type: corev1.PodResizePending, Status: corev1.ConditionTrue, Reason: corev1.PodReasonInfeasible, Message: "Node didn't have enough capacity",
	})
	// The StatefulSet has the template of the step.
	it.k8s.ExpectedCalls = slices.DeleteFunc(it.k8s.ExpectedCalls, func(c *mock.Call) bool { return c.Method == "GetStatefulSet" })
	it.k8s.On("GetStatefulSet", "testns", "rfr-test").Return(func(string, string) (*appsv1.StatefulSet, error) {
		ss := &appsv1.StatefulSet{Status: appsv1.StatefulSetStatus{UpdateRevision: it.updated}}
		ss.Spec.Template.Spec.Containers = []corev1.Container{{Name: "redis", Resources: memoryLimit("2Gi")}}
		return ss, nil
	})
	return it
}

// rollout runs UpdateRedisesPods with the state that readAutoscale reads,
// as Handle does.
func (it *inPlaceTest) rollout(t *testing.T) *RedisFailoverHandler {
	t.Helper()
	h := NewRedisFailoverHandler(Config{}, &mRFService.RedisFailoverClient{}, it.check, it.healer, it.k8s, metrics.Dummy, log.Dummy)
	read, err := h.readAutoscale(it.rf)
	require.NoError(t, err)
	h.autoscaleReads.Store(failoverKey(it.rf), read)
	require.NoError(t, h.UpdateRedisesPods(it.rf))
	return h
}

func (it *inPlaceTest) assertKept(t *testing.T) {
	t.Helper()
	it.k8s.AssertNotCalled(t, "DeletePod", mock.Anything, mock.Anything)
	it.k8s.AssertNotCalled(t, "DeletePodWithUID", mock.Anything, mock.Anything, mock.Anything)
	assert.Equal(t, "rollout holds pod rfr-test-1: resize infeasible: Node didn't have enough capacity", it.rf.Status.Message)
}

func stateConfigMap(it *inPlaceTest, h *RedisFailoverHandler, data string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "rfa-test", ResourceVersion: "1", OwnerReferences: h.createOwnerReferences(it.rf)},
		Data:       map[string]string{"state": data},
	}
}

// Mode Recommend completes a step of mode Auto. A lost state does not end
// the protection of the pod that refuses the step.
func TestRolloutKeepsAStuckStepPodAfterTheLossOfTheState(t *testing.T) {
	for _, mode := range []string{redisfailoverv1.AutoscalingModeRecommend, redisfailoverv1.AutoscalingModeAuto} {
		t.Run(mode, func(t *testing.T) {
			it := stuckStepTest(t, mode)
			it.rf.Spec.Redis.Resources = memoryLimit("1Gi")
			it.rf.Status.Autoscaling = redisfailoverv1.AutoscalingStatus{EffectiveMemory: "2Gi", Phase: autoscale.PhaseApplying}
			it.k8s.On("GetConfigMap", "testns", "rfa-test").Return(nil, apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, "rfa-test"))

			it.rollout(t)

			it.assertKept(t)
		})
	}
}

// A raise of the spec between the limit before the step and the limit of the
// step does not change the template, so it does not recreate the pod.
func TestRolloutKeepsAStuckStepPodAfterARaiseOfTheSpec(t *testing.T) {
	for _, phase := range []string{autoscale.PhaseApplying, autoscale.PhaseHeld} {
		t.Run(phase, func(t *testing.T) {
			it := stuckStepTest(t, redisfailoverv1.AutoscalingModeAuto)
			it.rf.Spec.Redis.Resources = memoryLimit("1536Mi")
			s := stepTo2Gi()
			s.Phase = phase
			h := NewRedisFailoverHandler(Config{}, nil, nil, nil, nil, metrics.Dummy, log.Dummy)
			it.k8s.On("GetConfigMap", "testns", "rfa-test").Return(stateConfigMap(it, h, encodeState(t, s)), nil)

			it.rollout(t)

			it.assertKept(t)
		})
	}
}

// A spec limit at the limit of the step, as in the procedure to pin the
// limit, does not change the template, so it does not recreate the pod.
func TestRolloutKeepsAStuckStepPodWithTheSpecAtTheStep(t *testing.T) {
	for _, phase := range []string{autoscale.PhaseApplying, autoscale.PhaseHeld} {
		t.Run(phase, func(t *testing.T) {
			it := stuckStepTest(t, redisfailoverv1.AutoscalingModeAuto)
			s := stepTo2Gi()
			s.Phase = phase
			h := NewRedisFailoverHandler(Config{}, nil, nil, nil, nil, metrics.Dummy, log.Dummy)
			it.k8s.On("GetConfigMap", "testns", "rfa-test").Return(stateConfigMap(it, h, encodeState(t, s)), nil)

			it.rollout(t)

			it.assertKept(t)
		})
	}
}

// A state that is not valid hides a step at the spec limit, so the pod stays.
func TestRolloutKeepsAStuckStepPodAfterAnInvalidState(t *testing.T) {
	it := stuckStepTest(t, redisfailoverv1.AutoscalingModeAuto)
	h := NewRedisFailoverHandler(Config{}, nil, nil, nil, nil, metrics.Dummy, log.Dummy)
	it.k8s.On("GetConfigMap", "testns", "rfa-test").Return(stateConfigMap(it, h, "{not json"), nil)

	it.rollout(t)

	it.assertKept(t)
}
