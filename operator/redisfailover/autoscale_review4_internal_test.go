package redisfailover

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	mRFService "github.com/saremox/redis-operator/mocks/operator/redisfailover/service"
	"github.com/saremox/redis-operator/operator/redisfailover/autoscale"
)

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
