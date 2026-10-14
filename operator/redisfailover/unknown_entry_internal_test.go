package redisfailover

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	"github.com/saremox/redis-operator/operator/redisfailover/autoscale"
)

// unknownEntry has a type that no map of the handler holds. Each reader
// treats it as a missing entry.
const unknownEntry = "unknown"

func unknownEntryRF() *redisfailoverv1.RedisFailover {
	return &redisfailoverv1.RedisFailover{ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "testns"}}
}

func TestAutoscalerLimitsIgnoreAnUnknownEntry(t *testing.T) {
	h := NewRedisFailoverHandler(Config{}, nil, nil, nil, nil, metrics.Dummy, log.Dummy)
	rf := autoscaleRolloutRF(redisfailoverv1.AutoscalingModeAuto)
	h.autoscaleReads.Store(failoverKey(rf), unknownEntry)

	assert.Zero(t, h.autoscalerLimits(rf))
}

func TestAutoscaleIgnoresAnUnknownSample(t *testing.T) {
	at := newAutoscaleTest(t, redisfailoverv1.AutoscalingModeRecommend)
	f := at.fakeStateStore(t)
	at.h.autoscaleSamples.Store(failoverKey(at.rf), unknownEntry)
	at.mrfc.On("GetAutoscalerInfo", at.rf, autoscaleMaster).Return(masterInfo(500), nil)
	at.mrec.On("SetAutoscaler", "testns", "test", int64(0), mock.Anything, int32(0), 1024*mib)

	at.run(redisfailoverv1.AutoscalingStatus{})

	at.mrfc.AssertNumberOfCalls(t, "GetAutoscalerInfo", 1)
	assert.Equal(t, autoscale.ReasonLow, at.rf.Status.Autoscaling.Reason)
	require.NotNil(t, f.stored)
	assert.Equal(t, autoscaleT0.Unix(), decodeState(t, f.stored).LastSample)
	v, _ := at.h.autoscaleSamples.Load(failoverKey(at.rf))
	assert.IsType(t, autoscaleSample{}, v)
}

func TestReportRolloutWaitReplacesAnUnknownEntry(t *testing.T) {
	h := NewRedisFailoverHandler(Config{}, nil, nil, nil, nil, metrics.Dummy, log.Dummy)
	rf := unknownEntryRF()
	h.rolloutWaits.Store(passwordKey(rf), unknownEntry)

	h.reportRolloutWait(rf, &rolloutWait{uid: "u", pod: "rfr-test-0", reason: "not ready"})

	v, _ := h.rolloutWaits.Load(passwordKey(rf))
	wait, ok := v.(rolloutWait)
	require.True(t, ok, "entry %v", v)
	assert.Equal(t, types.UID("u"), wait.uid)
	assert.WithinDuration(t, time.Now(), wait.since, time.Minute)
	assert.Empty(t, rf.Status.Message)
}

func TestLogAppliedSecretErrorWarnsAfterAnUnknownEntry(t *testing.T) {
	logger := &warnLogger{}
	h := NewRedisFailoverHandler(Config{}, nil, nil, nil, nil, metrics.Dummy, logger)
	rf := unknownEntryRF()
	h.appliedSecretLogged.Store(passwordKey(rf), unknownEntry)

	h.logAppliedSecretError(rf, true, "unable to read", errors.New("forbidden"))

	assert.Equal(t, []string{"unable to read: forbidden"}, logger.warnings)
	assert.Empty(t, logger.debugs)
	v, _ := h.appliedSecretLogged.Load(passwordKey(rf))
	assert.Equal(t, appliedSecretLog{read: true, err: "forbidden"}, v)
}

func TestForgetAppliedSecretLogKeepsAnUnknownEntry(t *testing.T) {
	h := NewRedisFailoverHandler(Config{}, nil, nil, nil, nil, metrics.Dummy, log.Dummy)
	rf := unknownEntryRF()
	h.appliedSecretLogged.Store(passwordKey(rf), unknownEntry)

	h.forgetAppliedSecretLog(rf, false)

	v, _ := h.appliedSecretLogged.Load(passwordKey(rf))
	assert.Equal(t, unknownEntry, v)
}

func TestStartSentinelHealIgnoresAnUnknownEntry(t *testing.T) {
	st := newSentinelResetTest()
	st.handler.sentinelResets.Store(failoverKey(st.rf), unknownEntry)

	heal := st.handler.startSentinelHeal(st.rf)

	assert.Empty(t, heal.resets)
}
