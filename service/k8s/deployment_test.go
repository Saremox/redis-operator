package k8s_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
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

var (
	deploymentsGroup = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
)

func newDeploymentUpdateAction(ns string, deployment *appsv1.Deployment) kubetesting.UpdateActionImpl {
	return kubetesting.NewUpdateAction(deploymentsGroup, ns, deployment)
}

func newDeploymentGetAction(ns, name string) kubetesting.GetActionImpl {
	return kubetesting.NewGetAction(deploymentsGroup, ns, name)
}

func newDeploymentCreateAction(ns string, deployment *appsv1.Deployment) kubetesting.CreateActionImpl {
	return kubetesting.NewCreateAction(deploymentsGroup, ns, deployment)
}

func newDeploymentDeleteAction(ns, name string) kubetesting.DeleteActionImpl {
	propagation := metav1.DeletePropagationForeground
	return kubetesting.NewDeleteActionWithOptions(deploymentsGroup, ns, name, metav1.DeleteOptions{PropagationPolicy: &propagation})
}

func newDeploymentListAction(ns string) kubetesting.ListActionImpl {
	return kubetesting.NewListAction(deploymentsGroup, schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}, ns, metav1.ListOptions{})
}

func TestDeploymentServiceGetCreateOrUpdate(t *testing.T) {
	testDeployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "testdeployment1",
			ResourceVersion: "10",
		},
	}

	testns := "testns"

	tests := []struct {
		name                string
		deployment          *appsv1.Deployment
		getDeploymentResult *appsv1.Deployment
		errorOnGet          error
		errorOnCreation     error
		expActions          []kubetesting.Action
		expErr              bool
	}{
		{
			name:                "A new deployment should create a new deployment.",
			deployment:          testDeployment,
			getDeploymentResult: nil,
			errorOnGet:          kubeerrors.NewNotFound(schema.GroupResource{}, ""),
			errorOnCreation:     nil,
			expActions: []kubetesting.Action{
				newDeploymentGetAction(testns, testDeployment.Name),
				newDeploymentCreateAction(testns, testDeployment),
			},
			expErr: false,
		},
		{
			name:                "A new deployment should error when create a new deployment fails.",
			deployment:          testDeployment,
			getDeploymentResult: nil,
			errorOnGet:          kubeerrors.NewNotFound(schema.GroupResource{}, ""),
			errorOnCreation:     errors.New("wanted error"),
			expActions: []kubetesting.Action{
				newDeploymentGetAction(testns, testDeployment.Name),
				newDeploymentCreateAction(testns, testDeployment),
			},
			expErr: true,
		},
		{
			name:                "An existent deployment should update the deployment.",
			deployment:          testDeployment,
			getDeploymentResult: testDeployment,
			errorOnGet:          nil,
			errorOnCreation:     nil,
			expActions: []kubetesting.Action{
				newDeploymentGetAction(testns, testDeployment.Name),
				newDeploymentUpdateAction(testns, testDeployment),
			},
			expErr: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertTest := assert.New(t)

			// Mock.
			mcli := &kubernetes.Clientset{}
			mcli.AddReactor("get", "deployments", func(action kubetesting.Action) (bool, runtime.Object, error) {
				return true, test.getDeploymentResult, test.errorOnGet
			})
			mcli.AddReactor("create", "deployments", func(action kubetesting.Action) (bool, runtime.Object, error) {
				return true, nil, test.errorOnCreation
			})

			service := k8s.NewDeploymentService(mcli, log.Dummy, metrics.Dummy)
			err := service.CreateOrUpdateDeployment(testns, test.deployment)

			if test.expErr {
				assertTest.Error(err)
			} else {
				assertTest.NoError(err)
				// Check calls to kubernetes.
				assertTest.Equal(test.expActions, mcli.Actions())
			}
		})
	}
}

func TestDeploymentServiceUpdate(t *testing.T) {
	testDeployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: "testdeployment1",
		},
	}

	testns := "testns"

	tests := []struct {
		name          string
		errorOnUpdate error
		expActions    []kubetesting.Action
		expErr        bool
	}{
		{
			name:          "Updating an existent deployment should not error.",
			errorOnUpdate: nil,
			expActions: []kubetesting.Action{
				newDeploymentUpdateAction(testns, testDeployment),
			},
			expErr: false,
		},
		{
			name:          "Updating should error when the client fails.",
			errorOnUpdate: errors.New("wanted error"),
			expActions: []kubetesting.Action{
				newDeploymentUpdateAction(testns, testDeployment),
			},
			expErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertTest := assert.New(t)

			mcli := &kubernetes.Clientset{}
			mcli.AddReactor("update", "deployments", func(action kubetesting.Action) (bool, runtime.Object, error) {
				return true, nil, test.errorOnUpdate
			})

			service := k8s.NewDeploymentService(mcli, log.Dummy, metrics.Dummy)
			err := service.UpdateDeployment(testns, testDeployment)

			if test.expErr {
				assertTest.Error(err)
			} else {
				assertTest.NoError(err)
			}
			assertTest.Equal(test.expActions, mcli.Actions())
		})
	}
}

// countActions tallies the verbs the fake clientset recorded.
func countActions(actions []kubetesting.Action) map[string]int {
	counts := map[string]int{}
	for _, a := range actions {
		counts[a.GetVerb()]++
	}
	return counts
}

func hashingTestDeployment(replicas int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "rfr-test", Namespace: "testns"},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
	}
}

// stampStored applies a deployment once with hashing on, so the object left in
// the fake cluster carries the resource-hash annotation exactly as a real
// hashing-enabled reconcile would leave it. It returns a fresh fake clientset
// already holding the stamped object, and the number of update actions used to
// stamp it (always 1) is discarded by the caller.
func stampStored(t *testing.T, stored *appsv1.Deployment) *kubernetes.Clientset {
	t.Helper()
	mcli := kubernetes.NewClientset(stored)
	svc := k8s.NewDeploymentService(mcli, log.Dummy, metrics.Dummy, k8s.WithObjectHashing(true))
	// The stored object is not yet stamped, so this first apply stamps it.
	assert.NoError(t, svc.CreateOrUpdateDeployment("testns", stored.DeepCopy()))
	mcli.ClearActions()
	return mcli
}

func TestDeploymentServiceObjectHashing(t *testing.T) {
	const testns = "testns"

	tests := []struct {
		name          string
		hashing       bool
		desired       *appsv1.Deployment
		expectUpdates int
	}{
		{
			name:          "hashing off still updates an unchanged object",
			hashing:       false,
			desired:       hashingTestDeployment(3),
			expectUpdates: 1,
		},
		{
			name:          "hashing on skips an unchanged object",
			hashing:       true,
			desired:       hashingTestDeployment(3),
			expectUpdates: 0,
		},
		{
			name:          "hashing on updates a changed object",
			hashing:       true,
			desired:       hashingTestDeployment(9), // differs from the stamped stored object
			expectUpdates: 1,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)

			// The cluster already holds a deployment stamped by a prior apply.
			mcli := stampStored(t, hashingTestDeployment(3))

			service := k8s.NewDeploymentService(mcli, log.Dummy, metrics.Dummy, k8s.WithObjectHashing(test.hashing))

			assert.NoError(service.CreateOrUpdateDeployment(testns, test.desired))
			assert.Equal(test.expectUpdates, countActions(mcli.Actions())["update"])
		})
	}
}

func TestDeploymentServiceDelete(t *testing.T) {
	testns := "testns"

	t.Run("deletes an existing deployment", func(t *testing.T) {
		assertTest := assert.New(t)

		deployment := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "testdeployment1",
				Namespace: testns,
			},
		}
		mcli := kubernetes.NewClientset(deployment)
		service := k8s.NewDeploymentService(mcli, log.Dummy, metrics.Dummy)

		err := service.DeleteDeployment(testns, "testdeployment1")
		assertTest.NoError(err)
		assertTest.Equal([]kubetesting.Action{newDeploymentDeleteAction(testns, "testdeployment1")}, mcli.Actions())

		_, getErr := mcli.AppsV1().Deployments(testns).Get(context.TODO(), "testdeployment1", metav1.GetOptions{})
		assertTest.Error(getErr)
		assertTest.True(kubeerrors.IsNotFound(getErr))
	})

	t.Run("returns a not found error when the deployment does not exist", func(t *testing.T) {
		assertTest := assert.New(t)

		mcli := kubernetes.NewClientset()
		service := k8s.NewDeploymentService(mcli, log.Dummy, metrics.Dummy)

		err := service.DeleteDeployment(testns, "does-not-exist")
		assertTest.Error(err)
		assertTest.True(kubeerrors.IsNotFound(err))
	})
}

func TestDeploymentServiceList(t *testing.T) {
	testns := "testns"

	t.Run("lists deployments in a namespace", func(t *testing.T) {
		assertTest := assert.New(t)

		d1 := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "d1", Namespace: testns}}
		d2 := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "d2", Namespace: testns}}
		d3 := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "d3", Namespace: "otherns"}}
		mcli := kubernetes.NewClientset(d1, d2, d3)
		service := k8s.NewDeploymentService(mcli, log.Dummy, metrics.Dummy)

		list, err := service.ListDeployments(testns)
		assertTest.NoError(err)
		assertTest.Len(list.Items, 2)
		names := []string{list.Items[0].Name, list.Items[1].Name}
		assertTest.ElementsMatch([]string{"d1", "d2"}, names)
	})

	t.Run("returns an error when the client fails", func(t *testing.T) {
		assertTest := assert.New(t)

		mcli := &kubernetes.Clientset{}
		wantErr := errors.New("wanted error")
		mcli.AddReactor("list", "deployments", func(action kubetesting.Action) (bool, runtime.Object, error) {
			return true, nil, wantErr
		})
		service := k8s.NewDeploymentService(mcli, log.Dummy, metrics.Dummy)

		list, err := service.ListDeployments(testns)
		assertTest.Error(err)
		assertTest.Empty(list.Items)
		assertTest.Equal([]kubetesting.Action{newDeploymentListAction(testns)}, mcli.Actions())
	})
}

func TestDeploymentServiceGetDeploymentPods(t *testing.T) {
	testns := "testns"

	t.Run("returns pods matching the deployment's selector", func(t *testing.T) {
		assertTest := assert.New(t)

		deployment := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "testdeployment1", Namespace: testns},
			Spec: appsv1.DeploymentSpec{
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"app": "redis"},
				},
			},
		}
		matchingPod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "matchingpod",
				Namespace: testns,
				Labels:    map[string]string{"app": "redis"},
			},
		}
		otherPod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "otherpod",
				Namespace: testns,
				Labels:    map[string]string{"app": "other"},
			},
		}
		mcli := kubernetes.NewClientset(deployment, matchingPod, otherPod)
		service := k8s.NewDeploymentService(mcli, log.Dummy, metrics.Dummy)

		pods, err := service.GetDeploymentPods(testns, "testdeployment1")
		assertTest.NoError(err)
		assertTest.Len(pods.Items, 1)
		assertTest.Equal("matchingpod", pods.Items[0].Name)
	})

	t.Run("returns an error when the deployment does not exist", func(t *testing.T) {
		assertTest := assert.New(t)

		mcli := kubernetes.NewClientset()
		service := k8s.NewDeploymentService(mcli, log.Dummy, metrics.Dummy)

		pods, err := service.GetDeploymentPods(testns, "does-not-exist")
		assertTest.Error(err)
		assertTest.True(kubeerrors.IsNotFound(err))
		assertTest.Nil(pods)
	})
}

func TestDeploymentServiceObjectHashingFirstPass(t *testing.T) {
	assert := assert.New(t)

	const testns = "testns"
	// Stored object exists but was never stamped (pre-upgrade state).
	stored := hashingTestDeployment(3)
	stored.ResourceVersion = "42"
	mcli := kubernetes.NewClientset(stored)

	service := k8s.NewDeploymentService(mcli, log.Dummy, metrics.Dummy, k8s.WithObjectHashing(true))

	assert.NoError(service.CreateOrUpdateDeployment(testns, hashingTestDeployment(3)))
	assert.Equal(1, countActions(mcli.Actions())["update"], "an unstamped object must be updated once")
}

// TestDeploymentServiceObjectHashingConvergence guards the ordering rule: the
// hash is taken before ResourceVersion is copied, so once an object is applied
// with hashing on, a second identical reconcile issues no further update.
func TestDeploymentServiceObjectHashingConvergence(t *testing.T) {
	assert := assert.New(t)

	const testns = "testns"
	replicas := int32(3)
	desired := func() *appsv1.Deployment {
		return &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "rfr-test", Namespace: testns},
			Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
		}
	}

	// First apply: object exists but was never stamped, so it is updated once
	// and the applied object carries the hash annotation.
	stored := desired()
	stored.ResourceVersion = "1"
	mcli := kubernetes.NewClientset(stored)
	service := k8s.NewDeploymentService(mcli, log.Dummy, metrics.Dummy, k8s.WithObjectHashing(true))

	assert.NoError(service.CreateOrUpdateDeployment(testns, desired()))
	assert.Equal(1, countActions(mcli.Actions())["update"], "first reconcile stamps and updates")

	// Second apply against the now-stamped object must be a no-op.
	assert.NoError(service.CreateOrUpdateDeployment(testns, desired()))
	assert.Equal(1, countActions(mcli.Actions())["update"], "second identical reconcile must not update again")
}
