package service_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	mK8SService "github.com/saremox/redis-operator/mocks/service/k8s"
	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
)

func TestRedisConfigMapExcludesPassword(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()
	rf.Spec.Auth = redisfailoverv1.AuthSettings{SecretPath: "my-redis-secret"}

	var cm *corev1.ConfigMap
	ms := &mK8SService.Services{}
	ms.On("CreateOrUpdateConfigMap", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
		cm = args.Get(1).(*corev1.ConfigMap)
	}).Return(nil)

	client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
	err := client.EnsureRedisConfigMap(rf, nil, []metav1.OwnerReference{})

	assert.NoError(err)
	for key, value := range cm.Data {
		assert.NotContains(value, "requirepass", "%s must not embed requirepass", key)
		assert.NotContains(value, "masterauth", "%s must not embed masterauth", key)
	}
}

func TestRedisCommandUsesEnvAuthWhenSecretConfigured(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()
	rf.Spec.Auth = redisfailoverv1.AuthSettings{SecretPath: "my-redis-secret"}

	var gotCommand []string
	ms := &mK8SService.Services{}
	ms.On("CreateOrUpdatePodDisruptionBudget", namespace, mock.Anything).Once().Return(nil, nil)
	// Unlike upstream dnse-tech, this fork's EnsureRedisStatefulset still
	// eagerly fetches the password for the liveness-probe/pinger ACL setup
	// (generateRedisStatefulSet embeds it into probe commands), even though
	// the redis command itself now sources REDIS_PASSWORD from the env.
	ms.On("GetSecret", namespace, "my-redis-secret").Once().Return(&corev1.Secret{
		Data: map[string][]byte{"password": []byte("pw1")},
	}, nil)
	ms.On("CreateOrUpdateStatefulSet", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
		gotCommand = args.Get(1).(*appsv1.StatefulSet).Spec.Template.Spec.Containers[0].Command
	}).Return(nil)

	client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
	err := client.EnsureRedisStatefulset(rf, nil, []metav1.OwnerReference{})

	assert.NoError(err)
	// Shell wrapper so $REDIS_PASSWORD expands; exec keeps redis as PID 1.
	assert.Equal([]string{"sh", "-c"}, gotCommand[:2])
	joined := strings.Join(gotCommand, " ")
	assert.Contains(joined, "exec redis-server")
	assert.Contains(joined, `--requirepass "$REDIS_PASSWORD"`)
	assert.Contains(joined, `--masterauth "$REDIS_PASSWORD"`)
}
