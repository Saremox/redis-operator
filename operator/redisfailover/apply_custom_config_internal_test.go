package redisfailover

// Internal test (package redisfailover) because applyRedisCustomConfig is
// unexported; the main checker_test.go is package redisfailover_test.

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	mRFService "github.com/saremox/redis-operator/mocks/operator/redisfailover/service"
	mK8SService "github.com/saremox/redis-operator/mocks/service/k8s"
	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
)

func newCustomConfigTestRF() *redisfailoverv1.RedisFailover {
	return &redisfailoverv1.RedisFailover{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "testns"},
	}
}

// An unreachable pod must be skipped: the reachable pods still get their config
// and the reconcile does not abort.
func TestApplyRedisCustomConfigSkipsUnreachablePod(t *testing.T) {
	assert := assert.New(t)

	rf := newCustomConfigTestRF()
	ips := []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}

	mrfc := &mRFService.RedisFailoverCheck{}
	mrfc.On("GetRedisesIPs", rf).Once().Return(ips, nil)

	mrfh := &mRFService.RedisFailoverHeal{}
	mrfh.On("SetRedisCustomConfig", "10.0.0.1", rf).Once().Return(nil)
	// Middle pod is on a downed node.
	mrfh.On("SetRedisCustomConfig", "10.0.0.2", rf).Once().Return(errors.New("dial tcp 10.0.0.2:6379: i/o timeout"))
	mrfh.On("SetRedisCustomConfig", "10.0.0.3", rf).Once().Return(nil)

	handler := NewRedisFailoverHandler(Config{}, &mRFService.RedisFailoverClient{}, mrfc, mrfh, &mK8SService.Services{}, metrics.Dummy, log.Dummy)

	err := handler.applyRedisCustomConfig(rf)

	assert.NoError(err)
	// All three pods were attempted - the unreachable one did not stop the loop.
	mrfh.AssertExpectations(t)
}

// A non-connection error (a genuinely bad config, an auth failure) still aborts,
// and the pods after it are not attempted.
func TestApplyRedisCustomConfigAbortsOnConfigError(t *testing.T) {
	assert := assert.New(t)

	rf := newCustomConfigTestRF()
	ips := []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}

	mrfc := &mRFService.RedisFailoverCheck{}
	mrfc.On("GetRedisesIPs", rf).Once().Return(ips, nil)

	mrfh := &mRFService.RedisFailoverHeal{}
	// First pod is reachable but rejects the config; the loop must stop here.
	mrfh.On("SetRedisCustomConfig", "10.0.0.1", rf).Once().Return(errors.New("ERR unknown parameter 'bad'"))
	// No expectation for 10.0.0.2 / 10.0.0.3 - calling them would panic the mock.

	handler := NewRedisFailoverHandler(Config{}, &mRFService.RedisFailoverClient{}, mrfc, mrfh, &mK8SService.Services{}, metrics.Dummy, log.Dummy)

	err := handler.applyRedisCustomConfig(rf)

	assert.Error(err)
	assert.Contains(err.Error(), "unknown parameter")
	mrfh.AssertExpectations(t)
}

// A blocked maxmemory change is reported in the status message without failing
// the reconcile.
func TestEnsureRedisMaxMemoryReportsBlockedMaxMemory(t *testing.T) {
	assert := assert.New(t)

	rf := newCustomConfigTestRF()
	rf.Spec.Redis.MaxMemory = &redisfailoverv1.MaxMemorySettings{Percent: 75, Policy: "noeviction"}
	rf.Status.State = redisfailoverv1.HealthyState
	ips := []string{"10.0.0.1", "10.0.0.2"}

	mrfc := &mRFService.RedisFailoverCheck{}
	mrfc.On("GetRedisesIPs", rf).Once().Return(ips, nil)
	mrfc.On("GetMasterIP", rf).Once().Return("10.0.0.1", nil)

	mrfh := &mRFService.RedisFailoverHeal{}
	mrfh.On("EnsureRedisMaxMemory", rf, "10.0.0.1", ips).Once().Return(rfservice.MaxMemoryResult{Message: "maxmemory kept", HoldRollout: true}, nil)

	handler := NewRedisFailoverHandler(Config{}, &mRFService.RedisFailoverClient{}, mrfc, mrfh, &mK8SService.Services{}, metrics.Dummy, log.Dummy)

	hold, err := handler.ensureRedisMaxMemory(rf, "10.0.0.1")

	assert.NoError(err)
	assert.True(hold)
	assert.Equal(redisfailoverv1.HealthyState, rf.Status.State)
	assert.Equal("maxmemory kept", rf.Status.Message)
	mrfh.AssertExpectations(t)
}

// The master is re-resolved before maxmemory is applied, so a failover earlier
// in the reconcile does not make a replica the memory reference.
func TestEnsureRedisMaxMemoryUsesCurrentMaster(t *testing.T) {
	rf := newCustomConfigTestRF()
	rf.Spec.Redis.MaxMemory = &redisfailoverv1.MaxMemorySettings{Percent: 75, Policy: "noeviction"}
	ips := []string{"10.0.0.1", "10.0.0.2"}

	mrfc := &mRFService.RedisFailoverCheck{}
	mrfc.On("GetRedisesIPs", rf).Once().Return(ips, nil)
	mrfc.On("GetMasterIP", rf).Once().Return("10.0.0.2", nil)

	mrfh := &mRFService.RedisFailoverHeal{}
	mrfh.On("EnsureRedisMaxMemory", rf, "10.0.0.2", ips).Once().Return(rfservice.MaxMemoryResult{}, nil)

	handler := NewRedisFailoverHandler(Config{}, &mRFService.RedisFailoverClient{}, mrfc, mrfh, &mK8SService.Services{}, metrics.Dummy, log.Dummy)

	hold, err := handler.ensureRedisMaxMemory(rf, "10.0.0.1")
	assert.NoError(t, err)
	assert.False(t, hold)
	mrfh.AssertExpectations(t)
}

// Without a single master, maxmemory and the pod rollout wait for the next reconcile.
func TestEnsureRedisMaxMemorySkippedWithoutMaster(t *testing.T) {
	rf := newCustomConfigTestRF()
	rf.Spec.Redis.MaxMemory = &redisfailoverv1.MaxMemorySettings{Percent: 75, Policy: "noeviction"}
	ips := []string{"10.0.0.1", "10.0.0.2"}

	mrfc := &mRFService.RedisFailoverCheck{}
	mrfc.On("GetRedisesIPs", rf).Once().Return(ips, nil)
	mrfc.On("GetMasterIP", rf).Once().Return("", errors.New("ambiguous master count"))

	// No EnsureRedisMaxMemory expectation - calling it would panic the mock.
	mrfh := &mRFService.RedisFailoverHeal{}

	handler := NewRedisFailoverHandler(Config{}, &mRFService.RedisFailoverClient{}, mrfc, mrfh, &mK8SService.Services{}, metrics.Dummy, log.Dummy)

	hold, err := handler.ensureRedisMaxMemory(rf, "10.0.0.1")
	assert.NoError(t, err)
	assert.True(t, hold)
}

func TestEnsureRedisMaxMemoryErrors(t *testing.T) {
	rf := newCustomConfigTestRF()
	rf.Spec.Redis.MaxMemory = &redisfailoverv1.MaxMemorySettings{Percent: 75, Policy: "noeviction"}
	ips := []string{"10.0.0.1"}

	t.Run("listing the redises", func(t *testing.T) {
		mrfc := &mRFService.RedisFailoverCheck{}
		mrfc.On("GetRedisesIPs", rf).Once().Return(nil, errors.New("boom"))
		handler := NewRedisFailoverHandler(Config{}, &mRFService.RedisFailoverClient{}, mrfc, &mRFService.RedisFailoverHeal{}, &mK8SService.Services{}, metrics.Dummy, log.Dummy)

		_, err := handler.ensureRedisMaxMemory(rf, "")
		assert.EqualError(t, err, "boom")
	})

	t.Run("applying maxmemory", func(t *testing.T) {
		mrfc := &mRFService.RedisFailoverCheck{}
		mrfc.On("GetRedisesIPs", rf).Once().Return(ips, nil)
		mrfh := &mRFService.RedisFailoverHeal{}
		mrfh.On("EnsureRedisMaxMemory", rf, "", ips).Once().Return(rfservice.MaxMemoryResult{}, errors.New("ERR boom"))
		handler := NewRedisFailoverHandler(Config{}, &mRFService.RedisFailoverClient{}, mrfc, mrfh, &mK8SService.Services{}, metrics.Dummy, log.Dummy)

		_, err := handler.ensureRedisMaxMemory(rf, "")
		assert.EqualError(t, err, "ERR boom")
		mrfh.AssertExpectations(t)
	})
}
