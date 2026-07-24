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
