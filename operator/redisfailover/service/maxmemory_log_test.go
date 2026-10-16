package service

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/saremox/redis-operator/log"
	mRedisService "github.com/saremox/redis-operator/mocks/service/redis"
	"github.com/saremox/redis-operator/service/redis"
)

// infoLogger records the Info lines.
type infoLogger struct {
	log.DummyLogger
	mu    sync.Mutex
	infos []string
}

func (l *infoLogger) With(string, any) log.Logger      { return l }
func (l *infoLogger) WithField(string, any) log.Logger { return l }

func (l *infoLogger) Infof(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.infos = append(l.infos, fmt.Sprintf(format, args...))
}

// ensureMaxMemoryLogs runs EnsureRedisMaxMemory and returns the Info lines.
func ensureMaxMemoryLogs(t *testing.T, policy, master string, mr *mRedisService.Client) []string {
	t.Helper()
	logger := &infoLogger{}
	_, err := NewRedisFailoverHealer(k8sWithPods(), mr, logger).EnsureRedisMaxMemory(rfWithMaxMemory(policy), master, mmRedises)
	require.NoError(t, err)
	mr.AssertExpectations(t)
	return logger.infos
}

func TestEnsureRedisMaxMemoryLogsEachChange(t *testing.T) {
	t.Run("lower", func(t *testing.T) {
		mr := redisMock()
		expectPolicy(mr, "noeviction")
		expectMemoryInfo(mr, &redis.MemoryInfo{MaxMemory: 900 * mi, UsedMemory: 700 * mi, MaxMemoryPolicy: "noeviction"})
		expectSet(mr, mmMaster, maxMemoryConfig(mmTarget))
		expectMemoryInfo(mr, afterLowering(650*mi))
		expectSet(mr, mmReplica, maxMemoryConfig(mmTarget))

		assert.Equal(t, []string{
			"Set maxmemory 900.0Mi -> 768.0Mi on 10.0.0.1 (used 700.0Mi, policy noeviction)",
			"Set maxmemory 768.0Mi on 10.0.0.2 (replica, the master had 900.0Mi)",
		}, ensureMaxMemoryLogs(t, "noeviction", mmMaster, mr))
	})

	t.Run("raise", func(t *testing.T) {
		mr := redisMock()
		expectPolicy(mr, "noeviction")
		expectMemoryInfo(mr, &redis.MemoryInfo{MaxMemory: 512 * mi, UsedMemory: 500 * mi, MaxMemoryPolicy: "noeviction"})
		expectSet(mr, mmMaster, maxMemoryConfig(mmTarget))
		expectSet(mr, mmReplica, maxMemoryConfig(mmTarget))

		assert.Equal(t, []string{
			"Set maxmemory 512.0Mi -> 768.0Mi on 10.0.0.1 (used 500.0Mi, policy noeviction)",
			"Set maxmemory 768.0Mi on 10.0.0.2 (replica, the master had 512.0Mi)",
		}, ensureMaxMemoryLogs(t, "noeviction", mmMaster, mr))
	})

	t.Run("restore", func(t *testing.T) {
		mr := redisMock()
		expectPolicy(mr, "noeviction")
		expectMemoryInfo(mr, &redis.MemoryInfo{MaxMemory: 900 * mi, UsedMemory: 700 * mi, MaxMemoryPolicy: "noeviction"})
		expectSet(mr, mmMaster, maxMemoryConfig(mmTarget))
		expectMemoryInfo(mr, afterLowering(790*mi))
		expectSet(mr, mmMaster, maxMemoryConfig(900*mi))
		expectSet(mr, mmReplica, maxMemoryConfig(900*mi))

		assert.Equal(t, []string{
			"Set maxmemory 900.0Mi -> 768.0Mi on 10.0.0.1 (used 700.0Mi, policy noeviction)",
			"Set maxmemory 768.0Mi -> 900.0Mi on 10.0.0.1 (used 790.0Mi, policy noeviction)",
		}, ensureMaxMemoryLogs(t, "noeviction", mmMaster, mr))
	})

	// The master did not take the lowered value, so nothing changed.
	t.Run("restore after a failed lower", func(t *testing.T) {
		fail := errors.New("ERR boom")
		mr := redisMock()
		expectPolicy(mr, "noeviction")
		expectMemoryInfo(mr, &redis.MemoryInfo{MaxMemory: 900 * mi, UsedMemory: 700 * mi, MaxMemoryPolicy: "noeviction"})
		mr.On("SetCustomRedisConfig", mmMaster, "0", []string{maxMemoryConfig(mmTarget)}, "").Once().Return(fail)
		expectSet(mr, mmMaster, maxMemoryConfig(900*mi))
		logger := &infoLogger{}
		_, err := NewRedisFailoverHealer(k8sWithPods(), mr, logger).EnsureRedisMaxMemory(rfWithMaxMemory("noeviction"), mmMaster, mmRedises)
		require.ErrorIs(t, err, fail)
		mr.AssertExpectations(t)
		assert.Empty(t, logger.infos)
	})

	t.Run("kept", func(t *testing.T) {
		mr := redisMock()
		expectPolicy(mr, "noeviction")
		expectMemoryInfo(mr, &redis.MemoryInfo{MaxMemory: 900 * mi, UsedMemory: 800 * mi, MaxMemoryPolicy: "noeviction"})
		expectSet(mr, mmReplica, maxMemoryConfig(900*mi))

		assert.Empty(t, ensureMaxMemoryLogs(t, "noeviction", mmMaster, mr))
	})

	t.Run("kept, capped from unlimited", func(t *testing.T) {
		mr := redisMock()
		expectPolicy(mr, "noeviction")
		expectMemoryInfo(mr, &redis.MemoryInfo{UsedMemory: 800 * mi, MaxMemoryPolicy: "noeviction"})
		expectSet(mr, mmMaster, maxMemoryConfig(992*mi))
		expectSet(mr, mmReplica, maxMemoryConfig(992*mi))

		assert.Equal(t, []string{
			"Set maxmemory unlimited -> 992.0Mi on 10.0.0.1 (used 800.0Mi, policy noeviction)",
			"Set maxmemory 992.0Mi on 10.0.0.2 (replica, the master had unlimited)",
		}, ensureMaxMemoryLogs(t, "noeviction", mmMaster, mr))
	})

	// The operator sets the value at each reconcile. It does not read the
	// replicas, so a replica with another value also gets no line.
	t.Run("unchanged", func(t *testing.T) {
		mr := redisMock()
		expectPolicy(mr, "noeviction")
		expectMemoryInfo(mr, &redis.MemoryInfo{MaxMemory: mmTarget, UsedMemory: 700 * mi, MaxMemoryPolicy: "noeviction"})
		expectSet(mr, mmMaster, maxMemoryConfig(mmTarget))
		expectSet(mr, mmReplica, maxMemoryConfig(mmTarget))

		assert.Empty(t, ensureMaxMemoryLogs(t, "noeviction", mmMaster, mr))
	})

	t.Run("unreachable replica", func(t *testing.T) {
		mr := &mRedisService.Client{}
		mr.On("SetCustomRedisConfig", mmMaster, "0", []string{"replica-ignore-maxmemory yes"}, "").Return(nil)
		mr.On("SetCustomRedisConfig", mmReplica, "0", []string{"replica-ignore-maxmemory yes"}, "").Return(nil)
		expectPolicy(mr, "noeviction")
		expectMemoryInfo(mr, &redis.MemoryInfo{MaxMemory: 512 * mi, UsedMemory: 500 * mi, MaxMemoryPolicy: "noeviction"})
		expectSet(mr, mmMaster, maxMemoryConfig(mmTarget))
		mr.On("SetCustomRedisConfig", mmReplica, "0", []string{maxMemoryConfig(mmTarget)}, "").Once().Return(errors.New("dial tcp 10.0.0.2:6379: connect: connection refused"))

		assert.Equal(t, []string{
			"Set maxmemory 512.0Mi -> 768.0Mi on 10.0.0.1 (used 500.0Mi, policy noeviction)",
		}, ensureMaxMemoryLogs(t, "noeviction", mmMaster, mr))
	})

	t.Run("without a master", func(t *testing.T) {
		mr := redisMock()
		expectPolicy(mr, "noeviction")
		expectMemoryInfo(mr, &redis.MemoryInfo{MaxMemory: 900 * mi, UsedMemory: 100 * mi, MaxMemoryPolicy: "noeviction"})
		expectSet(mr, mmMaster, maxMemoryConfig(mmTarget))
		mr.On("GetMemoryInfo", mmReplica, "0", "").Once().Return(&redis.MemoryInfo{MaxMemory: mmTarget, UsedMemory: 100 * mi, MaxMemoryPolicy: "noeviction"}, nil)
		expectSet(mr, mmReplica, maxMemoryConfig(mmTarget))

		assert.Equal(t, []string{
			"Set maxmemory 900.0Mi -> 768.0Mi on 10.0.0.1 (used 100.0Mi, policy noeviction)",
		}, ensureMaxMemoryLogs(t, "noeviction", "", mr))
	})
}

// The master line has the maxmemory-policy in force on the master when the
// operator writes maxmemory.
func TestEnsureRedisMaxMemoryLogsThePolicyInForce(t *testing.T) {
	t.Run("raise before the policy change", func(t *testing.T) {
		mr := redisMock()
		expectMemoryInfo(mr, &redis.MemoryInfo{MaxMemory: 512 * mi, UsedMemory: 500 * mi, MaxMemoryPolicy: "noeviction"})
		expectSet(mr, mmMaster, maxMemoryConfig(mmTarget))
		expectSet(mr, mmReplica, maxMemoryConfig(mmTarget))
		expectPolicy(mr, "allkeys-lru")

		assert.Equal(t, []string{
			"Set maxmemory 512.0Mi -> 768.0Mi on 10.0.0.1 (used 500.0Mi, policy noeviction)",
			"Set maxmemory 768.0Mi on 10.0.0.2 (replica, the master had 512.0Mi)",
		}, ensureMaxMemoryLogs(t, "allkeys-lru", mmMaster, mr))
	})

	t.Run("lower after the policy change", func(t *testing.T) {
		mr := redisMock()
		expectMemoryInfo(mr, &redis.MemoryInfo{MaxMemory: 900 * mi, UsedMemory: 700 * mi, MaxMemoryPolicy: "noeviction"})
		expectPolicy(mr, "allkeys-lru")
		expectSet(mr, mmMaster, maxMemoryConfig(mmTarget))
		expectMemoryInfo(mr, afterLowering(650*mi))
		expectSet(mr, mmReplica, maxMemoryConfig(mmTarget))

		assert.Equal(t, []string{
			"Set maxmemory 900.0Mi -> 768.0Mi on 10.0.0.1 (used 700.0Mi, policy allkeys-lru)",
			"Set maxmemory 768.0Mi on 10.0.0.2 (replica, the master had 900.0Mi)",
		}, ensureMaxMemoryLogs(t, "allkeys-lru", mmMaster, mr))
	})

	// The master skipped the policy as unreachable, so the read one stays.
	t.Run("lower, the master did not take the policy", func(t *testing.T) {
		mr := redisMock()
		expectMemoryInfo(mr, &redis.MemoryInfo{MaxMemory: 900 * mi, UsedMemory: 700 * mi, MaxMemoryPolicy: "noeviction"})
		mr.On("SetCustomRedisConfig", mmMaster, "0", []string{"maxmemory-policy allkeys-lru"}, "").Once().Return(errors.New("dial tcp 10.0.0.1:6379: connect: connection refused"))
		expectSet(mr, mmReplica, "maxmemory-policy allkeys-lru")
		expectSet(mr, mmMaster, maxMemoryConfig(mmTarget))
		expectMemoryInfo(mr, afterLowering(650*mi))
		expectSet(mr, mmReplica, maxMemoryConfig(mmTarget))

		assert.Equal(t, []string{
			"Set maxmemory 900.0Mi -> 768.0Mi on 10.0.0.1 (used 700.0Mi, policy noeviction)",
			"Set maxmemory 768.0Mi on 10.0.0.2 (replica, the master had 900.0Mi)",
		}, ensureMaxMemoryLogs(t, "allkeys-lru", mmMaster, mr))
	})
}

// A used memory of 0 is not "unlimited".
func TestLogMaxMemoryChangeFormatsAZeroUsedMemory(t *testing.T) {
	logger := &infoLogger{}
	logMaxMemoryChange(logger, mmMaster, 0, mmTarget, 0, "noeviction")
	assert.Equal(t, []string{"Set maxmemory unlimited -> 768.0Mi on 10.0.0.1 (used 0.0Mi, policy noeviction)"}, logger.infos)
}
