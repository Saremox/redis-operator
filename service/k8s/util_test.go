package k8s

import (
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	kubeerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubernetes "k8s.io/client-go/kubernetes/fake"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
)

// newTestServices builds a real k8s.Services implementation backed by a fake
// clientset, with only the Secret sub-service populated -- GetRedisPassword
// only ever calls s.GetSecret, and this is an internal (package k8s) test so
// it can use the unexported `services` struct directly instead of the mocks
// this package deliberately avoids.
func newTestServices(kubeClient *kubernetes.Clientset) Services {
	return &services{
		Secret: NewSecretService(kubeClient, log.Dummy, metrics.Dummy),
	}
}

func TestGetRedisPassword(t *testing.T) {
	testns := "testns"

	t.Run("no SecretPath configured returns a blank password with no error", func(t *testing.T) {
		assertTest := assert.New(t)

		// No secret seeded at all: if GetRedisPassword attempted to read a
		// secret here, the fake Get would fail (not found) and this
		// assertion on a nil error would catch it.
		mcli := kubernetes.NewSimpleClientset()
		s := newTestServices(mcli)

		rf := &redisfailoverv1.RedisFailover{
			ObjectMeta: metav1.ObjectMeta{Namespace: testns},
		}

		password, err := GetRedisPassword(s, rf)
		assertTest.NoError(err)
		assertTest.Equal("", password)
	})

	t.Run("SecretPath set and secret has a password field returns the password", func(t *testing.T) {
		assertTest := assert.New(t)

		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "redis-auth",
				Namespace: testns,
			},
			Data: map[string][]byte{
				"password": []byte("s3cr3t"),
			},
		}
		mcli := kubernetes.NewSimpleClientset(secret)
		s := newTestServices(mcli)

		rf := &redisfailoverv1.RedisFailover{
			ObjectMeta: metav1.ObjectMeta{Namespace: testns},
			Spec: redisfailoverv1.RedisFailoverSpec{
				Auth: redisfailoverv1.AuthSettings{SecretPath: "redis-auth"},
			},
		}

		password, err := GetRedisPassword(s, rf)
		assertTest.NoError(err)
		assertTest.Equal("s3cr3t", password)
	})

	t.Run("SecretPath set but the secret does not exist propagates the error", func(t *testing.T) {
		assertTest := assert.New(t)

		mcli := kubernetes.NewSimpleClientset()
		s := newTestServices(mcli)

		rf := &redisfailoverv1.RedisFailover{
			ObjectMeta: metav1.ObjectMeta{Namespace: testns},
			Spec: redisfailoverv1.RedisFailoverSpec{
				Auth: redisfailoverv1.AuthSettings{SecretPath: "does-not-exist"},
			},
		}

		password, err := GetRedisPassword(s, rf)
		assertTest.Error(err)
		assertTest.Equal("", password)
		assertTest.True(kubeerrors.IsNotFound(err))
	})

	t.Run("SecretPath set and secret exists but has no password field returns a descriptive error", func(t *testing.T) {
		assertTest := assert.New(t)

		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "redis-auth",
				Namespace: testns,
			},
			Data: map[string][]byte{
				"other-field": []byte("irrelevant"),
			},
		}
		mcli := kubernetes.NewSimpleClientset(secret)
		s := newTestServices(mcli)

		rf := &redisfailoverv1.RedisFailover{
			ObjectMeta: metav1.ObjectMeta{Namespace: testns},
			Spec: redisfailoverv1.RedisFailoverSpec{
				Auth: redisfailoverv1.AuthSettings{SecretPath: "redis-auth"},
			},
		}

		password, err := GetRedisPassword(s, rf)
		assertTest.Error(err)
		assertTest.Equal("", password)
		assertTest.Equal(`secret "redis-auth" does not have a password field`, err.Error())
	})
}

