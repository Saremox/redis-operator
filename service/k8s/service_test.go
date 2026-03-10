package k8s_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	kubeerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	kubernetes "k8s.io/client-go/kubernetes/fake"
	kubetesting "k8s.io/client-go/testing"

	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	"github.com/saremox/redis-operator/service/k8s"
)

var (
	servicesGroup = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "services"}
)

func newServiceUpdateAction(ns string, service *corev1.Service) kubetesting.UpdateActionImpl {
	return kubetesting.NewUpdateAction(servicesGroup, ns, service)
}

func newServiceGetAction(ns, name string) kubetesting.GetActionImpl {
	return kubetesting.NewGetAction(servicesGroup, ns, name)
}

func newServiceCreateAction(ns string, service *corev1.Service) kubetesting.CreateActionImpl {
	return kubetesting.NewCreateAction(servicesGroup, ns, service)
}

func TestServiceServiceGetCreateOrUpdate(t *testing.T) {
	testService := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "testservice1",
			ResourceVersion: "10",
		},
	}

	testns := "testns"

	tests := []struct {
		name             string
		service          *corev1.Service
		getServiceResult *corev1.Service
		errorOnGet       error
		errorOnCreation  error
		expActions       []kubetesting.Action
		expErr           bool
	}{
		{
			name:             "A new service should create a new service.",
			service:          testService,
			getServiceResult: nil,
			errorOnGet:       kubeerrors.NewNotFound(schema.GroupResource{}, ""),
			errorOnCreation:  nil,
			expActions: []kubetesting.Action{
				newServiceGetAction(testns, testService.Name),
				newServiceCreateAction(testns, testService),
			},
			expErr: false,
		},
		{
			name:             "A new service should error when create a new service fails.",
			service:          testService,
			getServiceResult: nil,
			errorOnGet:       kubeerrors.NewNotFound(schema.GroupResource{}, ""),
			errorOnCreation:  errors.New("wanted error"),
			expActions: []kubetesting.Action{
				newServiceGetAction(testns, testService.Name),
				newServiceCreateAction(testns, testService),
			},
			expErr: true,
		},
		{
			name:             "An existent service should update the service.",
			service:          testService,
			getServiceResult: testService,
			errorOnGet:       nil,
			errorOnCreation:  nil,
			expActions: []kubetesting.Action{
				newServiceGetAction(testns, testService.Name),
				newServiceUpdateAction(testns, testService),
			},
			expErr: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertTest := assert.New(t)

			// Mock.
			mcli := &kubernetes.Clientset{}
			mcli.AddReactor("get", "services", func(action kubetesting.Action) (bool, runtime.Object, error) {
				return true, test.getServiceResult, test.errorOnGet
			})
			mcli.AddReactor("create", "services", func(action kubetesting.Action) (bool, runtime.Object, error) {
				return true, nil, test.errorOnCreation
			})

			service := k8s.NewServiceService(mcli, log.Dummy, metrics.Dummy)
			err := service.CreateOrUpdateService(testns, test.service)

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

func TestCreateOrUpdateServicePreservesImmutableFields(t *testing.T) {
	testns := "testns"

	ipFamilyPolicy := corev1.IPFamilyPolicySingleStack
	storedService := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "testsvc",
			ResourceVersion: "42",
		},
		Spec: corev1.ServiceSpec{
			ClusterIP:      "10.0.0.1",
			ClusterIPs:     []string{"10.0.0.1"},
			IPFamilies:     []corev1.IPFamily{corev1.IPv4Protocol},
			IPFamilyPolicy: &ipFamilyPolicy,
			Ports: []corev1.ServicePort{
				{
					Name:       "redis",
					Port:       6379,
					TargetPort: intstr.FromString("redis"),
					Protocol:   corev1.ProtocolTCP,
					NodePort:   30001,
				},
			},
		},
	}

	// Desired service omits server-assigned immutable fields (as generators do).
	desiredService := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "testsvc",
			Labels: map[string]string{"app": "redis"},
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeClusterIP,
			Ports: []corev1.ServicePort{
				{
					Name:       "redis",
					Port:       6379,
					TargetPort: intstr.FromString("redis"),
					Protocol:   corev1.ProtocolTCP,
				},
			},
		},
	}

	var updatedService *corev1.Service
	mcli := &kubernetes.Clientset{}
	mcli.AddReactor("get", "services", func(action kubetesting.Action) (bool, runtime.Object, error) {
		return true, storedService, nil
	})
	mcli.AddReactor("update", "services", func(action kubetesting.Action) (bool, runtime.Object, error) {
		ua := action.(kubetesting.UpdateAction)
		updatedService = ua.GetObject().(*corev1.Service)
		return true, updatedService, nil
	})

	svc := k8s.NewServiceService(mcli, log.Dummy, metrics.Dummy)
	err := svc.CreateOrUpdateService(testns, desiredService)

	assert.NoError(t, err)
	assert.Equal(t, "42", updatedService.ResourceVersion, "ResourceVersion must be preserved")
	assert.Equal(t, "10.0.0.1", updatedService.Spec.ClusterIP, "clusterIP must be preserved")
	assert.Equal(t, []string{"10.0.0.1"}, updatedService.Spec.ClusterIPs, "clusterIPs must be preserved")
	assert.Equal(t, []corev1.IPFamily{corev1.IPv4Protocol}, updatedService.Spec.IPFamilies, "ipFamilies must be preserved")
	assert.Equal(t, &ipFamilyPolicy, updatedService.Spec.IPFamilyPolicy, "ipFamilyPolicy must be preserved")
	assert.Equal(t, int32(30001), updatedService.Spec.Ports[0].NodePort, "nodePort must be preserved for matching port")
	// Mutable fields must still reflect desired state.
	assert.Equal(t, map[string]string{"app": "redis"}, updatedService.Labels, "labels must come from desired service")
}

func TestCreateOrUpdateServicePreservesHealthCheckNodePort(t *testing.T) {
	testns := "testns"

	storedService := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "testsvc-hc",
			ResourceVersion: "7",
		},
		Spec: corev1.ServiceSpec{
			Type:                  corev1.ServiceTypeLoadBalancer,
			ExternalTrafficPolicy: corev1.ServiceExternalTrafficPolicyLocal,
			HealthCheckNodePort:   32100,
			ClusterIP:             "10.0.0.2",
			ClusterIPs:            []string{"10.0.0.2"},
		},
	}

	desiredService := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "testsvc-hc",
		},
		Spec: corev1.ServiceSpec{
			Type:                  corev1.ServiceTypeLoadBalancer,
			ExternalTrafficPolicy: corev1.ServiceExternalTrafficPolicyLocal,
		},
	}

	var updatedService *corev1.Service
	mcli := &kubernetes.Clientset{}
	mcli.AddReactor("get", "services", func(action kubetesting.Action) (bool, runtime.Object, error) {
		return true, storedService, nil
	})
	mcli.AddReactor("update", "services", func(action kubetesting.Action) (bool, runtime.Object, error) {
		ua := action.(kubetesting.UpdateAction)
		updatedService = ua.GetObject().(*corev1.Service)
		return true, updatedService, nil
	})

	svc := k8s.NewServiceService(mcli, log.Dummy, metrics.Dummy)
	err := svc.CreateOrUpdateService(testns, desiredService)

	assert.NoError(t, err)
	assert.Equal(t, int32(32100), updatedService.Spec.HealthCheckNodePort, "healthCheckNodePort must be preserved")
	assert.Equal(t, "10.0.0.2", updatedService.Spec.ClusterIP, "clusterIP must be preserved")
}
