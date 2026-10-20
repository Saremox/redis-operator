package redisfailover

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

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
	// promote: no Sentinel can fail over, so the election promotes master.
	promote bool
	// promoteErr is the error of PromoteBestReplica.
	promoteErr error
	// localhost: each Redis pod has 127.0.0.1 as master.
	localhost bool
	// master is the Redis master.
	master string
	// named holds the IPs that GetSentinelMasterIPs returns.
	named []string
	// namedErr is the error of GetSentinelMasterIPs.
	namedErr error
	// podsErr is the error of the pod list.
	podsErr error
	// noSentinelQuorum: the Sentinel pods do not run a quorum.
	noSentinelQuorum bool
	// nMastersErr, masterErr and passwordErr are the errors of
	// GetNumberMasters, GetMasterIP and ApplyPassword.
	nMastersErr, masterErr, passwordErr error
	// mode is "bootstrap" or "operator" for a reconcile in another mode.
	mode string
	// live is the reply of GetSentinelFailover, with the live timeouts.
	// Without it, the Sentinels give the defaults.
	live *redis.SentinelFailover
	// liveErr is the error of GetSentinelFailover.
	liveErr error
	// lives holds the reply of GetSentinelFailover of single Sentinels.
	lives map[string]smLive
}

// smLive is a reply of GetSentinelFailover.
type smLive struct {
	f   redis.SentinelFailover
	err error
}

// smResult is what a reconcile did.
type smResult struct {
	err error
	// timeoutReads counts the reads of the Sentinel timeouts.
	timeoutReads int
	labels       int
	replicaOf    int
	monitored    []string
	elected      int
}

// sentinelMasterTest runs CheckAndHeal in Sentinel mode with three Redis pods
// and three Sentinels.
type sentinelMasterTest struct {
	rf   *redisfailoverv1.RedisFailover
	pods []corev1.Pod
	// sentinelUIDs holds the UID of a Sentinel pod by IP, "uid-<IP>" without
	// an entry.
	sentinelUIDs map[string]types.UID
	check        *mRFService.RedisFailoverCheck
	heal         *mRFService.RedisFailoverHeal
	k8s          *mK8SService.Services
	handler      *RedisFailoverHandler
	clock        time.Time
	lines        []string
	requeues     []time.Duration
	// viaHandle runs Handle with a successful Ensure, not CheckAndHeal.
	viaHandle bool
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
	c, h, k, rf := noUnknownSentinelAddrs(st.check), st.heal, st.k8s, st.rf
	switch s.mode {
	case "bootstrap":
		rf.Spec.BootstrapNode = &redisfailoverv1.BootstrapSettings{Host: "10.0.9.9", Port: "6379", AllowSentinels: true}
		defer func() { rf.Spec.BootstrapNode = nil }()
	case "operator":
		rf.Spec.Sentinel.Enabled = new(false)
		defer func() { rf.Spec.Sentinel.Enabled = new(true) }()
	}
	live := redis.SentinelFailover{Timeout: 10 * time.Second, DownAfter: 5 * time.Second}
	if s.live != nil {
		live = *s.live
	}
	noAppliedSecret(k)
	k.On("GetStatefulSetPods", rf.Namespace, rfservice.GetRedisName(rf)).Maybe().Return(&corev1.PodList{Items: st.pods}, s.podsErr)
	k.On("UpdateRedisFailoverStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe().Return()
	if s.passwordErr != nil {
		st.handler.passwords.Delete("testns/test")
		h.On("ApplyPassword", mock.Anything, mock.Anything, mock.Anything).Maybe().Return(false, false, s.passwordErr)
	}
	h.On("ApplyPassword", mock.Anything, mock.Anything, mock.Anything).Maybe().Return(true, false, nil)
	h.On("ApplySentinelPassword", mock.Anything, mock.Anything).Maybe().Return(true, nil)
	// In another mode, the reconcile ends at the first check.
	c.On("IsRedisRunning", rf).Maybe().Return(s.mode == "")
	c.On("IsRedisRunningQuorum", rf).Maybe().Return(false)
	c.On("IsSentinelRunningQuorum", rf).Maybe().Return(!s.noSentinelQuorum)
	c.On("GetNumberMasters", rf).Maybe().Return(s.nMasters, s.nMastersErr)
	// The election of a master when the Sentinels have no quorum.
	c.On("GetMaxRedisPodTime", rf).Maybe().Return(time.Hour, nil)
	if s.promote {
		c.On("CheckSentinelQuorum", rf).Maybe().Return(0, nil)
		c.On("CheckIfMasterLocalhost", rf).Maybe().Return(false, nil)
		c.On("CheckSentinelsCannotFailover", rf).Maybe().Return(true, nil)
		c.On("GetBestReplicaForPromotion", rf).Maybe().Return(&rfservice.ReplicaInfo{PodName: "rfr-test-1", IP: s.master}, nil)
		h.On("PromoteBestReplica", s.master, rf).Maybe().Return(s.promoteErr)
	}
	if s.localhost {
		c.On("CheckSentinelQuorum", rf).Maybe().Return(0, nil)
		c.On("CheckIfMasterLocalhost", rf).Maybe().Return(true, nil)
	}
	c.On("CheckSentinelQuorum", rf).Maybe().Return(2, errors.New("no quorum"))
	h.On("SetOldestAsMaster", rf).Maybe().Return(s.master, nil)
	c.On("GetMasterIP", rf).Maybe().Return(s.master, s.masterErr)
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
	sentinelPods := &corev1.PodList{}
	for _, sip := range smSentinels {
		uid, ok := st.sentinelUIDs[sip]
		if !ok {
			uid = types.UID("uid-" + sip)
		}
		sentinelPods.Items = append(sentinelPods.Items, corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{UID: uid},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning, PodIP: sip},
		})
	}
	k.On("GetDeploymentPods", rf.Namespace, rfservice.GetSentinelName(rf)).Maybe().Return(sentinelPods, nil)
	for _, sip := range smSentinels {
		c.On("CheckSentinelMonitor", sip, s.master, mock.Anything).Maybe().Return(errors.New("sentinel monitoring another master"))
		reply := smLive{f: live, err: s.liveErr}
		if l, ok := s.lives[sip]; ok {
			reply = l
		}
		c.On("GetSentinelFailover", sip).Maybe().Return(reply.f, reply.err)
		h.On("NewSentinelMonitor", sip, s.master, rf).Maybe().Return(nil)
		c.On("CheckSentinelNumberInMemory", sip, rf).Maybe().Return(nil)
		c.On("CheckSentinelSlavesNumberInMemory", sip, rf).Maybe().Return(nil)
		h.On("SetSentinelCustomConfig", sip, rf).Maybe().Return(nil)
	}
	var err error
	if st.viaHandle {
		err = st.handle(rf)
	} else {
		err = st.handler.CheckAndHeal(rf)
	}
	// The heal reads the pods again after the wait, and two masters are an
	// error.
	if s.podsErr == nil && s.namedErr == nil && s.nMasters < 2 && s.nMastersErr == nil && s.masterErr == nil && s.passwordErr == nil && s.liveErr == nil && s.promoteErr == nil {
		require.NoError(t, err)
	}
	res := smResult{err: err}
	for _, call := range c.Calls {
		switch call.Method {
		case "CheckAllSlavesFromMaster":
			res.labels++
		case "GetSentinelFailover":
			res.timeoutReads++
		}
	}
	for _, call := range h.Calls {
		switch call.Method {
		case "SetMasterOnAll":
			res.replicaOf++
		case "NewSentinelMonitor":
			res.monitored = append(res.monitored, call.Arguments.String(0))
		case "SetOldestAsMaster", "PromoteBestReplica":
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

// handle runs Handle on rf, the production path, with a successful Ensure.
func (st *sentinelMasterTest) handle(rf *redisfailoverv1.RedisFailover) error {
	rfs := &mRFService.RedisFailoverClient{}
	rfs.On("EnsureNotPresentRedisService", mock.Anything).Maybe().Return(nil)
	rfs.On("EnsureNotPresentSentinelResources", mock.Anything).Maybe().Return(nil)
	for _, m := range []string{"EnsureRedisConfigMap", "EnsureRedisMasterService", "EnsureRedisReadinessConfigMap", "EnsureRedisService", "EnsureRedisShutdownConfigMap", "EnsureRedisSlaveService", "EnsureRedisStatefulset", "EnsureSentinelConfigMap", "EnsureSentinelDeployment", "EnsureSentinelService"} {
		rfs.On(m, mock.Anything, mock.Anything, mock.Anything).Maybe().Return(nil)
	}
	st.handler.rfService = rfs
	st.k8s.On("GetConfigMap", mock.Anything, mock.Anything).Maybe().Return(nil, apierrors.NewNotFound(corev1.Resource("configmaps"), "state"))
	rf.Finalizers = []string{redisFailoverFinalizer}
	return st.handler.Handle(context.Background(), rf)
}

// assertEnd checks the end of a wait: the roles heal. A Sentinel with a read
// error gets no SENTINEL MONITOR, as in the failover hold.
func (st *sentinelMasterTest) assertEnd(t *testing.T, res smResult) {
	t.Helper()
	assert.Equal(t, 1, res.labels)
	assert.Equal(t, 1, res.replicaOf)
	assert.NotEqual(t, sentinelMasterMsg, st.rf.Status.Message)
}

func (st *sentinelMasterTest) assertHeal(t *testing.T, res smResult) {
	t.Helper()
	assert.Equal(t, 1, res.labels)
	assert.Equal(t, 1, res.replicaOf)
	assert.Equal(t, smSentinels, res.monitored)
	assert.NotEqual(t, sentinelMasterMsg, st.rf.Status.Message)
}

const (
	smWaitLine = "The Sentinels give 10.0.0.1 as the master, but the Redis master is 10.0.0.2, so the operator does not change the roles for up to 15s"
	smEndLine  = "warning: The Sentinels give 10.0.0.1 as the master for 15s, but the Redis master is 10.0.0.2, so the operator heals the roles and the Sentinels"
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

	// The ended wait stays: each later round heals and starts no wait.
	for range 3 {
		st.clock = st.clock.Add(time.Second)
		res = st.run(t, orphaned)
		st.assertHeal(t, res)
		for _, line := range st.lines {
			assert.NotContains(t, line, "The Sentinels give", "one log line for the end")
		}
	}
}

// TestSentinelMasterWaitThroughHandle covers the wait in Handle, the path of
// the controller. Each requeue is a Handle, and it keeps the start of the
// wait.
func TestSentinelMasterWaitThroughHandle(t *testing.T) {
	st := newSentinelMasterTest()
	st.viaHandle = true
	st.assertWait(t, st.run(t, orphaned))
	v, ok := st.handler.sentinelMasterWaits.Load("testns/test")
	require.True(t, ok)
	first, _ := v.(sentinelMasterWait)

	st.clock = st.clock.Add(5 * time.Second)
	st.assertWait(t, st.run(t, orphaned))
	assert.Equal(t, []string{"debug: " + smWaitLine}, st.lines)
	v, ok = st.handler.sentinelMasterWaits.Load("testns/test")
	require.True(t, ok, "Handle dropped a wait that it checked")
	second, _ := v.(sentinelMasterWait)
	assert.Equal(t, first.since, second.since)

	st.clock = st.clock.Add(10 * time.Second)
	st.assertHeal(t, st.run(t, orphaned))
}

// TestSentinelMasterWaitForAGoneAddress covers Sentinels that give the address
// of a pod that is gone and never fail over it. The wait ends at the bound,
// and the heal runs as without the wait.
func TestSentinelMasterWaitForAGoneAddress(t *testing.T) {
	gone := smReconcile{nMasters: 1, master: smPod1, named: []string{"10.0.0.9", "10.0.0.9", "10.0.0.9"}}
	st := newSentinelMasterTest()
	st.assertWait(t, st.run(t, gone))
	st.clock = st.clock.Add(14 * time.Second)
	st.assertWait(t, st.run(t, gone))
	st.clock = st.clock.Add(time.Second)
	st.assertHeal(t, st.run(t, gone))
}

// TestSentinelMasterWaitUsesTheSentinelConfig covers the bound from the
// Sentinel customConfig, after the Sentinels applied it.
func TestSentinelMasterWaitUsesTheSentinelConfig(t *testing.T) {
	st := newSentinelMasterTest()
	st.rf.Spec.Sentinel.CustomConfig = []string{"down-after-milliseconds 1000", "failover-timeout 2000"}
	st.assertWait(t, st.run(t, withLive(orphaned, &redis.SentinelFailover{DownAfter: time.Second, Timeout: 2 * time.Second}, nil)))
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
	assert.Equal(t, []string{"info: The Sentinels give 10.0.0.1 as the master, but the Redis master is 10.0.0.3, so the operator does not change the roles for up to 15s"}, st.lines)

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
		// The leader and one more Sentinel give the promoted replica.
		"a quorum gives the promoted replica": {named: []string{smPod0, smPod1, smPod1}},
		"no IP has a quorum":                  {named: []string{smPod0, smPod2, smPod1}},
		"the Sentinels have no master":        {named: []string{"127.0.0.1", "127.0.0.1", "127.0.0.1"}},
		"an IPv6 loopback":                    {named: []string{"::1", "::1", "::1"}},
		"an unspecified address":              {named: []string{"0.0.0.0", "0.0.0.0", "0.0.0.0"}},
		"not an IP":                           {named: []string{"redis-0", "redis-0", "redis-0"}},
		"new Sentinels":                       {named: []string{smPod0, "127.0.0.1", "127.0.0.1"}},
		"less than a quorum answers":          {named: []string{smPod0}},
		"no Sentinel answers":                 {},
		"the Sentinels give an empty IP":      {named: []string{"", "", ""}},
	}
	for name, s := range tests {
		t.Run(name, func(t *testing.T) {
			st := newSentinelMasterTest()
			// A pod without an IP must not match an empty IP.
			st.pods = append(st.pods, smPod("rfr-test-3", "", false, false))
			s.nMasters, s.master = 1, smPod1
			st.assertHeal(t, st.run(t, s))
			assert.NotEqual(t, sentinelMasterMsg, st.rf.Status.Message)
			assert.Empty(t, st.requeues)
		})
	}
}

// TestSentinelMasterWaitForAQuorum covers the answers that give a quorum for
// another pod. Such a quorum can fail over the old master later, also when
// the other Sentinels give the Redis master.
func TestSentinelMasterWaitForAQuorum(t *testing.T) {
	x, y, m := smPod0, smPod2, smPod1
	tests := map[string]struct {
		named []string
		wait  bool
		// replicas is spec.sentinel.replicas, 3 when it is 0.
		replicas int32
	}{
		// A forced delete, or a new IP of the recreated pod: no pod has X.
		"[gone X, gone X, M]": {named: []string{"10.0.0.9", "10.0.0.9", m}, wait: true},
		// A Sentinel in deletion still votes until it stops. Its answer is
		// in the list, and the quorum of the spec of 3 Sentinels is 2.
		"[X (in deletion), X, M, M]":       {named: []string{x, x, m, m}, wait: true},
		"[unknown (in deletion), X, M, M]": {named: []string{"", x, m, m}, wait: true},
		// A Sentinel without an answer can be one more vote for X.
		"[X, unknown, M]":             {named: []string{x, "", m}, wait: true},
		"[unknown, unknown, M]":       {named: []string{"", "", m}},
		"[M, unknown, M]":             {named: []string{m, "", m}},
		"[X, unknown, M, M, unknown]": {named: []string{x, "", m, m, ""}, replicas: 5, wait: true},
		"[X, unknown, M, M, M]":       {named: []string{x, "", m, m, m}, replicas: 5},
		"[X, X, X]":                   {named: []string{x, x, x}, wait: true},
		"[X, X, M]":                   {named: []string{x, x, m}, wait: true},
		"[X, M, X]":                   {named: []string{x, m, x}, wait: true},
		"[X, X, 127.0.0.1]":           {named: []string{x, x, "127.0.0.1"}, wait: true},
		"[X, X, empty]":               {named: []string{x, x, ""}, wait: true},
		"[X, X] of three":             {named: []string{x, x}, wait: true},
		"[X, X, M, M] of four":        {named: []string{x, x, m, m}, wait: true},
		"[X, M, M]":                   {named: []string{x, m, m}},
		"[X, Y, M]":                   {named: []string{x, y, m}},
		"[M, M, M]":                   {named: []string{m, m, m}},
		"[X, 127.0.0.1, 127.0.0.1]":   {named: []string{x, "127.0.0.1", "127.0.0.1"}},
		"[X] of three":                {named: []string{x}},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			st := newSentinelMasterTest()
			if test.replicas != 0 {
				st.rf.Spec.Sentinel.Replicas = test.replicas
			}
			res := st.run(t, smReconcile{nMasters: 1, master: m, named: test.named})
			if test.wait {
				st.assertWait(t, res)
			} else {
				st.assertHeal(t, res)
			}
		})
	}
	// During a rollout, more Sentinels than the spec run, and two IPs can
	// have a quorum. The wait is the same in each reconcile.
	t.Run("two IPs with a quorum", func(t *testing.T) {
		for range 20 {
			st := newSentinelMasterTest()
			st.assertWait(t, st.run(t, smReconcile{nMasters: 1, master: m, named: []string{y, y, x, x}}))
			assert.Equal(t, []string{"info: " + smWaitLine}, st.lines)
		}
	})
	t.Run("two IPs with a quorum, one not a pod", func(t *testing.T) {
		st := newSentinelMasterTest()
		st.assertWait(t, st.run(t, smReconcile{nMasters: 1, master: m, named: []string{"10.0.0.0", "10.0.0.0", x, x}}))
		assert.Equal(t, []string{"info: The Sentinels give 10.0.0.0 as the master, but the Redis master is 10.0.0.2, so the operator does not change the roles for up to 15s"}, st.lines)
	})
}

// TestSentinelMasterWaitStopsOnAReadError covers a failed list of the Sentinel
// pods. The failed read can hide a quorum for another address, so the
// reconcile ends before it changes the roles.
func TestSentinelMasterWaitStopsOnAReadError(t *testing.T) {
	for name, s := range map[string]smReconcile{
		"the Sentinel pods": {nMasters: 1, master: smPod1, namedErr: errors.New("boom")},
	} {
		t.Run(name, func(t *testing.T) {
			st := newSentinelMasterTest()
			st.assertWait(t, st.run(t, orphaned))
			st.clock = st.clock.Add(time.Second)
			res := st.run(t, s)
			require.EqualError(t, res.err, "boom")
			assert.Zero(t, res.labels, "no label change")
			assert.Zero(t, res.replicaOf, "no REPLICAOF")
			assert.Empty(t, res.monitored, "no SENTINEL MONITOR")
			assert.Equal(t, redisfailoverv1.NotHealthyState, st.rf.Status.State)
			assert.Equal(t, "unable to check the master of the Sentinels", st.rf.Status.Message)
			// A reconcile without the check ends the wait.
			_, waiting := st.handler.sentinelMasterWaits.Load("testns/test")
			assert.False(t, waiting)
		})
	}
}

// TestSentinelMasterWaitWithAQuorumOfAnswers covers a node drain: one of the
// three Sentinels does not answer, and the two others are a quorum.
func TestSentinelMasterWaitWithAQuorumOfAnswers(t *testing.T) {
	st := newSentinelMasterTest()
	st.assertWait(t, st.run(t, smReconcile{nMasters: 1, master: smPod1, named: []string{smPod0, smPod0}}))
}

// TestSentinelMasterWaitStartsAgainAfterAnInterruption covers a reconcile
// that ends before the wait, or reads no answer for it. The pair of IPs that
// comes back after more than the bound from the first start gets a new bound.
func TestSentinelMasterWaitStartsAgainAfterAnInterruption(t *testing.T) {
	interrupted := func(s smReconcile) func(*testing.T, *sentinelMasterTest) {
		return func(t *testing.T, st *sentinelMasterTest) { st.run(t, s) }
	}
	// handled runs Handle, which returns wantErr before the checks.
	handled := func(wantErr string, edit func(*redisfailoverv1.RedisFailover), k8s func(*mK8SService.Services)) func(*testing.T, *sentinelMasterTest) {
		return func(t *testing.T, st *sentinelMasterTest) {
			st.k8s.ExpectedCalls, st.k8s.Calls = nil, nil
			noAppliedSecret(st.k8s)
			if k8s != nil {
				k8s(st.k8s)
			}
			st.k8s.On("GetConfigMap", mock.Anything, mock.Anything).Maybe().Return(nil, apierrors.NewNotFound(corev1.Resource("configmaps"), "state"))
			st.k8s.On("UpdateRedisFailoverStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe().Return()
			rfs := &mRFService.RedisFailoverClient{}
			rfs.On("EnsureNotPresentRedisService", mock.Anything).Maybe().Return(errors.New("ensure"))
			st.handler.rfService = rfs
			rf := st.rf.DeepCopy()
			rf.Finalizers = []string{redisFailoverFinalizer}
			if edit != nil {
				edit(rf)
			}
			err := st.handler.Handle(context.Background(), rf)
			if wantErr == "" {
				require.NoError(t, err)
			} else {
				require.EqualError(t, err, wantErr)
			}
		}
	}
	boom := errors.New("boom")
	for name, interrupt := range map[string]func(*testing.T, *sentinelMasterTest){
		"no Sentinel quorum":              interrupted(smReconcile{nMasters: 1, master: smPod1, named: orphaned.named, noSentinelQuorum: true}),
		"no master":                       interrupted(smReconcile{nMasters: 0, master: smPod1, named: orphaned.named}),
		"two masters":                     interrupted(smReconcile{nMasters: 2, master: smPod1, named: orphaned.named}),
		"the count of the masters fails":  interrupted(smReconcile{nMasters: 1, master: smPod1, named: orphaned.named, nMastersErr: boom}),
		"the read of the master fails":    interrupted(smReconcile{nMasters: 1, master: smPod1, named: orphaned.named, masterErr: boom}),
		"the read of the Sentinels fails": interrupted(smReconcile{nMasters: 1, master: smPod1, namedErr: boom}),
		"the password fails":              interrupted(smReconcile{nMasters: 1, master: smPod1, named: orphaned.named, passwordErr: boom}),
		"bootstrap mode":                  interrupted(smReconcile{nMasters: 1, master: smPod1, named: orphaned.named, mode: "bootstrap"}),
		"operator-managed mode":           interrupted(smReconcile{nMasters: 1, master: smPod1, named: orphaned.named, mode: "operator"}),
		"the skip annotation": handled("", func(rf *redisfailoverv1.RedisFailover) {
			rf.Annotations = map[string]string{skipReconcileAnnotation: "true"}
		}, nil),
		"the finalizer write fails": handled("boom", func(rf *redisfailoverv1.RedisFailover) { rf.Finalizers = nil }, func(k *mK8SService.Services) {
			k.On("PatchRedisFailoverFinalizers", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(boom)
		}),
		"the autoscaler state fails": handled("autoscaler state: boom", func(rf *redisfailoverv1.RedisFailover) {
			rf.Spec.Redis.Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}
			memoryMax := resource.MustParse("2Gi")
			rf.Spec.Redis.Autoscaling = &redisfailoverv1.AutoscalingSettings{Mode: redisfailoverv1.AutoscalingModeRecommend, MemoryMax: &memoryMax}
		}, func(k *mK8SService.Services) {
			k.On("GetConfigMap", mock.Anything, mock.Anything).Return(nil, boom)
		}),
		"Ensure fails": handled("ensure", nil, nil),
	} {
		t.Run(name, func(t *testing.T) {
			st := newSentinelMasterTest()
			st.assertWait(t, st.run(t, orphaned))
			st.clock = st.clock.Add(10 * time.Second)
			interrupt(t, st)
			_, waiting := st.handler.sentinelMasterWaits.Load("testns/test")
			assert.False(t, waiting)

			st.clock = st.clock.Add(time.Second)
			st.assertWait(t, st.run(t, orphaned))
			assert.Equal(t, []string{"info: " + smWaitLine}, st.lines)
			// 16s after the first wait, 5s after the second.
			st.clock = st.clock.Add(5 * time.Second)
			st.assertWait(t, st.run(t, orphaned))
		})
	}
}

// TestSentinelMasterWaitUsesTheLiveTimeouts covers the timeouts of the
// running Sentinels. They keep their old values until the heal applies the
// customConfig, and the wait stops that heal.
func TestSentinelMasterWaitUsesTheLiveTimeouts(t *testing.T) {
	short := []string{"down-after-milliseconds 1000", "failover-timeout 2000"}
	tests := map[string]struct {
		config []string
		each   smReconcile
		// waits is a time after the start that still waits, and heals the
		// time that heals.
		waits, heals time.Duration
	}{
		"a lowered customConfig waits for the live timeouts": {config: short, each: withLive(orphaned, &redis.SentinelFailover{DownAfter: 5 * time.Second, Timeout: 10 * time.Second}, nil), waits: 14 * time.Second, heals: 15 * time.Second},
		"Sentinels that never answer use the customConfig":   {config: short, each: withLive(orphaned, nil, errors.New("boom")), waits: 2 * time.Second, heals: 3 * time.Second},
		"a Sentinel without the timeouts":                    {config: short, each: withLive(orphaned, &redis.SentinelFailover{Timeout: 10 * time.Second}, nil), waits: 2 * time.Second, heals: 3 * time.Second},
		"shorter live timeouts use the customConfig":         {each: withLive(orphaned, &redis.SentinelFailover{DownAfter: time.Second, Timeout: 2 * time.Second}, nil), waits: 14 * time.Second, heals: 15 * time.Second},
		// One Sentinel flags the master down late, another leads the
		// failover with its long failover-timeout.
		"mixed timeouts add the largest values": {each: withLives(orphaned, map[string]smLive{
			smSentinels[0]: {f: redis.SentinelFailover{DownAfter: 100 * time.Second, Timeout: time.Second}},
			smSentinels[1]: {f: redis.SentinelFailover{DownAfter: time.Second, Timeout: 100 * time.Second}},
			smSentinels[2]: {f: redis.SentinelFailover{DownAfter: time.Second, Timeout: 100 * time.Second}},
		}), waits: 199 * time.Second, heals: 200 * time.Second},
		// The wait ends at the bound of the known values.
		"a Sentinel that never answers": {each: withLives(orphaned, map[string]smLive{
			smSentinels[0]: {err: errors.New("boom")},
			smSentinels[1]: {f: redis.SentinelFailover{DownAfter: 20 * time.Second, Timeout: 20 * time.Second}},
			smSentinels[2]: {f: redis.SentinelFailover{DownAfter: 20 * time.Second, Timeout: 20 * time.Second}},
		}), waits: 39 * time.Second, heals: 40 * time.Second},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			st := newSentinelMasterTest()
			st.rf.Spec.Sentinel.CustomConfig = test.config
			st.assertWait(t, st.run(t, test.each))
			st.clock = st.clock.Add(test.waits)
			st.assertWait(t, st.run(t, test.each))
			st.clock = st.clock.Add(test.heals - test.waits)
			st.assertEnd(t, st.run(t, test.each))
		})
	}
	t.Run("the customConfig counts at the start only", func(t *testing.T) {
		st := newSentinelMasterTest()
		st.assertWait(t, st.run(t, orphaned))
		st.rf.Spec.Sentinel.CustomConfig = short
		st.clock = st.clock.Add(10 * time.Second)
		st.assertWait(t, st.run(t, orphaned))
	})
	t.Run("no Sentinel list", func(t *testing.T) {
		st := newSentinelMasterTest()
		st.k8s.On("GetDeploymentPods", st.rf.Namespace, rfservice.GetSentinelName(st.rf)).Return(nil, errors.New("boom"))
		wait := sentinelMasterWait{downAfter: 5 * time.Second, failoverTimeout: 10 * time.Second, known: map[types.UID]bool{}}
		st.handler.readSentinelTimeouts(st.rf, &wait, st.handler.logger)
		assert.Equal(t, 5*time.Second, wait.downAfter)
		assert.Equal(t, 10*time.Second, wait.failoverTimeout)
	})
}

// TestSentinelMasterWaitReadsTheTimeoutsAgain covers a Sentinel that gives
// its timeouts only in a later check of the wait.
func TestSentinelMasterWaitReadsTheTimeoutsAgain(t *testing.T) {
	failing := withLives(orphaned, map[string]smLive{smSentinels[2]: {err: errors.New("boom")}})
	answers := func(downAfter, failoverTimeout time.Duration) smReconcile {
		return withLives(orphaned, map[string]smLive{smSentinels[2]: {f: redis.SentinelFailover{DownAfter: downAfter, Timeout: failoverTimeout}}})
	}
	t.Run("a later answer makes the bound longer", func(t *testing.T) {
		st := newSentinelMasterTest()
		st.assertWait(t, st.run(t, failing))
		st.clock = st.clock.Add(5 * time.Second)
		st.assertWait(t, st.run(t, answers(30*time.Second, 10*time.Second)))
		st.clock = st.clock.Add(34 * time.Second)
		st.assertWait(t, st.run(t, orphaned))
		st.clock = st.clock.Add(time.Second)
		st.assertHeal(t, st.run(t, orphaned))
	})
	t.Run("a later smaller answer keeps the bound", func(t *testing.T) {
		st := newSentinelMasterTest()
		big := redis.SentinelFailover{DownAfter: 20 * time.Second, Timeout: 20 * time.Second}
		start := withLives(orphaned, map[string]smLive{smSentinels[0]: {f: big}, smSentinels[1]: {f: big}, smSentinels[2]: {err: errors.New("boom")}})
		st.assertWait(t, st.run(t, start))
		st.clock = st.clock.Add(5 * time.Second)
		st.assertWait(t, st.run(t, answers(time.Second, time.Second)))
		st.clock = st.clock.Add(34 * time.Second)
		st.assertWait(t, st.run(t, orphaned))
	})
	t.Run("the reads stop after the first answer", func(t *testing.T) {
		st := newSentinelMasterTest()
		res := st.run(t, failing)
		st.assertWait(t, res)
		assert.Equal(t, 3, res.timeoutReads)
		st.clock = st.clock.Add(time.Second)
		res = st.run(t, failing)
		st.assertWait(t, res)
		assert.Equal(t, 1, res.timeoutReads, "only the Sentinel without an answer")
		st.clock = st.clock.Add(time.Second)
		st.assertWait(t, st.run(t, orphaned))
		st.clock = st.clock.Add(time.Second)
		res = st.run(t, orphaned)
		st.assertWait(t, res)
		assert.Zero(t, res.timeoutReads)
	})
	t.Run("no reads after the end", func(t *testing.T) {
		st := newSentinelMasterTest()
		st.assertWait(t, st.run(t, failing))
		st.clock = st.clock.Add(15 * time.Second)
		st.assertEnd(t, st.run(t, failing))
		// A late answer does not start the ended wait again.
		st.clock = st.clock.Add(time.Second)
		st.assertHeal(t, st.run(t, answers(30*time.Second, 10*time.Second)))
	})
}

// TestSentinelMasterWaitKnowsSentinelsByUID covers a Sentinel pod that is
// replaced in a wait and gets the IP of the old pod. The new pod can have
// other timeouts.
func TestSentinelMasterWaitKnowsSentinelsByUID(t *testing.T) {
	third := smSentinels[2]
	answers := func(downAfter, failoverTimeout time.Duration) smReconcile {
		return withLives(orphaned, map[string]smLive{third: {f: redis.SentinelFailover{DownAfter: downAfter, Timeout: failoverTimeout}}})
	}
	t.Run("a new pod with the old IP is read again", func(t *testing.T) {
		st := newSentinelMasterTest()
		st.assertWait(t, st.run(t, orphaned))
		st.clock = st.clock.Add(5 * time.Second)
		st.sentinelUIDs = map[string]types.UID{third: "uid-new"}
		res := st.run(t, answers(30*time.Second, 10*time.Second))
		st.assertWait(t, res)
		assert.Equal(t, 1, res.timeoutReads)
		st.clock = st.clock.Add(34 * time.Second)
		st.assertWait(t, st.run(t, orphaned))
		st.clock = st.clock.Add(time.Second)
		st.assertHeal(t, st.run(t, orphaned))
	})
	t.Run("the same pod is not read again", func(t *testing.T) {
		st := newSentinelMasterTest()
		st.assertWait(t, st.run(t, orphaned))
		st.clock = st.clock.Add(5 * time.Second)
		res := st.run(t, answers(30*time.Second, 10*time.Second))
		st.assertWait(t, res)
		assert.Zero(t, res.timeoutReads)
		st.clock = st.clock.Add(10 * time.Second)
		st.assertHeal(t, st.run(t, orphaned))
	})
	t.Run("the values of a replaced pod stay", func(t *testing.T) {
		st := newSentinelMasterTest()
		st.assertWait(t, st.run(t, answers(30*time.Second, 10*time.Second)))
		st.clock = st.clock.Add(5 * time.Second)
		st.sentinelUIDs = map[string]types.UID{third: "uid-new"}
		st.assertWait(t, st.run(t, answers(time.Second, time.Second)))
		st.clock = st.clock.Add(34 * time.Second)
		st.assertWait(t, st.run(t, orphaned))
	})
}

func withLives(s smReconcile, lives map[string]smLive) smReconcile {
	s.lives = lives
	return s
}

func withLive(s smReconcile, live *redis.SentinelFailover, err error) smReconcile {
	s.live, s.liveErr = live, err
	return s
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

// The election of the soak event: a SENTINEL RESET removed the replicas from
// the Sentinels, and the old master pod with the address smGone is gone.
const (
	smGone        = "10.0.0.9"
	smSkipLine    = "debug: The operator elected the master 10.0.0.2, so it does not wait for the Sentinels that give 10.0.0.9"
	smGoneWaitOn  = "info: The Sentinels give 10.0.0.9 as the master, but the Redis master is 10.0.0.2, so the operator does not change the roles for up to 15s"
	smGoneEndLine = "warning: The Sentinels give 10.0.0.9 as the master for 15s, but the Redis master is 10.0.0.2, so the operator heals the roles and the Sentinels"
)

var (
	smElection = smReconcile{promote: true, master: smPod1, named: []string{smGone, smGone, smGone}}
	smElected  = smReconcile{nMasters: 1, master: smPod1, named: smElection.named}
)

// elect runs the reconcile of the election.
func (st *sentinelMasterTest) elect(t *testing.T) {
	t.Helper()
	require.Equal(t, 1, st.run(t, smElection).elected)
}

// TestSentinelMasterWaitSkipsTheElectedMaster covers the reconcile after an
// election. The Sentinels know no replica, so they cannot fail over the
// address that they give, and the roles, the replicas and the Sentinels heal.
func TestSentinelMasterWaitSkipsTheElectedMaster(t *testing.T) {
	st := newSentinelMasterTest()
	st.elect(t)
	for range 2 {
		st.clock = st.clock.Add(time.Second)
		res := st.run(t, smElected)
		st.assertHeal(t, res)
		assert.Contains(t, st.lines, smSkipLine)
		assert.NotContains(t, st.lines, smGoneWaitOn)
		assert.Empty(t, st.requeues)
	}
}

// TestSentinelMasterWaitSkipEndsAtTheBound covers Sentinels that give the
// old address after the bound. The bound counts from the election, so the
// wait ends at once, and the heal holds the roles for at most one bound.
func TestSentinelMasterWaitSkipEndsAtTheBound(t *testing.T) {
	st := newSentinelMasterTest()
	elected := st.clock
	st.elect(t)
	st.clock = st.clock.Add(14900 * time.Millisecond)
	st.assertHeal(t, st.run(t, smElected))
	assert.Equal(t, []string{smSkipLine}, debugLines(st.lines))

	st.clock = st.clock.Add(100 * time.Millisecond)
	st.assertHeal(t, st.run(t, smElected))
	assert.Contains(t, st.lines, smGoneEndLine)
	assert.Empty(t, debugLines(st.lines))
	v, ok := st.handler.sentinelMasterWaits.Load("testns/test")
	require.True(t, ok)
	wait, _ := v.(sentinelMasterWait)
	assert.Equal(t, elected, wait.since)

	// The ended wait stays, and logs no more lines.
	st.clock = st.clock.Add(time.Second)
	st.assertHeal(t, st.run(t, smElected))
	for _, line := range st.lines {
		assert.NotContains(t, line, "The Sentinels give")
	}
}

// debugLines returns the lines of the skip and of the wait at debug level.
func debugLines(lines []string) []string {
	var debug []string
	for _, line := range lines {
		if strings.HasPrefix(line, "debug: The operator elected") || strings.HasPrefix(line, "debug: The Sentinels give") {
			debug = append(debug, line)
		}
	}
	return debug
}

// TestSentinelMasterWaitSkipUsesTheLiveTimeouts covers the bound of the skip.
// It is the bound of the wait.
func TestSentinelMasterWaitSkipUsesTheLiveTimeouts(t *testing.T) {
	st := newSentinelMasterTest()
	st.elect(t)
	long := withLive(smElected, &redis.SentinelFailover{DownAfter: 20 * time.Second, Timeout: 20 * time.Second}, nil)
	st.clock = st.clock.Add(39 * time.Second)
	st.assertHeal(t, st.run(t, long))
	assert.Equal(t, []string{smSkipLine}, debugLines(st.lines))
	st.clock = st.clock.Add(time.Second)
	st.assertHeal(t, st.run(t, long))
	assert.Contains(t, st.lines, "warning: The Sentinels give 10.0.0.9 as the master for 40s, but the Redis master is 10.0.0.2, so the operator heals the roles and the Sentinels")
}

// TestSentinelMasterWaitSkipAfterAPartialPromotion covers a promotion that
// fails after MakeMaster: on the master label or on a replica. The elected
// master works, so the next reconcile heals without a wait.
func TestSentinelMasterWaitSkipAfterAPartialPromotion(t *testing.T) {
	for name, cause := range map[string]error{
		"a replica":        rfservice.ErrPartialReconciliation,
		"the master label": rfservice.ErrMasterLabel,
	} {
		t.Run(name, func(t *testing.T) {
			st := newSentinelMasterTest()
			partial := smElection
			partial.promoteErr = fmt.Errorf("%w: boom", cause)
			res := st.run(t, partial)
			require.ErrorIs(t, res.err, cause)
			assert.Equal(t, 1, res.elected)

			st.clock = st.clock.Add(time.Second)
			st.assertHeal(t, st.run(t, smElected))
			assert.Contains(t, st.lines, smSkipLine)
		})
	}
}

// TestSentinelMasterWaitAfterAFailedPromotion covers a promotion that fails
// before the master works, for example on MakeMaster. The operator then
// records no election.
func TestSentinelMasterWaitAfterAFailedPromotion(t *testing.T) {
	st := newSentinelMasterTest()
	failed := smElection
	failed.promoteErr = errors.New("boom")
	require.EqualError(t, st.run(t, failed).err, "boom")

	st.clock = st.clock.Add(time.Second)
	st.assertWait(t, st.run(t, smElected))
}

// TestSentinelMasterWaitSkipEnds covers the events that end the skip before
// the bound. After each, the wait applies to the elected master again.
func TestSentinelMasterWaitSkipEnds(t *testing.T) {
	for name, end := range map[string]func(*testing.T, *sentinelMasterTest){
		// Sentinels that know a replica of the other master can fail over.
		"another master": func(t *testing.T, st *sentinelMasterTest) {
			st.assertWait(t, st.run(t, smReconcile{nMasters: 1, master: smPod2, named: smElection.named}))
		},
		"the Sentinels give the elected master": func(t *testing.T, st *sentinelMasterTest) {
			st.assertHeal(t, st.run(t, smReconcile{nMasters: 1, master: smPod1, named: []string{smPod1, smPod1, smPod1}}))
			_, ok := st.handler.sentinelMasterWaits.Load("testns/test")
			assert.False(t, ok)
		},
		"a reconcile without the check": func(t *testing.T, st *sentinelMasterTest) {
			st.run(t, smReconcile{nMasters: 1, master: smPod1, named: smElection.named, noSentinelQuorum: true})
		},
		"Forget": func(_ *testing.T, st *sentinelMasterTest) {
			st.handler.mClient = metrics.Dummy
			st.handler.Forget("testns/test")
		},
	} {
		t.Run(name, func(t *testing.T) {
			st := newSentinelMasterTest()
			st.elect(t)
			st.clock = st.clock.Add(time.Second)
			end(t, st)
			st.clock = st.clock.Add(time.Second)
			st.assertWait(t, st.run(t, smElected))
			assert.Equal(t, []string{smGoneWaitOn}, st.lines)
		})
	}
}

// smSingle is the soak event with one Redis pod: the master pod restarts
// with another IP, and the Sentinels give its old address.
var smSingle = smReconcile{master: smPod1, named: smElection.named}

// newSinglePodTest returns a test with one Redis pod, after the operator made
// that pod the master.
func newSinglePodTest(t *testing.T) *sentinelMasterTest {
	t.Helper()
	st := newSentinelMasterTest()
	st.rf.Spec.Redis.Replicas = 1
	require.Equal(t, 1, st.run(t, smSingle).elected)
	v, _ := st.handler.sentinelMasterWaits.Load("testns/test")
	wait, _ := v.(sentinelMasterWait)
	require.True(t, wait.elected)
	return st
}

// TestSentinelMasterWaitSkipsTheSinglePodMaster covers the reconcile after
// the operator made the only Redis pod the master. The Sentinels have no
// other pod to promote, so the roles and the Sentinels heal without a wait.
func TestSentinelMasterWaitSkipsTheSinglePodMaster(t *testing.T) {
	st := newSinglePodTest(t)
	st.clock = st.clock.Add(time.Second)
	st.assertHeal(t, st.run(t, smElected))
	assert.Contains(t, st.lines, smSkipLine)
	assert.NotContains(t, st.lines, smGoneWaitOn)
	assert.Empty(t, st.requeues)
}

// TestSentinelMasterWaitSinglePodSkipEnds covers the end of the skip after
// the election of the only Redis pod. Then the wait applies as for any other
// master.
func TestSentinelMasterWaitSinglePodSkipEnds(t *testing.T) {
	t.Run("the bound", func(t *testing.T) {
		st := newSinglePodTest(t)
		st.clock = st.clock.Add(14900 * time.Millisecond)
		st.assertHeal(t, st.run(t, smElected))
		assert.Equal(t, []string{smSkipLine}, debugLines(st.lines))
		st.clock = st.clock.Add(100 * time.Millisecond)
		st.assertHeal(t, st.run(t, smElected))
		assert.Contains(t, st.lines, smGoneEndLine)
		assert.Empty(t, debugLines(st.lines))
	})
	for name, end := range map[string]smReconcile{
		"another master":                        {nMasters: 1, master: smPod2, named: smElection.named},
		"the Sentinels give the elected master": {nMasters: 1, master: smPod1, named: []string{smPod1, smPod1, smPod1}},
	} {
		t.Run(name, func(t *testing.T) {
			st := newSinglePodTest(t)
			st.clock = st.clock.Add(time.Second)
			st.run(t, end)
			st.clock = st.clock.Add(time.Second)
			st.assertWait(t, st.run(t, smElected))
			assert.Equal(t, []string{smGoneWaitOn}, st.lines)
		})
	}
}

// TestSentinelMasterWaitWithoutASinglePodMaster covers a reconcile with one
// Redis pod in which SetOldestAsMaster sets no master, for example while the
// master pod stops. The operator records no election, and the reconcile
// drops the wait that it did not check.
func TestSentinelMasterWaitWithoutASinglePodMaster(t *testing.T) {
	st := newSentinelMasterTest()
	st.assertWait(t, st.run(t, orphaned))
	_, ok := st.handler.sentinelMasterWaits.Load("testns/test")
	require.True(t, ok)

	st.rf.Spec.Redis.Replicas = 1
	st.clock = st.clock.Add(time.Second)
	require.Equal(t, 1, st.run(t, smReconcile{named: orphaned.named}).elected)
	_, ok = st.handler.sentinelMasterWaits.Load("testns/test")
	assert.False(t, ok)
}

// TestSentinelMasterWaitAfterAnElectionOfTheOldestPod covers the other
// elections of the oldest pod. A Sentinel can still fail over to a replica
// pod, so the operator records no election and waits.
func TestSentinelMasterWaitAfterAnElectionOfTheOldestPod(t *testing.T) {
	for name, election := range map[string]smReconcile{
		// The Sentinel quorum can come back.
		"no Sentinel quorum": smSingle,
		// A container restart keeps the pod IP, so the Sentinels can know
		// the pods as replicas.
		"127.0.0.1": {localhost: true, master: smPod1, named: smElection.named},
	} {
		t.Run(name, func(t *testing.T) {
			st := newSentinelMasterTest()
			require.Equal(t, 1, st.run(t, election).elected)
			st.clock = st.clock.Add(time.Second)
			st.assertWait(t, st.run(t, smElected))
			assert.Equal(t, []string{smGoneWaitOn}, st.lines)
		})
	}
}
