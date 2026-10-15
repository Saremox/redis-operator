package k8s

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
)

// Pod the Pod service that knows how to interact with k8s to manage them.
type Pod interface {
	GetPod(namespace string, name string) (*corev1.Pod, error)
	DeletePod(namespace string, name string) error
	DeletePodWithUID(namespace string, name string, uid types.UID) error
	ListPods(namespace string) (*corev1.PodList, error)
	UpdatePodLabels(namespace, podName string, labels map[string]string) error
	UpdatePodAnnotations(namespace, podName string, annotations map[string]string) error
	RemovePodAnnotation(namespace, podName, key string) error
	ResizePod(namespace, podName string, resources map[string]corev1.ResourceRequirements) error
	PodResizeSupport() (PodResizeSupport, error)
}

// PodResizeSupport describes the cluster's support for in-place pod resize.
type PodResizeSupport struct {
	// Supported is set from Kubernetes 1.33, where in-place resize is on by default.
	Supported bool
	// MemoryLimitDecrease is set from Kubernetes 1.35, which allows lowering a
	// memory limit without restarting the container.
	MemoryLimitDecrease bool
}

// PodService is the pod service implementation using API calls to kubernetes.
type PodService struct {
	kubeClient      kubernetes.Interface
	logger          log.Logger
	metricsRecorder metrics.Recorder

	resizeSupportMu      sync.Mutex
	resizeSupport        PodResizeSupport
	resizeSupportChecked time.Time
}

// podResizeSupportTTL bounds how long the detected support is cached, so a
// cluster upgrade is picked up without restarting the operator.
const podResizeSupportTTL = 10 * time.Minute

// NewPodService returns a new Pod KubeService.
func NewPodService(kubeClient kubernetes.Interface, logger log.Logger, metricsRecorder metrics.Recorder) *PodService {
	logger = logger.With("service", "k8s.pod")
	return &PodService{
		kubeClient:      kubeClient,
		logger:          logger,
		metricsRecorder: metricsRecorder,
	}
}

// GetPod returns the named pod.
func (p *PodService) GetPod(namespace, name string) (*corev1.Pod, error) {
	pod, err := p.kubeClient.CoreV1().Pods(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	recordMetrics(namespace, "Pod", name, "GET", err, p.metricsRecorder)
	if err != nil {
		return nil, fmt.Errorf("get pod %s/%s: %w", namespace, name, err)
	}
	return pod, nil
}

// DeletePod deletes the named pod. Use DeletePodWithUID when a pod with the
// same name must stay.
func (p *PodService) DeletePod(namespace, name string) error {
	err := p.kubeClient.CoreV1().Pods(namespace).Delete(context.TODO(), name, metav1.DeleteOptions{})
	recordMetrics(namespace, "Pod", name, "DELETE", err, p.metricsRecorder)
	if err != nil {
		return fmt.Errorf("delete pod %s/%s: %w", namespace, name, err)
	}
	return nil
}

// DeletePodWithUID deletes the pod only while it has the UID uid. A pod
// created again with the same name stays, and the delete fails with a
// Conflict.
func (p *PodService) DeletePodWithUID(namespace, name string, uid types.UID) error {
	err := p.kubeClient.CoreV1().Pods(namespace).Delete(context.TODO(), name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
	recordMetrics(namespace, "Pod", name, "DELETE", err, p.metricsRecorder)
	if err != nil {
		return fmt.Errorf("delete pod %s/%s: %w", namespace, name, err)
	}
	return nil
}

// ListPods returns all the pods of the namespace.
func (p *PodService) ListPods(namespace string) (*corev1.PodList, error) {
	pods, err := p.kubeClient.CoreV1().Pods(namespace).List(context.TODO(), metav1.ListOptions{})
	recordMetrics(namespace, "Pod", metrics.NOT_APPLICABLE, "LIST", err, p.metricsRecorder)
	if err != nil {
		return pods, fmt.Errorf("list pods in %s: %w", namespace, err)
	}
	return pods, nil
}

// UpdatePodLabels sets the given labels on a pod. It uses a JSON merge patch
// because a JSON patch path needs `/` and `~` escaped in label keys such as
// `app.kubernetes.io/component`. An empty map sends no patch, because the
// merge patch `"labels": null` deletes all labels of the pod.
func (p *PodService) UpdatePodLabels(namespace, podName string, labels map[string]string) error {
	if len(labels) == 0 {
		return nil
	}
	p.logger.WithField("namespace", namespace).WithField("pod", podName).Infof("Update pod label, labels: %v", labels)

	patch := map[string]any{
		"metadata": map[string]any{
			"labels": labels,
		},
	}
	payloadBytes, _ := json.Marshal(patch)

	_, err := p.kubeClient.CoreV1().Pods(namespace).Patch(context.TODO(), podName, types.MergePatchType, payloadBytes, metav1.PatchOptions{})
	recordMetrics(namespace, "Pod", podName, "PATCH", err, p.metricsRecorder)
	if err != nil {
		logger := p.logger.WithField("namespace", namespace).WithField("pod", podName)
		logf := logger.Errorf
		if errors.IsNotFound(err) {
			// The reconcile read the pod before it was deleted.
			logf = logger.Debugf
		}
		logf("Update pod labels failed: %v", err)
		return fmt.Errorf("patch the labels of pod %s/%s: %w", namespace, podName, err)
	}
	return nil
}

// UpdatePodAnnotations sets the given annotations on a pod. The JSON merge
// patch creates the annotations map when it is absent and keeps the other
// annotations.
func (p *PodService) UpdatePodAnnotations(namespace, podName string, annotations map[string]string) error {
	p.logger.WithField("namespace", namespace).WithField("pod", podName).Infof("Update pod annotations, annotations: %v", annotations)

	patch := map[string]any{
		"metadata": map[string]any{
			"annotations": annotations,
		},
	}
	payloadBytes, _ := json.Marshal(patch)

	_, err := p.kubeClient.CoreV1().Pods(namespace).Patch(context.TODO(), podName, types.MergePatchType, payloadBytes, metav1.PatchOptions{})
	recordMetrics(namespace, "Pod", podName, "PATCH", err, p.metricsRecorder)
	if err != nil {
		p.logger.WithField("namespace", namespace).WithField("pod", podName).Errorf("Update pod annotations failed: %v", err)
		return fmt.Errorf("patch the annotations of pod %s/%s: %w", namespace, podName, err)
	}
	return nil
}

// RemovePodAnnotation deletes one annotation from a pod. In a JSON merge patch,
// a null value deletes the key.
func (p *PodService) RemovePodAnnotation(namespace, podName, key string) error {
	p.logger.WithField("namespace", namespace).WithField("pod", podName).Infof("Remove pod annotation, annotation: %s", key)

	patch := map[string]any{
		"metadata": map[string]any{
			"annotations": map[string]any{key: nil},
		},
	}
	payloadBytes, _ := json.Marshal(patch)

	_, err := p.kubeClient.CoreV1().Pods(namespace).Patch(context.TODO(), podName, types.MergePatchType, payloadBytes, metav1.PatchOptions{})
	recordMetrics(namespace, "Pod", podName, "PATCH", err, p.metricsRecorder)
	if err != nil {
		p.logger.WithField("namespace", namespace).WithField("pod", podName).Errorf("Remove pod annotation failed: %v", err)
		return fmt.Errorf("remove annotation %s of pod %s/%s: %w", key, namespace, podName, err)
	}
	return nil
}

// ResizePod sets the resources of the named containers through the pod's
// resize subresource.
func (p *PodService) ResizePod(namespace, podName string, resources map[string]corev1.ResourceRequirements) error {
	names := make([]string, 0, len(resources))
	for name := range resources {
		names = append(names, name)
	}
	sort.Strings(names)
	containers := make([]map[string]any, 0, len(resources))
	for _, name := range names {
		containers = append(containers, map[string]any{"name": name, "resources": resources[name]})
	}
	payloadBytes, _ := json.Marshal(map[string]any{"spec": map[string]any{"containers": containers}})
	_, err := p.kubeClient.CoreV1().Pods(namespace).Patch(context.TODO(), podName, types.StrategicMergePatchType, payloadBytes, metav1.PatchOptions{}, "resize")
	recordMetrics(namespace, "Pod", podName, "RESIZE", err, p.metricsRecorder)
	if err != nil {
		return fmt.Errorf("resize pod %s/%s: %w", namespace, podName, err)
	}
	return nil
}

// PodResizeSupport reports the cluster's support for in-place pod resize.
func (p *PodService) PodResizeSupport() (PodResizeSupport, error) {
	p.resizeSupportMu.Lock()
	defer p.resizeSupportMu.Unlock()
	if !p.resizeSupportChecked.IsZero() && time.Since(p.resizeSupportChecked) < podResizeSupportTTL {
		return p.resizeSupport, nil
	}
	version, err := p.kubeClient.Discovery().ServerVersion()
	if err != nil {
		return PodResizeSupport{}, fmt.Errorf("get the server version: %w", err)
	}
	// Minor can carry a suffix, e.g. "35+".
	minor, err := strconv.Atoi(strings.TrimRight(version.Minor, "+"))
	if err != nil || version.Major != "1" {
		return PodResizeSupport{}, fmt.Errorf("unexpected server version %s.%s", version.Major, version.Minor)
	}
	p.resizeSupport = PodResizeSupport{Supported: minor >= 33, MemoryLimitDecrease: minor >= 35}
	p.resizeSupportChecked = time.Now()
	return p.resizeSupport, nil
}
