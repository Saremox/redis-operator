package redisfailover

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	mRFService "github.com/saremox/redis-operator/mocks/operator/redisfailover/service"
	mK8SService "github.com/saremox/redis-operator/mocks/service/k8s"
	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
)

var updateGolden = flag.Bool("update", false, "write the golden files of TestGeneratedObjectsGolden")

const goldenDir = "testdata/golden"

// TestGeneratedObjectsGolden compares each object that Ensure writes for an
// example with a golden file. A change of the generated objects changes the
// pods of each existing RedisFailover, so it must show in the diff.
// Run `go test -run TestGeneratedObjectsGolden -update` to write the files.
func TestGeneratedObjectsGolden(t *testing.T) {
	files, err := filepath.Glob("../../example/redisfailover/*.yaml")
	require.NoError(t, err)
	require.NotEmpty(t, files)
	examples := make([]string, 0, len(files))
	for _, f := range files {
		examples = append(examples, strings.TrimSuffix(filepath.Base(f), ".yaml"))
	}

	stale, err := staleGoldenDirs(goldenDir, examples)
	require.NoError(t, err)
	if *updateGolden {
		for _, dir := range stale {
			require.NoError(t, os.RemoveAll(filepath.Join(goldenDir, dir)))
		}
	} else {
		assert.Empty(t, stale, "golden files of removed examples, run the test with -update")
	}

	for i, f := range files {
		example := examples[i]
		t.Run(example, func(t *testing.T) {
			got := ensuredObjects(t, exampleRedisFailover(t, f))
			dir := filepath.Join(goldenDir, example)
			if *updateGolden {
				require.NoError(t, os.RemoveAll(dir))
				require.NoError(t, os.MkdirAll(dir, 0o755))
				for name, data := range got {
					require.NoError(t, os.WriteFile(filepath.Join(dir, name), data, 0o644))
				}
			}

			entries, err := os.ReadDir(dir)
			require.NoError(t, err, "run the test with -update to write the golden files")
			var stored []string
			for _, e := range entries {
				stored = append(stored, e.Name())
			}
			var generated []string
			for name := range got {
				generated = append(generated, name)
			}
			slices.Sort(generated)
			require.Equal(t, stored, generated, "the generated objects")
			for _, name := range stored {
				want, err := os.ReadFile(filepath.Join(dir, name))
				require.NoError(t, err)
				assert.Equal(t, string(want), string(got[name]), name)
			}
		})
	}
}

// staleGoldenDirs returns the golden directories without an example.
func staleGoldenDirs(dir string, examples []string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var stale []string
	for _, e := range entries {
		if !slices.Contains(examples, e.Name()) {
			stale = append(stale, e.Name())
		}
	}
	return stale, nil
}

func TestStaleGoldenDirs(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"basic", "removed"} {
		require.NoError(t, os.Mkdir(filepath.Join(dir, name), 0o755))
	}
	stale, err := staleGoldenDirs(dir, []string{"basic", "minimum"})
	require.NoError(t, err)
	assert.Equal(t, []string{"removed"}, stale)

	_, err = staleGoldenDirs(filepath.Join(dir, "missing"), nil)
	assert.Error(t, err)
}

// exampleRedisFailover decodes the RedisFailover of an example file with
// unknown fields refused, and applies the defaults of Validate. It does not
// apply the defaults of the CRD schema.
func exampleRedisFailover(t *testing.T, file string) *redisfailoverv1.RedisFailover {
	t.Helper()
	data, err := os.ReadFile(file)
	require.NoError(t, err)
	var rf *redisfailoverv1.RedisFailover
	for _, doc := range regexp.MustCompile(`(?m)^---\s*$`).Split(string(data), -1) {
		var meta metav1.TypeMeta
		require.NoError(t, yaml.Unmarshal([]byte(doc), &meta))
		if meta.Kind != "RedisFailover" {
			continue
		}
		require.Nil(t, rf, "more than one RedisFailover in the file")
		rf = &redisfailoverv1.RedisFailover{}
		require.NoError(t, yaml.UnmarshalStrict([]byte(doc), rf))
	}
	require.NotNil(t, rf, "no RedisFailover in the file")
	if rf.Namespace == "" {
		rf.Namespace = "default"
	}
	require.NoError(t, rf.Validate())
	return rf
}

// ensuredObjects runs Ensure as Handle does and returns each object that it
// creates or updates, as indented JSON with sorted keys. The key is the file
// name: <kind>-<name>.json.
func ensuredObjects(t *testing.T, rf *redisfailoverv1.RedisFailover) map[string][]byte {
	t.Helper()
	got := map[string][]byte{}
	record := func(kind string) func(mock.Arguments) {
		return func(args mock.Arguments) {
			obj := args.Get(1).(metav1.Object)
			name := kind + "-" + obj.GetName() + ".json"
			require.NotContains(t, got, name)
			got[name] = canonicalJSON(t, obj)
		}
	}
	notFound := func(resource string) error {
		return apierrors.NewNotFound(schema.GroupResource{Resource: resource}, "")
	}

	ms := &mK8SService.Services{}
	for _, kind := range []string{"Service", "ConfigMap", "StatefulSet", "Deployment", "ServiceAccount", "PodDisruptionBudget"} {
		ms.On("CreateOrUpdate"+kind, rf.Namespace, mock.Anything).Maybe().Run(record(kind)).Return(nil)
	}
	// The objects of a disabled part do not exist.
	ms.On("GetService", rf.Namespace, mock.Anything).Maybe().Return(nil, notFound("services"))
	ms.On("GetDeployment", rf.Namespace, mock.Anything).Maybe().Return(nil, notFound("deployments"))
	ms.On("GetPodDisruptionBudget", rf.Namespace, mock.Anything).Maybe().Return(nil, notFound("poddisruptionbudgets"))
	ms.On("GetServiceAccount", rf.Namespace, mock.Anything).Maybe().Return(nil, notFound("serviceaccounts"))
	ms.On("GetConfigMap", rf.Namespace, mock.Anything).Maybe().Return(func(_, name string) (*corev1.ConfigMap, error) {
		if name == rf.Spec.Redis.ShutdownConfigMap {
			return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: rf.Namespace}}, nil
		}
		return nil, notFound("configmaps")
	})
	ms.On("GetSecret", rf.Namespace, rf.Spec.Auth.SecretPath).Maybe().Return(&corev1.Secret{Data: map[string][]byte{"password": []byte("golden")}}, nil)

	h := NewRedisFailoverHandler(Config{}, rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy), &mRFService.RedisFailoverCheck{}, &mRFService.RedisFailoverHeal{}, ms, metrics.Dummy, log.Dummy)
	require.NoError(t, h.Ensure(rf, h.getLabels(rf), h.createOwnerReferences(rf), metrics.Dummy))
	ms.AssertExpectations(t)
	return got
}

// canonicalJSON sorts the keys of all objects, so the file does not depend on
// the order of the struct fields.
func canonicalJSON(t *testing.T, obj any) []byte {
	t.Helper()
	data, err := json.Marshal(obj)
	require.NoError(t, err)
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	require.NoError(t, dec.Decode(&v))
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	// The scripts in the ConfigMaps stay readable without >.
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	require.NoError(t, enc.Encode(v))
	return out.Bytes()
}
