package service_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	mK8SService "github.com/saremox/redis-operator/mocks/service/k8s"
	mRedisService "github.com/saremox/redis-operator/mocks/service/redis"
	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
	"github.com/saremox/redis-operator/service/redis"
)

func peersPod(ip string, phase corev1.PodPhase, deleting bool) corev1.Pod {
	pod := corev1.Pod{Status: corev1.PodStatus{PodIP: ip, Phase: phase}}
	if deleting {
		pod.DeletionTimestamp = &metav1.Time{}
	}
	return pod
}

// TestGetUnknownSentinelAddrs reads the running Sentinels, also a Sentinel in
// deletion. A pod in each phase, also in deletion, is known.
func TestGetUnknownSentinelAddrs(t *testing.T) {
	rf := generateRF()
	sentinels := &corev1.PodList{Items: []corev1.Pod{
		peersPod("10.0.1.1", corev1.PodRunning, false),
		peersPod("10.0.1.2", corev1.PodRunning, true),
		peersPod("10.0.1.3", corev1.PodPending, false),
		peersPod("10.0.1.4", corev1.PodRunning, false),
		peersPod("10.0.1.5", corev1.PodRunning, false),
		peersPod("", corev1.PodPending, false),
		// A Sentinel in deletion on a lost node does not answer.
		peersPod("10.0.1.6", corev1.PodRunning, true),
	}}
	redises := &corev1.PodList{Items: []corev1.Pod{
		peersPod("10.0.0.1", corev1.PodRunning, false),
		peersPod("10.0.0.2", corev1.PodRunning, true),
		peersPod("10.0.0.3", corev1.PodPending, false),
	}}
	ms := &mK8SService.Services{}
	ms.On("GetDeploymentPods", namespace, rfservice.GetSentinelName(rf)).Once().Return(sentinels, nil)
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(redises, nil)
	mr := &mRedisService.Client{}
	mr.On("GetSentinelPeers", "10.0.1.1").Once().Return(redis.SentinelPeers{Master: "10.0.0.3", Peers: []string{"10.0.1.2", "10.0.1.3", "10.0.9.9"}}, nil)
	mr.On("GetSentinelPeers", "10.0.1.2").Once().Return(redis.SentinelPeers{Master: "10.0.9.8", Peers: []string{"10.0.1.1", "10.0.9.9"}}, nil)
	mr.On("GetSentinelPeers", "10.0.1.4").Once().Return(redis.SentinelPeers{}, errors.New("i/o timeout"))
	mr.On("GetSentinelPeers", "10.0.1.5").Once().Return(redis.SentinelPeers{Master: "10.0.0.2", Peers: []string{"10.0.1.1"}}, nil)
	mr.On("GetSentinelPeers", "10.0.1.6").Once().Return(redis.SentinelPeers{}, errors.New("i/o timeout"))
	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	addrs, err := checker.GetUnknownSentinelAddrs(rf)

	require.NoError(t, err)
	assert.Equal(t, rfservice.SentinelAddrs{
		Peers:   map[string][]string{"10.0.9.9": {"10.0.1.1", "10.0.1.2"}},
		Masters: map[string][]string{"10.0.9.8": {"10.0.1.2"}},
		Own: map[string]bool{
			"10.0.1.1": true, "10.0.1.2": true, "10.0.1.3": true, "10.0.1.4": true, "10.0.1.5": true, "10.0.1.6": true,
			"10.0.0.1": true, "10.0.0.2": true, "10.0.0.3": true,
		},
		Unread: []string{"10.0.1.4"},
	}, addrs)
	mr.AssertExpectations(t)
}

// TestGetUnknownSentinelAddrsKeepsALoopbackMaster leaves the decision about a
// loopback master to the caller.
func TestGetUnknownSentinelAddrsKeepsALoopbackMaster(t *testing.T) {
	rf := generateRF()
	ms := &mK8SService.Services{}
	ms.On("GetDeploymentPods", namespace, rfservice.GetSentinelName(rf)).Once().Return(&corev1.PodList{Items: []corev1.Pod{peersPod("10.0.1.1", corev1.PodRunning, false)}}, nil)
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(&corev1.PodList{}, nil)
	mr := &mRedisService.Client{}
	mr.On("GetSentinelPeers", "10.0.1.1").Once().Return(redis.SentinelPeers{Master: "127.0.0.1"}, nil)
	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	addrs, err := checker.GetUnknownSentinelAddrs(rf)

	require.NoError(t, err)
	assert.Equal(t, rfservice.SentinelAddrs{Peers: map[string][]string{}, Masters: map[string][]string{"127.0.0.1": {"10.0.1.1"}}, Own: map[string]bool{"10.0.1.1": true}}, addrs)
}

// TestGetUnknownSentinelAddrsComparesTheIPs knows both IPs of a dual-stack
// pod, and each form of an IPv6 address.
func TestGetUnknownSentinelAddrsComparesTheIPs(t *testing.T) {
	rf := generateRF()
	dualStack := func(ipv4, ipv6 string) corev1.Pod {
		pod := peersPod(ipv4, corev1.PodRunning, false)
		pod.Status.PodIPs = []corev1.PodIP{{IP: ipv4}, {IP: ipv6}}
		return pod
	}
	ms := &mK8SService.Services{}
	ms.On("GetDeploymentPods", namespace, rfservice.GetSentinelName(rf)).Once().Return(&corev1.PodList{Items: []corev1.Pod{
		dualStack("10.0.1.1", "fd00::1:1"),
		dualStack("10.0.1.2", "FD00:0:0:0:0:0:1:2"),
	}}, nil)
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(&corev1.PodList{Items: []corev1.Pod{dualStack("10.0.0.1", "fd00::1")}}, nil)
	mr := &mRedisService.Client{}
	mr.On("GetSentinelPeers", "10.0.1.1").Once().Return(redis.SentinelPeers{Master: "FD00:0::0001", Peers: []string{"fd00:0:0:0:0:0:1:2", "fd00:0::9"}}, nil)
	mr.On("GetSentinelPeers", "10.0.1.2").Once().Return(redis.SentinelPeers{Master: "fd00::0:2", Peers: []string{"FD00::1:1"}}, nil)
	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	addrs, err := checker.GetUnknownSentinelAddrs(rf)

	require.NoError(t, err)
	assert.Equal(t, map[string][]string{"fd00::9": {"10.0.1.1"}}, addrs.Peers)
	assert.Equal(t, map[string][]string{"fd00::2": {"10.0.1.2"}}, addrs.Masters)
	assert.Equal(t, map[string]bool{"10.0.1.1": true, "fd00::1:1": true, "10.0.1.2": true, "fd00::1:2": true, "10.0.0.1": true, "fd00::1": true}, addrs.Own)
}

func TestGetUnknownSentinelAddrsWithoutThePods(t *testing.T) {
	rf := generateRF()
	t.Run("no Sentinel pods", func(t *testing.T) {
		ms := &mK8SService.Services{}
		ms.On("GetDeploymentPods", namespace, rfservice.GetSentinelName(rf)).Once().Return(nil, errors.New("boom"))
		checker := rfservice.NewRedisFailoverChecker(ms, &mRedisService.Client{}, log.DummyLogger{}, metrics.Dummy)
		_, err := checker.GetUnknownSentinelAddrs(rf)
		require.EqualError(t, err, "boom")
	})
	t.Run("no Redis pods", func(t *testing.T) {
		ms := &mK8SService.Services{}
		ms.On("GetDeploymentPods", namespace, rfservice.GetSentinelName(rf)).Once().Return(&corev1.PodList{}, nil)
		ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(nil, errors.New("boom"))
		checker := rfservice.NewRedisFailoverChecker(ms, &mRedisService.Client{}, log.DummyLogger{}, metrics.Dummy)
		_, err := checker.GetUnknownSentinelAddrs(rf)
		require.EqualError(t, err, "boom")
	})
}
