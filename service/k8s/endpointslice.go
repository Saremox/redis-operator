package k8s

import (
	"context"
	"fmt"

	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
)

// EndpointSlice reads the addresses that a Service routes to.
type EndpointSlice interface {
	GetEndpointAddresses(namespace, serviceName string) ([]string, error)
}

// EndpointSliceService is the EndpointSlice implementation using API calls to kubernetes.
type EndpointSliceService struct {
	kubeClient      kubernetes.Interface
	metricsRecorder metrics.Recorder
}

func NewEndpointSliceService(kubeClient kubernetes.Interface, _ log.Logger, metricsRecorder metrics.Recorder) *EndpointSliceService {
	return &EndpointSliceService{kubeClient: kubeClient, metricsRecorder: metricsRecorder}
}

// GetEndpointAddresses returns the addresses in all EndpointSlices of the
// Service. A pod that the Service no longer routes to is missing from the list.
func (s *EndpointSliceService) GetEndpointAddresses(namespace, serviceName string) ([]string, error) {
	selector := metav1.ListOptions{LabelSelector: discoveryv1.LabelServiceName + "=" + serviceName}
	slices, err := s.kubeClient.DiscoveryV1().EndpointSlices(namespace).List(context.TODO(), selector)
	recordMetrics(namespace, "EndpointSlice", serviceName, "LIST", err, s.metricsRecorder)
	if err != nil {
		return nil, fmt.Errorf("list endpointslices of service %s/%s: %w", namespace, serviceName, err)
	}
	var addresses []string
	for _, slice := range slices.Items {
		for _, endpoint := range slice.Endpoints {
			addresses = append(addresses, endpoint.Addresses...)
		}
	}
	return addresses, nil
}
