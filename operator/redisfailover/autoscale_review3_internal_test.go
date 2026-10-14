package redisfailover

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/metrics"
	mRFService "github.com/saremox/redis-operator/mocks/operator/redisfailover/service"
	mK8SService "github.com/saremox/redis-operator/mocks/service/k8s"
	"github.com/saremox/redis-operator/operator/redisfailover/autoscale"
)

// Only a pod of the step that exists holds a revert that Ensure did not
// apply. A missing pod has no data of the step, and a quota can refuse it at
// the limit of the step. Without the StatefulSet, Ensure must create it.
func TestReadAutoscaleHoldsARevertOnlyForAnExistingPod(t *testing.T) {
	reverted := applying()
	reverted.Phase, reverted.Effective, reverted.Refusal = autoscale.PhaseReverted, 0, "pod rfr-test-1: quota"
	terminating := redisPod("rfr-test-1", "new", "1280Mi")
	terminating.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	notFound := apierrors.NewNotFound(schema.GroupResource{Group: "apps", Resource: "statefulsets"}, "rfr-test")
	tests := map[string]struct {
		pods []corev1.Pod
		err  error
		held bool
	}{
		"a pod is missing":               {pods: []corev1.Pod{redisPod("rfr-test-0", "old", "1Gi")}},
		"a pod of the step terminates":   {pods: []corev1.Pod{redisPod("rfr-test-0", "old", "1Gi"), terminating}},
		"no StatefulSet":                 {err: notFound},
		"a pod of the step exists":       {pods: []corev1.Pod{redisPod("rfr-test-0", "old", "1Gi"), redisPod("rfr-test-1", "new", "1280Mi")}, held: true},
		"a pod of the step is not ready": {pods: []corev1.Pod{startingPod("new", "1280Mi")}, held: true},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			at := newAutoscaleTest(t, redisfailoverv1.AutoscalingModeRecommend)
			at.rf.Spec.Redis.Autoscaling.Mode = redisfailoverv1.AutoscalingModeAuto
			at.rf.Spec.Redis.Replicas = 2
			at.mk.On("GetConfigMap", "testns", autoscaleCM).Once().Return(at.stateConfigMap(encodeState(t, reverted)), nil)
			at.mk.On("GetStatefulSetPods", "testns", "rfr-test").Return(&corev1.PodList{Items: test.pods}, test.err)

			read, err := at.h.readAutoscale(at.rf)

			require.NoError(t, err)
			if test.held {
				assert.Equal(t, autoscale.PhaseHeld, read.state.Phase)
				return
			}
			assert.Equal(t, autoscale.PhaseReverted, read.state.Phase)
			assert.Equal(t, 1024*mib, read.effective)
		})
	}
}

// Mode Recommend shows no step of an earlier mode Auto, only the step that
// it completes. The effective limit stays in the status.
func TestAutoscaleRecommendShowsNoEarlierStep(t *testing.T) {
	for phase, shown := range map[string]string{autoscale.PhaseReverted: "", autoscale.PhaseDone: "", autoscale.PhaseHeld: autoscale.PhaseHeld} {
		t.Run(phase, func(t *testing.T) {
			s := applying()
			s.Phase, s.Refusal, s.RetryAt = phase, "pod rfr-test-1: quota", stepNow.Add(-time.Hour).Unix()
			s.Effective, s.From, s.SpecLimit = 1152*mib, 1152*mib, 1024*mib
			st := newStepTest(t, s)
			st.rf.Spec.Redis.Autoscaling.Mode = redisfailoverv1.AutoscalingModeRecommend
			st.reconcile(t, &autoscaleRollout{complete: true, updateRevision: "rev1"})
			assert.Equal(t, shown, st.status.Phase)
			assert.Equal(t, shown != "", st.status.LastStep != "")
			assert.Empty(t, st.status.RetryAfter)
			assert.NotEmpty(t, st.status.EffectiveMemory)
			if shown == "" {
				assert.NotContains(t, st.status.Reason, "pod rfr-test-1")
				st.mrec.AssertCalled(t, "SetAutoscalerPhase", "testns", "test", 0)
			}
		})
	}
}

// A failed removal of a leftover resize request is logged at Warning once.
func TestRecordAutoscaleRolloutLogsAFailedRemoval(t *testing.T) {
	rf := autoscaleRolloutRF(redisfailoverv1.AutoscalingModeAuto)
	pod := redisPod("rfr-test-0", "new", "2Gi")
	pod.Annotations = map[string]string{"redisfailovers.databases.spotahome.com/resize-requested-at": "2026-01-01T00:00:00Z"}
	ss := &appsv1.StatefulSet{Status: appsv1.StatefulSetStatus{UpdateRevision: "new"}}
	logger := &warnLogger{}
	heal := &mRFService.RedisFailoverHeal{}
	heal.On("ClearResizeRequested", rf, "rfr-test-0").Twice().Return(errors.New("denied"))
	heal.On("ClearResizeRequested", rf, "rfr-test-0").Once().Return(nil)
	mk := &mK8SService.Services{}
	mk.On("GetStatefulSet", "testns", "rfr-test").Return(ss, nil)
	mk.On("GetStatefulSetPods", "testns", "rfr-test").Return(&corev1.PodList{Items: []corev1.Pod{pod}}, nil)
	h := NewRedisFailoverHandler(Config{}, nil, nil, heal, mk, metrics.Dummy, logger)
	for range 3 {
		require.NoError(t, h.recordAutoscaleRollout(rf, false))
	}
	assert.Equal(t, []string{"Autoscaler: the operator cannot remove the resize request: denied"}, logger.warnings)
	assert.Len(t, logger.debugs, 1)
	_, ok := h.autoscaleClearLogged.Load(failoverKey(rf))
	assert.False(t, ok, "a removal clears the log memory")
	heal.AssertExpectations(t)
}
