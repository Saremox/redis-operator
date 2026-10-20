package service_test

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/log"
	mK8SService "github.com/saremox/redis-operator/mocks/service/k8s"
	mRedisService "github.com/saremox/redis-operator/mocks/service/redis"
	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
)

func TestSetOldestAsMasterNewMasterError(t *testing.T) {
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
	mr := &mRedisService.Client{}
	mr.On("MakeMaster", "0.0.0.0", "0", "").Once().Return(errors.New(""))

	healer := rfservice.NewRedisFailoverHealer(ms, mr, log.DummyLogger{})

	master, err := healer.SetOldestAsMaster(rf)
	require.Error(t, err)
	assert.Empty(t, master)
	ms.AssertExpectations(t)
	mr.AssertExpectations(t)
}

func TestSetOldestAsMaster(t *testing.T) {
	rf := generateRF()

	pods := &corev1.PodList{
		Items: []corev1.Pod{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "pod-0"},
				Status: corev1.PodStatus{
					PodIP: "0.0.0.0",
				},
			},
		},
	}

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	ms.On("UpdatePodLabels", namespace, "pod-0", masterRoleLabel).Once().Return(nil)
	mr := &mRedisService.Client{}
	mr.On("MakeMaster", "0.0.0.0", "0", "").Once().Return(nil)

	healer := rfservice.NewRedisFailoverHealer(ms, mr, log.DummyLogger{})

	master, err := healer.SetOldestAsMaster(rf)
	require.NoError(t, err)
	assert.Equal(t, "0.0.0.0", master)
	ms.AssertExpectations(t)
	mr.AssertExpectations(t)
}

func TestSetOldestAsMasterMultiplePodsMakeSlaveOfError(t *testing.T) {
	rf := generateRF()

	pods := &corev1.PodList{
		Items: []corev1.Pod{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "pod-0"},
				Status: corev1.PodStatus{
					PodIP: "0.0.0.0",
				},
			},
			{
				ObjectMeta: metav1.ObjectMeta{Name: "pod-1"},
				Status: corev1.PodStatus{
					PodIP: "1.1.1.1",
				},
			},
		},
	}

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	ms.On("UpdatePodLabels", namespace, "pod-0", masterRoleLabel).Once().Return(nil)
	ms.On("UpdatePodLabels", namespace, "pod-1", slaveRoleLabel).Once().Return(nil)
	mr := &mRedisService.Client{}
	mr.On("MakeMaster", "0.0.0.0", "0", "").Once().Return(nil)
	mr.On("MakeSlaveOfWithPort", "1.1.1.1", "0", "0.0.0.0", "0", "").Once().Return(errors.New(""))

	healer := rfservice.NewRedisFailoverHealer(ms, mr, log.DummyLogger{})

	master, err := healer.SetOldestAsMaster(rf)
	require.NoError(t, err)
	assert.Equal(t, "0.0.0.0", master)
	ms.AssertExpectations(t)
	mr.AssertExpectations(t)
}

func TestSetOldestAsMasterMultiplePods(t *testing.T) {
	rf := generateRF()

	pods := &corev1.PodList{
		Items: []corev1.Pod{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "pod-0"},
				Status: corev1.PodStatus{
					PodIP: "0.0.0.0",
				},
			},
			{
				ObjectMeta: metav1.ObjectMeta{Name: "pod-1"},
				Status: corev1.PodStatus{
					PodIP: "1.1.1.1",
				},
			},
		},
	}

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	ms.On("UpdatePodLabels", namespace, "pod-0", masterRoleLabel).Once().Return(nil)
	ms.On("UpdatePodLabels", namespace, "pod-1", slaveRoleLabel).Once().Return(nil)
	mr := &mRedisService.Client{}
	mr.On("MakeMaster", "0.0.0.0", "0", "").Once().Return(nil)
	mr.On("MakeSlaveOfWithPort", "1.1.1.1", "0", "0.0.0.0", "0", "").Once().Return(nil)

	healer := rfservice.NewRedisFailoverHealer(ms, mr, log.DummyLogger{})

	master, err := healer.SetOldestAsMaster(rf)
	require.NoError(t, err)
	assert.Equal(t, "0.0.0.0", master)
	ms.AssertExpectations(t)
	mr.AssertExpectations(t)
}

func TestSetOldestAsMasterOrdering(t *testing.T) {
	rf := generateRF()

	pods := &corev1.PodList{
		Items: []corev1.Pod{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name: "pod-0",
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
					Name: "pod-1",
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
	ms.On("UpdatePodLabels", namespace, "pod-1", masterRoleLabel).Once().Return(nil)
	ms.On("UpdatePodLabels", namespace, "pod-0", slaveRoleLabel).Once().Return(nil)
	mr := &mRedisService.Client{}
	mr.On("MakeMaster", "1.1.1.1", "0", "").Once().Return(nil)
	mr.On("MakeSlaveOfWithPort", "0.0.0.0", "0", "1.1.1.1", "0", "").Once().Return(nil)

	healer := rfservice.NewRedisFailoverHealer(ms, mr, log.DummyLogger{})

	master, err := healer.SetOldestAsMaster(rf)
	require.NoError(t, err)
	assert.Equal(t, "1.1.1.1", master)
	ms.AssertExpectations(t)
	mr.AssertExpectations(t)
}

func TestSetOldestAsMasterCandidates(t *testing.T) {
	now := time.Now()
	pod := func(ip string, age time.Duration, ready, terminating bool) corev1.Pod {
		p := corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:              ip,
				CreationTimestamp: metav1.NewTime(now.Add(-age)),
			},
			Status: corev1.PodStatus{PodIP: ip},
		}
		if ready {
			p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		} else {
			p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}
		}
		if terminating {
			p.DeletionTimestamp = &metav1.Time{Time: now}
		}
		return p
	}

	master := func(p corev1.Pod) corev1.Pod {
		p.Labels = map[string]string{"redisfailovers-role": "master"}
		return p
	}

	tests := []struct {
		name       string
		pods       []corev1.Pod
		wantMaster string
		wantSlaves []string
		wantErr    bool
	}{
		{
			name: "ready terminating master blocks the election",
			pods: []corev1.Pod{
				pod("0.0.0.0", 3*time.Hour, true, false),
				master(pod("1.1.1.1", 2*time.Hour, true, true)),
				pod("2.2.2.2", time.Hour, true, false),
			},
		},
		{
			name: "not ready terminating master does not block the election",
			pods: []corev1.Pod{
				master(pod("0.0.0.0", 3*time.Hour, false, true)),
				pod("1.1.1.1", 2*time.Hour, true, false),
				pod("2.2.2.2", time.Hour, true, false),
			},
			wantMaster: "1.1.1.1",
			wantSlaves: []string{"2.2.2.2"},
		},
		{
			name: "terminating oldest pod is skipped",
			pods: []corev1.Pod{
				pod("0.0.0.0", 3*time.Hour, true, true),
				pod("1.1.1.1", 2*time.Hour, true, false),
				pod("2.2.2.2", time.Hour, true, false),
			},
			wantMaster: "1.1.1.1",
			wantSlaves: []string{"2.2.2.2"},
		},
		{
			name: "not ready oldest pod loses to younger ready pod",
			pods: []corev1.Pod{
				pod("0.0.0.0", 3*time.Hour, false, false),
				pod("1.1.1.1", time.Hour, true, false),
			},
			wantMaster: "1.1.1.1",
			wantSlaves: []string{"0.0.0.0"},
		},
		{
			name: "oldest ready pod wins among ready pods",
			pods: []corev1.Pod{
				pod("0.0.0.0", time.Hour, true, false),
				pod("1.1.1.1", 3*time.Hour, true, false),
				pod("2.2.2.2", 2*time.Hour, true, false),
			},
			wantMaster: "1.1.1.1",
			wantSlaves: []string{"2.2.2.2", "0.0.0.0"},
		},
		{
			name: "no ready pod falls back to oldest non-terminating pod",
			pods: []corev1.Pod{
				pod("0.0.0.0", 3*time.Hour, false, true),
				pod("1.1.1.1", time.Hour, false, false),
				pod("2.2.2.2", 2*time.Hour, false, false),
			},
			wantMaster: "2.2.2.2",
			wantSlaves: []string{"1.1.1.1"},
		},
		{
			name: "all pods terminating",
			pods: []corev1.Pod{
				pod("0.0.0.0", 2*time.Hour, true, true),
				pod("1.1.1.1", time.Hour, true, true),
			},
			wantErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)

			rf := generateRF()
			ms := &mK8SService.Services{}
			ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(&corev1.PodList{Items: test.pods}, nil)
			if test.wantMaster != "" {
				ms.On("UpdatePodLabels", namespace, test.wantMaster, masterRoleLabel).Once().Return(nil)
			}
			for _, ip := range test.wantSlaves {
				ms.On("UpdatePodLabels", namespace, ip, slaveRoleLabel).Once().Return(nil)
			}
			mr := &mRedisService.Client{}
			mr.On("MakeMaster", mock.Anything, "0", "").Return(nil)
			mr.On("MakeSlaveOfWithPort", mock.Anything, "0", mock.Anything, "0", "").Return(nil)

			healer := rfservice.NewRedisFailoverHealer(ms, mr, log.DummyLogger{})
			master, err := healer.SetOldestAsMaster(rf)
			assert.Equal(test.wantMaster, master)

			if test.wantMaster == "" {
				assert.Equal(test.wantErr, err != nil)
				mr.AssertNotCalled(t, "MakeMaster", mock.Anything, mock.Anything, mock.Anything)
				mr.AssertNotCalled(t, "MakeSlaveOfWithPort", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
				return
			}
			require.NoError(t, err)
			mr.AssertNumberOfCalls(t, "MakeMaster", 1)
			mr.AssertCalled(t, "MakeMaster", test.wantMaster, "0", "")
			mr.AssertNumberOfCalls(t, "MakeSlaveOfWithPort", len(test.wantSlaves))
			for _, ip := range test.wantSlaves {
				mr.AssertCalled(t, "MakeSlaveOfWithPort", ip, "0", test.wantMaster, "0", "")
			}
			ms.AssertExpectations(t)
		})
	}
}

func TestSetOldestAsMasterErrors(t *testing.T) {
	pods := &corev1.PodList{
		Items: []corev1.Pod{
			{ObjectMeta: metav1.ObjectMeta{Name: "master"}, Status: corev1.PodStatus{PodIP: "0.0.0.0"}},
			{ObjectMeta: metav1.ObjectMeta{Name: "slave"}, Status: corev1.PodStatus{PodIP: "1.1.1.1"}},
		},
	}

	tests := []struct {
		name  string
		setup func(rf *redisfailoverv1.RedisFailover, ms *mK8SService.Services)
	}{
		{
			name: "list pods fails",
			setup: func(rf *redisfailoverv1.RedisFailover, ms *mK8SService.Services) {
				ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(nil, errors.New(""))
			},
		},
		{
			name: "get password fails",
			setup: func(rf *redisfailoverv1.RedisFailover, ms *mK8SService.Services) {
				rf.Spec.Auth.SecretPath = "secret"
				ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
				ms.On("GetSecret", namespace, "secret").Once().Return(nil, errors.New(""))
			},
		},
		{
			name: "master label fails",
			setup: func(rf *redisfailoverv1.RedisFailover, ms *mK8SService.Services) {
				ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
				ms.On("UpdatePodLabels", namespace, "master", masterRoleLabel).Once().Return(errors.New(""))
			},
		},
		{
			name: "slave label fails",
			setup: func(rf *redisfailoverv1.RedisFailover, ms *mK8SService.Services) {
				ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
				ms.On("UpdatePodLabels", namespace, "master", masterRoleLabel).Once().Return(nil)
				ms.On("UpdatePodLabels", namespace, "slave", slaveRoleLabel).Once().Return(errors.New(""))
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rf := generateRF()
			ms := &mK8SService.Services{}
			test.setup(rf, ms)
			mr := &mRedisService.Client{}
			mr.On("MakeMaster", "0.0.0.0", "0", "").Return(nil)
			mr.On("MakeSlaveOfWithPort", "1.1.1.1", "0", "0.0.0.0", "0", "").Return(nil)

			healer := rfservice.NewRedisFailoverHealer(ms, mr, log.DummyLogger{})
			master, err := healer.SetOldestAsMaster(rf)
			require.Error(t, err)
			assert.Empty(t, master)
			ms.AssertExpectations(t)
		})
	}
}

func TestSetMasterOnAllMakeMasterError(t *testing.T) {
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
	mr := &mRedisService.Client{}
	mr.On("IsMaster", "0.0.0.0", "0", "").Return(false, errors.New(""))
	healer := rfservice.NewRedisFailoverHealer(ms, mr, log.DummyLogger{})

	err := healer.SetMasterOnAll("0.0.0.0", rf)
	require.Error(t, err)
	ms.AssertExpectations(t)
	mr.AssertExpectations(t)
}

// A slave that cannot be reached (MakeSlaveOf fails) is skipped rather than
// aborting the heal, so the reachable slaves are still repointed at the new
// master. The unreachable pod re-syncs via sentinel once its node recovers.
// Part of the #674 fix.
// TestSetMasterOnAllReturnsErrorWhenNoLongerMaster covers the case where
// IsMaster succeeds (err == nil) but reports the IP is no longer the master
// - e.g. sentinel or the operator made a switch mid-loop. This must be
// treated as a genuine failure of this reconfiguration round: previously,
// `return err` here returned the nil err from IsMaster's success return
// value, so the caller (CheckAndHeal / checkAndHealOperatorManagedMode) saw
// nil and treated an aborted, partially-reconfigured pod loop as complete
// success.
func TestSetMasterOnAllReturnsErrorWhenNoLongerMaster(t *testing.T) {
	rf := generateRF()

	pods := &corev1.PodList{
		Items: []corev1.Pod{
			{Status: corev1.PodStatus{PodIP: "0.0.0.0"}},
			{Status: corev1.PodStatus{PodIP: "1.1.1.1"}},
		},
	}

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	mr := &mRedisService.Client{}
	mr.On("IsMaster", "0.0.0.0", "0", "").Once().Return(false, nil)

	healer := rfservice.NewRedisFailoverHealer(ms, mr, log.DummyLogger{})

	err := healer.SetMasterOnAll("0.0.0.0", rf)
	require.Error(t, err, "must not report success when the IP is no longer the master")
	mr.AssertExpectations(t) // no MakeSlaveOfWithPort call should have happened
}

func TestSetMasterOnAllMakeSlaveOfErrorIsSkipped(t *testing.T) {
	rf := generateRF()

	pods := &corev1.PodList{
		Items: []corev1.Pod{
			{
				Status: corev1.PodStatus{
					PodIP: "0.0.0.0", // new master
				},
			},
			{
				Status: corev1.PodStatus{
					PodIP: "1.1.1.1", // unreachable old master
				},
			},
			{
				ObjectMeta: metav1.ObjectMeta{Name: "pod-2"},
				Status: corev1.PodStatus{
					PodIP: "2.2.2.2", // reachable slave
				},
			},
		},
	}

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	ms.On("UpdatePodLabels", namespace, "pod-2", slaveRoleLabel).Once().Return(nil)
	mr := &mRedisService.Client{}
	mr.On("IsMaster", "0.0.0.0", "0", "").Return(true, nil)
	mr.On("MakeSlaveOfWithPort", "1.1.1.1", "0", "0.0.0.0", "0", "").Once().Return(errors.New("i/o timeout"))
	// The reachable slave must still be repointed even though the previous pod failed.
	mr.On("MakeSlaveOfWithPort", "2.2.2.2", "0", "0.0.0.0", "0", "").Once().Return(nil)

	healer := rfservice.NewRedisFailoverHealer(ms, mr, log.DummyLogger{})

	err := healer.SetMasterOnAll("0.0.0.0", rf)
	require.NoError(t, err)
	ms.AssertExpectations(t)
	mr.AssertExpectations(t) // proves the reachable slave was still reconfigured
}

func TestSetMasterOnAll(t *testing.T) {
	rf := generateRF()

	pods := &corev1.PodList{
		Items: []corev1.Pod{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "pod-0"},
				Status: corev1.PodStatus{
					PodIP: "0.0.0.0",
				},
			},
			{
				ObjectMeta: metav1.ObjectMeta{Name: "pod-1"},
				Status: corev1.PodStatus{
					PodIP: "1.1.1.1",
				},
			},
		},
	}

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	ms.On("UpdatePodLabels", namespace, "pod-1", slaveRoleLabel).Once().Return(nil)
	mr := &mRedisService.Client{}
	mr.On("IsMaster", "0.0.0.0", "0", "").Return(true, nil)
	mr.On("MakeSlaveOfWithPort", "1.1.1.1", "0", "0.0.0.0", "0", "").Once().Return(nil)

	healer := rfservice.NewRedisFailoverHealer(ms, mr, log.DummyLogger{})

	err := healer.SetMasterOnAll("0.0.0.0", rf)
	require.NoError(t, err)
	ms.AssertExpectations(t)
	mr.AssertExpectations(t)
}

func TestSetMasterOnAllRejectsIPNotOwnedByRF(t *testing.T) {
	rf := generateRF()

	// None of this RedisFailover's own pods have this IP: it could be a stale
	// value resolved earlier in the reconcile that has since been reassigned to
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
	require.Error(t, err, "should refuse to act on an IP that isn't currently one of this RedisFailover's own pods")
	ms.AssertExpectations(t)
	mr.AssertExpectations(t) // no IsMaster/MakeSlaveOfWithPort calls should have happened
}

func TestSetMasterOnAllSlaveAlreadyLabeled(t *testing.T) {
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
	require.NoError(t, err)
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
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			ms.AssertExpectations(t)
			mr.AssertExpectations(t)
		})
	}
}

func TestPromoteBestReplicaSuccess(t *testing.T) {
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
	ms.On("UpdatePodLabels", namespace, "pod-master", masterRoleLabel).Once().Return(nil)
	ms.On("UpdatePodLabels", namespace, "pod-replica", slaveRoleLabel).Once().Return(nil)

	mr := &mRedisService.Client{}
	mr.On("MakeMaster", newMasterIP, "0", "").Once().Return(nil)
	mr.On("MakeSlaveOfWithPort", replicaIP, "0", newMasterIP, "0", "").Once().Return(nil)

	healer := rfservice.NewRedisFailoverHealer(ms, mr, log.DummyLogger{})

	err := healer.PromoteBestReplica(newMasterIP, rf)
	require.NoError(t, err)
	ms.AssertExpectations(t)
	mr.AssertExpectations(t)
}

// A failover usually runs while the old master pod terminates, or while its
// replacement is Pending without an IP. A replica command to such a pod fails
// and marks the failover as partial.
func TestPromoteBestReplicaSkipsPendingAndTerminatingPods(t *testing.T) {
	rf := generateRF()

	newMasterIP := "1.1.1.1"

	terminating := podWithRole("pod-old-master", "3.3.3.3", masterRoleLabel)
	terminating.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	pending := podWithRole("pod-pending", "", nil)
	pending.Status.Phase = corev1.PodPending
	pods := &corev1.PodList{
		Items: []corev1.Pod{
			podWithRole("pod-master", newMasterIP, slaveRoleLabel),
			terminating,
			pending,
		},
	}

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	ms.On("UpdatePodLabels", namespace, "pod-master", masterRoleLabel).Once().Return(nil)

	mr := &mRedisService.Client{}
	mr.On("MakeMaster", newMasterIP, "0", "").Once().Return(nil)

	healer := rfservice.NewRedisFailoverHealer(ms, mr, log.DummyLogger{})

	err := healer.PromoteBestReplica(newMasterIP, rf)
	require.NoError(t, err)
	ms.AssertExpectations(t)
	mr.AssertExpectations(t) // no MakeSlaveOfWithPort call to the terminating or the Pending pod
}

func TestPromoteBestReplicaReplicaRepointerFails(t *testing.T) {
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
	ms.On("UpdatePodLabels", namespace, "pod-master", masterRoleLabel).Once().Return(nil)

	mr := &mRedisService.Client{}
	mr.On("MakeMaster", newMasterIP, "0", "").Once().Return(nil)
	mr.On("MakeSlaveOfWithPort", replicaIP, "0", newMasterIP, "0", "").Once().Return(errors.New("replica repoint failed"))

	healer := rfservice.NewRedisFailoverHealer(ms, mr, log.DummyLogger{})

	err := healer.PromoteBestReplica(newMasterIP, rf)
	require.Error(t, err, "partial failover should not be reported as success")
	require.ErrorIs(t, err, rfservice.ErrPartialReconciliation, "replica repoint failure should be wrapped as ErrPartialReconciliation")
	ms.AssertExpectations(t)
	mr.AssertExpectations(t)
}

func TestPromoteBestReplicaRelabelsAnOldMasterThatDoesNotAnswer(t *testing.T) {
	t.Run("the relabel succeeds", func(t *testing.T) { testPromoteBestReplicaRelabelsOldMaster(t, nil) })
	t.Run("the relabel fails", func(t *testing.T) { testPromoteBestReplicaRelabelsOldMaster(t, errors.New("relabel failed")) })
}

func testPromoteBestReplicaRelabelsOldMaster(t *testing.T, labelErr error) {
	t.Helper()
	rf := generateRF()

	newMasterIP := "1.1.1.1"
	oldMasterIP := "2.2.2.2"

	pods := &corev1.PodList{
		Items: []corev1.Pod{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "pod-new-master"},
				Status:     corev1.PodStatus{PodIP: newMasterIP, Phase: corev1.PodRunning},
			},
			{
				ObjectMeta: metav1.ObjectMeta{Name: "pod-old-master", Labels: map[string]string{"redisfailovers-role": "master"}},
				Status:     corev1.PodStatus{PodIP: oldMasterIP, Phase: corev1.PodRunning},
			},
		},
	}

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	ms.On("UpdatePodLabels", namespace, "pod-new-master", map[string]string{"redisfailovers-role": "master"}).Once().Return(nil)
	ms.On("UpdatePodLabels", namespace, "pod-old-master", map[string]string{"redisfailovers-role": "slave"}).Once().Return(labelErr)

	mr := &mRedisService.Client{}
	mr.On("MakeMaster", newMasterIP, "0", "").Once().Return(nil)
	mr.On("MakeSlaveOfWithPort", oldMasterIP, "0", newMasterIP, "0", "").Once().Return(errors.New("i/o timeout"))

	healer := rfservice.NewRedisFailoverHealer(ms, mr, log.DummyLogger{})

	err := healer.PromoteBestReplica(newMasterIP, rf)
	require.ErrorIs(t, err, rfservice.ErrPartialReconciliation)
	if labelErr != nil {
		require.ErrorIs(t, err, labelErr)
	}
	ms.AssertExpectations(t)
	mr.AssertExpectations(t)
}

func TestPromoteBestReplicaLabelUpdateFails(t *testing.T) {
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
	ms.On("UpdatePodLabels", namespace, "pod-master", masterRoleLabel).Once().Return(nil)
	ms.On("UpdatePodLabels", namespace, "pod-replica", slaveRoleLabel).Once().Return(errors.New("label update failed"))

	mr := &mRedisService.Client{}
	mr.On("MakeMaster", newMasterIP, "0", "").Once().Return(nil)
	mr.On("MakeSlaveOfWithPort", replicaIP, "0", newMasterIP, "0", "").Once().Return(nil)

	healer := rfservice.NewRedisFailoverHealer(ms, mr, log.DummyLogger{})

	err := healer.PromoteBestReplica(newMasterIP, rf)
	require.Error(t, err, "label update failure should not be reported as success")
	require.ErrorIs(t, err, rfservice.ErrPartialReconciliation, "slave label update failure should be wrapped as ErrPartialReconciliation")
	ms.AssertExpectations(t)
	mr.AssertExpectations(t)
}

func TestPromoteBestReplicaMakeMasterFails(t *testing.T) {
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
	require.Error(t, err, "MakeMaster failure should return an error")
	require.NotErrorIs(t, err, rfservice.ErrPartialReconciliation, "full promotion failure should not be wrapped as ErrPartialReconciliation")
	require.NotErrorIs(t, err, rfservice.ErrMasterLabel)
	ms.AssertExpectations(t)
	mr.AssertExpectations(t)
}

func TestPromoteBestReplicaRejectsIPNotOwnedByRF(t *testing.T) {
	rf := generateRF()

	// newMasterIP is not a pod of this RedisFailover: refuse to promote it
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
	require.Error(t, err, "should refuse to promote an IP that isn't currently one of this RedisFailover's own pods")
	require.NotErrorIs(t, err, rfservice.ErrPartialReconciliation)
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
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			ms.AssertExpectations(t)
			mr.AssertExpectations(t)
		})
	}
}

func TestNewSentinelMonitorPasswordError(t *testing.T) {
	rf := generateRF()
	rf.Spec.Auth.SecretPath = "redis-secret"

	ms := &mK8SService.Services{}
	ms.On("GetSecret", namespace, "redis-secret").Once().Return(nil, errors.New("secret unavailable"))
	mr := &mRedisService.Client{}

	healer := rfservice.NewRedisFailoverHealer(ms, mr, log.DummyLogger{})

	err := healer.NewSentinelMonitor("0.0.0.0", "1.1.1.1", rf)
	require.Error(t, err)
	mr.AssertExpectations(t) // no MonitorRedisWithPort call should have happened
}

func TestNewSentinelMonitorWithPortPasswordError(t *testing.T) {
	rf := generateRF()
	rf.Spec.Auth.SecretPath = "redis-secret"

	ms := &mK8SService.Services{}
	ms.On("GetSecret", namespace, "redis-secret").Once().Return(nil, errors.New("secret unavailable"))
	mr := &mRedisService.Client{}

	healer := rfservice.NewRedisFailoverHealer(ms, mr, log.DummyLogger{})

	err := healer.NewSentinelMonitorWithPort("0.0.0.0", "1.1.1.1", "6379", rf)
	require.Error(t, err)
	mr.AssertExpectations(t) // no MonitorRedisWithPort call should have happened
}

// --- MakeMaster ---

func TestMakeMasterPasswordError(t *testing.T) {
	rf := generateRF()
	rf.Spec.Auth.SecretPath = "redis-secret"

	ms := &mK8SService.Services{}
	ms.On("GetSecret", namespace, "redis-secret").Once().Return(nil, errors.New("secret unavailable"))
	mr := &mRedisService.Client{}

	healer := rfservice.NewRedisFailoverHealer(ms, mr, log.DummyLogger{})

	err := healer.MakeMaster("1.1.1.1", rf)
	require.Error(t, err)
	ms.AssertExpectations(t)
	mr.AssertExpectations(t) // no redis MakeMaster call should have happened
}

func TestMakeMasterRedisClientError(t *testing.T) {
	rf := generateRF()

	ms := &mK8SService.Services{}
	mr := &mRedisService.Client{}
	mr.On("MakeMaster", "1.1.1.1", "0", "").Once().Return(errors.New("redis unreachable"))

	healer := rfservice.NewRedisFailoverHealer(ms, mr, log.DummyLogger{})

	err := healer.MakeMaster("1.1.1.1", rf)
	require.Error(t, err)
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
	require.NoError(t, err)
	ms.AssertExpectations(t) // label update must target the matching pod only
}

func TestMakeMasterSkipsLabelUpdateWhenAlreadyMaster(t *testing.T) {
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
	require.NoError(t, err)
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
	require.NoError(t, err)
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
	rf := generateRF()
	rf.Spec.Auth.SecretPath = "redis-secret"
	rf.Spec.Redis.CustomConfig = []string{"maxmemory 100mb"}

	ms := &mK8SService.Services{}
	ms.On("GetSecret", namespace, "redis-secret").Once().Return(nil, errors.New("secret unavailable"))
	mr := &mRedisService.Client{}

	healer := rfservice.NewRedisFailoverHealer(ms, mr, log.DummyLogger{})

	err := healer.SetRedisCustomConfig("1.1.1.1", rf)
	require.Error(t, err)
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
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			ms.AssertExpectations(t)
			mr.AssertExpectations(t)
		})
	}
}

// The healer returns the error of a failed read before it changes a Redis.
func TestHealerReadErrors(t *testing.T) {
	readErr := errors.New("read failed")
	pods := &corev1.PodList{Items: []corev1.Pod{podWithRole("pod-a", "1.1.1.1", nil)}}
	tests := []struct {
		name       string
		secretPath bool
		podsErr    bool
		call       func(*rfservice.RedisFailoverHealer, *redisfailoverv1.RedisFailover) error
	}{
		{"PromoteBestReplica, no password", true, false, func(h *rfservice.RedisFailoverHealer, rf *redisfailoverv1.RedisFailover) error {
			return h.PromoteBestReplica("1.1.1.1", rf)
		}},
		{"PromoteBestReplica, no pods", false, true, func(h *rfservice.RedisFailoverHealer, rf *redisfailoverv1.RedisFailover) error {
			return h.PromoteBestReplica("1.1.1.1", rf)
		}},
		{"SetMasterOnAll, no pods", false, true, func(h *rfservice.RedisFailoverHealer, rf *redisfailoverv1.RedisFailover) error {
			return h.SetMasterOnAll("1.1.1.1", rf)
		}},
		{"SetMasterOnAll, no password", true, false, func(h *rfservice.RedisFailoverHealer, rf *redisfailoverv1.RedisFailover) error {
			return h.SetMasterOnAll("1.1.1.1", rf)
		}},
		{"SetExternalMasterOnAll, no password", true, false, func(h *rfservice.RedisFailoverHealer, rf *redisfailoverv1.RedisFailover) error {
			return h.SetExternalMasterOnAll("5.5.5.5", "6379", rf)
		}},
		{"SetOldestAsMaster, no password", true, false, func(h *rfservice.RedisFailoverHealer, rf *redisfailoverv1.RedisFailover) error {
			_, err := h.SetOldestAsMaster(rf)
			return err
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rf := generateRF()
			ms := &mK8SService.Services{}
			switch {
			case test.podsErr:
				ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(nil, readErr)
			case test.name != "PromoteBestReplica, no password":
				// PromoteBestReplica reads the password first.
				ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
			}
			if test.secretPath {
				rf.Spec.Auth.SecretPath = "redis-secret"
				ms.On("GetSecret", namespace, "redis-secret").Once().Return(nil, readErr)
			}
			mr := &mRedisService.Client{}

			err := test.call(rfservice.NewRedisFailoverHealer(ms, mr, log.DummyLogger{}), rf)
			require.ErrorIs(t, err, readErr)
			ms.AssertExpectations(t)
			mr.AssertExpectations(t) // no call to a Redis
		})
	}
}

// A failed master label stops PromoteBestReplica before it changes a replica.
func TestPromoteBestReplicaMasterLabelFails(t *testing.T) {
	rf := generateRF()
	labelErr := errors.New("label failed")
	pods := &corev1.PodList{Items: []corev1.Pod{
		podWithRole("pod-master", "1.1.1.1", slaveRoleLabel),
		podWithRole("pod-replica", "2.2.2.2", slaveRoleLabel),
	}}

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	ms.On("UpdatePodLabels", namespace, "pod-master", masterRoleLabel).Once().Return(labelErr)
	mr := &mRedisService.Client{}
	mr.On("MakeMaster", "1.1.1.1", "0", "").Once().Return(nil)

	err := rfservice.NewRedisFailoverHealer(ms, mr, log.DummyLogger{}).PromoteBestReplica("1.1.1.1", rf)
	require.ErrorIs(t, err, labelErr)
	require.ErrorIs(t, err, rfservice.ErrMasterLabel)
	require.NotErrorIs(t, err, rfservice.ErrPartialReconciliation)
	ms.AssertExpectations(t)
	mr.AssertExpectations(t) // no MakeSlaveOfWithPort call
}
