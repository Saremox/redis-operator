package service

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/log"
	mK8SService "github.com/saremox/redis-operator/mocks/service/k8s"
	mRedisService "github.com/saremox/redis-operator/mocks/service/redis"
	"github.com/saremox/redis-operator/service/redis"
)

const (
	handoverMaster = "10.0.0.1"
	handoverTarget = "10.0.0.2"
)

func handoverRF() *redisfailoverv1.RedisFailover {
	return &redisfailoverv1.RedisFailover{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "testns"},
		Spec:       redisfailoverv1.RedisFailoverSpec{Redis: redisfailoverv1.RedisSettings{Replicas: 3, Port: 6379}},
	}
}

func shortHandoverWait(t *testing.T) {
	pause, margin, interval := handoverWritePause, handoverWaitMargin, handoverPollInterval
	t.Cleanup(func() { handoverWritePause, handoverWaitMargin, handoverPollInterval = pause, margin, interval })
	handoverWritePause, handoverWaitMargin, handoverPollInterval = 20*time.Millisecond, 20*time.Millisecond, time.Millisecond
}

func masterInfo(state string) *redis.ReplicationInfo {
	return &redis.ReplicationInfo{Role: "master", FailoverState: state}
}

func replicaInfo(state, master string) *redis.ReplicationInfo {
	return &redis.ReplicationInfo{Role: "slave", MasterHost: master, FailoverState: state}
}

// The old master pauses the writes until the target has its whole
// replication stream. Without FORCE, Redis aborts on the timeout, and the
// master continues. Each read of master_failover_state is a separate INFO,
// so an operator restart continues a running failover and does not start a
// second one.
func TestHandOverMaster(t *testing.T) {
	errBoom := errors.New("boom")
	tests := []struct {
		name string
		// infos are the INFO replication replies of the old master, in order.
		infos       []*redis.ReplicationInfo
		infoErr     error
		failover    bool
		failoverErr error
		targetRole  *bool
		targetErr   error
		// abort is the reply to FAILOVER ABORT, if the test expects one, and
		// afterAbort the INFO replication reply after it.
		abort      *error
		afterAbort *redis.ReplicationInfo
		// runIDs are the GetRunID replies of the target, in order. The last
		// one repeats. An empty entry is an error. nil means "run-1".
		runIDs []string
		// maxReads limits the INFO reads of the old master before the abort,
		// when the abort must come before the end of the wait.
		maxReads int
		want     HandoverResult
		wantErr  string
	}{
		{
			name:       "the failover moves the role",
			infos:      []*redis.ReplicationInfo{masterInfo("no-failover"), masterInfo("waiting-for-sync"), replicaInfo("failover-in-progress", handoverTarget), replicaInfo("no-failover", handoverTarget)},
			failover:   true,
			targetRole: ptrTo(true),
			want:       HandoverDone,
		},
		{
			name:     "Redis aborts on the timeout",
			infos:    []*redis.ReplicationInfo{masterInfo("no-failover"), masterInfo("waiting-for-sync"), masterInfo("no-failover")},
			failover: true,
			want:     HandoverAborted,
		},
		{
			name:       "a role change that does not end in the wait is aborted",
			infos:      []*redis.ReplicationInfo{masterInfo("no-failover"), replicaInfo("failover-in-progress", handoverTarget)},
			failover:   true,
			abort:      ptrTo[error](nil),
			afterAbort: masterInfo("no-failover"),
			want:       HandoverAborted,
		},
		{
			name:       "a target that stops answering ends the failover at once",
			infos:      []*redis.ReplicationInfo{masterInfo("no-failover"), replicaInfo("failover-in-progress", handoverTarget)},
			failover:   true,
			runIDs:     []string{"run-1", ""},
			maxReads:   2,
			abort:      ptrTo[error](nil),
			afterAbort: masterInfo("no-failover"),
			want:       HandoverAborted,
		},
		{
			name:       "a target that restarted ends the failover at once",
			infos:      []*redis.ReplicationInfo{masterInfo("no-failover"), masterInfo("waiting-for-sync"), replicaInfo("failover-in-progress", handoverTarget)},
			failover:   true,
			runIDs:     []string{"run-1", "run-1", "run-2"},
			maxReads:   3,
			abort:      ptrTo[error](nil),
			afterAbort: masterInfo("no-failover"),
			want:       HandoverAborted,
		},
		{
			name:    "the target does not answer before FAILOVER",
			infos:   []*redis.ReplicationInfo{masterInfo("no-failover")},
			runIDs:  []string{""},
			wantErr: "boom",
		},
		{
			name:       "the role change ends just before the abort",
			infos:      []*redis.ReplicationInfo{masterInfo("no-failover"), replicaInfo("failover-in-progress", handoverTarget)},
			failover:   true,
			abort:      ptrTo(errors.New("ERR No failover in progress.")),
			afterAbort: replicaInfo("no-failover", handoverTarget),
			targetRole: ptrTo(true),
			want:       HandoverDone,
		},
		{
			name:     "FAILOVER ABORT fails",
			infos:    []*redis.ReplicationInfo{masterInfo("no-failover"), replicaInfo("failover-in-progress", handoverTarget)},
			failover: true,
			abort:    ptrTo(errBoom),
			wantErr:  "boom",
		},
		{
			name:     "INFO fails after the abort",
			infos:    []*redis.ReplicationInfo{masterInfo("no-failover"), replicaInfo("failover-in-progress", handoverTarget)},
			failover: true,
			abort:    ptrTo[error](nil),
			wantErr:  "boom",
		},
		{
			name:       "the failover still runs after the abort",
			infos:      []*redis.ReplicationInfo{masterInfo("no-failover"), replicaInfo("failover-in-progress", handoverTarget)},
			failover:   true,
			abort:      ptrTo[error](nil),
			afterAbort: replicaInfo("failover-in-progress", handoverTarget),
			wantErr:    "still runs after FAILOVER ABORT",
		},
		{
			name:       "a restart continues a running failover",
			infos:      []*redis.ReplicationInfo{masterInfo("waiting-for-sync"), replicaInfo("no-failover", handoverTarget)},
			targetRole: ptrTo(true),
			want:       HandoverDone,
		},
		{
			name:       "a restart takes the run ID of the target in the wait",
			infos:      []*redis.ReplicationInfo{masterInfo("waiting-for-sync"), masterInfo("waiting-for-sync"), replicaInfo("no-failover", handoverTarget)},
			runIDs:     []string{"run-1", "run-1"},
			targetRole: ptrTo(true),
			want:       HandoverDone,
		},
		{
			name:       "a restart during the role change continues it",
			infos:      []*redis.ReplicationInfo{replicaInfo("failover-in-progress", handoverTarget), replicaInfo("no-failover", handoverTarget)},
			targetRole: ptrTo(true),
			want:       HandoverDone,
		},
		{
			name:  "Redis before 6.2 has no master_failover_state",
			infos: []*redis.ReplicationInfo{masterInfo("")},
			want:  HandoverUnsupported,
		},
		{
			name:        "a rename-command disables FAILOVER",
			infos:       []*redis.ReplicationInfo{masterInfo("no-failover")},
			failover:    true,
			failoverErr: errors.New("ERR unknown command 'FAILOVER', with args beginning with: 'TO' "),
			want:        HandoverUnsupported,
		},
		{
			name:        "the target is not an online replica",
			infos:       []*redis.ReplicationInfo{masterInfo("no-failover")},
			failover:    true,
			failoverErr: errors.New("ERR FAILOVER target HOST and PORT is not a replica."),
			want:        HandoverRefused,
		},
		{
			name:        "an ACL denies FAILOVER",
			infos:       []*redis.ReplicationInfo{masterInfo("no-failover")},
			failover:    true,
			failoverErr: errors.New("NOPERM User operator has no permissions to run the 'failover' command"),
			want:        HandoverRefused,
		},
		{
			name:        "FAILOVER fails",
			infos:       []*redis.ReplicationInfo{masterInfo("no-failover")},
			failover:    true,
			failoverErr: errBoom,
			wantErr:     "boom",
		},
		{
			name:    "INFO fails",
			infoErr: errBoom,
			wantErr: "boom",
		},
		{
			name:     "INFO fails in the wait",
			infos:    []*redis.ReplicationInfo{masterInfo("no-failover")},
			failover: true,
			wantErr:  "boom",
		},
		{
			name:     "the old master follows a different master",
			infos:    []*redis.ReplicationInfo{masterInfo("no-failover"), replicaInfo("no-failover", "10.0.0.3")},
			failover: true,
			wantErr:  "is a replica of 10.0.0.3",
		},
		{
			name:       "the target is not a master",
			infos:      []*redis.ReplicationInfo{masterInfo("no-failover"), replicaInfo("no-failover", handoverTarget)},
			failover:   true,
			targetRole: ptrTo(false),
			wantErr:    "is not a master",
		},
		{
			name:      "the role of the target is unknown",
			infos:     []*redis.ReplicationInfo{masterInfo("no-failover"), replicaInfo("no-failover", handoverTarget)},
			failover:  true,
			targetErr: errBoom,
			wantErr:   "boom",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			shortHandoverWait(t)
			mr := &mRedisService.Client{}
			reads, aborted := 0, false
			mr.On("GetReplicationInfo", handoverMaster, "6379", "").Return(func(string, string, string) (*redis.ReplicationInfo, error) {
				switch {
				case aborted && test.afterAbort == nil:
					return nil, errBoom
				case aborted:
					return test.afterAbort, nil
				case test.infoErr != nil, test.name == "INFO fails in the wait" && reads == len(test.infos):
					return nil, errBoom
				}
				// The last reply repeats until the wait ends.
				info := test.infos[min(reads, len(test.infos)-1)]
				reads++
				return info, nil
			})
			runIDs := test.runIDs
			if runIDs == nil {
				runIDs = []string{"run-1"}
			}
			runReads := 0
			mr.On("GetRunID", handoverTarget, "6379", "").Maybe().Return(func(string, string, string) (string, error) {
				id := runIDs[min(runReads, len(runIDs)-1)]
				runReads++
				if id == "" {
					return "", errBoom
				}
				return id, nil
			})
			if test.abort != nil {
				mr.On("FailoverAbort", handoverMaster, "6379", "").Once().Return(func(string, string, string) error {
					aborted = true
					return *test.abort
				})
			}
			if test.failover {
				mr.On("FailoverTo", handoverMaster, "6379", "", handoverTarget, handoverWritePause).Once().Return(test.failoverErr)
			}
			if test.targetRole != nil || test.targetErr != nil {
				role := test.targetRole != nil && *test.targetRole
				mr.On("IsMaster", handoverTarget, "6379", "").Once().Return(role, test.targetErr)
			}
			healer := NewRedisFailoverHealer(&mK8SService.Services{}, mr, log.Dummy)

			got, err := healer.HandOverMaster(handoverMaster, handoverTarget, handoverRF())

			if test.wantErr != "" {
				assert.ErrorContains(t, err, test.wantErr)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, test.want, got)
			}
			if !test.failover {
				mr.AssertNotCalled(t, "FailoverTo", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
			}
			if test.abort == nil {
				mr.AssertNotCalled(t, "FailoverAbort", mock.Anything, mock.Anything, mock.Anything)
			}
			if test.maxReads > 0 {
				assert.LessOrEqual(t, reads, test.maxReads, "the abort came only at the end of the wait")
			}
			mr.AssertExpectations(t)
		})
	}
}

func TestHandOverMasterPasswordError(t *testing.T) {
	rf := handoverRF()
	rf.Spec.Auth.SecretPath = "redis-secret"
	ms := &mK8SService.Services{}
	ms.On("GetSecret", "testns", "redis-secret").Once().Return(nil, errors.New("secret unavailable"))
	healer := NewRedisFailoverHealer(ms, &mRedisService.Client{}, log.Dummy)

	_, err := healer.HandOverMaster(handoverMaster, handoverTarget, rf)

	assert.ErrorContains(t, err, "secret unavailable")
}

func TestAbortOrphanedFailover(t *testing.T) {
	errBoom := errors.New("boom")
	tests := []struct {
		name    string
		info    *redis.ReplicationInfo
		infoErr error
		abort   *error
		want    bool
		wantErr string
	}{
		{name: "a role change runs", info: replicaInfo("failover-in-progress", handoverTarget), abort: ptrTo[error](nil), want: true},
		{name: "a catch-up runs", info: masterInfo("waiting-for-sync"), abort: ptrTo[error](nil), want: true},
		{name: "the failover ends just before the abort", info: masterInfo("waiting-for-sync"), abort: ptrTo(errors.New("ERR No failover in progress.")), want: true},
		{name: "the abort fails", info: masterInfo("waiting-for-sync"), abort: ptrTo(errBoom), wantErr: "boom"},
		{name: "no failover runs", info: masterInfo("no-failover")},
		{name: "Redis before 6.2", info: masterInfo("")},
		// The checks of the master handle a master that does not answer.
		{name: "the master does not answer", infoErr: errBoom},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mr := &mRedisService.Client{}
			mr.On("GetReplicationInfo", handoverMaster, "6379", "").Once().Return(test.info, test.infoErr)
			if test.abort != nil {
				mr.On("FailoverAbort", handoverMaster, "6379", "").Once().Return(*test.abort)
			}
			healer := NewRedisFailoverHealer(&mK8SService.Services{}, mr, log.Dummy)

			got, err := healer.AbortOrphanedFailover(handoverMaster, handoverRF())

			if test.wantErr != "" {
				assert.ErrorContains(t, err, test.wantErr)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, test.want, got)
			}
			mr.AssertExpectations(t)
		})
	}
}

func TestAbortOrphanedFailoverPasswordError(t *testing.T) {
	rf := handoverRF()
	rf.Spec.Auth.SecretPath = "redis-secret"
	ms := &mK8SService.Services{}
	ms.On("GetSecret", "testns", "redis-secret").Once().Return(nil, errors.New("secret unavailable"))
	healer := NewRedisFailoverHealer(ms, &mRedisService.Client{}, log.Dummy)

	_, err := healer.AbortOrphanedFailover(handoverMaster, rf)

	assert.ErrorContains(t, err, "secret unavailable")
}

func ptrTo[T any](v T) *T { return &v }
