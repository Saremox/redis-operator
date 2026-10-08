package service_test

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	mK8SService "github.com/saremox/redis-operator/mocks/service/k8s"
	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
)

var updateGolden = flag.Bool("update-golden", false, "write the golden files of TestNonTLSObjectsUnchanged")

// TestNonTLSObjectsUnchanged compares the generated StatefulSet, Redis
// ConfigMap and Services of RedisFailovers without TLS with golden files. A
// change of the StatefulSet restarts each Redis pod of each user on the
// upgrade. Regenerate the files with -update-golden only for an intended
// change.
func TestNonTLSObjectsUnchanged(t *testing.T) {
	cases := map[string]func(rf *redisfailoverv1.RedisFailover){
		"default": func(*redisfailoverv1.RedisFailover) {},
		"auth-exporter-pvc": func(rf *redisfailoverv1.RedisFailover) {
			rf.Spec.Auth.SecretPath = "auth"
			rf.Spec.Redis.Port = 7000
			rf.Spec.Redis.Exporter.Enabled = true
			rf.Spec.Redis.CustomCommandRenames = []redisfailoverv1.RedisCommandRename{{From: "flushall", To: "x"}}
			rf.Spec.Redis.Storage.PersistentVolumeClaim = &redisfailoverv1.EmbeddedPersistentVolumeClaim{
				EmbeddedObjectMetadata: redisfailoverv1.EmbeddedObjectMetadata{Name: "data"},
				Spec: corev1.PersistentVolumeClaimSpec{
					Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}},
				},
			}
		},
		"sentinel": func(rf *redisfailoverv1.RedisFailover) {
			rf.Spec.Sentinel.Enabled = ptr.To(true)
			rf.Spec.Redis.Exporter.Enabled = true
			rf.Spec.Redis.Exporter.Port = 9200
		},
	}
	for name, modify := range cases {
		t.Run(name, func(t *testing.T) {
			rf := generateRF()
			modify(rf)
			require.NoError(t, rf.Validate())

			objects := map[string]any{}
			ms := &mK8SService.Services{}
			ms.On("GetSecret", namespace, "auth").Return(&corev1.Secret{Data: map[string][]byte{"password": []byte("pw")}}, nil)
			ms.On("CreateOrUpdatePodDisruptionBudget", namespace, mock.Anything).Return(nil)
			ms.On("CreateOrUpdateStatefulSet", namespace, mock.Anything).Run(func(args mock.Arguments) {
				objects["statefulset"] = args.Get(1).(*appsv1.StatefulSet)
			}).Return(nil)
			ms.On("CreateOrUpdateConfigMap", namespace, mock.Anything).Run(func(args mock.Arguments) {
				objects["configmap"] = args.Get(1).(*corev1.ConfigMap)
			}).Return(nil)
			ms.On("CreateOrUpdateService", namespace, mock.Anything).Run(func(args mock.Arguments) {
				svc := args.Get(1).(*corev1.Service)
				objects["service-"+svc.Name] = svc
			}).Return(nil)

			client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
			refs := []metav1.OwnerReference{{Name: name}}
			require.NoError(t, client.EnsureRedisStatefulset(rf, nil, refs))
			require.NoError(t, client.EnsureRedisConfigMap(rf, nil, refs))
			require.NoError(t, client.EnsureRedisMasterService(rf, nil, refs))
			require.NoError(t, client.EnsureRedisSlaveService(rf, nil, refs))
			require.NoError(t, client.EnsureRedisService(rf, nil, refs))

			got, err := json.MarshalIndent(objects, "", "  ")
			require.NoError(t, err)
			file := filepath.Join("testdata", "nontls-"+name+".json")
			if *updateGolden {
				require.NoError(t, os.WriteFile(file, append(got, '\n'), 0o644))
			}
			want, err := os.ReadFile(file)
			require.NoError(t, err)
			require.JSONEq(t, string(want), string(got))
		})
	}
}
