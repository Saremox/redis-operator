package k8s_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	kubeerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kubernetes "k8s.io/client-go/kubernetes/fake"
	kubetesting "k8s.io/client-go/testing"

	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	"github.com/saremox/redis-operator/service/k8s"
)

// The callers check the API errors with apierrors.IsNotFound and similar
// helpers, so the wrapped errors must keep the API status.
func TestServicesWrapTheAPIErrors(t *testing.T) {
	meta := metav1.ObjectMeta{Name: "x", Namespace: "ns"}
	tests := []struct {
		name       string
		call       func(s k8s.Services) error
		wantPrefix string
		wantReason func(error) bool
	}{
		{"get configmap", func(s k8s.Services) error { _, err := s.GetConfigMap("ns", "x"); return err }, "get configmap ns/x: ", kubeerrors.IsNotFound},
		{"create configmap", func(s k8s.Services) error { return s.CreateConfigMap("ns", &corev1.ConfigMap{ObjectMeta: meta}) }, "create configmap ns/x: ", kubeerrors.IsConflict},
		{"update configmap", func(s k8s.Services) error { return s.UpdateConfigMap("ns", &corev1.ConfigMap{ObjectMeta: meta}) }, "update configmap ns/x: ", kubeerrors.IsConflict},
		{"delete configmap", func(s k8s.Services) error { return s.DeleteConfigMap("ns", "x") }, "delete configmap ns/x: ", kubeerrors.IsConflict},
		{"list configmaps", func(s k8s.Services) error { _, err := s.ListConfigMaps("ns"); return err }, "list configmaps in ns: ", kubeerrors.IsConflict},
		{"get deployment", func(s k8s.Services) error { _, err := s.GetDeployment("ns", "x"); return err }, "get deployment ns/x: ", kubeerrors.IsNotFound},
		{"get deployment pods", func(s k8s.Services) error { _, err := s.GetDeploymentPods("ns", "x"); return err }, "get deployment ns/x: ", kubeerrors.IsNotFound},
		{"create deployment", func(s k8s.Services) error { return s.CreateDeployment("ns", &appsv1.Deployment{ObjectMeta: meta}) }, "create deployment ns/x: ", kubeerrors.IsConflict},
		{"update deployment", func(s k8s.Services) error { return s.UpdateDeployment("ns", &appsv1.Deployment{ObjectMeta: meta}) }, "update deployment ns/x: ", kubeerrors.IsConflict},
		{"delete deployment", func(s k8s.Services) error { return s.DeleteDeployment("ns", "x") }, "delete deployment ns/x: ", kubeerrors.IsConflict},
		{"list deployments", func(s k8s.Services) error { _, err := s.ListDeployments("ns"); return err }, "list deployments in ns: ", kubeerrors.IsConflict},
		{"get pod", func(s k8s.Services) error { _, err := s.GetPod("ns", "x"); return err }, "get pod ns/x: ", kubeerrors.IsNotFound},
		{"delete pod", func(s k8s.Services) error { return s.DeletePod("ns", "x") }, "delete pod ns/x: ", kubeerrors.IsConflict},
		{"delete pod with uid", func(s k8s.Services) error { return s.DeletePodWithUID("ns", "x", "uid") }, "delete pod ns/x: ", kubeerrors.IsConflict},
		{"list pods", func(s k8s.Services) error { _, err := s.ListPods("ns"); return err }, "list pods in ns: ", kubeerrors.IsConflict},
		{"patch pod labels", func(s k8s.Services) error { return s.UpdatePodLabels("ns", "x", map[string]string{"a": "b"}) }, "patch the labels of pod ns/x: ", kubeerrors.IsConflict},
		{"patch pod annotations", func(s k8s.Services) error { return s.UpdatePodAnnotations("ns", "x", map[string]string{"a": "b"}) }, "patch the annotations of pod ns/x: ", kubeerrors.IsConflict},
		{"remove pod annotation", func(s k8s.Services) error { return s.RemovePodAnnotation("ns", "x", "a") }, "remove annotation a of pod ns/x: ", kubeerrors.IsConflict},
		{"resize pod", func(s k8s.Services) error {
			return s.ResizePod("ns", "x", map[string]corev1.ResourceRequirements{"redis": {}})
		}, "resize pod ns/x: ", kubeerrors.IsConflict},
		{"get pdb", func(s k8s.Services) error { _, err := s.GetPodDisruptionBudget("ns", "x"); return err }, "get poddisruptionbudget ns/x: ", kubeerrors.IsNotFound},
		{"create pdb", func(s k8s.Services) error {
			return s.CreatePodDisruptionBudget("ns", &policyv1.PodDisruptionBudget{ObjectMeta: meta})
		}, "create poddisruptionbudget ns/x: ", kubeerrors.IsConflict},
		{"update pdb", func(s k8s.Services) error {
			return s.UpdatePodDisruptionBudget("ns", &policyv1.PodDisruptionBudget{ObjectMeta: meta})
		}, "update poddisruptionbudget ns/x: ", kubeerrors.IsConflict},
		{"delete pdb", func(s k8s.Services) error { return s.DeletePodDisruptionBudget("ns", "x") }, "delete poddisruptionbudget ns/x: ", kubeerrors.IsConflict},
		{"get secret", func(s k8s.Services) error { _, err := s.GetSecret("ns", "x"); return err }, "get secret ns/x: ", kubeerrors.IsNotFound},
		{"create secret", func(s k8s.Services) error { return s.CreateOrUpdateSecret("ns", &corev1.Secret{ObjectMeta: meta}) }, "create secret ns/x: ", kubeerrors.IsConflict},
		{"get service", func(s k8s.Services) error { _, err := s.GetService("ns", "x"); return err }, "get service ns/x: ", kubeerrors.IsNotFound},
		{"create service", func(s k8s.Services) error { return s.CreateService("ns", &corev1.Service{ObjectMeta: meta}) }, "create service ns/x: ", kubeerrors.IsConflict},
		{"update service", func(s k8s.Services) error { return s.UpdateService("ns", &corev1.Service{ObjectMeta: meta}) }, "update service ns/x: ", kubeerrors.IsConflict},
		{"delete service", func(s k8s.Services) error { return s.DeleteService("ns", "x") }, "delete service ns/x: ", kubeerrors.IsConflict},
		{"list services", func(s k8s.Services) error { _, err := s.ListServices("ns"); return err }, "list services in ns: ", kubeerrors.IsConflict},
		{"get serviceaccount", func(s k8s.Services) error { _, err := s.GetServiceAccount("ns", "x"); return err }, "get serviceaccount ns/x: ", kubeerrors.IsNotFound},
		{"create serviceaccount", func(s k8s.Services) error {
			return s.CreateServiceAccount("ns", &corev1.ServiceAccount{ObjectMeta: meta})
		}, "create serviceaccount ns/x: ", kubeerrors.IsConflict},
		{"update serviceaccount", func(s k8s.Services) error {
			return s.UpdateServiceAccount("ns", &corev1.ServiceAccount{ObjectMeta: meta})
		}, "update serviceaccount ns/x: ", kubeerrors.IsConflict},
		{"delete serviceaccount", func(s k8s.Services) error { return s.DeleteServiceAccount("ns", "x") }, "delete serviceaccount ns/x: ", kubeerrors.IsConflict},
		{"list serviceaccounts", func(s k8s.Services) error { _, err := s.ListServiceAccounts("ns"); return err }, "list serviceaccounts in ns: ", kubeerrors.IsConflict},
		{"get statefulset", func(s k8s.Services) error { _, err := s.GetStatefulSet("ns", "x"); return err }, "get statefulset ns/x: ", kubeerrors.IsNotFound},
		{"get statefulset pods", func(s k8s.Services) error { _, err := s.GetStatefulSetPods("ns", "x"); return err }, "get statefulset ns/x: ", kubeerrors.IsNotFound},
		{"create statefulset", func(s k8s.Services) error { return s.CreateStatefulSet("ns", &appsv1.StatefulSet{ObjectMeta: meta}) }, "create statefulset ns/x: ", kubeerrors.IsConflict},
		{"update statefulset", func(s k8s.Services) error { return s.UpdateStatefulSet("ns", &appsv1.StatefulSet{ObjectMeta: meta}) }, "update statefulset ns/x: ", kubeerrors.IsConflict},
		{"delete statefulset", func(s k8s.Services) error { return s.DeleteStatefulSet("ns", "x") }, "delete statefulset ns/x: ", kubeerrors.IsConflict},
		{"list statefulsets", func(s k8s.Services) error { _, err := s.ListStatefulSets("ns"); return err }, "list statefulsets in ns: ", kubeerrors.IsConflict},
		{"get controllerrevision", func(s k8s.Services) error { _, err := s.GetControllerRevision("ns", "x"); return err }, "get controllerrevision ns/x: ", kubeerrors.IsNotFound},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := kubernetes.NewClientset()
			client.PrependReactor("*", "*", func(action kubetesting.Action) (bool, runtime.Object, error) {
				gr := schema.GroupResource{Group: action.GetResource().Group, Resource: action.GetResource().Resource}
				if action.GetVerb() == "get" {
					return true, nil, kubeerrors.NewNotFound(gr, "x")
				}
				return true, nil, kubeerrors.NewConflict(gr, "x", nil)
			})
			services := k8s.New(client, nil, log.Dummy, metrics.Dummy)

			err := test.call(services)

			require.Error(t, err)
			assert.True(t, strings.HasPrefix(err.Error(), test.wantPrefix), err.Error())
			assert.True(t, test.wantReason(err), err.Error())
		})
	}
}

func TestCreateOrUpdateSecretWrapsTheUpdateError(t *testing.T) {
	controller := true
	owner := []metav1.OwnerReference{{APIVersion: "databases.spotahome.com/v1", Kind: "RedisFailover", Name: "rf", UID: "uid", Controller: &controller}}
	stored := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "ns", OwnerReferences: owner}}
	client := kubernetes.NewClientset(stored)
	client.PrependReactor("update", "secrets", func(kubetesting.Action) (bool, runtime.Object, error) {
		return true, nil, kubeerrors.NewConflict(schema.GroupResource{Resource: "secrets"}, "x", nil)
	})

	err := k8s.NewSecretService(client, log.Dummy, metrics.Dummy).CreateOrUpdateSecret("ns", stored.DeepCopy())

	require.Error(t, err)
	assert.True(t, strings.HasPrefix(err.Error(), "update secret ns/x: "), err.Error())
	assert.True(t, kubeerrors.IsConflict(err), err.Error())
}
