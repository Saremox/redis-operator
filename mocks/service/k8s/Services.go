// Code generated by mockery v2.20.0. DO NOT EDIT.

package mocks

import (
	context "context"

	appsv1 "k8s.io/api/apps/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	mock "github.com/stretchr/testify/mock"

	policyv1 "k8s.io/api/policy/v1"

	rbacv1 "k8s.io/api/rbac/v1"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"

	v1 "k8s.io/api/core/v1"

	watch "k8s.io/apimachinery/pkg/watch"
)

// Services is an autogenerated mock type for the Services type
type Services struct {
	mock.Mock
}

// CreateConfigMap provides a mock function with given fields: namespace, configMap
func (_m *Services) CreateConfigMap(namespace string, configMap *v1.ConfigMap) error {
	ret := _m.Called(namespace, configMap)

	var r0 error
	if rf, ok := ret.Get(0).(func(string, *v1.ConfigMap) error); ok {
		r0 = rf(namespace, configMap)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// CreateDeployment provides a mock function with given fields: namespace, deployment
func (_m *Services) CreateDeployment(namespace string, deployment *appsv1.Deployment) error {
	ret := _m.Called(namespace, deployment)

	var r0 error
	if rf, ok := ret.Get(0).(func(string, *appsv1.Deployment) error); ok {
		r0 = rf(namespace, deployment)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// CreateIfNotExistsService provides a mock function with given fields: namespace, service
func (_m *Services) CreateIfNotExistsService(namespace string, service *v1.Service) error {
	ret := _m.Called(namespace, service)

	var r0 error
	if rf, ok := ret.Get(0).(func(string, *v1.Service) error); ok {
		r0 = rf(namespace, service)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// CreateOrUpdateConfigMap provides a mock function with given fields: namespace, np
func (_m *Services) CreateOrUpdateConfigMap(namespace string, np *v1.ConfigMap) error {
	ret := _m.Called(namespace, np)

	var r0 error
	if rf, ok := ret.Get(0).(func(string, *v1.ConfigMap) error); ok {
		r0 = rf(namespace, np)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// CreateOrUpdateDeployment provides a mock function with given fields: namespace, deployment
func (_m *Services) CreateOrUpdateDeployment(namespace string, deployment *appsv1.Deployment) error {
	ret := _m.Called(namespace, deployment)

	var r0 error
	if rf, ok := ret.Get(0).(func(string, *appsv1.Deployment) error); ok {
		r0 = rf(namespace, deployment)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// CreateOrUpdatePod provides a mock function with given fields: namespace, pod
func (_m *Services) CreateOrUpdatePod(namespace string, pod *v1.Pod) error {
	ret := _m.Called(namespace, pod)

	var r0 error
	if rf, ok := ret.Get(0).(func(string, *v1.Pod) error); ok {
		r0 = rf(namespace, pod)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// CreateOrUpdatePodDisruptionBudget provides a mock function with given fields: namespace, podDisruptionBudget
func (_m *Services) CreateOrUpdatePodDisruptionBudget(namespace string, podDisruptionBudget *policyv1.PodDisruptionBudget) error {
	ret := _m.Called(namespace, podDisruptionBudget)

	var r0 error
	if rf, ok := ret.Get(0).(func(string, *policyv1.PodDisruptionBudget) error); ok {
		r0 = rf(namespace, podDisruptionBudget)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// CreateOrUpdateRole provides a mock function with given fields: namespace, binding
func (_m *Services) CreateOrUpdateRole(namespace string, binding *rbacv1.Role) error {
	ret := _m.Called(namespace, binding)

	var r0 error
	if rf, ok := ret.Get(0).(func(string, *rbacv1.Role) error); ok {
		r0 = rf(namespace, binding)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// CreateOrUpdateRoleBinding provides a mock function with given fields: namespace, binding
func (_m *Services) CreateOrUpdateRoleBinding(namespace string, binding *rbacv1.RoleBinding) error {
	ret := _m.Called(namespace, binding)

	var r0 error
	if rf, ok := ret.Get(0).(func(string, *rbacv1.RoleBinding) error); ok {
		r0 = rf(namespace, binding)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// CreateOrUpdateService provides a mock function with given fields: namespace, service
func (_m *Services) CreateOrUpdateService(namespace string, service *v1.Service) error {
	ret := _m.Called(namespace, service)

	var r0 error
	if rf, ok := ret.Get(0).(func(string, *v1.Service) error); ok {
		r0 = rf(namespace, service)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// CreateOrUpdateStatefulSet provides a mock function with given fields: namespace, statefulSet
func (_m *Services) CreateOrUpdateStatefulSet(namespace string, statefulSet *appsv1.StatefulSet) error {
	ret := _m.Called(namespace, statefulSet)

	var r0 error
	if rf, ok := ret.Get(0).(func(string, *appsv1.StatefulSet) error); ok {
		r0 = rf(namespace, statefulSet)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// CreatePod provides a mock function with given fields: namespace, pod
func (_m *Services) CreatePod(namespace string, pod *v1.Pod) error {
	ret := _m.Called(namespace, pod)

	var r0 error
	if rf, ok := ret.Get(0).(func(string, *v1.Pod) error); ok {
		r0 = rf(namespace, pod)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// CreatePodDisruptionBudget provides a mock function with given fields: namespace, podDisruptionBudget
func (_m *Services) CreatePodDisruptionBudget(namespace string, podDisruptionBudget *policyv1.PodDisruptionBudget) error {
	ret := _m.Called(namespace, podDisruptionBudget)

	var r0 error
	if rf, ok := ret.Get(0).(func(string, *policyv1.PodDisruptionBudget) error); ok {
		r0 = rf(namespace, podDisruptionBudget)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// CreateRole provides a mock function with given fields: namespace, role
func (_m *Services) CreateRole(namespace string, role *rbacv1.Role) error {
	ret := _m.Called(namespace, role)

	var r0 error
	if rf, ok := ret.Get(0).(func(string, *rbacv1.Role) error); ok {
		r0 = rf(namespace, role)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// CreateRoleBinding provides a mock function with given fields: namespace, binding
func (_m *Services) CreateRoleBinding(namespace string, binding *rbacv1.RoleBinding) error {
	ret := _m.Called(namespace, binding)

	var r0 error
	if rf, ok := ret.Get(0).(func(string, *rbacv1.RoleBinding) error); ok {
		r0 = rf(namespace, binding)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// CreateService provides a mock function with given fields: namespace, service
func (_m *Services) CreateService(namespace string, service *v1.Service) error {
	ret := _m.Called(namespace, service)

	var r0 error
	if rf, ok := ret.Get(0).(func(string, *v1.Service) error); ok {
		r0 = rf(namespace, service)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// CreateStatefulSet provides a mock function with given fields: namespace, statefulSet
func (_m *Services) CreateStatefulSet(namespace string, statefulSet *appsv1.StatefulSet) error {
	ret := _m.Called(namespace, statefulSet)

	var r0 error
	if rf, ok := ret.Get(0).(func(string, *appsv1.StatefulSet) error); ok {
		r0 = rf(namespace, statefulSet)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// DeleteConfigMap provides a mock function with given fields: namespace, name
func (_m *Services) DeleteConfigMap(namespace string, name string) error {
	ret := _m.Called(namespace, name)

	var r0 error
	if rf, ok := ret.Get(0).(func(string, string) error); ok {
		r0 = rf(namespace, name)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// DeleteDeployment provides a mock function with given fields: namespace, name
func (_m *Services) DeleteDeployment(namespace string, name string) error {
	ret := _m.Called(namespace, name)

	var r0 error
	if rf, ok := ret.Get(0).(func(string, string) error); ok {
		r0 = rf(namespace, name)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// DeletePod provides a mock function with given fields: namespace, name
func (_m *Services) DeletePod(namespace string, name string) error {
	ret := _m.Called(namespace, name)

	var r0 error
	if rf, ok := ret.Get(0).(func(string, string) error); ok {
		r0 = rf(namespace, name)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// DeletePodDisruptionBudget provides a mock function with given fields: namespace, name
func (_m *Services) DeletePodDisruptionBudget(namespace string, name string) error {
	ret := _m.Called(namespace, name)

	var r0 error
	if rf, ok := ret.Get(0).(func(string, string) error); ok {
		r0 = rf(namespace, name)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// DeleteService provides a mock function with given fields: namespace, name
func (_m *Services) DeleteService(namespace string, name string) error {
	ret := _m.Called(namespace, name)

	var r0 error
	if rf, ok := ret.Get(0).(func(string, string) error); ok {
		r0 = rf(namespace, name)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// DeleteStatefulSet provides a mock function with given fields: namespace, name
func (_m *Services) DeleteStatefulSet(namespace string, name string) error {
	ret := _m.Called(namespace, name)

	var r0 error
	if rf, ok := ret.Get(0).(func(string, string) error); ok {
		r0 = rf(namespace, name)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// GetClusterRole provides a mock function with given fields: name
func (_m *Services) GetClusterRole(name string) (*rbacv1.ClusterRole, error) {
	ret := _m.Called(name)

	var r0 *rbacv1.ClusterRole
	var r1 error
	if rf, ok := ret.Get(0).(func(string) (*rbacv1.ClusterRole, error)); ok {
		return rf(name)
	}
	if rf, ok := ret.Get(0).(func(string) *rbacv1.ClusterRole); ok {
		r0 = rf(name)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(*rbacv1.ClusterRole)
		}
	}

	if rf, ok := ret.Get(1).(func(string) error); ok {
		r1 = rf(name)
	} else {
		r1 = ret.Error(1)
	}

	return r0, r1
}

// GetConfigMap provides a mock function with given fields: namespace, name
func (_m *Services) GetConfigMap(namespace string, name string) (*v1.ConfigMap, error) {
	ret := _m.Called(namespace, name)

	var r0 *v1.ConfigMap
	var r1 error
	if rf, ok := ret.Get(0).(func(string, string) (*v1.ConfigMap, error)); ok {
		return rf(namespace, name)
	}
	if rf, ok := ret.Get(0).(func(string, string) *v1.ConfigMap); ok {
		r0 = rf(namespace, name)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(*v1.ConfigMap)
		}
	}

	if rf, ok := ret.Get(1).(func(string, string) error); ok {
		r1 = rf(namespace, name)
	} else {
		r1 = ret.Error(1)
	}

	return r0, r1
}

// GetDeployment provides a mock function with given fields: namespace, name
func (_m *Services) GetDeployment(namespace string, name string) (*appsv1.Deployment, error) {
	ret := _m.Called(namespace, name)

	var r0 *appsv1.Deployment
	var r1 error
	if rf, ok := ret.Get(0).(func(string, string) (*appsv1.Deployment, error)); ok {
		return rf(namespace, name)
	}
	if rf, ok := ret.Get(0).(func(string, string) *appsv1.Deployment); ok {
		r0 = rf(namespace, name)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(*appsv1.Deployment)
		}
	}

	if rf, ok := ret.Get(1).(func(string, string) error); ok {
		r1 = rf(namespace, name)
	} else {
		r1 = ret.Error(1)
	}

	return r0, r1
}

// GetDeploymentPods provides a mock function with given fields: namespace, name
func (_m *Services) GetDeploymentPods(namespace string, name string) (*v1.PodList, error) {
	ret := _m.Called(namespace, name)

	var r0 *v1.PodList
	var r1 error
	if rf, ok := ret.Get(0).(func(string, string) (*v1.PodList, error)); ok {
		return rf(namespace, name)
	}
	if rf, ok := ret.Get(0).(func(string, string) *v1.PodList); ok {
		r0 = rf(namespace, name)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(*v1.PodList)
		}
	}

	if rf, ok := ret.Get(1).(func(string, string) error); ok {
		r1 = rf(namespace, name)
	} else {
		r1 = ret.Error(1)
	}

	return r0, r1
}

// GetPod provides a mock function with given fields: namespace, name
func (_m *Services) GetPod(namespace string, name string) (*v1.Pod, error) {
	ret := _m.Called(namespace, name)

	var r0 *v1.Pod
	var r1 error
	if rf, ok := ret.Get(0).(func(string, string) (*v1.Pod, error)); ok {
		return rf(namespace, name)
	}
	if rf, ok := ret.Get(0).(func(string, string) *v1.Pod); ok {
		r0 = rf(namespace, name)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(*v1.Pod)
		}
	}

	if rf, ok := ret.Get(1).(func(string, string) error); ok {
		r1 = rf(namespace, name)
	} else {
		r1 = ret.Error(1)
	}

	return r0, r1
}

// GetPodDisruptionBudget provides a mock function with given fields: namespace, name
func (_m *Services) GetPodDisruptionBudget(namespace string, name string) (*policyv1.PodDisruptionBudget, error) {
	ret := _m.Called(namespace, name)

	var r0 *policyv1.PodDisruptionBudget
	var r1 error
	if rf, ok := ret.Get(0).(func(string, string) (*policyv1.PodDisruptionBudget, error)); ok {
		return rf(namespace, name)
	}
	if rf, ok := ret.Get(0).(func(string, string) *policyv1.PodDisruptionBudget); ok {
		r0 = rf(namespace, name)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(*policyv1.PodDisruptionBudget)
		}
	}

	if rf, ok := ret.Get(1).(func(string, string) error); ok {
		r1 = rf(namespace, name)
	} else {
		r1 = ret.Error(1)
	}

	return r0, r1
}

// GetRole provides a mock function with given fields: namespace, name
func (_m *Services) GetRole(namespace string, name string) (*rbacv1.Role, error) {
	ret := _m.Called(namespace, name)

	var r0 *rbacv1.Role
	var r1 error
	if rf, ok := ret.Get(0).(func(string, string) (*rbacv1.Role, error)); ok {
		return rf(namespace, name)
	}
	if rf, ok := ret.Get(0).(func(string, string) *rbacv1.Role); ok {
		r0 = rf(namespace, name)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(*rbacv1.Role)
		}
	}

	if rf, ok := ret.Get(1).(func(string, string) error); ok {
		r1 = rf(namespace, name)
	} else {
		r1 = ret.Error(1)
	}

	return r0, r1
}

// GetRoleBinding provides a mock function with given fields: namespace, name
func (_m *Services) GetRoleBinding(namespace string, name string) (*rbacv1.RoleBinding, error) {
	ret := _m.Called(namespace, name)

	var r0 *rbacv1.RoleBinding
	var r1 error
	if rf, ok := ret.Get(0).(func(string, string) (*rbacv1.RoleBinding, error)); ok {
		return rf(namespace, name)
	}
	if rf, ok := ret.Get(0).(func(string, string) *rbacv1.RoleBinding); ok {
		r0 = rf(namespace, name)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(*rbacv1.RoleBinding)
		}
	}

	if rf, ok := ret.Get(1).(func(string, string) error); ok {
		r1 = rf(namespace, name)
	} else {
		r1 = ret.Error(1)
	}

	return r0, r1
}

// GetSecret provides a mock function with given fields: namespace, name
func (_m *Services) GetSecret(namespace string, name string) (*v1.Secret, error) {
	ret := _m.Called(namespace, name)

	var r0 *v1.Secret
	var r1 error
	if rf, ok := ret.Get(0).(func(string, string) (*v1.Secret, error)); ok {
		return rf(namespace, name)
	}
	if rf, ok := ret.Get(0).(func(string, string) *v1.Secret); ok {
		r0 = rf(namespace, name)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(*v1.Secret)
		}
	}

	if rf, ok := ret.Get(1).(func(string, string) error); ok {
		r1 = rf(namespace, name)
	} else {
		r1 = ret.Error(1)
	}

	return r0, r1
}

// GetService provides a mock function with given fields: namespace, name
func (_m *Services) GetService(namespace string, name string) (*v1.Service, error) {
	ret := _m.Called(namespace, name)

	var r0 *v1.Service
	var r1 error
	if rf, ok := ret.Get(0).(func(string, string) (*v1.Service, error)); ok {
		return rf(namespace, name)
	}
	if rf, ok := ret.Get(0).(func(string, string) *v1.Service); ok {
		r0 = rf(namespace, name)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(*v1.Service)
		}
	}

	if rf, ok := ret.Get(1).(func(string, string) error); ok {
		r1 = rf(namespace, name)
	} else {
		r1 = ret.Error(1)
	}

	return r0, r1
}

// GetStatefulSet provides a mock function with given fields: namespace, name
func (_m *Services) GetStatefulSet(namespace string, name string) (*appsv1.StatefulSet, error) {
	ret := _m.Called(namespace, name)

	var r0 *appsv1.StatefulSet
	var r1 error
	if rf, ok := ret.Get(0).(func(string, string) (*appsv1.StatefulSet, error)); ok {
		return rf(namespace, name)
	}
	if rf, ok := ret.Get(0).(func(string, string) *appsv1.StatefulSet); ok {
		r0 = rf(namespace, name)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(*appsv1.StatefulSet)
		}
	}

	if rf, ok := ret.Get(1).(func(string, string) error); ok {
		r1 = rf(namespace, name)
	} else {
		r1 = ret.Error(1)
	}

	return r0, r1
}

// GetStatefulSetPods provides a mock function with given fields: namespace, name
func (_m *Services) GetStatefulSetPods(namespace string, name string) (*v1.PodList, error) {
	ret := _m.Called(namespace, name)

	var r0 *v1.PodList
	var r1 error
	if rf, ok := ret.Get(0).(func(string, string) (*v1.PodList, error)); ok {
		return rf(namespace, name)
	}
	if rf, ok := ret.Get(0).(func(string, string) *v1.PodList); ok {
		r0 = rf(namespace, name)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(*v1.PodList)
		}
	}

	if rf, ok := ret.Get(1).(func(string, string) error); ok {
		r1 = rf(namespace, name)
	} else {
		r1 = ret.Error(1)
	}

	return r0, r1
}

// ListConfigMaps provides a mock function with given fields: namespace
func (_m *Services) ListConfigMaps(namespace string) (*v1.ConfigMapList, error) {
	ret := _m.Called(namespace)

	var r0 *v1.ConfigMapList
	var r1 error
	if rf, ok := ret.Get(0).(func(string) (*v1.ConfigMapList, error)); ok {
		return rf(namespace)
	}
	if rf, ok := ret.Get(0).(func(string) *v1.ConfigMapList); ok {
		r0 = rf(namespace)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(*v1.ConfigMapList)
		}
	}

	if rf, ok := ret.Get(1).(func(string) error); ok {
		r1 = rf(namespace)
	} else {
		r1 = ret.Error(1)
	}

	return r0, r1
}

// ListDeployments provides a mock function with given fields: namespace
func (_m *Services) ListDeployments(namespace string) (*appsv1.DeploymentList, error) {
	ret := _m.Called(namespace)

	var r0 *appsv1.DeploymentList
	var r1 error
	if rf, ok := ret.Get(0).(func(string) (*appsv1.DeploymentList, error)); ok {
		return rf(namespace)
	}
	if rf, ok := ret.Get(0).(func(string) *appsv1.DeploymentList); ok {
		r0 = rf(namespace)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(*appsv1.DeploymentList)
		}
	}

	if rf, ok := ret.Get(1).(func(string) error); ok {
		r1 = rf(namespace)
	} else {
		r1 = ret.Error(1)
	}

	return r0, r1
}

// ListPods provides a mock function with given fields: namespace
func (_m *Services) ListPods(namespace string) (*v1.PodList, error) {
	ret := _m.Called(namespace)

	var r0 *v1.PodList
	var r1 error
	if rf, ok := ret.Get(0).(func(string) (*v1.PodList, error)); ok {
		return rf(namespace)
	}
	if rf, ok := ret.Get(0).(func(string) *v1.PodList); ok {
		r0 = rf(namespace)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(*v1.PodList)
		}
	}

	if rf, ok := ret.Get(1).(func(string) error); ok {
		r1 = rf(namespace)
	} else {
		r1 = ret.Error(1)
	}

	return r0, r1
}

// ListRedisFailovers provides a mock function with given fields: ctx, namespace, opts
func (_m *Services) ListRedisFailovers(ctx context.Context, namespace string, opts metav1.ListOptions) (*redisfailoverv1.RedisFailoverList, error) {
	ret := _m.Called(ctx, namespace, opts)

	var r0 *redisfailoverv1.RedisFailoverList
	var r1 error
	if rf, ok := ret.Get(0).(func(context.Context, string, metav1.ListOptions) (*redisfailoverv1.RedisFailoverList, error)); ok {
		return rf(ctx, namespace, opts)
	}
	if rf, ok := ret.Get(0).(func(context.Context, string, metav1.ListOptions) *redisfailoverv1.RedisFailoverList); ok {
		r0 = rf(ctx, namespace, opts)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(*redisfailoverv1.RedisFailoverList)
		}
	}

	if rf, ok := ret.Get(1).(func(context.Context, string, metav1.ListOptions) error); ok {
		r1 = rf(ctx, namespace, opts)
	} else {
		r1 = ret.Error(1)
	}

	return r0, r1
}

// ListServices provides a mock function with given fields: namespace
func (_m *Services) ListServices(namespace string) (*v1.ServiceList, error) {
	ret := _m.Called(namespace)

	var r0 *v1.ServiceList
	var r1 error
	if rf, ok := ret.Get(0).(func(string) (*v1.ServiceList, error)); ok {
		return rf(namespace)
	}
	if rf, ok := ret.Get(0).(func(string) *v1.ServiceList); ok {
		r0 = rf(namespace)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(*v1.ServiceList)
		}
	}

	if rf, ok := ret.Get(1).(func(string) error); ok {
		r1 = rf(namespace)
	} else {
		r1 = ret.Error(1)
	}

	return r0, r1
}

// ListStatefulSets provides a mock function with given fields: namespace
func (_m *Services) ListStatefulSets(namespace string) (*appsv1.StatefulSetList, error) {
	ret := _m.Called(namespace)

	var r0 *appsv1.StatefulSetList
	var r1 error
	if rf, ok := ret.Get(0).(func(string) (*appsv1.StatefulSetList, error)); ok {
		return rf(namespace)
	}
	if rf, ok := ret.Get(0).(func(string) *appsv1.StatefulSetList); ok {
		r0 = rf(namespace)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(*appsv1.StatefulSetList)
		}
	}

	if rf, ok := ret.Get(1).(func(string) error); ok {
		r1 = rf(namespace)
	} else {
		r1 = ret.Error(1)
	}

	return r0, r1
}

// UpdateConfigMap provides a mock function with given fields: namespace, configMap
func (_m *Services) UpdateConfigMap(namespace string, configMap *v1.ConfigMap) error {
	ret := _m.Called(namespace, configMap)

	var r0 error
	if rf, ok := ret.Get(0).(func(string, *v1.ConfigMap) error); ok {
		r0 = rf(namespace, configMap)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// UpdateDeployment provides a mock function with given fields: namespace, deployment
func (_m *Services) UpdateDeployment(namespace string, deployment *appsv1.Deployment) error {
	ret := _m.Called(namespace, deployment)

	var r0 error
	if rf, ok := ret.Get(0).(func(string, *appsv1.Deployment) error); ok {
		r0 = rf(namespace, deployment)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// UpdatePod provides a mock function with given fields: namespace, pod
func (_m *Services) UpdatePod(namespace string, pod *v1.Pod) error {
	ret := _m.Called(namespace, pod)

	var r0 error
	if rf, ok := ret.Get(0).(func(string, *v1.Pod) error); ok {
		r0 = rf(namespace, pod)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// UpdatePodDisruptionBudget provides a mock function with given fields: namespace, podDisruptionBudget
func (_m *Services) UpdatePodDisruptionBudget(namespace string, podDisruptionBudget *policyv1.PodDisruptionBudget) error {
	ret := _m.Called(namespace, podDisruptionBudget)

	var r0 error
	if rf, ok := ret.Get(0).(func(string, *policyv1.PodDisruptionBudget) error); ok {
		r0 = rf(namespace, podDisruptionBudget)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// UpdatePodLabels provides a mock function with given fields: namespace, podName, labels
func (_m *Services) UpdatePodLabels(namespace string, podName string, labels map[string]string) error {
	ret := _m.Called(namespace, podName, labels)

	var r0 error
	if rf, ok := ret.Get(0).(func(string, string, map[string]string) error); ok {
		r0 = rf(namespace, podName, labels)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// UpdateRole provides a mock function with given fields: namespace, role
func (_m *Services) UpdateRole(namespace string, role *rbacv1.Role) error {
	ret := _m.Called(namespace, role)

	var r0 error
	if rf, ok := ret.Get(0).(func(string, *rbacv1.Role) error); ok {
		r0 = rf(namespace, role)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// UpdateRoleBinding provides a mock function with given fields: namespace, binding
func (_m *Services) UpdateRoleBinding(namespace string, binding *rbacv1.RoleBinding) error {
	ret := _m.Called(namespace, binding)

	var r0 error
	if rf, ok := ret.Get(0).(func(string, *rbacv1.RoleBinding) error); ok {
		r0 = rf(namespace, binding)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// UpdateService provides a mock function with given fields: namespace, service
func (_m *Services) UpdateService(namespace string, service *v1.Service) error {
	ret := _m.Called(namespace, service)

	var r0 error
	if rf, ok := ret.Get(0).(func(string, *v1.Service) error); ok {
		r0 = rf(namespace, service)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// UpdateStatefulSet provides a mock function with given fields: namespace, statefulSet
func (_m *Services) UpdateStatefulSet(namespace string, statefulSet *appsv1.StatefulSet) error {
	ret := _m.Called(namespace, statefulSet)

	var r0 error
	if rf, ok := ret.Get(0).(func(string, *appsv1.StatefulSet) error); ok {
		r0 = rf(namespace, statefulSet)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// WatchRedisFailovers provides a mock function with given fields: ctx, namespace, opts
func (_m *Services) WatchRedisFailovers(ctx context.Context, namespace string, opts metav1.ListOptions) (watch.Interface, error) {
	ret := _m.Called(ctx, namespace, opts)

	var r0 watch.Interface
	var r1 error
	if rf, ok := ret.Get(0).(func(context.Context, string, metav1.ListOptions) (watch.Interface, error)); ok {
		return rf(ctx, namespace, opts)
	}
	if rf, ok := ret.Get(0).(func(context.Context, string, metav1.ListOptions) watch.Interface); ok {
		r0 = rf(ctx, namespace, opts)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(watch.Interface)
		}
	}

	if rf, ok := ret.Get(1).(func(context.Context, string, metav1.ListOptions) error); ok {
		r1 = rf(ctx, namespace, opts)
	} else {
		r1 = ret.Error(1)
	}

	return r0, r1
}

func (_m *Services) UpdateRedisFailoverStatus(ctx context.Context, namespace string, redisFailover *redisfailoverv1.RedisFailover, opts metav1.PatchOptions) {}

type mockConstructorTestingTNewServices interface {
	mock.TestingT
	Cleanup(func())
}

// NewServices creates a new instance of Services. It also registers a testing interface on the mock and a cleanup function to assert the mocks expectations.
func NewServices(t mockConstructorTestingTNewServices) *Services {
	mock := &Services{}
	mock.Mock.Test(t)

	t.Cleanup(func() { mock.AssertExpectations(t) })

	return mock
}
