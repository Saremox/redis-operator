package redisfailover

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
)

// rolledOutSentinels returns a Sentinel Deployment of three pods that has
// finished its rollout.
func rolledOutSentinels() *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "rfs-test", UID: "sentinels", Generation: 2},
		Spec:       appsv1.DeploymentSpec{Replicas: ptr.To(int32(3))},
		Status:     appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 3, UpdatedReplicas: 3, ReadyReplicas: 3, AvailableReplicas: 3},
	}
}

func sentinelPod(name string, terminating bool) corev1.Pod {
	pod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	if terminating {
		pod.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	}
	return pod
}

// sentinelRolloutTest is a stale master in Sentinel mode whose replica is
// synced and ready, so that only the gates before the master delete remain.
type sentinelRolloutTest struct {
	*rolloutTest
	logger   *warnLogger
	requeues []time.Duration
}

func newSentinelRolloutTest(t *testing.T) *sentinelRolloutTest {
	t.Helper()
	st := &sentinelRolloutTest{rolloutTest: newStaleRolloutTest(t, "rfr-test-0", true), logger: &warnLogger{}}
	st.sentinelPods = []corev1.Pod{sentinelPod("rfs-test-a", false), sentinelPod("rfs-test-b", false), sentinelPod("rfs-test-c", false)}
	st.heal.On("ResizePodInPlace", st.rf, "rfr-test-0", "new", rfservice.AutoscalerLimits{}).Return(rfservice.ResizeResult{Action: rfservice.ResizeRecreate}, nil)
	st.handler.logger = st.logger
	st.handler.requeue = func(_ string, after time.Duration) { st.requeues = append(st.requeues, after) }
	return st
}

// expectMasterDelete sets the Sentinel quorum check and the delete that
// follow a finished Sentinel rollout.
func (st *sentinelRolloutTest) expectMasterDelete() {
	st.check.On("GetSentinelsIPs", st.rf).Once().Return([]string{"10.0.1.1"}, nil)
	st.check.On("CheckSentinelSlavesNumberQuorumInMemory", "10.0.1.1", st.rf).Once().Return(nil)
	st.heal.On("DeletePod", "rfr-test-0", st.rf).Once().Return(nil)
}

func (st *sentinelRolloutTest) assertMasterKept(t *testing.T) {
	t.Helper()
	st.heal.AssertNotCalled(t, "DeletePod", mock.Anything, mock.Anything)
	st.check.AssertNotCalled(t, "GetSentinelsIPs", mock.Anything)
}

// The preStop hook of the master sends SENTINEL FAILOVER through the Sentinel
// Service. A Sentinel that the rollout stops must not get it, so the master
// pod stays until the Sentinel rollout has finished.
func TestUpdateRedisesPodsKeepsTheMasterDuringTheSentinelRollout(t *testing.T) {
	tests := []struct {
		name        string
		status      appsv1.DeploymentStatus
		terminating bool
		reason      string
	}{
		{
			name:   "a surge pod is not ready",
			status: appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 4, UpdatedReplicas: 2, ReadyReplicas: 3, AvailableReplicas: 3},
			reason: "Deployment rfs-test has 4 pods, 2 updated, 3 ready and 3 available, of 3",
		},
		{
			// A strategy with maxSurge 0 replaces one pod after the other.
			name:   "a pod is not updated",
			status: appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 3, UpdatedReplicas: 2, ReadyReplicas: 3, AvailableReplicas: 3},
			reason: "Deployment rfs-test has 3 pods, 2 updated, 3 ready and 3 available, of 3",
		},
		{
			name:   "an old pod is not stopped",
			status: appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 4, UpdatedReplicas: 3, ReadyReplicas: 4, AvailableReplicas: 4},
			reason: "Deployment rfs-test has 4 pods, 3 updated, 4 ready and 4 available, of 3",
		},
		{
			name:   "an old pod that is not ready is not stopped",
			status: appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 4, UpdatedReplicas: 3, ReadyReplicas: 3, AvailableReplicas: 3},
			reason: "Deployment rfs-test has 4 pods, 3 updated, 3 ready and 3 available, of 3",
		},
		{
			name:   "an updated pod is not ready",
			status: appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 3, UpdatedReplicas: 3, ReadyReplicas: 2, AvailableReplicas: 2},
			reason: "Deployment rfs-test has 3 pods, 3 updated, 2 ready and 2 available, of 3",
		},
		{
			name:   "an updated pod is not available",
			status: appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 3, UpdatedReplicas: 3, ReadyReplicas: 3, AvailableReplicas: 2},
			reason: "Deployment rfs-test has 3 pods, 3 updated, 3 ready and 2 available, of 3",
		},
		{
			name:   "the Deployment controller did not observe the template",
			status: appsv1.DeploymentStatus{ObservedGeneration: 1, Replicas: 3, UpdatedReplicas: 3, ReadyReplicas: 3, AvailableReplicas: 3},
			reason: "the Deployment controller did not observe generation 2 of Deployment rfs-test",
		},
		{
			name:        "a Sentinel pod is terminating",
			status:      rolledOutSentinels().Status,
			terminating: true,
			reason:      "pod rfs-test-old of Deployment rfs-test is terminating",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			st := newSentinelRolloutTest(t)
			st.sentinels.Status = test.status
			if test.terminating {
				st.sentinelPods = append(st.sentinelPods, sentinelPod("rfs-test-old", true))
			}

			assert.Equal(t, sentinelRolloutMsg, st.update(""))
			st.assertMasterKept(t)
			assert.Equal(t, []string{sentinelRolloutMsg + ": " + test.reason}, st.logger.infos)
			assert.Equal(t, []time.Duration{sentinelRolloutRecheck}, st.requeues)
			v, _ := st.handler.rolloutWaits.Load(passwordKey(st.rf))
			last, ok := v.(rolloutWait)
			require.True(t, ok)
			assert.Equal(t, types.UID("sentinels"), last.uid)
			assert.Equal(t, test.reason, last.reason)

			// A later reconcile of the same wait logs at Debug.
			st.logger.reset()
			assert.Equal(t, sentinelRolloutMsg, st.update(""))
			st.assertMasterKept(t)
			assert.Empty(t, st.logger.infos)
			assert.Contains(t, st.logger.debugs, sentinelRolloutMsg+": "+test.reason)
		})
	}
}

// After the Sentinel rollout, the master delete follows the Sentinel quorum
// check as without a rollout, and the wait ends.
func TestUpdateRedisesPodsReplacesTheMasterAfterTheSentinelRollout(t *testing.T) {
	st := newSentinelRolloutTest(t)
	st.sentinels.Status.UpdatedReplicas = 2
	st.sentinelPods = append(st.sentinelPods, sentinelPod("rfs-test-old", true))
	assert.Equal(t, sentinelRolloutMsg, st.update(""))
	st.assertMasterKept(t)

	// The Deployment status shows the end of the rollout before the old pod
	// is gone.
	st.sentinels.Status = rolledOutSentinels().Status
	assert.Equal(t, sentinelRolloutMsg, st.update(""))
	st.assertMasterKept(t)

	st.sentinelPods = st.sentinelPods[:3]
	st.requeues = nil
	st.expectMasterDelete()
	assert.Empty(t, st.update(""))
	st.heal.AssertExpectations(t)
	st.check.AssertExpectations(t)
	assert.Empty(t, st.requeues)
	_, ok := st.handler.rolloutWaits.Load(passwordKey(st.rf))
	assert.False(t, ok)
}

// A Sentinel rollout that does not finish holds the master pod. After
// rolloutStallTimeout, the status and a Warning say so, as for each other
// wait of the rollout.
func TestUpdateRedisesPodsReportsAStalledSentinelRollout(t *testing.T) {
	st := newSentinelRolloutTest(t)
	st.sentinels.Status.ReadyReplicas = 2
	st.sentinels.Status.AvailableReplicas = 2
	reason := "Deployment rfs-test has 3 pods, 3 updated, 2 ready and 2 available, of 3"
	assert.Equal(t, sentinelRolloutMsg, st.update(""))
	assert.Empty(t, st.logger.warnings)

	time.Sleep(2 * rolloutStallTimeout)
	stall := "rollout waiting for more than 0m: " + reason
	assert.Equal(t, sentinelRolloutMsg+"; "+stall, st.update(""))
	assert.Equal(t, []string{stall}, st.logger.warnings)
	st.assertMasterKept(t)
}

// Without Sentinel, the master hands its role over, and no Sentinel
// Deployment exists.
func TestUpdateRedisesPodsReadsNoSentinelDeploymentInOperatorManagedMode(t *testing.T) {
	rt := newStaleRolloutTest(t, "rfr-test-0", false)
	rt.heal.On("ResizePodInPlace", rt.rf, "rfr-test-0", "new", rfservice.AutoscalerLimits{}).Once().Return(rfservice.ResizeResult{Action: rfservice.ResizeRecreate}, nil)
	rt.check.On("GetBestReplicaForPromotion", rt.rf).Once().Return(&rfservice.ReplicaInfo{IP: "10.0.0.2", PodName: "rfr-test-1", Synced: true}, nil)
	rt.heal.On("HandOverMaster", "10.0.0.1", "10.0.0.2", rt.rf).Once().Return(rfservice.HandoverDone, nil)
	rt.heal.On("PromoteBestReplica", "10.0.0.2", rt.rf).Once().Return(nil)

	assert.Empty(t, rt.update(""))
	rt.heal.AssertExpectations(t)
	rt.k8s.AssertNotCalled(t, "GetDeployment", mock.Anything, mock.Anything)
	rt.k8s.AssertNotCalled(t, "GetDeploymentPods", mock.Anything, mock.Anything)
}

// A failed read of the Sentinel Deployment or its pods keeps the master pod.
func TestUpdateRedisesPodsKeepsTheMasterWithoutTheSentinelRollout(t *testing.T) {
	notFound := apierrors.NewNotFound(schema.GroupResource{Group: "apps", Resource: "deployments"}, "rfs-test")
	errBoom := errors.New("boom")
	tests := map[string]func(*sentinelRolloutTest){
		"no Deployment":                  func(st *sentinelRolloutTest) { st.sentinels, st.sentinelsErr = nil, notFound },
		"no pod list":                    func(st *sentinelRolloutTest) { st.sentinelPodsErr = errBoom },
		"a read error of the Deployment": func(st *sentinelRolloutTest) { st.sentinels, st.sentinelsErr = nil, errBoom },
	}
	for name, setup := range tests {
		t.Run(name, func(t *testing.T) {
			st := newSentinelRolloutTest(t)
			setup(st)
			st.rf.Status.Message = ""
			assert.Error(t, st.handler.UpdateRedisesPods(st.rf))
			st.assertMasterKept(t)
			assert.Empty(t, st.requeues)
		})
	}
}
