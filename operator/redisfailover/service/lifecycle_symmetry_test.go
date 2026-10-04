package service_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
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
	k8sService := k8s.New(kubecli, crdcli, log.Dummy, metrics.Dummy)
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
	k8sService := k8s.New(kubecli, crdcli, log.Dummy, metrics.Dummy)
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

// ownedLifecycleRF returns sentinelLifecycleRF with a UID, and the owner
// reference that the handler gives to the objects of this RF.
func ownedLifecycleRF() (*redisfailoverv1.RedisFailover, []metav1.OwnerReference) {
	rf := sentinelLifecycleRF()
	rf.UID = "lifecycle-uid"
	return rf, []metav1.OwnerReference{*metav1.NewControllerRef(rf, redisfailoverv1.VersionKind(redisfailoverv1.RFKind))}
}

// TestDisablePodDisruptionBudgetDeletesExistingPDB covers a PDB that the
// operator created before the user disabled it. A PDB that stays blocks the
// node drains that the user disabled it for.
func TestDisablePodDisruptionBudgetDeletesExistingPDB(t *testing.T) {
	tests := []struct {
		name    string
		pdbName string
		flag    func(rf *redisfailoverv1.RedisFailover) *bool
		ensure  func(client *rfservice.RedisFailoverKubeClient, rf *redisfailoverv1.RedisFailover, ownerRefs []metav1.OwnerReference) error
	}{
		{
			name:    "redis",
			pdbName: "rfr-lifecycle",
			flag:    func(rf *redisfailoverv1.RedisFailover) *bool { return &rf.Spec.Redis.DisablePodDisruptionBudget },
			ensure: func(client *rfservice.RedisFailoverKubeClient, rf *redisfailoverv1.RedisFailover, ownerRefs []metav1.OwnerReference) error {
				return client.EnsureRedisStatefulset(rf, map[string]string{}, ownerRefs)
			},
		},
		{
			name:    "sentinel",
			pdbName: "rfs-lifecycle",
			flag:    func(rf *redisfailoverv1.RedisFailover) *bool { return &rf.Spec.Sentinel.DisablePodDisruptionBudget },
			ensure: func(client *rfservice.RedisFailoverKubeClient, rf *redisfailoverv1.RedisFailover, ownerRefs []metav1.OwnerReference) error {
				return client.EnsureSentinelDeployment(rf, map[string]string{}, ownerRefs)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rf, ownerRefs := ownedLifecycleRF()
			kubecli := kubefake.NewClientset()
			k8sService := k8s.New(kubecli, redisfailoverfake.NewSimpleClientset(), log.Dummy, metrics.Dummy)
			client := rfservice.NewRedisFailoverKubeClient(k8sService, log.Dummy, metrics.Dummy)
			pdbExists := func() bool {
				_, err := kubecli.PolicyV1().PodDisruptionBudgets(rf.Namespace).Get(t.Context(), test.pdbName, metav1.GetOptions{})
				return err == nil
			}

			// A PDB that another RedisFailover or the user owns stays.
			assert.NoError(t, test.ensure(client, rf, nil))
			*test.flag(rf) = true
			assert.NoError(t, test.ensure(client, rf, ownerRefs))
			assert.True(t, pdbExists(), "a PDB that the RedisFailover does not own must stay")

			assert.NoError(t, kubecli.PolicyV1().PodDisruptionBudgets(rf.Namespace).Delete(t.Context(), test.pdbName, metav1.DeleteOptions{}))
			*test.flag(rf) = false
			assert.NoError(t, test.ensure(client, rf, ownerRefs))
			assert.True(t, pdbExists())

			*test.flag(rf) = true
			assert.NoError(t, test.ensure(client, rf, ownerRefs))
			assert.False(t, pdbExists(), "the disabled PDB must be deleted")

			// The next reconcile finds no PDB, which is not an error.
			assert.NoError(t, test.ensure(client, rf, ownerRefs))

			// A missing RBAC verb does not stop the reconcile.
			kubecli.PrependReactor("get", "poddisruptionbudgets", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, apierrors.NewForbidden(policyv1.Resource("poddisruptionbudgets"), test.pdbName, errors.New("no get"))
			})
			assert.NoError(t, test.ensure(client, rf, ownerRefs))

			kubecli.PrependReactor("get", "poddisruptionbudgets", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, errors.New("get failed")
			})
			assert.Error(t, test.ensure(client, rf, ownerRefs))
		})
	}
}

// TestDisablePodDisruptionBudgetIgnoresForbiddenDelete covers a ClusterRole
// without the delete verb. The cleanup is optional, so the reconcile goes on.
func TestDisablePodDisruptionBudgetIgnoresForbiddenDelete(t *testing.T) {
	rf, ownerRefs := ownedLifecycleRF()
	kubecli := kubefake.NewClientset()
	k8sService := k8s.New(kubecli, redisfailoverfake.NewSimpleClientset(), log.Dummy, metrics.Dummy)
	client := rfservice.NewRedisFailoverKubeClient(k8sService, log.Dummy, metrics.Dummy)
	assert.NoError(t, client.EnsureRedisStatefulset(rf, map[string]string{}, ownerRefs))

	kubecli.PrependReactor("delete", "poddisruptionbudgets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(policyv1.Resource("poddisruptionbudgets"), "rfr-lifecycle", errors.New("no delete"))
	})
	rf.Spec.Redis.DisablePodDisruptionBudget = true
	assert.NoError(t, client.EnsureRedisStatefulset(rf, map[string]string{}, ownerRefs))
}

// TestEnsureSentinelDeploymentDeletesUnusedServiceAccount covers a
// ServiceAccount that the operator created before the user set
// sentinel.serviceAccountName.
func TestEnsureSentinelDeploymentDeletesUnusedServiceAccount(t *testing.T) {
	rf, ownerRefs := ownedLifecycleRF()
	autoName := rfservice.GetSentinelServiceAccountName(rf)
	kubecli := kubefake.NewClientset()
	k8sService := k8s.New(kubecli, redisfailoverfake.NewSimpleClientset(), log.Dummy, metrics.Dummy)
	client := rfservice.NewRedisFailoverKubeClient(k8sService, log.Dummy, metrics.Dummy)
	ensure := func() error {
		return client.EnsureSentinelDeployment(rf, map[string]string{}, ownerRefs)
	}
	saExists := func() bool {
		_, err := kubecli.CoreV1().ServiceAccounts(rf.Namespace).Get(t.Context(), autoName, metav1.GetOptions{})
		return err == nil
	}

	assert.NoError(t, ensure())
	assert.True(t, saExists())
	d, err := kubecli.AppsV1().Deployments(rf.Namespace).Get(t.Context(), rfservice.GetSentinelName(rf), metav1.GetOptions{})
	assert.NoError(t, err)

	// A user ServiceAccount with this name is the one that the pods use.
	rf.Spec.Sentinel.ServiceAccountName = autoName
	assert.NoError(t, ensure())
	assert.True(t, saExists())

	// A pod of the old ReplicaSet still uses the ServiceAccount.
	podLabels := map[string]string{"pod-template-hash": "oldhash"}
	for key, value := range d.Spec.Selector.MatchLabels {
		podLabels[key] = value
	}
	oldPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "rfs-lifecycle-old",
			Namespace: rf.Namespace,
			Labels:    podLabels,
			OwnerReferences: []metav1.OwnerReference{
				{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: d.Name + "-oldhash", UID: "old-rs-uid", Controller: ptr.To(true)},
			},
		},
		Spec: corev1.PodSpec{ServiceAccountName: autoName},
	}
	_, err = kubecli.CoreV1().Pods(rf.Namespace).Create(t.Context(), oldPod, metav1.CreateOptions{})
	assert.NoError(t, err)
	// A pod with the labels but without the owner does not keep the ServiceAccount.
	foreignPod := oldPod.DeepCopy()
	foreignPod.Name = "rfs-lifecycle-foreign"
	foreignPod.OwnerReferences = nil
	_, err = kubecli.CoreV1().Pods(rf.Namespace).Create(t.Context(), foreignPod, metav1.CreateOptions{})
	assert.NoError(t, err)
	rf.Spec.Sentinel.ServiceAccountName = "user-managed-sa"
	assert.NoError(t, ensure())
	assert.True(t, saExists(), "the ServiceAccount must stay while a Sentinel pod uses it")

	assert.NoError(t, kubecli.CoreV1().Pods(rf.Namespace).Delete(t.Context(), oldPod.Name, metav1.DeleteOptions{}))
	assert.NoError(t, ensure())
	assert.False(t, saExists(), "the unused ServiceAccount must be deleted")

	// The next reconcile finds no ServiceAccount, which is not an error.
	assert.NoError(t, ensure())
}

// TestEnsureSentinelDeploymentKeepsServiceAccount covers the cases where the
// operator must not delete the ServiceAccount, or where the delete fails.
func TestEnsureSentinelDeploymentKeepsServiceAccount(t *testing.T) {
	tests := []struct {
		name      string
		ownerRefs func(ownerRefs []metav1.OwnerReference) []metav1.OwnerReference
		reactor   func(kubecli *kubefake.Clientset)
		expErr    bool
		expExists bool
		// noDelete is true when no DELETE may be sent.
		noDelete bool
	}{
		{
			name:      "the RedisFailover does not own it",
			ownerRefs: func([]metav1.OwnerReference) []metav1.OwnerReference { return nil },
			expExists: true,
			noDelete:  true,
		},
		{
			name: "the Deployment update fails",
			reactor: func(kubecli *kubefake.Clientset) {
				kubecli.PrependReactor("update", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, errors.New("update failed")
				})
			},
			expErr:    true,
			expExists: true,
			noDelete:  true,
		},
		{
			name: "the operator cannot list the Sentinel pods",
			reactor: func(kubecli *kubefake.Clientset) {
				kubecli.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, apierrors.NewForbidden(corev1.Resource("pods"), "", errors.New("no list"))
				})
			},
			expExists: true,
			noDelete:  true,
		},
		{
			name: "the operator cannot delete it",
			reactor: func(kubecli *kubefake.Clientset) {
				kubecli.PrependReactor("delete", "serviceaccounts", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, apierrors.NewForbidden(corev1.Resource("serviceaccounts"), "rfs-sa-lifecycle", errors.New("no delete"))
				})
			},
			expExists: true,
		},
		{
			name: "the delete fails",
			reactor: func(kubecli *kubefake.Clientset) {
				kubecli.PrependReactor("delete", "serviceaccounts", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, errors.New("delete failed")
				})
			},
			expErr:    true,
			expExists: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rf, ownerRefs := ownedLifecycleRF()
			kubecli := kubefake.NewClientset()
			k8sService := k8s.New(kubecli, redisfailoverfake.NewSimpleClientset(), log.Dummy, metrics.Dummy)
			client := rfservice.NewRedisFailoverKubeClient(k8sService, log.Dummy, metrics.Dummy)
			createRefs := ownerRefs
			if test.ownerRefs != nil {
				createRefs = test.ownerRefs(ownerRefs)
			}
			assert.NoError(t, client.EnsureSentinelDeployment(rf, map[string]string{}, createRefs))

			if test.reactor != nil {
				test.reactor(kubecli)
			}
			kubecli.ClearActions()
			rf.Spec.Sentinel.ServiceAccountName = "user-managed-sa"
			rf.Spec.Sentinel.Replicas = 5
			err := client.EnsureSentinelDeployment(rf, map[string]string{}, ownerRefs)
			if test.expErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}

			_, err = kubecli.CoreV1().ServiceAccounts(rf.Namespace).Get(t.Context(), rfservice.GetSentinelServiceAccountName(rf), metav1.GetOptions{})
			assert.Equal(t, test.expExists, err == nil)
			if test.noDelete {
				for _, action := range kubecli.Actions() {
					assert.False(t, action.Matches("delete", "serviceaccounts"), "unexpected DELETE of the ServiceAccount")
				}
			}
		})
	}
}
