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
			// The stored and desired objects must actually differ here: an
			// identical desired object is now a no-op (see
			// TestServiceServiceObjectUpToDate) and would issue no Update
			// action, defeating the point of this test.
			name: "An existent service should update the service.",
			service: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: "testservice1"},
				Spec:       corev1.ServiceSpec{Selector: map[string]string{"app": "redis"}},
			},
			getServiceResult: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: "testservice1", ResourceVersion: "10"},
			},
			errorOnGet:      nil,
			errorOnCreation: nil,
			expActions: []kubetesting.Action{
				newServiceGetAction(testns, "testservice1"),
				newServiceUpdateAction(testns, &corev1.Service{
					ObjectMeta: metav1.ObjectMeta{Name: "testservice1", ResourceVersion: "10"},
					Spec:       corev1.ServiceSpec{Selector: map[string]string{"app": "redis"}},
				}),
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
			// A real change (new selector) so the update actually fires: an
			// otherwise-identical desired object is now a no-op (see
			// TestServiceServiceObjectUpToDate) and would never call Update,
			// leaving nothing for this test's assertions to check.
			Selector: map[string]string{"app": "redis"},
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

// realisticService returns a Service shaped like what generateRedisService
// builds: Type and every port's Protocol are explicit, but SessionAffinity
// and InternalTrafficPolicy are left unset, relying on the API server to
// default them.
func realisticService(selectorValue string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "rfs-test",
			Namespace: "testns",
			Labels:    map[string]string{"app.kubernetes.io/name": "test"},
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: map[string]string{"app.kubernetes.io/name": selectorValue},
			Ports: []corev1.ServicePort{
				{Name: "redis", Port: 6379, TargetPort: intstr.FromString("redis"), Protocol: corev1.ProtocolTCP},
			},
		},
	}
}

// serverDefaultedService is realisticDeployment's serverDefaulted
// counterpart for Service.
func serverDefaultedService(s *corev1.Service) *corev1.Service {
	s = s.DeepCopy()
	s.Spec.ClusterIP = "10.0.0.5"
	s.Spec.ClusterIPs = []string{"10.0.0.5"}
	s.Spec.SessionAffinity = corev1.ServiceAffinityNone
	policy := corev1.ServiceInternalTrafficPolicyCluster
	s.Spec.InternalTrafficPolicy = &policy
	return s
}

func TestServiceServiceObjectUpToDate(t *testing.T) {
	testns := "testns"

	tests := []struct {
		name          string
		stored        *corev1.Service
		desired       *corev1.Service
		expectUpdates int
	}{
		{
			name:          "identical desired is a no-op",
			stored:        realisticService("test"),
			desired:       realisticService("test"),
			expectUpdates: 0,
		},
		{
			name:          "server-defaulted fields the operator never sets do not trigger an update",
			stored:        serverDefaultedService(realisticService("test")),
			desired:       realisticService("test"),
			expectUpdates: 0,
		},
		{
			name:          "a real spec change still triggers an update",
			stored:        realisticService("test"),
			desired:       realisticService("changed"),
			expectUpdates: 1,
		},
		{
			name:          "manual drift on the live object is detected and corrected, even though desired is unchanged",
			stored:        realisticService("drifted"),
			desired:       realisticService("test"),
			expectUpdates: 1,
		},
		{
			name: "an annotation change still triggers an update",
			stored: func() *corev1.Service {
				s := realisticService("test")
				s.Annotations = map[string]string{"prometheus.io/scrape": "true"}
				return s
			}(),
			desired:       realisticService("test"),
			expectUpdates: 1,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)

			stored := test.stored.DeepCopy()
			stored.ResourceVersion = "1"
			mcli := kubernetes.NewClientset(stored)

			service := k8s.NewServiceService(mcli, log.Dummy, metrics.Dummy)
			assert.NoError(service.CreateOrUpdateService(testns, test.desired.DeepCopy()))

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

func TestServiceServiceCreateOrUpdateServiceGetError(t *testing.T) {
	assertTest := assert.New(t)
	testns := "testns"

	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "testsvc",
		},
	}

	mcli := &kubernetes.Clientset{}
	mcli.AddReactor("get", "services", func(action kubetesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("connection refused")
	})

	svc := k8s.NewServiceService(mcli, log.Dummy, metrics.Dummy)
	err := svc.CreateOrUpdateService(testns, service)
	assertTest.Error(err)
	assertTest.Equal([]kubetesting.Action{newServiceGetAction(testns, service.Name)}, mcli.Actions())
}

func TestServiceServiceDeleteService(t *testing.T) {
	testns := "testns"

	t.Run("deletes an existing Service", func(t *testing.T) {
		assertTest := assert.New(t)

		svc := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "testsvc",
				Namespace: testns,
			},
		}
		mcli := kubernetes.NewClientset(svc)
		service := k8s.NewServiceService(mcli, log.Dummy, metrics.Dummy)

		err := service.DeleteService(testns, "testsvc")
		assertTest.NoError(err)

		_, err = mcli.CoreV1().Services(testns).Get(context.TODO(), "testsvc", metav1.GetOptions{})
		assertTest.Error(err)
		assertTest.True(kubeerrors.IsNotFound(err))
	})

	t.Run("returns an error when deleting a non-existent Service", func(t *testing.T) {
		assertTest := assert.New(t)

		mcli := kubernetes.NewClientset()
		service := k8s.NewServiceService(mcli, log.Dummy, metrics.Dummy)

		err := service.DeleteService(testns, "does-not-exist")
		assertTest.Error(err)
		assertTest.True(kubeerrors.IsNotFound(err))
	})
}

func TestServiceServiceListServices(t *testing.T) {
	assertTest := assert.New(t)
	testns := "testns"

	svc1 := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "svc1", Namespace: testns}}
	svc2 := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "svc2", Namespace: testns}}
	otherNsSvc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "svc3", Namespace: "otherns"}}

	mcli := kubernetes.NewClientset(svc1, svc2, otherNsSvc)
	service := k8s.NewServiceService(mcli, log.Dummy, metrics.Dummy)

	list, err := service.ListServices(testns)
	assertTest.NoError(err)
	assertTest.Len(list.Items, 2)
}
