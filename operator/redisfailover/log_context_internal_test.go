package redisfailover

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	"github.com/saremox/redis-operator/metrics"
	mRFService "github.com/saremox/redis-operator/mocks/operator/redisfailover/service"
	mK8SService "github.com/saremox/redis-operator/mocks/service/k8s"
)

// CheckSentinelQuorum leaves the log to its caller, so the caller line
// carries the error.
func TestNoMasterWithoutSentinelQuorumLogsTheError(t *testing.T) {
	rf := newCustomConfigTestRF()
	rf.Spec.Sentinel.Enabled = ptr.To(true)
	mk := &mK8SService.Services{}
	mk.On("GetStatefulSetPods", mock.Anything, mock.Anything).Return(&corev1.PodList{}, nil)
	mk.On("UpdateRedisFailoverStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return()
	mrfc := &mRFService.RedisFailoverCheck{}
	mrfc.On("IsRedisRunningQuorum", rf).Once().Return(true)
	mrfc.On("IsSentinelRunningQuorum", rf).Once().Return(true)
	mrfc.On("GetNumberMasters", rf).Once().Return(0, nil)
	mrfc.On("GetMaxRedisPodTime", rf).Once().Return(time.Hour, nil)
	mrfc.On("CheckSentinelQuorum", rf).Once().Return(2, errors.New("insufficient sentinels to reach the quorum"))
	mrfh := &mRFService.RedisFailoverHeal{}
	mrfh.On("ApplyPassword", mock.Anything, mock.Anything, mock.Anything).Maybe().Return(true, nil)
	mrfh.On("SetOldestAsMaster", rf).Once().Return(errors.New("stop here"))
	logger := &warnLogger{}
	handler := NewRedisFailoverHandler(Config{}, &mRFService.RedisFailoverClient{}, mrfc, mrfh, mk, metrics.Dummy, logger)

	assert.EqualError(t, handler.CheckAndHeal(rf), "stop here")
	assert.Contains(t, logger.warnings, "Quorum not available for sentinel to choose master, estimated unhealthy sentinels: 2, operator to step in: insufficient sentinels to reach the quorum")
}
