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
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	kubetesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
	"github.com/saremox/redis-operator/service/k8s"
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
	clock    time.Time
}

func newSentinelRolloutTest(t *testing.T) *sentinelRolloutTest {
	t.Helper()
	st := &sentinelRolloutTest{rolloutTest: newStaleRolloutTest(t, "rfr-test-0", true), logger: &warnLogger{}}
	st.sentinelPods = []corev1.Pod{sentinelPod("rfs-test-a", false), sentinelPod("rfs-test-b", false), sentinelPod("rfs-test-c", false)}
	st.heal.On("ResizePodInPlace", st.rf, "rfr-test-0", "new", rfservice.AutoscalerLimits{}).Return(rfservice.ResizeResult{Action: rfservice.ResizeRecreate}, nil)
	st.handler.logger = st.logger
	st.handler.requeue = func(_ string, after time.Duration) { st.requeues = append(st.requeues, after) }
	st.clock = time.Date(2026, 10, 18, 16, 0, 0, 0, time.UTC)
	st.handler.now = func() time.Time { return st.clock }
	// newRolloutTest restores the bound.
	rolloutStallTimeout = 10 * time.Minute
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
		noReplicas  bool
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
			// The API server sets 1 for a missing spec.replicas.
			name:       "a missing spec.replicas is 1",
			status:     rolledOutSentinels().Status,
			noReplicas: true,
			reason:     "Deployment rfs-test has 3 pods, 3 updated, 3 ready and 3 available, of 1",
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
			if test.noReplicas {
				st.sentinels.Spec.Replicas = nil
			}
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
			assert.Equal(t, types.UID("sentinels/2"), last.uid)
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

// A Sentinel rollout that does not finish holds the master pod only for
// rolloutStallTimeout. Then one Warning and the status say so, and the master
// replacement continues with the Sentinel quorum check.
func TestUpdateRedisesPodsReplacesTheMasterAfterAStalledSentinelRollout(t *testing.T) {
	st := newSentinelRolloutTest(t)
	st.sentinels.Status.ReadyReplicas = 2
	st.sentinels.Status.AvailableReplicas = 2
	reason := "Deployment rfs-test has 3 pods, 3 updated, 2 ready and 2 available, of 3"
	assert.Equal(t, sentinelRolloutMsg, st.update(""))

	st.clock = st.clock.Add(rolloutStallTimeout - time.Millisecond)
	assert.Equal(t, sentinelRolloutMsg, st.update(""))
	st.assertMasterKept(t)
	assert.Empty(t, st.logger.warnings)

	st.clock = st.clock.Add(time.Millisecond)
	stall := "rollout waiting for more than 10m: " + reason + ", so the master pod replacement continues before the end of the Sentinel rollout"
	st.check.On("GetSentinelsIPs", st.rf).Once().Return([]string{"10.0.1.1"}, nil)
	st.check.On("CheckSentinelSlavesNumberQuorumInMemory", "10.0.1.1", st.rf).Once().Return(errors.New("below quorum"))
	assert.Equal(t, stall, st.update(""))
	assert.Equal(t, []string{stall}, st.logger.warnings)
	st.heal.AssertNotCalled(t, "DeletePod", mock.Anything, mock.Anything)

	// While the quorum check holds the master, the status keeps the reason.
	st.clock = st.clock.Add(time.Minute)
	st.check.On("GetSentinelsIPs", st.rf).Once().Return([]string{"10.0.1.1"}, nil)
	st.check.On("CheckSentinelSlavesNumberQuorumInMemory", "10.0.1.1", st.rf).Once().Return(errors.New("below quorum"))
	assert.Equal(t, stall, st.update(""))
	assert.Equal(t, []string{stall}, st.logger.warnings)
	st.heal.AssertNotCalled(t, "DeletePod", mock.Anything, mock.Anything)

	// The passed wait stays, so a later reconcile deletes the master without
	// a second Warning.
	st.clock = st.clock.Add(time.Minute)
	st.requeues = nil
	st.expectMasterDelete()
	assert.Equal(t, stall, st.update(""))
	assert.Equal(t, []string{stall}, st.logger.warnings)
	assert.Empty(t, st.requeues)
	st.heal.AssertExpectations(t)
	st.check.AssertExpectations(t)
}

// A new generation of the Sentinel Deployment is a new rollout, so the bound
// starts again.
func TestUpdateRedisesPodsRestartsTheSentinelRolloutWaitForANewGeneration(t *testing.T) {
	st := newSentinelRolloutTest(t)
	st.sentinels.Status.UpdatedReplicas = 2
	assert.Equal(t, sentinelRolloutMsg, st.update(""))

	st.clock = st.clock.Add(rolloutStallTimeout - time.Minute)
	st.sentinels.Generation, st.sentinels.Status.ObservedGeneration = 3, 3
	st.logger.reset()
	assert.Equal(t, sentinelRolloutMsg, st.update(""))
	assert.Len(t, st.logger.infos, 1)

	st.clock = st.clock.Add(time.Minute)
	assert.Equal(t, sentinelRolloutMsg, st.update(""))
	st.assertMasterKept(t)
	assert.Empty(t, st.logger.warnings)

	st.clock = st.clock.Add(rolloutStallTimeout - time.Minute)
	st.expectMasterDelete()
	assert.Contains(t, st.update(""), "so the master pod replacement continues")
	st.heal.AssertExpectations(t)
}

// apiServerDeployments makes cs act as the API server for a Deployment. It
// sets the probe defaults that generateSentinelDeployment leaves out, so each
// reconcile sends an update. It increments the generation when the spec or the
// annotations change, as PrepareForUpdate of the Deployment registry does.
func apiServerDeployments(cs *kubefake.Clientset) {
	gvr := appsv1.SchemeGroupVersion.WithResource("deployments")
	defaults := func(d *appsv1.Deployment) {
		for i := range d.Spec.Template.Spec.Containers {
			c := &d.Spec.Template.Spec.Containers[i]
			for _, p := range []*corev1.Probe{c.LivenessProbe, c.ReadinessProbe, c.StartupProbe} {
				if p != nil {
					p.PeriodSeconds, p.SuccessThreshold, p.FailureThreshold = 10, 1, 3
				}
			}
		}
	}
	cs.PrependReactor("create", "deployments", func(action kubetesting.Action) (bool, runtime.Object, error) {
		d := action.(kubetesting.CreateAction).GetObject().(*appsv1.Deployment).DeepCopy()
		defaults(d)
		d.UID, d.Generation = "sentinels", 1
		return true, d, cs.Tracker().Create(gvr, d, d.Namespace)
	})
	cs.PrependReactor("update", "deployments", func(action kubetesting.Action) (bool, runtime.Object, error) {
		d := action.(kubetesting.UpdateAction).GetObject().(*appsv1.Deployment).DeepCopy()
		obj, err := cs.Tracker().Get(gvr, d.Namespace, d.Name)
		if err != nil {
			return true, nil, err
		}
		old := obj.(*appsv1.Deployment)
		defaults(d)
		d.UID, d.Generation, d.Status = old.UID, old.Generation, old.Status
		if !equality.Semantic.DeepEqual(d.Spec, old.Spec) || !equality.Semantic.DeepEqual(d.Annotations, old.Annotations) {
			d.Generation++
		}
		return true, d, cs.Tracker().Update(gvr, d, d.Namespace)
	})
}

// Each reconcile updates the Sentinel Deployment before the checks, and the
// Deployment controller then sets its revision and its status. The update must
// not start a new generation, or the wait for the Sentinel rollout starts
// again in each reconcile and never ends.
func TestUpdateRedisesPodsBoundsTheSentinelRolloutWaitOverReconciles(t *testing.T) {
	st := newSentinelRolloutTest(t)
	st.rf.Spec.Sentinel.Replicas = 3
	st.rf.Spec.Sentinel.Image = "redis:7"
	cs := kubefake.NewClientset()
	apiServerDeployments(cs)
	client := rfservice.NewRedisFailoverKubeClient(k8s.New(cs, nil, log.Dummy, metrics.Dummy), log.Dummy, metrics.Dummy)
	gvr := appsv1.SchemeGroupVersion.WithResource("deployments")
	reason := "Deployment rfs-test has 4 pods, 3 updated, 3 ready and 3 available, of 3"
	reconcile := func() string {
		t.Helper()
		require.NoError(t, client.EnsureSentinelDeployment(st.rf, map[string]string{}, nil))
		obj, err := cs.Tracker().Get(gvr, "testns", "rfs-test")
		require.NoError(t, err)
		d := obj.(*appsv1.Deployment).DeepCopy()
		d.Annotations = map[string]string{"deployment.kubernetes.io/revision": "2"}
		d.Status = appsv1.DeploymentStatus{ObservedGeneration: d.Generation, Replicas: 4, UpdatedReplicas: 3, ReadyReplicas: 3, AvailableReplicas: 3}
		require.NoError(t, cs.Tracker().Update(gvr, d, "testns"))
		st.sentinels = d
		return st.update("")
	}
	start := st.clock

	assert.Equal(t, sentinelRolloutMsg, reconcile())
	generation := st.sentinels.Generation
	for range 3 {
		st.clock = st.clock.Add(sentinelRolloutRecheck)
		assert.Equal(t, sentinelRolloutMsg, reconcile())
	}
	st.assertMasterKept(t)
	updates := 0
	for _, a := range cs.Actions() {
		if a.GetVerb() == "update" && a.GetResource().Resource == "deployments" {
			updates++
		}
	}
	// The update path runs. The count changes when deploymentUpToDate sets the probe defaults.
	assert.Equal(t, 3, updates)
	assert.Equal(t, generation, st.sentinels.Generation)
	assert.Equal(t, []string{sentinelRolloutMsg + ": " + reason}, st.logger.infos)
	continued := 0
	for _, msg := range st.logger.debugs {
		if msg == sentinelRolloutMsg+": "+reason {
			continued++
		}
	}
	assert.Equal(t, 3, continued)
	v, _ := st.handler.rolloutWaits.Load(passwordKey(st.rf))
	last, ok := v.(rolloutWait)
	require.True(t, ok)
	assert.Equal(t, start, last.since)

	st.clock = start.Add(rolloutStallTimeout)
	st.expectMasterDelete()
	stall := "rollout waiting for more than 10m: " + reason + ", so the master pod replacement continues before the end of the Sentinel rollout"
	assert.Equal(t, stall, reconcile())
	assert.Equal(t, []string{stall}, st.logger.warnings)
	st.heal.AssertExpectations(t)
	st.check.AssertExpectations(t)
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
