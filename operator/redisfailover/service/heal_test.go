package service_test

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/saremox/redis-operator/log"
	mK8SService "github.com/saremox/redis-operator/mocks/service/k8s"
	mRedisService "github.com/saremox/redis-operator/mocks/service/redis"
	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
)

func TestSetOldestAsMasterNewMasterError(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	pods := &corev1.PodList{
		Items: []corev1.Pod{
			{
				Status: corev1.PodStatus{
					PodIP: "0.0.0.0",
				},
			},
		},
	}

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	ms.On("UpdatePodLabels", namespace, mock.AnythingOfType("string"), mock.Anything).Return(nil)
	mr := &mRedisService.Client{}
	mr.On("MakeMaster", "0.0.0.0", "0", "").Once().Return(errors.New(""))

	healer := rfservice.NewRedisFailoverHealer(ms, mr, log.DummyLogger{})

	err := healer.SetOldestAsMaster(rf)
	assert.Error(err)
}

func TestSetOldestAsMaster(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	pods := &corev1.PodList{
		Items: []corev1.Pod{
			{
				Status: corev1.PodStatus{
					PodIP: "0.0.0.0",
				},
			},
		},
	}

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	ms.On("UpdatePodLabels", namespace, mock.AnythingOfType("string"), mock.Anything).Once().Return(nil)
	mr := &mRedisService.Client{}
	mr.On("MakeMaster", "0.0.0.0", "0", "").Once().Return(nil)

	healer := rfservice.NewRedisFailoverHealer(ms, mr, log.DummyLogger{})

	err := healer.SetOldestAsMaster(rf)
	assert.NoError(err)
}

func TestSetOldestAsMasterMultiplePodsMakeSlaveOfError(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	pods := &corev1.PodList{
		Items: []corev1.Pod{
			{
				Status: corev1.PodStatus{
					PodIP: "0.0.0.0",
				},
			},
			{
				Status: corev1.PodStatus{
					PodIP: "1.1.1.1",
				},
			},
		},
	}

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	ms.On("UpdatePodLabels", namespace, mock.AnythingOfType("string"), mock.Anything).Return(nil)
	mr := &mRedisService.Client{}
	mr.On("MakeMaster", "0.0.0.0", "0", "").Once().Return(nil)
	mr.On("MakeSlaveOfWithPort", "1.1.1.1", "0", "0.0.0.0", "0", "").Once().Return(errors.New(""))

	healer := rfservice.NewRedisFailoverHealer(ms, mr, log.DummyLogger{})

	err := healer.SetOldestAsMaster(rf)
	assert.NoError(err)
}

func TestSetOldestAsMasterMultiplePods(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	pods := &corev1.PodList{
		Items: []corev1.Pod{
			{
				Status: corev1.PodStatus{
					PodIP: "0.0.0.0",
				},
			},
			{
				Status: corev1.PodStatus{
					PodIP: "1.1.1.1",
				},
			},
		},
	}

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	ms.On("UpdatePodLabels", namespace, mock.AnythingOfType("string"), mock.Anything).Return(nil)
	mr := &mRedisService.Client{}
	mr.On("MakeMaster", "0.0.0.0", "0", "").Once().Return(nil)
	mr.On("MakeSlaveOfWithPort", "1.1.1.1", "0", "0.0.0.0", "0", "").Once().Return(nil)

	healer := rfservice.NewRedisFailoverHealer(ms, mr, log.DummyLogger{})

	err := healer.SetOldestAsMaster(rf)
	assert.NoError(err)
}

func TestSetOldestAsMasterOrdering(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	pods := &corev1.PodList{
		Items: []corev1.Pod{
			{
				ObjectMeta: metav1.ObjectMeta{
					CreationTimestamp: metav1.Time{
						Time: time.Now(),
					},
				},
				Status: corev1.PodStatus{
					PodIP: "0.0.0.0",
				},
			},
			{
				ObjectMeta: metav1.ObjectMeta{
					CreationTimestamp: metav1.Time{
						Time: time.Now().Add(-1 * time.Hour), // This is older by 1 hour
					},
				},
				Status: corev1.PodStatus{
					PodIP: "1.1.1.1",
				},
			},
		},
	}

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	ms.On("UpdatePodLabels", namespace, mock.AnythingOfType("string"), mock.Anything).Return(nil)
	mr := &mRedisService.Client{}
	mr.On("MakeMaster", "1.1.1.1", "0", "").Once().Return(nil)
	mr.On("MakeSlaveOfWithPort", "0.0.0.0", "0", "1.1.1.1", "0", "").Once().Return(nil)

	healer := rfservice.NewRedisFailoverHealer(ms, mr, log.DummyLogger{})

	err := healer.SetOldestAsMaster(rf)
	assert.NoError(err)
}

func TestSetMasterOnAllMakeMasterError(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	pods := &corev1.PodList{
		Items: []corev1.Pod{
			{
				Status: corev1.PodStatus{
					PodIP: "0.0.0.0",
				},
			},
			{
				Status: corev1.PodStatus{
					PodIP: "1.1.1.1",
				},
			},
		},
	}

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	ms.On("UpdatePodLabels", namespace, mock.AnythingOfType("string"), mock.Anything).Once().Return(nil)
	mr := &mRedisService.Client{}
	mr.On("IsMaster", "0.0.0.0", "0", "").Return(false, errors.New(""))
	healer := rfservice.NewRedisFailoverHealer(ms, mr, log.DummyLogger{})

	err := healer.SetMasterOnAll("0.0.0.0", rf)
	assert.Error(err)
}

func TestSetMasterOnAllMakeSlaveOfError(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	pods := &corev1.PodList{
		Items: []corev1.Pod{
			{
				Status: corev1.PodStatus{
					PodIP: "0.0.0.0",
				},
			},
			{
				Status: corev1.PodStatus{
					PodIP: "1.1.1.1",
				},
			},
		},
	}

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	ms.On("UpdatePodLabels", namespace, mock.AnythingOfType("string"), mock.Anything).Return(nil)
	mr := &mRedisService.Client{}
	mr.On("IsMaster", "0.0.0.0", "0", "").Return(true, nil)
	mr.On("MakeSlaveOfWithPort", "1.1.1.1", "0", "0.0.0.0", "0", "").Once().Return(errors.New(""))

	healer := rfservice.NewRedisFailoverHealer(ms, mr, log.DummyLogger{})

	err := healer.SetMasterOnAll("0.0.0.0", rf)
	assert.Error(err)
}

func TestSetMasterOnAll(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	pods := &corev1.PodList{
		Items: []corev1.Pod{
			{
				Status: corev1.PodStatus{
					PodIP: "0.0.0.0",
				},
			},
			{
				Status: corev1.PodStatus{
					PodIP: "1.1.1.1",
				},
			},
		},
	}

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	ms.On("UpdatePodLabels", namespace, mock.AnythingOfType("string"), mock.Anything).Return(nil)
	mr := &mRedisService.Client{}
	mr.On("IsMaster", "0.0.0.0", "0", "").Return(true, nil)
	mr.On("MakeSlaveOfWithPort", "1.1.1.1", "0", "0.0.0.0", "0", "").Once().Return(nil)

	healer := rfservice.NewRedisFailoverHealer(ms, mr, log.DummyLogger{})

	err := healer.SetMasterOnAll("0.0.0.0", rf)
	assert.NoError(err)
}

func TestSetMasterOnAllRejectsIPNotOwnedByRF(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	// None of this RedisFailover's own pods have this IP: it could be a stale
	// value resolved earlier in the reconcile that's since been reassigned to
	// an unrelated pod, possibly in a different namespace/RedisFailover. See
	// https://github.com/spotahome/redis-operator/issues/698.
	pods := &corev1.PodList{
		Items: []corev1.Pod{
			{Status: corev1.PodStatus{PodIP: "1.1.1.1"}},
			{Status: corev1.PodStatus{PodIP: "2.2.2.2"}},
		},
	}

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	mr := &mRedisService.Client{}

	healer := rfservice.NewRedisFailoverHealer(ms, mr, log.DummyLogger{})

	err := healer.SetMasterOnAll("9.9.9.9", rf)
	assert.Error(err, "should refuse to act on an IP that isn't currently one of this RedisFailover's own pods")
	ms.AssertExpectations(t)
	mr.AssertExpectations(t) // no IsMaster/MakeSlaveOfWithPort calls should have happened
}

func TestSetMasterOnAllSlaveAlreadyLabeled(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	pods := &corev1.PodList{
		Items: []corev1.Pod{
			{
				Status: corev1.PodStatus{
					PodIP: "0.0.0.0",
				},
			},
			{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"redisfailovers-role": "slave"},
				},
				Status: corev1.PodStatus{
					PodIP: "1.1.1.1",
				},
			},
		},
	}

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	mr := &mRedisService.Client{}
	mr.On("IsMaster", "0.0.0.0", "0", "").Return(true, nil)
	mr.On("MakeSlaveOfWithPort", "1.1.1.1", "0", "0.0.0.0", "0", "").Once().Return(nil)

	healer := rfservice.NewRedisFailoverHealer(ms, mr, log.DummyLogger{})

	err := healer.SetMasterOnAll("0.0.0.0", rf)
	assert.NoError(err)
	ms.AssertExpectations(t) // pod already has the slave label, so UpdatePodLabels must not be called
}

func TestSetExternalMasterOnAll(t *testing.T) {
	tests := []struct {
		name                  string
		errorOnGetStatefulSet bool
		errorOnMakeSlaveOf    bool
	}{
		{
			name: "makes all redis pods a slave of provided ip and port",
		},
		{
			name:                  "errors on failure to get stateful set pods",
			errorOnGetStatefulSet: true,
		},
		{
			name:               "errors on failure to make pod a slave",
			errorOnMakeSlaveOf: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			rf := generateRF()
			pods := &corev1.PodList{
				Items: []corev1.Pod{
					{
						Status: corev1.PodStatus{
							PodIP: "0.0.0.0",
						},
					},
					{
						Status: corev1.PodStatus{
							PodIP: "1.1.1.1",
						},
					},
				},
			}

			ms := &mK8SService.Services{}
			expectError := false

			if test.errorOnGetStatefulSet {
				expectError = true
				ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(nil, errors.New(""))
			} else {
				ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
			}

			mr := &mRedisService.Client{}
			if !expectError {
				mr.On("MakeSlaveOfWithPort", "0.0.0.0", "0", "5.5.5.5", "6379", "").Once().Return(nil)
				if test.errorOnMakeSlaveOf {
					expectError = true
					mr.On("MakeSlaveOfWithPort", "1.1.1.1", "0", "5.5.5.5", "6379", "").Once().Return(errors.New(""))
				} else {
					mr.On("MakeSlaveOfWithPort", "1.1.1.1", "0", "5.5.5.5", "6379", "").Once().Return(nil)
				}
			}

			healer := rfservice.NewRedisFailoverHealer(ms, mr, log.DummyLogger{})

			err := healer.SetExternalMasterOnAll("5.5.5.5", "6379", rf)

			if expectError {
				assert.Error(err)
			} else {
				assert.NoError(err)
			}
			ms.AssertExpectations(t)
			mr.AssertExpectations(t)
		})
	}
}

func TestPromoteBestReplicaSuccess(t *testing.T) {
	assert := assert.New(t)
	rf := generateRF()

	newMasterIP := "1.1.1.1"
	replicaIP := "2.2.2.2"

	pods := &corev1.PodList{
		Items: []corev1.Pod{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "pod-master"},
				Status: corev1.PodStatus{
					PodIP: newMasterIP,
					Phase: corev1.PodRunning,
				},
			},
			{
				ObjectMeta: metav1.ObjectMeta{Name: "pod-replica"},
				Status: corev1.PodStatus{
					PodIP: replicaIP,
					Phase: corev1.PodRunning,
				},
			},
		},
	}

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	ms.On("UpdatePodLabels", namespace, mock.AnythingOfType("string"), mock.Anything).Return(nil)

	mr := &mRedisService.Client{}
	mr.On("MakeMaster", newMasterIP, "0", "").Once().Return(nil)
	mr.On("MakeSlaveOfWithPort", replicaIP, "0", newMasterIP, "0", "").Once().Return(nil)

	healer := rfservice.NewRedisFailoverHealer(ms, mr, log.DummyLogger{})

	err := healer.PromoteBestReplica(newMasterIP, rf)
	assert.NoError(err)
	ms.AssertExpectations(t)
	mr.AssertExpectations(t)
}

func TestPromoteBestReplicaReplicaRepointerFails(t *testing.T) {
	assert := assert.New(t)
	rf := generateRF()

	newMasterIP := "1.1.1.1"
	replicaIP := "2.2.2.2"

	pods := &corev1.PodList{
		Items: []corev1.Pod{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "pod-master"},
				Status: corev1.PodStatus{
					PodIP: newMasterIP,
					Phase: corev1.PodRunning,
				},
			},
			{
				ObjectMeta: metav1.ObjectMeta{Name: "pod-replica"},
				Status: corev1.PodStatus{
					PodIP: replicaIP,
					Phase: corev1.PodRunning,
				},
			},
		},
	}

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	ms.On("UpdatePodLabels", namespace, "pod-master", mock.Anything).Return(nil)

	mr := &mRedisService.Client{}
	mr.On("MakeMaster", newMasterIP, "0", "").Once().Return(nil)
	mr.On("MakeSlaveOfWithPort", replicaIP, "0", newMasterIP, "0", "").Once().Return(errors.New("replica repoint failed"))

	healer := rfservice.NewRedisFailoverHealer(ms, mr, log.DummyLogger{})

	err := healer.PromoteBestReplica(newMasterIP, rf)
	assert.Error(err, "partial failover should not be reported as success")
	assert.True(errors.Is(err, rfservice.ErrPartialReconciliation), "replica repoint failure should be wrapped as ErrPartialReconciliation")
	ms.AssertExpectations(t)
	mr.AssertExpectations(t)
}

func TestPromoteBestReplicaLabelUpdateFails(t *testing.T) {
	assert := assert.New(t)
	rf := generateRF()

	newMasterIP := "1.1.1.1"
	replicaIP := "2.2.2.2"

	pods := &corev1.PodList{
		Items: []corev1.Pod{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "pod-master"},
				Status: corev1.PodStatus{
					PodIP: newMasterIP,
					Phase: corev1.PodRunning,
				},
			},
			{
				ObjectMeta: metav1.ObjectMeta{Name: "pod-replica"},
				Status: corev1.PodStatus{
					PodIP: replicaIP,
					Phase: corev1.PodRunning,
				},
			},
		},
	}

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	ms.On("UpdatePodLabels", namespace, "pod-master", mock.Anything).Return(nil)
	ms.On("UpdatePodLabels", namespace, "pod-replica", mock.Anything).Return(errors.New("label update failed"))

	mr := &mRedisService.Client{}
	mr.On("MakeMaster", newMasterIP, "0", "").Once().Return(nil)
	mr.On("MakeSlaveOfWithPort", replicaIP, "0", newMasterIP, "0", "").Once().Return(nil)

	healer := rfservice.NewRedisFailoverHealer(ms, mr, log.DummyLogger{})

	err := healer.PromoteBestReplica(newMasterIP, rf)
	assert.Error(err, "label update failure should not be reported as success")
	assert.True(errors.Is(err, rfservice.ErrPartialReconciliation), "slave label update failure should be wrapped as ErrPartialReconciliation")
	ms.AssertExpectations(t)
	mr.AssertExpectations(t)
}

func TestPromoteBestReplicaMakeMasterFails(t *testing.T) {
	assert := assert.New(t)
	rf := generateRF()

	newMasterIP := "1.1.1.1"

	pods := &corev1.PodList{
		Items: []corev1.Pod{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "pod-master"},
				Status: corev1.PodStatus{
					PodIP: newMasterIP,
					Phase: corev1.PodRunning,
				},
			},
		},
	}

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	mr := &mRedisService.Client{}
	mr.On("MakeMaster", newMasterIP, "0", "").Once().Return(errors.New("promotion failed"))

	healer := rfservice.NewRedisFailoverHealer(ms, mr, log.DummyLogger{})

	err := healer.PromoteBestReplica(newMasterIP, rf)
	assert.Error(err, "MakeMaster failure should return an error")
	assert.False(errors.Is(err, rfservice.ErrPartialReconciliation), "full promotion failure should not be wrapped as ErrPartialReconciliation")
	ms.AssertExpectations(t)
	mr.AssertExpectations(t)
}

func TestPromoteBestReplicaRejectsIPNotOwnedByRF(t *testing.T) {
	assert := assert.New(t)
	rf := generateRF()

	// newMasterIP isn't a pod of this RedisFailover: refuse to promote it
	// rather than blindly issuing MakeMaster against a possibly-foreign
	// instance. See https://github.com/spotahome/redis-operator/issues/698.
	newMasterIP := "9.9.9.9"

	pods := &corev1.PodList{
		Items: []corev1.Pod{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "pod-replica"},
				Status: corev1.PodStatus{
					PodIP: "2.2.2.2",
					Phase: corev1.PodRunning,
				},
			},
		},
	}

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	mr := &mRedisService.Client{}

	healer := rfservice.NewRedisFailoverHealer(ms, mr, log.DummyLogger{})

	err := healer.PromoteBestReplica(newMasterIP, rf)
	assert.Error(err, "should refuse to promote an IP that isn't currently one of this RedisFailover's own pods")
	assert.False(errors.Is(err, rfservice.ErrPartialReconciliation))
	ms.AssertExpectations(t)
	mr.AssertExpectations(t) // no MakeMaster call should have happened
}

func TestNewSentinelMonitor(t *testing.T) {
	tests := []struct {
		name                string
		errorOnMonitorRedis bool
	}{
		{
			name: "updates provided IP to monitor new redis master",
		},
		{
			name:                "errors on failurer to set monitor",
			errorOnMonitorRedis: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			rf := generateRF()
			ms := &mK8SService.Services{}
			mr := &mRedisService.Client{}
			errorExpected := false

			if test.errorOnMonitorRedis {
				errorExpected = true
				mr.On("MonitorRedisWithPort", "0.0.0.0", "1.1.1.1", "0", "2", "").Once().Return(errors.New(""))
			} else {
				mr.On("MonitorRedisWithPort", "0.0.0.0", "1.1.1.1", "0", "2", "").Once().Return(nil)
			}

			healer := rfservice.NewRedisFailoverHealer(ms, mr, log.DummyLogger{})

			err := healer.NewSentinelMonitor("0.0.0.0", "1.1.1.1", rf)

			if errorExpected {
				assert.Error(err)
			} else {
				assert.NoError(err)
			}
			ms.AssertExpectations(t)
			mr.AssertExpectations(t)
		})
	}
}

func TestNewSentinelMonitorPasswordError(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()
	rf.Spec.Auth.SecretPath = "redis-secret"

	ms := &mK8SService.Services{}
	ms.On("GetSecret", namespace, "redis-secret").Once().Return(nil, errors.New("secret unavailable"))
	mr := &mRedisService.Client{}

	healer := rfservice.NewRedisFailoverHealer(ms, mr, log.DummyLogger{})

	err := healer.NewSentinelMonitor("0.0.0.0", "1.1.1.1", rf)
	assert.Error(err)
	mr.AssertExpectations(t) // no MonitorRedisWithPort call should have happened
}

func TestNewSentinelMonitorWithPortPasswordError(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()
	rf.Spec.Auth.SecretPath = "redis-secret"

	ms := &mK8SService.Services{}
	ms.On("GetSecret", namespace, "redis-secret").Once().Return(nil, errors.New("secret unavailable"))
	mr := &mRedisService.Client{}

	healer := rfservice.NewRedisFailoverHealer(ms, mr, log.DummyLogger{})

	err := healer.NewSentinelMonitorWithPort("0.0.0.0", "1.1.1.1", "6379", rf)
	assert.Error(err)
	mr.AssertExpectations(t) // no MonitorRedisWithPort call should have happened
}

// --- MakeMaster ---

func TestMakeMasterPasswordError(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()
	rf.Spec.Auth.SecretPath = "redis-secret"

	ms := &mK8SService.Services{}
	ms.On("GetSecret", namespace, "redis-secret").Once().Return(nil, errors.New("secret unavailable"))
	mr := &mRedisService.Client{}

	healer := rfservice.NewRedisFailoverHealer(ms, mr, log.DummyLogger{})

	err := healer.MakeMaster("1.1.1.1", rf)
	assert.Error(err)
	ms.AssertExpectations(t)
	mr.AssertExpectations(t) // no redis MakeMaster call should have happened
}

func TestMakeMasterRedisClientError(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	ms := &mK8SService.Services{}
	mr := &mRedisService.Client{}
	mr.On("MakeMaster", "1.1.1.1", "0", "").Once().Return(errors.New("redis unreachable"))

	healer := rfservice.NewRedisFailoverHealer(ms, mr, log.DummyLogger{})

	err := healer.MakeMaster("1.1.1.1", rf)
	assert.Error(err)
	ms.AssertExpectations(t) // no GetStatefulSetPods call should have happened
}

func TestMakeMasterGetStatefulSetPodsError(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(nil, errors.New("boom"))
	mr := &mRedisService.Client{}
	mr.On("MakeMaster", "1.1.1.1", "0", "").Once().Return(nil)

	healer := rfservice.NewRedisFailoverHealer(ms, mr, log.DummyLogger{})

	err := healer.MakeMaster("1.1.1.1", rf)
	assert.Error(err)
}

func TestMakeMasterSetsLabelOnMatchingPod(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	pods := &corev1.PodList{
		Items: []corev1.Pod{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "redis-0"},
				Status:     corev1.PodStatus{PodIP: "1.1.1.1"},
			},
			{
				ObjectMeta: metav1.ObjectMeta{Name: "redis-1"},
				Status:     corev1.PodStatus{PodIP: "2.2.2.2"},
			},
		},
	}

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	ms.On("UpdatePodLabels", namespace, "redis-0", mock.Anything).Once().Return(nil)
	mr := &mRedisService.Client{}
	mr.On("MakeMaster", "1.1.1.1", "0", "").Once().Return(nil)

	healer := rfservice.NewRedisFailoverHealer(ms, mr, log.DummyLogger{})

	err := healer.MakeMaster("1.1.1.1", rf)
	assert.NoError(err)
	ms.AssertExpectations(t) // label update must target the matching pod only
}

func TestMakeMasterSkipsLabelUpdateWhenAlreadyMaster(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	pods := &corev1.PodList{
		Items: []corev1.Pod{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "redis-0",
					Labels: map[string]string{"redisfailovers-role": "master"},
				},
				Status: corev1.PodStatus{PodIP: "1.1.1.1"},
			},
		},
	}

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	mr := &mRedisService.Client{}
	mr.On("MakeMaster", "1.1.1.1", "0", "").Once().Return(nil)

	healer := rfservice.NewRedisFailoverHealer(ms, mr, log.DummyLogger{})

	err := healer.MakeMaster("1.1.1.1", rf)
	assert.NoError(err)
	ms.AssertExpectations(t) // no UpdatePodLabels call should have happened, the pod is already labeled master
}

func TestMakeMasterLabelUpdateError(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	pods := &corev1.PodList{
		Items: []corev1.Pod{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "redis-0"},
				Status:     corev1.PodStatus{PodIP: "1.1.1.1"},
			},
		},
	}

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	ms.On("UpdatePodLabels", namespace, "redis-0", mock.Anything).Once().Return(errors.New("label update failed"))
	mr := &mRedisService.Client{}
	mr.On("MakeMaster", "1.1.1.1", "0", "").Once().Return(nil)

	healer := rfservice.NewRedisFailoverHealer(ms, mr, log.DummyLogger{})

	err := healer.MakeMaster("1.1.1.1", rf)
	assert.Error(err)
}

func TestMakeMasterNoMatchingPodReturnsNil(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	pods := &corev1.PodList{
		Items: []corev1.Pod{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "redis-0"},
				Status:     corev1.PodStatus{PodIP: "9.9.9.9"},
			},
		},
	}

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	mr := &mRedisService.Client{}
	mr.On("MakeMaster", "1.1.1.1", "0", "").Once().Return(nil)

	healer := rfservice.NewRedisFailoverHealer(ms, mr, log.DummyLogger{})

	err := healer.MakeMaster("1.1.1.1", rf)
	assert.NoError(err)
	ms.AssertExpectations(t) // no UpdatePodLabels call should have happened, no pod matched the ip
}

// --- RestoreSentinel ---

func TestRestoreSentinel(t *testing.T) {
	assert := assert.New(t)

	ms := &mK8SService.Services{}
	mr := &mRedisService.Client{}
	mr.On("ResetSentinel", "1.1.1.1").Once().Return(nil)

	healer := rfservice.NewRedisFailoverHealer(ms, mr, log.DummyLogger{})

	err := healer.RestoreSentinel("1.1.1.1")
	assert.NoError(err)
}

func TestRestoreSentinelError(t *testing.T) {
	assert := assert.New(t)

	ms := &mK8SService.Services{}
	mr := &mRedisService.Client{}
	mr.On("ResetSentinel", "1.1.1.1").Once().Return(errors.New("reset failed"))

	healer := rfservice.NewRedisFailoverHealer(ms, mr, log.DummyLogger{})

	err := healer.RestoreSentinel("1.1.1.1")
	assert.Error(err)
}

// --- SetSentinelCustomConfig ---

func TestSetSentinelCustomConfig(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()
	rf.Spec.Sentinel.CustomConfig = []string{"down-after-milliseconds 5000"}

	ms := &mK8SService.Services{}
	mr := &mRedisService.Client{}
	mr.On("SetCustomSentinelConfig", "1.1.1.1", []string{"down-after-milliseconds 5000"}).Once().Return(nil)

	healer := rfservice.NewRedisFailoverHealer(ms, mr, log.DummyLogger{})

	err := healer.SetSentinelCustomConfig("1.1.1.1", rf)
	assert.NoError(err)
}

func TestSetSentinelCustomConfigError(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()
	rf.Spec.Sentinel.CustomConfig = []string{"down-after-milliseconds 5000"}

	ms := &mK8SService.Services{}
	mr := &mRedisService.Client{}
	mr.On("SetCustomSentinelConfig", "1.1.1.1", []string{"down-after-milliseconds 5000"}).Once().Return(errors.New("set failed"))

	healer := rfservice.NewRedisFailoverHealer(ms, mr, log.DummyLogger{})

	err := healer.SetSentinelCustomConfig("1.1.1.1", rf)
	assert.Error(err)
}

// --- SetRedisCustomConfig ---

func TestSetRedisCustomConfigPasswordError(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()
	rf.Spec.Auth.SecretPath = "redis-secret"
	rf.Spec.Redis.CustomConfig = []string{"maxmemory 100mb"}

	ms := &mK8SService.Services{}
	ms.On("GetSecret", namespace, "redis-secret").Once().Return(nil, errors.New("secret unavailable"))
	mr := &mRedisService.Client{}

	healer := rfservice.NewRedisFailoverHealer(ms, mr, log.DummyLogger{})

	err := healer.SetRedisCustomConfig("1.1.1.1", rf)
	assert.Error(err)
	mr.AssertExpectations(t) // no SetCustomRedisConfig call should have happened
}

func TestSetRedisCustomConfig(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()
	rf.Spec.Redis.CustomConfig = []string{"maxmemory 100mb"}

	ms := &mK8SService.Services{}
	mr := &mRedisService.Client{}
	mr.On("SetCustomRedisConfig", "1.1.1.1", "0", []string{"maxmemory 100mb"}, "").Once().Return(nil)

	healer := rfservice.NewRedisFailoverHealer(ms, mr, log.DummyLogger{})

	err := healer.SetRedisCustomConfig("1.1.1.1", rf)
	assert.NoError(err)
}

func TestSetRedisCustomConfigError(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()
	rf.Spec.Redis.CustomConfig = []string{"maxmemory 100mb"}

	ms := &mK8SService.Services{}
	mr := &mRedisService.Client{}
	mr.On("SetCustomRedisConfig", "1.1.1.1", "0", []string{"maxmemory 100mb"}, "").Once().Return(errors.New("set failed"))

	healer := rfservice.NewRedisFailoverHealer(ms, mr, log.DummyLogger{})

	err := healer.SetRedisCustomConfig("1.1.1.1", rf)
	assert.Error(err)
}

// --- DeletePod ---

func TestDeletePod(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	ms := &mK8SService.Services{}
	ms.On("DeletePod", namespace, "redis-0").Once().Return(nil)
	mr := &mRedisService.Client{}

	healer := rfservice.NewRedisFailoverHealer(ms, mr, log.DummyLogger{})

	err := healer.DeletePod("redis-0", rf)
	assert.NoError(err)
}

func TestDeletePodError(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	ms := &mK8SService.Services{}
	ms.On("DeletePod", namespace, "redis-0").Once().Return(errors.New("delete failed"))
	mr := &mRedisService.Client{}

	healer := rfservice.NewRedisFailoverHealer(ms, mr, log.DummyLogger{})

	err := healer.DeletePod("redis-0", rf)
	assert.Error(err)
}

func TestNewSentinelMonitorWithPort(t *testing.T) {
	tests := []struct {
		name                string
		errorOnMonitorRedis bool
	}{
		{
			name: "updates provided IP to monitor new redis master",
		},
		{
			name:                "errors on failurer to set monitor",
			errorOnMonitorRedis: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			rf := generateRF()
			ms := &mK8SService.Services{}
			mr := &mRedisService.Client{}
			errorExpected := false

			if test.errorOnMonitorRedis {
				errorExpected = true
				mr.On("MonitorRedisWithPort", "0.0.0.0", "1.1.1.1", "6379", "2", "").Once().Return(errors.New(""))
			} else {
				mr.On("MonitorRedisWithPort", "0.0.0.0", "1.1.1.1", "6379", "2", "").Once().Return(nil)
			}

			healer := rfservice.NewRedisFailoverHealer(ms, mr, log.DummyLogger{})

			err := healer.NewSentinelMonitorWithPort("0.0.0.0", "1.1.1.1", "6379", rf)

			if errorExpected {
				assert.Error(err)
			} else {
				assert.NoError(err)
			}
			ms.AssertExpectations(t)
			mr.AssertExpectations(t)
		})
	}
}
