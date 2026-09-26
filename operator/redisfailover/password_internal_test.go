package redisfailover

import (
	"testing"

	"github.com/stretchr/testify/assert"
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
