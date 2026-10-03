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
	kubernetes "k8s.io/client-go/kubernetes/fake"
	kubetesting "k8s.io/client-go/testing"

	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	"github.com/saremox/redis-operator/service/k8s"
)

var (
	podsGroup = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"}
)

func newPodDeleteAction(ns, name string) kubetesting.DeleteActionImpl {
	return kubetesting.NewDeleteAction(podsGroup, ns, name)
}

func newPodListAction(ns string) kubetesting.ListActionImpl {
	return kubetesting.NewListAction(podsGroup, schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Pod"}, ns, metav1.ListOptions{})
}

// TestPodServiceUpdatePodLabels covers UpdatePodLabels, which sets the role
// label during a failover.
func TestPodServiceUpdatePodLabels(t *testing.T) {
	testns := "testns"

	t.Run("updates an existing label to a new value", func(t *testing.T) {
		assertTest := assert.New(t)

		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "testpod",
				Namespace: testns,
				Labels: map[string]string{
					"role": "slave",
				},
			},
		}
		mcli := kubernetes.NewClientset(pod)
		service := k8s.NewPodService(mcli, log.Dummy, metrics.Dummy)

		err := service.UpdatePodLabels(testns, "testpod", map[string]string{"role": "master"})
		assertTest.NoError(err)

		got, err := mcli.CoreV1().Pods(testns).Get(context.TODO(), "testpod", metav1.GetOptions{})
		assertTest.NoError(err)
		assertTest.Equal("master", got.Labels["role"])
	})

	t.Run("updates multiple existing labels at once", func(t *testing.T) {
		assertTest := assert.New(t)

		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "testpod",
				Namespace: testns,
				Labels: map[string]string{
					"role":  "slave",
					"ready": "false",
				},
			},
		}
		mcli := kubernetes.NewClientset(pod)
		service := k8s.NewPodService(mcli, log.Dummy, metrics.Dummy)

		err := service.UpdatePodLabels(testns, "testpod", map[string]string{
			"role":  "master",
			"ready": "true",
		})
		assertTest.NoError(err)

		got, err := mcli.CoreV1().Pods(testns).Get(context.TODO(), "testpod", metav1.GetOptions{})
		assertTest.NoError(err)
		assertTest.Equal("master", got.Labels["role"])
		assertTest.Equal("true", got.Labels["ready"])
	})

	t.Run("returns an error when the pod does not exist", func(t *testing.T) {
		assertTest := assert.New(t)

		mcli := kubernetes.NewClientset()
		service := k8s.NewPodService(mcli, log.Dummy, metrics.Dummy)

		err := service.UpdatePodLabels(testns, "does-not-exist", map[string]string{"role": "master"})
		assertTest.Error(err)
	})

	t.Run("sets a label key that contains a slash and keeps the other labels", func(t *testing.T) {
		assertTest := assert.New(t)

		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "testpod",
				Namespace: testns,
				Labels: map[string]string{
					"role":                        "slave",
					"app.kubernetes.io/component": "redis",
				},
			},
		}
		mcli := kubernetes.NewClientset(pod)
		service := k8s.NewPodService(mcli, log.Dummy, metrics.Dummy)

		err := service.UpdatePodLabels(testns, "testpod", map[string]string{"app.kubernetes.io/component": "sentinel"})
		assertTest.NoError(err)

		got, err := mcli.CoreV1().Pods(testns).Get(context.TODO(), "testpod", metav1.GetOptions{})
		assertTest.NoError(err)
		assertTest.Equal("sentinel", got.Labels["app.kubernetes.io/component"])
		assertTest.Equal("slave", got.Labels["role"])
	})

	t.Run("creates the labels on a pod without any", func(t *testing.T) {
		assertTest := assert.New(t)

		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "nolabelspod",
				Namespace: testns,
			},
		}
		mcli := kubernetes.NewClientset(pod)
		service := k8s.NewPodService(mcli, log.Dummy, metrics.Dummy)

		err := service.UpdatePodLabels(testns, "nolabelspod", map[string]string{"role": "master"})
		assertTest.NoError(err)

		got, err := mcli.CoreV1().Pods(testns).Get(context.TODO(), "nolabelspod", metav1.GetOptions{})
		assertTest.NoError(err)
		assertTest.Equal(map[string]string{"role": "master"}, got.Labels)
	})

	t.Run("keeps the labels when no labels are given", func(t *testing.T) {
		assertTest := assert.New(t)

		labels := map[string]string{"role": "slave", "app.kubernetes.io/component": "redis"}
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "testpod",
				Namespace: testns,
				Labels:    labels,
			},
		}
		mcli := kubernetes.NewClientset(pod)
		service := k8s.NewPodService(mcli, log.Dummy, metrics.Dummy)

		for _, empty := range []map[string]string{nil, {}} {
			assertTest.NoError(service.UpdatePodLabels(testns, "testpod", empty))
		}

		got, err := mcli.CoreV1().Pods(testns).Get(context.TODO(), "testpod", metav1.GetOptions{})
		assertTest.NoError(err)
		assertTest.Equal(labels, got.Labels)
	})
}

func TestPodServiceDelete(t *testing.T) {
	testns := "testns"

	t.Run("deletes an existing pod", func(t *testing.T) {
		assertTest := assert.New(t)

		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "testpod1",
				Namespace: testns,
			},
		}
		mcli := kubernetes.NewClientset(pod)
		service := k8s.NewPodService(mcli, log.Dummy, metrics.Dummy)

		err := service.DeletePod(testns, "testpod1")
		assertTest.NoError(err)
		assertTest.Equal([]kubetesting.Action{newPodDeleteAction(testns, "testpod1")}, mcli.Actions())

		_, getErr := mcli.CoreV1().Pods(testns).Get(context.TODO(), "testpod1", metav1.GetOptions{})
		assertTest.Error(getErr)
		assertTest.True(kubeerrors.IsNotFound(getErr))
	})

	t.Run("returns a not found error when the pod does not exist", func(t *testing.T) {
		assertTest := assert.New(t)

		mcli := kubernetes.NewClientset()
		service := k8s.NewPodService(mcli, log.Dummy, metrics.Dummy)

		err := service.DeletePod(testns, "does-not-exist")
		assertTest.Error(err)
		assertTest.True(kubeerrors.IsNotFound(err))
	})
}

func TestPodServiceList(t *testing.T) {
	testns := "testns"

	t.Run("lists pods in a namespace", func(t *testing.T) {
		assertTest := assert.New(t)

		p1 := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: testns}}
		p2 := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p2", Namespace: testns}}
		p3 := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p3", Namespace: "otherns"}}
		mcli := kubernetes.NewClientset(p1, p2, p3)
		service := k8s.NewPodService(mcli, log.Dummy, metrics.Dummy)

		list, err := service.ListPods(testns)
		assertTest.NoError(err)
		assertTest.Len(list.Items, 2)
		names := []string{list.Items[0].Name, list.Items[1].Name}
		assertTest.ElementsMatch([]string{"p1", "p2"}, names)
	})

	t.Run("returns an error when the client fails", func(t *testing.T) {
		assertTest := assert.New(t)

		mcli := &kubernetes.Clientset{}
		wantErr := errors.New("wanted error")
		mcli.AddReactor("list", "pods", func(action kubetesting.Action) (bool, runtime.Object, error) {
			return true, nil, wantErr
		})
		service := k8s.NewPodService(mcli, log.Dummy, metrics.Dummy)

		list, err := service.ListPods(testns)
		assertTest.Error(err)
		assertTest.Empty(list.Items)
		assertTest.Equal([]kubetesting.Action{newPodListAction(testns)}, mcli.Actions())
	})
}

func TestPodServiceUpdatePodAnnotations(t *testing.T) {
	testns := "testns"
	tests := []struct {
		name     string
		existing map[string]string
		expected map[string]string
	}{
		{
			name:     "creates the annotations on a pod without any",
			expected: map[string]string{"safe-to-evict": "false"},
		},
		{
			name:     "keeps unrelated annotations and replaces the given key",
			existing: map[string]string{"other": "kept", "safe-to-evict": "true"},
			expected: map[string]string{"other": "kept", "safe-to-evict": "false"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "testpod", Namespace: testns, Annotations: test.existing}}
			mcli := kubernetes.NewClientset(pod)
			service := k8s.NewPodService(mcli, log.Dummy, metrics.Dummy)

			assert.NoError(t, service.UpdatePodAnnotations(testns, "testpod", map[string]string{"safe-to-evict": "false"}))

			got, err := mcli.CoreV1().Pods(testns).Get(context.TODO(), "testpod", metav1.GetOptions{})
			assert.NoError(t, err)
			assert.Equal(t, test.expected, got.Annotations)
		})
	}

	t.Run("returns not found for a missing pod", func(t *testing.T) {
		service := k8s.NewPodService(kubernetes.NewClientset(), log.Dummy, metrics.Dummy)
		err := service.UpdatePodAnnotations(testns, "missing", map[string]string{"safe-to-evict": "false"})
		assert.True(t, kubeerrors.IsNotFound(err))
	})
}

func TestPodServiceRemovePodAnnotation(t *testing.T) {
	testns := "testns"
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "testpod", Namespace: testns, Annotations: map[string]string{"other": "kept", "safe-to-evict": "false"}}}
	mcli := kubernetes.NewClientset(pod)
	service := k8s.NewPodService(mcli, log.Dummy, metrics.Dummy)

	assert.NoError(t, service.RemovePodAnnotation(testns, "testpod", "safe-to-evict"))

	got, err := mcli.CoreV1().Pods(testns).Get(context.TODO(), "testpod", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.Equal(t, map[string]string{"other": "kept"}, got.Annotations)

	err = service.RemovePodAnnotation(testns, "missing", "safe-to-evict")
	assert.True(t, kubeerrors.IsNotFound(err))
}
