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

// newFailoverTimeoutTest keeps the pod annotations that the handler sets.
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
	ft.k8s.On("RemovePodAnnotation", "testns", mock.Anything, mock.Anything).Maybe().Return(func(_, name, key string) error {
		if ft.patchErr != nil {
			return ft.patchErr
		}
		for i := range ft.pods {
			if ft.pods[i].Name == name {
				delete(ft.pods[i].Annotations, key)
			}
		}
		return nil
	})
	ft.restart()
	return ft
}

// restart simulates an operator restart or a new leader.
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

// unhealthyMaster moves the clock by at.
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

// healthyMaster moves the clock by at, and the reconcile stops after the
// master check.
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
			assert.Equal(t, fmt.Sprintf("master unreachable since 2026-10-02T12:00:00Z, failing over after %s", test.want), ft.rf.Status.Message)
			assert.Equal(t, "2026-10-02T12:00:00Z", ft.unreachableSince(), "recorded on the master pod")
			waiting := ft.rf.Status

			// Each status change queues a reconcile at once, so the status
			// stays the same during the wait.
			assert.NoError(t, ft.unhealthyMaster(test.want-1500*time.Millisecond, false))
			assert.Equal(t, waiting, ft.rf.Status)
			assert.Equal(t, []time.Duration{test.want, 1500 * time.Millisecond}, ft.requeues, "reconciled again when the timeout runs out")
			assert.Equal(t, "2026-10-02T12:00:00Z", ft.unreachableSince(), "recorded once")

			assert.NoError(t, ft.unhealthyMaster(1500*time.Millisecond, true))
			assert.Equal(t, redisfailoverv1.HealthyState, ft.rf.Status.State)
			assert.NotContains(t, ft.pods[0].Annotations, masterUnreachableAnnotation, "cleared once failed over")

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

// A failed annotation write can't stop a failover without a wait.
func TestOperatorManagedModeZeroFailoverTimeoutDoesNotPatchThePod(t *testing.T) {
	ft := newFailoverTimeoutTest(&metav1.Duration{})
	ft.patchErr = errors.New("api err")
	assert.NoError(t, ft.unhealthyMaster(0, true))
	assert.Equal(t, redisfailoverv1.HealthyState, ft.rf.Status.State)
	ft.k8s.AssertNotCalled(t, "UpdatePodAnnotations", mock.Anything, mock.Anything, mock.Anything)
	ft.assertExpectations(t)
}

func TestOperatorManagedModeFailoverTimeoutSurvivesARestart(t *testing.T) {
	ft := newFailoverTimeoutTest(&metav1.Duration{Duration: 30 * time.Second})
	ft.pods[0].Annotations[masterUnreachableAnnotation] = ft.now.Add(-25 * time.Second).Format(time.RFC3339)

	assert.NoError(t, ft.unhealthyMaster(0, false))
	assert.Equal(t, "master unreachable since 2026-10-02T11:59:35Z, failing over after 30s", ft.rf.Status.Message)
	assert.Equal(t, []time.Duration{5 * time.Second}, ft.requeues)
	assert.Equal(t, "2026-10-02T11:59:35Z", ft.unreachableSince(), "kept")

	ft.restart()
	assert.NoError(t, ft.unhealthyMaster(5*time.Second, true))
	assert.NotContains(t, ft.pods[0].Annotations, masterUnreachableAnnotation)
	ft.assertExpectations(t)
}

func TestOperatorManagedModeFailoverTimeoutRunOutBeforeARestartFailsOverAtOnce(t *testing.T) {
	ft := newFailoverTimeoutTest(&metav1.Duration{Duration: 30 * time.Second})
	ft.pods[0].Annotations[masterUnreachableAnnotation] = ft.now.Add(-45 * time.Second).Format(time.RFC3339)

	assert.NoError(t, ft.unhealthyMaster(0, true))
	assert.Empty(t, ft.requeues)
	assert.NotContains(t, ft.pods[0].Annotations, masterUnreachableAnnotation)
	ft.assertExpectations(t)
}

func TestOperatorManagedModeFailoverTimeoutRestartsWhenTheMasterAnswers(t *testing.T) {
	ft := newFailoverTimeoutTest(nil)
	assert.NoError(t, ft.unhealthyMaster(0, false))

	assert.Error(t, ft.healthyMaster(8*time.Second))
	assert.NotContains(t, ft.pods[0].Annotations, masterUnreachableAnnotation, "cleared when the master answers")

	// A new stall gets the full timeout again.
	assert.NoError(t, ft.unhealthyMaster(4*time.Second, false))
	assert.Equal(t, "master unreachable since 2026-10-02T12:00:12Z, failing over after 10s", ft.rf.Status.Message)
	assert.NoError(t, ft.unhealthyMaster(9*time.Second, false))
	assert.NoError(t, ft.unhealthyMaster(time.Second, true))

	ft.assertExpectations(t)
}

// After a restart, the handler does not know if a pod has the annotation, so
// the first healthy reconcile examines the pods.
func TestOperatorManagedModeClearsALeftoverAnnotationAfterARestart(t *testing.T) {
	ft := newFailoverTimeoutTest(nil)
	ft.pods[0].Annotations[masterUnreachableAnnotation] = "2026-10-01T12:00:00Z"

	assert.Error(t, ft.healthyMaster(0))
	assert.NotContains(t, ft.pods[0].Annotations, masterUnreachableAnnotation)

	ft.pods[0].Annotations[masterUnreachableAnnotation] = "2026-10-01T12:00:00Z"
	assert.Error(t, ft.healthyMaster(time.Second))
	assert.NotEmpty(t, ft.unreachableSince(), "not looked for again while known clear")
	ft.assertExpectations(t)
}

// The counted master can be restarted or a replica now, so a wait only makes
// the outage longer.
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

// The wait starts at the ErrRedisNotAnswering error, while the pod is still
// ready.
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
	assert.Equal(t, "master unreachable since 2026-10-02T12:00:00Z, failing over after 10s", ft.rf.Status.Message)
	assert.Equal(t, []time.Duration{4 * time.Second}, ft.requeues)

	ft.expectPromotion()
	assert.NoError(t, noMaster(4*time.Second))
	assert.NotContains(t, ft.pods[0].Annotations, masterUnreachableAnnotation)

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

// healthyMasterReconcile moves the clock by at and runs a full reconcile with
// nothing to change.
func (ft *failoverTimeoutTest) healthyMasterReconcile(at time.Duration) error {
	ft.now = ft.now.Add(at)
	ft.checker.On("IsRedisRunningQuorum", ft.rf).Once().Return(true)
	ft.checker.On("GetNumberMasters", ft.rf).Once().Return(1, nil)
	ft.checker.On("CheckMasterHealth", ft.rf).Once().Return(true, timeoutMaster, nil)
	ft.checker.On("CheckAllSlavesFromMaster", timeoutMaster, ft.rf).Once().Return(nil)
	ft.checker.On("GetRedisesIPs", ft.rf).Twice().Return([]string{timeoutMaster}, nil)
	ft.healer.On("SetRedisCustomConfig", timeoutMaster, ft.rf).Once().Return(nil)
	ft.checker.On("GetMasterIP", ft.rf).Once().Return(timeoutMaster, nil)
	ft.checker.On("GetStatefulSetUpdateRevision", ft.rf).Once().Return("1", nil)
	ft.checker.On("GetRedisesSlavesPods", ft.rf).Once().Return([]string{}, nil)
	ft.checker.On("GetRedisesMasterPod", ft.rf).Once().Return(timeoutMaster, nil)
	ft.checker.On("GetRedisRevisionHash", timeoutMaster, ft.rf).Once().Return("1", nil)
	return ft.handler.CheckAndHeal(ft.rf)
}

func TestOperatorManagedModeFailoverTimeoutErrors(t *testing.T) {
	apiErr := errors.New("api err")
	assertNotHealthy := func(t *testing.T, ft *failoverTimeoutTest, err error, message string) {
		t.Helper()
		assert.ErrorIs(t, err, apiErr)
		assert.Equal(t, redisfailoverv1.NotHealthyState, ft.rf.Status.State)
		assert.Equal(t, message, ft.rf.Status.Message)
	}

	t.Run("looking up the pod of a master that doesn't answer", func(t *testing.T) {
		ft := newFailoverTimeoutTest(nil)
		ft.listErr = apiErr
		assertNotHealthy(t, ft, ft.unhealthyMaster(0, false), "unable to look up the master pod")
		ft.assertExpectations(t)
	})

	t.Run("looking up a not ready master pod", func(t *testing.T) {
		ft := newFailoverTimeoutTest(nil)
		// The first listing is for masterPodStopping. The second listing fails.
		ft.k8s = &mK8SService.Services{}
		ft.k8s.On("UpdateRedisFailoverStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return()
		ft.k8s.On("GetStatefulSetPods", "testns", rfservice.GetRedisName(ft.rf)).Once().Return(&corev1.PodList{}, nil)
		ft.k8s.On("GetStatefulSetPods", "testns", rfservice.GetRedisName(ft.rf)).Once().Return(nil, apiErr)
		ft.restart()
		ft.checker.On("IsRedisRunningQuorum", ft.rf).Once().Return(true)
		ft.checker.On("GetNumberMasters", ft.rf).Once().Return(0, nil)
		assertNotHealthy(t, ft, ft.handler.CheckAndHeal(ft.rf), "unable to look up the master pod")
		ft.assertExpectations(t)
		ft.k8s.AssertExpectations(t)
	})

	t.Run("recording the unreachable master", func(t *testing.T) {
		ft := newFailoverTimeoutTest(nil)
		ft.patchErr = apiErr
		assertNotHealthy(t, ft, ft.unhealthyMaster(0, false), "unable to record when the master became unreachable")
		assert.Empty(t, ft.requeues)
		ft.assertExpectations(t)
	})

	// The master answers again, so a failed clear keeps the RedisFailover
	// healthy.
	t.Run("clearing once the master answers", func(t *testing.T) {
		ft := newFailoverTimeoutTest(nil)
		assert.NoError(t, ft.unhealthyMaster(0, false))

		ft.patchErr = apiErr
		assert.NoError(t, ft.healthyMasterReconcile(5*time.Second))
		assert.Equal(t, redisfailoverv1.HealthyState, ft.rf.Status.State)
		assert.NotEmpty(t, ft.unreachableSince())

		ft.patchErr = nil
		assert.NoError(t, ft.healthyMasterReconcile(time.Second))
		assert.NotContains(t, ft.pods[0].Annotations, masterUnreachableAnnotation, "cleared on the next reconcile")
		ft.assertExpectations(t)
	})

	t.Run("listing pods to clear once the master answers", func(t *testing.T) {
		ft := newFailoverTimeoutTest(nil)
		ft.pods[0].Annotations[masterUnreachableAnnotation] = "2026-10-01T12:00:00Z"

		ft.listErr = apiErr
		assert.NoError(t, ft.healthyMasterReconcile(0))
		assert.Equal(t, redisfailoverv1.HealthyState, ft.rf.Status.State)

		ft.listErr = nil
		assert.NoError(t, ft.healthyMasterReconcile(time.Second))
		assert.NotContains(t, ft.pods[0].Annotations, masterUnreachableAnnotation, "cleared on the next reconcile")
		ft.assertExpectations(t)
	})

	t.Run("clearing after a failover", func(t *testing.T) {
		ft := newFailoverTimeoutTest(nil)
		assert.NoError(t, ft.unhealthyMaster(0, false))

		ft.patchErr = apiErr
		assert.NoError(t, ft.unhealthyMaster(10*time.Second, true))
		assert.Equal(t, redisfailoverv1.HealthyState, ft.rf.Status.State)
		assert.NotEmpty(t, ft.unreachableSince())

		ft.patchErr = nil
		assert.NoError(t, ft.healthyMasterReconcile(time.Second))
		assert.NotContains(t, ft.pods[0].Annotations, masterUnreachableAnnotation, "cleared on the next reconcile")
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
