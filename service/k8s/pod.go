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

// PatchStringValue specifies a patch operation for a string.
type PatchStringValue struct {
	Op    string      `json:"op"`
	Path  string      `json:"path"`
	Value interface{} `json:"value"`
}

func (p *PodService) UpdatePodLabels(namespace, podName string, labels map[string]string) error {
	p.logger.Infof("Update pod label, namespace: %s, pod name: %s, labels: %v", namespace, podName, labels)

	var payloads []interface{}
	for labelKey, labelValue := range labels {
		payload := PatchStringValue{
			Op:    "replace",
			Path:  "/metadata/labels/" + labelKey,
			Value: labelValue,
		}
		payloads = append(payloads, payload)
	}
	payloadBytes, _ := json.Marshal(payloads)

	_, err := p.kubeClient.CoreV1().Pods(namespace).Patch(context.TODO(), podName, types.JSONPatchType, payloadBytes, metav1.PatchOptions{})
	recordMetrics(namespace, "Pod", podName, "PATCH", err, p.metricsRecorder)
	if err != nil {
		p.logger.Errorf("Update pod labels failed, namespace: %s, pod name: %s, error: %v", namespace, podName, err)
	}
	return err
}

// UpdatePodAnnotations sets the given annotations on a pod. It uses a JSON merge
// patch so the annotations map is created when absent and existing annotations
// are left untouched, unlike the JSON-patch "replace" used for labels.
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

// ResizePod sets the resources of the named containers through the pod's
// resize subresource.
func (p *PodService) ResizePod(namespace, podName string, resources map[string]corev1.ResourceRequirements) error {
	names := make([]string, 0, len(resources))
	for name := range resources {
		names = append(names, name)
	}
	sort.Strings(names)
	containers := make([]map[string]interface{}, 0, len(resources))
	for _, name := range names {
		containers = append(containers, map[string]interface{}{"name": name, "resources": resources[name]})
	}
	payloadBytes, _ := json.Marshal(map[string]interface{}{"spec": map[string]interface{}{"containers": containers}})
	_, err := p.kubeClient.CoreV1().Pods(namespace).Patch(context.TODO(), podName, types.StrategicMergePatchType, payloadBytes, metav1.PatchOptions{}, "resize")
	recordMetrics(namespace, "Pod", podName, "RESIZE", err, p.metricsRecorder)
	return err
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
		return PodResizeSupport{}, err
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
