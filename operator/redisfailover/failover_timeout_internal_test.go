package redisfailover

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	mRFService "github.com/saremox/redis-operator/mocks/operator/redisfailover/service"
	mK8SService "github.com/saremox/redis-operator/mocks/service/k8s"
	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
)

const (
	timeoutMaster    = "10.0.0.1"
	timeoutMasterPod = "rfr-test-0"
	timeoutReplica   = "10.0.0.2"
)

type failoverTimeoutTest struct {
	rf       *redisfailoverv1.RedisFailover
	checker  *mRFService.RedisFailoverCheck
	healer   *mRFService.RedisFailoverHeal
	k8s      *mK8SService.Services
	pods     []corev1.Pod
	listErr  error
	patchErr error
	now      time.Time
	requeues []time.Duration
	handler  *RedisFailoverHandler
}

// newFailoverTimeoutTest serves three running, ready pods, the first one
// labelled master, and keeps the annotations the handler sets on them.
func newFailoverTimeoutTest(timeout *metav1.Duration) *failoverTimeoutTest {
	ft := &failoverTimeoutTest{
		rf: &redisfailoverv1.RedisFailover{
			ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "testns"},
			Spec:       redisfailoverv1.RedisFailoverSpec{Sentinel: redisfailoverv1.SentinelSettings{FailoverTimeout: timeout}},
		},
		checker: &mRFService.RedisFailoverCheck{},
		healer:  &mRFService.RedisFailoverHeal{},
		k8s:     &mK8SService.Services{},
		pods: []corev1.Pod{
			timeoutRedisPod(timeoutMasterPod, timeoutMaster, true, true),
			timeoutRedisPod("rfr-test-1", timeoutReplica, false, true),
			timeoutRedisPod("rfr-test-2", "10.0.0.3", false, true),
		},
		now: time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC),
	}
	ft.healer.On("ApplyPassword", mock.Anything, mock.Anything, mock.Anything).Maybe().Return(true, nil)
	ft.healer.On("ApplySentinelPassword", mock.Anything, mock.Anything).Maybe().Return(true, nil)
	ft.k8s.On("UpdateRedisFailoverStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return()
	ft.k8s.On("GetStatefulSetPods", "testns", rfservice.GetRedisName(ft.rf)).Maybe().Return(func(string, string) (*corev1.PodList, error) {
		if ft.listErr != nil {
			return nil, ft.listErr
		}
		return &corev1.PodList{Items: append([]corev1.Pod(nil), ft.pods...)}, nil
	})
	ft.k8s.On("UpdatePodAnnotations", "testns", mock.Anything, mock.Anything).Maybe().Return(func(_, name string, annotations map[string]string) error {
		if ft.patchErr != nil {
			return ft.patchErr
		}
		for i := range ft.pods {
			if ft.pods[i].Name == name {
				for k, v := range annotations {
					ft.pods[i].Annotations[k] = v
				}
			}
		}
		return nil
	})
	ft.restart()
	return ft
}

// restart replaces the handler, as an operator restart or a new leader would.
func (ft *failoverTimeoutTest) restart() {
	ft.handler = NewRedisFailoverHandler(Config{}, &mRFService.RedisFailoverClient{}, ft.checker, ft.healer, ft.k8s, metrics.Dummy, log.Dummy)
	ft.handler.now = func() time.Time { return ft.now }
	ft.handler.requeue = func(key string, after time.Duration) {
		if key == "testns/test" {
			ft.requeues = append(ft.requeues, after)
		}
	}
}

func timeoutRedisPod(name, ip string, master, ready bool) corev1.Pod {
	role := "slave"
	if master {
		role = "master"
	}
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"redisfailovers-role": role}, Annotations: map[string]string{}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, PodIP: ip},
	}
	if ready {
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	}
	return pod
}

func (ft *failoverTimeoutTest) unreachableSince() string {
	return ft.pods[0].Annotations[masterUnreachableAnnotation]
}

// unhealthyMaster runs a reconcile at the given offset that finds the master
// counted but not answering, expecting a promotion or not.
func (ft *failoverTimeoutTest) unhealthyMaster(at time.Duration, wantPromote bool) error {
	ft.now = ft.now.Add(at)
	ft.checker.On("IsRedisRunningQuorum", ft.rf).Once().Return(true)
	ft.checker.On("GetNumberMasters", ft.rf).Once().Return(1, nil)
	ft.checker.On("CheckMasterHealth", ft.rf).Once().Return(false, timeoutMaster, nil)
	if wantPromote {
		ft.expectPromotion()
	}
	return ft.handler.CheckAndHeal(ft.rf)
}

// healthyMaster runs a reconcile at the given offset that finds the master
// answering. The rest of the reconcile doesn't matter.
func (ft *failoverTimeoutTest) healthyMaster(at time.Duration) error {
	ft.now = ft.now.Add(at)
	ft.checker.On("IsRedisRunningQuorum", ft.rf).Once().Return(true)
	ft.checker.On("GetNumberMasters", ft.rf).Once().Return(1, nil)
	ft.checker.On("CheckMasterHealth", ft.rf).Once().Return(true, timeoutMaster, nil)
	ft.checker.On("CheckAllSlavesFromMaster", timeoutMaster, ft.rf).Once().Return(nil)
	ft.checker.On("GetRedisesIPs", ft.rf).Once().Return(nil, errors.New("stop here"))
	return ft.handler.CheckAndHeal(ft.rf)
}

func (ft *failoverTimeoutTest) expectPromotion() {
	ft.checker.On("GetBestReplicaForPromotion", ft.rf).Once().Return(&rfservice.ReplicaInfo{IP: timeoutReplica}, nil)
	ft.healer.On("PromoteBestReplica", timeoutReplica, ft.rf).Once().Return(nil)
}

func (ft *failoverTimeoutTest) assertExpectations(t *testing.T) {
	t.Helper()
	ft.checker.AssertExpectations(t)
	ft.healer.AssertExpectations(t)
}

func TestOperatorManagedModeFailsOverAfterTheFailoverTimeout(t *testing.T) {
	tests := []struct {
		name    string
		timeout *metav1.Duration
		want    time.Duration
	}{
		{name: "default", want: 10 * time.Second},
		{name: "custom", timeout: &metav1.Duration{Duration: 45 * time.Second}, want: 45 * time.Second},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ft := newFailoverTimeoutTest(test.timeout)

			assert.NoError(t, ft.unhealthyMaster(0, false))
			assert.Equal(t, redisfailoverv1.NotHealthyState, ft.rf.Status.State)
			assert.Equal(t, fmt.Sprintf("master unreachable for 0s, failing over after %s", test.want), ft.rf.Status.Message)
			assert.Equal(t, "2026-10-02T12:00:00Z", ft.unreachableSince(), "recorded on the master pod")

			assert.NoError(t, ft.unhealthyMaster(test.want-1500*time.Millisecond, false))
			assert.Equal(t, fmt.Sprintf("master unreachable for %s, failing over after %s", test.want-2*time.Second, test.want), ft.rf.Status.Message)
			assert.Equal(t, []time.Duration{test.want, 1500 * time.Millisecond}, ft.requeues, "reconciled again when the timeout runs out")
			assert.Equal(t, "2026-10-02T12:00:00Z", ft.unreachableSince(), "recorded once")

			assert.NoError(t, ft.unhealthyMaster(1500*time.Millisecond, true))
			assert.Equal(t, redisfailoverv1.HealthyState, ft.rf.Status.State)
			assert.Empty(t, ft.unreachableSince(), "cleared once failed over")

			ft.assertExpectations(t)
		})
	}
}

func TestOperatorManagedModeZeroFailoverTimeoutFailsOverAtOnce(t *testing.T) {
	ft := newFailoverTimeoutTest(&metav1.Duration{})
	assert.NoError(t, ft.unhealthyMaster(0, true))
	assert.Empty(t, ft.requeues)
	ft.assertExpectations(t)
}

// An operator restart or a new leader keeps the deadline recorded on the pod.
func TestOperatorManagedModeFailoverTimeoutSurvivesARestart(t *testing.T) {
	ft := newFailoverTimeoutTest(&metav1.Duration{Duration: 30 * time.Second})
	ft.pods[0].Annotations[masterUnreachableAnnotation] = ft.now.Add(-25 * time.Second).Format(time.RFC3339)

	assert.NoError(t, ft.unhealthyMaster(0, false))
	assert.Equal(t, "master unreachable for 25s, failing over after 30s", ft.rf.Status.Message)
	assert.Equal(t, []time.Duration{5 * time.Second}, ft.requeues)
	assert.Equal(t, "2026-10-02T11:59:35Z", ft.unreachableSince(), "kept")

	ft.restart()
	assert.NoError(t, ft.unhealthyMaster(5*time.Second, true))
	assert.Empty(t, ft.unreachableSince())
	ft.assertExpectations(t)
}

func TestOperatorManagedModeFailoverTimeoutRunOutBeforeARestartFailsOverAtOnce(t *testing.T) {
	ft := newFailoverTimeoutTest(&metav1.Duration{Duration: 30 * time.Second})
	ft.pods[0].Annotations[masterUnreachableAnnotation] = ft.now.Add(-45 * time.Second).Format(time.RFC3339)

	assert.NoError(t, ft.unhealthyMaster(0, true))
	assert.Empty(t, ft.requeues)
	assert.Empty(t, ft.unreachableSince())
	ft.assertExpectations(t)
}

func TestOperatorManagedModeFailoverTimeoutRestartsWhenTheMasterAnswers(t *testing.T) {
	ft := newFailoverTimeoutTest(nil)
	assert.NoError(t, ft.unhealthyMaster(0, false))

	assert.Error(t, ft.healthyMaster(8*time.Second))
	assert.Empty(t, ft.unreachableSince(), "cleared when the master answers")

	// A new blip gets the full timeout again.
	assert.NoError(t, ft.unhealthyMaster(4*time.Second, false))
	assert.Equal(t, "master unreachable for 0s, failing over after 10s", ft.rf.Status.Message)
	assert.NoError(t, ft.unhealthyMaster(9*time.Second, false))
	assert.NoError(t, ft.unhealthyMaster(time.Second, true))

	ft.assertExpectations(t)
}

// After a restart the handler doesn't know whether a pod still carries the
// annotation, so the first healthy reconcile looks once.
func TestOperatorManagedModeClearsALeftoverAnnotationAfterARestart(t *testing.T) {
	ft := newFailoverTimeoutTest(nil)
	ft.pods[0].Annotations[masterUnreachableAnnotation] = "2026-10-01T12:00:00Z"

	assert.Error(t, ft.healthyMaster(0))
	assert.Empty(t, ft.unreachableSince())

	ft.pods[0].Annotations[masterUnreachableAnnotation] = "2026-10-01T12:00:00Z"
	assert.Error(t, ft.healthyMaster(time.Second))
	assert.NotEmpty(t, ft.unreachableSince(), "not looked for again while known clear")
	ft.assertExpectations(t)
}

// A health check that finds no master at all doesn't wait: the counted
// master may have restarted or become a replica since.
func TestOperatorManagedModeNoMasterFoundByHealthCheckFailsOverAtOnce(t *testing.T) {
	ft := newFailoverTimeoutTest(nil)
	ft.checker.On("IsRedisRunningQuorum", ft.rf).Once().Return(true)
	ft.checker.On("GetNumberMasters", ft.rf).Once().Return(1, nil)
	ft.checker.On("CheckMasterHealth", ft.rf).Once().Return(false, "", nil)
	ft.expectPromotion()

	assert.NoError(t, ft.handler.CheckAndHeal(ft.rf))
	assert.Equal(t, redisfailoverv1.HealthyState, ft.rf.Status.State)
	assert.Empty(t, ft.requeues)
	ft.assertExpectations(t)
}

func TestOperatorManagedModeUnhealthyMasterWithoutAPodFailsOverAtOnce(t *testing.T) {
	ft := newFailoverTimeoutTest(nil)
	ft.pods = ft.pods[1:]
	assert.NoError(t, ft.unhealthyMaster(0, true))
	assert.Empty(t, ft.requeues)
	ft.assertExpectations(t)
}

// With no master answering, a master pod that is still there gets the
// timeout. Its clock starts while a ready pod doesn't answer, which itself
// blocks any promotion.
func TestOperatorManagedModeWaitsForAnUnreachableMasterPod(t *testing.T) {
	ft := newFailoverTimeoutTest(nil)

	ft.checker.On("IsRedisRunningQuorum", ft.rf).Once().Return(true)
	ft.checker.On("GetNumberMasters", ft.rf).Once().Return(0, fmt.Errorf("%w: rfr-test-0: i/o timeout", rfservice.ErrRedisNotAnswering))
	assert.Error(t, ft.handler.CheckAndHeal(ft.rf))
	assert.Equal(t, "2026-10-02T12:00:00Z", ft.unreachableSince())

	ft.pods[0] = timeoutRedisPod(timeoutMasterPod, timeoutMaster, true, false)
	ft.pods[0].Annotations[masterUnreachableAnnotation] = "2026-10-02T12:00:00Z"
	noMaster := func(at time.Duration) error {
		ft.now = ft.now.Add(at)
		ft.checker.On("IsRedisRunningQuorum", ft.rf).Once().Return(true)
		ft.checker.On("GetNumberMasters", ft.rf).Once().Return(0, nil)
		return ft.handler.CheckAndHeal(ft.rf)
	}
	assert.NoError(t, noMaster(6*time.Second))
	assert.Equal(t, "master unreachable for 6s, failing over after 10s", ft.rf.Status.Message)
	assert.Equal(t, []time.Duration{4 * time.Second}, ft.requeues)

	ft.expectPromotion()
	assert.NoError(t, noMaster(4*time.Second))
	assert.Empty(t, ft.unreachableSince())

	ft.assertExpectations(t)
}

func TestOperatorManagedModeElectsAtOnceWithoutAnUnreachableMasterPod(t *testing.T) {
	tests := []struct {
		name string
		pod  corev1.Pod
	}{
		{name: "the master pod is gone", pod: timeoutRedisPod("rfr-test-0", timeoutMaster, false, false)},
		{name: "the pod labelled master is ready, so it answered as replica", pod: timeoutRedisPod("rfr-test-0", timeoutMaster, true, true)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ft := newFailoverTimeoutTest(nil)
			ft.pods[0] = test.pod
			ft.checker.On("IsRedisRunningQuorum", ft.rf).Once().Return(true)
			ft.checker.On("GetNumberMasters", ft.rf).Once().Return(0, nil)
			ft.expectPromotion()
			assert.NoError(t, ft.handler.CheckAndHeal(ft.rf))
			ft.assertExpectations(t)
		})
	}
}

func TestOperatorManagedModeFailoverTimeoutErrors(t *testing.T) {
	apiErr := errors.New("api err")

	t.Run("listing pods for the unreachable master", func(t *testing.T) {
		ft := newFailoverTimeoutTest(nil)
		ft.listErr = apiErr
		assert.ErrorIs(t, ft.unhealthyMaster(0, false), apiErr)
		ft.assertExpectations(t)
	})

	t.Run("listing pods for a not ready master", func(t *testing.T) {
		ft := newFailoverTimeoutTest(nil)
		// masterPodStopping lists first; the second listing fails.
		ft.k8s = &mK8SService.Services{}
		ft.k8s.On("UpdateRedisFailoverStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return()
		ft.k8s.On("GetStatefulSetPods", "testns", rfservice.GetRedisName(ft.rf)).Once().Return(&corev1.PodList{}, nil)
		ft.k8s.On("GetStatefulSetPods", "testns", rfservice.GetRedisName(ft.rf)).Once().Return(nil, apiErr)
		ft.restart()
		ft.checker.On("IsRedisRunningQuorum", ft.rf).Once().Return(true)
		ft.checker.On("GetNumberMasters", ft.rf).Once().Return(0, nil)
		assert.ErrorIs(t, ft.handler.CheckAndHeal(ft.rf), apiErr)
		ft.assertExpectations(t)
		ft.k8s.AssertExpectations(t)
	})

	t.Run("recording the unreachable master", func(t *testing.T) {
		ft := newFailoverTimeoutTest(nil)
		ft.patchErr = apiErr
		assert.ErrorIs(t, ft.unhealthyMaster(0, false), apiErr)
		assert.Empty(t, ft.requeues)
		ft.assertExpectations(t)
	})

	t.Run("listing pods to clear the annotation", func(t *testing.T) {
		ft := newFailoverTimeoutTest(nil)
		ft.listErr = apiErr
		ft.checker.On("IsRedisRunningQuorum", ft.rf).Once().Return(true)
		ft.checker.On("GetNumberMasters", ft.rf).Once().Return(1, nil)
		ft.checker.On("CheckMasterHealth", ft.rf).Once().Return(true, timeoutMaster, nil)
		assert.ErrorIs(t, ft.handler.CheckAndHeal(ft.rf), apiErr)
		ft.assertExpectations(t)
	})

	t.Run("clearing the annotation", func(t *testing.T) {
		ft := newFailoverTimeoutTest(nil)
		assert.NoError(t, ft.unhealthyMaster(0, false))
		ft.patchErr = apiErr
		assert.ErrorIs(t, ft.unhealthyMaster(10*time.Second, true), apiErr)
		assert.NotEmpty(t, ft.unreachableSince(), "cleared on a later reconcile")
		ft.assertExpectations(t)
	})
}

func TestFailoverTimeoutForgottenOnDeletion(t *testing.T) {
	ft := newFailoverTimeoutTest(nil)
	assert.Error(t, ft.healthyMaster(0))
	_, cleared := ft.handler.unreachableCleared.Load(failoverKey(ft.rf))
	assert.True(t, cleared)

	now := metav1.Now()
	ft.rf.DeletionTimestamp = &now
	ft.rf.Finalizers = []string{redisFailoverFinalizer}
	ft.k8s.On("PatchRedisFailoverFinalizers", mock.Anything, "testns", "test", mock.Anything, mock.Anything).Return(nil)
	assert.NoError(t, ft.handler.Handle(context.Background(), ft.rf))
	_, cleared = ft.handler.unreachableCleared.Load(failoverKey(ft.rf))
	assert.False(t, cleared)
}
