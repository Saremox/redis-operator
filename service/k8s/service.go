package k8s

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
)

// Service the Service service that knows how to interact with k8s to manage them.
type Service interface {
	GetService(namespace string, name string) (*corev1.Service, error)
	CreateService(namespace string, service *corev1.Service) error
	UpdateService(namespace string, service *corev1.Service) error
	CreateOrUpdateService(namespace string, service *corev1.Service) error
	DeleteService(namespace string, name string) error
	ListServices(namespace string) (*corev1.ServiceList, error)
}

// ServiceService implements Service with calls to the Kubernetes API.
type ServiceService struct {
	kubeClient      kubernetes.Interface
	logger          log.Logger
	metricsRecorder metrics.Recorder
}

// NewServiceService returns a new Service KubeService.
func NewServiceService(kubeClient kubernetes.Interface, logger log.Logger, metricsRecorder metrics.Recorder) *ServiceService {
	logger = logger.With("service", "k8s.service")
	return &ServiceService{
		kubeClient:      kubeClient,
		logger:          logger,
		metricsRecorder: metricsRecorder,
	}
}

// GetService returns the named Service.
func (s *ServiceService) GetService(namespace, name string) (*corev1.Service, error) {
	service, err := s.kubeClient.CoreV1().Services(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	recordMetrics(namespace, "Service", name, "GET", err, s.metricsRecorder)
	if err != nil {
		return nil, fmt.Errorf("get service %s/%s: %w", namespace, name, err)
	}
	return service, nil
}

// CreateService creates the given Service.
func (s *ServiceService) CreateService(namespace string, service *corev1.Service) error {
	_, err := s.kubeClient.CoreV1().Services(namespace).Create(context.TODO(), service, metav1.CreateOptions{})
	recordMetrics(namespace, "Service", service.GetName(), "CREATE", err, s.metricsRecorder)
	if err != nil {
		return fmt.Errorf("create service %s/%s: %w", namespace, service.Name, err)
	}
	s.logger.WithField("namespace", namespace).WithField("serviceName", service.Name).Debugf("service created")
	return nil
}

// UpdateService replaces the stored Service with the given Service.
func (s *ServiceService) UpdateService(namespace string, service *corev1.Service) error {
	_, err := s.kubeClient.CoreV1().Services(namespace).Update(context.TODO(), service, metav1.UpdateOptions{})
	recordMetrics(namespace, "Service", service.GetName(), "UPDATE", err, s.metricsRecorder)
	if err != nil {
		return fmt.Errorf("update service %s/%s: %w", namespace, service.Name, err)
	}
	s.logger.WithField("namespace", namespace).WithField("serviceName", service.Name).Debugf("service updated")
	return nil
}

// CreateOrUpdateService creates the Service, or updates the stored Service
// when it is not up to date. It keeps the immutable fields that the API server
// assigned, and returns a ControlledByOtherError when another controller
// controls the stored Service.
func (s *ServiceService) CreateOrUpdateService(namespace string, service *corev1.Service) error {
	storedService, err := s.GetService(namespace, service.Name)
	if err != nil {
		if errors.IsNotFound(err) {
			return s.CreateService(namespace, service)
		}
		log.Errorf("Error while updating service %v in %v namespace : %v", service.GetName(), namespace, err)
		return err
	}

	if err := checkNotControlledByOther("Service", storedService, service); err != nil {
		return err
	}

	// With the stored resource version, the update fails when the object
	// changed after the read.
	service.ResourceVersion = storedService.ResourceVersion
	mergeImmutableServiceFields(storedService, service)

	if serviceUpToDate(storedService, service) {
		s.logger.WithField("namespace", namespace).WithField("serviceName", service.Name).Debugf("service already up to date, skipping update")
		return nil
	}

	return s.UpdateService(namespace, service)
}

// mergeImmutableServiceFields copies server-assigned immutable fields from the
// stored Service into the desired Service so that update calls do not drop them.
// The operator still controls labels, selectors, annotations, and desired ports.
func mergeImmutableServiceFields(stored, desired *corev1.Service) {
	if desired.Spec.ClusterIP == "" {
		desired.Spec.ClusterIP = stored.Spec.ClusterIP
	}
	if len(desired.Spec.ClusterIPs) == 0 {
		desired.Spec.ClusterIPs = stored.Spec.ClusterIPs
	}
	if len(desired.Spec.IPFamilies) == 0 {
		desired.Spec.IPFamilies = stored.Spec.IPFamilies
	}
	if desired.Spec.IPFamilyPolicy == nil {
		desired.Spec.IPFamilyPolicy = stored.Spec.IPFamilyPolicy
	}
	if desired.Spec.HealthCheckNodePort == 0 {
		desired.Spec.HealthCheckNodePort = stored.Spec.HealthCheckNodePort
	}

	// Preserve nodePort for each port that matches by name and protocol.
	storedPorts := make(map[string]int32, len(stored.Spec.Ports))
	for _, p := range stored.Spec.Ports {
		storedPorts[p.Name+"/"+string(p.Protocol)] = p.NodePort
	}
	for i := range desired.Spec.Ports {
		p := &desired.Spec.Ports[i]
		if p.NodePort == 0 {
			p.NodePort = storedPorts[p.Name+"/"+string(p.Protocol)]
		}
	}
}

// DeleteService deletes the named Service with foreground propagation.
func (s *ServiceService) DeleteService(namespace, name string) error {
	propagation := metav1.DeletePropagationForeground
	err := s.kubeClient.CoreV1().Services(namespace).Delete(context.TODO(), name, metav1.DeleteOptions{PropagationPolicy: &propagation})
	recordMetrics(namespace, "Service", name, "DELETE", err, s.metricsRecorder)
	if err != nil {
		return fmt.Errorf("delete service %s/%s: %w", namespace, name, err)
	}
	return nil
}

// ListServices returns all the Services of the namespace.
func (s *ServiceService) ListServices(namespace string) (*corev1.ServiceList, error) {
	serviceList, err := s.kubeClient.CoreV1().Services(namespace).List(context.TODO(), metav1.ListOptions{})
	recordMetrics(namespace, "Service", metrics.NOT_APPLICABLE, "LIST", err, s.metricsRecorder)
	if err != nil {
		return serviceList, fmt.Errorf("list services in %s: %w", namespace, err)
	}
	return serviceList, nil
}
