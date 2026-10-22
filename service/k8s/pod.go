package k8s

import (
	"context"
	"encoding/json"

	"k8s.io/apimachinery/pkg/types"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
)

// Pod the ServiceAccount service that knows how to interact with k8s to manage them
type Pod interface {
	GetPod(namespace string, name string) (*corev1.Pod, error)
	DeletePod(namespace string, name string) error
	ListPods(namespace string) (*corev1.PodList, error)
	UpdatePodLabels(namespace, podName string, labels map[string]string) error
	UpdatePodAnnotations(namespace, podName string, annotations map[string]string) error
}

// PodService is the pod service implementation using API calls to kubernetes.
type PodService struct {
	kubeClient      kubernetes.Interface
	logger          log.Logger
	metricsRecorder metrics.Recorder
}

// NewPodService returns a new Pod KubeService.
func NewPodService(kubeClient kubernetes.Interface, logger log.Logger, metricsRecorder metrics.Recorder) *PodService {
	logger = logger.With("service", "k8s.pod")
	return &PodService{
		kubeClient:      kubeClient,
		logger:          logger,
		metricsRecorder: metricsRecorder,
	}
}

func (p *PodService) GetPod(namespace string, name string) (*corev1.Pod, error) {
	pod, err := p.kubeClient.CoreV1().Pods(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	recordMetrics(namespace, "Pod", name, "GET", err, p.metricsRecorder)
	if err != nil {
		return nil, err
	}
	return pod, err
}

func (p *PodService) DeletePod(namespace string, name string) error {
	err := p.kubeClient.CoreV1().Pods(namespace).Delete(context.TODO(), name, metav1.DeleteOptions{})
	recordMetrics(namespace, "Pod", name, "DELETE", err, p.metricsRecorder)
	return err
}

func (p *PodService) ListPods(namespace string) (*corev1.PodList, error) {
	pods, err := p.kubeClient.CoreV1().Pods(namespace).List(context.TODO(), metav1.ListOptions{})
	recordMetrics(namespace, "Pod", metrics.NOT_APPLICABLE, "LIST", err, p.metricsRecorder)
	return pods, err
}

// UpdatePodLabels sets the given labels on a pod. It uses a JSON merge patch
// because a JSON patch path needs `/` and `~` escaped in label keys such as
// `app.kubernetes.io/component`. An empty map sends no patch, because the
// merge patch `"labels": null` deletes all labels of the pod.
func (p *PodService) UpdatePodLabels(namespace, podName string, labels map[string]string) error {
	if len(labels) == 0 {
		return nil
	}
	p.logger.Infof("Update pod label, namespace: %s, pod name: %s, labels: %v", namespace, podName, labels)

	patch := map[string]interface{}{
		"metadata": map[string]interface{}{
			"labels": labels,
		},
	}
	payloadBytes, _ := json.Marshal(patch)

	_, err := p.kubeClient.CoreV1().Pods(namespace).Patch(context.TODO(), podName, types.MergePatchType, payloadBytes, metav1.PatchOptions{})
	recordMetrics(namespace, "Pod", podName, "PATCH", err, p.metricsRecorder)
	if err != nil {
		p.logger.Errorf("Update pod labels failed, namespace: %s, pod name: %s, error: %v", namespace, podName, err)
	}
	return err
}

// UpdatePodAnnotations sets the given annotations on a pod. The JSON merge
// patch creates the annotations map when it is absent and keeps the other
// annotations.
func (p *PodService) UpdatePodAnnotations(namespace, podName string, annotations map[string]string) error {
	p.logger.Infof("Update pod annotations, namespace: %s, pod name: %s, annotations: %v", namespace, podName, annotations)

	patch := map[string]interface{}{
		"metadata": map[string]interface{}{
			"annotations": annotations,
		},
	}
	payloadBytes, _ := json.Marshal(patch)

	_, err := p.kubeClient.CoreV1().Pods(namespace).Patch(context.TODO(), podName, types.MergePatchType, payloadBytes, metav1.PatchOptions{})
	recordMetrics(namespace, "Pod", podName, "PATCH", err, p.metricsRecorder)
	if err != nil {
		p.logger.Errorf("Update pod annotations failed, namespace: %s, pod name: %s, error: %v", namespace, podName, err)
	}
	return err
}
