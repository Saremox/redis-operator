package redisfailover

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/metrics"
	mRFService "github.com/saremox/redis-operator/mocks/operator/redisfailover/service"
	mK8SService "github.com/saremox/redis-operator/mocks/service/k8s"
	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
	"github.com/saremox/redis-operator/service/redis"
)

// The orphaned failover of the soak events: the old master pod-0 stops, a
// Sentinel leader promoted pod-1 and stopped, and pod-2 still replicates from
// pod-0.
const (
	smPod0 = "10.0.0.1"
	smPod1 = "10.0.0.2"
	smPod2 = "10.0.0.3"
)

var smSentinels = []string{"10.0.1.1", "10.0.1.2", "10.0.1.3"}

// smReconcile is the state in one reconcile.
type smReconcile struct {
	// nMasters is the first count of the masters. 0 runs the election.
	nMasters int
	// master is the Redis master.
	master string
	// named holds the IPs that GetSentinelMasterIPs returns.
	named []string
	// namedErr is the error of GetSentinelMasterIPs.
	namedErr error
	// podsErr is the error of the pod list.
	podsErr error
}

// smResult is what a reconcile did.
type smResult struct {
	labels    int
	replicaOf int
	monitored []string
	elected   int
}

// sentinelMasterTest runs CheckAndHeal in Sentinel mode with three Redis pods
// and three Sentinels.
type sentinelMasterTest struct {
	rf       *redisfailoverv1.RedisFailover
	pods     []corev1.Pod
	check    *mRFService.RedisFailoverCheck
	heal     *mRFService.RedisFailoverHeal
	k8s      *mK8SService.Services
	handler  *RedisFailoverHandler
	clock    time.Time
	lines    []string
	requeues []time.Duration
}

func smPod(name, ip string, master, deleting bool) corev1.Pod {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, PodIP: ip},
	}
	if master {
		pod.Labels["redisfailovers-role"] = "master"
	}
	if deleting {
		pod.DeletionTimestamp = &metav1.Time{Time: time.Unix(900, 0)}
	}
	return pod
}

func newSentinelMasterTest() *sentinelMasterTest {
	st := &sentinelMasterTest{
		rf:    newCustomConfigTestRF(),
		clock: time.Unix(1000, 0),
		// pod-0 is not ready, so the stopping master does not end the
		// reconcile before the Sentinel heal.
		pods: []corev1.Pod{
			smPod("rfr-test-0", smPod0, true, true),
			smPod("rfr-test-1", smPod1, false, false),
			smPod("rfr-test-2", smPod2, false, false),
		},
	}
	st.rf.Spec.Sentinel.Enabled = new(true)
	st.rf.Spec.Sentinel.Replicas = 3
	st.rf.Spec.Redis.Replicas = 3
	st.k8s = noAppliedSecret(&mK8SService.Services{})
	st.check = &mRFService.RedisFailoverCheck{}
	st.heal = &mRFService.RedisFailoverHeal{}
	st.handler = NewRedisFailoverHandler(Config{}, &mRFService.RedisFailoverClient{}, st.check, st.heal, st.k8s, metrics.Dummy, levelLogger{lines: &st.lines})
	st.handler.now = func() time.Time { return st.clock }
	st.handler.requeue = func(key string, after time.Duration) {
		if key == "testns/test" {
			st.requeues = append(st.requeues, after)
		}
	}
	return st
}

func (st *sentinelMasterTest) run(t *testing.T, s smReconcile) smResult {
	t.Helper()
	for _, m := range []*mock.Mock{&st.check.Mock, &st.heal.Mock, &st.k8s.Mock} {
		m.ExpectedCalls, m.Calls = nil, nil
	}
	st.lines, st.requeues = nil, nil
	c, h, k, rf := st.check, st.heal, st.k8s, st.rf
	noAppliedSecret(k)
	k.On("GetStatefulSetPods", rf.Namespace, rfservice.GetRedisName(rf)).Maybe().Return(&corev1.PodList{Items: st.pods}, s.podsErr)
	k.On("UpdateRedisFailoverStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe().Return()
	h.On("ApplyPassword", mock.Anything, mock.Anything, mock.Anything).Maybe().Return(true, false, nil)
	h.On("ApplySentinelPassword", mock.Anything, mock.Anything).Maybe().Return(true, nil)
	c.On("IsRedisRunning", rf).Maybe().Return(true)
	c.On("IsSentinelRunningQuorum", rf).Maybe().Return(true)
	c.On("GetNumberMasters", rf).Maybe().Return(s.nMasters, nil)
	// The election of a master when the Sentinels have no quorum.
	c.On("GetMaxRedisPodTime", rf).Maybe().Return(time.Hour, nil)
	c.On("CheckSentinelQuorum", rf).Maybe().Return(2, errors.New("no quorum"))
	h.On("SetOldestAsMaster", rf).Maybe().Return(nil)
	c.On("GetMasterIP", rf).Maybe().Return(s.master, nil)
	c.On("GetSentinelMasterIPs", rf).Maybe().Return(s.named, s.namedErr)
	c.On("CheckAllSlavesFromMaster", s.master, rf).Maybe().Return(&rfservice.WrongMasterError{Pod: "rfr-test-2", IP: smPod2, Master: s.master, Has: smPod0})
	h.On("SetMasterOnAll", s.master, rf).Maybe().Return(nil)
	c.On("GetRedisesIPs", rf).Maybe().Return([]string{s.master}, nil)
	h.On("SetRedisCustomConfig", s.master, rf).Maybe().Return(nil)
	c.On("GetStatefulSetUpdateRevision", rf).Maybe().Return("1", nil)
	c.On("GetRedisesSlavesPods", rf).Maybe().Return([]string{}, nil)
	c.On("GetRedisesMasterPod", rf).Maybe().Return("rfr-test-1", nil)
	c.On("GetRedisRevisionHash", "rfr-test-1", rf).Maybe().Return("1", nil)
	c.On("GetSentinelsIPs", rf).Maybe().Return(smSentinels, nil)
	for _, sip := range smSentinels {
		c.On("CheckSentinelMonitor", sip, s.master, mock.Anything).Maybe().Return(errors.New("sentinel monitoring another master"))
		c.On("GetSentinelFailover", sip).Maybe().Return(redis.SentinelFailover{}, nil)
		h.On("NewSentinelMonitor", sip, s.master, rf).Maybe().Return(nil)
		c.On("CheckSentinelNumberInMemory", sip, rf).Maybe().Return(nil)
		c.On("CheckSentinelSlavesNumberInMemory", sip, rf).Maybe().Return(nil)
		h.On("SetSentinelCustomConfig", sip, rf).Maybe().Return(nil)
	}
	err := st.handler.CheckAndHeal(rf)
	// The heal reads the pods again after the wait.
	if s.podsErr == nil {
		require.NoError(t, err)
	}
	var res smResult
	for _, call := range c.Calls {
		if call.Method == "CheckAllSlavesFromMaster" {
			res.labels++
		}
	}
	for _, call := range h.Calls {
		switch call.Method {
		case "SetMasterOnAll":
			res.replicaOf++
		case "NewSentinelMonitor":
			res.monitored = append(res.monitored, call.Arguments.String(0))
		case "SetOldestAsMaster":
			res.elected++
		}
	}
	return res
}

func (st *sentinelMasterTest) assertWait(t *testing.T, res smResult) {
	t.Helper()
	assert.Zero(t, res.labels, "no label change")
	assert.Zero(t, res.replicaOf, "no REPLICAOF")
	assert.Empty(t, res.monitored, "no SENTINEL MONITOR")
	assert.Equal(t, redisfailoverv1.NotHealthyState, st.rf.Status.State)
	assert.Equal(t, sentinelMasterMsg, st.rf.Status.Message)
}

func (st *sentinelMasterTest) assertHeal(t *testing.T, res smResult) {
	t.Helper()
	assert.Equal(t, 1, res.labels)
	assert.Equal(t, 1, res.replicaOf)
	assert.Equal(t, smSentinels, res.monitored)
	assert.NotEqual(t, sentinelMasterMsg, st.rf.Status.Message)
}

const (
	smWaitLine = "The Sentinels give pod rfr-test-0 (10.0.0.1) as the master, but the Redis master is 10.0.0.2, so the operator does not change the roles for up to 15s"
	smEndLine  = "warning: The Sentinels give pod rfr-test-0 (10.0.0.1) as the master for 15s, but the Redis master is 10.0.0.2, so the operator heals the roles and the Sentinels"
)

// orphaned is the state of the soak events: all the Sentinels give the old
// master pod-0, which stops, and Redis has pod-1 as the only master.
var orphaned = smReconcile{nMasters: 1, master: smPod1, named: []string{smPod0, smPod0, smPod0}}

// TestSentinelMasterWaitHoldsTheHeal covers the soak events. The master label
// and the replicas stay away from pod-1, so pod-1 accepts no writes that a
// second failover loses.
func TestSentinelMasterWaitHoldsTheHeal(t *testing.T) {
	st := newSentinelMasterTest()
	res := st.run(t, orphaned)
	st.assertWait(t, res)
	assert.Equal(t, []string{"info: " + smWaitLine}, st.lines)
	assert.Equal(t, []time.Duration{time.Second}, st.requeues)

	st.clock = st.clock.Add(time.Second)
	res = st.run(t, orphaned)
	st.assertWait(t, res)
	assert.Equal(t, []string{"debug: " + smWaitLine}, st.lines)

	// The Sentinels fail over to pod-2, and Redis agrees.
	st.clock = st.clock.Add(9 * time.Second)
	res = st.run(t, smReconcile{nMasters: 1, master: smPod2, named: []string{smPod2, smPod2, smPod2}})
	assert.Equal(t, 1, res.labels)
	assert.NotContains(t, st.lines, "info: "+smWaitLine)
	_, waiting := st.handler.sentinelMasterWaits.Load("testns/test")
	assert.False(t, waiting)
}

// TestSentinelMasterWaitEndsAtTheBound covers Sentinels that do not fail
// over. After down-after-milliseconds plus failover-timeout, the operator
// heals as before, and it logs the end one time.
func TestSentinelMasterWaitEndsAtTheBound(t *testing.T) {
	st := newSentinelMasterTest()
	st.assertWait(t, st.run(t, orphaned))

	st.clock = st.clock.Add(14900 * time.Millisecond)
	st.assertWait(t, st.run(t, orphaned))
	assert.Equal(t, []time.Duration{100 * time.Millisecond}, st.requeues)

	st.clock = st.clock.Add(100 * time.Millisecond)
	res := st.run(t, orphaned)
	st.assertHeal(t, res)
	assert.Contains(t, st.lines, smEndLine)

	st.clock = st.clock.Add(time.Second)
	res = st.run(t, orphaned)
	st.assertHeal(t, res)
	for _, line := range st.lines {
		assert.NotContains(t, line, "The Sentinels give pod", "one log line for the end")
	}
}

// TestSentinelMasterWaitUsesTheSentinelConfig covers the bound from the
// Sentinel customConfig.
func TestSentinelMasterWaitUsesTheSentinelConfig(t *testing.T) {
	st := newSentinelMasterTest()
	st.rf.Spec.Sentinel.CustomConfig = []string{"down-after-milliseconds 1000", "failover-timeout 2000"}
	st.assertWait(t, st.run(t, orphaned))
	st.clock = st.clock.Add(3 * time.Second)
	st.assertHeal(t, st.run(t, orphaned))
}

// TestSentinelMasterWaitRestartsForAnotherPair covers a change of the master
// or of the pod that the Sentinels give. It is another failover, so it has its
// own bound.
func TestSentinelMasterWaitRestartsForAnotherPair(t *testing.T) {
	st := newSentinelMasterTest()
	st.assertWait(t, st.run(t, orphaned))

	st.clock = st.clock.Add(10 * time.Second)
	other := smReconcile{nMasters: 1, master: smPod2, named: orphaned.named}
	st.assertWait(t, st.run(t, other))
	assert.Equal(t, []string{"info: The Sentinels give pod rfr-test-0 (10.0.0.1) as the master, but the Redis master is 10.0.0.3, so the operator does not change the roles for up to 15s"}, st.lines)

	st.clock = st.clock.Add(10 * time.Second)
	st.assertWait(t, st.run(t, other))

	st.clock = st.clock.Add(5 * time.Second)
	res := st.run(t, other)
	assert.Equal(t, 1, res.labels)
	assert.NotEqual(t, sentinelMasterMsg, st.rf.Status.Message)

	// The Sentinels give another pod after the end of a wait.
	st.clock = st.clock.Add(time.Second)
	st.assertWait(t, st.run(t, smReconcile{nMasters: 1, master: smPod2, named: []string{smPod1, smPod1, smPod1}}))
}

// TestSentinelMasterWaitNotNeeded covers the cases without a wait. The heal
// runs as before.
func TestSentinelMasterWaitNotNeeded(t *testing.T) {
	tests := map[string]smReconcile{
		// A planned failover after +switch-master, and the steady state.
		"the Sentinels agree with Redis": {named: []string{smPod1, smPod1, smPod1}},
		// The leader gives the promoted replica before the end of the failover.
		"the leader gives the promoted replica": {named: []string{smPod0, smPod1, smPod0}},
		// A forced delete: no pod has the old IP.
		"the old master is not a pod":    {named: []string{"10.0.0.9", "10.0.0.9", "10.0.0.9"}},
		"the Sentinels have no master":   {named: []string{"127.0.0.1", "127.0.0.1", "127.0.0.1"}},
		"a new Sentinel":                 {named: []string{smPod0, smPod0, "127.0.0.1"}},
		"less than a quorum answers":     {named: []string{smPod0}},
		"no Sentinel answers":            {},
		"the Sentinels give an empty IP": {named: []string{"", "", ""}},
		"the Sentinels cannot be read":   {namedErr: errors.New("boom")},
		"the pods cannot be read":        {named: orphaned.named, podsErr: errors.New("boom")},
	}
	for name, s := range tests {
		t.Run(name, func(t *testing.T) {
			st := newSentinelMasterTest()
			// A pod without an IP must not match an empty IP.
			st.pods = append(st.pods, smPod("rfr-test-3", "", false, false))
			s.nMasters, s.master = 1, smPod1
			res := st.run(t, s)
			if s.podsErr == nil {
				st.assertHeal(t, res)
			} else {
				assert.Equal(t, 1, res.labels)
			}
			assert.NotEqual(t, sentinelMasterMsg, st.rf.Status.Message)
			assert.Empty(t, st.requeues)
		})
	}
}

// TestSentinelMasterWaitAfterAnElection covers the election of a master in
// the same reconcile. The Sentinels give the old master until the operator
// monitors the elected master, so a wait would only delay that monitor.
func TestSentinelMasterWaitAfterAnElection(t *testing.T) {
	st := newSentinelMasterTest()
	res := st.run(t, smReconcile{nMasters: 0, master: smPod1, named: orphaned.named})
	assert.Equal(t, 1, res.elected)
	st.assertHeal(t, res)
}

// TestSentinelMasterWaitForget covers the removal of a RedisFailover.
func TestSentinelMasterWaitForget(t *testing.T) {
	st := newSentinelMasterTest()
	st.assertWait(t, st.run(t, orphaned))
	_, waiting := st.handler.sentinelMasterWaits.Load("testns/test")
	require.True(t, waiting)
	st.handler.mClient = metrics.Dummy
	st.handler.Forget("testns/test")
	_, waiting = st.handler.sentinelMasterWaits.Load("testns/test")
	assert.False(t, waiting)
}
