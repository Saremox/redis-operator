package k8s_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	apitypes "k8s.io/apimachinery/pkg/types"
	kubernetes "k8s.io/client-go/kubernetes/fake"

	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	"github.com/saremox/redis-operator/service/k8s"
)

const (
	ownershipNS   = "testns"
	ownershipName = "rfr-s-foo"
)

func controllerRef(name, uid string) []metav1.OwnerReference {
	return []metav1.OwnerReference{*metav1.NewControllerRef(
		&metav1.ObjectMeta{Name: name, UID: apitypes.UID(uid)},
		corev1.SchemeGroupVersion.WithKind("RedisFailover"),
	)}
}

// ownershipKind has the operations of one object kind that the test needs.
type ownershipKind struct {
	kind string
	// object returns an object with a label value and owner references.
	object func(label string, refs []metav1.OwnerReference) runtime.Object
	// apply calls CreateOrUpdate with the object.
	apply func(c *kubernetes.Clientset, obj runtime.Object) error
	// label returns the label value that the cluster holds.
	label func(c *kubernetes.Clientset) string
}

func ownershipKinds() []ownershipKind {
	meta := func(label string, refs []metav1.OwnerReference) metav1.ObjectMeta {
		return metav1.ObjectMeta{Name: ownershipName, Namespace: ownershipNS, Labels: map[string]string{"v": label}, OwnerReferences: refs}
	}
	get := context.TODO()
	return []ownershipKind{
		{
			kind: "ConfigMap",
			object: func(l string, r []metav1.OwnerReference) runtime.Object {
				return &corev1.ConfigMap{ObjectMeta: meta(l, r)}
			},
			apply: func(c *kubernetes.Clientset, o runtime.Object) error {
				return k8s.NewConfigMapService(c, log.Dummy, metrics.Dummy).CreateOrUpdateConfigMap(ownershipNS, o.(*corev1.ConfigMap))
			},
			label: func(c *kubernetes.Clientset) string {
				o, _ := c.CoreV1().ConfigMaps(ownershipNS).Get(get, ownershipName, metav1.GetOptions{})
				return o.Labels["v"]
			},
		},
		{
			kind: "Service",
			object: func(l string, r []metav1.OwnerReference) runtime.Object {
				return &corev1.Service{ObjectMeta: meta(l, r)}
			},
			apply: func(c *kubernetes.Clientset, o runtime.Object) error {
				return k8s.NewServiceService(c, log.Dummy, metrics.Dummy).CreateOrUpdateService(ownershipNS, o.(*corev1.Service))
			},
			label: func(c *kubernetes.Clientset) string {
				o, _ := c.CoreV1().Services(ownershipNS).Get(get, ownershipName, metav1.GetOptions{})
				return o.Labels["v"]
			},
		},
		{
			kind: "StatefulSet",
			object: func(l string, r []metav1.OwnerReference) runtime.Object {
				return &appsv1.StatefulSet{ObjectMeta: meta(l, r)}
			},
			apply: func(c *kubernetes.Clientset, o runtime.Object) error {
				return k8s.NewStatefulSetService(c, log.Dummy, metrics.Dummy).CreateOrUpdateStatefulSet(ownershipNS, o.(*appsv1.StatefulSet))
			},
			label: func(c *kubernetes.Clientset) string {
				o, _ := c.AppsV1().StatefulSets(ownershipNS).Get(get, ownershipName, metav1.GetOptions{})
				return o.Labels["v"]
			},
		},
		{
			kind: "Deployment",
			object: func(l string, r []metav1.OwnerReference) runtime.Object {
				return &appsv1.Deployment{ObjectMeta: meta(l, r)}
			},
			apply: func(c *kubernetes.Clientset, o runtime.Object) error {
				return k8s.NewDeploymentService(c, log.Dummy, metrics.Dummy).CreateOrUpdateDeployment(ownershipNS, o.(*appsv1.Deployment))
			},
			label: func(c *kubernetes.Clientset) string {
				o, _ := c.AppsV1().Deployments(ownershipNS).Get(get, ownershipName, metav1.GetOptions{})
				return o.Labels["v"]
			},
		},
		{
			kind: "ServiceAccount",
			object: func(l string, r []metav1.OwnerReference) runtime.Object {
				return &corev1.ServiceAccount{ObjectMeta: meta(l, r)}
			},
			apply: func(c *kubernetes.Clientset, o runtime.Object) error {
				return k8s.NewServiceAccountService(c, log.Dummy, metrics.Dummy).CreateOrUpdateServiceAccount(ownershipNS, o.(*corev1.ServiceAccount))
			},
			label: func(c *kubernetes.Clientset) string {
				o, _ := c.CoreV1().ServiceAccounts(ownershipNS).Get(get, ownershipName, metav1.GetOptions{})
				return o.Labels["v"]
			},
		},
		{
			kind: "PodDisruptionBudget",
			object: func(l string, r []metav1.OwnerReference) runtime.Object {
				return &policyv1.PodDisruptionBudget{ObjectMeta: meta(l, r)}
			},
			apply: func(c *kubernetes.Clientset, o runtime.Object) error {
				return k8s.NewPodDisruptionBudgetService(c, log.Dummy, metrics.Dummy).CreateOrUpdatePodDisruptionBudget(ownershipNS, o.(*policyv1.PodDisruptionBudget))
			},
			label: func(c *kubernetes.Clientset) string {
				o, _ := c.PolicyV1().PodDisruptionBudgets(ownershipNS).Get(get, ownershipName, metav1.GetOptions{})
				return o.Labels["v"]
			},
		},
	}
}

func TestCreateOrUpdateChecksTheController(t *testing.T) {
	foo := controllerRef("foo", "1")
	sFoo := controllerRef("s-foo", "2")
	fooNewUID := controllerRef("foo", "3")

	tests := []struct {
		name      string
		stored    []metav1.OwnerReference
		desired   []metav1.OwnerReference
		wantLabel string
		wantErr   bool
	}{
		{name: "another controller: no write", stored: sFoo, desired: foo, wantLabel: "old", wantErr: true},
		{name: "same controller: update", stored: foo, desired: foo, wantLabel: "new"},
		{name: "no controller on the stored object: adopt", stored: nil, desired: foo, wantLabel: "new"},
		{name: "same Kind and Name with another UID: update", stored: fooNewUID, desired: foo, wantLabel: "new"},
		{name: "no controller on the desired object: update", stored: sFoo, desired: nil, wantLabel: "new"},
	}

	for _, kind := range ownershipKinds() {
		for _, test := range tests {
			t.Run(kind.kind+"/"+test.name, func(t *testing.T) {
				c := kubernetes.NewClientset(kind.object("old", test.stored))

				err := kind.apply(c, kind.object("new", test.desired))

				assert.Equal(t, test.wantLabel, kind.label(c))
				if !test.wantErr {
					assert.NoError(t, err)
					return
				}
				var other *k8s.ControlledByOtherError
				require.True(t, errors.As(err, &other))
				assert.Equal(t, kind.kind, other.Kind)
				assert.Equal(t, ownershipName, other.Name)
				assert.Equal(t, "RedisFailover", other.OwnerKind)
				assert.Equal(t, "s-foo", other.OwnerName)
				assert.Equal(t, `cannot manage `+kind.kind+` "rfr-s-foo": RedisFailover "s-foo" controls it`, err.Error())
				for _, a := range c.Actions() {
					assert.NotEqual(t, "update", a.GetVerb())
				}
			})
		}
	}
}
