package redisfailover

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
)

// withoutExpectation drops the expectations of method, so that a test can set
// its own.
func withoutExpectation(m *mock.Mock, method string) {
	calls := m.ExpectedCalls[:0]
	for _, call := range m.ExpectedCalls {
		if call.Method != method {
			calls = append(calls, call)
		}
	}
	m.ExpectedCalls = calls
}

// A FAILOVER at the start of a reconcile has no watcher, for example after an
// operator restart in a handover. Without the abort, the old master stays a
// replica without writes, and the election can promote a second master.
func TestOperatorManagedModeAbortsAnOrphanedFailover(t *testing.T) {
	errBoom := errors.New("boom")
	stop := errors.New("stop here")
	tests := []struct {
		name        string
		labelled    bool
		aborted     bool
		abortErr    error
		wantErr     error
		wantMessage string
		wantRequeue []time.Duration
	}{
		{
			name:        "a failover runs",
			labelled:    true,
			aborted:     true,
			wantMessage: "aborted a FAILOVER of pod rfr-test-0 that no reconcile watched",
			wantRequeue: []time.Duration{time.Second},
		},
		{
			name:        "the abort fails",
			labelled:    true,
			abortErr:    errBoom,
			wantErr:     errBoom,
			wantMessage: "unable to abort a FAILOVER that no reconcile watches",
		},
		{name: "no failover runs", labelled: true, wantErr: stop, wantMessage: "unable to get number of masters"},
		{name: "no pod is labelled master", wantErr: stop, wantMessage: "unable to get number of masters"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ft := newFailoverTimeoutTest(nil)
			if !test.labelled {
				ft.pods[0] = timeoutRedisPod(timeoutMasterPod, timeoutMaster, false, true)
			}
			withoutExpectation(&ft.healer.Mock, "AbortOrphanedFailover")
			if test.labelled {
				ft.healer.On("AbortOrphanedFailover", timeoutMaster, ft.rf).Once().Return(test.aborted, test.abortErr)
			}
			ft.checker.On("IsRedisRunningQuorum", ft.rf).Once().Return(true)
			if !test.aborted && test.abortErr == nil {
				ft.checker.On("GetNumberMasters", ft.rf).Once().Return(0, stop)
			}

			err := ft.handler.CheckAndHeal(ft.rf)

			assert.Equal(t, test.wantErr, err)
			assert.Equal(t, redisfailoverv1.NotHealthyState, ft.rf.Status.State)
			assert.Equal(t, test.wantMessage, ft.rf.Status.Message)
			assert.Equal(t, test.wantRequeue, ft.requeues)
			ft.assertExpectations(t)
		})
	}
}
