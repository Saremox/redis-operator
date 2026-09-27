package redisfailover

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	mRFService "github.com/saremox/redis-operator/mocks/operator/redisfailover/service"
	mK8SService "github.com/saremox/redis-operator/mocks/service/k8s"
)

func newPasswordTestHandler(password *string) (*RedisFailoverHandler, *redisfailoverv1.RedisFailover, *mRFService.RedisFailoverHeal) {
	rf := &redisfailoverv1.RedisFailover{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "testns"},
		Spec:       redisfailoverv1.RedisFailoverSpec{Auth: redisfailoverv1.AuthSettings{SecretPath: "redis-auth"}},
	}
	ms := &mK8SService.Services{}
	ms.On("GetSecret", "testns", "redis-auth").Return(func(string, string) (*corev1.Secret, error) {
		return &corev1.Secret{Data: map[string][]byte{"password": []byte(*password)}}, nil
	})
	ms.On("UpdateRedisFailoverStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return()
	mrfh := &mRFService.RedisFailoverHeal{}
	handler := NewRedisFailoverHandler(Config{}, &mRFService.RedisFailoverClient{}, &mRFService.RedisFailoverCheck{}, mrfh, ms, metrics.Dummy, log.Dummy)
	return handler, rf, mrfh
}

func TestApplyPasswordRemembersAcceptedPassword(t *testing.T) {
	password := "v1"
	handler, rf, mrfh := newPasswordTestHandler(&password)

	// With none known, the password every running pod accepts is remembered,
	// and the Sentinels are retried until every one has it.
	mrfh.On("ApplyPassword", rf, "v1", "v1").Once().Return(false, nil)
	mrfh.On("ApplySentinelPassword", rf, "v1").Once().Return(false, nil)
	assert.NoError(t, handler.applyPassword(rf))
	mrfh.On("ApplySentinelPassword", rf, "v1").Once().Return(true, nil)
	assert.NoError(t, handler.applyPassword(rf))

	// Nothing to check while the secret is unchanged.
	assert.NoError(t, handler.applyPassword(rf))

	// A changed secret is applied with the password the pods accepted. The
	// Sentinels get it as soon as the running pods do, while a pod yet to
	// start keeps the old password in use for the pods.
	password = "v2"
	mrfh.On("ApplyPassword", rf, "v2", "v1").Twice().Return(false, nil)
	mrfh.On("ApplySentinelPassword", rf, "v2").Once().Return(true, nil)
	assert.NoError(t, handler.applyPassword(rf))
	assert.NoError(t, handler.applyPassword(rf))
	mrfh.On("ApplyPassword", rf, "v2", "v1").Once().Return(true, nil)
	assert.NoError(t, handler.applyPassword(rf))
	assert.NoError(t, handler.applyPassword(rf))

	mrfh.AssertExpectations(t)
	mrfh.AssertNumberOfCalls(t, "ApplyPassword", 4)
	mrfh.AssertNumberOfCalls(t, "ApplySentinelPassword", 3)
}

func TestApplyPasswordForgetsDeletedRedisFailover(t *testing.T) {
	password := "v1"
	handler, rf, _ := newPasswordTestHandler(&password)
	handler.passwords.Store(passwordKey(rf), passwordState{redis: "v1", sentinel: "v1"})

	now := metav1.Now()
	rf.DeletionTimestamp = &now
	rf.Finalizers = []string{redisFailoverFinalizer}
	ms := handler.k8sservice.(*mK8SService.Services)
	ms.On("PatchRedisFailoverFinalizers", mock.Anything, "testns", "test", mock.Anything, mock.Anything).Return(nil)
	assert.NoError(t, handler.Handle(context.Background(), rf))

	_, cached := handler.passwords.Load(passwordKey(rf))
	assert.False(t, cached)
}

func TestCheckAndHealReportsAnUnappliedPassword(t *testing.T) {
	boom := errors.New("boom")
	tests := []struct {
		name        string
		applyErr    error
		sentinelErr error
	}{
		{name: "failed apply", applyErr: boom},
		{name: "failed sentinel apply", sentinelErr: boom},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			password := "v1"
			handler, rf, mrfh := newPasswordTestHandler(&password)
			mrfh.On("ApplyPassword", rf, "v1", "v1").Return(true, test.applyErr)
			mrfh.On("ApplySentinelPassword", rf, "v1").Return(false, test.sentinelErr)

			assert.ErrorIs(t, handler.CheckAndHeal(rf), boom)
			assert.Equal(t, redisfailoverv1.NotHealthyState, rf.Status.State)
			assert.Equal(t, "unable to apply the configured password", rf.Status.Message)
			_, cached := handler.passwords.Load(passwordKey(rf))
			assert.False(t, cached)
		})
	}
}

func TestCheckAndHealReportsAnUnreadableSecret(t *testing.T) {
	rf := &redisfailoverv1.RedisFailover{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "testns"},
		Spec:       redisfailoverv1.RedisFailoverSpec{Auth: redisfailoverv1.AuthSettings{SecretPath: "redis-auth"}},
	}
	boom := errors.New("boom")
	ms := &mK8SService.Services{}
	ms.On("GetSecret", "testns", "redis-auth").Return(nil, boom)
	ms.On("UpdateRedisFailoverStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return()
	handler := NewRedisFailoverHandler(Config{}, &mRFService.RedisFailoverClient{}, &mRFService.RedisFailoverCheck{}, &mRFService.RedisFailoverHeal{}, ms, metrics.Dummy, log.Dummy)

	assert.ErrorIs(t, handler.CheckAndHeal(rf), boom)
	assert.Equal(t, "unable to apply the configured password", rf.Status.Message)
}
