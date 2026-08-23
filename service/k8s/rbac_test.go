package k8s_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	rbacv1 "k8s.io/api/rbac/v1"
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
	rbGroup   = schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"}
	roleGroup = schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"}
)

func newRBUpdateAction(ns string, rb *rbacv1.RoleBinding) kubetesting.UpdateActionImpl {
	return kubetesting.NewUpdateAction(rbGroup, ns, rb)
}

func newRBGetAction(ns, name string) kubetesting.GetActionImpl {
	return kubetesting.NewGetAction(rbGroup, ns, name)
}

func newRBCreateAction(ns string, rb *rbacv1.RoleBinding) kubetesting.CreateActionImpl {
	return kubetesting.NewCreateAction(rbGroup, ns, rb)
}
func newRBDeleteAction(ns string, name string) kubetesting.DeleteActionImpl {
	return kubetesting.NewDeleteAction(rbGroup, ns, name)
}

func newRoleGetAction(ns, name string) kubetesting.GetActionImpl {
	return kubetesting.NewGetAction(roleGroup, ns, name)
}

func newRoleCreateAction(ns string, role *rbacv1.Role) kubetesting.CreateActionImpl {
	return kubetesting.NewCreateAction(roleGroup, ns, role)
}

func newRoleUpdateAction(ns string, role *rbacv1.Role) kubetesting.UpdateActionImpl {
	return kubetesting.NewUpdateAction(roleGroup, ns, role)
}

func TestRBACServiceGetCreateOrUpdateRoleBinding(t *testing.T) {
	testRB := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "test1",
			ResourceVersion: "15",
		},
		RoleRef: rbacv1.RoleRef{
			Name: "test1",
		},
	}

	testns := "testns"

	tests := []struct {
		name            string
		rb              *rbacv1.RoleBinding
		getRBResult     *rbacv1.RoleBinding
		errorOnGet      error
		errorOnCreation error
		expActions      []kubetesting.Action
		expErr          bool
	}{
		{
			name:            "A new role binding should create a new role binding.",
			rb:              testRB,
			getRBResult:     nil,
			errorOnGet:      kubeerrors.NewNotFound(schema.GroupResource{}, ""),
			errorOnCreation: nil,
			expActions: []kubetesting.Action{
				newRBGetAction(testns, testRB.Name),
				newRBCreateAction(testns, testRB),
			},
			expErr: false,
		},
		{
			name:            "A new role binding should error when create a new role binding fails.",
			rb:              testRB,
			getRBResult:     nil,
			errorOnGet:      kubeerrors.NewNotFound(schema.GroupResource{}, ""),
			errorOnCreation: errors.New("wanted error"),
			expActions: []kubetesting.Action{
				newRBGetAction(testns, testRB.Name),
				newRBUpdateAction(testns, testRB),
			},
			expErr: true,
		},
		{
			name:            "An existent role binding should update the role binding.",
			rb:              testRB,
			getRBResult:     testRB,
			errorOnGet:      nil,
			errorOnCreation: nil,
			expActions: []kubetesting.Action{
				newRBGetAction(testns, testRB.Name),
				newRBUpdateAction(testns, testRB),
			},
			expErr: false,
		},
		{
			name: "An change in role reference inside binding should recreate the role binding.",
			rb:   testRB,
			getRBResult: &rbacv1.RoleBinding{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "test1",
					ResourceVersion: "15",
				},
				RoleRef: rbacv1.RoleRef{
					Name: "oldroleRef",
				},
			},
			errorOnGet:      nil,
			errorOnCreation: nil,
			expActions: []kubetesting.Action{
				newRBGetAction(testns, testRB.Name),
				newRBDeleteAction(testns, testRB.Name),
				newRBCreateAction(testns, testRB),
			},
			expErr: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertTest := assert.New(t)

			// Mock.
			mcli := &kubernetes.Clientset{}
			mcli.AddReactor("get", "rolebindings", func(action kubetesting.Action) (bool, runtime.Object, error) {
				return true, test.getRBResult, test.errorOnGet
			})
			mcli.AddReactor("create", "rolebindings", func(action kubetesting.Action) (bool, runtime.Object, error) {
				return true, nil, test.errorOnCreation
			})

			service := k8s.NewRBACService(mcli, log.Dummy, metrics.Dummy)
			err := service.CreateOrUpdateRoleBinding(testns, test.rb)

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

func TestRBACServiceGetClusterRole(t *testing.T) {
	t.Run("returns an existing ClusterRole", func(t *testing.T) {
		assertTest := assert.New(t)

		cr := &rbacv1.ClusterRole{
			ObjectMeta: metav1.ObjectMeta{
				Name: "testclusterrole",
			},
		}
		mcli := kubernetes.NewClientset(cr)
		service := k8s.NewRBACService(mcli, log.Dummy, metrics.Dummy)

		got, err := service.GetClusterRole("testclusterrole")
		assertTest.NoError(err)
		assertTest.NotNil(got)
		assertTest.Equal("testclusterrole", got.Name)
	})

	t.Run("returns an error when the ClusterRole does not exist", func(t *testing.T) {
		assertTest := assert.New(t)

		mcli := kubernetes.NewClientset()
		service := k8s.NewRBACService(mcli, log.Dummy, metrics.Dummy)

		_, err := service.GetClusterRole("does-not-exist")
		assertTest.Error(err)
		assertTest.True(kubeerrors.IsNotFound(err))
	})
}

func TestRBACServiceGetRole(t *testing.T) {
	testns := "testns"

	t.Run("returns an existing Role", func(t *testing.T) {
		assertTest := assert.New(t)

		role := &rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "testrole",
				Namespace: testns,
			},
		}
		mcli := kubernetes.NewClientset(role)
		service := k8s.NewRBACService(mcli, log.Dummy, metrics.Dummy)

		got, err := service.GetRole(testns, "testrole")
		assertTest.NoError(err)
		assertTest.NotNil(got)
		assertTest.Equal("testrole", got.Name)
	})

	t.Run("returns an error when the Role does not exist", func(t *testing.T) {
		assertTest := assert.New(t)

		mcli := kubernetes.NewClientset()
		service := k8s.NewRBACService(mcli, log.Dummy, metrics.Dummy)

		_, err := service.GetRole(testns, "does-not-exist")
		assertTest.Error(err)
		assertTest.True(kubeerrors.IsNotFound(err))
	})
}

func TestRBACServiceDeleteRole(t *testing.T) {
	testns := "testns"

	t.Run("deletes an existing Role", func(t *testing.T) {
		assertTest := assert.New(t)

		role := &rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "testrole",
				Namespace: testns,
			},
		}
		mcli := kubernetes.NewClientset(role)
		service := k8s.NewRBACService(mcli, log.Dummy, metrics.Dummy)

		err := service.DeleteRole(testns, "testrole")
		assertTest.NoError(err)

		_, err = mcli.RbacV1().Roles(testns).Get(context.TODO(), "testrole", metav1.GetOptions{})
		assertTest.Error(err)
		assertTest.True(kubeerrors.IsNotFound(err))
	})

	t.Run("returns an error when deleting a non-existent Role", func(t *testing.T) {
		assertTest := assert.New(t)

		mcli := kubernetes.NewClientset()
		service := k8s.NewRBACService(mcli, log.Dummy, metrics.Dummy)

		err := service.DeleteRole(testns, "does-not-exist")
		assertTest.Error(err)
		assertTest.True(kubeerrors.IsNotFound(err))
	})
}

func TestRBACServiceCreateRole(t *testing.T) {
	assertTest := assert.New(t)
	testns := "testns"

	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "testrole",
			Namespace: testns,
		},
	}
	mcli := kubernetes.NewClientset()
	service := k8s.NewRBACService(mcli, log.Dummy, metrics.Dummy)

	err := service.CreateRole(testns, role)
	assertTest.NoError(err)

	got, err := mcli.RbacV1().Roles(testns).Get(context.TODO(), "testrole", metav1.GetOptions{})
	assertTest.NoError(err)
	assertTest.Equal("testrole", got.Name)
}

func TestRBACServiceUpdateRole(t *testing.T) {
	assertTest := assert.New(t)
	testns := "testns"

	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "testrole",
			Namespace: testns,
			Labels:    map[string]string{"foo": "bar"},
		},
	}
	mcli := kubernetes.NewClientset(role)
	service := k8s.NewRBACService(mcli, log.Dummy, metrics.Dummy)

	role.Labels["foo"] = "baz"
	err := service.UpdateRole(testns, role)
	assertTest.NoError(err)

	got, err := mcli.RbacV1().Roles(testns).Get(context.TODO(), "testrole", metav1.GetOptions{})
	assertTest.NoError(err)
	assertTest.Equal("baz", got.Labels["foo"])
}

func TestRBACServiceUpdateRoleError(t *testing.T) {
	assertTest := assert.New(t)
	testns := "testns"

	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name: "does-not-exist",
		},
	}
	mcli := kubernetes.NewClientset()
	service := k8s.NewRBACService(mcli, log.Dummy, metrics.Dummy)

	err := service.UpdateRole(testns, role)
	assertTest.Error(err)
	assertTest.True(kubeerrors.IsNotFound(err))
}

func TestRBACServiceGetCreateOrUpdateRole(t *testing.T) {
	testRole := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "test1",
			ResourceVersion: "15",
		},
	}

	testns := "testns"

	tests := []struct {
		name            string
		role            *rbacv1.Role
		getRoleResult   *rbacv1.Role
		errorOnGet      error
		errorOnCreation error
		expActions      []kubetesting.Action
		expErr          bool
	}{
		{
			name:            "A new role should create a new role.",
			role:            testRole,
			getRoleResult:   nil,
			errorOnGet:      kubeerrors.NewNotFound(schema.GroupResource{}, ""),
			errorOnCreation: nil,
			expActions: []kubetesting.Action{
				newRoleGetAction(testns, testRole.Name),
				newRoleCreateAction(testns, testRole),
			},
			expErr: false,
		},
		{
			name:            "A new role should error when create a new role fails.",
			role:            testRole,
			getRoleResult:   nil,
			errorOnGet:      kubeerrors.NewNotFound(schema.GroupResource{}, ""),
			errorOnCreation: errors.New("wanted error"),
			expActions: []kubetesting.Action{
				newRoleGetAction(testns, testRole.Name),
				newRoleCreateAction(testns, testRole),
			},
			expErr: true,
		},
		{
			name:            "An existent role should update the role.",
			role:            testRole,
			getRoleResult:   testRole,
			errorOnGet:      nil,
			errorOnCreation: nil,
			expActions: []kubetesting.Action{
				newRoleGetAction(testns, testRole.Name),
				newRoleUpdateAction(testns, testRole),
			},
			expErr: false,
		},
		{
			name:            "A non-NotFound error on get should be returned as-is, without creating or updating.",
			role:            testRole,
			getRoleResult:   nil,
			errorOnGet:      errors.New("connection refused"),
			errorOnCreation: nil,
			expActions: []kubetesting.Action{
				newRoleGetAction(testns, testRole.Name),
			},
			expErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertTest := assert.New(t)

			// Mock.
			mcli := &kubernetes.Clientset{}
			mcli.AddReactor("get", "roles", func(action kubetesting.Action) (bool, runtime.Object, error) {
				return true, test.getRoleResult, test.errorOnGet
			})
			mcli.AddReactor("create", "roles", func(action kubetesting.Action) (bool, runtime.Object, error) {
				return true, nil, test.errorOnCreation
			})

			service := k8s.NewRBACService(mcli, log.Dummy, metrics.Dummy)
			err := service.CreateOrUpdateRole(testns, test.role)

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

func TestRBACServiceDeleteRoleBindingError(t *testing.T) {
	assertTest := assert.New(t)
	testns := "testns"

	mcli := kubernetes.NewClientset()
	service := k8s.NewRBACService(mcli, log.Dummy, metrics.Dummy)

	err := service.DeleteRoleBinding(testns, "does-not-exist")
	assertTest.Error(err)
	assertTest.True(kubeerrors.IsNotFound(err))
}

func TestRBACServiceUpdateRoleBindingError(t *testing.T) {
	assertTest := assert.New(t)
	testns := "testns"

	binding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: "does-not-exist",
		},
	}
	mcli := kubernetes.NewClientset()
	service := k8s.NewRBACService(mcli, log.Dummy, metrics.Dummy)

	err := service.UpdateRoleBinding(testns, binding)
	assertTest.Error(err)
	assertTest.True(kubeerrors.IsNotFound(err))
}

func TestRBACServiceCreateOrUpdateRoleBindingGetError(t *testing.T) {
	assertTest := assert.New(t)
	testns := "testns"

	binding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test1",
		},
	}

	mcli := &kubernetes.Clientset{}
	mcli.AddReactor("get", "rolebindings", func(action kubetesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("connection refused")
	})

	service := k8s.NewRBACService(mcli, log.Dummy, metrics.Dummy)
	err := service.CreateOrUpdateRoleBinding(testns, binding)
	assertTest.Error(err)
	assertTest.Equal([]kubetesting.Action{newRBGetAction(testns, binding.Name)}, mcli.Actions())
}

func TestRBACServiceCreateOrUpdateRoleBindingRoleRefChangeDeleteError(t *testing.T) {
	assertTest := assert.New(t)
	testns := "testns"

	testRB := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "test1",
			ResourceVersion: "15",
		},
		RoleRef: rbacv1.RoleRef{
			Name: "test1",
		},
	}
	storedRB := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "test1",
			ResourceVersion: "15",
		},
		RoleRef: rbacv1.RoleRef{
			Name: "oldroleRef",
		},
	}

	mcli := &kubernetes.Clientset{}
	mcli.AddReactor("get", "rolebindings", func(action kubetesting.Action) (bool, runtime.Object, error) {
		return true, storedRB, nil
	})
	mcli.AddReactor("delete", "rolebindings", func(action kubetesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("wanted delete error")
	})

	service := k8s.NewRBACService(mcli, log.Dummy, metrics.Dummy)
	err := service.CreateOrUpdateRoleBinding(testns, testRB)
	assertTest.Error(err)
}
