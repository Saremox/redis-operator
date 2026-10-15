package redisfailover

import (
	"errors"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/metrics"
	mMetrics "github.com/saremox/redis-operator/mocks/metrics"
	mRFService "github.com/saremox/redis-operator/mocks/operator/redisfailover/service"
	mK8SService "github.com/saremox/redis-operator/mocks/service/k8s"
	"github.com/saremox/redis-operator/service/redis"
)

// sentinelCheckRecorder records the status of each Sentinel check per
// indicator and IP.
type sentinelCheckRecorder struct {
	metrics.Recorder
	status map[string]string
}

func (r *sentinelCheckRecorder) RecordSentinelCheck(_, _, indicator, instance, status string) {
	r.status[indicator+" "+instance] = status
}

type sentinelResetTest struct {
	rf      *redisfailoverv1.RedisFailover
	check   *mRFService.RedisFailoverCheck
	heal    *mRFService.RedisFailoverHeal
	metrics *sentinelCheckRecorder
	logger  *warnLogger
	handler *RedisFailoverHandler
	clock   time.Time
}

func newSentinelResetTest() *sentinelResetTest {
	st := &sentinelResetTest{
		rf:      &redisfailoverv1.RedisFailover{ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "testns"}},
		check:   &mRFService.RedisFailoverCheck{},
		heal:    &mRFService.RedisFailoverHeal{},
		metrics: &sentinelCheckRecorder{Recorder: metrics.Dummy, status: map[string]string{}},
		logger:  &warnLogger{},
		clock:   time.Unix(1000, 0),
	}
	st.handler = NewRedisFailoverHandler(Config{}, &mRFService.RedisFailoverClient{}, st.check, st.heal, &mK8SService.Services{}, st.metrics, st.logger)
	st.handler.now = func() time.Time { return st.clock }
	return st
}

// run runs checkAndHealSentinels as the callers do.
func (st *sentinelResetTest) run(sentinels []string) error {
	heal := st.handler.startSentinelHeal(st.rf)
	defer st.handler.saveSentinelResets(heal, sentinels)
	return st.handler.checkAndHealSentinels(st.rf, sentinels, heal)
}

// reconcile runs checkAndHealSentinels. Each Sentinel in mismatch has a wrong
// number of Sentinels. It returns the Sentinels that got a reset.
func (st *sentinelResetTest) reconcile(t *testing.T, sentinels []string, mismatch ...string) []string {
	t.Helper()
	st.check.ExpectedCalls, st.check.Calls = nil, nil
	st.heal.ExpectedCalls, st.heal.Calls = nil, nil
	for _, s := range sentinels {
		var err error
		if slices.Contains(mismatch, s) {
			err = errors.New("sentinels in memory mismatch")
		}
		st.check.On("CheckSentinelNumberInMemory", s, st.rf).Once().Return(err)
		st.check.On("CheckSentinelSlavesNumberInMemory", s, st.rf).Once().Return(nil)
		st.heal.On("RestoreSentinel", s).Maybe().Return(nil)
		st.check.On("GetSentinelFailover", mock.Anything).Maybe().Return(redis.SentinelFailover{}, nil)
		st.heal.On("SetSentinelCustomConfig", s, st.rf).Once().Return(nil)
	}
	require.NoError(t, st.run(sentinels))
	st.check.AssertExpectations(t)
	var reset []string
	for _, c := range st.heal.Calls {
		if c.Method == "RestoreSentinel" {
			reset = append(reset, c.Arguments.String(0))
		}
	}
	return reset
}

func TestSentinelResetHoldOff(t *testing.T) {
	st := newSentinelResetTest()
	sentinels := []string{"10.0.1.1", "10.0.1.2"}
	a, b := sentinels[0], sentinels[1]
	mismatchMetric := metrics.SENTINEL_NUMBER_IN_MEMORY_MISMATCH + " " + a

	assert.Equal(t, []string{a}, st.reconcile(t, sentinels, a))

	// The hold-off of a does not stop the reset of b.
	st.clock = st.clock.Add(10 * time.Second)
	st.metrics.status = map[string]string{}
	var requeues []time.Duration
	st.handler.requeue = func(key string, after time.Duration) {
		assert.Equal(t, "testns/test", key)
		requeues = append(requeues, after)
	}
	assert.Equal(t, []string{b}, st.reconcile(t, sentinels, a, b))
	assert.Equal(t, metrics.STATUS_UNHEALTHY, st.metrics.status[mismatchMetric])
	// The reconcile at the end of the hold-off resets a.
	assert.Equal(t, []time.Duration{15 * time.Second}, requeues)

	// A mismatch that stays after the hold-off gets a reset again.
	st.clock = st.clock.Add(15 * time.Second)
	assert.Equal(t, []string{a}, st.reconcile(t, sentinels, a, b))
}

// TestSentinelResetOnceInAReconcile covers the replica check after a reset,
// which clears the replicas. It does not reset the Sentinel again.
func TestSentinelResetOnceInAReconcile(t *testing.T) {
	st := newSentinelResetTest()
	s := "10.0.1.1"
	st.check.On("CheckSentinelNumberInMemory", s, st.rf).Once().Return(errors.New("sentinels in memory mismatch"))
	st.check.On("CheckSentinelSlavesNumberInMemory", s, st.rf).Once().Return(errors.New("slaves in memory mismatch"))
	st.heal.On("RestoreSentinel", s).Once().Return(nil)
	st.check.On("GetSentinelFailover", mock.Anything).Maybe().Return(redis.SentinelFailover{}, nil)
	st.heal.On("SetSentinelCustomConfig", s, st.rf).Once().Return(nil)

	require.NoError(t, st.run([]string{s}))

	st.heal.AssertExpectations(t)
	assert.Equal(t, metrics.STATUS_UNHEALTHY, st.metrics.status[metrics.REDIS_SLAVES_NUMBER_IN_MEMORY_MISMATCH+" "+s])
}

// TestSentinelResetFailedResetIsNotRecorded checks that a failed reset starts
// no hold-off, and that the hold-off of the other Sentinels stays.
func TestSentinelResetFailedResetIsNotRecorded(t *testing.T) {
	st := newSentinelResetTest()
	a, b := "10.0.1.1", "10.0.1.2"
	st.reconcile(t, []string{a, b}, a)

	st.clock = st.clock.Add(5 * time.Second)
	st.check.ExpectedCalls, st.check.Calls = nil, nil
	st.heal.ExpectedCalls, st.heal.Calls = nil, nil
	st.check.On("CheckSentinelNumberInMemory", a, st.rf).Once().Return(nil)
	st.check.On("CheckSentinelNumberInMemory", b, st.rf).Once().Return(errors.New("sentinels in memory mismatch"))
	st.heal.On("RestoreSentinel", b).Once().Return(errors.New("ERR unknown subcommand"))
	st.check.On("GetSentinelFailover", mock.Anything).Maybe().Return(redis.SentinelFailover{}, nil)

	require.Error(t, st.run([]string{a, b}))

	assert.Equal(t, redisfailoverv1.NotHealthyState, st.rf.Status.State)
	assert.Equal(t, "unable to restore sentinel", st.rf.Status.Message)
	v, _ := st.handler.sentinelResets.Load(failoverKey(st.rf))
	assert.Equal(t, map[string]time.Time{a: st.clock.Add(-5 * time.Second)}, v)
}

// TestUnreachableSentinelSkipsTheLaterSteps checks that a Sentinel that does
// not answer gets no reset, and that the later steps do not dial it.
func TestUnreachableSentinelSkipsTheLaterSteps(t *testing.T) {
	st := newSentinelResetTest()
	gone, ok := "10.0.1.1", "10.0.1.2"
	st.check.On("CheckSentinelNumberInMemory", gone, st.rf).Once().Return(errors.New("dial tcp 10.0.1.1:26379: i/o timeout"))
	st.check.On("CheckSentinelNumberInMemory", ok, st.rf).Once().Return(nil)
	st.check.On("CheckSentinelSlavesNumberInMemory", ok, st.rf).Once().Return(nil)
	st.heal.On("SetSentinelCustomConfig", ok, st.rf).Once().Return(nil)

	err := st.run([]string{gone, ok})

	require.EqualError(t, err, "sentinel(s) 10.0.1.1 unreachable")
	assert.Equal(t, redisfailoverv1.NotHealthyState, st.rf.Status.State)
	assert.Equal(t, "sentinel(s) 10.0.1.1 unreachable", st.rf.Status.Message)
	assert.Equal(t, []string{"Skipping unreachable sentinel 10.0.1.1: dial tcp 10.0.1.1:26379: i/o timeout"}, st.logger.warnings)
	st.heal.AssertNotCalled(t, "RestoreSentinel", mock.Anything)
	st.check.AssertExpectations(t)
	st.heal.AssertExpectations(t)
	for _, property := range []string{metrics.SENTINEL_NUMBER_IN_MEMORY_MISMATCH, metrics.REDIS_SLAVES_NUMBER_IN_MEMORY_MISMATCH, metrics.APPLY_SENTINEL_CONFIG} {
		assert.Equal(t, metrics.STATUS_UNHEALTHY, st.metrics.status[property+" "+gone], property)
	}
	_, stored := st.handler.sentinelResets.Load(failoverKey(st.rf))
	assert.False(t, stored)
}

// TestSentinelCheckKubernetesErrorIsNotUnreachable covers a check that fails
// in the Kubernetes API. The Sentinel answered, so the status does not name it.
func TestSentinelCheckKubernetesErrorIsNotUnreachable(t *testing.T) {
	st := newSentinelResetTest()
	s := "10.0.1.1"
	apiErr := &url.Error{Op: "Get", URL: "https://10.96.0.1:443/api/v1/pods", Err: errors.New("i/o timeout")}
	st.check.On("CheckSentinelNumberInMemory", s, st.rf).Once().Return(nil)
	st.check.On("CheckSentinelSlavesNumberInMemory", s, st.rf).Once().Return(apiErr)
	st.heal.On("RestoreSentinel", s).Once().Return(nil)
	st.check.On("GetSentinelFailover", mock.Anything).Maybe().Return(redis.SentinelFailover{}, nil)
	st.heal.On("SetSentinelCustomConfig", s, st.rf).Once().Return(nil)

	require.NoError(t, st.run([]string{s}))

	st.heal.AssertExpectations(t)
	assert.Empty(t, st.rf.Status.Message)
}

// TestUnreachableSentinelWithoutIP checks the status of a Running Sentinel pod
// that has no IP.
func TestUnreachableSentinelWithoutIP(t *testing.T) {
	st := newSentinelResetTest()
	st.check.On("CheckSentinelNumberInMemory", "", st.rf).Once().Return(errors.New("dial tcp: i/o timeout"))

	require.EqualError(t, st.run([]string{""}), "sentinel(s) (no IP) unreachable")
}

func TestSentinelResetHoldOffDropsOldEntries(t *testing.T) {
	st := newSentinelResetTest()
	key := failoverKey(st.rf)
	old, recreated := "10.0.1.1", "10.0.1.9"

	st.reconcile(t, []string{old}, old)
	_, ok := st.handler.sentinelResets.Load(key)
	require.True(t, ok)

	// The pod got a new IP. The entry of the old IP goes.
	assert.Equal(t, []string{recreated}, st.reconcile(t, []string{recreated}, recreated))
	v, _ := st.handler.sentinelResets.Load(key)
	assert.Equal(t, map[string]time.Time{recreated: st.clock}, v)

	// An entry after the hold-off goes.
	st.clock = st.clock.Add(sentinelResetHoldOff)
	st.reconcile(t, []string{recreated})
	_, ok = st.handler.sentinelResets.Load(key)
	assert.False(t, ok)

	// Forget drops the entries of the RedisFailover.
	st.reconcile(t, []string{recreated}, recreated)
	mrec := &mMetrics.Recorder{}
	mrec.On("DeleteCluster", "testns", "test").Once()
	st.handler.mClient = mrec
	st.handler.Forget(key)
	_, ok = st.handler.sentinelResets.Load(key)
	assert.False(t, ok)
}
