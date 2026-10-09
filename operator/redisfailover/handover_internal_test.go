package redisfailover

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	mMetrics "github.com/saremox/redis-operator/mocks/metrics"
	mRFService "github.com/saremox/redis-operator/mocks/operator/redisfailover/service"
	mK8SService "github.com/saremox/redis-operator/mocks/service/k8s"
	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
)

// infoLogger records the Info lines.
type infoLogger struct {
	warnLogger
	infos []string
}

func (l *infoLogger) With(string, interface{}) log.Logger      { return l }
func (l *infoLogger) WithField(string, interface{}) log.Logger { return l }

func (l *infoLogger) Infof(format string, args ...interface{}) {
	l.infos = append(l.infos, fmt.Sprintf(format, args...))
}

type handoverTest struct {
	rf       *redisfailoverv1.RedisFailover
	best     *rfservice.ReplicaInfo
	check    *mRFService.RedisFailoverCheck
	heal     *mRFService.RedisFailoverHeal
	metrics  *mMetrics.Recorder
	logger   *infoLogger
	handler  *RedisFailoverHandler
	clock    time.Time
	requeues []time.Duration
}

func newHandoverTest() *handoverTest {
	ht := &handoverTest{
		rf: &redisfailoverv1.RedisFailover{
			ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "testns"},
			Spec:       redisfailoverv1.RedisFailoverSpec{Redis: redisfailoverv1.RedisSettings{Replicas: 3}},
		},
		best:    &rfservice.ReplicaInfo{IP: "10.0.0.2", PodName: "rfr-test-1", Synced: true},
		check:   &mRFService.RedisFailoverCheck{},
		heal:    &mRFService.RedisFailoverHeal{},
		metrics: &mMetrics.Recorder{},
		logger:  &infoLogger{},
		clock:   time.Unix(1000, 0),
	}
	ht.check.On("GetBestReplicaForPromotion", ht.rf).Return(func(*redisfailoverv1.RedisFailover) (*rfservice.ReplicaInfo, error) {
		return ht.best, nil
	})
	ht.metrics.On("RecordRedisCheck", "testns", "test", metrics.MASTER_HANDOVER_ABORTED, metrics.NOT_APPLICABLE, mock.Anything).Return()
	ht.metrics.On("DeleteCluster", "testns", "test").Maybe().Return()
	ht.handler = NewRedisFailoverHandler(Config{}, &mRFService.RedisFailoverClient{}, ht.check, ht.heal, &mK8SService.Services{}, ht.metrics, ht.logger)
	ht.handler.now = func() time.Time { return ht.clock }
	ht.handler.requeue = func(_ string, after time.Duration) { ht.requeues = append(ht.requeues, after) }
	return ht
}

// handOver runs one handover with a fresh status. It returns the status
// message, and false when the caller must delete the master pod.
func (ht *handoverTest) handOver(t *testing.T, result rfservice.HandoverResult) (string, bool) {
	t.Helper()
	ht.heal.ExpectedCalls = nil
	ht.heal.Calls = nil
	ht.heal.On("HandOverMaster", "10.0.0.1", ht.best.IP, ht.rf).Return(result, nil)
	ht.heal.On("PromoteBestReplica", ht.best.IP, ht.rf).Return(nil)
	ht.rf.Status = redisfailoverv1.RedisFailoverStatus{State: redisfailoverv1.HealthyState}
	handled, err := ht.handler.handOverMaster(ht.rf, "10.0.0.1", "rfr-test-0")
	require.NoError(t, err)
	return ht.rf.Status.Message, handled
}

func (ht *handoverTest) message(t *testing.T, result rfservice.HandoverResult) string {
	t.Helper()
	message, handled := ht.handOver(t, result)
	assert.True(t, handled)
	return message
}

const aborted = "the handover of the master role to pod rfr-test-1 was aborted, because the replica did not catch up with the master in 2s, the next attempt is after "

// Each attempt can pause the writes, so an aborted handover waits before the
// next attempt, and the wait doubles. An abort never deletes the master: the
// rollout waits until the replica catches up. The status keeps its message
// during the wait, so it does not change at each reconcile.
func TestHandOverMasterWaitsBeforeTheNextAttempt(t *testing.T) {
	ht := newHandoverTest()

	assert.Equal(t, aborted+"30s", ht.message(t, rfservice.HandoverAborted))
	assert.Equal(t, []time.Duration{30 * time.Second}, ht.requeues)

	// In the wait, no FAILOVER is sent.
	ht.clock = ht.clock.Add(29 * time.Second)
	assert.Equal(t, aborted+"30s", ht.message(t, rfservice.HandoverAborted))
	ht.heal.AssertNotCalled(t, "HandOverMaster", "10.0.0.1", "10.0.0.2", ht.rf)
	assert.Equal(t, redisfailoverv1.NotHealthyState, ht.rf.Status.State)

	// After the wait, the next attempt runs, and the wait doubles to a limit.
	for _, want := range []string{"1m0s", "2m0s", "4m0s", "5m0s", "5m0s", "5m0s"} {
		ht.clock = ht.clock.Add(handoverRetryMax)
		assert.Equal(t, aborted+want, ht.message(t, rfservice.HandoverAborted))
		ht.heal.AssertCalled(t, "HandOverMaster", "10.0.0.1", "10.0.0.2", ht.rf)
	}
	ht.metrics.AssertNumberOfCalls(t, "RecordRedisCheck", 7)
	ht.metrics.AssertCalled(t, "RecordRedisCheck", "testns", "test", metrics.MASTER_HANDOVER_ABORTED, metrics.NOT_APPLICABLE, metrics.STATUS_UNHEALTHY)

	// A handover that moves the role ends the wait.
	ht.clock = ht.clock.Add(handoverRetryMax)
	assert.Empty(t, ht.message(t, rfservice.HandoverDone))
	ht.metrics.AssertCalled(t, "RecordRedisCheck", "testns", "test", metrics.MASTER_HANDOVER_ABORTED, metrics.NOT_APPLICABLE, metrics.STATUS_HEALTHY)
	_, waiting := ht.handler.handoverRetries.Load(failoverKey(ht.rf))
	assert.False(t, waiting)
	assert.Equal(t, aborted+"30s", ht.message(t, rfservice.HandoverAborted))

	// Forget drops the wait.
	ht.handler.Forget(failoverKey(ht.rf))
	_, waiting = ht.handler.handoverRetries.Load(failoverKey(ht.rf))
	assert.False(t, waiting)
}

// A master that sees its replicas at another address refuses each FAILOVER
// TO the pod IP. After handoverMaxRefusals refusals of the same target in a
// row, the rollout deletes the master pod, as without FAILOVER.
func TestHandOverMasterFallsBackAfterRepeatedRefusals(t *testing.T) {
	ht := newHandoverTest()
	const refused = "the master refused the handover of the master role to pod rfr-test-1 "
	const fallback = "the master refused the handover of the master role 3 times in a row, so the rollout deletes the master pod rfr-test-0"
	refuse := func() (string, bool) {
		ht.clock = ht.clock.Add(handoverRetryMax)
		return ht.handOver(t, rfservice.HandoverRefused)
	}

	message, handled := refuse()
	assert.True(t, handled)
	assert.Equal(t, refused+"(1 of 3), the next attempt is after 30s", message)
	message, handled = refuse()
	assert.True(t, handled)
	assert.Equal(t, refused+"(2 of 3), the next attempt is after 1m0s", message)
	_, handled = refuse()
	assert.False(t, handled)
	assert.Equal(t, []string{fallback}, ht.logger.infos)
	// The next refusal also deletes, and the log does not repeat.
	_, handled = refuse()
	assert.False(t, handled)
	assert.Equal(t, []string{fallback}, ht.logger.infos)
	assert.Equal(t, []string{fallback}, ht.logger.debugs)

	// An abort resets the count.
	ht.clock = ht.clock.Add(handoverRetryMax)
	ht.message(t, rfservice.HandoverAborted)
	message, _ = refuse()
	assert.Contains(t, message, "(1 of 3)")

	// A refusal of a different target starts a new count.
	refuse()
	ht.best = &rfservice.ReplicaInfo{IP: "10.0.0.3", PodName: "rfr-test-1", Synced: true}
	message, _ = refuse()
	assert.Contains(t, message, "(1 of 3)")

	// A handover that moves the role resets the count.
	refuse()
	ht.clock = ht.clock.Add(handoverRetryMax)
	ht.message(t, rfservice.HandoverDone)
	message, _ = refuse()
	assert.Contains(t, message, "(1 of 3)")

	// Forget drops the count and the log entry.
	refuse()
	ht.handler.Forget(failoverKey(ht.rf))
	message, _ = refuse()
	assert.Contains(t, message, "(1 of 3)")
	_, logged := ht.handler.handoverFallbackLogged.Load(failoverKey(ht.rf))
	assert.False(t, logged)
}

// Without FAILOVER, the rollout deletes the master pod at each attempt, and
// the log says so once.
func TestHandOverMasterLogsTheFallbackOnce(t *testing.T) {
	ht := newHandoverTest()
	const line = "pod rfr-test-0 has no FAILOVER command (Redis before 6.2, or renamed), so the rollout deletes it"

	for range 3 {
		message, handled := ht.handOver(t, rfservice.HandoverUnsupported)
		assert.Empty(t, message)
		assert.False(t, handled)
	}
	assert.Equal(t, []string{line}, ht.logger.infos)

	ht.handler.Forget(failoverKey(ht.rf))
	ht.handOver(t, rfservice.HandoverUnsupported)
	assert.Equal(t, []string{line, line}, ht.logger.infos)
}

// Without a connected controller, an abort still waits.
func TestHandOverMasterWithoutRequeue(t *testing.T) {
	ht := newHandoverTest()
	ht.handler.requeue = nil

	assert.Contains(t, ht.message(t, rfservice.HandoverAborted), "the next attempt is after 30s")
}
