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

// warnLogger records the Info, Warning and Debug lines.
type warnLogger struct {
	log.DummyLogger
	mu       sync.Mutex
	infos    []string
	warnings []string
	debugs   []string
}

func (l *warnLogger) With(string, any) log.Logger      { return l }
func (l *warnLogger) WithField(string, any) log.Logger { return l }

func (l *warnLogger) Warningf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warnings = append(l.warnings, fmt.Sprintf(format, args...))
}

func (l *warnLogger) Infof(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.infos = append(l.infos, fmt.Sprintf(format, args...))
}

func (l *warnLogger) Debugf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.debugs = append(l.debugs, fmt.Sprintf(format, args...))
}

func (l *warnLogger) reset() {
	l.infos, l.warnings, l.debugs = nil, nil, nil
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

func TestEnsureRedisMaxMemoryForgetsTheLoggedOutcomeWhenMaxMemoryIsRemoved(t *testing.T) {
	rf := newCustomConfigTestRF()
	rf.Spec.Redis.MaxMemory = &redisfailoverv1.MaxMemorySettings{Percent: 75, Policy: "noeviction"}
	ips := []string{"10.0.0.1"}
	result := rfservice.MaxMemoryResult{Message: "maxmemory kept at 1: lowering it to 2 would not fit", HoldRollout: true}

	mrfc := &mRFService.RedisFailoverCheck{}
	mrfc.On("GetRedisesIPs", rf).Return(ips, nil)
	mrfc.On("GetMasterIP", rf).Return("10.0.0.1", nil)
	mrfh := &mRFService.RedisFailoverHeal{}
	mrfh.On("EnsureRedisMaxMemory", rf, "10.0.0.1", ips).Return(result, nil)
	logger := &warnLogger{}
	handler := NewRedisFailoverHandler(Config{}, &mRFService.RedisFailoverClient{}, mrfc, mrfh, &mK8SService.Services{}, metrics.Dummy, logger)

	_, err := handler.ensureRedisMaxMemory(rf, "10.0.0.1")
	require.NoError(t, err)
	_, tracked := handler.maxMemoryLogged.Load(failoverKey(rf))
	require.True(t, tracked)

	// Removing maxMemory drops the entry, so that the same outcome after a
	// later re-enable is a Warning again.
	maxMemory := rf.Spec.Redis.MaxMemory
	rf.Spec.Redis.MaxMemory = nil
	logger.reset()
	held, err := handler.ensureRedisMaxMemory(rf, "10.0.0.1")
	require.NoError(t, err)
	assert.False(t, held)
	_, tracked = handler.maxMemoryLogged.Load(failoverKey(rf))
	assert.False(t, tracked)
	// The message and the hold end with the setting.
	assert.Equal(t, []string{"The maxmemory message ended: " + result.Message, "The maxmemory hold of the pod rollout ended"}, logger.infos)
	assert.Empty(t, logger.warnings)

	rf.Spec.Redis.MaxMemory = maxMemory
	logger.reset()
	_, err = handler.ensureRedisMaxMemory(rf, "10.0.0.1")
	require.NoError(t, err)
	assert.Equal(t, []string{result.Message, "Holding the pod rollout until maxmemory fits the lowered memory limit"}, logger.warnings)
	assert.Empty(t, logger.debugs)
}

// Each part of an outcome that ends is one Info line. A changed outcome is a
// Warning, and an unchanged one a Debug line.
func TestEnsureRedisMaxMemoryLogsTheEndOfAnOutcome(t *testing.T) {
	const (
		m         = "maxmemory kept at 1: lowering it to 2 would not fit"
		m2        = "maxmemory kept at 1: lowering it to 3 would not fit"
		hold      = "Holding the pod rollout until maxmemory fits the lowered memory limit"
		ended     = "The maxmemory message ended: " + m
		holdEnded = "The maxmemory hold of the pod rollout ended"
	)
	mh := rfservice.MaxMemoryResult{Message: m, HoldRollout: true}
	none := rfservice.MaxMemoryResult{}
	tests := []struct {
		name                    string
		steps                   []rfservice.MaxMemoryResult // the last step is checked
		infos, warnings, debugs []string
	}{
		{"message and hold end", []rfservice.MaxMemoryResult{mh, none}, []string{ended, holdEnded}, nil, nil},
		{"the message ends", []rfservice.MaxMemoryResult{mh, {HoldRollout: true}}, []string{ended}, []string{hold}, nil},
		{"the hold ends", []rfservice.MaxMemoryResult{mh, {Message: m}}, []string{holdEnded}, []string{m}, nil},
		{"the message changes", []rfservice.MaxMemoryResult{{Message: m}, {Message: m2}}, nil, []string{m2}, nil},
		{"a hold alone ends", []rfservice.MaxMemoryResult{{HoldRollout: true}, none}, []string{holdEnded}, nil, nil},
		{"message and hold start", []rfservice.MaxMemoryResult{none, mh}, nil, []string{m, hold}, nil},
		{"unchanged", []rfservice.MaxMemoryResult{mh, mh}, nil, nil, []string{m, hold}},
		{"nothing ends a second time", []rfservice.MaxMemoryResult{mh, none, none}, nil, nil, nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rf := newCustomConfigTestRF()
			rf.Spec.Redis.MaxMemory = &redisfailoverv1.MaxMemorySettings{Percent: 75, Policy: "noeviction"}
			ips := []string{"10.0.0.1"}
			mrfc := &mRFService.RedisFailoverCheck{}
			mrfc.On("GetRedisesIPs", rf).Return(ips, nil)
			mrfc.On("GetMasterIP", rf).Return("10.0.0.1", nil)
			mrfh := &mRFService.RedisFailoverHeal{}
			logger := &warnLogger{}
			handler := NewRedisFailoverHandler(Config{}, &mRFService.RedisFailoverClient{}, mrfc, mrfh, &mK8SService.Services{}, metrics.Dummy, logger)
			for _, result := range test.steps {
				logger.reset()
				mrfh.ExpectedCalls = nil
				mrfh.On("EnsureRedisMaxMemory", rf, "10.0.0.1", ips).Once().Return(result, nil)
				_, err := handler.ensureRedisMaxMemory(rf, "10.0.0.1")
				require.NoError(t, err)
			}
			assert.Equal(t, test.infos, logger.infos)
			assert.Equal(t, test.warnings, logger.warnings)
			assert.Equal(t, test.debugs, logger.debugs)
		})
	}
}
