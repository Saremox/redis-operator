package service_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	apiextensionsfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	redisfailoverfake "github.com/saremox/redis-operator/client/k8s/clientset/versioned/fake"
	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
	"github.com/saremox/redis-operator/service/k8s"
)

// sentinelLifecycleRF returns an RF with Sentinel enabled and no
// user-supplied ServiceAccountName, so every Sentinel resource in play
// (Service, ConfigMap, Deployment, PodDisruptionBudget, ServiceAccount) is
// operator-provisioned and therefore operator-owned to clean up.
func sentinelLifecycleRF() *redisfailoverv1.RedisFailover {
	return &redisfailoverv1.RedisFailover{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "lifecycle",
			Namespace: "lifecycle-ns",
		},
		Spec: redisfailoverv1.RedisFailoverSpec{
			Redis: redisfailoverv1.RedisSettings{
				Replicas: int32(3),
			},
			Sentinel: redisfailoverv1.SentinelSettings{
				Enabled:  ptr.To(true),
				Replicas: int32(3),
			},
		},
	}
}

// sentinelResourceInventory counts every live object, across every resource
// kind Sentinel can own, that carries this RF's Sentinel selector labels
// (app.kubernetes.io/name=<rf>,app.kubernetes.io/component=sentinel). It is
// deliberately inventory-based rather than a hand-maintained list of expected
// names: it counts whatever actually exists, so a future resource kind added
// to EnsureSentinelDeployment (the way the ServiceAccount was, before it was
// found missing from cleanup) is caught by this test going stale-empty
// instead of silently leaking, with no per-resource-kind test edit needed.
func sentinelResourceInventory(t *testing.T, kubecli *kubefake.Clientset, rf *redisfailoverv1.RedisFailover) map[string]int {
	t.Helper()
	selector := "app.kubernetes.io/name=" + rf.Name + ",app.kubernetes.io/component=sentinel"
	listOpts := metav1.ListOptions{LabelSelector: selector}

	deployments, err := kubecli.AppsV1().Deployments(rf.Namespace).List(t.Context(), listOpts)
	assert.NoError(t, err)
	services, err := kubecli.CoreV1().Services(rf.Namespace).List(t.Context(), listOpts)
	assert.NoError(t, err)
	configMaps, err := kubecli.CoreV1().ConfigMaps(rf.Namespace).List(t.Context(), listOpts)
	assert.NoError(t, err)
	pdbs, err := kubecli.PolicyV1().PodDisruptionBudgets(rf.Namespace).List(t.Context(), listOpts)
	assert.NoError(t, err)
	serviceAccounts, err := kubecli.CoreV1().ServiceAccounts(rf.Namespace).List(t.Context(), listOpts)
	assert.NoError(t, err)

	return map[string]int{
		"Deployment":          len(deployments.Items),
		"Service":             len(services.Items),
		"ConfigMap":           len(configMaps.Items),
		"PodDisruptionBudget": len(pdbs.Items),
		"ServiceAccount":      len(serviceAccounts.Items),
	}
}

// TestSentinelResourceLifecycleSymmetry reproduces, at the level of the real
// RedisFailoverKubeClient against a fake API server (not mocks), the class of
// gap that let a Sentinel ServiceAccount survive EnsureNotPresentSentinelResources
// after sentinel.enabled flipped to false (see #163): everything Sentinel
// creates on enable must be gone after disable, counted by inventory rather
// than by a hand-written list of names.
func TestSentinelResourceLifecycleSymmetry(t *testing.T) {
	rf := sentinelLifecycleRF()
	labels := map[string]string{}
	ownerRefs := []metav1.OwnerReference{}

	kubecli := kubefake.NewClientset()
	crdcli := redisfailoverfake.NewSimpleClientset()
	apiextcli := apiextensionsfake.NewSimpleClientset()
	k8sService := k8s.New(kubecli, crdcli, apiextcli, log.Dummy, metrics.Dummy)
	client := rfservice.NewRedisFailoverKubeClient(k8sService, log.Dummy, metrics.Dummy)

	// Create every Sentinel resource the way Ensure() (ensurer.go) does when
	// sentinelsAllowed is true.
	assert.NoError(t, client.EnsureSentinelService(rf, labels, ownerRefs))
	assert.NoError(t, client.EnsureSentinelConfigMap(rf, labels, ownerRefs))
	assert.NoError(t, client.EnsureSentinelDeployment(rf, labels, ownerRefs))

	before := sentinelResourceInventory(t, kubecli, rf)
	for kind, count := range before {
		assert.Equalf(t, 1, count, "expected exactly one %s to exist after Ensure*, found %d", kind, count)
	}

	// Disable Sentinel the way Ensure() does when sentinelsAllowed flips to false.
	assert.NoError(t, client.EnsureNotPresentSentinelResources(rf))

	after := sentinelResourceInventory(t, kubecli, rf)
	for kind, count := range after {
		assert.Equalf(t, 0, count, "expected no %s to remain after EnsureNotPresentSentinelResources, found %d", kind, count)
	}
}

// TestSentinelResourceLifecycleSymmetryPreservesUserServiceAccount is the
// counterpart guard: when the user supplies their own ServiceAccountName,
// EnsureNotPresentSentinelResources must still remove every operator-owned
// resource, but must never touch the user's ServiceAccount, since it never
// created it.
func TestSentinelResourceLifecycleSymmetryPreservesUserServiceAccount(t *testing.T) {
	rf := sentinelLifecycleRF()
	rf.Spec.Sentinel.ServiceAccountName = "user-managed-sa"
	labels := map[string]string{}
	ownerRefs := []metav1.OwnerReference{}

	kubecli := kubefake.NewClientset()
	crdcli := redisfailoverfake.NewSimpleClientset()
	apiextcli := apiextensionsfake.NewSimpleClientset()
	k8sService := k8s.New(kubecli, crdcli, apiextcli, log.Dummy, metrics.Dummy)
	client := rfservice.NewRedisFailoverKubeClient(k8sService, log.Dummy, metrics.Dummy)

	// The user's own ServiceAccount, pre-existing and unrelated to the
	// operator - it deliberately does not carry the sentinel selector labels,
	// since the user created it, not us.
	userSA := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "user-managed-sa",
			Namespace: rf.Namespace,
		},
	}
	_, err := kubecli.CoreV1().ServiceAccounts(rf.Namespace).Create(t.Context(), userSA, metav1.CreateOptions{})
	assert.NoError(t, err)

	assert.NoError(t, client.EnsureSentinelService(rf, labels, ownerRefs))
	assert.NoError(t, client.EnsureSentinelConfigMap(rf, labels, ownerRefs))
	assert.NoError(t, client.EnsureSentinelDeployment(rf, labels, ownerRefs))

	// ensureSentinelServiceAccount must not have run: no operator-owned
	// ServiceAccount (the sentinel-labeled kind) exists.
	inventory := sentinelResourceInventory(t, kubecli, rf)
	assert.Equal(t, 0, inventory["ServiceAccount"], "no operator-owned ServiceAccount should be created when ServiceAccountName is user-supplied")

	assert.NoError(t, client.EnsureNotPresentSentinelResources(rf))

	_, err = kubecli.CoreV1().ServiceAccounts(rf.Namespace).Get(t.Context(), "user-managed-sa", metav1.GetOptions{})
	assert.NoError(t, err, "the user's own ServiceAccount must survive cleanup")

	after := sentinelResourceInventory(t, kubecli, rf)
	for kind, count := range after {
		assert.Equalf(t, 0, count, "expected no operator-owned %s to remain after cleanup, found %d", kind, count)
	}
}
