package redisfailover

import (
	"encoding/json"
	"errors"
	"fmt"
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
				it.rf.Spec.Sentinel.Enabled = new(true)
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

	// Without a rollout result, for example with a lost quorum, the
	// operator lists the pods. Only pods that no node can hold allow a
	// revert.
	step := func(name string, phase corev1.PodPhase, limit string) corev1.Pod {
		pod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name}, Status: corev1.PodStatus{Phase: phase}}
		pod.Spec.Containers = []corev1.Container{{Name: "redis", Resources: memoryLimit(limit)}}
		if phase == corev1.PodPending {
			pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable}}
		} else {
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "redis", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}
		}
		return pod
	}
	terminating := step("rfr-test-0", corev1.PodRunning, "1Gi")
	terminating.DeletionTimestamp = &metav1.Time{Time: stepNow}
	unplaced := step("rfr-test-0", corev1.PodPending, "1280Mi")
	unplaced.Annotations = map[string]string{"redisfailovers.databases.spotahome.com/resize-requested-at": "2026-01-01T00:00:00Z"}
	tests := map[string]struct {
		replicas int32
		pods     []corev1.Pod
		err      error
		want     []string
	}{
		"a pod is missing":                          {2, []corev1.Pod{step("rfr-test-0", corev1.PodRunning, "1Gi")}, nil, []string{"Ensure 1280Mi", "write Held 1280Mi"}},
		"a pod terminates":                          {1, []corev1.Pod{terminating}, nil, []string{"Ensure 1280Mi", "write Held 1280Mi"}},
		"a pod of the step runs":                    {1, []corev1.Pod{step("rfr-test-0", corev1.PodRunning, "1280Mi")}, nil, []string{"Ensure 1280Mi", "write Held 1280Mi"}},
		"a pod of the step starts":                  {1, []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "rfr-test-0"}, Spec: unplaced.Spec}}, nil, []string{"Ensure 1280Mi", "write Held 1280Mi"}},
		"a list error":                              {1, nil, errors.New("timeout"), []string{"Ensure 1280Mi", "write Held 1280Mi"}},
		"no StatefulSet":                            {1, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "statefulsets"}, "rfr-test"), []string{"Ensure 1280Mi", "write Held 1280Mi"}},
		"the only pod of the step cannot be placed": {1, []corev1.Pod{unplaced}, nil, []string{"Ensure 1280Mi", "clear rfr-test-0", "write Reverted 0", "requeue testns/test 0s"}},
	}
	for name, test := range tests {
		t.Run("without a rollout result, "+name, func(t *testing.T) {
			st := newStepTest(t, stored)
			st.rf.Spec.Redis.Replicas = test.replicas
			st.pods, st.podsErr = test.pods, test.err
			st.heal.On("ClearResizeRequested", mock.Anything, mock.Anything).Run(func(args mock.Arguments) {
				st.calls = append(st.calls, "clear "+args.String(1))
			}).Return(nil)
			st.mrec.On("CountAutoscalerStep", "testns", "test", mock.Anything).Once()
			st.reconcile(t, nil)
			assert.Equal(t, test.want, st.calls)
		})
	}
}

// A revert after the timeout waits while the resize request of a pod cannot
// be removed, and holds the step when the wait continues. A pod that is gone
// has no request.
func TestAutoscaleHoldsAStepThatTheOperatorCannotRevert(t *testing.T) {
	refused := &autoscaleRollout{complete: true, requested: []string{"rfr-test-1"}, updateRevision: "rev1"}
	timedOut := applying()
	timedOut.StartedAt = stepNow.Add(-autoscale.ApplyTimeout).Unix()

	t.Run("a short error waits", func(t *testing.T) {
		st := newStepTest(t, timedOut)
		st.heal.On("ClearResizeRequested", mock.Anything, "rfr-test-1").Once().Return(errors.New("timeout"))
		st.heal.On("ClearResizeRequested", mock.Anything, "rfr-test-1").Once().Return(nil)
		st.mrec.On("CountAutoscalerStep", "testns", "test", metrics.AutoscalerStepReverted).Once()
		st.reconcile(t, refused)
		st.now = stepNow.Add(time.Minute)
		st.reconcile(t, refused)
		assert.Equal(t, []string{"Ensure 1280Mi", "Ensure 1280Mi", "write Reverted 0", "requeue testns/test 0s"}, st.calls)
	})

	t.Run("a lasting error holds after a second timeout", func(t *testing.T) {
		stored := applying()
		stored.StartedAt = stepNow.Add(-2 * autoscale.ApplyTimeout).Unix()
		st := newStepTest(t, stored)
		st.heal.On("ClearResizeRequested", mock.Anything, "rfr-test-1").Return(errors.New("denied by webhook"))
		st.mrec.On("CountAutoscalerStep", "testns", "test", metrics.AutoscalerStepHeld).Once()
		st.reconcile(t, refused)
		assert.Equal(t, []string{"Ensure 1280Mi", "write Held 1280Mi"}, st.calls)
		assert.Equal(t, autoscale.ReasonHeld+": "+autoscale.RefusalClear, st.status.Reason)
	})

	t.Run("the pod is gone", func(t *testing.T) {
		st := newStepTest(t, timedOut)
		st.heal.On("ClearResizeRequested", mock.Anything, "rfr-test-1").Return(apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "rfr-test-1"))
		st.mrec.On("CountAutoscalerStep", "testns", "test", metrics.AutoscalerStepReverted).Once()
		st.reconcile(t, refused)
		assert.Equal(t, "write Reverted 0", st.calls[1])
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
			at.onStatefulSet().Once().Return(redisStatefulSet("1280Mi"), nil)

			read, err := at.h.readAutoscale(at.rf)

			require.NoError(t, err)
			assert.Equal(t, 1280*mib, read.effective)
			at.mk.AssertExpectations(t)

			// The status can be lost together with the state.
			at = newAutoscaleTest(t, redisfailoverv1.AutoscalingModeRecommend)
			at.mk.On("GetConfigMap", "testns", autoscaleCM).Once().Return(cm(at))
			at.onStatefulSet().Once().Return(redisStatefulSet("1280Mi"), nil)
			read, err = at.h.readAutoscale(at.rf)
			require.NoError(t, err)
			assert.Equal(t, 1280*mib, read.effective)
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
	at.onStatefulSet().Return(redisStatefulSet("1280Mi"), nil)
	at.mrec.On("SetAutoscaler", "testns", "test", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	at.mrfc.On("GetAutoscalerInfo", mock.Anything, autoscaleMaster).Return(masterInfo(500), nil)

	rf := at.handle(t, redisfailoverv1.AutoscalingStatus{})
	require.True(t, decodeState(t, f.stored).Frozen)
	assert.Equal(t, autoscale.ReasonFrozen, rf.Status.Autoscaling.Reason)
	assert.Equal(t, "1280Mi", rf.Status.Autoscaling.EffectiveMemory)

	at.rf.Generation = 2
	at.now = stepNow.Add(2 * time.Minute)
	rf = at.handle(t, rf.Status.Autoscaling)
	s := decodeState(t, f.stored)
	assert.False(t, s.Frozen)
	// A pod can still refuse the step of the lost state.
	assert.Equal(t, autoscale.PhaseHeld, s.Phase)
	assert.Equal(t, autoscale.ReasonHeld+": "+autoscale.RefusalLostState, rf.Status.Autoscaling.Reason)
	assert.Equal(t, "1280Mi", rf.Status.Autoscaling.EffectiveMemory, "Recommend keeps the limit")
}

// The procedure to pin the limit after a lost state sets the spec limit to
// the frozen limit. The template stays, and a pod can still refuse the step of
// the lost state. The freeze becomes a held step to that limit at the first
// change of the spec, so a later pin also keeps the step.
func TestAutoscaleFrozenLimitPinnedBecomesAHeldStep(t *testing.T) {
	for _, mode := range []string{redisfailoverv1.AutoscalingModeRecommend, redisfailoverv1.AutoscalingModeAuto} {
		for _, later := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s later %v", mode, later), func(t *testing.T) {
				testFrozenLimitPinned(t, mode, later)
			})
		}
	}
}

func testFrozenLimitPinned(t *testing.T, mode string, later bool) {
	t.Helper()
	at := newAutoscaleTest(t, redisfailoverv1.AutoscalingModeRecommend)
	at.rf.Spec.Redis.Autoscaling.Mode = mode
	at.now = stepNow
	at.rf.Status.Autoscaling.EffectiveMemory = "1280Mi"
	f := at.fakeStateStore(t)
	at.onStatefulSet().Return(redisStatefulSet("1280Mi"), nil)
	at.mrec.On("SetAutoscaler", "testns", "test", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	at.mrfc.On("GetAutoscalerInfo", mock.Anything, autoscaleMaster).Return(masterInfo(500), nil)
	rf := at.handle(t, redisfailoverv1.AutoscalingStatus{})
	require.True(t, decodeState(t, f.stored).Frozen)

	if later {
		// A change of the spec that keeps the template.
		at.rf.Generation++
		at.rf.Spec.Redis.Replicas = 3
		at.now = at.now.Add(2 * time.Minute)
		rf = at.handle(t, rf.Status.Autoscaling)
		require.Equal(t, autoscale.PhaseHeld, decodeState(t, f.stored).Phase)
	}
	at.rf.Generation++
	at.rf.Spec.Redis.Resources = memoryLimit("1280Mi")
	at.now = at.now.Add(2 * time.Minute)
	rf = at.handle(t, rf.Status.Autoscaling)

	s := decodeState(t, f.stored)
	assert.False(t, s.Frozen)
	assert.Equal(t, autoscale.PhaseHeld, s.Phase)
	assert.Equal(t, 1024*mib, s.From)
	assert.Equal(t, 1280*mib, s.To)
	assert.Zero(t, s.Effective)
	assert.Equal(t, autoscale.ReasonHeld+": "+autoscale.RefusalLostState, rf.Status.Autoscaling.Reason)
	read, err := at.h.readAutoscale(at.rf)
	require.NoError(t, err)
	at.h.autoscaleReads.Store(failoverKey(at.rf), read)
	assert.Equal(t, rfservice.AutoscalerLimits{Spec: 1024 * mib, Effective: 1280 * mib}, at.h.autoscalerLimits(at.rf))
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
	at.onStatefulSet().Once().Return(redisStatefulSet("1280Mi"), nil)
	at.rf.Spec.Redis.Replicas = 1
	at.mk.On("GetStatefulSetPods", "testns", "rfr-test").Once().Return(&corev1.PodList{Items: []corev1.Pod{redisPod("rfr-test-0", "old", "1152Mi")}}, nil)

	read, err := at.h.readAutoscale(at.rf)

	require.NoError(t, err)
	assert.Equal(t, 1152*mib, read.effective)
	assert.Equal(t, autoscale.PhaseReverted, read.state.Phase)
}

// Before Ensure applies a revert, the operator lists the pods again. A pod
// that runs or starts from the template of the step holds the step, also
// after an operator outage.
func TestReadAutoscaleHoldsARevertThatEnsureDidNotApply(t *testing.T) {
	reverted := applying()
	reverted.Phase, reverted.Effective, reverted.Refusal = autoscale.PhaseReverted, 0, autoscale.RefusalTimeout
	running := redisPod("rfr-test-1", "new", "1280Mi")
	pending := startingPod("new", "1280Mi")
	tests := map[string]struct {
		state    func(s autoscale.State) autoscale.State
		pods     []corev1.Pod
		listed   bool
		held     bool
		listErr  error
		expiries bool
	}{
		"a pod of the step runs":   {pods: []corev1.Pod{running}, listed: true, held: true},
		"a pod of the step starts": {pods: []corev1.Pod{pending}, listed: true, held: true},
		"no pod of the step":       {pods: []corev1.Pod{redisPod("rfr-test-1", "old", "1Gi")}, listed: true},
		"after an outage":          {pods: []corev1.Pod{running}, listed: true, held: true, expiries: true},
		"list error":               {listErr: errors.New("timeout"), listed: true},
		"Ensure applied the revert": {state: func(s autoscale.State) autoscale.State {
			s.RevertApplied = true
			return s
		}, pods: []corev1.Pod{running}},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			s := reverted
			if test.state != nil {
				s = test.state(s)
			}
			if test.expiries {
				s.LastSample = autoscaleT0.Add(-time.Hour).Unix()
			}
			at := newAutoscaleTest(t, redisfailoverv1.AutoscalingModeRecommend)
			at.rf.Spec.Redis.Autoscaling.Mode = redisfailoverv1.AutoscalingModeAuto
			at.rf.Spec.Redis.Replicas = 1
			at.mk.On("GetConfigMap", "testns", autoscaleCM).Once().Return(at.stateConfigMap(encodeState(t, s)), nil)
			at.mk.On("GetStatefulSetPods", "testns", "rfr-test").Return(&corev1.PodList{Items: test.pods}, test.listErr)
			template := "1280Mi"
			if s.RevertApplied {
				template = "1Gi"
			}
			at.onStatefulSet().Maybe().Return(redisStatefulSet(template), nil)

			read, err := at.h.readAutoscale(at.rf)

			if test.listErr != nil {
				require.ErrorIs(t, err, test.listErr)
				return
			}
			require.NoError(t, err)
			if test.held {
				assert.Equal(t, autoscale.PhaseHeld, read.state.Phase)
				assert.Equal(t, autoscale.RefusalStepPod, read.state.Refusal)
				assert.Equal(t, 1280*mib, read.effective)
				assert.True(t, read.resumed, "the autoscaler writes it")
			} else {
				assert.Equal(t, autoscale.PhaseReverted, read.state.Phase)
				assert.Equal(t, 1024*mib, read.effective)
			}
			if !test.listed {
				at.mk.AssertNotCalled(t, "GetStatefulSetPods", mock.Anything, mock.Anything)
			}
		})
	}
}

// The autoscaler marks a revert that Ensure applied, so later reconciles do
// not list the pods.
func TestAutoscaleMarksAnAppliedRevert(t *testing.T) {
	reverted := applying()
	reverted.Phase, reverted.Effective, reverted.RetryAt = autoscale.PhaseReverted, 0, stepNow.Add(time.Hour).Unix()
	st := newStepTest(t, reverted)

	st.reconcile(t, &autoscaleRollout{complete: true, applied: true, updateRevision: "rev0"})
	require.True(t, decodeState(t, st.store.stored).RevertApplied)
	st.reconcile(t, &autoscaleRollout{complete: true, applied: true, updateRevision: "rev0"})

	assert.Equal(t, []string{"Ensure 1Gi", "write Reverted 0", "Ensure 1Gi"}, st.calls)
	st.mk.AssertNumberOfCalls(t, "GetStatefulSetPods", 1)

	t.Run("not without Handle", func(t *testing.T) {
		st := newStepTest(t, reverted)
		st.rf.Status.Autoscaling = redisfailoverv1.AutoscalingStatus{}
		st.h.autoscale(st.rf, redisfailoverv1.AutoscalingStatus{})
		assert.Zero(t, st.store.updates)
	})
}

func TestUnplaceable(t *testing.T) {
	unschedulable := []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable}}
	assert.True(t, unplaceable(&corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending, Conditions: unschedulable}}))
	assert.False(t, unplaceable(&corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending}}))
	assert.False(t, unplaceable(&corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: unschedulable}}))
}
