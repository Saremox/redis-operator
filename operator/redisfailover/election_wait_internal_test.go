package redisfailover

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	mRFService "github.com/saremox/redis-operator/mocks/operator/redisfailover/service"
	mK8SService "github.com/saremox/redis-operator/mocks/service/k8s"
	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
)

var electionNow = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

// electionPod builds a Redis pod of the given age.
func electionPod(name string, phase corev1.PodPhase, ready bool, age time.Duration) corev1.Pod {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			CreationTimestamp: metav1.NewTime(electionNow.Add(-age)),
		},
		Status: corev1.PodStatus{Phase: phase},
	}
	if ready {
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	}
	return pod
}

type electionTest struct {
	rf       *redisfailoverv1.RedisFailover
	handler  *RedisFailoverHandler
	requeues []time.Duration
}

func newElectionTest(t *testing.T, replicas int32, pods []corev1.Pod, listErr error) *electionTest {
	t.Helper()

	et := &electionTest{
		rf: &redisfailoverv1.RedisFailover{
			ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "testns"},
			Spec:       redisfailoverv1.RedisFailoverSpec{Redis: redisfailoverv1.RedisSettings{Replicas: replicas}},
		},
	}
	k8s := &mK8SService.Services{}
	k8s.On("GetStatefulSetPods", "testns", rfservice.GetRedisName(et.rf)).Maybe().
		Return(&corev1.PodList{Items: pods}, listErr)
	et.handler = NewRedisFailoverHandler(Config{}, &mRFService.RedisFailoverClient{}, &mRFService.RedisFailoverCheck{}, &mRFService.RedisFailoverHeal{}, k8s, metrics.Dummy, log.Dummy)
	et.handler.now = func() time.Time { return electionNow }
	et.handler.requeue = func(_ string, after time.Duration) {
		et.requeues = append(et.requeues, after)
	}
	return et
}

// A pod that is not ready can hold newer data on its volume, and it answers
// no INFO yet. The election waits for the rest of failoverTimeout.
func TestElectionWaitsForAStartingPodBelowAQuorum(t *testing.T) {
	et := newElectionTest(t, 3, []corev1.Pod{
		electionPod("rfr-test-0", corev1.PodRunning, true, time.Hour),
		electionPod("rfr-test-1", corev1.PodPending, false, 4*time.Second),
	}, nil)

	wait, err := et.handler.waitForStartingPods(et.rf)

	require.NoError(t, err)
	assert.True(t, wait, "a starting pod must hold the election")
	assert.Equal(t, []time.Duration{6 * time.Second}, et.requeues)
	assert.Contains(t, et.rf.Status.Message, "rfr-test-1")
	assert.Equal(t, redisfailoverv1.NotHealthyState, et.rf.Status.State)
}

// The shape of the incident: a pod that the scheduler cannot place. It is
// older than failoverTimeout, so the election goes on without it.
func TestElectionDoesNotWaitForAPodThatCannotBeScheduled(t *testing.T) {
	et := newElectionTest(t, 2, []corev1.Pod{
		electionPod("rfr-test-0", corev1.PodRunning, true, time.Hour),
		electionPod("rfr-test-1", corev1.PodPending, false, 40*time.Minute),
	}, nil)

	wait, err := et.handler.waitForStartingPods(et.rf)

	require.NoError(t, err)
	assert.False(t, wait, "an unschedulable pod must not hold the election")
	assert.Empty(t, et.requeues)
}

// With a quorum the ranking already sees a majority of the pods, so the
// election does not wait.
func TestElectionDoesNotWaitWithAQuorum(t *testing.T) {
	et := newElectionTest(t, 3, []corev1.Pod{
		electionPod("rfr-test-0", corev1.PodRunning, true, time.Hour),
		electionPod("rfr-test-1", corev1.PodRunning, true, time.Hour),
		electionPod("rfr-test-2", corev1.PodPending, false, time.Second),
	}, nil)

	wait, err := et.handler.waitForStartingPods(et.rf)

	require.NoError(t, err)
	assert.False(t, wait)
	assert.Empty(t, et.requeues)
}

// A stopping pod never becomes ready, so it must not hold the election.
func TestElectionDoesNotWaitForAStoppingPod(t *testing.T) {
	stopping := electionPod("rfr-test-1", corev1.PodRunning, false, time.Second)
	deleted := metav1.NewTime(electionNow)
	stopping.DeletionTimestamp = &deleted
	et := newElectionTest(t, 3, []corev1.Pod{
		electionPod("rfr-test-0", corev1.PodRunning, true, time.Hour),
		stopping,
	}, nil)

	wait, err := et.handler.waitForStartingPods(et.rf)

	require.NoError(t, err)
	assert.False(t, wait)
}

// Without a timeout there is no deadline to keep.
func TestElectionDoesNotWaitWithoutAFailoverTimeout(t *testing.T) {
	et := newElectionTest(t, 3, []corev1.Pod{
		electionPod("rfr-test-0", corev1.PodRunning, true, time.Hour),
		electionPod("rfr-test-1", corev1.PodPending, false, time.Second),
	}, nil)
	et.rf.Spec.Sentinel.FailoverTimeout = &metav1.Duration{}

	wait, err := et.handler.waitForStartingPods(et.rf)

	require.NoError(t, err)
	assert.False(t, wait)
}

func TestElectionReportsAFailedPodList(t *testing.T) {
	et := newElectionTest(t, 3, nil, errors.New("boom"))

	wait, err := et.handler.waitForStartingPods(et.rf)

	require.Error(t, err)
	assert.False(t, wait)
	assert.Equal(t, redisPodLookupFailed, et.rf.Status.Message)
}

// A failover that starts has no master label yet, so the gate logs no
// Warning on every reconcile.
func TestHadMaster(t *testing.T) {
	master := electionPod("rfr-test-0", corev1.PodPending, false, time.Second)
	master.Labels = map[string]string{"redisfailovers-role": "master"}

	tests := []struct {
		name    string
		pods    []corev1.Pod
		listErr error
		want    bool
	}{
		{name: "a new failover", pods: []corev1.Pod{electionPod("rfr-test-0", corev1.PodPending, false, time.Second)}},
		{name: "no pod", pods: nil},
		{name: "a master label", pods: []corev1.Pod{master}, want: true},
		{name: "a failed list", listErr: errors.New("boom"), want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			et := newElectionTest(t, 3, test.pods, test.listErr)

			assert.Equal(t, test.want, et.handler.hadMaster(et.rf))
		})
	}
}
