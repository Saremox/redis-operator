package redisfailover

import (
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

func TestApplyPasswordRemembersAcceptedPassword(t *testing.T) {
	rf := &redisfailoverv1.RedisFailover{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "testns"},
		Spec:       redisfailoverv1.RedisFailoverSpec{Auth: redisfailoverv1.AuthSettings{SecretPath: "redis-auth"}},
	}
	password := "v1"
	ms := &mK8SService.Services{}
	ms.On("GetSecret", "testns", "redis-auth").Return(func(string, string) (*corev1.Secret, error) {
		return &corev1.Secret{Data: map[string][]byte{"password": []byte(password)}}, nil
	})
	mrfh := &mRFService.RedisFailoverHeal{}
	handler := NewRedisFailoverHandler(Config{}, &mRFService.RedisFailoverClient{}, &mRFService.RedisFailoverCheck{}, mrfh, ms, metrics.Dummy, log.Dummy)

	// Unknown until every pod has accepted it once.
	mrfh.On("ApplyPassword", rf, "v1").Once().Return(false, nil)
	assert.NoError(t, handler.applyPassword(rf))
	mrfh.On("ApplyPassword", rf, "v1").Once().Return(true, nil)
	assert.NoError(t, handler.applyPassword(rf))

	// Nothing to check while the secret is unchanged.
	assert.NoError(t, handler.applyPassword(rf))

	// A changed secret is applied with the password the pods accepted.
	password = "v2"
	mrfh.On("ApplyPassword", rf, "v1").Once().Return(true, nil)
	assert.NoError(t, handler.applyPassword(rf))
	assert.NoError(t, handler.applyPassword(rf))

	mrfh.AssertExpectations(t)
	mrfh.AssertNumberOfCalls(t, "ApplyPassword", 3)
}

func TestCheckAndHealReportsAnUnappliedPassword(t *testing.T) {
	boom := errors.New("boom")
	tests := []struct {
		name      string
		secretErr error
		applyErr  error
	}{
		{name: "unreadable secret", secretErr: boom},
		{name: "failed apply", applyErr: boom},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rf := &redisfailoverv1.RedisFailover{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "testns"},
				Spec:       redisfailoverv1.RedisFailoverSpec{Auth: redisfailoverv1.AuthSettings{SecretPath: "redis-auth"}},
			}
			ms := &mK8SService.Services{}
			ms.On("GetSecret", "testns", "redis-auth").Return(&corev1.Secret{Data: map[string][]byte{"password": []byte("v1")}}, test.secretErr)
			ms.On("UpdateRedisFailoverStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return()
			mrfh := &mRFService.RedisFailoverHeal{}
			mrfh.On("ApplyPassword", rf, "v1").Return(false, test.applyErr)
			handler := NewRedisFailoverHandler(Config{}, &mRFService.RedisFailoverClient{}, &mRFService.RedisFailoverCheck{}, mrfh, ms, metrics.Dummy, log.Dummy)

			assert.ErrorIs(t, handler.CheckAndHeal(rf), boom)
			assert.Equal(t, redisfailoverv1.NotHealthyState, rf.Status.State)
			assert.Equal(t, "unable to apply the configured password", rf.Status.Message)
			_, cached := handler.passwords.Load("testns/test")
			assert.False(t, cached)
		})
	}
}
