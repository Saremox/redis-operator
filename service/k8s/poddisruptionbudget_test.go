package k8s_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
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

var podDisruptionBudgetsGroup = schema.GroupVersionResource{Group: "policy", Version: "v1", Resource: "poddisruptionbudgets"}

func newPodDisruptionBudgetUpdateAction(ns string, podDisruptionBudget *policyv1.PodDisruptionBudget) kubetesting.UpdateActionImpl {
	return kubetesting.NewUpdateAction(podDisruptionBudgetsGroup, ns, podDisruptionBudget)
}

func newPodDisruptionBudgetGetAction(ns, name string) kubetesting.GetActionImpl {
	return kubetesting.NewGetAction(podDisruptionBudgetsGroup, ns, name)
}

func newPodDisruptionBudgetCreateAction(ns string, podDisruptionBudget *policyv1.PodDisruptionBudget) kubetesting.CreateActionImpl {
	return kubetesting.NewCreateAction(podDisruptionBudgetsGroup, ns, podDisruptionBudget)
}

func newPodDisruptionBudgetDeleteAction(ns, name string) kubetesting.DeleteActionImpl {
	return kubetesting.NewDeleteAction(podDisruptionBudgetsGroup, ns, name)
}

func TestPodDisruptionBudgetServiceGetCreateOrUpdate(t *testing.T) {
	testPodDisruptionBudget := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "testpodDisruptionBudget1",
			ResourceVersion: "10",
		},
	}

	testns := "testns"

	tests := []struct {
		name                         string
		podDisruptionBudget          *policyv1.PodDisruptionBudget
		getPodDisruptionBudgetResult *policyv1.PodDisruptionBudget
		errorOnGet                   error
		errorOnCreation              error
		expActions                   []kubetesting.Action
		expErr                       bool
	}{
		{
			name:                         "A new podDisruptionBudget should create a new podDisruptionBudget.",
			podDisruptionBudget:          testPodDisruptionBudget,
			getPodDisruptionBudgetResult: nil,
			errorOnGet:                   kubeerrors.NewNotFound(schema.GroupResource{}, ""),
			errorOnCreation:              nil,
			expActions: []kubetesting.Action{
				newPodDisruptionBudgetGetAction(testns, testPodDisruptionBudget.Name),
				newPodDisruptionBudgetCreateAction(testns, testPodDisruptionBudget),
			},
			expErr: false,
		},
		{
			name:                         "A new podDisruptionBudget should error when create a new podDisruptionBudget fails.",
			podDisruptionBudget:          testPodDisruptionBudget,
			getPodDisruptionBudgetResult: nil,
			errorOnGet:                   kubeerrors.NewNotFound(schema.GroupResource{}, ""),
			errorOnCreation:              errors.New("wanted error"),
			expActions: []kubetesting.Action{
				newPodDisruptionBudgetGetAction(testns, testPodDisruptionBudget.Name),
				newPodDisruptionBudgetCreateAction(testns, testPodDisruptionBudget),
			},
			expErr: true,
		},
		{
			name:                         "An existent podDisruptionBudget should update the podDisruptionBudget.",
			podDisruptionBudget:          testPodDisruptionBudget,
			getPodDisruptionBudgetResult: testPodDisruptionBudget,
			errorOnGet:                   nil,
			errorOnCreation:              nil,
			expActions: []kubetesting.Action{
				newPodDisruptionBudgetGetAction(testns, testPodDisruptionBudget.Name),
				newPodDisruptionBudgetUpdateAction(testns, testPodDisruptionBudget),
			},
			expErr: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertTest := assert.New(t)

			// Mock.
			mcli := &kubernetes.Clientset{}
			mcli.AddReactor("get", "poddisruptionbudgets", func(action kubetesting.Action) (bool, runtime.Object, error) {
				return true, test.getPodDisruptionBudgetResult, test.errorOnGet
			})
			mcli.AddReactor("create", "poddisruptionbudgets", func(action kubetesting.Action) (bool, runtime.Object, error) {
				return true, nil, test.errorOnCreation
			})

			service := k8s.NewPodDisruptionBudgetService(mcli, log.Dummy, metrics.Dummy)
			err := service.CreateOrUpdatePodDisruptionBudget(testns, test.podDisruptionBudget)

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

func TestPodDisruptionBudgetServiceUpdate(t *testing.T) {
	testPodDisruptionBudget := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name: "testpodDisruptionBudget1",
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
			name:          "Updating an existent podDisruptionBudget should not error.",
			errorOnUpdate: nil,
			expActions: []kubetesting.Action{
				newPodDisruptionBudgetUpdateAction(testns, testPodDisruptionBudget),
			},
			expErr: false,
		},
		{
			name:          "Updating should error when the client fails.",
			errorOnUpdate: errors.New("wanted error"),
			expActions: []kubetesting.Action{
				newPodDisruptionBudgetUpdateAction(testns, testPodDisruptionBudget),
			},
			expErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertTest := assert.New(t)

			mcli := &kubernetes.Clientset{}
			mcli.AddReactor("update", "poddisruptionbudgets", func(action kubetesting.Action) (bool, runtime.Object, error) {
				return true, nil, test.errorOnUpdate
			})

			service := k8s.NewPodDisruptionBudgetService(mcli, log.Dummy, metrics.Dummy)
			err := service.UpdatePodDisruptionBudget(testns, testPodDisruptionBudget)

			if test.expErr {
				assertTest.Error(err)
			} else {
				assertTest.NoError(err)
			}
			assertTest.Equal(test.expActions, mcli.Actions())
		})
	}
}

func TestPodDisruptionBudgetServiceDelete(t *testing.T) {
	testns := "testns"

	t.Run("deletes an existing podDisruptionBudget", func(t *testing.T) {
		assertTest := assert.New(t)

		pdb := &policyv1.PodDisruptionBudget{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "testpodDisruptionBudget1",
				Namespace: testns,
			},
		}
		mcli := kubernetes.NewClientset(pdb)
		service := k8s.NewPodDisruptionBudgetService(mcli, log.Dummy, metrics.Dummy)

		err := service.DeletePodDisruptionBudget(testns, "testpodDisruptionBudget1")
		assertTest.NoError(err)
		assertTest.Equal([]kubetesting.Action{newPodDisruptionBudgetDeleteAction(testns, "testpodDisruptionBudget1")}, mcli.Actions())

		_, getErr := mcli.PolicyV1().PodDisruptionBudgets(testns).Get(context.TODO(), "testpodDisruptionBudget1", metav1.GetOptions{})
		assertTest.Error(getErr)
		assertTest.True(kubeerrors.IsNotFound(getErr))
	})

	t.Run("returns a not found error when the podDisruptionBudget does not exist", func(t *testing.T) {
		assertTest := assert.New(t)

		mcli := kubernetes.NewClientset()
		service := k8s.NewPodDisruptionBudgetService(mcli, log.Dummy, metrics.Dummy)

		err := service.DeletePodDisruptionBudget(testns, "does-not-exist")
		assertTest.Error(err)
		assertTest.True(kubeerrors.IsNotFound(err))
	})
}
