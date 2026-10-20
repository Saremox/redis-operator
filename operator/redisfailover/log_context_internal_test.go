package redisfailover

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	mRFService "github.com/saremox/redis-operator/mocks/operator/redisfailover/service"
	mK8SService "github.com/saremox/redis-operator/mocks/service/k8s"
	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
)

// podFieldLogger records the Info, Warning and Error lines with the level and
// the pod field in front.
type podFieldLogger struct {
	log.DummyLogger
	pod   string
	lines *[]string
}

func (l podFieldLogger) WithField(key string, value any) log.Logger {
	if key == "pod" {
		l.pod = fmt.Sprintf("pod=%v ", value)
	}
	return l
}

func (l podFieldLogger) addf(level, format string, args ...any) {
	*l.lines = append(*l.lines, level+": "+l.pod+fmt.Sprintf(format, args...))
}

func (l podFieldLogger) Infof(format string, args ...any) {
	l.addf("info", format, args...)
}

func (l podFieldLogger) Warningf(format string, args ...any) {
	l.addf("warning", format, args...)
}

func (l podFieldLogger) Errorf(format string, args ...any) {
	l.addf("error", format, args...)
}

// Each Redis pod has 127.0.0.1 as master after the first start, so that is
// not a fault.
func TestNoMasterWithLocalhostMastersLogsAtInfo(t *testing.T) {
	rf := newCustomConfigTestRF()
	rf.Spec.Sentinel.Enabled = new(true)
	mk := noAppliedSecret(&mK8SService.Services{})
	mk.On("GetStatefulSetPods", mock.Anything, mock.Anything).Return(&corev1.PodList{}, nil)
	mk.On("UpdateRedisFailoverStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return()
	mrfc := noUnknownSentinelAddrs(&mRFService.RedisFailoverCheck{})
	mrfc.On("IsRedisRunning", rf).Once().Return(true)
	mrfc.On("IsSentinelRunningQuorum", rf).Once().Return(true)
	mrfc.On("GetNumberMasters", rf).Once().Return(0, nil)
	mrfc.On("GetMaxRedisPodTime", rf).Once().Return(time.Hour, nil)
	mrfc.On("CheckSentinelQuorum", rf).Once().Return(0, nil)
	mrfc.On("CheckIfMasterLocalhost", rf).Once().Return(true, nil)
	mrfc.On("GetMasterIP", rf).Once().Return("", errors.New("stop here"))
	mrfh := &mRFService.RedisFailoverHeal{}
	mrfh.On("ApplyPassword", mock.Anything, mock.Anything, mock.Anything).Maybe().Return(true, false, nil)
	mrfh.On("SetOldestAsMaster", rf).Once().Return("", nil)
	var lines []string
	handler := NewRedisFailoverHandler(Config{}, &mRFService.RedisFailoverClient{}, mrfc, mrfh, mk, metrics.Dummy, podFieldLogger{lines: &lines})

	require.EqualError(t, handler.CheckAndHeal(rf), "stop here")
	assert.Contains(t, lines, "info: each available redis has 127.0.0.1 as master, the operator elects a master")
	for _, line := range lines {
		assert.NotRegexp(t, "^error: ", line)
	}
}

// CheckSentinelQuorum leaves the log to its caller, so the caller line
// carries the error.
func TestNoMasterWithoutSentinelQuorumLogsTheError(t *testing.T) {
	rf := newCustomConfigTestRF()
	rf.Spec.Sentinel.Enabled = new(true)
	mk := noAppliedSecret(&mK8SService.Services{})
	mk.On("GetStatefulSetPods", mock.Anything, mock.Anything).Return(&corev1.PodList{}, nil)
	mk.On("UpdateRedisFailoverStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return()
	mrfc := noUnknownSentinelAddrs(&mRFService.RedisFailoverCheck{})
	mrfc.On("IsRedisRunning", rf).Once().Return(true)
	mrfc.On("IsSentinelRunningQuorum", rf).Once().Return(true)
	mrfc.On("GetNumberMasters", rf).Once().Return(0, nil)
	mrfc.On("GetMaxRedisPodTime", rf).Once().Return(time.Hour, nil)
	mrfc.On("CheckSentinelQuorum", rf).Once().Return(2, errors.New("insufficient sentinels to reach the quorum"))
	mrfh := &mRFService.RedisFailoverHeal{}
	mrfh.On("ApplyPassword", mock.Anything, mock.Anything, mock.Anything).Maybe().Return(true, false, nil)
	mrfh.On("SetOldestAsMaster", rf).Once().Return("", errors.New("stop here"))
	logger := &warnLogger{}
	handler := NewRedisFailoverHandler(Config{}, &mRFService.RedisFailoverClient{}, mrfc, mrfh, mk, metrics.Dummy, logger)

	require.EqualError(t, handler.CheckAndHeal(rf), "stop here")
	assert.Contains(t, logger.warnings, "Quorum not available for sentinel to choose master, estimated unhealthy sentinels: 2, operator to step in: insufficient sentinels to reach the quorum")
}

// The pod field of a replica with a wrong master comes from the typed error,
// not from the error text.
func TestSlaveNotAssociatedToMasterLogsThePod(t *testing.T) {
	const master = "10.0.0.1"
	wrong := &rfservice.WrongMasterError{Pod: "rfr-1", IP: "10.0.0.2", Master: master, Has: "10.0.0.9"}
	tests := []struct {
		name     string
		sentinel bool
		err      error
		expLine  string
	}{
		{
			name:     "Sentinel mode",
			sentinel: true,
			err:      wrong,
			expLine:  "warning: redisfailover=test namespace=testns pod=rfr-1 Slave not associated to master: slave 10.0.0.2 (pod rfr-1) don't have the master 10.0.0.1, has 10.0.0.9",
		},
		{
			name:    "operator-managed mode",
			err:     wrong,
			expLine: "warning: redisfailover=test namespace=testns pod=rfr-1 Slave not associated to master: slave 10.0.0.2 (pod rfr-1) don't have the master 10.0.0.1, has 10.0.0.9",
		},
		{
			name:    "an error without a pod",
			err:     errors.New("forbidden"),
			expLine: "warning: redisfailover=test namespace=testns Slave not associated to master: forbidden",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rf := newCustomConfigTestRF()
			rf.Spec.Sentinel.Enabled = new(test.sentinel)
			mk := noAppliedSecret(&mK8SService.Services{})
			mk.On("GetStatefulSetPods", mock.Anything, mock.Anything).Return(&corev1.PodList{}, nil)
			mk.On("UpdateRedisFailoverStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return()
			mrfc := noUnknownSentinelAddrs(&mRFService.RedisFailoverCheck{})
			mrfc.On("GetSentinelMasterIPs", mock.Anything).Maybe().Return(nil, nil)
			mrfc.On("IsRedisRunning", rf).Once().Return(true)
			mrfc.On("IsSentinelRunningQuorum", rf).Maybe().Return(true)
			mrfc.On("GetNumberMasters", rf).Once().Return(1, nil)
			mrfc.On("CheckMasterHealth", rf).Maybe().Return(true, master, nil)
			mrfc.On("GetMasterIP", rf).Once().Return(master, nil)
			mrfc.On("GetMasterIP", rf).Maybe().Return("", errors.New("stop here"))
			mrfc.On("CheckAllSlavesFromMaster", master, rf).Once().Return(test.err)
			mrfh := &mRFService.RedisFailoverHeal{}
			mrfh.On("AbortOrphanedFailover", mock.Anything, mock.Anything).Maybe().Return(false, nil)
			mrfh.On("ApplyPassword", mock.Anything, mock.Anything, mock.Anything).Maybe().Return(true, false, nil)
			mrfh.On("SetMasterOnAll", master, rf).Maybe().Return(errors.New("stop here"))
			logs := &recordedLogs{}
			handler := NewRedisFailoverHandler(Config{}, &mRFService.RedisFailoverClient{}, mrfc, mrfh, mk, metrics.Dummy, recordingLogger{logs: logs})

			require.EqualError(t, handler.CheckAndHeal(rf), "stop here")
			assert.Contains(t, logs.lines, test.expLine)
		})
	}
}

func TestGetLabelsLogsTheRedisFailover(t *testing.T) {
	rf := newCustomConfigTestRF()
	rf.Spec.LabelWhitelist = []string{"("}
	logs := &recordedLogs{}
	handler := NewRedisFailoverHandler(Config{}, &mRFService.RedisFailoverClient{}, noUnknownSentinelAddrs(&mRFService.RedisFailoverCheck{}), &mRFService.RedisFailoverHeal{}, &mK8SService.Services{}, metrics.Dummy, recordingLogger{logs: logs})

	handler.getLabels(rf)

	assert.Equal(t, []string{"redisfailover=test namespace=testns Unable to compile label whitelist regex '(', ignoring it."}, logs.lines)
}
