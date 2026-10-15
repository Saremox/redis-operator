package redisfailover

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime/schema"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/metrics"
	mRFService "github.com/saremox/redis-operator/mocks/operator/redisfailover/service"
	"github.com/saremox/redis-operator/operator/redisfailover/autoscale"
	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
	"github.com/saremox/redis-operator/service/k8s"
)

// stepNow is the time of the reconciles of a stepTest. The stored states have
// a sample 30s before, so no INFO call is due.
var stepNow = autoscaleT0.Add(30 * time.Second)

// stepTest is an autoscaleTest in mode Auto with managed maxmemory and a
// stored state. calls holds the order of the Ensure limits, the state writes,
// the annotation clears and the requeues.
type stepTest struct {
	*autoscaleTest
	store  *fakeStateStore
	heal   *mRFService.RedisFailoverHeal
	calls  []string
	status redisfailoverv1.AutoscalingStatus
	// pods and podsErr are the answer to a list of the pods.
	pods    []corev1.Pod
	podsErr error
}

func newStepTest(t *testing.T, stored autoscale.State) *stepTest {
	t.Helper()
	at := newAutoscaleTest(t, redisfailoverv1.AutoscalingModeRecommend)
	at.rf.Spec.Redis.Autoscaling.Mode = redisfailoverv1.AutoscalingModeAuto
	at.rf.Spec.Redis.MaxMemory = &redisfailoverv1.MaxMemorySettings{Percent: 75, Policy: "noeviction"}
	at.now = stepNow
	st := &stepTest{autoscaleTest: at, heal: &mRFService.RedisFailoverHeal{}}
	at.h.rfHealer = st.heal
	st.store = at.fakeStateStore(t)
	st.store.stored = at.stateConfigMap(encodeState(t, stored))
	st.store.onWrite = func(s autoscale.State) {
		st.calls = append(st.calls, "write "+s.Phase+" "+quantity(s.Effective))
	}
	at.h.requeue = func(key string, after time.Duration) {
		st.calls = append(st.calls, fmt.Sprintf("requeue %s %s", key, after))
	}
	at.mrec.On("SetAutoscaler", "testns", "test", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()
	at.rf.Spec.Redis.Replicas = 0
	at.mk.On("GetStatefulSetPods", "testns", "rfr-test").Maybe().Return(func(string, string) (*corev1.PodList, error) {
		return &corev1.PodList{Items: st.pods}, st.podsErr
	})
	return st
}

// reconcile runs the read before Ensure and the autoscaler after the checks,
// with rollout as the result of UpdateRedisesPods, or none for nil.
func (st *stepTest) reconcile(t *testing.T, rollout *autoscaleRollout) *redisfailoverv1.RedisFailover {
	t.Helper()
	rf, err := st.h.withEffectiveLimit(st.rf)
	require.NoError(t, err)
	st.calls = append(st.calls, "Ensure "+rf.Spec.Redis.Resources.Limits.Memory().String())
	st.h.autoscaleRollout.Delete(failoverKey(rf))
	if rollout != nil {
		st.h.autoscaleRollout.Store(failoverKey(rf), *rollout)
	}
	rf.Status.Autoscaling = redisfailoverv1.AutoscalingStatus{}
	st.h.autoscale(rf, st.status)
	st.status = rf.Status.Autoscaling
	return rf
}

// recommending is a state with a sustained recommendation of 1280Mi for the
// spec limit of 1Gi.
func recommending() autoscale.State {
	return autoscale.State{
		Version: autoscale.StateVersion, Workload: "Store", RunID: "run1", Limit: 1024 * mib, LastSample: autoscaleT0.Unix(),
		Reason: autoscale.ReasonSustained, Recommended: 1280 * mib, HighWindows: 3, Phase: autoscale.PhaseIdle,
	}
}

// applying is a step from 1Gi to 1280Mi that started at autoscaleT0.
func applying() autoscale.State {
	s := recommending()
	s.Phase, s.From, s.To, s.Effective, s.SpecLimit = autoscale.PhaseApplying, 1024*mib, 1280*mib, 1280*mib, 1024*mib
	s.StartedAt, s.Steps[autoscale.StepBudget-1] = autoscaleT0.Unix(), autoscaleT0.Unix()
	s.Limit = 1280 * mib
	return s
}

// settledRollout is a rollout with all pods ready on the update revision rev1
// with the effective limit applied.
func settledRollout() *autoscaleRollout {
	return &autoscaleRollout{complete: true, settled: true, applied: true, anyApplied: true, updateRevision: "rev1"}
}

// allowStep lets the checks before a step pass.
func (st *stepTest) allowStep() {
	st.mk.On("PodResizeSupport").Return(k8s.PodResizeSupport{Supported: true}, nil)
	st.mk.On("GetControllerRevision", "testns", "rev1").Return(&appsv1.ControllerRevision{}, nil)
}

// A step writes its state first. Ensure of the same reconcile still has the
// earlier limit, and the requeue makes the next reconcile apply the step.
func TestAutoscaleStartsAStep(t *testing.T) {
	st := newStepTest(t, recommending())
	st.allowStep()

	st.reconcile(t, settledRollout())

	assert.Equal(t, autoscale.PhaseApplying, st.status.Phase)
	assert.Equal(t, stepNow.UTC().Truncate(time.Minute).Format(time.RFC3339), st.status.LastStep)
	stored := decodeState(t, st.store.stored)
	assert.Equal(t, 1280*mib, stored.Effective)
	assert.Equal(t, 1024*mib, stored.From)

	st.reconcile(t, &autoscaleRollout{complete: true, updateRevision: "rev1"})
	assert.Equal(t, []string{"Ensure 1Gi", "write Applying 1280Mi", "requeue testns/test 0s", "Ensure 1280Mi"}, st.calls)
	assert.Equal(t, "1280Mi", st.status.EffectiveMemory)
	st.mk.AssertNumberOfCalls(t, "PodResizeSupport", 1)
	st.mk.AssertNumberOfCalls(t, "GetControllerRevision", 1)
	st.mrec.AssertCalled(t, "SetAutoscalerPhase", "testns", "test", 1)
}

// A failed state write starts no step: the template stays.
func TestAutoscaleFailedWriteStartsNoStep(t *testing.T) {
	st := newStepTest(t, recommending())
	st.allowStep()
	st.store.err = errors.New("conflict")

	st.reconcile(t, settledRollout())
	st.reconcile(t, settledRollout())

	assert.Equal(t, []string{"Ensure 1Gi", "Ensure 1Gi"}, st.calls)
	assert.Equal(t, autoscale.PhaseIdle, st.status.Phase)
	assert.Equal(t, autoscale.PhaseIdle, decodeState(t, st.store.stored).Phase)
	assert.Equal(t, []string{"Autoscaler: conflict"}, st.logger.warnings)
}

// Each check before a step stops it with a reason in the status, and without
// an error. Nothing is written and no pod changes.
func TestAutoscaleChecksBeforeAStep(t *testing.T) {
	forbidden := apierrors.NewForbidden(schema.GroupResource{Group: "apps", Resource: "controllerrevisions"}, "rev1", errors.New("rbac"))
	tests := map[string]struct {
		change  func(st *stepTest, rollout *autoscaleRollout)
		reason  string
		warning string
	}{
		"inPlaceResize Disabled": {change: func(st *stepTest, _ *autoscaleRollout) { st.rf.Spec.Redis.InPlaceResize = "Disabled" }, reason: autoscale.ReasonInPlaceDisabled},
		"no maxMemory":           {change: func(st *stepTest, _ *autoscaleRollout) { st.rf.Spec.Redis.MaxMemory = nil }, reason: autoscale.ReasonNoManagedMaxMemory},
		"customConfig maxmemory": {change: func(st *stepTest, _ *autoscaleRollout) { st.rf.Spec.Redis.CustomConfig = []string{"maxmemory 100mb"} }, reason: autoscale.ReasonCustomMaxMemory},
		"VerticalPodAutoscaler":  {change: func(_ *stepTest, r *autoscaleRollout) { r.vpa = true }, reason: autoscale.ReasonVPA},
		"restart on a resize":    {change: func(_ *stepTest, r *autoscaleRollout) { r.restartOnResize = true }, reason: autoscale.ReasonRestartOnResize},
		"Kubernetes 1.32": {change: func(st *stepTest, _ *autoscaleRollout) {
			st.mk.On("PodResizeSupport").Return(k8s.PodResizeSupport{}, nil).Twice()
		}, reason: autoscale.ReasonNoInPlaceResize},
		"server version unknown": {change: func(st *stepTest, _ *autoscaleRollout) {
			st.mk.On("PodResizeSupport").Return(k8s.PodResizeSupport{}, errors.New("no version")).Twice()
		}, reason: autoscale.ReasonPreconditionUnknown, warning: "Autoscaler: no version"},
		"no permission for controllerrevisions": {change: func(st *stepTest, _ *autoscaleRollout) {
			st.mk.On("PodResizeSupport").Return(k8s.PodResizeSupport{Supported: true}, nil).Twice()
			st.mk.On("GetControllerRevision", "testns", "rev1").Return(nil, forbidden).Twice()
		}, reason: autoscale.ReasonNoRevisionAccess},
		"revision read error": {change: func(st *stepTest, _ *autoscaleRollout) {
			st.mk.On("PodResizeSupport").Return(k8s.PodResizeSupport{Supported: true}, nil).Twice()
			st.mk.On("GetControllerRevision", "testns", "rev1").Return(nil, errors.New("timeout")).Twice()
		}, reason: autoscale.ReasonPreconditionUnknown, warning: "Autoscaler: timeout"},
		"rollout not settled": {change: func(_ *stepTest, r *autoscaleRollout) { r.settled = false }, reason: autoscale.ReasonNotSettled},
		"no rollout result":   {change: func(_ *stepTest, r *autoscaleRollout) { *r = autoscaleRollout{} }, reason: autoscale.ReasonNotSettled},
		"not Healthy": {change: func(st *stepTest, _ *autoscaleRollout) {
			st.rf.Status.State = redisfailoverv1.NotHealthyState
		}, reason: autoscale.ReasonNotSettled},
		"frozen limit": {change: func(st *stepTest, _ *autoscaleRollout) {
			s := recommending()
			s.Frozen = true
			st.store.stored = st.stateConfigMap(encodeState(t, s))
		}, reason: autoscale.ReasonFrozen},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			st := newStepTest(t, recommending())
			rollout := settledRollout()
			test.change(st, rollout)

			st.reconcile(t, rollout)
			st.reconcile(t, rollout)

			assert.Equal(t, []string{"Ensure 1Gi", "Ensure 1Gi"}, st.calls)
			assert.Equal(t, test.reason, st.status.Reason)
			assert.Equal(t, "1280Mi", st.status.RecommendedMemory)
			assert.Zero(t, st.store.updates)
			if test.warning != "" {
				assert.Equal(t, []string{test.warning}, st.logger.warnings, "a repeated error is logged once")
			} else {
				assert.Empty(t, st.logger.warnings)
			}
			st.mk.AssertNotCalled(t, "CreateConfigMap", mock.Anything, mock.Anything)
		})
	}
}

// A refusal before any pod applied the step reverts it. The annotation of
// the resize request goes first, and the state write before the template
// changes back.
func TestAutoscaleRevertsAStep(t *testing.T) {
	refusal := &resizeRefusal{pod: "rfr-test-1", class: rfservice.ResizePermanent, message: "resize infeasible: no node"}
	refused := func() *autoscaleRollout {
		return &autoscaleRollout{complete: true, refusal: refusal, requested: []string{"rfr-test-1"}, updateRevision: "rev1"}
	}

	t.Run("revert", func(t *testing.T) {
		st := newStepTest(t, applying())
		st.heal.On("ClearResizeRequested", mock.Anything, "rfr-test-1").Once().Run(func(mock.Arguments) {
			st.calls = append(st.calls, "clear rfr-test-1")
		}).Return(nil)
		st.mrec.On("CountAutoscalerStep", "testns", "test", metrics.AutoscalerStepReverted).Once()

		st.reconcile(t, refused())
		st.reconcile(t, &autoscaleRollout{complete: true, applied: true, updateRevision: "rev0"})

		// The second reconcile marks the revert that Ensure applied.
		assert.Equal(t, []string{"Ensure 1280Mi", "clear rfr-test-1", "write Reverted 0", "requeue testns/test 0s", "Ensure 1Gi", "write Reverted 0"}, st.calls)
		assert.Equal(t, autoscale.PhaseReverted, st.status.Phase)
		assert.Equal(t, autoscale.ReasonReverted+": pod rfr-test-1: resize infeasible: no node", st.status.Reason)
		assert.Equal(t, stepNow.Add(time.Hour).UTC().Truncate(time.Minute).Format(time.RFC3339), st.status.RetryAfter)
		assert.Empty(t, st.status.EffectiveMemory)
		st.heal.AssertExpectations(t)
		st.mrec.AssertExpectations(t)
		st.mrec.AssertCalled(t, "SetAutoscalerPhase", "testns", "test", 3)
	})

	t.Run("clear error waits", func(t *testing.T) {
		st := newStepTest(t, applying())
		st.heal.On("ClearResizeRequested", mock.Anything, "rfr-test-1").Return(errors.New("forbidden"))

		st.reconcile(t, refused())
		st.reconcile(t, refused())

		assert.Equal(t, []string{"Ensure 1280Mi", "Ensure 1280Mi"}, st.calls)
		assert.Equal(t, autoscale.PhaseApplying, st.status.Phase)
		assert.Equal(t, []string{"Autoscaler: the revert of the autoscaler step waits: forbidden"}, st.logger.warnings)
	})

	t.Run("a pod applied the step: hold", func(t *testing.T) {
		st := newStepTest(t, applying())
		st.mrec.On("CountAutoscalerStep", "testns", "test", metrics.AutoscalerStepHeld).Once()
		rollout := refused()
		rollout.anyApplied = true

		st.reconcile(t, rollout)
		st.reconcile(t, rollout)

		assert.Equal(t, []string{"Ensure 1280Mi", "write Held 1280Mi", "Ensure 1280Mi"}, st.calls)
		assert.Equal(t, autoscale.ReasonHeld+": pod rfr-test-1: resize infeasible: no node", st.status.Reason)
		assert.Equal(t, "1280Mi", st.status.EffectiveMemory)
		st.heal.AssertNotCalled(t, "ClearResizeRequested", mock.Anything, mock.Anything)
		st.mrec.AssertExpectations(t)
	})

	t.Run("a refusal of an incomplete rollout is not used", func(t *testing.T) {
		st := newStepTest(t, applying())
		rollout := refused()
		rollout.complete = false

		st.reconcile(t, rollout)

		assert.Equal(t, []string{"Ensure 1280Mi"}, st.calls)
		assert.Equal(t, autoscale.PhaseApplying, st.status.Phase)
	})
}

// A step that does not end in 15 minutes is held, and a held step is done
// when all pods applied the limit. Only the applied limit counts, not the
// revision label.
func TestAutoscaleStepTimeoutAndDone(t *testing.T) {
	stored := applying()
	stored.StartedAt = stepNow.Add(-autoscale.ApplyTimeout).Unix()
	st := newStepTest(t, stored)
	// The held step counts one time, as held.
	st.mrec.On("CountAutoscalerStep", "testns", "test", metrics.AutoscalerStepHeld).Once()
	waiting := &autoscaleRollout{complete: true, anyApplied: true, updateRevision: "rev1"}

	st.reconcile(t, waiting)
	assert.Equal(t, autoscale.ReasonHeld+": "+autoscale.RefusalTimeout, st.status.Reason)

	// Each pod applied the limit, and the rollout moved each label.
	st.reconcile(t, &autoscaleRollout{complete: true, applied: true, anyApplied: true, updateRevision: "rev1"})

	assert.Equal(t, []string{"Ensure 1280Mi", "write Held 1280Mi", "Ensure 1280Mi", "write Done 1280Mi"}, st.calls)
	assert.Equal(t, autoscale.PhaseDone, st.status.Phase)
	st.mrec.AssertExpectations(t)
	st.mrec.AssertCalled(t, "SetAutoscalerPhase", "testns", "test", 4)
	st.mrec.AssertCalled(t, "SetAutoscalerPhase", "testns", "test", 2)
}

// A transient outcome of the rollout waits.
func TestAutoscaleStepWaits(t *testing.T) {
	st := newStepTest(t, applying())

	st.reconcile(t, &autoscaleRollout{complete: true, anyApplied: true, requested: []string{"rfr-test-1"}, updateRevision: "rev1"})
	st.reconcile(t, nil)

	assert.Equal(t, []string{"Ensure 1280Mi", "Ensure 1280Mi"}, st.calls)
	assert.Equal(t, autoscale.PhaseApplying, st.status.Phase)
}

// The rows of the pre-emption table: a change of the user during a step.
func TestAutoscaleUserChangeDuringAStep(t *testing.T) {
	t.Run("spec raised above To", func(t *testing.T) {
		st := newStepTest(t, applying())
		st.rf.Spec.Redis.Resources.Limits[corev1.ResourceMemory] = resource.MustParse("1536Mi")

		st.reconcile(t, &autoscaleRollout{complete: true, updateRevision: "rev2"})

		assert.Equal(t, []string{"Ensure 1536Mi", "write Idle 0"}, st.calls)
		assert.Empty(t, st.status.EffectiveMemory)
		assert.Zero(t, st.h.autoscalerLimits(st.rf), "the rollout takes the user path")
	})

	t.Run("spec lowered", func(t *testing.T) {
		st := newStepTest(t, applying())
		st.rf.Spec.Redis.Resources.Limits[corev1.ResourceMemory] = resource.MustParse("512Mi")

		st.reconcile(t, &autoscaleRollout{complete: true, updateRevision: "rev1"})

		assert.Equal(t, []string{"Ensure 1280Mi"}, st.calls)
		assert.Equal(t, autoscale.PhaseApplying, st.status.Phase)
		assert.Contains(t, st.status.Reason, autoscale.NoteSpecBelowEffective)
	})

	t.Run("generation changed", func(t *testing.T) {
		st := newStepTest(t, applying())
		st.rf.Generation = 5

		st.reconcile(t, &autoscaleRollout{complete: true, updateRevision: "rev1"})

		assert.Equal(t, []string{"Ensure 1280Mi"}, st.calls)
		assert.Equal(t, autoscale.PhaseApplying, st.status.Phase)
	})

	t.Run("mode Recommend ends the step", func(t *testing.T) {
		st := newStepTest(t, applying())
		st.rf.Spec.Redis.Autoscaling.Mode = redisfailoverv1.AutoscalingModeRecommend
		st.mrec.On("CountAutoscalerStep", "testns", "test", metrics.AutoscalerStepDone).Once()
		rf, err := st.h.withEffectiveLimit(st.rf)
		require.NoError(t, err)
		assert.Equal(t, rfservice.AutoscalerLimits{Spec: 1024 * mib, Effective: 1280 * mib, Step: 1280 * mib}, st.h.autoscalerLimits(rf), "the step keeps its pods")

		st.reconcile(t, &autoscaleRollout{complete: true, applied: true, anyApplied: true, updateRevision: "rev1"})

		assert.Equal(t, []string{"Ensure 1280Mi", "write Done 1280Mi"}, st.calls)
		st.mrec.AssertExpectations(t)
	})

	t.Run("mode Recommend starts no step", func(t *testing.T) {
		st := newStepTest(t, recommending())
		st.rf.Spec.Redis.Autoscaling.Mode = redisfailoverv1.AutoscalingModeRecommend

		st.reconcile(t, settledRollout())

		assert.Equal(t, []string{"Ensure 1Gi"}, st.calls)
		assert.Equal(t, autoscale.ReasonSustained, st.status.Reason)
		st.mk.AssertNotCalled(t, "PodResizeSupport")
	})
}

func TestAutoscaleSteppingInModeRecommend(t *testing.T) {
	h := NewRedisFailoverHandler(Config{}, nil, nil, nil, nil, metrics.Dummy, nil)
	rf := autoscaleRolloutRF(redisfailoverv1.AutoscalingModeRecommend)
	for phase, want := range map[string]bool{
		autoscale.PhaseApplying: true, autoscale.PhaseHeld: true,
		autoscale.PhaseDone: false, autoscale.PhaseReverted: false, autoscale.PhaseIdle: false,
	} {
		h.autoscaleReads.Store(failoverKey(rf), autoscaleRead{state: autoscale.State{Phase: phase}, spec: 1 << 30, effective: autoscaleEffective})
		assert.Equal(t, want, h.autoscaleStepping(rf), phase)
		// The limits do not depend on the phase.
		assert.Equal(t, autoscaleLimits, h.autoscalerLimits(rf), phase)
	}
	assert.False(t, h.autoscaleStepping(autoscaleRolloutRF(redisfailoverv1.AutoscalingModeOff)))
}

func TestPhaseValue(t *testing.T) {
	for phase, want := range map[string]int{
		"": 0, autoscale.PhaseIdle: 0, autoscale.PhaseApplying: 1, autoscale.PhaseDone: 2,
		autoscale.PhaseReverted: 3, autoscale.PhaseHeld: 4, "Shrinking": 4,
	} {
		assert.Equal(t, want, phaseValue(phase), phase)
	}
}

// An expired state shows the limit of an earlier time. The live template
// wins: its limit is the limit of the pods.
func TestReadAutoscaleResumesAnExpiredState(t *testing.T) {
	old := applying()
	old.LastSample = autoscaleT0.Add(-time.Hour).Unix()
	tests := map[string]struct {
		template  string
		effective int64
		phase     string
	}{
		"template has the step":    {"1280Mi", 1280 * mib, autoscale.PhaseApplying},
		"template at the spec":     {"1Gi", 1024 * mib, autoscale.PhaseIdle},
		"template between":         {"1152Mi", 1152 * mib, autoscale.PhaseIdle},
		"template above the state": {"1536Mi", 1536 * mib, autoscale.PhaseIdle},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			at := newAutoscaleTest(t, redisfailoverv1.AutoscalingModeRecommend)
			at.mk.On("GetConfigMap", "testns", autoscaleCM).Once().Return(at.stateConfigMap(encodeState(t, old)), nil)
			at.onStatefulSet().Once().Return(redisStatefulSet(test.template), nil)

			read, err := at.h.readAutoscale(at.rf)

			require.NoError(t, err)
			assert.Equal(t, test.effective, read.effective)
			assert.Equal(t, test.phase, read.state.Phase)
			assert.True(t, read.resumed)
			at.mk.AssertExpectations(t)
		})
	}

	t.Run("no StatefulSet", func(t *testing.T) {
		at := newAutoscaleTest(t, redisfailoverv1.AutoscalingModeRecommend)
		at.mk.On("GetConfigMap", "testns", autoscaleCM).Return(at.stateConfigMap(encodeState(t, old)), nil)
		at.onStatefulSet().Return(nil, apierrors.NewNotFound(schema.GroupResource{Resource: "statefulsets"}, "rfr-test"))
		at.mk.On("ListPods", "testns").Return(&corev1.PodList{}, nil)
		read, err := at.h.readAutoscale(at.rf)
		require.NoError(t, err)
		assert.Equal(t, 1024*mib, read.effective)
	})

	t.Run("StatefulSet read error", func(t *testing.T) {
		at := newAutoscaleTest(t, redisfailoverv1.AutoscalingModeRecommend)
		at.mk.On("GetConfigMap", "testns", autoscaleCM).Return(at.stateConfigMap(encodeState(t, old)), nil)
		at.onStatefulSet().Return(nil, errors.New("forbidden"))
		_, err := at.h.readAutoscale(at.rf)
		assert.EqualError(t, err, "forbidden")
	})

	t.Run("a sample in memory keeps the state", func(t *testing.T) {
		at := newAutoscaleTest(t, redisfailoverv1.AutoscalingModeRecommend)
		cm := at.stateConfigMap(encodeState(t, old))
		at.mk.On("GetConfigMap", "testns", autoscaleCM).Return(cm, nil)
		sampled := old
		sampled.LastSample = autoscaleT0.Unix()
		at.h.autoscaleSamples.Store(failoverKey(at.rf), autoscaleSample{state: sampled, version: cm.ResourceVersion, sampled: true})

		read, err := at.h.readAutoscale(at.rf)

		require.NoError(t, err)
		assert.Equal(t, 1280*mib, read.effective)
		assert.False(t, read.resumed)
	})

	t.Run("the autoscaler writes the resumed state", func(t *testing.T) {
		st := newStepTest(t, old)
		st.mk.On("GetStatefulSet", "testns", "rfr-test").Return(redisStatefulSet("1Gi"), nil)
		st.mrfc.On("GetAutoscalerInfo", mock.Anything, autoscaleMaster).Return(masterInfo(500), nil)
		st.h.autoscaleMasters.Store(failoverKey(st.rf), autoscaleMaster)

		st.reconcile(t, &autoscaleRollout{complete: true, updateRevision: "rev1"})

		assert.Equal(t, []string{"Ensure 1Gi", "write Idle 0"}, st.calls)
	})
}

// Without autoscaling, Handle removes a leftover state one time, also
// without a status.
func TestWithEffectiveLimitRemovesALeftoverState(t *testing.T) {
	at := newAutoscaleTest(t, redisfailoverv1.AutoscalingModeOff)
	at.mk.On("GetConfigMap", "testns", autoscaleCM).Once().Return(at.stateConfigMap(encodeState(t, applying())), nil)
	at.mk.On("DeleteConfigMap", "testns", autoscaleCM).Once().Return(nil)
	at.mrec.On("DeleteAutoscaler", "testns", "test").Once()

	for range 2 {
		rf, err := at.h.withEffectiveLimit(at.rf)
		require.NoError(t, err)
		assert.Same(t, at.rf, rf)
	}

	at.mk.AssertExpectations(t)
	at.mrec.AssertExpectations(t)

	t.Run("with a status the autoscaler removes it", func(t *testing.T) {
		at := newAutoscaleTest(t, "")
		at.rf.Status.Autoscaling = redisfailoverv1.AutoscalingStatus{Reason: autoscale.ReasonLow}
		_, err := at.h.withEffectiveLimit(at.rf)
		require.NoError(t, err)
		at.assertNoCalls(t)
	})

	t.Run("a read error is retried", func(t *testing.T) {
		at := newAutoscaleTest(t, "")
		at.mk.On("GetConfigMap", "testns", autoscaleCM).Twice().Return(nil, errors.New("timeout"))
		for range 2 {
			_, err := at.h.withEffectiveLimit(at.rf)
			require.NoError(t, err)
		}
		at.mk.AssertExpectations(t)
		assert.Equal(t, []string{"Autoscaler: timeout"}, at.logger.warnings)
	})
}

func TestLogResizeRefusal(t *testing.T) {
	logger := &warnLogger{}
	h := NewRedisFailoverHandler(Config{}, nil, nil, nil, nil, metrics.Dummy, logger)
	rf := autoscaleRolloutRF(redisfailoverv1.AutoscalingModeAuto)
	first := &resizeRefusal{pod: "rfr-test-1", message: "resize infeasible"}

	h.logResizeRefusal(rf, first)
	h.logResizeRefusal(rf, first)
	h.logResizeRefusal(rf, &resizeRefusal{pod: "rfr-test-0", message: "resize infeasible"})

	assert.Equal(t, []string{"The resize to the autoscaler revision failed: resize infeasible", "The resize to the autoscaler revision failed: resize infeasible"}, logger.warnings)
	assert.Len(t, logger.debugs, 1)
	h.Forget(failoverKey(rf))
	_, ok := h.autoscaleRefusalLogged.Load(failoverKey(rf))
	assert.False(t, ok)
}
