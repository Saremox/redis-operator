package redisfailover

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	mRFService "github.com/saremox/redis-operator/mocks/operator/redisfailover/service"
	mK8SService "github.com/saremox/redis-operator/mocks/service/k8s"
	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
)

var podReady = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}

// rolloutTest is a two pod RedisFailover whose master rfr-test-0 is on the old
// revision and whose replica rfr-test-1 is on the new one.
type rolloutTest struct {
	t       *testing.T
	rf      *redisfailoverv1.RedisFailover
	pods    []corev1.Pod
	synced  bool
	revErr  error
	podsErr error
	// sentinels is the Sentinel Deployment, and sentinelPods are its pods.
	sentinels       *appsv1.Deployment
	sentinelsErr    error
	sentinelPods    []corev1.Pod
	sentinelPodsErr error
	check           *mRFService.RedisFailoverCheck
	heal            *mRFService.RedisFailoverHeal
	k8s             *mK8SService.Services
	handler         *RedisFailoverHandler
}

func newRolloutTest(t *testing.T) *rolloutTest {
	t.Helper()
	timeout := rolloutStallTimeout
	t.Cleanup(func() { rolloutStallTimeout = timeout })
	rolloutStallTimeout = 50 * time.Millisecond

	rt := &rolloutTest{
		t: t,
		rf: &redisfailoverv1.RedisFailover{
			ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "testns"},
			Spec:       redisfailoverv1.RedisFailoverSpec{Redis: redisfailoverv1.RedisSettings{Replicas: 2}},
		},
		pods: []corev1.Pod{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "rfr-test-0", UID: "m", Labels: map[string]string{appsv1.ControllerRevisionHashLabelKey: "old"}},
				Status:     corev1.PodStatus{PodIP: "10.0.0.1", Conditions: podReady},
			},
			{
				ObjectMeta: metav1.ObjectMeta{Name: "rfr-test-1", UID: "r1", Labels: map[string]string{appsv1.ControllerRevisionHashLabelKey: "new"}},
				Status:     corev1.PodStatus{PodIP: "10.0.0.2"},
			},
		},
		heal:      &mRFService.RedisFailoverHeal{},
		sentinels: rolledOutSentinels(),
	}
	rf := rt.rf
	mrfc := &mRFService.RedisFailoverCheck{}
	rt.check = mrfc
	mrfc.On("GetRedisesIPs", rf).Return([]string{"10.0.0.1", "10.0.0.2"}, nil)
	mrfc.On("GetMasterIP", rf).Return("10.0.0.1", nil)
	mrfc.On("CheckRedisSlavesReady", "10.0.0.2", rf).Return(func(string, *redisfailoverv1.RedisFailover) (bool, error) {
		return rt.synced, nil
	})
	mrfc.On("GetStatefulSetUpdateRevision", rf).Return(func(*redisfailoverv1.RedisFailover) (string, error) {
		return "new", rt.revErr
	})
	mrfc.On("GetRedisesSlavesPods", rf).Return([]string{"rfr-test-1"}, nil)
	mrfc.On("GetRedisRevisionHash", "rfr-test-1", rf).Return(func(string, *redisfailoverv1.RedisFailover) (string, error) {
		if len(rt.pods) < 2 {
			return "new", nil
		}
		return rt.pods[1].Labels[appsv1.ControllerRevisionHashLabelKey], nil
	})
	mrfc.On("GetRedisesMasterPod", rf).Return("rfr-test-0", nil)
	mrfc.On("GetRedisRevisionHash", "rfr-test-0", rf).Return(func(string, *redisfailoverv1.RedisFailover) (string, error) {
		return rt.pods[0].Labels[appsv1.ControllerRevisionHashLabelKey], nil
	})
	mk := &mK8SService.Services{}
	mk.On("GetStatefulSetPods", "testns", "rfr-test").Return(func(string, string) (*corev1.PodList, error) {
		return &corev1.PodList{Items: rt.pods}, rt.podsErr
	})
	mk.On("GetDeployment", "testns", "rfs-test").Maybe().Return(func(string, string) (*appsv1.Deployment, error) {
		return rt.sentinels, rt.sentinelsErr
	})
	mk.On("GetDeploymentPods", "testns", "rfs-test").Maybe().Return(func(string, string) (*corev1.PodList, error) {
		return &corev1.PodList{Items: rt.sentinelPods}, rt.sentinelPodsErr
	})
	rt.k8s = mk
	rt.handler = NewRedisFailoverHandler(Config{}, &mRFService.RedisFailoverClient{}, mrfc, rt.heal, mk, metrics.Dummy, log.Dummy)
	return rt
}

// update runs UpdateRedisesPods on a fresh status, keeping message, and
// returns the status message.
func (rt *rolloutTest) update(message string) string {
	rt.rf.Status = redisfailoverv1.RedisFailoverStatus{Message: message}
	assert.NoError(rt.t, rt.handler.UpdateRedisesPods(rt.rf))
	return rt.rf.Status.Message
}

func TestUpdateRedisesPodsReportsAStalledRollout(t *testing.T) {
	rt := newRolloutTest(t)

	// A replaced replica that does not sync is reported once the rollout has
	// waited on it for longer than the bound.
	assert.Empty(t, rt.update(""))
	time.Sleep(2 * rolloutStallTimeout)
	assert.Equal(t, "rollout waiting on pod rfr-test-1 for more than 0m: not synced with the master", rt.update(""))

	// The pod recreated starts over.
	rt.pods[1].UID = "r2"
	assert.Empty(t, rt.update(""))

	// Once it syncs, the rollout moves on, and a later wait starts over.
	time.Sleep(2 * rolloutStallTimeout)
	rt.synced = true
	rt.pods[1].Status.Conditions = podReady
	rt.heal.On("ResizePodInPlace", rt.rf, "rfr-test-0", "new", rfservice.AutoscalerLimits{}).Once().Return(rfservice.ResizeResult{Action: rfservice.ResizeRecreate}, nil)
	rt.check.On("GetBestReplicaForPromotion", rt.rf).Once().Return(&rfservice.ReplicaInfo{IP: "10.0.0.2", PodName: "rfr-test-1", Synced: true}, nil)
	rt.heal.On("HandOverMaster", "10.0.0.1", "10.0.0.2", rt.rf).Once().Return(rfservice.HandoverDone, nil)
	rt.heal.On("PromoteBestReplica", "10.0.0.2", rt.rf).Once().Return(nil)
	assert.Empty(t, rt.update(""))
	rt.synced = false
	assert.Empty(t, rt.update(""))

	// A replica that is not synced while no rollout is pending is not reported.
	rt.pods[0].Labels[appsv1.ControllerRevisionHashLabelKey] = "new"
	time.Sleep(2 * rolloutStallTimeout)
	assert.Empty(t, rt.update(""))
	rt.heal.AssertExpectations(t)
}

func TestUpdateRedisesPodsReportsAStalledReplacement(t *testing.T) {
	tests := []struct {
		name string
		// pods changes the replica, which is synced, so the rollout waits in
		// redisPodsSettled before replacing the master.
		pods func(pods []corev1.Pod) []corev1.Pod
		want string
	}{
		{
			name: "replaced pod not ready",
			pods: func(pods []corev1.Pod) []corev1.Pod { return pods },
			want: "rollout waiting on pod rfr-test-1 for more than 0m: not ready",
		},
		{
			name: "pod terminating",
			pods: func(pods []corev1.Pod) []corev1.Pod {
				pods[1].DeletionTimestamp = &metav1.Time{Time: time.Now()}
				return pods
			},
			want: "rollout waiting on pod rfr-test-1 for more than 0m: terminating",
		},
		{
			name: "pod missing",
			pods: func(pods []corev1.Pod) []corev1.Pod { return pods[:1] },
			want: "rollout waiting for more than 0m: 1 of 2 pods exist",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rt := newRolloutTest(t)
			rt.synced = true
			rt.pods = test.pods(rt.pods)

			assert.Empty(t, rt.update(""))
			time.Sleep(2 * rolloutStallTimeout)
			assert.Equal(t, test.want, rt.update(""))
			rt.heal.AssertExpectations(t)
		})
	}
}

// A revert makes a stalled replica stale. The rollout replaces it, and the
// stall time starts again.
func TestUpdateRedisesPodsResetsTheWaitOnAReplacedStaleReplica(t *testing.T) {
	rt := newRolloutTest(t)
	key := passwordKey(rt.rf)

	assert.Empty(t, rt.update(""))
	time.Sleep(2 * rolloutStallTimeout)
	assert.Equal(t, "rollout waiting on pod rfr-test-1 for more than 0m: not synced with the master", rt.update(""))

	// The revert puts the unsynced replica on a stale revision.
	rt.pods[1].Labels[appsv1.ControllerRevisionHashLabelKey] = "old"
	rt.heal.On("ResizePodInPlace", rt.rf, "rfr-test-1", "new", rfservice.AutoscalerLimits{}).Once().Return(rfservice.ResizeResult{Action: rfservice.ResizeRecreate}, nil)
	rt.heal.On("DeletePod", "rfr-test-1", rt.rf).Once().Return(nil)
	assert.Empty(t, rt.update(""))
	_, tracked := rt.handler.rolloutWaits.Load(key)
	assert.False(t, tracked)

	// The stall time of the replacement starts at this wait.
	rt.pods[1].UID = "r2"
	rt.pods[1].Labels[appsv1.ControllerRevisionHashLabelKey] = "new"
	assert.Empty(t, rt.update(""))
	v, tracked := rt.handler.rolloutWaits.Load(key)
	assert.True(t, tracked)
	assert.Equal(t, "r2", string(v.(rolloutWait).uid))
	rt.heal.AssertExpectations(t)
}

func TestUpdateRedisesPodsKeepsAnEarlierMessage(t *testing.T) {
	rt := newRolloutTest(t)

	assert.Equal(t, "maxmemory kept at 100mb", rt.update("maxmemory kept at 100mb"))
	time.Sleep(2 * rolloutStallTimeout)
	assert.Equal(t, "maxmemory kept at 100mb; rollout waiting on pod rfr-test-1 for more than 0m: not synced with the master",
		rt.update("maxmemory kept at 100mb"))
}

func TestUpdateRedisesPodsKeepsWaitingAcrossErrors(t *testing.T) {
	for _, step := range []string{"update revision", "pods"} {
		t.Run(step, func(t *testing.T) {
			rt := newRolloutTest(t)
			assert.Empty(t, rt.update(""))
			time.Sleep(2 * rolloutStallTimeout)

			// A failed lookup is returned and does not restart the wait.
			err := errors.New("lookup failed")
			if step == "pods" {
				rt.podsErr = err
			} else {
				rt.revErr = err
			}
			rt.rf.Status = redisfailoverv1.RedisFailoverStatus{}
			assert.Equal(t, err, rt.handler.UpdateRedisesPods(rt.rf))
			assert.Empty(t, rt.rf.Status.Message)

			rt.podsErr, rt.revErr = nil, nil
			assert.Equal(t, "rollout waiting on pod rfr-test-1 for more than 0m: not synced with the master", rt.update(""))
		})
	}
}

func TestRolloutWaitString(t *testing.T) {
	assert.Equal(t, "pod rfr-test-1 is not ready", (&rolloutWait{pod: "rfr-test-1", reason: "not ready"}).String())
	assert.Equal(t, "2 of 3 pods exist", (&rolloutWait{reason: "2 of 3 pods exist"}).String())
}

// unschedulablePod is a Pending pod on the old revision that the scheduler
// cannot place.
func unschedulablePod() corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "rfr-test-0", UID: "p", Labels: map[string]string{appsv1.ControllerRevisionHashLabelKey: "old"}},
		Status: corev1.PodStatus{
			Phase:      corev1.PodPending,
			Conditions: []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable}},
		},
	}
}

// pendingModes are the modes of CheckAndHeal, each with its own gate on the
// running redis pods.
var pendingModes = map[string]func(rf *redisfailoverv1.RedisFailover){
	"sentinel": func(rf *redisfailoverv1.RedisFailover) {
		enabled := true
		rf.Spec.Sentinel.Enabled = &enabled
	},
	"operator-managed": func(rf *redisfailoverv1.RedisFailover) {
		enabled := false
		rf.Spec.Sentinel.Enabled = &enabled
	},
	"bootstrapping": func(rf *redisfailoverv1.RedisFailover) {
		rf.Spec.BootstrapNode = &redisfailoverv1.BootstrapSettings{Host: "10.9.9.9"}
	},
}

// pendingTest runs CheckAndHeal on a RedisFailover whose redis pods do not
// all run.
type pendingTest struct {
	rf      *redisfailoverv1.RedisFailover
	pods    []corev1.Pod
	ss      *appsv1.StatefulSet
	podsErr error
	ssErr   error
	check   *mRFService.RedisFailoverCheck
	heal    *mRFService.RedisFailoverHeal
	k8s     *mK8SService.Services
	logger  *warnLogger
	handler *RedisFailoverHandler
}

// newPendingTest has the pod first, and replicas-1 more pods that are
// Pending on the update revision. IsRedisRunning, and IsRedisRunningQuorum
// out of bootstrap mode, report false once.
func newPendingTest(mode string, replicas int32, first corev1.Pod) *pendingTest {
	pt := &pendingTest{
		rf: &redisfailoverv1.RedisFailover{
			ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "testns"},
			Spec:       redisfailoverv1.RedisFailoverSpec{Redis: redisfailoverv1.RedisSettings{Replicas: replicas}},
		},
		pods:   []corev1.Pod{first},
		ss:     &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Generation: 2}, Status: appsv1.StatefulSetStatus{ObservedGeneration: 2, UpdateRevision: "new"}},
		check:  &mRFService.RedisFailoverCheck{},
		heal:   &mRFService.RedisFailoverHeal{},
		k8s:    noAppliedSecret(&mK8SService.Services{}),
		logger: &warnLogger{},
	}
	pendingModes[mode](pt.rf)
	for i := int32(1); i < replicas; i++ {
		pt.pods = append(pt.pods, corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("rfr-test-%d", i), Labels: map[string]string{appsv1.ControllerRevisionHashLabelKey: "new"}},
			Status:     corev1.PodStatus{Phase: corev1.PodPending},
		})
	}
	pt.heal.On("ApplyPassword", mock.Anything, mock.Anything, mock.Anything).Maybe().Return(true, false, nil)
	pt.heal.On("ApplySentinelPassword", mock.Anything, mock.Anything).Maybe().Return(true, nil)
	pt.k8s.On("GetStatefulSetPods", "testns", "rfr-test").Maybe().Return(func(string, string) (*corev1.PodList, error) {
		return &corev1.PodList{Items: pt.pods}, pt.podsErr
	})
	pt.k8s.On("GetStatefulSet", "testns", "rfr-test").Maybe().Return(func(string, string) (*appsv1.StatefulSet, error) {
		return pt.ss, pt.ssErr
	})
	pt.k8s.On("UpdateRedisFailoverStatus", mock.Anything, "testns", pt.rf, mock.Anything).Maybe().Return()
	pt.check.On("IsRedisRunning", pt.rf).Once().Return(false)
	if !pt.rf.Bootstrapping() {
		pt.check.On("IsRedisRunningQuorum", pt.rf).Once().Return(false)
	}
	pt.handler = NewRedisFailoverHandler(Config{}, &mRFService.RedisFailoverClient{}, pt.check, pt.heal, pt.k8s, metrics.Dummy, pt.logger)
	return pt
}

// expectDelete expects the delete of the stale pod rfr-test-0 with the UID
// of the listed pod, and returns err.
func (pt *pendingTest) expectDelete(err error) {
	pt.k8s.On("DeletePodWithUID", "testns", "rfr-test-0", types.UID("p")).Once().Return(err)
}

// gateMessage is the status message of a reconcile that the gate on the
// running redis pods ends.
func (pt *pendingTest) gateMessage() string {
	if pt.rf.Bootstrapping() {
		return "not all replicas running"
	}
	return "redis quorum not running"
}

// A stale pod that the scheduler cannot place never ran, so CheckAndHeal
// deletes it before the gates on the running pods. The StatefulSet then
// creates it on the update revision. With one or two replicas, the pod leaves
// fewer pods running than a quorum, and bootstrapping needs all pods.
func TestCheckAndHealDeletesAnUnschedulableStalePod(t *testing.T) {
	for mode := range pendingModes {
		for _, replicas := range []int32{1, 2} {
			t.Run(fmt.Sprintf("%s with %d replicas", mode, replicas), func(t *testing.T) {
				pt := newPendingTest(mode, replicas, unschedulablePod())
				pt.expectDelete(nil)
				require.NoError(t, pt.handler.CheckAndHeal(pt.rf))
				assert.Equal(t, redisfailoverv1.NotHealthyState, pt.rf.Status.State)
				assert.Equal(t, pt.gateMessage(), pt.rf.Status.Message)
				pt.k8s.AssertExpectations(t)
				pt.check.AssertExpectations(t)
			})
		}
	}
}

// An error of the step is a warning only. The gate on the running pods then
// decides as without the step, so a webhook that denies the delete does not
// block the election.
func TestCheckAndHealGoesOnAfterAnErrorOfTheStalePodStep(t *testing.T) {
	fail := errors.New("boom")
	pods := corev1.Resource("pods")
	tests := map[string]struct {
		change  func(pt *pendingTest)
		warning bool
	}{
		"pods":        {func(pt *pendingTest) { pt.podsErr = fail }, true},
		"StatefulSet": {func(pt *pendingTest) { pt.ssErr = fail }, true},
		"delete denied": {func(pt *pendingTest) {
			pt.expectDelete(apierrors.NewForbidden(pods, "rfr-test-0", errors.New("denied by a webhook")))
		}, true},
		// The pod is gone, or the StatefulSet created it again with another
		// UID. Both are no failure of the step.
		"pod deleted by another client": {func(pt *pendingTest) { pt.expectDelete(apierrors.NewNotFound(pods, "rfr-test-0")) }, false},
		"pod created again": {func(pt *pendingTest) {
			pt.expectDelete(apierrors.NewConflict(pods, "rfr-test-0", errors.New("the UID in the precondition does not match")))
		}, false},
	}
	for name, test := range tests {
		for mode := range pendingModes {
			t.Run(name+" in "+mode, func(t *testing.T) {
				pt := newPendingTest(mode, 1, unschedulablePod())
				test.change(pt)
				require.NoError(t, pt.handler.CheckAndHeal(pt.rf))
				assert.Equal(t, redisfailoverv1.NotHealthyState, pt.rf.Status.State)
				assert.Equal(t, pt.gateMessage(), pt.rf.Status.Message)
				assert.Equal(t, test.warning, len(pt.logger.warnings) == 1, pt.logger.warnings)
				pt.check.AssertExpectations(t)
				pt.k8s.AssertExpectations(t)
			})
		}
	}
}

// With 3 replicas, one Pending pod leaves a quorum running. The step deletes
// the stale pod, and the reconcile goes on to the next gate.
func TestCheckAndHealGoesOnWithAQuorumAndAPendingPod(t *testing.T) {
	for _, mode := range []string{"sentinel", "operator-managed"} {
		t.Run(mode, func(t *testing.T) {
			pt := newPendingTest(mode, 3, unschedulablePod())
			for i := 1; i < 3; i++ {
				pt.pods[i].Status.Phase = corev1.PodRunning
			}
			fail := errors.New("no masters")
			pt.check.ExpectedCalls = nil
			pt.check.On("IsRedisRunning", pt.rf).Once().Return(false)
			pt.check.On("IsRedisRunningQuorum", pt.rf).Once().Return(true)
			pt.expectDelete(nil)
			if mode == "sentinel" {
				pt.check.On("IsSentinelRunningQuorum", pt.rf).Once().Return(false)
				require.NoError(t, pt.handler.CheckAndHeal(pt.rf))
				assert.Equal(t, "sentinel quorum not running", pt.rf.Status.Message)
			} else {
				pt.heal.On("AbortOrphanedFailover", mock.Anything, mock.Anything).Maybe().Return(false, nil)
				pt.check.On("GetNumberMasters", pt.rf).Once().Return(0, fail)
				require.ErrorIs(t, pt.handler.CheckAndHeal(pt.rf), fail)
			}
			pt.check.AssertExpectations(t)
			pt.k8s.AssertExpectations(t)
		})
	}
}

func TestCheckAndHealKeepsOtherPendingPods(t *testing.T) {
	pod := func(change func(pod *corev1.Pod)) corev1.Pod {
		p := unschedulablePod()
		change(&p)
		return p
	}
	started := func(cs corev1.ContainerStatus) func(pod *corev1.Pod) {
		return func(pod *corev1.Pod) {
			cs.Name = "redis"
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{cs}
		}
	}
	tests := map[string]corev1.Pod{
		"running": pod(func(p *corev1.Pod) { p.Status.Phase = corev1.PodRunning }),
		"on the update revision": pod(func(p *corev1.Pod) {
			p.Labels[appsv1.ControllerRevisionHashLabelKey] = "new"
		}),
		"terminating":                 pod(func(p *corev1.Pod) { p.DeletionTimestamp = &metav1.Time{Time: time.Now()} }),
		"scheduled":                   pod(func(p *corev1.Pod) { p.Status.Conditions[0].Status = corev1.ConditionTrue }),
		"scheduling gated":            pod(func(p *corev1.Pod) { p.Status.Conditions[0].Reason = corev1.PodReasonSchedulingGated }),
		"without condition":           pod(func(p *corev1.Pod) { p.Status.Conditions = nil }),
		"a running container":         pod(started(corev1.ContainerStatus{State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}})),
		"a terminated container":      pod(started(corev1.ContainerStatus{State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}}})),
		"a container that ran before": pod(started(corev1.ContainerStatus{LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}}})),
		"a restarted container":       pod(started(corev1.ContainerStatus{RestartCount: 1})),
		"an init container that ran": pod(func(p *corev1.Pod) {
			p.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "init", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}}}}
		}),
	}
	for name, first := range tests {
		for mode := range pendingModes {
			t.Run(name+" in "+mode, func(t *testing.T) {
				pt := newPendingTest(mode, 1, first)
				require.NoError(t, pt.handler.CheckAndHeal(pt.rf))
				assert.Equal(t, pt.gateMessage(), pt.rf.Status.Message)
				pt.k8s.AssertNotCalled(t, "DeletePodWithUID", mock.Anything, mock.Anything, mock.Anything)
				pt.check.AssertExpectations(t)
			})
		}
	}

	// Before the StatefulSet controller sees a change of the spec, the update
	// revision in its status can be the one before.
	for name, ss := range map[string]*appsv1.StatefulSet{
		"status of an older generation": {ObjectMeta: metav1.ObjectMeta{Generation: 3}, Status: appsv1.StatefulSetStatus{ObservedGeneration: 2, UpdateRevision: "new"}},
		"no update revision":            {},
		"no StatefulSet":                nil,
	} {
		t.Run(name, func(t *testing.T) {
			pt := newPendingTest("sentinel", 1, unschedulablePod())
			pt.ss = ss
			require.NoError(t, pt.handler.CheckAndHeal(pt.rf))
			pt.k8s.AssertNotCalled(t, "DeletePodWithUID", mock.Anything, mock.Anything, mock.Anything)
			pt.check.AssertExpectations(t)
		})
	}
}

// Without a Pending pod that the step can delete, it reads no StatefulSet.
func TestCheckAndHealReadsNoStatefulSetWithoutACandidate(t *testing.T) {
	first := unschedulablePod()
	first.Status.Phase = corev1.PodRunning
	pt := newPendingTest("sentinel", 2, first)
	require.NoError(t, pt.handler.CheckAndHeal(pt.rf))
	pt.k8s.AssertNotCalled(t, "GetStatefulSet", mock.Anything, mock.Anything)
	pt.check.AssertExpectations(t)
}

// With all pods running, the step makes no call beyond its gate, and the
// gates on a quorum make none.
func TestCheckAndHealMakesNoExtraCallsWhenAllRun(t *testing.T) {
	for mode := range pendingModes {
		t.Run(mode, func(t *testing.T) {
			pt := newPendingTest(mode, 1, unschedulablePod())
			pt.check.ExpectedCalls = nil
			pt.check.On("IsRedisRunning", pt.rf).Once().Return(true)
			assert.True(t, pt.handler.redisPodsRunning(pt.rf))
			pt.k8s.AssertNotCalled(t, "GetStatefulSetPods", mock.Anything, mock.Anything)
			pt.k8s.AssertNotCalled(t, "GetStatefulSet", mock.Anything, mock.Anything)
			pt.k8s.AssertNotCalled(t, "DeletePodWithUID", mock.Anything, mock.Anything, mock.Anything)
			pt.check.AssertExpectations(t)
		})
	}

	// The reconcile passes the gates on the redis pods without a quorum check.
	// The strict mock fails on a call of IsRedisRunningQuorum.
	pt := newPendingTest("sentinel", 1, unschedulablePod())
	pt.check.ExpectedCalls = nil
	pt.check.On("IsRedisRunning", pt.rf).Once().Return(true)
	pt.check.On("IsSentinelRunningQuorum", pt.rf).Once().Return(false)
	require.NoError(t, pt.handler.CheckAndHeal(pt.rf))
	assert.Equal(t, "sentinel quorum not running", pt.rf.Status.Message)
	pt.check.AssertExpectations(t)

	pt = newPendingTest("operator-managed", 1, unschedulablePod())
	fail := errors.New("no masters")
	pt.check.ExpectedCalls = nil
	pt.check.On("IsRedisRunning", pt.rf).Once().Return(true)
	pt.check.On("GetNumberMasters", pt.rf).Once().Return(0, fail)
	pt.heal.On("AbortOrphanedFailover", mock.Anything, mock.Anything).Maybe().Return(false, nil)
	require.ErrorIs(t, pt.handler.CheckAndHeal(pt.rf), fail)
	pt.check.AssertExpectations(t)
}
