package redisfailover

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	mK8SService "github.com/saremox/redis-operator/mocks/service/k8s"
	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
)

const autoscaleEffective = int64(2 << 30)

// newStaleRolloutTest is a rolloutTest with synced, ready pods, where only
// the pod stale is on the old revision.
func newStaleRolloutTest(t *testing.T, stale string, sentinel bool) *rolloutTest {
	rt := newRolloutTest(t)
	rt.synced = true
	rt.pods[1].Status.Conditions = podReady
	if stale == "rfr-test-1" {
		rt.pods[0].Labels[appsv1.ControllerRevisionHashLabelKey] = "new"
		rt.pods[1].Labels[appsv1.ControllerRevisionHashLabelKey] = "old"
	}
	if sentinel {
		rt.rf.Spec.Sentinel.Enabled = ptr.To(true)
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
				rt.handler.autoscaleReads.Store(failoverKey(rt.rf), autoscaleRead{spec: 1 << 30, effective: autoscaleEffective})
				result := refusal
				result.AutoscalerRevision = true
				rt.heal.On("ResizePodInPlace", rt.rf, p.pod, "new", autoscaleEffective).Once().Return(result, nil)

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
				rt.heal.On("ResizePodInPlace", rt.rf, p.pod, "new", int64(0)).Once().Return(refusal, nil)
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
	rt.handler.autoscaleReads.Store(failoverKey(rt.rf), autoscaleRead{spec: 1 << 30, effective: autoscaleEffective})
	rt.heal.On("ResizePodInPlace", rt.rf, "rfr-test-1", "new", autoscaleEffective).Once().Return(rfservice.ResizeResult{Action: rfservice.ResizeWaiting, Class: rfservice.ResizeTransient, Message: "resizing pod rfr-test-1 in place", AutoscalerRevision: true}, nil)
	assert.Equal(t, "resizing pod rfr-test-1 in place", rt.update(""))
	rt.heal.AssertNotCalled(t, "DeletePod", mock.Anything, mock.Anything)

	rt.heal.On("ResizePodInPlace", rt.rf, "rfr-test-1", "new", autoscaleEffective).Once().Return(rfservice.ResizeResult{Action: rfservice.ResizeRecreate, Class: rfservice.ResizeUserRevision}, nil)
	rt.heal.On("DeletePod", "rfr-test-1", rt.rf).Once().Return(nil)
	rt.update("")
	rt.heal.AssertExpectations(t)
	_, ok := rt.handler.autoscaleRollout.Load(failoverKey(rt.rf))
	assert.False(t, ok)
}

func TestAutoscalerLimit(t *testing.T) {
	h := NewRedisFailoverHandler(Config{}, nil, nil, nil, nil, metrics.Dummy, log.Dummy)
	rf := &redisfailoverv1.RedisFailover{ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "testns"}}
	assert.Zero(t, h.autoscalerLimit(rf))
	h.autoscaleReads.Store(failoverKey(rf), autoscaleRead{spec: 1 << 30, effective: 1 << 30})
	assert.Zero(t, h.autoscalerLimit(rf))
	h.autoscaleReads.Store(failoverKey(rf), autoscaleRead{spec: 1 << 30, effective: autoscaleEffective})
	assert.Equal(t, autoscaleEffective, h.autoscalerLimit(rf))
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
		"template not observed":    {pods: pods(nil), observed: 1, want: want{false, true, true}},
		"a refusal":                {pods: pods(nil), observed: 2, refusal: refusal, want: want{false, true, true}},
		"a pod missing":            {pods: pods(func(p []corev1.Pod) []corev1.Pod { return p[:1] }), observed: 2, want: want{false, false, true}},
		"a stale pod, not applied": {pods: pods(func(p []corev1.Pod) []corev1.Pod { p[1] = redisPod("rfr-test-1", "old", "1Gi"); return p }), observed: 2, want: want{false, false, true}},
		"a stale pod, applied": {pods: pods(func(p []corev1.Pod) []corev1.Pod {
			p[1].Labels[appsv1.ControllerRevisionHashLabelKey] = "old"
			return p
		}), observed: 2, want: want{false, true, true}},
		"no pod applied": {pods: pods(func(p []corev1.Pod) []corev1.Pod {
			return []corev1.Pod{redisPod("rfr-test-0", "old", "1Gi"), redisPod("rfr-test-1", "old", "1Gi")}
		}), observed: 2, want: want{false, false, false}},
		"a pod not ready":   {pods: pods(func(p []corev1.Pod) []corev1.Pod { p[1].Status.Conditions = nil; return p }), observed: 2, want: want{false, true, true}},
		"a terminating pod": {pods: pods(func(p []corev1.Pod) []corev1.Pod { p[1].DeletionTimestamp = &metav1.Time{Time: time.Now()}; return p }), observed: 2, want: want{false, true, true}},
		"a resize not applied yet": {pods: pods(func(p []corev1.Pod) []corev1.Pod {
			p[1].Annotations = map[string]string{"redisfailovers.databases.spotahome.com/resize-requested-at": "2026-01-01T00:00:00Z"}
			return p
		}), observed: 2, want: want{false, true, true}},
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

			assert.NoError(t, h.recordAutoscaleRollout(rf, test.waits))
			mk.AssertExpectations(t)
			v, _ := h.autoscaleRollout.Load(failoverKey(rf))
			got := v.(autoscaleRollout)
			assert.Equal(t, test.want, want{got.settled, got.applied, got.anyApplied})
			assert.Equal(t, test.refusal, got.refusal)
		})
	}
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
			assert.NoError(t, h.recordAutoscaleRollout(rf, false))
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
	rt.heal.On("ResizePodInPlace", rt.rf, "rfr-test-1", "new", autoscaleEffective).Once().Return(rfservice.ResizeResult{Action: rfservice.ResizeRecreate, Class: rfservice.ResizePermanent, Message: "resize infeasible", AutoscalerRevision: true}, nil)

	rt.update("")
	v, _ := rt.handler.autoscaleRollout.Load(failoverKey(rt.rf))
	assert.Equal(t, autoscaleRollout{anyApplied: true, refusal: &resizeRefusal{pod: "rfr-test-1", class: rfservice.ResizePermanent, message: "resize infeasible"}}, v)

	// The next call starts without the refusal.
	rt.pods[1] = redisPod("rfr-test-1", "new", "2Gi")
	rt.pods[1].Status.PodIP = "10.0.0.2"
	rt.update("")
	v, _ = rt.handler.autoscaleRollout.Load(failoverKey(rt.rf))
	assert.Equal(t, autoscaleRollout{settled: true, applied: true, anyApplied: true}, v)

	ssErr = errors.New("boom")
	rt.rf.Status = redisfailoverv1.RedisFailoverStatus{}
	assert.ErrorIs(t, rt.handler.UpdateRedisesPods(rt.rf), ssErr)
	_, ok := rt.handler.autoscaleRollout.Load(failoverKey(rt.rf))
	assert.False(t, ok)
}

// The mocks fail a call that the test does not expect. In mode Recommend with
// the spec limit, the rollout makes the API calls of an instance without
// autoscaling.
func TestUpdateRedisesPodsWithTheSpecLimit(t *testing.T) {
	rt := newStaleRolloutTest(t, "rfr-test-1", false)
	rt.rf.Spec.Redis.Autoscaling = &redisfailoverv1.AutoscalingSettings{Mode: redisfailoverv1.AutoscalingModeRecommend}
	rt.handler.autoscaleReads.Store(failoverKey(rt.rf), autoscaleRead{spec: 1 << 30, effective: 1 << 30})
	rt.heal.On("ResizePodInPlace", rt.rf, "rfr-test-1", "new", int64(0)).Once().Return(rfservice.ResizeResult{Action: rfservice.ResizeRecreate, Class: rfservice.ResizePermanent}, nil)
	rt.heal.On("DeletePod", "rfr-test-1", rt.rf).Once().Return(nil)
	rt.update("")
	rt.heal.AssertExpectations(t)
	_, ok := rt.handler.autoscaleRollout.Load(failoverKey(rt.rf))
	assert.False(t, ok)
}
