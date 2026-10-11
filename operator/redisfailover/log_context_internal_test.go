package redisfailover

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	mRFService "github.com/saremox/redis-operator/mocks/operator/redisfailover/service"
	mK8SService "github.com/saremox/redis-operator/mocks/service/k8s"
)

// podFieldLogger records the Info, Warning and Error lines with the level and
// the pod field in front.
type podFieldLogger struct {
	log.DummyLogger
	pod   string
	lines *[]string
}

func (l podFieldLogger) WithField(key string, value interface{}) log.Logger {
	if key == "pod" {
		l.pod = fmt.Sprintf("pod=%v ", value)
	}
	return l
}

func (l podFieldLogger) add(level, format string, args ...interface{}) {
	*l.lines = append(*l.lines, level+": "+l.pod+fmt.Sprintf(format, args...))
}

func (l podFieldLogger) Infof(format string, args ...interface{}) {
	l.add("info", format, args...)
}

func (l podFieldLogger) Warningf(format string, args ...interface{}) {
	l.add("warning", format, args...)
}

func (l podFieldLogger) Errorf(format string, args ...interface{}) {
	l.add("error", format, args...)
}

// Each Redis pod has 127.0.0.1 as master after the first start, so that is
// not a fault.
func TestNoMasterWithLocalhostMastersLogsAtInfo(t *testing.T) {
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
	mrfc.On("CheckSentinelQuorum", rf).Once().Return(0, nil)
	mrfc.On("CheckIfMasterLocalhost", rf).Once().Return(true, nil)
	mrfc.On("GetMasterIP", rf).Once().Return("", errors.New("stop here"))
	mrfh := &mRFService.RedisFailoverHeal{}
	mrfh.On("ApplyPassword", mock.Anything, mock.Anything, mock.Anything).Maybe().Return(true, nil)
	mrfh.On("SetOldestAsMaster", rf).Once().Return(nil)
	var lines []string
	handler := NewRedisFailoverHandler(Config{}, &mRFService.RedisFailoverClient{}, mrfc, mrfh, mk, metrics.Dummy, podFieldLogger{lines: &lines})

	assert.EqualError(t, handler.CheckAndHeal(rf), "stop here")
	assert.Contains(t, lines, "info: each available redis has 127.0.0.1 as master, the operator elects a master")
	for _, line := range lines {
		assert.NotRegexp(t, "^error: ", line)
	}
}

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
