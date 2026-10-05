package redisfailover

import (
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	mRFService "github.com/saremox/redis-operator/mocks/operator/redisfailover/service"
	mK8SService "github.com/saremox/redis-operator/mocks/service/k8s"
	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
)

// warnLogger records the Warning and Debug lines.
type warnLogger struct {
	log.DummyLogger
	mu       sync.Mutex
	warnings []string
	debugs   []string
}

func (l *warnLogger) With(string, interface{}) log.Logger      { return l }
func (l *warnLogger) WithField(string, interface{}) log.Logger { return l }

func (l *warnLogger) Warningf(format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warnings = append(l.warnings, fmt.Sprintf(format, args...))
}

func (l *warnLogger) Debugf(format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.debugs = append(l.debugs, fmt.Sprintf(format, args...))
}

func (l *warnLogger) reset() {
	l.warnings, l.debugs = nil, nil
}

func TestEnsureRedisMaxMemoryLogsAnUnchangedHoldOnce(t *testing.T) {
	rf := newCustomConfigTestRF()
	rf.Spec.Redis.MaxMemory = &redisfailoverv1.MaxMemorySettings{Percent: 75, Policy: "noeviction"}
	ips := []string{"10.0.0.1"}
	const hold = "Holding the pod rollout until maxmemory fits the lowered memory limit"

	mrfc := &mRFService.RedisFailoverCheck{}
	mrfc.On("GetRedisesIPs", rf).Return(ips, nil)
	mrfc.On("GetMasterIP", rf).Return("10.0.0.1", nil)
	mrfh := &mRFService.RedisFailoverHeal{}
	logger := &warnLogger{}
	handler := NewRedisFailoverHandler(Config{}, &mRFService.RedisFailoverClient{}, mrfc, mrfh, &mK8SService.Services{}, metrics.Dummy, logger)

	ensure := func(result rfservice.MaxMemoryResult) (warnings, debugs []string) {
		logger.reset()
		mrfh.ExpectedCalls = nil
		mrfh.On("EnsureRedisMaxMemory", rf, "10.0.0.1", ips).Once().Return(result, nil)
		got, err := handler.ensureRedisMaxMemory(rf, "10.0.0.1")
		require.NoError(t, err)
		assert.Equal(t, result.HoldRollout, got)
		return logger.warnings, logger.debugs
	}
	first := rfservice.MaxMemoryResult{Message: "maxmemory kept at 1: lowering it to 2 would not fit", HoldRollout: true}
	second := rfservice.MaxMemoryResult{Message: "maxmemory kept at 1: lowering it to 3 would not fit", HoldRollout: true}

	// The first outcome is a Warning. The status carries the same text.
	warnings, debugs := ensure(first)
	assert.Equal(t, []string{first.Message, hold}, warnings)
	assert.Empty(t, debugs)
	assert.Equal(t, first.Message, rf.Status.Message)

	// The same outcome is a Debug line at each later reconcile.
	for range 3 {
		warnings, debugs = ensure(first)
		assert.Empty(t, warnings)
		assert.Equal(t, []string{first.Message, hold}, debugs)
		assert.Equal(t, first.Message, rf.Status.Message)
	}

	// A changed message is a Warning again.
	warnings, debugs = ensure(second)
	assert.Equal(t, []string{second.Message, hold}, warnings)
	assert.Empty(t, debugs)

	// A hold without a message is also logged once.
	warnings, _ = ensure(rfservice.MaxMemoryResult{HoldRollout: true})
	assert.Equal(t, []string{hold}, warnings)
	warnings, debugs = ensure(rfservice.MaxMemoryResult{HoldRollout: true})
	assert.Empty(t, warnings)
	assert.Equal(t, []string{hold}, debugs)

	// The entry ends with the hold, so the next hold is a Warning.
	ensure(rfservice.MaxMemoryResult{})
	_, ok := handler.maxMemoryLogged.Load(failoverKey(rf))
	assert.False(t, ok)
	warnings, _ = ensure(second)
	assert.Equal(t, []string{second.Message, hold}, warnings)

	// Forget drops the entry of the RedisFailover and keeps the others.
	handler.maxMemoryLogged.Store("testns/other", maxMemoryOutcome{message: "kept"})
	handler.Forget(failoverKey(rf))
	_, ok = handler.maxMemoryLogged.Load(failoverKey(rf))
	assert.False(t, ok)
	_, ok = handler.maxMemoryLogged.Load("testns/other")
	assert.True(t, ok)
	warnings, _ = ensure(second)
	assert.Equal(t, []string{second.Message, hold}, warnings)
}

// A message without a hold follows the same rule.
func TestEnsureRedisMaxMemoryLogsAnUnchangedMessageOnce(t *testing.T) {
	rf := newCustomConfigTestRF()
	rf.Spec.Redis.MaxMemory = &redisfailoverv1.MaxMemorySettings{Percent: 75, Policy: "noeviction"}
	ips := []string{"10.0.0.1"}
	mrfc := &mRFService.RedisFailoverCheck{}
	mrfc.On("GetRedisesIPs", rf).Return(ips, nil)
	mrfc.On("GetMasterIP", rf).Return("10.0.0.1", nil)
	mrfh := &mRFService.RedisFailoverHeal{}
	mrfh.On("EnsureRedisMaxMemory", rf, "10.0.0.1", ips).Return(rfservice.MaxMemoryResult{Message: "maxmemory not managed: x"}, nil)
	logger := &warnLogger{}
	handler := NewRedisFailoverHandler(Config{}, &mRFService.RedisFailoverClient{}, mrfc, mrfh, &mK8SService.Services{}, metrics.Dummy, logger)

	hold, err := handler.ensureRedisMaxMemory(rf, "10.0.0.1")
	require.NoError(t, err)
	assert.False(t, hold)
	assert.Equal(t, []string{"maxmemory not managed: x"}, logger.warnings)

	logger.reset()
	_, err = handler.ensureRedisMaxMemory(rf, "10.0.0.1")
	require.NoError(t, err)
	assert.Empty(t, logger.warnings)
	assert.Equal(t, []string{"maxmemory not managed: x"}, logger.debugs)
}
