package k8s

import (
	"context"
	"testing"

	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	mmetrics "github.com/saremox/redis-operator/mocks/metrics"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"

	errors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubernetes "k8s.io/client-go/kubernetes/fake"
	kubetesting "k8s.io/client-go/testing"
)

func TestSecretServiceGet(t *testing.T) {

	t.Run("Test getting a secret", func(t *testing.T) {
		assertTest := assert.New(t)

		secret := corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test_secret",
				Namespace: "test_namespace",
			},
			Data: map[string][]byte{
				"foo": []byte("bar"),
			},
		}

		mcli := &kubernetes.Clientset{}
		mcli.AddReactor("create", "secrets", func(action kubetesting.Action) (bool, runtime.Object, error) {
			return true, &secret, nil
		})
		mcli.AddReactor("get", "secrets", func(action kubetesting.Action) (bool, runtime.Object, error) {
			a := (action).(kubetesting.GetActionImpl)
			if a.Namespace == secret.Namespace && a.Name == secret.Name {
				return true, &secret, nil
			}
			return true, nil, errors.NewNotFound(action.GetResource().GroupResource(), a.Name)
		})

		_, err := mcli.CoreV1().Secrets(secret.Namespace).Create(context.TODO(), &secret, metav1.CreateOptions{})
		assertTest.NoError(err)

		// test getting the secret
		service := NewSecretService(mcli, log.Dummy, metrics.Dummy)
		ss, err := service.GetSecret(secret.Namespace, secret.Name)
		assertTest.NotNil(ss)
		assertTest.NoError(err)

		// test getting a nonexistent secret
		_, err = service.GetSecret(secret.Namespace, secret.Name+"nonexistent")
		assertTest.Error(err)
		assertTest.True(errors.IsNotFound(err))
	})
}

func TestSecretServiceGetMetricLabel(t *testing.T) {
	for _, name := range []string{"auth", "team/auth-secret-with-a-long-name"} {
		t.Run(name, func(t *testing.T) {
			cli := kubernetes.NewSimpleClientset()
			cli.PrependReactor("get", "secrets", func(action kubetesting.Action) (bool, runtime.Object, error) {
				return true, &corev1.Secret{}, nil
			})
			rec := &mmetrics.Recorder{}
			rec.On("RecordK8sOperation", "ns", "Secret", metrics.NOT_APPLICABLE, "GET", metrics.SUCCESS, metrics.NOT_APPLICABLE).Once()

			_, err := NewSecretService(cli, log.Dummy, rec).GetSecret("ns", name)

			assert.NoError(t, err)
			rec.AssertExpectations(t)
		})
	}
}
