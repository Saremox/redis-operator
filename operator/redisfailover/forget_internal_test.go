package redisfailover

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	mmetrics "github.com/saremox/redis-operator/mocks/metrics"
)

func trackState(t *testing.T, h *RedisFailoverHandler, rf *redisfailoverv1.RedisFailover) {
	t.Helper()
	require.NoError(t, h.applyPassword(rf))
	h.reportRolloutWait(rf, &rolloutWait{uid: "u", pod: "rfr-test-0", reason: "not synced with the master"})
	h.unreachableCleared.Store(failoverKey(rf), true)
}

func assertTracked(t *testing.T, h *RedisFailoverHandler, key string, want bool) {
	t.Helper()
	_, passwords := h.passwords.Load(key)
	_, waits := h.rolloutWaits.Load(key)
	_, cleared := h.unreachableCleared.Load(key)
	assert.Equal(t, []bool{want, want, want}, []bool{passwords, waits, cleared}, key)
}

func TestForgetDropsTheStateOfOneRedisFailover(t *testing.T) {
	password := "v1"
	h, rf, mrfh := newPasswordTestHandler(&password)
	other := rf.DeepCopy()
	other.Name = "other"
	for _, r := range []*redisfailoverv1.RedisFailover{rf, other} {
		mrfh.On("ApplyPassword", r, "v1", []string{"v1"}).Return(true, nil)
		mrfh.On("ApplySentinelPassword", r, "v1").Return(true, nil)
	}
	mrec := &mmetrics.Recorder{}
	mrec.On("DeleteCluster", "testns", "test").Once()
	h.mClient = mrec
	trackState(t, h, rf)
	trackState(t, h, other)

	h.Forget("testns/test")

	assertTracked(t, h, "testns/test", false)
	assertTracked(t, h, "testns/other", true)
	mrec.AssertExpectations(t)
}

func TestForgetIgnoresAnUnknownKey(t *testing.T) {
	password := "v1"
	h, rf, mrfh := newPasswordTestHandler(&password)
	mrfh.On("ApplyPassword", rf, "v1", []string{"v1"}).Return(true, nil)
	mrfh.On("ApplySentinelPassword", rf, "v1").Return(true, nil)
	mrec := &mmetrics.Recorder{}
	mrec.On("DeleteCluster", "testns", "unknown").Once()
	mrec.On("DeleteCluster", "nokey", "").Once()
	h.mClient = mrec
	trackState(t, h, rf)

	assert.NotPanics(t, func() {
		h.Forget("testns/unknown")
		h.Forget("nokey")
	})

	assertTracked(t, h, "testns/test", true)
	mrec.AssertExpectations(t)
}

// The maps are caches. The next reconcile of a live RedisFailover fills them
// again without a password change.
func TestForgetOfALiveRedisFailoverIsRebuiltByTheNextReconcile(t *testing.T) {
	password := "v1"
	h, rf, mrfh := newPasswordTestHandler(&password)
	mrfh.On("ApplyPassword", rf, "v1", []string{"v1"}).Twice().Return(true, nil)
	mrfh.On("ApplySentinelPassword", rf, "v1").Twice().Return(true, nil)
	mrec := &mmetrics.Recorder{}
	mrec.On("DeleteCluster", "testns", "test").Once()
	h.mClient = mrec

	require.NoError(t, h.applyPassword(rf))
	h.Forget(failoverKey(rf))
	require.NoError(t, h.applyPassword(rf))
	require.NoError(t, h.applyPassword(rf))

	v, tracked := h.passwords.Load("testns/test")
	assert.True(t, tracked)
	assert.Equal(t, passwordState{redis: "v1", applied: "v1", sentinel: "v1"}, v)
	mrfh.AssertExpectations(t)
}
