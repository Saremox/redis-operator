package service_test

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	mK8SService "github.com/saremox/redis-operator/mocks/service/k8s"
	mRedisService "github.com/saremox/redis-operator/mocks/service/redis"
	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
)

// TestGetSentinelMasterIPsKeepsASentinelWithAnError covers the running
// Sentinels only, also the Sentinels in deletion. A Sentinel that gives an
// error has an empty entry.
func TestGetSentinelMasterIPsKeepsASentinelWithAnError(t *testing.T) {
	rf := generateRF()
	pods := &corev1.PodList{Items: []corev1.Pod{
		{Status: corev1.PodStatus{PodIP: "1.1.1.1", Phase: corev1.PodRunning}},
		{Status: corev1.PodStatus{PodIP: "2.2.2.2", Phase: corev1.PodRunning}},
		{Status: corev1.PodStatus{PodIP: "3.3.3.3", Phase: corev1.PodRunning}},
		{Status: corev1.PodStatus{PodIP: "4.4.4.4", Phase: corev1.PodPending}},
		{ObjectMeta: metav1.ObjectMeta{DeletionTimestamp: &metav1.Time{}}, Status: corev1.PodStatus{PodIP: "5.5.5.5", Phase: corev1.PodRunning}},
		{ObjectMeta: metav1.ObjectMeta{DeletionTimestamp: &metav1.Time{}}, Status: corev1.PodStatus{PodIP: "6.6.6.6", Phase: corev1.PodRunning}},
	}}
	ms := &mK8SService.Services{}
	ms.On("GetDeploymentPods", namespace, rfservice.GetSentinelName(rf)).Once().Return(pods, nil)
	mr := &mRedisService.Client{}
	mr.On("GetSentinelMasterIP", "1.1.1.1").Once().Return("10.0.0.1", nil)
	mr.On("GetSentinelMasterIP", "2.2.2.2").Once().Return("", errors.New("i/o timeout"))
	mr.On("GetSentinelMasterIP", "3.3.3.3").Once().Return("127.0.0.1", nil)
	mr.On("GetSentinelMasterIP", "5.5.5.5").Once().Return("10.0.0.1", nil)
	mr.On("GetSentinelMasterIP", "6.6.6.6").Once().Return("", errors.New("connection refused"))
	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	ips, err := checker.GetSentinelMasterIPs(rf)

	require.NoError(t, err)
	assert.Equal(t, []string{"10.0.0.1", "", "127.0.0.1", "10.0.0.1", ""}, ips)
	mr.AssertExpectations(t)
}

func TestGetSentinelMasterIPsWithoutTheSentinelPods(t *testing.T) {
	rf := generateRF()
	ms := &mK8SService.Services{}
	ms.On("GetDeploymentPods", namespace, rfservice.GetSentinelName(rf)).Once().Return(nil, errors.New("boom"))
	checker := rfservice.NewRedisFailoverChecker(ms, &mRedisService.Client{}, log.DummyLogger{}, metrics.Dummy)

	ips, err := checker.GetSentinelMasterIPs(rf)

	require.EqualError(t, err, "boom")
	assert.Nil(t, ips)
}

func TestSentinelFailoverWindow(t *testing.T) {
	rf := generateRF()
	assert.Equal(t, 15*time.Second, rfservice.SentinelFailoverWindow(rf), "the defaults")
	rf.Spec.Sentinel.CustomConfig = []string{"down-after-milliseconds 1000", "failover-timeout 2500"}
	assert.Equal(t, 3500*time.Millisecond, rfservice.SentinelFailoverWindow(rf))
	downAfter, failoverTimeout := rfservice.SentinelTimeouts(rf)
	assert.Equal(t, time.Second, downAfter)
	assert.Equal(t, 2500*time.Millisecond, failoverTimeout)
}
