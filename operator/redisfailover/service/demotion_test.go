package service_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	mK8SService "github.com/saremox/redis-operator/mocks/service/k8s"
	mRedisService "github.com/saremox/redis-operator/mocks/service/redis"
	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
)

var (
	masterRoleLabel = map[string]string{"redisfailovers-role": "master"}
	slaveRoleLabel  = map[string]string{"redisfailovers-role": "slave"}
)

func podWithRole(name, ip string, labels map[string]string) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Status:     corev1.PodStatus{PodIP: ip, Phase: corev1.PodRunning},
	}
}

type fakeDisconnector struct {
	calls *[]string
}

func (f fakeDisconnector) DisconnectDemoted(_ *redisfailoverv1.RedisFailover, pod corev1.Pod, _, _ string) {
	*f.calls = append(*f.calls, "disconnect "+pod.Name)
}

func TestCheckAllSlavesFromMasterDemotedMasterLabel(t *testing.T) {
	tests := []struct {
		name            string
		oldLabels       map[string]string
		noDisconnector  bool
		expectRelabel   bool
		expectedActions []string
	}{
		{
			name:            "master label replaced: clients disconnected after relabel",
			oldLabels:       masterRoleLabel,
			expectRelabel:   true,
			expectedActions: []string{"relabel", "disconnect old-master"},
		},
		{
			name:            "master label replaced without a disconnector: label only",
			oldLabels:       masterRoleLabel,
			noDisconnector:  true,
			expectRelabel:   true,
			expectedActions: []string{"relabel"},
		},
		{
			name:            "unlabelled pod gets its first label: nothing to disconnect",
			oldLabels:       nil,
			expectRelabel:   true,
			expectedActions: []string{"relabel"},
		},
		{
			name:      "already labelled slave: untouched",
			oldLabels: slaveRoleLabel,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rf := generateRF()
			pods := &corev1.PodList{Items: []corev1.Pod{
				podWithRole("new-master", "0.0.0.0", masterRoleLabel),
				podWithRole("old-master", "1.1.1.1", test.oldLabels),
			}}

			var actions []string
			ms := &mK8SService.Services{}
			ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
			if test.expectRelabel {
				ms.On("UpdatePodLabels", namespace, "old-master", slaveRoleLabel).Once().Return(nil).
					Run(func(mock.Arguments) { actions = append(actions, "relabel") })
			}
			mr := &mRedisService.Client{}
			mr.On("GetSlaveOf", "0.0.0.0", "0", "").Once().Return("", nil)
			mr.On("GetSlaveOf", "1.1.1.1", "0", "").Once().Return("0.0.0.0", nil)

			var opts []rfservice.Option
			if !test.noDisconnector {
				opts = append(opts, rfservice.WithClientDisconnector(fakeDisconnector{calls: &actions}))
			}
			checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy, opts...)
			err := checker.CheckAllSlavesFromMaster("0.0.0.0", rf)

			assert.NoError(t, err)
			ms.AssertExpectations(t)
			mr.AssertExpectations(t)
			assert.Equal(t, test.expectedActions, actions)
		})
	}
}

// A pod still behind the master Service would get its clients straight back.
func TestCheckAllSlavesFromMasterRelabelFailureSkipsDisconnect(t *testing.T) {
	assert := assert.New(t)
	rf := generateRF()
	pods := &corev1.PodList{Items: []corev1.Pod{
		podWithRole("old-master", "1.1.1.1", masterRoleLabel),
	}}

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	ms.On("UpdatePodLabels", namespace, "old-master", slaveRoleLabel).Once().Return(errors.New("conflict"))
	var disconnects []string

	checker := rfservice.NewRedisFailoverChecker(ms, &mRedisService.Client{}, log.DummyLogger{}, metrics.Dummy,
		rfservice.WithClientDisconnector(fakeDisconnector{calls: &disconnects}))
	err := checker.CheckAllSlavesFromMaster("0.0.0.0", rf)

	assert.Error(err)
	assert.Empty(disconnects)
}

// Replicas that merely get SLAVEOF re-issued keep their (read) clients.
func TestSetMasterOnAllDisconnectsOnlyTheDemotedMaster(t *testing.T) {
	assert := assert.New(t)
	rf := generateRF()
	pods := &corev1.PodList{Items: []corev1.Pod{
		podWithRole("new-master", "0.0.0.0", masterRoleLabel),
		podWithRole("old-master", "1.1.1.1", masterRoleLabel),
		podWithRole("replica", "2.2.2.2", slaveRoleLabel),
	}}

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	ms.On("UpdatePodLabels", namespace, "old-master", slaveRoleLabel).Once().Return(nil)
	mr := &mRedisService.Client{}
	mr.On("IsMaster", "0.0.0.0", "0", "").Return(true, nil)
	mr.On("MakeSlaveOfWithPort", "1.1.1.1", "0.0.0.0", "0", "").Once().Return(nil)
	mr.On("MakeSlaveOfWithPort", "2.2.2.2", "0.0.0.0", "0", "").Once().Return(nil)
	var disconnects []string

	healer := rfservice.NewRedisFailoverHealer(ms, mr, log.DummyLogger{},
		rfservice.WithClientDisconnector(fakeDisconnector{calls: &disconnects}))
	err := healer.SetMasterOnAll("0.0.0.0", rf)

	assert.NoError(err)
	ms.AssertExpectations(t)
	mr.AssertExpectations(t)
	assert.Equal([]string{"disconnect old-master"}, disconnects)
}

// Bootstrap mode re-issues SLAVEOF on every reconcile; that must not disconnect anyone.
func TestSetExternalMasterOnAllNeverDisconnectsClients(t *testing.T) {
	assert := assert.New(t)
	rf := generateRF()
	pods := &corev1.PodList{Items: []corev1.Pod{
		podWithRole("pod-0", "0.0.0.0", masterRoleLabel),
		podWithRole("pod-1", "1.1.1.1", slaveRoleLabel),
	}}

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Return(pods, nil)
	mr := &mRedisService.Client{}
	mr.On("MakeSlaveOfWithPort", mock.Anything, "5.5.5.5", "6379", "").Return(nil)
	var disconnects []string

	healer := rfservice.NewRedisFailoverHealer(ms, mr, log.DummyLogger{},
		rfservice.WithClientDisconnector(fakeDisconnector{calls: &disconnects}))
	for reconcile := 0; reconcile < 3; reconcile++ {
		assert.NoError(healer.SetExternalMasterOnAll("5.5.5.5", "6379", rf))
	}

	mr.AssertNumberOfCalls(t, "MakeSlaveOfWithPort", 6)
	assert.Empty(disconnects)
	ms.AssertNotCalled(t, "UpdatePodLabels", mock.Anything, mock.Anything, mock.Anything)
}

func masterEndpointSlice(rf *redisfailoverv1.RedisFailover, ips ...string) *discoveryv1.EndpointSlice {
	return endpointSlice(rfservice.GetRedisMasterName(rf), ips...)
}

func endpointSlice(service string, ips ...string) *discoveryv1.EndpointSlice {
	slice := &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{
		Name:      service + "-abcde",
		Namespace: namespace,
		Labels:    map[string]string{discoveryv1.LabelServiceName: service},
	}}
	for _, ip := range ips {
		slice.Endpoints = append(slice.Endpoints, discoveryv1.Endpoint{Addresses: []string{ip}})
	}
	return slice
}

func disconnectClientsMock(err error) (*mRedisService.Client, chan time.Time) {
	called := make(chan time.Time, 1)
	mr := &mRedisService.Client{}
	mr.On("DisconnectClients", "1.1.1.1", "0", "").Once().Return(err).
		Run(func(mock.Arguments) { called <- time.Now() })
	return mr, called
}

func TestClientDisconnectorWaitsForPodToLeaveMasterService(t *testing.T) {
	rf := generateRF()
	kubeClient := fake.NewClientset(
		masterEndpointSlice(rf, "0.0.0.0", "1.1.1.1"),
		endpointSlice("unrelated", "1.1.1.1"),
	)
	mr, called := disconnectClientsMock(nil)
	disconnector := rfservice.NewClientDisconnector(kubeClient, mr, log.DummyLogger{}, time.Minute, 0)

	demoted := podWithRole("old-master", "1.1.1.1", slaveRoleLabel)
	disconnector.DisconnectDemoted(rf, demoted, "0", "")
	disconnector.DisconnectDemoted(rf, demoted, "0", "")

	select {
	case <-called:
		t.Fatal("clients disconnected while the pod was still behind the master Service")
	case <-time.After(time.Second):
	}

	_, err := kubeClient.DiscoveryV1().EndpointSlices(namespace).
		Update(context.Background(), masterEndpointSlice(rf, "0.0.0.0"), metav1.UpdateOptions{})
	require.NoError(t, err)

	select {
	case <-called:
	case <-time.After(5 * time.Second):
		t.Fatal("clients not disconnected after the pod left the master Service")
	}
	time.Sleep(500 * time.Millisecond)
	mr.AssertNumberOfCalls(t, "DisconnectClients", 1)
}

func TestClientDisconnectorWaitsForGraceAfterRemoval(t *testing.T) {
	rf := generateRF()
	kubeClient := fake.NewClientset(masterEndpointSlice(rf, "0.0.0.0"))
	mr, called := disconnectClientsMock(nil)
	grace := 500 * time.Millisecond
	disconnector := rfservice.NewClientDisconnector(kubeClient, mr, log.DummyLogger{}, time.Minute, grace)

	start := time.Now()
	disconnector.DisconnectDemoted(rf, podWithRole("old-master", "1.1.1.1", slaveRoleLabel), "0", "")

	select {
	case at := <-called:
		assert.GreaterOrEqual(t, at.Sub(start), grace)
	case <-time.After(5 * time.Second):
		t.Fatal("clients not disconnected")
	}
}

func TestClientDisconnectorFallsBackToDisconnecting(t *testing.T) {
	tests := []struct {
		name    string
		timeout time.Duration
		setup   func(*fake.Clientset)
	}{
		{
			name:    "pod never leaves the master Service",
			timeout: 500 * time.Millisecond,
		},
		{
			name:    "EndpointSlices can't be listed",
			timeout: time.Minute,
			setup: func(c *fake.Clientset) {
				c.PrependReactor("list", "endpointslices", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, errors.New("forbidden")
				})
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rf := generateRF()
			kubeClient := fake.NewClientset(masterEndpointSlice(rf, "1.1.1.1"))
			if test.setup != nil {
				test.setup(kubeClient)
			}
			mr, called := disconnectClientsMock(errors.New("NOPERM"))
			disconnector := rfservice.NewClientDisconnector(kubeClient, mr, log.DummyLogger{}, test.timeout, 0)

			disconnector.DisconnectDemoted(rf, podWithRole("old-master", "1.1.1.1", slaveRoleLabel), "0", "")

			select {
			case <-called:
			case <-time.After(5 * time.Second):
				t.Fatal("clients not disconnected")
			}
		})
	}
}
