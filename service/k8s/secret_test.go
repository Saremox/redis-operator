package k8s

import (
	"context"
	"testing"

	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	mmetrics "github.com/saremox/redis-operator/mocks/metrics"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
		mcli.AddReactor("create", "secrets", func(_ kubetesting.Action) (bool, runtime.Object, error) {
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
		require.NoError(t, err)

		// test getting the secret
		service := NewSecretService(mcli, log.Dummy, metrics.Dummy)
		ss, err := service.GetSecret(secret.Namespace, secret.Name)
		assertTest.NotNil(ss)
		require.NoError(t, err)

		// test getting a nonexistent secret
		_, err = service.GetSecret(secret.Namespace, secret.Name+"nonexistent")
		require.Error(t, err)
		assertTest.True(errors.IsNotFound(err))
	})
}

func TestSecretServiceGetMetricLabel(t *testing.T) {
	for _, name := range []string{"auth", "team/auth-secret-with-a-long-name"} {
		t.Run(name, func(t *testing.T) {
			cli := kubernetes.NewSimpleClientset()
			cli.PrependReactor("get", "secrets", func(_ kubetesting.Action) (bool, runtime.Object, error) {
				return true, &corev1.Secret{}, nil
			})
			rec := &mmetrics.Recorder{}
			rec.On("RecordK8sOperation", "ns", "Secret", metrics.NOT_APPLICABLE, "GET", metrics.SUCCESS, metrics.NOT_APPLICABLE).Once()

			_, err := NewSecretService(cli, log.Dummy, rec).GetSecret("ns", name)

			require.NoError(t, err)
			rec.AssertExpectations(t)
		})
	}
}

func appliedSecret(owner, value string) *corev1.Secret {
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "rfr-auth-applied-test", Namespace: "ns"},
		Data:       map[string][]byte{"redis": []byte(value)},
	}
	if owner != "" {
		controller := true
		s.OwnerReferences = []metav1.OwnerReference{{APIVersion: "databases.spotahome.com/v1", Kind: "RedisFailover", Name: owner, Controller: &controller}}
	}
	return s
}

func TestSecretServiceCreateOrUpdate(t *testing.T) {
	cli := kubernetes.NewSimpleClientset()
	rec := &mmetrics.Recorder{}
	rec.On("RecordK8sOperation", "ns", "Secret", metrics.NOT_APPLICABLE, "GET", metrics.FAIL, metrics.K8S_NOT_FOUND).Once()
	rec.On("RecordK8sOperation", "ns", "Secret", metrics.NOT_APPLICABLE, "GET", metrics.SUCCESS, metrics.NOT_APPLICABLE).Once()
	rec.On("RecordK8sOperation", "ns", "Secret", "rfr-auth-applied-test", "CREATE", metrics.SUCCESS, metrics.NOT_APPLICABLE).Once()
	rec.On("RecordK8sOperation", "ns", "Secret", "rfr-auth-applied-test", "UPDATE", metrics.SUCCESS, metrics.NOT_APPLICABLE).Once()
	service := NewSecretService(cli, log.Dummy, rec)

	require.NoError(t, service.CreateOrUpdateSecret("ns", appliedSecret("test", "v1")))
	stored, err := cli.CoreV1().Secrets("ns").Get(context.TODO(), "rfr-auth-applied-test", metav1.GetOptions{})
	require.NoError(t, err)
	stored.ResourceVersion = "7"
	_, err = cli.CoreV1().Secrets("ns").Update(context.TODO(), stored, metav1.UpdateOptions{})
	require.NoError(t, err)

	update := appliedSecret("test", "v2")
	require.NoError(t, service.CreateOrUpdateSecret("ns", update))
	rec.AssertExpectations(t)

	got, err := cli.CoreV1().Secrets("ns").Get(context.TODO(), "rfr-auth-applied-test", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "v2", string(got.Data["redis"]))
	assert.Equal(t, "7", update.ResourceVersion)
}

func TestSecretServiceCreateOrUpdateKeepsAForeignSecret(t *testing.T) {
	for _, owner := range []string{"", "other"} {
		t.Run("owner "+owner, func(t *testing.T) {
			foreign := appliedSecret(owner, "v1")
			cli := kubernetes.NewSimpleClientset(foreign)

			err := NewSecretService(cli, log.Dummy, metrics.Dummy).CreateOrUpdateSecret("ns", appliedSecret("test", "v2"))

			var other *ControlledByOtherError
			require.ErrorAs(t, err, &other)
			if owner == "" {
				require.EqualError(t, err, `cannot manage Secret "rfr-auth-applied-test": it has no controller`)
			}
			got, _ := cli.CoreV1().Secrets("ns").Get(context.TODO(), "rfr-auth-applied-test", metav1.GetOptions{})
			assert.Equal(t, "v1", string(got.Data["redis"]))
			for _, a := range cli.Actions() {
				assert.Equal(t, "get", a.GetVerb())
			}
		})
	}
}

func TestSecretServiceCreateOrUpdateReturnsAReadError(t *testing.T) {
	cli := kubernetes.NewSimpleClientset()
	forbidden := errors.NewForbidden(corev1.Resource("secrets"), "s", nil)
	cli.PrependReactor("get", "secrets", func(kubetesting.Action) (bool, runtime.Object, error) {
		return true, nil, forbidden
	})
	err := NewSecretService(cli, log.Dummy, metrics.Dummy).CreateOrUpdateSecret("ns", appliedSecret("test", "v1"))
	assert.True(t, errors.IsForbidden(err))
	assert.Len(t, cli.Actions(), 1)
}
