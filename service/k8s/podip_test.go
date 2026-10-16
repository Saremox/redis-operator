package k8s_test

import (
	"testing"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubernetes "k8s.io/client-go/kubernetes/fake"

	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	mmetrics "github.com/saremox/redis-operator/mocks/metrics"
	"github.com/saremox/redis-operator/service/k8s"
)

// The metrics get the IP of each listed pod, so the Redis and Sentinel
// series carry the pod name. A pod without an IP and a pod of another owner
// give no IP.
func TestPodListsSetPodIPs(t *testing.T) {
	const testns = "testns"
	selectorLabels := map[string]string{"app": "redis"}
	withIP := func(pod *corev1.Pod, ip string) *corev1.Pod {
		pod.Status.PodIP = ip
		return pod
	}
	newRecorder := func() *mmetrics.Recorder {
		rec := &mmetrics.Recorder{}
		rec.On("RecordK8sOperation", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()
		return rec
	}

	t.Run("GetStatefulSetPods", func(t *testing.T) {
		sts := &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Name: "rfr-rf", Namespace: testns, UID: "sts-uid"},
			Spec:       appsv1.StatefulSetSpec{Selector: &metav1.LabelSelector{MatchLabels: selectorLabels}},
		}
		other := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: testns, UID: "other-uid"}}
		mcli := kubernetes.NewClientset(sts,
			withIP(ownedPod(sts, "rfr-rf-0", selectorLabels), "10.0.0.1"),
			ownedPod(sts, "rfr-rf-1", selectorLabels),
			withIP(ownedPod(other, "rfr-rf-2", selectorLabels), "10.0.0.3"),
		)
		rec := newRecorder()
		rec.On("SetPodIP", metrics.KIND_REDIS, testns, "rfr-rf-0", "10.0.0.1").Once()

		_, err := k8s.NewStatefulSetService(mcli, log.Dummy, rec).GetStatefulSetPods(testns, "rfr-rf")
		require.NoError(t, err)
		rec.AssertExpectations(t)
	})

	t.Run("GetDeploymentPods", func(t *testing.T) {
		deployment := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "rfs-rf", Namespace: testns},
			Spec:       appsv1.DeploymentSpec{Selector: &metav1.LabelSelector{MatchLabels: selectorLabels}},
		}
		mcli := kubernetes.NewClientset(deployment,
			withIP(replicaSetPod("rfs-rf-abc-1", testns, "abc", "rfs-rf-abc", selectorLabels), "10.0.0.4"),
			replicaSetPod("rfs-rf-abc-2", testns, "abc", "rfs-rf-abc", selectorLabels),
			withIP(replicaSetPod("other-abc-3", testns, "abc", "other-abc", selectorLabels), "10.0.0.6"),
		)
		rec := newRecorder()
		rec.On("SetPodIP", metrics.KIND_SENTINEL, testns, "rfs-rf-abc-1", "10.0.0.4").Once()

		_, err := k8s.NewDeploymentService(mcli, log.Dummy, rec).GetDeploymentPods(testns, "rfs-rf")
		require.NoError(t, err)
		rec.AssertExpectations(t)
	})
}
