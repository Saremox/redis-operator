package utils

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/rest"
)

// The client constructors do not connect, so any server address is sufficient.
const testKubeConfig = `apiVersion: v1
kind: Config
clusters:
- name: test
  cluster:
    server: https://127.0.0.1:1
contexts:
- name: test
  context:
    cluster: test
    user: test
current-context: test
users:
- name: test
  user:
    token: test
`

func TestCreateKubernetesClients(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kubeconfig")
	require.NoError(t, os.WriteFile(path, []byte(testKubeConfig), 0o600))

	kube, custom, meta, err := CreateKubernetesClients(&CMDFlags{Development: true, KubeConfig: path, K8sQueriesPerSecond: 5, K8sQueriesBurstable: 10})

	require.NoError(t, err)
	assert.NotNil(t, kube)
	assert.NotNil(t, custom)
	assert.NotNil(t, meta)
}

// The callers can match the cause of a configuration error. The text of the
// error is the prefix and the text of the cause.
func TestLoadKubernetesConfigWrapsTheCause(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")

	tests := map[string]struct {
		flags  *CMDFlags
		cause  error
		prefix string
	}{
		"missing kubeconfig":  {&CMDFlags{Development: true, KubeConfig: filepath.Join(t.TempDir(), "missing")}, fs.ErrNotExist, "could not load configuration: "},
		"outside the cluster": {&CMDFlags{}, rest.ErrNotInCluster, "error loading kubernetes configuration inside cluster, check app is running outside kubernetes cluster or run in development mode: "},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := LoadKubernetesConfig(test.flags)
			require.ErrorIs(t, err, test.cause)
			assert.EqualError(t, err, test.prefix+errors.Unwrap(err).Error())
		})
	}
}

func TestCreateKubernetesClientsErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kubeconfig")
	require.NoError(t, os.WriteFile(path, []byte(testKubeConfig), 0o600))
	t.Setenv("KUBERNETES_SERVICE_HOST", "")

	tests := map[string]*CMDFlags{
		"missing kubeconfig":  {Development: true, KubeConfig: filepath.Join(t.TempDir(), "missing")},
		"outside the cluster": {},
		"qps without a burst": {Development: true, KubeConfig: path, K8sQueriesPerSecond: 5},
	}
	for name, flags := range tests {
		t.Run(name, func(t *testing.T) {
			_, _, _, err := CreateKubernetesClients(flags)
			assert.Error(t, err)
		})
	}
}
