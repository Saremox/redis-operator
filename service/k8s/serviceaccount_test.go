package k8s_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
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
	serviceAccountsGroup = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "serviceaccounts"}
)

func newServiceAccountUpdateAction(ns string, sa *corev1.ServiceAccount) kubetesting.UpdateActionImpl {
	return kubetesting.NewUpdateAction(serviceAccountsGroup, ns, sa)
}

func newServiceAccountGetAction(ns, name string) kubetesting.GetActionImpl {
	return kubetesting.NewGetAction(serviceAccountsGroup, ns, name)
}

func newServiceAccountCreateAction(ns string, sa *corev1.ServiceAccount) kubetesting.CreateActionImpl {
	return kubetesting.NewCreateAction(serviceAccountsGroup, ns, sa)
}

func TestServiceAccountServiceGet(t *testing.T) {
	testns := "testns"

	t.Run("returns an existing ServiceAccount", func(t *testing.T) {
		assertTest := assert.New(t)

		sa := &corev1.ServiceAccount{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "testsa",
				Namespace: testns,
			},
		}
		mcli := kubernetes.NewClientset(sa)
		service := k8s.NewServiceAccountService(mcli, log.Dummy, metrics.Dummy)

		got, err := service.GetServiceAccount(testns, "testsa")
		assertTest.NoError(err)
		assertTest.NotNil(got)
		assertTest.Equal("testsa", got.Name)
	})

	t.Run("returns an error when the ServiceAccount does not exist", func(t *testing.T) {
		assertTest := assert.New(t)

		mcli := kubernetes.NewClientset()
		service := k8s.NewServiceAccountService(mcli, log.Dummy, metrics.Dummy)

		got, err := service.GetServiceAccount(testns, "does-not-exist")
		assertTest.Error(err)
		assertTest.Nil(got)
	})
}

func TestServiceAccountServiceCreate(t *testing.T) {
	assertTest := assert.New(t)
	testns := "testns"

	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "testsa",
			Namespace: testns,
		},
	}
	mcli := kubernetes.NewClientset()
	service := k8s.NewServiceAccountService(mcli, log.Dummy, metrics.Dummy)

	err := service.CreateServiceAccount(testns, sa)
	assertTest.NoError(err)

	got, err := mcli.CoreV1().ServiceAccounts(testns).Get(context.TODO(), "testsa", metav1.GetOptions{})
	assertTest.NoError(err)
	assertTest.Equal("testsa", got.Name)
}

func TestServiceAccountServiceUpdate(t *testing.T) {
	assertTest := assert.New(t)
	testns := "testns"

	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "testsa",
			Namespace: testns,
			Labels:    map[string]string{"foo": "bar"},
		},
	}
	mcli := kubernetes.NewClientset(sa)
	service := k8s.NewServiceAccountService(mcli, log.Dummy, metrics.Dummy)

	sa.Labels["foo"] = "baz"
	err := service.UpdateServiceAccount(testns, sa)
	assertTest.NoError(err)

	got, err := mcli.CoreV1().ServiceAccounts(testns).Get(context.TODO(), "testsa", metav1.GetOptions{})
	assertTest.NoError(err)
	assertTest.Equal("baz", got.Labels["foo"])
}

func TestServiceAccountServiceUpdateError(t *testing.T) {
	assertTest := assert.New(t)
	testns := "testns"

	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "does-not-exist",
			Namespace: testns,
		},
	}
	mcli := kubernetes.NewClientset()
	service := k8s.NewServiceAccountService(mcli, log.Dummy, metrics.Dummy)

	err := service.UpdateServiceAccount(testns, sa)
	assertTest.Error(err)
	assertTest.True(kubeerrors.IsNotFound(err))
}

func TestServiceAccountServiceDelete(t *testing.T) {
	assertTest := assert.New(t)
	testns := "testns"

	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "testsa",
			Namespace: testns,
		},
	}
	mcli := kubernetes.NewClientset(sa)
	service := k8s.NewServiceAccountService(mcli, log.Dummy, metrics.Dummy)

	err := service.DeleteServiceAccount(testns, "testsa")
	assertTest.NoError(err)

	_, err = mcli.CoreV1().ServiceAccounts(testns).Get(context.TODO(), "testsa", metav1.GetOptions{})
	assertTest.Error(err)
	assertTest.True(kubeerrors.IsNotFound(err))
}

func TestServiceAccountServiceList(t *testing.T) {
	assertTest := assert.New(t)
	testns := "testns"

	sa1 := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "sa1", Namespace: testns}}
	sa2 := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "sa2", Namespace: testns}}
	otherNsSa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "sa3", Namespace: "otherns"}}

	mcli := kubernetes.NewClientset(sa1, sa2, otherNsSa)
	service := k8s.NewServiceAccountService(mcli, log.Dummy, metrics.Dummy)

	list, err := service.ListServiceAccounts(testns)
	assertTest.NoError(err)
	assertTest.Len(list.Items, 2)
}

// TestServiceAccountServiceGetCreateOrUpdate exercises the create/update branching
// of CreateOrUpdateServiceAccount against hand-rolled reactors (matching the
// convention used by the other CreateOrUpdate* tests in this package), including
// propagation of a create error.
func TestServiceAccountServiceObjectUpToDate(t *testing.T) {
	testns := "testns"

	realistic := func(labelValue string) *corev1.ServiceAccount {
		return &corev1.ServiceAccount{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "rfs-test",
				Namespace: "testns",
				Labels:    map[string]string{"app.kubernetes.io/name": labelValue},
			},
		}
	}

	tests := []struct {
		name          string
		stored        *corev1.ServiceAccount
		desired       *corev1.ServiceAccount
		expectUpdates int
	}{
		{
			name:          "identical desired is a no-op",
			stored:        realistic("test"),
			desired:       realistic("test"),
			expectUpdates: 0,
		},
		{
			name:          "a real label change still triggers an update",
			stored:        realistic("test"),
			desired:       realistic("changed"),
			expectUpdates: 1,
		},
		{
			name:          "manual drift on the live object is detected and corrected, even though desired is unchanged",
			stored:        realistic("drifted"),
			desired:       realistic("test"),
			expectUpdates: 1,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)

			stored := test.stored.DeepCopy()
			stored.ResourceVersion = "1"
			mcli := kubernetes.NewClientset(stored)

			service := k8s.NewServiceAccountService(mcli, log.Dummy, metrics.Dummy)
			assert.NoError(service.CreateOrUpdateServiceAccount(testns, test.desired.DeepCopy()))

			updates := 0
			for _, a := range mcli.Actions() {
				if a.GetVerb() == "update" {
					updates++
				}
			}
			assert.Equal(test.expectUpdates, updates)
		})
	}
}

func TestServiceAccountServiceGetCreateOrUpdate(t *testing.T) {
	testServiceAccount := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "testserviceaccount1",
			ResourceVersion: "10",
		},
	}

	testns := "testns"

	tests := []struct {
		name                    string
		serviceAccount          *corev1.ServiceAccount
		getServiceAccountResult *corev1.ServiceAccount
		errorOnGet              error
		errorOnCreation         error
		expActions              []kubetesting.Action
		expErr                  bool
	}{
		{
			name:                    "A new ServiceAccount should create a new ServiceAccount.",
			serviceAccount:          testServiceAccount,
			getServiceAccountResult: nil,
			errorOnGet:              kubeerrors.NewNotFound(schema.GroupResource{}, ""),
			errorOnCreation:         nil,
			expActions: []kubetesting.Action{
				newServiceAccountGetAction(testns, testServiceAccount.Name),
				newServiceAccountCreateAction(testns, testServiceAccount),
			},
			expErr: false,
		},
		{
			name:                    "A new ServiceAccount should error when create fails.",
			serviceAccount:          testServiceAccount,
			getServiceAccountResult: nil,
			errorOnGet:              kubeerrors.NewNotFound(schema.GroupResource{}, ""),
			errorOnCreation:         errors.New("wanted error"),
			expActions: []kubetesting.Action{
				newServiceAccountGetAction(testns, testServiceAccount.Name),
				newServiceAccountCreateAction(testns, testServiceAccount),
			},
			expErr: true,
		},
		{
			// The stored and desired objects must actually differ here: an
			// identical desired object is now a no-op (see
			// TestServiceAccountServiceObjectUpToDate) and would issue no
			// Update action, defeating the point of this test.
			name: "An existent ServiceAccount should update the ServiceAccount.",
			serviceAccount: &corev1.ServiceAccount{
				ObjectMeta: metav1.ObjectMeta{Name: "testserviceaccount1", Labels: map[string]string{"app": "redis"}},
			},
			getServiceAccountResult: &corev1.ServiceAccount{
				ObjectMeta: metav1.ObjectMeta{Name: "testserviceaccount1", ResourceVersion: "10"},
			},
			errorOnGet:      nil,
			errorOnCreation: nil,
			expActions: []kubetesting.Action{
				newServiceAccountGetAction(testns, "testserviceaccount1"),
				newServiceAccountUpdateAction(testns, &corev1.ServiceAccount{
					ObjectMeta: metav1.ObjectMeta{Name: "testserviceaccount1", ResourceVersion: "10", Labels: map[string]string{"app": "redis"}},
				}),
			},
			expErr: false,
		},
		{
			name:                    "A non-NotFound error on get should be returned as-is, without creating or updating.",
			serviceAccount:          testServiceAccount,
			getServiceAccountResult: nil,
			errorOnGet:              errors.New("connection refused"),
			errorOnCreation:         nil,
			expActions: []kubetesting.Action{
				newServiceAccountGetAction(testns, testServiceAccount.Name),
			},
			expErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertTest := assert.New(t)

			// Mock.
			mcli := &kubernetes.Clientset{}
			mcli.AddReactor("get", "serviceaccounts", func(action kubetesting.Action) (bool, runtime.Object, error) {
				return true, test.getServiceAccountResult, test.errorOnGet
			})
			mcli.AddReactor("create", "serviceaccounts", func(action kubetesting.Action) (bool, runtime.Object, error) {
				return true, nil, test.errorOnCreation
			})

			service := k8s.NewServiceAccountService(mcli, log.Dummy, metrics.Dummy)
			err := service.CreateOrUpdateServiceAccount(testns, test.serviceAccount)

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
