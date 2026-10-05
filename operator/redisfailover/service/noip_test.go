package service_test

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	mK8SService "github.com/saremox/redis-operator/mocks/service/k8s"
	mRedisService "github.com/saremox/redis-operator/mocks/service/redis"
	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
)

// levelLogger records the Error and Debug lines.
type levelLogger struct {
	log.DummyLogger
	mu     sync.Mutex
	errors []string
	debugs []string
}

func (l *levelLogger) With(string, interface{}) log.Logger      { return l }
func (l *levelLogger) WithField(string, interface{}) log.Logger { return l }

func (l *levelLogger) Errorf(format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.errors = append(l.errors, fmt.Sprintf(format, args...))
}

func (l *levelLogger) Debugf(format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.debugs = append(l.debugs, fmt.Sprintf(format, args...))
}

func (l *levelLogger) debugged(s string) bool {
	return strings.Contains(strings.Join(l.debugs, "\n"), s)
}

func pendingPod(name string) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status:     corev1.PodStatus{Phase: corev1.PodPending},
	}
}

// A pod that did not start has no IP. The check skips it without a Redis call
// and without an Error line.
func TestCheckAllSlavesFromMasterSkipsAPodWithoutIP(t *testing.T) {
	rf := generateRF()
	pods := &corev1.PodList{Items: []corev1.Pod{
		runningPod("rfr-test-0", "10.0.0.1"),
		pendingPod("rfr-test-1"),
	}}
	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	ms.On("UpdatePodLabels", namespace, mock.AnythingOfType("string"), mock.Anything).Return(nil)
	mr := &mRedisService.Client{}
	mr.On("GetSlaveOf", "10.0.0.1", "0", "").Once().Return("", nil)
	logger := &levelLogger{}

	checker := rfservice.NewRedisFailoverChecker(ms, mr, logger, metrics.Dummy)

	assert.NoError(t, checker.CheckAllSlavesFromMaster("10.0.0.1", rf))
	mr.AssertExpectations(t)
	mr.AssertNotCalled(t, "GetSlaveOf", "", mock.Anything, mock.Anything)
	assert.Empty(t, logger.errors)
	assert.True(t, logger.debugged("Pod rfr-test-1 has no IP yet"))
}

// A replica that answers wrongly is still an error next to a pod without IP.
func TestCheckAllSlavesFromMasterKeepsTheWrongMasterErrorNextToAPodWithoutIP(t *testing.T) {
	rf := generateRF()
	pods := &corev1.PodList{Items: []corev1.Pod{
		pendingPod("rfr-test-0"),
		runningPod("rfr-test-1", "10.0.0.2"),
	}}
	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	ms.On("UpdatePodLabels", namespace, mock.AnythingOfType("string"), mock.Anything).Return(nil)
	mr := &mRedisService.Client{}
	mr.On("GetSlaveOf", "10.0.0.2", "0", "").Once().Return("10.0.0.9", nil)

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.Dummy, metrics.Dummy)

	assert.Error(t, checker.CheckAllSlavesFromMaster("10.0.0.1", rf))
	mr.AssertExpectations(t)
}

// The pod without IP gets no REPLICAOF and no label, as before. The other
// replicas change.
func TestSetMasterOnAllSkipsAPodWithoutIP(t *testing.T) {
	rf := generateRF()
	pods := &corev1.PodList{Items: []corev1.Pod{
		runningPod("pod-0", "10.0.0.1"),
		pendingPod("pod-1"),
		runningPod("pod-2", "10.0.0.3"),
	}}
	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	ms.On("UpdatePodLabels", namespace, "pod-2", slaveRoleLabel).Once().Return(nil)
	mr := &mRedisService.Client{}
	mr.On("IsMaster", "10.0.0.1", "0", "").Return(true, nil)
	mr.On("MakeSlaveOfWithPort", "10.0.0.3", "0", "10.0.0.1", "0", "").Once().Return(nil)
	logger := &levelLogger{}

	healer := rfservice.NewRedisFailoverHealer(ms, mr, logger)

	assert.NoError(t, healer.SetMasterOnAll("10.0.0.1", rf))
	ms.AssertExpectations(t)
	mr.AssertExpectations(t)
	mr.AssertNotCalled(t, "MakeSlaveOfWithPort", "", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	assert.Empty(t, logger.errors)
	assert.True(t, logger.debugged("Pod pod-1 has no IP yet"))
}

// A pod without IP cannot be the master. As a replica, it gets no REPLICAOF
// and still gets the replica label.
func TestSetOldestAsMasterSkipsAPodWithoutIP(t *testing.T) {
	rf := generateRF()
	pods := &corev1.PodList{Items: []corev1.Pod{
		pendingPod("pod-0"),
		runningPod("pod-1", "10.0.0.2"),
		pendingPod("pod-2"),
		runningPod("pod-3", "10.0.0.4"),
	}}
	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	ms.On("UpdatePodLabels", namespace, "pod-1", masterRoleLabel).Once().Return(nil)
	ms.On("UpdatePodLabels", namespace, "pod-2", slaveRoleLabel).Once().Return(nil)
	ms.On("UpdatePodLabels", namespace, "pod-3", slaveRoleLabel).Once().Return(nil)
	mr := &mRedisService.Client{}
	mr.On("MakeMaster", "10.0.0.2", "0", "").Once().Return(nil)
	mr.On("MakeSlaveOfWithPort", "10.0.0.4", "0", "10.0.0.2", "0", "").Once().Return(nil)
	logger := &levelLogger{}

	healer := rfservice.NewRedisFailoverHealer(ms, mr, logger)

	assert.NoError(t, healer.SetOldestAsMaster(rf))
	ms.AssertExpectations(t)
	mr.AssertExpectations(t)
	mr.AssertNotCalled(t, "MakeMaster", "", mock.Anything, mock.Anything)
	mr.AssertNotCalled(t, "MakeSlaveOfWithPort", "", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	assert.Empty(t, logger.errors)
	assert.True(t, logger.debugged("Pod pod-0 has no IP yet, so it cannot be the master"))
	assert.True(t, logger.debugged("Pod pod-2 has no IP yet, so it is not made a replica"))
}

// No pod can be the master, so the election still fails.
func TestSetOldestAsMasterFailsWhenNoPodHasAnIP(t *testing.T) {
	rf := generateRF()
	pods := &corev1.PodList{Items: []corev1.Pod{pendingPod("pod-0")}}
	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	mr := &mRedisService.Client{}

	healer := rfservice.NewRedisFailoverHealer(ms, mr, log.Dummy)

	assert.Error(t, healer.SetOldestAsMaster(rf))
	mr.AssertNotCalled(t, "MakeMaster", mock.Anything, mock.Anything, mock.Anything)
}
