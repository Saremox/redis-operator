package service_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"

	"github.com/stretchr/testify/assert"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

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

	ss := &appsv1.StatefulSet{
		Spec: appsv1.StatefulSetSpec{
			Replicas: ptr.To(int32(4)),
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

	ss := &appsv1.StatefulSet{
		Spec: appsv1.StatefulSetSpec{
			Replicas: ptr.To(int32(3)),
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

	ss := &appsv1.Deployment{
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(4)),
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

	ss := &appsv1.Deployment{
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(3)),
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

// An unreachable pod (GetSlaveOf fails) must be skipped, not abort the check:
// its label was already applied and there is nothing more the operator can do
// for it. This is the core of the #674 fix - a downed node's stale master pod
// used to stop the whole heal.
func TestCheckAllSlavesFromMasterGetSlaveOfErrorIsSkipped(t *testing.T) {
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
	assert.NoError(err)
}

// The #674 scenario: master is reachable, the old master pod on the downed node
// is not. The new master must still get its master-role label (so the master
// Service follows it) and the unreachable pod must not turn into an error.
func TestCheckAllSlavesFromMasterLabelsMasterDespiteUnreachablePod(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	pods := &corev1.PodList{
		Items: []corev1.Pod{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "rfr-test-0"},
				Status:     corev1.PodStatus{PodIP: "10.0.0.1", Phase: corev1.PodRunning},
			},
			{
				ObjectMeta: metav1.ObjectMeta{Name: "rfr-test-1"},
				Status:     corev1.PodStatus{PodIP: "10.0.0.2", Phase: corev1.PodRunning},
			},
		},
	}

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	// The master pod must be labelled master; assert on that specific call.
	ms.On("UpdatePodLabels", namespace, "rfr-test-0", map[string]string{"redisfailovers-role": "master"}).Once().Return(nil)
	// The unreachable pod is still labelled slave before its GetSlaveOf fails.
	ms.On("UpdatePodLabels", namespace, "rfr-test-1", map[string]string{"redisfailovers-role": "slave"}).Once().Return(nil)
	mr := &mRedisService.Client{}
	mr.On("GetSlaveOf", "10.0.0.1", "0", "").Once().Return("", nil)                       // master, reachable
	mr.On("GetSlaveOf", "10.0.0.2", "0", "").Once().Return("", errors.New("i/o timeout")) // old master, unreachable

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	err := checker.CheckAllSlavesFromMaster("10.0.0.1", rf)
	assert.NoError(err)
	ms.AssertExpectations(t) // proves the master-role label was applied
}

func TestCheckAllSlavesFromMasterDoesNotDialTerminatingPods(t *testing.T) {
	rf := generateRF()
	pods := &corev1.PodList{
		Items: []corev1.Pod{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "rfr-test-0", Labels: map[string]string{"redisfailovers-role": "master"}},
				Status:     corev1.PodStatus{PodIP: "10.0.0.1", Phase: corev1.PodRunning},
			},
			{
				ObjectMeta: metav1.ObjectMeta{Name: "rfr-test-1", DeletionTimestamp: &metav1.Time{Time: time.Now()}},
				Status:     corev1.PodStatus{PodIP: "10.0.0.2", Phase: corev1.PodRunning},
			},
		},
	}

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	ms.On("UpdatePodLabels", namespace, "rfr-test-1", map[string]string{"redisfailovers-role": "slave"}).Once().Return(nil)
	mr := &mRedisService.Client{}
	mr.On("GetSlaveOf", "10.0.0.1", "0", "").Once().Return("", nil)

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	assert.NoError(t, checker.CheckAllSlavesFromMaster("10.0.0.1", rf))
	ms.AssertExpectations(t)
	mr.AssertExpectations(t)
}

func TestIsMasterPod(t *testing.T) {
	assert.True(t, rfservice.IsMasterPod(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"redisfailovers-role": "master"}}}))
	assert.False(t, rfservice.IsMasterPod(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"redisfailovers-role": "slave"}}}))
	assert.False(t, rfservice.IsMasterPod(&corev1.Pod{}))
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
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().
		Return(podsWithPhases(corev1.PodRunning, corev1.PodRunning, corev1.PodRunning), nil)
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

// TestCheckSentinelNumberInMemoryRunningPods checks that a Sentinel that
// knows fewer Sentinels than the spec is reset only when it does not know all
// the running Sentinels. A reset does not add a Pending Sentinel.
func TestCheckSentinelNumberInMemoryRunningPods(t *testing.T) {
	run, pend := corev1.PodRunning, corev1.PodPending
	tests := []struct {
		name      string
		inMemory  int32
		pods      *corev1.PodList
		podsErr   error
		wantReset bool
	}{
		{name: "one sentinel pending", inMemory: 2, pods: podsWithPhases(run, run, pend)},
		{name: "a running sentinel not known", inMemory: 2, pods: podsWithPhases(run, run, run), wantReset: true},
		{name: "pods not listed", inMemory: 2, podsErr: errors.New("list error"), wantReset: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rf := generateRF()
			ms := &mK8SService.Services{}
			ms.On("GetDeploymentPods", namespace, rfservice.GetSentinelName(rf)).Once().Return(test.pods, test.podsErr)
			mr := &mRedisService.Client{}
			mr.On("GetNumberSentinelsInMemory", "1.1.1.1").Once().Return(test.inMemory, nil)
			checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

			err := checker.CheckSentinelNumberInMemory("1.1.1.1", rf)

			assert.Equal(t, test.wantReset, err != nil, "error: %v", err)
			ms.AssertExpectations(t)
		})
	}
}

// TestCheckSentinelSlavesNumberInMemoryRunningPods checks that a Sentinel
// that knows fewer slaves than the spec is reset only when it does not know
// all the running slaves.
func TestCheckSentinelSlavesNumberInMemoryRunningPods(t *testing.T) {
	run, pend := corev1.PodRunning, corev1.PodPending
	tests := []struct {
		name      string
		bootstrap bool
		inMemory  int32
		pods      *corev1.PodList
		podsErr   error
		wantReset bool
	}{
		{name: "one redis pending", inMemory: 1, pods: podsWithPhases(run, run, pend)},
		{name: "a running slave not known", inMemory: 1, pods: podsWithPhases(run, run, run), wantReset: true},
		{name: "bootstrapping, one redis pending", bootstrap: true, inMemory: 2, pods: podsWithPhases(run, run, pend)},
		{name: "pods not listed", inMemory: 1, podsErr: errors.New("list error"), wantReset: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rf := generateRF()
			if test.bootstrap {
				rf.Spec.BootstrapNode = &redisfailoverv1.BootstrapSettings{Host: "127.0.0.1"}
			}
			ms := &mK8SService.Services{}
			ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(test.pods, test.podsErr)
			mr := &mRedisService.Client{}
			mr.On("GetNumberSentinelSlavesInMemory", "1.1.1.1").Once().Return(test.inMemory, nil)
			checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

			err := checker.CheckSentinelSlavesNumberInMemory("1.1.1.1", rf)

			assert.Equal(t, test.wantReset, err != nil, "error: %v", err)
			ms.AssertExpectations(t)
		})
	}
}

func TestCheckSentinelSlavesNumberQuorumInMemoryGetNumberSentinelSlavesInMemoryError(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	ms := &mK8SService.Services{}
	mr := &mRedisService.Client{}
	mr.On("GetNumberSentinelSlavesInMemory", "1.1.1.1").Once().Return(int32(0), errors.New(""))

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	err := checker.CheckSentinelSlavesNumberQuorumInMemory("1.1.1.1", rf)
	assert.Error(err)
}

// TestCheckSentinelSlavesNumberQuorumInMemory covers the fix for the
// deadlock a full-strict CheckSentinelSlavesNumberInMemory gate can cause
// before replacing a stale master: with 5 replicas (4 expected slaves,
// quorum 3), sentinel seeing only 3 of the 4 (one permanently missing, e.g.
// a replica whose PVC is stuck in a dead zone) must still be accepted,
// where the exact-match check would block forever.
func TestCheckSentinelSlavesNumberQuorumInMemory(t *testing.T) {
	tests := []struct {
		name     string
		replicas int32
		nSlaves  int32
		expError bool
	}{
		{"all expected slaves present", 5, 4, false},
		{"quorum met, one permanently missing slave", 5, 3, false},
		{"exactly one below quorum", 5, 2, true},
		{"far below quorum", 5, 0, true},
		// Replicas: 1 means 0 expected slaves (master-only). The majority
		// formula expected/2+1 degenerates to 1 at expected=0, which would
		// wrongly demand a slave that was never expected to exist and
		// permanently block master pod replacement. 0 expected must mean 0
		// quorum.
		{"single replica, no slaves expected", 1, 0, false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)

			rf := generateRF()
			rf.Spec.Redis.Replicas = test.replicas

			ms := &mK8SService.Services{}
			mr := &mRedisService.Client{}
			mr.On("GetNumberSentinelSlavesInMemory", "1.1.1.1").Once().Return(test.nSlaves, nil)

			checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

			err := checker.CheckSentinelSlavesNumberQuorumInMemory("1.1.1.1", rf)
			if test.expError {
				assert.Error(err)
			} else {
				assert.NoError(err)
			}
		})
	}
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

func TestGetNumberMastersReadyPodUnanswered(t *testing.T) {
	ready := []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	pods := &corev1.PodList{
		Items: []corev1.Pod{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "redis-0"},
				Status:     corev1.PodStatus{PodIP: "0.0.0.0", Phase: corev1.PodRunning, Conditions: ready},
			},
			{
				ObjectMeta: metav1.ObjectMeta{Name: "redis-1"},
				Status:     corev1.PodStatus{PodIP: "1.1.1.1", Phase: corev1.PodRunning, Conditions: ready},
			},
		},
	}

	tests := []struct {
		name          string
		otherIsMaster bool
		expN          int
		expErr        bool
	}{
		{name: "no master answered", otherIsMaster: false, expN: 0, expErr: true},
		{name: "another master answered", otherIsMaster: true, expN: 1, expErr: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rf := generateRF()
			ms := &mK8SService.Services{}
			ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
			mr := &mRedisService.Client{}
			mr.On("IsMaster", "0.0.0.0", "0", "").Once().Return(false, errors.New("i/o timeout"))
			mr.On("IsMaster", "1.1.1.1", "0", "").Once().Return(test.otherIsMaster, nil)

			checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

			n, err := checker.GetNumberMasters(rf)
			assert.Equal(t, test.expN, n)
			if test.expErr {
				assert.ErrorContains(t, err, "redis-0")
				assert.ErrorIs(t, err, rfservice.ErrRedisNotAnswering)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// A pod that is not running or that is terminating is not asked, and does not
// make the count unknown.
func TestGetNumberMastersSkipsPodsNotRunningOrTerminating(t *testing.T) {
	ready := []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	pods := &corev1.PodList{
		Items: []corev1.Pod{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "redis-0"},
				Status:     corev1.PodStatus{PodIP: "0.0.0.0", Phase: corev1.PodRunning, Conditions: ready},
			},
			{
				ObjectMeta: metav1.ObjectMeta{Name: "redis-1"},
				Status:     corev1.PodStatus{PodIP: "1.1.1.1", Phase: corev1.PodPending},
			},
			{
				ObjectMeta: metav1.ObjectMeta{Name: "redis-2", DeletionTimestamp: &metav1.Time{Time: time.Now()}},
				Status:     corev1.PodStatus{PodIP: "2.2.2.2", Phase: corev1.PodRunning, Conditions: ready},
			},
		},
	}

	rf := generateRF()
	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	mr := &mRedisService.Client{}
	// The mock panics if IsMaster is called for redis-1 or redis-2.
	mr.On("IsMaster", "0.0.0.0", "0", "").Once().Return(false, nil)

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	n, err := checker.GetNumberMasters(rf)
	assert.NoError(t, err)
	assert.Equal(t, 0, n)
	mr.AssertExpectations(t)
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
			name: "being resized in place",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						appsv1.ControllerRevisionHashLabelKey: "10",
					},
					Annotations: map[string]string{
						"redisfailovers.databases.spotahome.com/resize-requested-at": "2026-01-01T00:00:00Z",
					},
				},
			},
			expectedHash:  "",
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

// TestIsRedisRunningQuorum covers the real RedisFailoverChecker.IsRedisRunningQuorum
// wrapper - AreQuorumRunning itself already has direct table-driven coverage in
// quorum_running_test.go, but that leaves the GetStatefulSetPods call and its
// error branch untested.
func TestIsRedisRunningQuorum(t *testing.T) {
	assert := assert.New(t)
	rf := generateRF()

	t.Run("quorum of pods running", func(t *testing.T) {
		ms := &mK8SService.Services{}
		ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().
			Return(podsWithPhases(corev1.PodRunning, corev1.PodRunning, corev1.PodPending), nil)
		checker := rfservice.NewRedisFailoverChecker(ms, &mRedisService.Client{}, log.DummyLogger{}, metrics.Dummy)
		assert.True(checker.IsRedisRunningQuorum(rf))
	})

	t.Run("below quorum", func(t *testing.T) {
		ms := &mK8SService.Services{}
		ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().
			Return(podsWithPhases(corev1.PodRunning, corev1.PodPending, corev1.PodPending), nil)
		checker := rfservice.NewRedisFailoverChecker(ms, &mRedisService.Client{}, log.DummyLogger{}, metrics.Dummy)
		assert.False(checker.IsRedisRunningQuorum(rf))
	})

	t.Run("GetStatefulSetPods errors", func(t *testing.T) {
		ms := &mK8SService.Services{}
		ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().
			Return(nil, errors.New("statefulset pods unavailable"))
		checker := rfservice.NewRedisFailoverChecker(ms, &mRedisService.Client{}, log.DummyLogger{}, metrics.Dummy)
		assert.False(checker.IsRedisRunningQuorum(rf))
	})
}

// TestIsSentinelRunningQuorum is the sentinel-side counterpart to
// TestIsRedisRunningQuorum, covering RedisFailoverChecker.IsSentinelRunningQuorum.
func TestIsSentinelRunningQuorum(t *testing.T) {
	assert := assert.New(t)
	rf := generateRF()

	t.Run("quorum of pods running", func(t *testing.T) {
		ms := &mK8SService.Services{}
		ms.On("GetDeploymentPods", namespace, rfservice.GetSentinelName(rf)).Once().
			Return(podsWithPhases(corev1.PodRunning, corev1.PodRunning, corev1.PodPending), nil)
		checker := rfservice.NewRedisFailoverChecker(ms, &mRedisService.Client{}, log.DummyLogger{}, metrics.Dummy)
		assert.True(checker.IsSentinelRunningQuorum(rf))
	})

	t.Run("below quorum", func(t *testing.T) {
		ms := &mK8SService.Services{}
		ms.On("GetDeploymentPods", namespace, rfservice.GetSentinelName(rf)).Once().
			Return(podsWithPhases(corev1.PodRunning, corev1.PodPending, corev1.PodPending), nil)
		checker := rfservice.NewRedisFailoverChecker(ms, &mRedisService.Client{}, log.DummyLogger{}, metrics.Dummy)
		assert.False(checker.IsSentinelRunningQuorum(rf))
	})

	t.Run("GetDeploymentPods errors", func(t *testing.T) {
		ms := &mK8SService.Services{}
		ms.On("GetDeploymentPods", namespace, rfservice.GetSentinelName(rf)).Once().
			Return(nil, errors.New("deployment pods unavailable"))
		checker := rfservice.NewRedisFailoverChecker(ms, &mRedisService.Client{}, log.DummyLogger{}, metrics.Dummy)
		assert.False(checker.IsSentinelRunningQuorum(rf))
	})
}

// --- CheckMasterHealth ---
//
// CheckMasterHealth resolves the master via GetMasterIP (which itself calls
// IsMaster once per known pod), then performs a *second*, independent
// IsMaster call directly against the resolved master IP as the actual health
// check. Tests below account for both calls.

func TestCheckMasterHealthPodListError(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(nil, errors.New("list err"))
	mr := &mRedisService.Client{}

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	healthy, masterIP, err := checker.CheckMasterHealth(rf)
	assert.EqualError(err, "list err", "an API error is no reason to promote a replica")
	assert.False(healthy)
	assert.Equal("", masterIP)
}

// GetMasterIP finds no master. Only a second count that confirms it lets the
// caller promote a replica without the failoverTimeout wait.
func TestCheckMasterHealthNoMasterFound(t *testing.T) {
	ready := []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	stall := errors.New("i/o timeout")
	tests := []struct {
		name        string
		secondCount error
		wantErr     error
		wantErrMsg  string
	}{
		{
			name: "the second count finds no master",
		},
		{
			name:        "the ready master does not answer the second count",
			secondCount: stall,
			wantErr:     rfservice.ErrRedisNotAnswering,
		},
		{
			name:       "the master answers the second count",
			wantErrMsg: "the master did not answer every check, checking again on the next reconcile",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rf := generateRF()
			pods := &corev1.PodList{Items: []corev1.Pod{
				{Status: corev1.PodStatus{PodIP: "0.0.0.0", Phase: corev1.PodRunning, Conditions: ready}},
				{Status: corev1.PodStatus{PodIP: "1.1.1.1", Phase: corev1.PodRunning, Conditions: ready}},
			}}
			ms := &mK8SService.Services{}
			ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Twice().Return(pods, nil)
			mr := &mRedisService.Client{}
			// GetMasterIP: the master stalls.
			mr.On("IsMaster", "0.0.0.0", "0", "").Once().Return(false, stall)
			mr.On("IsMaster", "1.1.1.1", "0", "").Twice().Return(false, nil)
			// The second count.
			switch {
			case test.secondCount != nil:
				mr.On("IsMaster", "0.0.0.0", "0", "").Once().Return(false, test.secondCount)
			case test.wantErrMsg != "":
				mr.On("IsMaster", "0.0.0.0", "0", "").Once().Return(true, nil)
			default:
				mr.On("IsMaster", "0.0.0.0", "0", "").Once().Return(false, nil)
			}

			checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

			healthy, masterIP, err := checker.CheckMasterHealth(rf)
			switch {
			case test.wantErr != nil:
				assert.ErrorIs(t, err, test.wantErr)
			case test.wantErrMsg != "":
				assert.EqualError(t, err, test.wantErrMsg)
			default:
				assert.NoError(t, err)
			}
			assert.False(t, healthy)
			assert.Equal(t, "", masterIP)
			mr.AssertExpectations(t)
		})
	}
}

// TestCheckMasterHealthSplitBrainDetected covers the case GetMasterIP
// returns ErrAmbiguousMasterCount because more than one pod claims to be
// master. Before this fix, CheckMasterHealth treated any GetMasterIP error
// identically to "no master" and returned (false, "", nil) - which the
// caller (checkAndHealOperatorManagedMode) reads as "go promote another
// replica", turning an existing split-brain into a 3-way conflict instead of
// routing it to manual intervention.
func TestCheckMasterHealthSplitBrainDetected(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()

	pods := &corev1.PodList{
		Items: []corev1.Pod{
			{Status: corev1.PodStatus{PodIP: "0.0.0.0", Phase: corev1.PodRunning}},
			{Status: corev1.PodStatus{PodIP: "1.1.1.1", Phase: corev1.PodRunning}},
		},
	}

	ms := &mK8SService.Services{}
	// Once for GetMasterIP's own scan, once more for CheckMasterHealth's
	// corroborating GetNumberMasters call.
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Twice().Return(pods, nil)
	mr := &mRedisService.Client{}
	mr.On("IsMaster", "0.0.0.0", "0", "").Times(2).Return(true, nil)
	mr.On("IsMaster", "1.1.1.1", "0", "").Times(2).Return(true, nil)

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

	healthy, masterIP, err := checker.CheckMasterHealth(rf)
	assert.Error(err, "split-brain must be reported as an error, not silently treated as no-master")
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
	mr.On("IsMaster", "0.0.0.0", "0", "").Once().Return(true, nil)                         // used by GetMasterIP
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

func TestGetReplicaReplicationOffsetsSyncedFlag(t *testing.T) {
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
				assert.Equal(test.expectedReady, replicas[0].Synced)
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
		assert.True(best.Synced)
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
		assert.False(best.Synced)
	}
}

func TestGetBestReplicaForPromotionRanking(t *testing.T) {
	readyCond := []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	notReadyCond := []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}

	type replica struct {
		ready  bool
		linkUp bool
		offset int64
	}
	tests := []struct {
		name     string
		replicas []replica
		expected string
	}{
		{
			// A master that restarted in place can have writes that the
			// Ready replica did not get. A promotion of that replica drops them.
			name:     "higher offset wins over a ready pod",
			replicas: []replica{{ready: false, offset: 900}, {ready: true, offset: 800}},
			expected: "1.1.1.1",
		},
		{
			// After a graceful shutdown, the restarted old master has the
			// same offset as its replica.
			name:     "ready pod wins at equal offset",
			replicas: []replica{{ready: false, offset: 800}, {ready: true, offset: 800}},
			expected: "1.1.1.2",
		},
		{
			name:     "synced replica wins over a higher offset",
			replicas: []replica{{ready: true, offset: 900}, {ready: false, linkUp: true, offset: 800}},
			expected: "1.1.1.2",
		},
		{
			name:     "highest offset when no pod is ready",
			replicas: []replica{{ready: false, offset: 700}, {ready: false, offset: 800}},
			expected: "1.1.1.2",
		},
		{
			name:     "first in list on equal rank and offset",
			replicas: []replica{{ready: true, offset: 800}, {ready: true, offset: 800}},
			expected: "1.1.1.1",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			rf := generateRF()

			pods := &corev1.PodList{}
			mr := &mRedisService.Client{}
			for i, rep := range test.replicas {
				ip := fmt.Sprintf("1.1.1.%d", i+1)
				conds := notReadyCond
				if rep.ready {
					conds = readyCond
				}
				pods.Items = append(pods.Items, corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("rfr-%d", i)},
					Status:     corev1.PodStatus{PodIP: ip, Phase: corev1.PodRunning, Conditions: conds},
				})
				link := "down"
				if rep.linkUp {
					link = "up"
				}
				mr.On("GetReplicationInfo", ip, "0", "").Once().Return(&redis.ReplicationInfo{
					Role: "slave", MasterLinkStatus: link, SlaveReplOffset: rep.offset,
				}, nil)
			}

			ms := &mK8SService.Services{}
			ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)

			checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

			best, err := checker.GetBestReplicaForPromotion(rf)
			assert.NoError(err)
			if assert.NotNil(best) {
				assert.Equal(test.expected, best.IP)
			}
		})
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

// TestCheckSentinelsCannotFailover covers the state of the soak test: every
// Sentinel monitors the IP of a pod that is gone and knows no replica.
func TestCheckSentinelsCannotFailover(t *testing.T) {
	const gone = "10.0.0.9"
	podWithIP := func(name, ip string, phase corev1.PodPhase) corev1.Pod {
		return corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Status:     corev1.PodStatus{Phase: phase, PodIP: ip},
		}
	}
	redisPods := &corev1.PodList{Items: []corev1.Pod{
		podWithIP("rfr-test-0", "10.0.0.1", corev1.PodRunning),
		podWithIP("rfr-test-1", "10.0.0.2", corev1.PodRunning),
		// A pod not running yet keeps its IP when it starts, so it counts.
		podWithIP("rfr-test-2", "10.0.0.3", corev1.PodPending),
		// A pod without an IP matches no Sentinel address.
		podWithIP("rfr-test-3", "", corev1.PodPending),
	}}
	sentinelPods := &corev1.PodList{Items: []corev1.Pod{
		podWithIP("rfs-test-a", "10.1.0.1", corev1.PodRunning),
		podWithIP("rfs-test-b", "10.1.0.2", corev1.PodRunning),
		podWithIP("rfs-test-c", "10.1.0.3", corev1.PodRunning),
	}}
	sentinels := []string{"10.1.0.1", "10.1.0.2", "10.1.0.3"}

	type view struct {
		master     string
		masterErr  error
		replicas   []string
		replicaErr error
	}
	stuck := view{master: gone}
	tests := []struct {
		name        string
		redisErr    error
		sentinelErr error
		views       []view
		want        bool
		wantErr     bool
	}{
		{
			name:  "no Sentinel knows a replica and the master is gone",
			views: []view{stuck, stuck, stuck},
			want:  true,
		},
		{
			name:  "the Sentinels know only replicas that are gone",
			views: []view{{master: gone, replicas: []string{"10.0.0.8"}}, stuck, stuck},
			want:  true,
		},
		{
			name:  "a Sentinel monitors the address 127.0.0.1 of its configuration",
			views: []view{{master: "127.0.0.1"}, stuck, stuck},
			want:  true,
		},
		{
			name:  "a Sentinel knows a replica that runs, so Sentinel can fail over",
			views: []view{stuck, {master: gone, replicas: []string{"10.0.0.8", "10.0.0.2"}}, stuck},
		},
		{
			name:  "a Sentinel monitors a pod, so the operator must not elect a second master",
			views: []view{stuck, stuck, {master: "10.0.0.1"}},
		},
		{
			name:  "a Sentinel monitors a pod that is not running",
			views: []view{{master: "10.0.0.3"}, stuck, stuck},
		},
		{
			name:  "a quorum answers and knows no replica",
			views: []view{stuck, {masterErr: errors.New("timeout")}, stuck},
			want:  true,
		},
		{
			name:  "less than a quorum answers",
			views: []view{stuck, {masterErr: errors.New("timeout")}, {master: gone, replicaErr: errors.New("timeout")}},
		},
		{
			name:     "listing the redis pods fails",
			redisErr: errors.New("list err"),
			wantErr:  true,
		},
		{
			name:        "listing the Sentinel pods fails",
			sentinelErr: errors.New("list err"),
			wantErr:     true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rf := generateRF()
			ms := &mK8SService.Services{}
			ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(redisPods, test.redisErr)
			if test.redisErr == nil {
				ms.On("GetDeploymentPods", namespace, rfservice.GetSentinelName(rf)).Once().Return(sentinelPods, test.sentinelErr)
			}
			mr := &mRedisService.Client{}
			for i, v := range test.views {
				mr.On("GetSentinelMonitor", sentinels[i]).Maybe().Return(v.master, "6379", v.masterErr)
				mr.On("GetSentinelReplicas", sentinels[i]).Maybe().Return(v.replicas, v.replicaErr)
			}

			checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)
			got, err := checker.CheckSentinelsCannotFailover(rf)
			if test.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
			assert.Equal(t, test.want, got)
			ms.AssertExpectations(t)
		})
	}
}
