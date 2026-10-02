package utils

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The client constructors don't dial, so an unreachable server is enough.
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
