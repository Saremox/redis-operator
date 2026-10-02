package redisfailover

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	mRFService "github.com/saremox/redis-operator/mocks/operator/redisfailover/service"
	mK8SService "github.com/saremox/redis-operator/mocks/service/k8s"
)

var podReady = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}

// rolloutTest is a two pod RedisFailover whose master rfr-test-0 is on the old
// revision and whose replica rfr-test-1 is on the new one.
type rolloutTest struct {
	t       *testing.T
	rf      *redisfailoverv1.RedisFailover
	pods    []corev1.Pod
	synced  bool
	revErr  error
	podsErr error
	heal    *mRFService.RedisFailoverHeal
	handler *RedisFailoverHandler
}

func newRolloutTest(t *testing.T) *rolloutTest {
	timeout := rolloutStallTimeout
	t.Cleanup(func() { rolloutStallTimeout = timeout })
	rolloutStallTimeout = 50 * time.Millisecond

	rt := &rolloutTest{
		t: t,
		rf: &redisfailoverv1.RedisFailover{
			ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "testns"},
			Spec:       redisfailoverv1.RedisFailoverSpec{Redis: redisfailoverv1.RedisSettings{Replicas: 2}},
		},
		pods: []corev1.Pod{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "rfr-test-0", UID: "m", Labels: map[string]string{appsv1.ControllerRevisionHashLabelKey: "old"}},
				Status:     corev1.PodStatus{PodIP: "10.0.0.1", Conditions: podReady},
			},
			{
				ObjectMeta: metav1.ObjectMeta{Name: "rfr-test-1", UID: "r1", Labels: map[string]string{appsv1.ControllerRevisionHashLabelKey: "new"}},
				Status:     corev1.PodStatus{PodIP: "10.0.0.2"},
			},
		},
		heal: &mRFService.RedisFailoverHeal{},
	}
	rf := rt.rf
	mrfc := &mRFService.RedisFailoverCheck{}
	mrfc.On("GetRedisesIPs", rf).Return([]string{"10.0.0.1", "10.0.0.2"}, nil)
	mrfc.On("GetMasterIP", rf).Return("10.0.0.1", nil)
	mrfc.On("CheckRedisSlavesReady", "10.0.0.2", rf).Return(func(string, *redisfailoverv1.RedisFailover) (bool, error) {
		return rt.synced, nil
	})
	mrfc.On("GetStatefulSetUpdateRevision", rf).Return(func(*redisfailoverv1.RedisFailover) (string, error) {
		return "new", rt.revErr
	})
	mrfc.On("GetRedisesSlavesPods", rf).Return([]string{"rfr-test-1"}, nil)
	mrfc.On("GetRedisRevisionHash", "rfr-test-1", rf).Return("new", nil)
	mrfc.On("GetRedisesMasterPod", rf).Return("rfr-test-0", nil)
	mrfc.On("GetRedisRevisionHash", "rfr-test-0", rf).Return(func(string, *redisfailoverv1.RedisFailover) (string, error) {
		return rt.pods[0].Labels[appsv1.ControllerRevisionHashLabelKey], nil
	})
	mk := &mK8SService.Services{}
	mk.On("GetStatefulSetPods", "testns", "rfr-test").Return(func(string, string) (*corev1.PodList, error) {
		return &corev1.PodList{Items: rt.pods}, rt.podsErr
	})
	rt.handler = NewRedisFailoverHandler(Config{}, &mRFService.RedisFailoverClient{}, mrfc, rt.heal, mk, metrics.Dummy, log.Dummy)
	return rt
}

// update runs UpdateRedisesPods on a fresh status, keeping message, and
// returns the status message.
func (rt *rolloutTest) update(message string) string {
	rt.rf.Status = redisfailoverv1.RedisFailoverStatus{Message: message}
	assert.NoError(rt.t, rt.handler.UpdateRedisesPods(rt.rf))
	return rt.rf.Status.Message
}

func TestUpdateRedisesPodsReportsAStalledRollout(t *testing.T) {
	rt := newRolloutTest(t)

	// A replaced replica that doesn't sync is reported once the rollout has
	// waited on it for longer than the bound.
	assert.Empty(t, rt.update(""))
	time.Sleep(2 * rolloutStallTimeout)
	assert.Equal(t, "rollout waiting on pod rfr-test-1 for more than 0m: not synced with the master", rt.update(""))

	// The pod recreated starts over.
	rt.pods[1].UID = "r2"
	assert.Empty(t, rt.update(""))

	// Once it syncs, the rollout moves on, and a later wait starts over.
	time.Sleep(2 * rolloutStallTimeout)
	rt.synced = true
	rt.pods[1].Status.Conditions = podReady
	rt.heal.On("DeletePod", "rfr-test-0", rt.rf).Once().Return(nil)
	assert.Empty(t, rt.update(""))
	rt.synced = false
	assert.Empty(t, rt.update(""))

	// A replica that isn't synced while no rollout is pending is not reported.
	rt.pods[0].Labels[appsv1.ControllerRevisionHashLabelKey] = "new"
	time.Sleep(2 * rolloutStallTimeout)
	assert.Empty(t, rt.update(""))
	rt.heal.AssertExpectations(t)
}

func TestUpdateRedisesPodsReportsAStalledReplacement(t *testing.T) {
	tests := []struct {
		name string
		// pods changes the replica, which is synced, so the rollout waits in
		// redisPodsSettled before replacing the master.
		pods func(pods []corev1.Pod) []corev1.Pod
		want string
	}{
		{
			name: "replaced pod not ready",
			pods: func(pods []corev1.Pod) []corev1.Pod { return pods },
			want: "rollout waiting on pod rfr-test-1 for more than 0m: not ready",
		},
		{
			name: "pod terminating",
			pods: func(pods []corev1.Pod) []corev1.Pod {
				pods[1].DeletionTimestamp = &metav1.Time{Time: time.Now()}
				return pods
			},
			want: "rollout waiting on pod rfr-test-1 for more than 0m: terminating",
		},
		{
			name: "pod missing",
			pods: func(pods []corev1.Pod) []corev1.Pod { return pods[:1] },
			want: "rollout waiting for more than 0m: 1 of 2 pods exist",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rt := newRolloutTest(t)
			rt.synced = true
			rt.pods = test.pods(rt.pods)

			assert.Empty(t, rt.update(""))
			time.Sleep(2 * rolloutStallTimeout)
			assert.Equal(t, test.want, rt.update(""))
			rt.heal.AssertExpectations(t)
		})
	}
}

func TestUpdateRedisesPodsKeepsAnEarlierMessage(t *testing.T) {
	rt := newRolloutTest(t)

	assert.Equal(t, "maxmemory kept at 100mb", rt.update("maxmemory kept at 100mb"))
	time.Sleep(2 * rolloutStallTimeout)
	assert.Equal(t, "maxmemory kept at 100mb; rollout waiting on pod rfr-test-1 for more than 0m: not synced with the master",
		rt.update("maxmemory kept at 100mb"))
}

func TestUpdateRedisesPodsKeepsWaitingAcrossErrors(t *testing.T) {
	for _, step := range []string{"update revision", "pods"} {
		t.Run(step, func(t *testing.T) {
			rt := newRolloutTest(t)
			assert.Empty(t, rt.update(""))
			time.Sleep(2 * rolloutStallTimeout)

			// A failed lookup is returned and doesn't restart the wait.
			err := errors.New("lookup failed")
			if step == "pods" {
				rt.podsErr = err
			} else {
				rt.revErr = err
			}
			rt.rf.Status = redisfailoverv1.RedisFailoverStatus{}
			assert.Equal(t, err, rt.handler.UpdateRedisesPods(rt.rf))
			assert.Empty(t, rt.rf.Status.Message)

			rt.podsErr, rt.revErr = nil, nil
			assert.Equal(t, "rollout waiting on pod rfr-test-1 for more than 0m: not synced with the master", rt.update(""))
		})
	}
}

func TestRolloutWaitString(t *testing.T) {
	assert.Equal(t, "pod rfr-test-1 is not ready", (&rolloutWait{pod: "rfr-test-1", reason: "not ready"}).String())
	assert.Equal(t, "2 of 3 pods exist", (&rolloutWait{reason: "2 of 3 pods exist"}).String())
}
