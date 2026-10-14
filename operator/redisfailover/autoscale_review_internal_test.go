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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/metrics"
	"github.com/saremox/redis-operator/operator/redisfailover/autoscale"
	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
)

// revertStep runs the autoscaler of h on the rollout result of its last
// UpdateRedisesPods, and returns the state that it writes.
func revertStep(t *testing.T, it *inPlaceTest, h *RedisFailoverHandler) autoscale.State {
	t.Helper()
	var written autoscale.State
	it.k8s.On("UpdateConfigMap", "testns", mock.Anything).Once().Run(func(args mock.Arguments) {
		require.NoError(t, json.Unmarshal([]byte(args.Get(1).(*corev1.ConfigMap).Data["state"]), &written))
	}).Return(nil)
	stored := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "rfa-test", ResourceVersion: "1"}}
	h.autoscaleReads.Store(failoverKey(it.rf), autoscaleRead{state: stepTo2Gi(), cm: stored, spec: 1 << 30, effective: autoscaleEffective})
	it.rf.Status.State = redisfailoverv1.HealthyState
	h.autoscale(it.rf, redisfailoverv1.AutoscalingStatus{})
	require.Equal(t, autoscale.PhaseReverted, written.Phase)
	return written
}

// After a revert, Ensure writes the template before the step. Until the
// StatefulSet controller observes it, the status still names the revision of
// the step, and each pod looks stale. The rollout must wait: the user path
// would recreate the pod that refused the resize, or resize it again.
func TestAutoscaleRevertWithALaggingStatefulSetStatus(t *testing.T) {
	forbidden := apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "rfr-test-1", errors.New("rbac: pods/resize"))
	paths := map[string]struct {
		single, sentinel, succeedsLater bool
	}{
		"replica":                            {},
		"one pod, operator-managed mode":     {single: true},
		"one pod, Sentinel mode":             {single: true, sentinel: true},
		"replica, the resize succeeds later": {succeedsLater: true},
		"one pod, the resize succeeds later": {single: true, succeedsLater: true},
	}
	for name, p := range paths {
		t.Run(name, func(t *testing.T) {
			it := newInPlaceTest(t)
			if p.single {
				it.rf.Spec.Redis.Replicas = 1
				it.pods = it.pods[:1]
				it.check.ExpectedCalls = slices.DeleteFunc(it.check.ExpectedCalls, func(c *mock.Call) bool {
					return c.Method == "GetRedisesIPs" || c.Method == "GetRedisesSlavesPods"
				})
				it.check.On("GetRedisesIPs", it.rf).Return([]string{"10.0.0.1"}, nil)
				it.check.On("GetRedisesSlavesPods", it.rf).Return([]string{}, nil)
			}
			if p.sentinel {
				it.rf.Spec.Sentinel.Enabled = ptr.To(true)
			}
			it.k8s.On("DeletePod", "testns", mock.Anything).Return(nil)
			it.resizeErr = forbidden
			h := it.handler(stepTo2Gi())
			require.NoError(t, h.UpdateRedisesPods(it.rf))
			v, _ := h.autoscaleRollout.Load(failoverKey(it.rf))
			require.NotNil(t, v.(autoscaleRollout).refusal)
			revertStep(t, it, h)

			// The next reconcile has the template before the step, and the
			// status does not show it.
			if p.succeedsLater {
				it.resizeErr = nil
			}
			it.rf.Spec.Redis.Resources = memoryLimit("1Gi")
			it.setLagging()
			reverted := stepTo2Gi()
			reverted.Phase, reverted.Effective = autoscale.PhaseReverted, 0
			h.autoscaleReads.Store(failoverKey(it.rf), autoscaleRead{state: reverted, spec: 1 << 30, effective: 1 << 30})
			for range 2 {
				h.autoscaleRollout.Delete(failoverKey(it.rf))
				require.NoError(t, h.UpdateRedisesPods(it.rf))
				v, _ := h.autoscaleRollout.Load(failoverKey(it.rf))
				assert.False(t, v.(autoscaleRollout).settled)
			}

			// The status shows the template before the step.
			it.setUpdateRevision("old")
			require.NoError(t, h.UpdateRedisesPods(it.rf))

			it.k8s.AssertNumberOfCalls(t, "ResizePod", 1)
			it.k8s.AssertNotCalled(t, "DeletePod", mock.Anything, mock.Anything)
			it.k8s.AssertNotCalled(t, "UpdatePodLabels", mock.Anything, mock.Anything, mock.Anything)
			for _, pod := range it.pods {
				assert.Equal(t, "old", pod.Labels[appsv1.ControllerRevisionHashLabelKey], pod.Name)
				assert.Equal(t, "1Gi", pod.Spec.Containers[0].Resources.Limits.Memory().String(), pod.Name)
			}
		})
	}
}

// The wait also applies to a user rollout, and only while the status lags.
func TestUpdateRedisesPodsWaitsForTheObservedTemplate(t *testing.T) {
	rt := newStaleRolloutTest(t, "rfr-test-1", false)
	rt.check.ExpectedCalls = slices.DeleteFunc(rt.check.ExpectedCalls, func(c *mock.Call) bool { return c.Method == "GetStatefulSetUpdateRevision" })
	rt.check.On("GetStatefulSetUpdateRevision", rt.rf).Return("", rfservice.ErrUpdateRevisionNotObserved)

	rt.update("")
	time.Sleep(2 * rolloutStallTimeout)
	assert.Equal(t, "rollout waiting for more than 0m: "+rfservice.ErrUpdateRevisionNotObserved.Error(), rt.update(""))
	rt.heal.AssertNotCalled(t, "ResizePodInPlace", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	rt.heal.AssertNotCalled(t, "DeletePod", mock.Anything, mock.Anything)

	// A replica that is not synced asks for the revision first.
	rt.synced = false
	rt.pods[1].Labels[appsv1.ControllerRevisionHashLabelKey] = "new"
	rt.update("")
	rt.heal.AssertNotCalled(t, "DeletePod", mock.Anything, mock.Anything)

	fail := errors.New("boom")
	rt.check.ExpectedCalls = slices.DeleteFunc(rt.check.ExpectedCalls, func(c *mock.Call) bool { return c.Method == "GetStatefulSetUpdateRevision" })
	rt.check.On("GetStatefulSetUpdateRevision", rt.rf).Return("", fail)
	assert.ErrorIs(t, rt.handler.UpdateRedisesPods(rt.rf), fail)
}

// A step whose pods did not apply it in 15 minutes is reverted, for example
// when a pod of the step is Pending. The revert clears the resize requests.
func TestAutoscaleTimeoutRevertsAStepThatNoPodApplied(t *testing.T) {
	stored := applying()
	stored.StartedAt = stepNow.Add(-autoscale.ApplyTimeout).Unix()
	st := newStepTest(t, stored)
	st.heal.On("ClearResizeRequested", mock.Anything, "rfr-test-1").Once().Return(nil)
	st.mrec.On("CountAutoscalerStep", "testns", "test", metrics.AutoscalerStepReverted).Once()

	st.reconcile(t, &autoscaleRollout{complete: true, requested: []string{"rfr-test-1"}, updateRevision: "rev1"})

	assert.Equal(t, []string{"Ensure 1280Mi", "write Reverted 0", "requeue testns/test 0s"}, st.calls)
	assert.Equal(t, autoscale.ReasonReverted+": "+autoscale.RefusalTimeout, st.status.Reason)
	st.heal.AssertExpectations(t)

	t.Run("without a complete rollout result: hold", func(t *testing.T) {
		st := newStepTest(t, stored)
		st.mrec.On("CountAutoscalerStep", "testns", "test", metrics.AutoscalerStepHeld).Once()
		st.reconcile(t, nil)
		assert.Equal(t, []string{"Ensure 1280Mi", "write Held 1280Mi"}, st.calls)
	})
}

// A refusal outside a step holds the rollout of a change of the spec, and
// the status tells it.
func TestAutoscaleShowsARefusalInEachPhase(t *testing.T) {
	done := applying()
	done.Phase, done.DoneAt, done.Recommended = autoscale.PhaseDone, stepNow.Add(-time.Hour).Unix(), 0
	st := newStepTest(t, done)
	refused := &autoscaleRollout{complete: true, applied: true, anyApplied: true, updateRevision: "rev2",
		refusal: &resizeRefusal{pod: "rfr-test-1", message: "the pod's revision is not available"}}

	st.reconcile(t, refused)

	assert.Equal(t, autoscale.ReasonResizeRefused+": pod rfr-test-1: the pod's revision is not available", st.status.Reason)
	assert.Equal(t, autoscale.PhaseIdle, st.status.Phase, "the cooldown ended")
	assert.Equal(t, "pod rfr-test-2", (&resizeRefusal{pod: "rfr-test-2"}).String())
}

// A refusal that ends is logged again at Warning when it comes back.
func TestAutoscaleLogsARefusalAgainAfterItEnded(t *testing.T) {
	st := newStepTest(t, recommending())
	refusal := &resizeRefusal{pod: "rfr-test-1", message: "resize infeasible"}
	st.h.logResizeRefusal(st.rf, refusal)
	st.reconcile(t, &autoscaleRollout{complete: true, updateRevision: "rev1"})
	st.h.logResizeRefusal(st.rf, refusal)
	assert.Len(t, st.logger.warnings, 2)
}

// In mode Recommend, a lost or blocked state also keeps the limit of an
// earlier step. Only a status with an effective limit costs the extra read.
func TestReadAutoscaleFreezesTheLimitInRecommend(t *testing.T) {
	cases := map[string]func(at *autoscaleTest) (*corev1.ConfigMap, error){
		"missing": func(*autoscaleTest) (*corev1.ConfigMap, error) { return nil, notFound() },
		"corrupt": func(at *autoscaleTest) (*corev1.ConfigMap, error) { return at.stateConfigMap("{"), nil },
		"foreign": func(*autoscaleTest) (*corev1.ConfigMap, error) {
			return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: autoscaleCM}}, nil
		},
		"newer": func(at *autoscaleTest) (*corev1.ConfigMap, error) { return at.stateConfigMap(`{"version":3}`), nil },
	}
	for name, cm := range cases {
		t.Run(name, func(t *testing.T) {
			at := newAutoscaleTest(t, redisfailoverv1.AutoscalingModeRecommend)
			at.rf.Status.Autoscaling.EffectiveMemory = "1280Mi"
			at.mk.On("GetConfigMap", "testns", autoscaleCM).Once().Return(cm(at))
			at.mk.On("GetStatefulSet", "testns", "rfr-test").Once().Return(redisStatefulSet("1280Mi"), nil)

			read, err := at.h.readAutoscale(at.rf)

			require.NoError(t, err)
			assert.Equal(t, 1280*mib, read.effective)
			at.mk.AssertExpectations(t)

			at = newAutoscaleTest(t, redisfailoverv1.AutoscalingModeRecommend)
			at.mk.On("GetConfigMap", "testns", autoscaleCM).Once().Return(cm(at))
			read, err = at.h.readAutoscale(at.rf)
			require.NoError(t, err)
			assert.Equal(t, 1024*mib, read.effective, "no step, no extra read")
			at.mk.AssertNotCalled(t, "GetStatefulSet", mock.Anything, mock.Anything)
		})
	}
}

// A frozen limit ends at a change of the spec in each mode, also in mode
// Recommend, where Plan does not run.
func TestAutoscaleFrozenEndsInModeRecommend(t *testing.T) {
	at := newAutoscaleTest(t, redisfailoverv1.AutoscalingModeRecommend)
	at.now = stepNow
	at.rf.Status.Autoscaling.EffectiveMemory = "1280Mi"
	f := at.fakeStateStore(t)
	at.mk.On("GetStatefulSet", "testns", "rfr-test").Return(redisStatefulSet("1280Mi"), nil)
	at.mrec.On("SetAutoscaler", "testns", "test", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	at.mrfc.On("GetAutoscalerInfo", mock.Anything, autoscaleMaster).Return(masterInfo(500), nil)

	rf := at.handle(t, redisfailoverv1.AutoscalingStatus{})
	require.True(t, decodeState(t, f.stored).Frozen)
	assert.Equal(t, autoscale.ReasonFrozen, rf.Status.Autoscaling.Reason)
	assert.Equal(t, "1280Mi", rf.Status.Autoscaling.EffectiveMemory)

	at.rf.Generation = 2
	at.now = stepNow.Add(2 * time.Minute)
	rf = at.handle(t, rf.Status.Autoscaling)
	assert.False(t, decodeState(t, f.stored).Frozen)
	assert.Equal(t, autoscale.ReasonLow, rf.Status.Autoscaling.Reason)
	assert.Equal(t, "1280Mi", rf.Status.Autoscaling.EffectiveMemory, "Recommend keeps the limit")
}

// An expired revert keeps its limit: Ensure did not write the template of the
// revert, and no pod applied the step.
func TestReadAutoscaleKeepsAnExpiredRevert(t *testing.T) {
	reverted := applying()
	reverted.Phase, reverted.From, reverted.Effective = autoscale.PhaseReverted, 1152*mib, 1152*mib
	reverted.LastSample = autoscaleT0.Add(-time.Hour).Unix()
	at := newAutoscaleTest(t, redisfailoverv1.AutoscalingModeRecommend)
	at.rf.Spec.Redis.Autoscaling.Mode = redisfailoverv1.AutoscalingModeAuto
	at.mk.On("GetConfigMap", "testns", autoscaleCM).Once().Return(at.stateConfigMap(encodeState(t, reverted)), nil)
	at.mk.On("GetStatefulSet", "testns", "rfr-test").Once().Return(redisStatefulSet("1280Mi"), nil)

	read, err := at.h.readAutoscale(at.rf)

	require.NoError(t, err)
	assert.Equal(t, 1152*mib, read.effective)
	assert.Equal(t, autoscale.PhaseReverted, read.state.Phase)
}

func TestUnplaceable(t *testing.T) {
	unschedulable := []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable}}
	assert.True(t, unplaceable(&corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending, Conditions: unschedulable}}))
	assert.False(t, unplaceable(&corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending}}))
	assert.False(t, unplaceable(&corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: unschedulable}}))
}
