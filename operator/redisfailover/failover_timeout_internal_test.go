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
	timeoutMaster  = "10.0.0.1"
	timeoutReplica = "10.0.0.2"
)

type failoverTimeoutTest struct {
	handler  *RedisFailoverHandler
	rf       *redisfailoverv1.RedisFailover
	checker  *mRFService.RedisFailoverCheck
	healer   *mRFService.RedisFailoverHeal
	k8s      *mK8SService.Services
	now      time.Time
	requeues []time.Duration
}

func newFailoverTimeoutTest(timeout *metav1.Duration) *failoverTimeoutTest {
	ft := &failoverTimeoutTest{
		rf: &redisfailoverv1.RedisFailover{
			ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "testns"},
			Spec:       redisfailoverv1.RedisFailoverSpec{Sentinel: redisfailoverv1.SentinelSettings{FailoverTimeout: timeout}},
		},
		checker: &mRFService.RedisFailoverCheck{},
		healer:  &mRFService.RedisFailoverHeal{},
		k8s:     &mK8SService.Services{},
		now:     time.Now(),
	}
	ft.healer.On("ApplyPassword", mock.Anything, mock.Anything, mock.Anything).Maybe().Return(true, nil)
	ft.healer.On("ApplySentinelPassword", mock.Anything, mock.Anything).Maybe().Return(true, nil)
	ft.k8s.On("UpdateRedisFailoverStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return()
	ft.handler = NewRedisFailoverHandler(Config{}, &mRFService.RedisFailoverClient{}, ft.checker, ft.healer, ft.k8s, metrics.Dummy, log.Dummy)
	ft.handler.now = func() time.Time { return ft.now }
	ft.handler.requeue = func(key string, after time.Duration) {
		if key == "testns/test" {
			ft.requeues = append(ft.requeues, after)
		}
	}
	return ft
}

// unhealthyMaster runs a reconcile at the given offset that finds the master
// counted but not answering, expecting a promotion or not.
func (ft *failoverTimeoutTest) unhealthyMaster(t *testing.T, at time.Duration, wantPromote bool) error {
	t.Helper()
	ft.now = ft.now.Add(at)
	ft.checker.On("IsRedisRunningQuorum", ft.rf).Once().Return(true)
	ft.checker.On("GetNumberMasters", ft.rf).Once().Return(1, nil)
	ft.checker.On("CheckMasterHealth", ft.rf).Once().Return(false, timeoutMaster, nil)
	if wantPromote {
		ft.expectPromotion()
	}
	return ft.handler.CheckAndHeal(ft.rf)
}

func (ft *failoverTimeoutTest) expectPromotion() {
	ft.checker.On("GetBestReplicaForPromotion", ft.rf).Once().Return(&rfservice.ReplicaInfo{IP: timeoutReplica}, nil)
	ft.healer.On("PromoteBestReplica", timeoutReplica, ft.rf).Once().Return(nil)
}

func (ft *failoverTimeoutTest) tracked() bool {
	_, ok := ft.handler.masterUnreachable.Load(failoverKey(ft.rf))
	return ok
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

			assert.NoError(t, ft.unhealthyMaster(t, 0, false))
			assert.Equal(t, redisfailoverv1.NotHealthyState, ft.rf.Status.State)
			assert.Equal(t, fmt.Sprintf("master unreachable for 0s, failing over after %s", test.want), ft.rf.Status.Message)

			assert.NoError(t, ft.unhealthyMaster(t, test.want-1500*time.Millisecond, false))
			assert.Equal(t, fmt.Sprintf("master unreachable for %s, failing over after %s", test.want-2*time.Second, test.want), ft.rf.Status.Message)
			assert.Equal(t, []time.Duration{test.want, 1500 * time.Millisecond}, ft.requeues, "reconciled again when the timeout runs out")

			assert.NoError(t, ft.unhealthyMaster(t, 1500*time.Millisecond, true))
			assert.Equal(t, redisfailoverv1.HealthyState, ft.rf.Status.State)
			assert.False(t, ft.tracked(), "forgotten once failed over")

			ft.checker.AssertExpectations(t)
			ft.healer.AssertExpectations(t)
		})
	}
}

func TestOperatorManagedModeZeroFailoverTimeoutFailsOverAtOnce(t *testing.T) {
	ft := newFailoverTimeoutTest(&metav1.Duration{})
	assert.NoError(t, ft.unhealthyMaster(t, 0, true))
	assert.Empty(t, ft.requeues)
	ft.healer.AssertExpectations(t)
}

func TestOperatorManagedModeFailoverTimeoutRestartsWhenTheMasterAnswers(t *testing.T) {
	ft := newFailoverTimeoutTest(nil)
	assert.NoError(t, ft.unhealthyMaster(t, 0, false))

	// The master answers again. The rest of this reconcile doesn't matter.
	ft.now = ft.now.Add(8 * time.Second)
	ft.checker.On("IsRedisRunningQuorum", ft.rf).Once().Return(true)
	ft.checker.On("GetNumberMasters", ft.rf).Once().Return(1, nil)
	ft.checker.On("CheckMasterHealth", ft.rf).Once().Return(true, timeoutMaster, nil)
	ft.checker.On("CheckAllSlavesFromMaster", timeoutMaster, ft.rf).Once().Return(nil)
	ft.checker.On("GetRedisesIPs", ft.rf).Once().Return(nil, errors.New("stop here"))
	assert.Error(t, ft.handler.CheckAndHeal(ft.rf))
	assert.False(t, ft.tracked())

	// A new blip gets the full timeout again.
	assert.NoError(t, ft.unhealthyMaster(t, 4*time.Second, false))
	assert.Equal(t, "master unreachable for 0s, failing over after 10s", ft.rf.Status.Message)
	assert.NoError(t, ft.unhealthyMaster(t, 9*time.Second, false))
	assert.NoError(t, ft.unhealthyMaster(t, time.Second, true))

	ft.checker.AssertExpectations(t)
	ft.healer.AssertExpectations(t)
}

func timeoutRedisPod(master, ready bool) corev1.Pod {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"redisfailovers-role": "slave"}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	if master {
		pod.Labels["redisfailovers-role"] = "master"
	}
	if ready {
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	}
	return pod
}

// With no master answering, a master pod that is still there gets the
// timeout. Its clock starts while a ready pod doesn't answer, which itself
// blocks any promotion.
func TestOperatorManagedModeWaitsForAnUnreachableMasterPod(t *testing.T) {
	ft := newFailoverTimeoutTest(nil)
	ft.k8s.On("GetStatefulSetPods", "testns", rfservice.GetRedisName(ft.rf)).Return(&corev1.PodList{Items: []corev1.Pod{
		timeoutRedisPod(true, false), timeoutRedisPod(false, true), timeoutRedisPod(false, true),
	}}, nil)

	ft.checker.On("IsRedisRunningQuorum", ft.rf).Once().Return(true)
	ft.checker.On("GetNumberMasters", ft.rf).Once().Return(0, fmt.Errorf("%w: rfr-test-0: i/o timeout", rfservice.ErrRedisNotAnswering))
	assert.Error(t, ft.handler.CheckAndHeal(ft.rf))
	assert.True(t, ft.tracked())

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
	assert.False(t, ft.tracked())

	ft.checker.AssertExpectations(t)
	ft.healer.AssertExpectations(t)
}

func TestOperatorManagedModeElectsAtOnceWithoutAnUnreachableMasterPod(t *testing.T) {
	tests := []struct {
		name string
		pod  corev1.Pod
	}{
		{name: "the master pod is gone", pod: timeoutRedisPod(false, false)},
		{name: "the pod labelled master is ready, so it answered as replica", pod: timeoutRedisPod(true, true)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ft := newFailoverTimeoutTest(nil)
			ft.k8s.On("GetStatefulSetPods", "testns", rfservice.GetRedisName(ft.rf)).Return(&corev1.PodList{Items: []corev1.Pod{test.pod, timeoutRedisPod(false, true)}}, nil)
			ft.checker.On("IsRedisRunningQuorum", ft.rf).Once().Return(true)
			ft.checker.On("GetNumberMasters", ft.rf).Once().Return(0, nil)
			ft.expectPromotion()
			assert.NoError(t, ft.handler.CheckAndHeal(ft.rf))
			ft.healer.AssertExpectations(t)
		})
	}
}

func TestFailoverTimeoutForgottenOnDeletion(t *testing.T) {
	ft := newFailoverTimeoutTest(nil)
	assert.NoError(t, ft.unhealthyMaster(t, 0, false))
	assert.True(t, ft.tracked())

	now := metav1.Now()
	ft.rf.DeletionTimestamp = &now
	ft.rf.Finalizers = []string{redisFailoverFinalizer}
	ft.k8s.On("PatchRedisFailoverFinalizers", mock.Anything, "testns", "test", mock.Anything, mock.Anything).Return(nil)
	assert.NoError(t, ft.handler.Handle(context.Background(), ft.rf))
	assert.False(t, ft.tracked())
}
