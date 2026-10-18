package redisfailover

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
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
	c.On("CheckSentinelQuorum", rf).Maybe().Return(2, errors.New("no quorum"))
	h.On("SetOldestAsMaster", rf).Maybe().Return(nil)
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
	for _, sip := range smSentinels {
		c.On("CheckSentinelMonitor", sip, s.master, mock.Anything).Maybe().Return(errors.New("sentinel monitoring another master"))
		c.On("GetSentinelFailover", sip).Maybe().Return(live, s.liveErr)
		h.On("NewSentinelMonitor", sip, s.master, rf).Maybe().Return(nil)
		c.On("CheckSentinelNumberInMemory", sip, rf).Maybe().Return(nil)
		c.On("CheckSentinelSlavesNumberInMemory", sip, rf).Maybe().Return(nil)
		h.On("SetSentinelCustomConfig", sip, rf).Maybe().Return(nil)
	}
	err := st.handler.CheckAndHeal(rf)
	// The heal reads the pods again after the wait, and two masters are an
	// error.
	if s.podsErr == nil && s.nMasters < 2 && s.nMastersErr == nil && s.masterErr == nil && s.passwordErr == nil && s.liveErr == nil {
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
		"the pod list fails":              interrupted(smReconcile{nMasters: 1, master: smPod1, named: orphaned.named, podsErr: boom}),
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
		start  smReconcile
		// waits is a time after the start that still waits, and heals the
		// time that heals.
		waits, heals time.Duration
	}{
		"a lowered customConfig waits for the live timeouts": {config: short, start: withLive(orphaned, &redis.SentinelFailover{DownAfter: 5 * time.Second, Timeout: 10 * time.Second}, nil), waits: 14 * time.Second, heals: 15 * time.Second},
		"a live read error uses the customConfig":            {config: short, start: withLive(orphaned, nil, errors.New("boom")), waits: 2 * time.Second, heals: 3 * time.Second},
		"a Sentinel without the timeouts":                    {config: short, start: withLive(orphaned, &redis.SentinelFailover{Timeout: 10 * time.Second}, nil), waits: 2 * time.Second, heals: 3 * time.Second},
		"shorter live timeouts use the customConfig":         {start: withLive(orphaned, &redis.SentinelFailover{DownAfter: time.Second, Timeout: 2 * time.Second}, nil), waits: 14 * time.Second, heals: 15 * time.Second},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			st := newSentinelMasterTest()
			st.rf.Spec.Sentinel.CustomConfig = test.config
			st.assertWait(t, st.run(t, test.start))
			st.clock = st.clock.Add(test.waits)
			st.assertWait(t, st.run(t, orphaned))
			st.clock = st.clock.Add(test.heals - test.waits)
			st.assertHeal(t, st.run(t, orphaned))
		})
	}
	t.Run("the bound stays for the wait", func(t *testing.T) {
		st := newSentinelMasterTest()
		st.assertWait(t, st.run(t, orphaned))
		// The customConfig changes and the live read fails during the wait.
		st.rf.Spec.Sentinel.CustomConfig = short
		st.clock = st.clock.Add(10 * time.Second)
		st.assertWait(t, st.run(t, withLive(orphaned, nil, errors.New("boom"))))
	})
	t.Run("no Sentinel list", func(t *testing.T) {
		st := newSentinelMasterTest()
		st.check.On("GetSentinelsIPs", st.rf).Return(nil, errors.New("boom"))
		assert.Equal(t, 15*time.Second, st.handler.sentinelMasterLimit(st.rf, st.handler.logger))
	})
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
