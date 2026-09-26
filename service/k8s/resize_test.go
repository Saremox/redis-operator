package k8s_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/version"
	fakediscovery "k8s.io/client-go/discovery/fake"
	kubernetes "k8s.io/client-go/kubernetes/fake"
	kubetesting "k8s.io/client-go/testing"

	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	"github.com/saremox/redis-operator/service/k8s"
)

func TestPodServiceResizePod(t *testing.T) {
	client := &kubernetes.Clientset{}
	var patch kubetesting.PatchActionImpl
	client.AddReactor("patch", "pods", func(action kubetesting.Action) (bool, runtime.Object, error) {
		patch = action.(kubetesting.PatchActionImpl)
		return true, &corev1.Pod{}, nil
	})
	limits := corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}}

	err := k8s.NewPodService(client, log.Dummy, metrics.Dummy).ResizePod("ns", "pod", map[string]corev1.ResourceRequirements{"redis": limits, "exporter": {}})
	require.NoError(t, err)

	assert.Equal(t, "resize", patch.GetSubresource())
	var body struct {
		Spec struct {
			Containers []corev1.Container `json:"containers"`
		} `json:"spec"`
	}
	require.NoError(t, json.Unmarshal(patch.GetPatch(), &body))
	require.Len(t, body.Spec.Containers, 2)
	assert.Equal(t, "exporter", body.Spec.Containers[0].Name)
	assert.Equal(t, "redis", body.Spec.Containers[1].Name)
	assert.Equal(t, "1Gi", body.Spec.Containers[1].Resources.Limits.Memory().String())
}

func TestPodServicePodResizeSupport(t *testing.T) {
	tests := map[string]struct {
		minor   string
		want    k8s.PodResizeSupport
		wantErr bool
	}{
		"1.32":     {minor: "32", want: k8s.PodResizeSupport{}},
		"1.33":     {minor: "33", want: k8s.PodResizeSupport{Supported: true}},
		"1.35+":    {minor: "35+", want: k8s.PodResizeSupport{Supported: true, MemoryLimitDecrease: true}},
		"unparsed": {minor: "x", wantErr: true},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			client := kubernetes.NewClientset()
			client.Discovery().(*fakediscovery.FakeDiscovery).FakedServerVersion = &version.Info{Major: "1", Minor: test.minor}
			service := k8s.NewPodService(client, log.Dummy, metrics.Dummy)

			got, err := service.PodResizeSupport()
			if test.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, test.want, got)

			// Cached: a changed server version is not seen right away.
			client.Discovery().(*fakediscovery.FakeDiscovery).FakedServerVersion = &version.Info{Major: "1", Minor: "40"}
			got, err = service.PodResizeSupport()
			require.NoError(t, err)
			assert.Equal(t, test.want, got)
		})
	}
}

func TestPodServicePodResizeSupportDiscoveryError(t *testing.T) {
	client := kubernetes.NewClientset()
	client.PrependReactor("get", "version", func(kubetesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("discovery down")
	})
	_, err := k8s.NewPodService(client, log.Dummy, metrics.Dummy).PodResizeSupport()
	assert.EqualError(t, err, "discovery down")
}

func TestStatefulSetServiceGetControllerRevision(t *testing.T) {
	revision := &appsv1.ControllerRevision{ObjectMeta: metav1.ObjectMeta{Name: "rfr-test-abc", Namespace: "ns"}, Revision: 2}
	client := kubernetes.NewClientset(revision)

	got, err := k8s.NewStatefulSetService(client, log.Dummy, metrics.Dummy).GetControllerRevision("ns", "rfr-test-abc")
	require.NoError(t, err)
	assert.Equal(t, int64(2), got.Revision)

	_, err = k8s.NewStatefulSetService(client, log.Dummy, metrics.Dummy).GetControllerRevision("ns", "missing")
	assert.Error(t, err)
}
