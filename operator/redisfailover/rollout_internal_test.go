package redisfailover

import (
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
	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
)

func TestUpdateRedisesPodsReportsAStalledRollout(t *testing.T) {
	defer func(d time.Duration) { rolloutStallTimeout = d }(rolloutStallTimeout)
	rolloutStallTimeout = 50 * time.Millisecond

	rf := &redisfailoverv1.RedisFailover{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "testns"},
		Spec:       redisfailoverv1.RedisFailoverSpec{Redis: redisfailoverv1.RedisSettings{Replicas: 2}},
	}
	ready := []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	pods := []corev1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "rfr-test-0", UID: "m", Labels: map[string]string{appsv1.ControllerRevisionHashLabelKey: "old"}},
			Status:     corev1.PodStatus{PodIP: "10.0.0.1", Conditions: ready},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "rfr-test-1", UID: "r1", Labels: map[string]string{appsv1.ControllerRevisionHashLabelKey: "new"}},
			Status:     corev1.PodStatus{PodIP: "10.0.0.2"},
		},
	}
	synced := false

	mrfc := &mRFService.RedisFailoverCheck{}
	mrfc.On("GetRedisesIPs", rf).Return([]string{"10.0.0.1", "10.0.0.2"}, nil)
	mrfc.On("GetMasterIP", rf).Return("10.0.0.1", nil)
	mrfc.On("CheckRedisSlavesReady", "10.0.0.2", rf).Return(func(string, *redisfailoverv1.RedisFailover) (bool, error) {
		return synced, nil
	})
	mrfc.On("GetStatefulSetUpdateRevision", rf).Return("new", nil)
	mrfc.On("GetRedisesSlavesPods", rf).Return([]string{"rfr-test-1"}, nil)
	mrfc.On("GetRedisRevisionHash", "rfr-test-1", rf).Return("new", nil)
	mrfc.On("GetRedisesMasterPod", rf).Return("rfr-test-0", nil)
	mrfc.On("GetRedisRevisionHash", "rfr-test-0", rf).Return(func(string, *redisfailoverv1.RedisFailover) (string, error) {
		return pods[0].Labels[appsv1.ControllerRevisionHashLabelKey], nil
	})
	mk := &mK8SService.Services{}
	mk.On("GetStatefulSetPods", "testns", "rfr-test").Return(func(string, string) (*corev1.PodList, error) {
		return &corev1.PodList{Items: pods}, nil
	})
	mrfh := &mRFService.RedisFailoverHeal{}
	handler := NewRedisFailoverHandler(Config{}, &mRFService.RedisFailoverClient{}, mrfc, mrfh, mk, metrics.Dummy, log.Dummy)

	update := func() string {
		rf.Status = redisfailoverv1.RedisFailoverStatus{}
		assert.NoError(t, handler.UpdateRedisesPods(rf))
		return rf.Status.Message
	}

	// A replaced replica that doesn't sync is reported once the rollout has
	// waited on it for longer than the bound.
	assert.Empty(t, update())
	time.Sleep(2 * rolloutStallTimeout)
	assert.Equal(t, "rollout waiting on pod rfr-test-1 for more than 0m: not synced with the master", update())

	// The pod recreated starts over.
	pods[1].UID = "r2"
	assert.Empty(t, update())

	// Once it syncs, the rollout moves on, and a later wait starts over.
	time.Sleep(2 * rolloutStallTimeout)
	synced = true
	pods[1].Status.Conditions = ready
	mrfh.On("ResizePodInPlace", rf, "rfr-test-0", "new").Once().Return(rfservice.ResizeResult{Action: rfservice.ResizeRecreate}, nil)
	mrfh.On("DeletePod", "rfr-test-0", rf).Once().Return(nil)
	assert.Empty(t, update())
	synced = false
	assert.Empty(t, update())

	// A replica that isn't synced while no rollout is pending is not reported.
	pods[0].Labels[appsv1.ControllerRevisionHashLabelKey] = "new"
	time.Sleep(2 * rolloutStallTimeout)
	assert.Empty(t, update())
	mrfh.AssertExpectations(t)
}
