package service_test

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"

	"github.com/stretchr/testify/assert"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	mK8SService "github.com/saremox/redis-operator/mocks/service/k8s"
	mRedisService "github.com/saremox/redis-operator/mocks/service/redis"
	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
	"github.com/saremox/redis-operator/service/redis"
)

func generateRF() *redisfailoverv1.RedisFailover {
	return &redisfailoverv1.RedisFailover{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: redisfailoverv1.RedisFailoverSpec{
			Redis: redisfailoverv1.RedisSettings{
				Replicas: int32(3),
			},
			Sentinel: redisfailoverv1.SentinelSettings{
				Replicas: int32(3),
			},
		},
	}
}

func TestCheckRedisNumberError(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSet", namespace, rfservice.GetRedisName(rf)).Once().Return(nil, errors.New(""))
	mr := &mRedisService.Client{}

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	err := checker.CheckRedisNumber(rf)
	assert.Error(err)
}

func TestCheckRedisNumberFalse(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	wrongNumber := int32(4)
	ss := &appsv1.StatefulSet{
		Spec: appsv1.StatefulSetSpec{
			Replicas: &wrongNumber,
		},
	}
	ms := &mK8SService.Services{}
	ms.On("GetStatefulSet", namespace, rfservice.GetRedisName(rf)).Once().Return(ss, nil)
	mr := &mRedisService.Client{}

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	err := checker.CheckRedisNumber(rf)
	assert.Error(err)
}

func TestCheckRedisNumberTrue(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	goodNumber := int32(3)
	ss := &appsv1.StatefulSet{
		Spec: appsv1.StatefulSetSpec{
			Replicas: &goodNumber,
		},
	}
	ms := &mK8SService.Services{}
	ms.On("GetStatefulSet", namespace, rfservice.GetRedisName(rf)).Once().Return(ss, nil)
	mr := &mRedisService.Client{}

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	err := checker.CheckRedisNumber(rf)
	assert.NoError(err)
}

func TestCheckSentinelNumberError(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	ms := &mK8SService.Services{}
	ms.On("GetDeployment", namespace, rfservice.GetSentinelName(rf)).Once().Return(nil, errors.New(""))
	mr := &mRedisService.Client{}

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	err := checker.CheckSentinelNumber(rf)
	assert.Error(err)
}

func TestCheckSentinelNumberFalse(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	wrongNumber := int32(4)
	ss := &appsv1.Deployment{
		Spec: appsv1.DeploymentSpec{
			Replicas: &wrongNumber,
		},
	}
	ms := &mK8SService.Services{}
	ms.On("GetDeployment", namespace, rfservice.GetSentinelName(rf)).Once().Return(ss, nil)
	mr := &mRedisService.Client{}

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	err := checker.CheckSentinelNumber(rf)
	assert.Error(err)
}

func TestCheckSentinelNumberTrue(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	goodNumber := int32(3)
	ss := &appsv1.Deployment{
		Spec: appsv1.DeploymentSpec{
			Replicas: &goodNumber,
		},
	}
	ms := &mK8SService.Services{}
	ms.On("GetDeployment", namespace, rfservice.GetSentinelName(rf)).Once().Return(ss, nil)
	mr := &mRedisService.Client{}

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	err := checker.CheckSentinelNumber(rf)
	assert.NoError(err)
}

func TestCheckAllSlavesFromMasterGetStatefulSetError(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(nil, errors.New(""))
	ms.On("UpdatePodLabels", namespace, mock.AnythingOfType("string"), mock.Anything).Once().Return(nil)
	mr := &mRedisService.Client{}

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	err := checker.CheckAllSlavesFromMaster("", rf)
	assert.Error(err)
}

func TestCheckAllSlavesFromMasterGetSlaveOfError(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	pods := &corev1.PodList{
		Items: []corev1.Pod{
			{
				Status: corev1.PodStatus{
					PodIP: "",
					Phase: corev1.PodRunning,
				},
			},
		},
	}

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	ms.On("UpdatePodLabels", namespace, mock.AnythingOfType("string"), mock.Anything).Once().Return(nil)
	mr := &mRedisService.Client{}
	mr.On("GetSlaveOf", "", "0", "").Once().Return("", errors.New(""))

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	err := checker.CheckAllSlavesFromMaster("", rf)
	assert.Error(err)
}

func TestCheckAllSlavesFromMasterDifferentMaster(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	pods := &corev1.PodList{
		Items: []corev1.Pod{
			{
				Status: corev1.PodStatus{
					PodIP: "0.0.0.0",
					Phase: corev1.PodRunning,
				},
			},
		},
	}

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	ms.On("UpdatePodLabels", namespace, mock.AnythingOfType("string"), mock.Anything).Once().Return(nil)
	mr := &mRedisService.Client{}
	mr.On("GetSlaveOf", "0.0.0.0", "0", "").Once().Return("1.1.1.1", nil)

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	err := checker.CheckAllSlavesFromMaster("0.0.0.0", rf)
	assert.Error(err)
}

func TestCheckAllSlavesFromMaster(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	pods := &corev1.PodList{
		Items: []corev1.Pod{
			{
				Status: corev1.PodStatus{
					PodIP: "0.0.0.0",
					Phase: corev1.PodRunning,
				},
			},
		},
	}

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	ms.On("UpdatePodLabels", namespace, mock.AnythingOfType("string"), mock.Anything).Once().Return(nil)
	mr := &mRedisService.Client{}
	mr.On("GetSlaveOf", "0.0.0.0", "0", "").Once().Return("1.1.1.1", nil)

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	err := checker.CheckAllSlavesFromMaster("1.1.1.1", rf)
	assert.NoError(err)
}

func TestCheckAllSlavesFromMasterMasterAlreadyLabeled(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	pods := &corev1.PodList{
		Items: []corev1.Pod{
			{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"redisfailovers-role": "master"},
				},
				Status: corev1.PodStatus{
					PodIP: "0.0.0.0",
					Phase: corev1.PodRunning,
				},
			},
		},
	}

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	mr := &mRedisService.Client{}
	mr.On("GetSlaveOf", "0.0.0.0", "0", "").Once().Return("", nil)

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	err := checker.CheckAllSlavesFromMaster("0.0.0.0", rf)
	assert.NoError(err)
	ms.AssertExpectations(t) // pod already has the master label, so UpdatePodLabels must not be called
}

func TestCheckAllSlavesFromMasterSlaveAlreadyLabeled(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	pods := &corev1.PodList{
		Items: []corev1.Pod{
			{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"redisfailovers-role": "slave"},
				},
				Status: corev1.PodStatus{
					PodIP: "1.1.1.1",
					Phase: corev1.PodRunning,
				},
			},
		},
	}

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	mr := &mRedisService.Client{}
	mr.On("GetSlaveOf", "1.1.1.1", "0", "").Once().Return("0.0.0.0", nil)

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	err := checker.CheckAllSlavesFromMaster("0.0.0.0", rf)
	assert.NoError(err)
	ms.AssertExpectations(t) // pod already has the slave label, so UpdatePodLabels must not be called
}

func TestCheckSentinelNumberInMemoryGetDeploymentPodsError(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	ms := &mK8SService.Services{}
	mr := &mRedisService.Client{}
	mr.On("GetNumberSentinelsInMemory", "1.1.1.1").Once().Return(int32(0), errors.New("expected error"))

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	err := checker.CheckSentinelNumberInMemory("1.1.1.1", rf)
	assert.Error(err)
}

func TestCheckSentinelNumberInMemoryGetNumberSentinelInMemoryError(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	ms := &mK8SService.Services{}
	mr := &mRedisService.Client{}
	mr.On("GetNumberSentinelsInMemory", "1.1.1.1").Once().Return(int32(0), errors.New(""))

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	err := checker.CheckSentinelNumberInMemory("1.1.1.1", rf)
	assert.Error(err)
}

func TestCheckSentinelNumberInMemoryNumberMismatch(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	ms := &mK8SService.Services{}
	mr := &mRedisService.Client{}
	mr.On("GetNumberSentinelsInMemory", "1.1.1.1").Once().Return(int32(4), nil)

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	err := checker.CheckSentinelNumberInMemory("1.1.1.1", rf)
	assert.Error(err)
}

func TestCheckSentinelNumberInMemory(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	ms := &mK8SService.Services{}
	mr := &mRedisService.Client{}
	mr.On("GetNumberSentinelsInMemory", "1.1.1.1").Once().Return(int32(3), nil)

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	err := checker.CheckSentinelNumberInMemory("1.1.1.1", rf)
	assert.NoError(err)
}

func TestCheckSentinelSlavesNumberInMemoryGetNumberSentinelSlavesInMemoryError(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	ms := &mK8SService.Services{}
	mr := &mRedisService.Client{}
	mr.On("GetNumberSentinelSlavesInMemory", "1.1.1.1").Once().Return(int32(0), errors.New(""))

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	err := checker.CheckSentinelSlavesNumberInMemory("1.1.1.1", rf)
	assert.Error(err)
}

func TestCheckSentinelSlavesNumberInMemoryReplicasMismatch(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	ms := &mK8SService.Services{}
	mr := &mRedisService.Client{}
	mr.On("GetNumberSentinelSlavesInMemory", "1.1.1.1").Once().Return(int32(3), nil)

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	err := checker.CheckSentinelSlavesNumberInMemory("1.1.1.1", rf)
	assert.Error(err)
}

func TestCheckSentinelSlavesNumberInMemory(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()
	rf.Spec.Redis.Replicas = 5

	ms := &mK8SService.Services{}
	mr := &mRedisService.Client{}
	mr.On("GetNumberSentinelSlavesInMemory", "1.1.1.1").Once().Return(int32(4), nil)

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	err := checker.CheckSentinelSlavesNumberInMemory("1.1.1.1", rf)
	assert.NoError(err)
}

func TestCheckSentinelSlavesNumberInMemoryBootstrappingMismatch(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()
	rf.Spec.BootstrapNode = &redisfailoverv1.BootstrapSettings{Host: "127.0.0.1"}
	rf.Spec.Redis.Replicas = 3

	ms := &mK8SService.Services{}
	mr := &mRedisService.Client{}
	mr.On("GetNumberSentinelSlavesInMemory", "1.1.1.1").Once().Return(int32(2), nil)

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	err := checker.CheckSentinelSlavesNumberInMemory("1.1.1.1", rf)
	assert.Error(err, "while bootstrapping, sentinel slave count must match replicas exactly")
}

func TestCheckSentinelSlavesNumberInMemoryBootstrappingMatch(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()
	rf.Spec.BootstrapNode = &redisfailoverv1.BootstrapSettings{Host: "127.0.0.1"}
	rf.Spec.Redis.Replicas = 3

	ms := &mK8SService.Services{}
	mr := &mRedisService.Client{}
	mr.On("GetNumberSentinelSlavesInMemory", "1.1.1.1").Once().Return(int32(3), nil)

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	err := checker.CheckSentinelSlavesNumberInMemory("1.1.1.1", rf)
	assert.NoError(err)
}

func TestCheckSentinelMonitorGetSentinelMonitorError(t *testing.T) {
	assert := assert.New(t)

	ms := &mK8SService.Services{}
	mr := &mRedisService.Client{}
	mr.On("GetSentinelMonitor", "0.0.0.0").Once().Return("", "", errors.New(""))

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	err := checker.CheckSentinelMonitor("0.0.0.0", "1.1.1.1")
	assert.Error(err)
}

func TestCheckSentinelMonitorMismatch(t *testing.T) {
	assert := assert.New(t)

	ms := &mK8SService.Services{}
	mr := &mRedisService.Client{}
	mr.On("GetSentinelMonitor", "0.0.0.0").Once().Return("2.2.2.2", "6379", nil)

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	err := checker.CheckSentinelMonitor("0.0.0.0", "1.1.1.1")
	assert.Error(err)
}

func TestCheckSentinelMonitor(t *testing.T) {
	assert := assert.New(t)

	ms := &mK8SService.Services{}
	mr := &mRedisService.Client{}
	mr.On("GetSentinelMonitor", "0.0.0.0").Once().Return("1.1.1.1", "6379", nil)

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	err := checker.CheckSentinelMonitor("0.0.0.0", "1.1.1.1")
	assert.NoError(err)
}

func TestCheckSentinelMonitorWithPort(t *testing.T) {
	assert := assert.New(t)

	ms := &mK8SService.Services{}
	mr := &mRedisService.Client{}
	mr.On("GetSentinelMonitor", "0.0.0.0").Once().Return("1.1.1.1", "6379", nil)

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	err := checker.CheckSentinelMonitor("0.0.0.0", "1.1.1.1", "6379")
	assert.NoError(err)
}

func TestCheckSentinelMonitorWithPortMismatch(t *testing.T) {
	assert := assert.New(t)

	ms := &mK8SService.Services{}
	mr := &mRedisService.Client{}
	mr.On("GetSentinelMonitor", "0.0.0.0").Once().Return("1.1.1.1", "6379", nil)

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	err := checker.CheckSentinelMonitor("0.0.0.0", "0.0.0.0", "6379")
	assert.Error(err)
}

func TestCheckSentinelMonitorWithPortIPMismatch(t *testing.T) {
	assert := assert.New(t)

	ms := &mK8SService.Services{}
	mr := &mRedisService.Client{}
	mr.On("GetSentinelMonitor", "0.0.0.0").Once().Return("1.1.1.1", "6379", nil)

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	err := checker.CheckSentinelMonitor("0.0.0.0", "1.1.1.1", "6380")
	assert.Error(err)
}

func TestGetMasterIPGetStatefulSetPodsError(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(nil, errors.New(""))
	mr := &mRedisService.Client{}

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	_, err := checker.GetMasterIP(rf)
	assert.Error(err)
}

func TestGetMasterIPIsMasterError(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	pods := &corev1.PodList{
		Items: []corev1.Pod{
			{
				Status: corev1.PodStatus{
					PodIP: "0.0.0.0",
					Phase: corev1.PodRunning,
				},
			},
		},
	}

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	mr := &mRedisService.Client{}
	mr.On("IsMaster", "0.0.0.0", "0", "").Once().Return(false, errors.New(""))

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	_, err := checker.GetMasterIP(rf)
	assert.Error(err)
}

func TestGetMasterIPMultipleMastersError(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	pods := &corev1.PodList{
		Items: []corev1.Pod{
			{
				Status: corev1.PodStatus{
					PodIP: "0.0.0.0",
					Phase: corev1.PodRunning,
				},
			},
			{
				Status: corev1.PodStatus{
					PodIP: "1.1.1.1",
					Phase: corev1.PodRunning,
				},
			},
		},
	}

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	mr := &mRedisService.Client{}
	mr.On("IsMaster", "0.0.0.0", "0", "").Once().Return(true, nil)
	mr.On("IsMaster", "1.1.1.1", "0", "").Once().Return(true, nil)

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	_, err := checker.GetMasterIP(rf)
	assert.Error(err)
}

func TestGetMasterIP(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	pods := &corev1.PodList{
		Items: []corev1.Pod{
			{
				Status: corev1.PodStatus{
					PodIP: "0.0.0.0",
					Phase: corev1.PodRunning,
				},
			},
			{
				Status: corev1.PodStatus{
					PodIP: "1.1.1.1",
					Phase: corev1.PodRunning,
				},
			},
		},
	}

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	mr := &mRedisService.Client{}
	mr.On("IsMaster", "0.0.0.0", "0", "").Once().Return(true, nil)
	mr.On("IsMaster", "1.1.1.1", "0", "").Once().Return(false, nil)

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	master, err := checker.GetMasterIP(rf)
	assert.NoError(err)
	assert.Equal("0.0.0.0", master, "the master should be the expected")
}

func TestGetNumberMastersGetStatefulSetPodsError(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(nil, errors.New(""))
	mr := &mRedisService.Client{}

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	_, err := checker.GetNumberMasters(rf)
	assert.Error(err)
}

func TestGetNumberMastersIsMasterError(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	pods := &corev1.PodList{
		Items: []corev1.Pod{
			{
				Status: corev1.PodStatus{
					PodIP: "0.0.0.0",
					Phase: corev1.PodRunning,
				},
			},
		},
	}

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	mr := &mRedisService.Client{}
	mr.On("IsMaster", "0.0.0.0", "0", "").Once().Return(true, errors.New(""))

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	_, err := checker.GetNumberMasters(rf)
	assert.NoError(err)
}

func TestGetNumberMasters(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	pods := &corev1.PodList{
		Items: []corev1.Pod{
			{
				Status: corev1.PodStatus{
					PodIP: "0.0.0.0",
					Phase: corev1.PodRunning,
				},
			},
			{
				Status: corev1.PodStatus{
					PodIP: "1.1.1.1",
					Phase: corev1.PodRunning,
				},
			},
		},
	}

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	mr := &mRedisService.Client{}
	mr.On("IsMaster", "0.0.0.0", "0", "").Once().Return(true, nil)
	mr.On("IsMaster", "1.1.1.1", "0", "").Once().Return(false, nil)

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	masterNumber, err := checker.GetNumberMasters(rf)
	assert.NoError(err)
	assert.Equal(1, masterNumber, "the master number should be ok")
}

func TestGetNumberMastersTwo(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	pods := &corev1.PodList{
		Items: []corev1.Pod{
			{
				Status: corev1.PodStatus{
					PodIP: "0.0.0.0",
					Phase: corev1.PodRunning,
				},
			},
			{
				Status: corev1.PodStatus{
					PodIP: "1.1.1.1",
					Phase: corev1.PodRunning,
				},
			},
		},
	}

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	mr := &mRedisService.Client{}
	mr.On("IsMaster", "0.0.0.0", "0", "").Once().Return(true, nil)
	mr.On("IsMaster", "1.1.1.1", "0", "").Once().Return(true, nil)

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	masterNumber, err := checker.GetNumberMasters(rf)
	assert.NoError(err)
	assert.Equal(2, masterNumber, "the master number should be ok")
}

func TestGetMaxRedisPodTimeGetStatefulSetPodsError(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(nil, errors.New(""))
	mr := &mRedisService.Client{}

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	_, err := checker.GetMaxRedisPodTime(rf)
	assert.Error(err)
}

func TestGetMaxRedisPodTime(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	now := time.Now()
	oneHour := now.Add(-1 * time.Hour)
	oneMinute := now.Add(-1 * time.Minute)

	pods := &corev1.PodList{
		Items: []corev1.Pod{
			{
				Status: corev1.PodStatus{
					StartTime: &metav1.Time{
						Time: oneHour,
					},
				},
			},
			{
				Status: corev1.PodStatus{
					StartTime: &metav1.Time{
						Time: oneMinute,
					},
				},
			},
		},
	}

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	mr := &mRedisService.Client{}

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	maxTime, err := checker.GetMaxRedisPodTime(rf)
	assert.NoError(err)

	expected := now.Sub(oneHour).Round(time.Second)
	assert.Equal(expected, maxTime.Round(time.Second), "the closest time should be given")
}

func TestGetRedisPodsNames(t *testing.T) {
	assert := assert.New(t)
	rf := generateRF()

	pods := &corev1.PodList{
		Items: []corev1.Pod{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name: "slave1",
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					PodIP: "0.0.0.0",
				},
			},
			{
				ObjectMeta: metav1.ObjectMeta{
					Name: "master",
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					PodIP: "1.1.1.1",
				},
			},
			{
				ObjectMeta: metav1.ObjectMeta{
					Name: "slave2",
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					PodIP: "0.0.0.0",
				},
			},
		},
	}

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	mr := &mRedisService.Client{}
	mr.On("IsMaster", "0.0.0.0", "0", "").Twice().Return(false, nil)
	mr.On("IsMaster", "1.1.1.1", "0", "").Once().Return(true, nil)

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)
	master, err := checker.GetRedisesMasterPod(rf)

	assert.NoError(err)

	assert.Equal(master, "master")

	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	mr.On("IsMaster", "0.0.0.0", "0", "").Twice().Return(false, nil)
	mr.On("IsMaster", "1.1.1.1", "0", "").Once().Return(true, nil)

	namePods, err := checker.GetRedisesSlavesPods(rf)

	assert.NoError(err)

	assert.Equal(namePods, []string{"slave1", "slave2"})
}

// --- GetRedisesMasterPod ---

func TestGetRedisesMasterPodGetStatefulSetPodsError(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(nil, errors.New("boom"))
	mr := &mRedisService.Client{}

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	master, err := checker.GetRedisesMasterPod(rf)
	assert.Error(err)
	assert.Equal("", master)
}

func TestGetRedisesMasterPodPasswordError(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()
	rf.Spec.Auth.SecretPath = "redis-secret"

	pods := &corev1.PodList{
		Items: []corev1.Pod{
			{Status: corev1.PodStatus{PodIP: "0.0.0.0", Phase: corev1.PodRunning}},
		},
	}

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	ms.On("GetSecret", namespace, "redis-secret").Once().Return(nil, errors.New("secret unavailable"))
	mr := &mRedisService.Client{}

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	master, err := checker.GetRedisesMasterPod(rf)
	assert.Error(err)
	assert.Equal("", master)
	mr.AssertExpectations(t) // no IsMaster call should have happened
}

func TestGetRedisesMasterPodIsMasterError(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	pods := &corev1.PodList{
		Items: []corev1.Pod{
			{Status: corev1.PodStatus{PodIP: "0.0.0.0", Phase: corev1.PodRunning}},
		},
	}

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	mr := &mRedisService.Client{}
	mr.On("IsMaster", "0.0.0.0", "0", "").Once().Return(false, errors.New("timeout"))

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	master, err := checker.GetRedisesMasterPod(rf)
	assert.Error(err)
	assert.Equal("", master)
}

func TestGetRedisesMasterPodNotFound(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	pods := &corev1.PodList{
		Items: []corev1.Pod{
			{Status: corev1.PodStatus{PodIP: "0.0.0.0", Phase: corev1.PodRunning}},
		},
	}

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	mr := &mRedisService.Client{}
	mr.On("IsMaster", "0.0.0.0", "0", "").Once().Return(false, nil)

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	master, err := checker.GetRedisesMasterPod(rf)
	assert.Error(err)
	assert.Contains(err.Error(), "not found")
	assert.Equal("", master)
}

// --- GetRedisesSlavesPods ---

func TestGetRedisesSlavesPodsGetStatefulSetPodsError(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(nil, errors.New("boom"))
	mr := &mRedisService.Client{}

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	slaves, err := checker.GetRedisesSlavesPods(rf)
	assert.Error(err)
	assert.Nil(slaves)
}

func TestGetRedisesSlavesPodsPasswordError(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()
	rf.Spec.Auth.SecretPath = "redis-secret"

	pods := &corev1.PodList{
		Items: []corev1.Pod{
			{Status: corev1.PodStatus{PodIP: "0.0.0.0", Phase: corev1.PodRunning}},
		},
	}

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	ms.On("GetSecret", namespace, "redis-secret").Once().Return(nil, errors.New("secret unavailable"))
	mr := &mRedisService.Client{}

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	slaves, err := checker.GetRedisesSlavesPods(rf)
	assert.Error(err)
	assert.Empty(slaves)
	mr.AssertExpectations(t) // no IsMaster call should have happened
}

func TestGetRedisesSlavesPodsIsMasterError(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	pods := &corev1.PodList{
		Items: []corev1.Pod{
			{Status: corev1.PodStatus{PodIP: "0.0.0.0", Phase: corev1.PodRunning}},
		},
	}

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	mr := &mRedisService.Client{}
	mr.On("IsMaster", "0.0.0.0", "0", "").Once().Return(false, errors.New("timeout"))

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	slaves, err := checker.GetRedisesSlavesPods(rf)
	assert.Error(err)
	assert.Empty(slaves)
}

func TestGetStatefulSetUpdateRevision(t *testing.T) {
	tests := []struct {
		name             string
		ss               *appsv1.StatefulSet
		expectedUVersion string
		expectedError    error
	}{
		{
			name: "revision ok",
			ss: &appsv1.StatefulSet{
				Status: appsv1.StatefulSetStatus{
					UpdateRevision: "10",
				},
			},
			expectedUVersion: "10",
			expectedError:    nil,
		},
		{
			name:             "no stateful set",
			ss:               nil,
			expectedUVersion: "",
			expectedError:    errors.New("not found"),
		},
	}

	for _, test := range tests {
		assert := assert.New(t)

		rf := generateRF()
		ms := &mK8SService.Services{}
		ms.On("GetStatefulSet", namespace, rfservice.GetRedisName(rf)).Once().Return(test.ss, nil)
		mr := &mRedisService.Client{}

		checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)
		version, err := checker.GetStatefulSetUpdateRevision(rf)

		if test.expectedError == nil {
			assert.NoError(err)
		} else {
			assert.Error(err)
		}

		assert.Equal(version, test.expectedUVersion)
	}

}

func TestGetStatefulSetUpdateRevisionGetStatefulSetError(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()
	ms := &mK8SService.Services{}
	ms.On("GetStatefulSet", namespace, rfservice.GetRedisName(rf)).Once().Return(nil, errors.New("boom"))
	mr := &mRedisService.Client{}

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)
	version, err := checker.GetStatefulSetUpdateRevision(rf)

	assert.Error(err)
	assert.Equal("", version)
}

func TestGetRedisRevisionHash(t *testing.T) {
	tests := []struct {
		name          string
		pod           *corev1.Pod
		expectedHash  string
		expectedError error
	}{
		{
			name: "has ok",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						appsv1.ControllerRevisionHashLabelKey: "10",
					},
				},
			},
			expectedHash:  "10",
			expectedError: nil,
		},
		{
			name:          "no pod",
			pod:           nil,
			expectedHash:  "",
			expectedError: errors.New("not found"),
		},
		{
			name: "pod has no labels",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{},
			},
			expectedHash:  "",
			expectedError: errors.New("labels not found"),
		},
	}

	for _, test := range tests {
		assert := assert.New(t)

		rf := generateRF()
		ms := &mK8SService.Services{}
		ms.On("GetPod", namespace, "namepod").Once().Return(test.pod, nil)
		mr := &mRedisService.Client{}

		checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)
		hash, err := checker.GetRedisRevisionHash("namepod", rf)

		if test.expectedError == nil {
			assert.NoError(err)
		} else {
			assert.Error(err)
		}

		assert.Equal(hash, test.expectedHash)
	}

}

func TestGetRedisRevisionHashGetPodError(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()
	ms := &mK8SService.Services{}
	ms.On("GetPod", namespace, "namepod").Once().Return(nil, errors.New("boom"))
	mr := &mRedisService.Client{}

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)
	hash, err := checker.GetRedisRevisionHash("namepod", rf)

	assert.Error(err)
	assert.Equal("", hash)
}

func TestClusterRunning(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	allRunning := &corev1.PodList{
		Items: []corev1.Pod{
			{
				Status: corev1.PodStatus{
					PodIP: "0.0.0.0",
					Phase: corev1.PodRunning,
				},
			},
			{
				Status: corev1.PodStatus{
					PodIP: "1.1.1.1",
					Phase: corev1.PodRunning,
				},
			},
			{
				Status: corev1.PodStatus{
					PodIP: "1.1.1.1",
					Phase: corev1.PodRunning,
				},
			},
		},
	}

	notAllRunning := &corev1.PodList{
		Items: []corev1.Pod{
			{
				Status: corev1.PodStatus{
					PodIP: "0.0.0.0",
					Phase: corev1.PodRunning,
				},
			},
			{
				Status: corev1.PodStatus{
					PodIP: "1.1.1.1",
					Phase: corev1.PodPending,
				},
			},
			{
				Status: corev1.PodStatus{
					PodIP: "1.1.1.1",
					Phase: corev1.PodRunning,
				},
			},
		},
	}

	notAllReplicas := &corev1.PodList{
		Items: []corev1.Pod{
			{
				Status: corev1.PodStatus{
					PodIP: "0.0.0.0",
					Phase: corev1.PodRunning,
				},
			},
			{
				Status: corev1.PodStatus{
					PodIP: "1.1.1.1",
					Phase: corev1.PodRunning,
				},
			},
		},
	}

	ms := &mK8SService.Services{}
	ms.On("GetDeploymentPods", namespace, rfservice.GetSentinelName(rf)).Once().Return(allRunning, nil)
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(allRunning, nil)
	mr := &mRedisService.Client{}

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	assert.True(checker.IsClusterRunning(rf))

	ms.On("GetDeploymentPods", namespace, rfservice.GetSentinelName(rf)).Once().Return(allRunning, nil)
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(notAllReplicas, nil)
	assert.False(checker.IsClusterRunning(rf))

	ms.On("GetDeploymentPods", namespace, rfservice.GetSentinelName(rf)).Once().Return(notAllRunning, nil)
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(allRunning, nil)
	assert.False(checker.IsClusterRunning(rf))

}

// --- CheckMasterHealth ---
//
// CheckMasterHealth resolves the master via GetMasterIP (which itself calls
// IsMaster once per known pod), then performs a *second*, independent
// IsMaster call directly against the resolved master IP as the actual health
// check. Tests below account for both calls.

func TestCheckMasterHealthNoMasterFound(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(nil, errors.New(""))
	mr := &mRedisService.Client{}

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	healthy, masterIP, err := checker.CheckMasterHealth(rf)
	assert.NoError(err)
	assert.False(healthy)
	assert.Equal("", masterIP)
}

func TestCheckMasterHealthPasswordError(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()
	rf.Spec.Auth.SecretPath = "redis-secret"

	pods := &corev1.PodList{
		Items: []corev1.Pod{
			{Status: corev1.PodStatus{PodIP: "0.0.0.0", Phase: corev1.PodRunning}},
		},
	}

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	// First GetSecret call is made inside GetMasterIP and succeeds; the second
	// is CheckMasterHealth's own password fetch, which fails.
	ms.On("GetSecret", namespace, "redis-secret").Once().Return(&corev1.Secret{
		Data: map[string][]byte{"password": []byte("pw1")},
	}, nil)
	ms.On("GetSecret", namespace, "redis-secret").Once().Return(nil, errors.New("secret unavailable"))
	mr := &mRedisService.Client{}
	mr.On("IsMaster", "0.0.0.0", "0", "pw1").Once().Return(true, nil)

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	healthy, masterIP, err := checker.CheckMasterHealth(rf)
	assert.Error(err)
	assert.False(healthy)
	assert.Equal("0.0.0.0", masterIP)
}

func TestCheckMasterHealthIsMasterCheckError(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	pods := &corev1.PodList{
		Items: []corev1.Pod{
			{Status: corev1.PodStatus{PodIP: "0.0.0.0", Phase: corev1.PodRunning}},
		},
	}

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	mr := &mRedisService.Client{}
	mr.On("IsMaster", "0.0.0.0", "0", "").Once().Return(true, nil)                          // used by GetMasterIP
	mr.On("IsMaster", "0.0.0.0", "0", "").Once().Return(false, errors.New("ping timeout")) // used by the health check itself

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	healthy, masterIP, err := checker.CheckMasterHealth(rf)
	assert.NoError(err)
	assert.False(healthy)
	assert.Equal("0.0.0.0", masterIP)
}

func TestCheckMasterHealthTrue(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	pods := &corev1.PodList{
		Items: []corev1.Pod{
			{Status: corev1.PodStatus{PodIP: "0.0.0.0", Phase: corev1.PodRunning}},
		},
	}

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	mr := &mRedisService.Client{}
	mr.On("IsMaster", "0.0.0.0", "0", "").Twice().Return(true, nil)

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	healthy, masterIP, err := checker.CheckMasterHealth(rf)
	assert.NoError(err)
	assert.True(healthy)
	assert.Equal("0.0.0.0", masterIP)
}

func TestCheckMasterHealthFalse(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	pods := &corev1.PodList{
		Items: []corev1.Pod{
			{Status: corev1.PodStatus{PodIP: "0.0.0.0", Phase: corev1.PodRunning}},
		},
	}

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	mr := &mRedisService.Client{}
	mr.On("IsMaster", "0.0.0.0", "0", "").Once().Return(true, nil)  // used by GetMasterIP
	mr.On("IsMaster", "0.0.0.0", "0", "").Once().Return(false, nil) // used by the health check itself

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	healthy, masterIP, err := checker.CheckMasterHealth(rf)
	assert.NoError(err)
	assert.False(healthy)
	assert.Equal("0.0.0.0", masterIP)
}

// --- GetReplicaReplicationOffsets ---

func TestGetReplicaReplicationOffsetsGetStatefulSetPodsError(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(nil, errors.New(""))
	mr := &mRedisService.Client{}

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	replicas, err := checker.GetReplicaReplicationOffsets(rf)
	assert.Error(err)
	assert.Nil(replicas)
}

func TestGetReplicaReplicationOffsetsPasswordError(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()
	rf.Spec.Auth.SecretPath = "redis-secret"

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(&corev1.PodList{}, nil)
	ms.On("GetSecret", namespace, "redis-secret").Once().Return(nil, errors.New("secret unavailable"))
	mr := &mRedisService.Client{}

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	replicas, err := checker.GetReplicaReplicationOffsets(rf)
	assert.Error(err)
	assert.Nil(replicas)
}

func TestGetReplicaReplicationOffsetsEmptyPods(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(&corev1.PodList{}, nil)
	mr := &mRedisService.Client{}

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	replicas, err := checker.GetReplicaReplicationOffsets(rf)
	assert.NoError(err)
	assert.Empty(replicas)
}

func TestGetReplicaReplicationOffsetsFiltersNonRunningPods(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	deletionTime := metav1.Now()
	pods := &corev1.PodList{
		Items: []corev1.Pod{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "replica-running"},
				Status:     corev1.PodStatus{PodIP: "1.1.1.1", Phase: corev1.PodRunning},
			},
			{
				ObjectMeta: metav1.ObjectMeta{Name: "replica-pending"},
				Status:     corev1.PodStatus{PodIP: "2.2.2.2", Phase: corev1.PodPending},
			},
			{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "replica-terminating",
					DeletionTimestamp: &deletionTime,
				},
				Status: corev1.PodStatus{PodIP: "3.3.3.3", Phase: corev1.PodRunning},
			},
		},
	}

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	mr := &mRedisService.Client{}
	mr.On("GetReplicationInfo", "1.1.1.1", "0", "").Once().Return(&redis.ReplicationInfo{
		Role:             "slave",
		MasterLinkStatus: "up",
		SlaveReplOffset:  100,
	}, nil)

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	replicas, err := checker.GetReplicaReplicationOffsets(rf)
	assert.NoError(err)
	if assert.Len(replicas, 1) {
		assert.Equal("1.1.1.1", replicas[0].IP)
	}
	ms.AssertExpectations(t)
	mr.AssertExpectations(t) // GetReplicationInfo must not be called for pending/terminating pods
}

func TestGetReplicaReplicationOffsetsSkipsPodOnReplicationInfoError(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	pods := &corev1.PodList{
		Items: []corev1.Pod{
			{ObjectMeta: metav1.ObjectMeta{Name: "replica-bad"}, Status: corev1.PodStatus{PodIP: "1.1.1.1", Phase: corev1.PodRunning}},
			{ObjectMeta: metav1.ObjectMeta{Name: "replica-good"}, Status: corev1.PodStatus{PodIP: "2.2.2.2", Phase: corev1.PodRunning}},
		},
	}

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	mr := &mRedisService.Client{}
	mr.On("GetReplicationInfo", "1.1.1.1", "0", "").Once().Return(nil, errors.New("timeout"))
	mr.On("GetReplicationInfo", "2.2.2.2", "0", "").Once().Return(&redis.ReplicationInfo{
		Role:             "slave",
		MasterLinkStatus: "up",
		SlaveReplOffset:  200,
	}, nil)

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	replicas, err := checker.GetReplicaReplicationOffsets(rf)
	assert.NoError(err, "a failed pod should be skipped, not surfaced as an error")
	if assert.Len(replicas, 1) {
		assert.Equal("2.2.2.2", replicas[0].IP)
		assert.EqualValues(200, replicas[0].ReplicationOffset)
	}
}

func TestGetReplicaReplicationOffsetsExcludesMaster(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	pods := &corev1.PodList{
		Items: []corev1.Pod{
			{ObjectMeta: metav1.ObjectMeta{Name: "master"}, Status: corev1.PodStatus{PodIP: "1.1.1.1", Phase: corev1.PodRunning}},
			{ObjectMeta: metav1.ObjectMeta{Name: "replica"}, Status: corev1.PodStatus{PodIP: "2.2.2.2", Phase: corev1.PodRunning}},
		},
	}

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	mr := &mRedisService.Client{}
	mr.On("GetReplicationInfo", "1.1.1.1", "0", "").Once().Return(&redis.ReplicationInfo{
		Role: "master",
	}, nil)
	mr.On("GetReplicationInfo", "2.2.2.2", "0", "").Once().Return(&redis.ReplicationInfo{
		Role:             "slave",
		MasterLinkStatus: "up",
		SlaveReplOffset:  50,
	}, nil)

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	replicas, err := checker.GetReplicaReplicationOffsets(rf)
	assert.NoError(err)
	if assert.Len(replicas, 1) {
		assert.Equal("2.2.2.2", replicas[0].IP)
	}
}

func TestGetReplicaReplicationOffsetsIsReadyFlag(t *testing.T) {
	tests := []struct {
		name             string
		syncInProgress   bool
		masterLinkStatus string
		expectedReady    bool
	}{
		{name: "synced and link up is ready", syncInProgress: false, masterLinkStatus: "up", expectedReady: true},
		{name: "syncing is not ready", syncInProgress: true, masterLinkStatus: "up", expectedReady: false},
		{name: "link down is not ready", syncInProgress: false, masterLinkStatus: "down", expectedReady: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			rf := generateRF()

			pods := &corev1.PodList{
				Items: []corev1.Pod{
					{ObjectMeta: metav1.ObjectMeta{Name: "replica"}, Status: corev1.PodStatus{PodIP: "1.1.1.1", Phase: corev1.PodRunning}},
				},
			}

			ms := &mK8SService.Services{}
			ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
			mr := &mRedisService.Client{}
			mr.On("GetReplicationInfo", "1.1.1.1", "0", "").Once().Return(&redis.ReplicationInfo{
				Role:             "slave",
				SyncInProgress:   test.syncInProgress,
				MasterLinkStatus: test.masterLinkStatus,
				SlaveReplOffset:  42,
			}, nil)

			checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

			replicas, err := checker.GetReplicaReplicationOffsets(rf)
			assert.NoError(err)
			if assert.Len(replicas, 1) {
				assert.Equal(test.expectedReady, replicas[0].IsReady)
				assert.EqualValues(42, replicas[0].ReplicationOffset)
			}
		})
	}
}

// --- GetBestReplicaForPromotion ---

func TestGetBestReplicaForPromotionUnderlyingError(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(nil, errors.New("boom"))
	mr := &mRedisService.Client{}

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	best, err := checker.GetBestReplicaForPromotion(rf)
	assert.Error(err)
	assert.Nil(best)
}

func TestGetBestReplicaForPromotionNoReplicas(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(&corev1.PodList{}, nil)
	mr := &mRedisService.Client{}

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	best, err := checker.GetBestReplicaForPromotion(rf)
	assert.Error(err)
	assert.Nil(best)
	assert.Contains(err.Error(), "no replicas available for promotion")
}

func TestGetBestReplicaForPromotionPicksHighestReadyOffset(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	pods := &corev1.PodList{
		Items: []corev1.Pod{
			{ObjectMeta: metav1.ObjectMeta{Name: "replica-low"}, Status: corev1.PodStatus{PodIP: "1.1.1.1", Phase: corev1.PodRunning}},
			{ObjectMeta: metav1.ObjectMeta{Name: "replica-high"}, Status: corev1.PodStatus{PodIP: "2.2.2.2", Phase: corev1.PodRunning}},
			{ObjectMeta: metav1.ObjectMeta{Name: "replica-notready"}, Status: corev1.PodStatus{PodIP: "3.3.3.3", Phase: corev1.PodRunning}},
		},
	}

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	mr := &mRedisService.Client{}
	mr.On("GetReplicationInfo", "1.1.1.1", "0", "").Once().Return(&redis.ReplicationInfo{
		Role: "slave", MasterLinkStatus: "up", SlaveReplOffset: 100,
	}, nil)
	mr.On("GetReplicationInfo", "2.2.2.2", "0", "").Once().Return(&redis.ReplicationInfo{
		Role: "slave", MasterLinkStatus: "up", SlaveReplOffset: 500,
	}, nil)
	// Highest overall offset, but not ready: must lose to replica-high which is ready.
	mr.On("GetReplicationInfo", "3.3.3.3", "0", "").Once().Return(&redis.ReplicationInfo{
		Role: "slave", MasterLinkStatus: "up", SyncInProgress: true, SlaveReplOffset: 900,
	}, nil)

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	best, err := checker.GetBestReplicaForPromotion(rf)
	assert.NoError(err)
	if assert.NotNil(best) {
		assert.Equal("2.2.2.2", best.IP)
		assert.EqualValues(500, best.ReplicationOffset)
		assert.True(best.IsReady)
	}
}

func TestGetBestReplicaForPromotionFallsBackWhenNoneReady(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	pods := &corev1.PodList{
		Items: []corev1.Pod{
			{ObjectMeta: metav1.ObjectMeta{Name: "replica-a"}, Status: corev1.PodStatus{PodIP: "1.1.1.1", Phase: corev1.PodRunning}},
			{ObjectMeta: metav1.ObjectMeta{Name: "replica-b"}, Status: corev1.PodStatus{PodIP: "2.2.2.2", Phase: corev1.PodRunning}},
		},
	}

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	mr := &mRedisService.Client{}
	// Neither replica is ready (both still syncing), so the "ready" pass finds
	// nothing and GetBestReplicaForPromotion must fall back to the highest
	// offset among all replicas regardless of readiness.
	mr.On("GetReplicationInfo", "1.1.1.1", "0", "").Once().Return(&redis.ReplicationInfo{
		Role: "slave", MasterLinkStatus: "up", SyncInProgress: true, SlaveReplOffset: 300,
	}, nil)
	mr.On("GetReplicationInfo", "2.2.2.2", "0", "").Once().Return(&redis.ReplicationInfo{
		Role: "slave", MasterLinkStatus: "up", SyncInProgress: true, SlaveReplOffset: 700,
	}, nil)

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	best, err := checker.GetBestReplicaForPromotion(rf)
	assert.NoError(err)
	if assert.NotNil(best) {
		assert.Equal("2.2.2.2", best.IP, "fallback should still pick highest offset even though not ready")
		assert.False(best.IsReady)
	}
}

// --- CheckRedisSlavesReady ---

func TestCheckRedisSlavesReadyPasswordError(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()
	rf.Spec.Auth.SecretPath = "redis-secret"

	ms := &mK8SService.Services{}
	ms.On("GetSecret", namespace, "redis-secret").Once().Return(nil, errors.New("secret unavailable"))
	mr := &mRedisService.Client{}

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	ready, err := checker.CheckRedisSlavesReady("1.1.1.1", rf)
	assert.Error(err)
	assert.False(ready)
	mr.AssertExpectations(t) // no SlaveIsReady call should have happened
}

func TestCheckRedisSlavesReadyTrue(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	ms := &mK8SService.Services{}
	mr := &mRedisService.Client{}
	mr.On("SlaveIsReady", "1.1.1.1", "0", "").Once().Return(true, nil)

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	ready, err := checker.CheckRedisSlavesReady("1.1.1.1", rf)
	assert.NoError(err)
	assert.True(ready)
}

func TestCheckRedisSlavesReadyFalse(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	ms := &mK8SService.Services{}
	mr := &mRedisService.Client{}
	mr.On("SlaveIsReady", "1.1.1.1", "0", "").Once().Return(false, nil)

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	ready, err := checker.CheckRedisSlavesReady("1.1.1.1", rf)
	assert.NoError(err)
	assert.False(ready)
}

func TestCheckRedisSlavesReadyError(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	ms := &mK8SService.Services{}
	mr := &mRedisService.Client{}
	mr.On("SlaveIsReady", "1.1.1.1", "0", "").Once().Return(false, errors.New("check failed"))

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	ready, err := checker.CheckRedisSlavesReady("1.1.1.1", rf)
	assert.Error(err)
	assert.False(ready)
}

// --- CheckIfMasterLocalhost ---

func TestCheckIfMasterLocalhostGetRedisesIPsEmpty(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(&corev1.PodList{}, nil)
	mr := &mRedisService.Client{}

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	ok, err := checker.CheckIfMasterLocalhost(rf)
	assert.Error(err)
	assert.False(ok)
}

func TestCheckIfMasterLocalhostGetRedisesIPsError(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(nil, errors.New("boom"))
	mr := &mRedisService.Client{}

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	ok, err := checker.CheckIfMasterLocalhost(rf)
	assert.Error(err)
	assert.False(ok)
}

func TestCheckIfMasterLocalhostPasswordError(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()
	rf.Spec.Auth.SecretPath = "redis-secret"

	pods := &corev1.PodList{
		Items: []corev1.Pod{
			{Status: corev1.PodStatus{PodIP: "1.1.1.1", Phase: corev1.PodRunning}},
		},
	}

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	ms.On("GetSecret", namespace, "redis-secret").Once().Return(nil, errors.New("secret unavailable"))
	mr := &mRedisService.Client{}

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	ok, err := checker.CheckIfMasterLocalhost(rf)
	assert.Error(err)
	assert.False(ok)
}

func TestCheckIfMasterLocalhostGetSlaveOfError(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	pods := &corev1.PodList{
		Items: []corev1.Pod{
			{Status: corev1.PodStatus{PodIP: "1.1.1.1", Phase: corev1.PodRunning}},
		},
	}

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	mr := &mRedisService.Client{}
	mr.On("GetSlaveOf", "1.1.1.1", "0", "").Once().Return("", errors.New("timeout"))

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	ok, err := checker.CheckIfMasterLocalhost(rf)
	assert.Error(err)
	assert.False(ok)
}

func TestCheckIfMasterLocalhostEmptyMasterUnexpectedState(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	pods := &corev1.PodList{
		Items: []corev1.Pod{
			{Status: corev1.PodStatus{PodIP: "1.1.1.1", Phase: corev1.PodRunning}},
		},
	}

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	mr := &mRedisService.Client{}
	mr.On("GetSlaveOf", "1.1.1.1", "0", "").Once().Return("", nil)

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	ok, err := checker.CheckIfMasterLocalhost(rf)
	assert.Error(err)
	assert.False(ok)
	assert.Contains(err.Error(), "unexpected master state")
}

func TestCheckIfMasterLocalhostAllLocalhost(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	pods := &corev1.PodList{
		Items: []corev1.Pod{
			{Status: corev1.PodStatus{PodIP: "1.1.1.1", Phase: corev1.PodRunning}},
			{Status: corev1.PodStatus{PodIP: "2.2.2.2", Phase: corev1.PodRunning}},
		},
	}

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	mr := &mRedisService.Client{}
	mr.On("GetSlaveOf", "1.1.1.1", "0", "").Once().Return("127.0.0.1", nil)
	mr.On("GetSlaveOf", "2.2.2.2", "0", "").Once().Return("127.0.0.1", nil)

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	ok, err := checker.CheckIfMasterLocalhost(rf)
	assert.NoError(err)
	assert.True(ok)
}

func TestCheckIfMasterLocalhostSomeNotLocalhost(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	pods := &corev1.PodList{
		Items: []corev1.Pod{
			{Status: corev1.PodStatus{PodIP: "1.1.1.1", Phase: corev1.PodRunning}},
			{Status: corev1.PodStatus{PodIP: "2.2.2.2", Phase: corev1.PodRunning}},
		},
	}

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	mr := &mRedisService.Client{}
	mr.On("GetSlaveOf", "1.1.1.1", "0", "").Once().Return("127.0.0.1", nil)
	mr.On("GetSlaveOf", "2.2.2.2", "0", "").Once().Return("1.1.1.1", nil)

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	ok, err := checker.CheckIfMasterLocalhost(rf)
	assert.NoError(err)
	assert.False(ok)
}

// --- CheckSentinelQuorum ---
//
// getQuorum(rf) = rf.Spec.Sentinel.Replicas/2 + 1. Tests below set Replicas
// to 5, so quorum is 3.

func TestCheckSentinelQuorumGetSentinelsIPsError(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	ms := &mK8SService.Services{}
	ms.On("GetDeploymentPods", namespace, rfservice.GetSentinelName(rf)).Once().Return(nil, errors.New("boom"))
	mr := &mRedisService.Client{}

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	cnt, err := checker.CheckSentinelQuorum(rf)
	assert.Error(err)
	assert.Equal(-1, cnt)
}

func TestCheckSentinelQuorumInsufficientSentinels(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()
	rf.Spec.Sentinel.Replicas = 5 // quorum = 3

	pods := &corev1.PodList{
		Items: []corev1.Pod{
			{Status: corev1.PodStatus{PodIP: "1.1.1.1", Phase: corev1.PodRunning}},
			{Status: corev1.PodStatus{PodIP: "2.2.2.2", Phase: corev1.PodRunning}},
		},
	}

	ms := &mK8SService.Services{}
	ms.On("GetDeploymentPods", namespace, rfservice.GetSentinelName(rf)).Once().Return(pods, nil)
	mr := &mRedisService.Client{}

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	cnt, err := checker.CheckSentinelQuorum(rf)
	assert.Error(err)
	assert.Equal(1, cnt, "unhealthy count should be quorum minus available sentinels")
	mr.AssertExpectations(t) // no SentinelCheckQuorum calls should have happened
}

func TestCheckSentinelQuorumAllHealthy(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()
	rf.Spec.Sentinel.Replicas = 5 // quorum = 3

	pods := &corev1.PodList{
		Items: []corev1.Pod{
			{Status: corev1.PodStatus{PodIP: "1.1.1.1", Phase: corev1.PodRunning}},
			{Status: corev1.PodStatus{PodIP: "2.2.2.2", Phase: corev1.PodRunning}},
			{Status: corev1.PodStatus{PodIP: "3.3.3.3", Phase: corev1.PodRunning}},
			{Status: corev1.PodStatus{PodIP: "4.4.4.4", Phase: corev1.PodRunning}},
			{Status: corev1.PodStatus{PodIP: "5.5.5.5", Phase: corev1.PodRunning}},
		},
	}

	ms := &mK8SService.Services{}
	ms.On("GetDeploymentPods", namespace, rfservice.GetSentinelName(rf)).Once().Return(pods, nil)
	mr := &mRedisService.Client{}
	for _, ip := range []string{"1.1.1.1", "2.2.2.2", "3.3.3.3", "4.4.4.4", "5.5.5.5"} {
		mr.On("SentinelCheckQuorum", ip).Once().Return(nil)
	}

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	cnt, err := checker.CheckSentinelQuorum(rf)
	assert.NoError(err)
	assert.Equal(0, cnt)
}

func TestCheckSentinelQuorumSomeFailUnderThreshold(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()
	rf.Spec.Sentinel.Replicas = 5 // quorum = 3

	pods := &corev1.PodList{
		Items: []corev1.Pod{
			{Status: corev1.PodStatus{PodIP: "1.1.1.1", Phase: corev1.PodRunning}},
			{Status: corev1.PodStatus{PodIP: "2.2.2.2", Phase: corev1.PodRunning}},
			{Status: corev1.PodStatus{PodIP: "3.3.3.3", Phase: corev1.PodRunning}},
			{Status: corev1.PodStatus{PodIP: "4.4.4.4", Phase: corev1.PodRunning}},
			{Status: corev1.PodStatus{PodIP: "5.5.5.5", Phase: corev1.PodRunning}},
		},
	}

	ms := &mK8SService.Services{}
	ms.On("GetDeploymentPods", namespace, rfservice.GetSentinelName(rf)).Once().Return(pods, nil)
	mr := &mRedisService.Client{}
	mr.On("SentinelCheckQuorum", "1.1.1.1").Once().Return(errors.New("unreachable"))
	mr.On("SentinelCheckQuorum", "2.2.2.2").Once().Return(errors.New("unreachable"))
	mr.On("SentinelCheckQuorum", "3.3.3.3").Once().Return(nil)
	mr.On("SentinelCheckQuorum", "4.4.4.4").Once().Return(nil)
	mr.On("SentinelCheckQuorum", "5.5.5.5").Once().Return(nil)

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	cnt, err := checker.CheckSentinelQuorum(rf)
	assert.NoError(err, "unhealthy count still under quorum threshold should not be an error")
	assert.Equal(2, cnt)
}

func TestCheckSentinelQuorumExceedsThreshold(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()
	rf.Spec.Sentinel.Replicas = 5 // quorum = 3

	pods := &corev1.PodList{
		Items: []corev1.Pod{
			{Status: corev1.PodStatus{PodIP: "1.1.1.1", Phase: corev1.PodRunning}},
			{Status: corev1.PodStatus{PodIP: "2.2.2.2", Phase: corev1.PodRunning}},
			{Status: corev1.PodStatus{PodIP: "3.3.3.3", Phase: corev1.PodRunning}},
			{Status: corev1.PodStatus{PodIP: "4.4.4.4", Phase: corev1.PodRunning}},
			{Status: corev1.PodStatus{PodIP: "5.5.5.5", Phase: corev1.PodRunning}},
		},
	}

	ms := &mK8SService.Services{}
	ms.On("GetDeploymentPods", namespace, rfservice.GetSentinelName(rf)).Once().Return(pods, nil)
	mr := &mRedisService.Client{}
	mr.On("SentinelCheckQuorum", "1.1.1.1").Once().Return(errors.New("unreachable"))
	mr.On("SentinelCheckQuorum", "2.2.2.2").Once().Return(errors.New("unreachable"))
	mr.On("SentinelCheckQuorum", "3.3.3.3").Once().Return(errors.New("unreachable"))
	mr.On("SentinelCheckQuorum", "4.4.4.4").Once().Return(nil)
	mr.On("SentinelCheckQuorum", "5.5.5.5").Once().Return(nil)

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	cnt, err := checker.CheckSentinelQuorum(rf)
	assert.Error(err)
	assert.Equal(3, cnt)
}

// --- GetSentinelsIPs ---

func TestGetSentinelsIPsGetDeploymentPodsError(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	ms := &mK8SService.Services{}
	ms.On("GetDeploymentPods", namespace, rfservice.GetSentinelName(rf)).Once().Return(nil, errors.New("boom"))
	mr := &mRedisService.Client{}

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	ips, err := checker.GetSentinelsIPs(rf)
	assert.Error(err)
	assert.Nil(ips)
}

func TestGetSentinelsIPsFiltersNonRunningAndTerminating(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	deletionTime := metav1.Now()
	pods := &corev1.PodList{
		Items: []corev1.Pod{
			{Status: corev1.PodStatus{PodIP: "1.1.1.1", Phase: corev1.PodRunning}},
			{Status: corev1.PodStatus{PodIP: "2.2.2.2", Phase: corev1.PodPending}},
			{
				ObjectMeta: metav1.ObjectMeta{DeletionTimestamp: &deletionTime},
				Status:     corev1.PodStatus{PodIP: "3.3.3.3", Phase: corev1.PodRunning},
			},
		},
	}

	ms := &mK8SService.Services{}
	ms.On("GetDeploymentPods", namespace, rfservice.GetSentinelName(rf)).Once().Return(pods, nil)
	mr := &mRedisService.Client{}

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	ips, err := checker.GetSentinelsIPs(rf)
	assert.NoError(err)
	assert.Equal([]string{"1.1.1.1"}, ips)
}
