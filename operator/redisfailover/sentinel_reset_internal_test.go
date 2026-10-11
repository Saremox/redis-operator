package redisfailover

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/metrics"
	mMetrics "github.com/saremox/redis-operator/mocks/metrics"
	mRFService "github.com/saremox/redis-operator/mocks/operator/redisfailover/service"
	mK8SService "github.com/saremox/redis-operator/mocks/service/k8s"
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
	handler *RedisFailoverHandler
	clock   time.Time
}

func newSentinelResetTest() *sentinelResetTest {
	st := &sentinelResetTest{
		rf:      &redisfailoverv1.RedisFailover{ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "testns"}},
		check:   &mRFService.RedisFailoverCheck{},
		heal:    &mRFService.RedisFailoverHeal{},
		metrics: &sentinelCheckRecorder{Recorder: metrics.Dummy, status: map[string]string{}},
		clock:   time.Unix(1000, 0),
	}
	st.handler = NewRedisFailoverHandler(Config{}, &mRFService.RedisFailoverClient{}, st.check, st.heal, &mK8SService.Services{}, st.metrics, &warnLogger{})
	st.handler.now = func() time.Time { return st.clock }
	return st
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
		st.heal.On("SetSentinelCustomConfig", s, st.rf).Once().Return(nil)
	}
	require.NoError(t, st.handler.checkAndHealSentinels(st.rf, sentinels))
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
	assert.Equal(t, []string{b}, st.reconcile(t, sentinels, a, b))
	assert.Equal(t, metrics.STATUS_UNHEALTHY, st.metrics.status[mismatchMetric])

	// A mismatch that stays after the hold-off gets a reset again.
	st.clock = st.clock.Add(20 * time.Second)
	assert.Equal(t, []string{a}, st.reconcile(t, sentinels, a, b))
}

// A reset clears the replicas that the Sentinel knows. The replica check
// after it in the same reconcile does not reset the Sentinel again.
func TestSentinelResetOnceInAReconcile(t *testing.T) {
	st := newSentinelResetTest()
	s := "10.0.1.1"
	st.check.On("CheckSentinelNumberInMemory", s, st.rf).Once().Return(errors.New("sentinels in memory mismatch"))
	st.check.On("CheckSentinelSlavesNumberInMemory", s, st.rf).Once().Return(errors.New("slaves in memory mismatch"))
	st.heal.On("RestoreSentinel", s).Once().Return(nil)
	st.heal.On("SetSentinelCustomConfig", s, st.rf).Once().Return(nil)

	require.NoError(t, st.handler.checkAndHealSentinels(st.rf, []string{s}))

	st.heal.AssertExpectations(t)
	assert.Equal(t, metrics.STATUS_UNHEALTHY, st.metrics.status[metrics.REDIS_SLAVES_NUMBER_IN_MEMORY_MISMATCH+" "+s])
}

// A failed reset starts no hold-off.
func TestSentinelResetHoldOffNeedsASuccessfulReset(t *testing.T) {
	st := newSentinelResetTest()
	s := "10.0.1.1"
	st.check.On("CheckSentinelNumberInMemory", s, st.rf).Once().Return(errors.New("sentinels in memory mismatch"))
	st.heal.On("RestoreSentinel", s).Once().Return(errors.New("dial tcp 10.0.1.1:26379: i/o timeout"))
	st.check.On("CheckSentinelSlavesNumberInMemory", s, st.rf).Once().Return(nil)
	st.heal.On("SetSentinelCustomConfig", s, st.rf).Once().Return(nil)

	require.NoError(t, st.handler.checkAndHealSentinels(st.rf, []string{s}))

	_, ok := st.handler.sentinelResets.Load(failoverKey(st.rf))
	assert.False(t, ok)
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
