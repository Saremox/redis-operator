package k8s_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	kubeerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	kubernetes "k8s.io/client-go/kubernetes/fake"
	kubetesting "k8s.io/client-go/testing"

	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	mmetrics "github.com/saremox/redis-operator/mocks/metrics"
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
		require.NoError(t, err)

		got, err := mcli.CoreV1().Pods(testns).Get(context.TODO(), "testpod", metav1.GetOptions{})
		require.NoError(t, err)
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
		require.NoError(t, err)

		got, err := mcli.CoreV1().Pods(testns).Get(context.TODO(), "testpod", metav1.GetOptions{})
		require.NoError(t, err)
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
		require.NoError(t, err)

		got, err := mcli.CoreV1().Pods(testns).Get(context.TODO(), "testpod", metav1.GetOptions{})
		require.NoError(t, err)
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
		require.NoError(t, err)

		got, err := mcli.CoreV1().Pods(testns).Get(context.TODO(), "nolabelspod", metav1.GetOptions{})
		require.NoError(t, err)
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
			require.NoError(t, service.UpdatePodLabels(testns, "testpod", empty))
		}

		got, err := mcli.CoreV1().Pods(testns).Get(context.TODO(), "testpod", metav1.GetOptions{})
		require.NoError(t, err)
		assertTest.Equal(labels, got.Labels)
	})
}

// levelLogger records the level of each Debug and Error line.
type levelLogger struct {
	log.DummyLogger
	levels *[]string
}

func (l levelLogger) With(string, any) log.Logger      { return l }
func (l levelLogger) WithField(string, any) log.Logger { return l }
func (l levelLogger) Debugf(string, ...any)            { *l.levels = append(*l.levels, "debug") }
func (l levelLogger) Errorf(string, ...any)            { *l.levels = append(*l.levels, "error") }

// fieldLogger records each line with the level and the fields in front.
type fieldLogger struct {
	log.DummyLogger
	fields string
	lines  *[]string
}

func (l fieldLogger) With(key string, value any) log.Logger { return l.WithField(key, value) }

func (l fieldLogger) WithField(key string, value any) log.Logger {
	l.fields += fmt.Sprintf("%s=%v ", key, value)
	return l
}

func (l fieldLogger) addf(level, format string, args ...any) {
	*l.lines = append(*l.lines, level+": "+l.fields+fmt.Sprintf(format, args...))
}

func (l fieldLogger) Debugf(format string, args ...any) { l.addf("debug", format, args...) }
func (l fieldLogger) Infof(format string, args ...any)  { l.addf("info", format, args...) }
func (l fieldLogger) Errorf(format string, args ...any) { l.addf("error", format, args...) }

func TestPodServiceLogsTheNamespaceAndPod(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		call     func(*k8s.PodService) error
		expLines []string
	}{
		{
			name: "update the labels",
			call: func(s *k8s.PodService) error {
				return s.UpdatePodLabels("testns", "testpod", map[string]string{"role": "master"})
			},
			expLines: []string{"info: service=k8s.pod namespace=testns pod=testpod Update pod label, labels: map[role:master]"},
		},
		{
			name: "the label update fails",
			err:  errors.New("forbidden"),
			call: func(s *k8s.PodService) error {
				return s.UpdatePodLabels("testns", "testpod", map[string]string{"role": "master"})
			},
			expLines: []string{
				"info: service=k8s.pod namespace=testns pod=testpod Update pod label, labels: map[role:master]",
				"error: service=k8s.pod namespace=testns pod=testpod Update pod labels failed: forbidden",
			},
		},
		{
			name: "the pod of the label update is gone",
			err:  kubeerrors.NewNotFound(podsGroup.GroupResource(), "testpod"),
			call: func(s *k8s.PodService) error {
				return s.UpdatePodLabels("testns", "testpod", map[string]string{"role": "master"})
			},
			expLines: []string{
				"info: service=k8s.pod namespace=testns pod=testpod Update pod label, labels: map[role:master]",
				`debug: service=k8s.pod namespace=testns pod=testpod Update pod labels failed: pods "testpod" not found`,
			},
		},
		{
			name: "the annotation update fails",
			err:  errors.New("forbidden"),
			call: func(s *k8s.PodService) error {
				return s.UpdatePodAnnotations("testns", "testpod", map[string]string{"a": "b"})
			},
			expLines: []string{
				"info: service=k8s.pod namespace=testns pod=testpod Update pod annotations, annotations: map[a:b]",
				"error: service=k8s.pod namespace=testns pod=testpod Update pod annotations failed: forbidden",
			},
		},
		{
			name: "the annotation removal fails",
			err:  errors.New("forbidden"),
			call: func(s *k8s.PodService) error {
				return s.RemovePodAnnotation("testns", "testpod", "a")
			},
			expLines: []string{
				"info: service=k8s.pod namespace=testns pod=testpod Remove pod annotation, annotation: a",
				"error: service=k8s.pod namespace=testns pod=testpod Remove pod annotation failed: forbidden",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mcli := kubernetes.NewClientset(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "testpod", Namespace: "testns"}})
			if test.err != nil {
				mcli.PrependReactor("patch", "pods", func(kubetesting.Action) (bool, runtime.Object, error) {
					return true, nil, test.err
				})
			}
			var lines []string
			service := k8s.NewPodService(mcli, fieldLogger{lines: &lines}, metrics.Dummy)

			err := test.call(service)
			require.ErrorIs(t, err, test.err)
			assert.Equal(t, test.expLines, lines)
		})
	}
}

// A reconcile can read a pod just before its deletion, so a missing pod is
// not a fault.
func TestPodServiceUpdatePodLabelsLogLevel(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expLevel string
	}{
		{name: "the pod is gone", err: kubeerrors.NewNotFound(podsGroup.GroupResource(), "testpod"), expLevel: "debug"},
		{name: "the patch fails", err: errors.New("forbidden"), expLevel: "error"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mcli := kubernetes.NewClientset()
			mcli.PrependReactor("patch", "pods", func(kubetesting.Action) (bool, runtime.Object, error) {
				return true, nil, test.err
			})
			var levels []string
			service := k8s.NewPodService(mcli, levelLogger{levels: &levels}, metrics.Dummy)

			err := service.UpdatePodLabels("testns", "testpod", map[string]string{"role": "master"})
			require.Error(t, err)
			assert.Equal(t, []string{test.expLevel}, levels)
		})
	}
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
		require.NoError(t, err)
		assertTest.Equal([]kubetesting.Action{newPodDeleteAction(testns, "testpod1")}, mcli.Actions())

		_, getErr := mcli.CoreV1().Pods(testns).Get(context.TODO(), "testpod1", metav1.GetOptions{})
		require.Error(t, getErr)
		assertTest.True(kubeerrors.IsNotFound(getErr))
	})

	t.Run("returns a not found error when the pod does not exist", func(t *testing.T) {
		assertTest := assert.New(t)

		mcli := kubernetes.NewClientset()
		service := k8s.NewPodService(mcli, log.Dummy, metrics.Dummy)

		err := service.DeletePod(testns, "does-not-exist")
		require.Error(t, err)
		assertTest.True(kubeerrors.IsNotFound(err))
	})
}

// The API server refuses the delete when the UID differs, so a pod created
// again with the same name stays. The fake clientset does not check the
// precondition, so the test checks the request.
func TestPodServiceDeleteWithUID(t *testing.T) {
	testns := "testns"
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "testpod1", Namespace: testns, UID: "uid1"}}
	mcli := kubernetes.NewClientset(pod)
	service := k8s.NewPodService(mcli, log.Dummy, metrics.Dummy)

	require.NoError(t, service.DeletePodWithUID(testns, "testpod1", "uid1"))
	uid := types.UID("uid1")
	want := kubetesting.NewDeleteActionWithOptions(podsGroup, testns, "testpod1", metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
	assert.Equal(t, []kubetesting.Action{want}, mcli.Actions())

	err := service.DeletePodWithUID(testns, "testpod1", "uid1")
	assert.True(t, kubeerrors.IsNotFound(err), err)
}

func TestPodServiceGetPod(t *testing.T) {
	testns := "testns"
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "testpod", Namespace: testns, UID: "uid1"}}
	mcli := kubernetes.NewClientset(pod)
	rec := &mmetrics.Recorder{}
	rec.On("RecordK8sOperation", testns, "Pod", "testpod", "GET", metrics.SUCCESS, metrics.NOT_APPLICABLE).Once()
	rec.On("RecordK8sOperation", testns, "Pod", "missing", "GET", metrics.FAIL, metrics.K8S_NOT_FOUND).Once()
	service := k8s.NewPodService(mcli, log.Dummy, rec)

	got, err := service.GetPod(testns, "testpod")
	require.NoError(t, err)
	assert.Equal(t, pod, got)

	got, err = service.GetPod(testns, "missing")
	assert.True(t, kubeerrors.IsNotFound(err), err)
	assert.Nil(t, got)

	rec.AssertExpectations(t)
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
		require.NoError(t, err)
		assertTest.Len(list.Items, 2)
		names := []string{list.Items[0].Name, list.Items[1].Name}
		assertTest.ElementsMatch([]string{"p1", "p2"}, names)
	})

	t.Run("returns an error when the client fails", func(t *testing.T) {
		assertTest := assert.New(t)

		mcli := &kubernetes.Clientset{}
		wantErr := errors.New("wanted error")
		mcli.AddReactor("list", "pods", func(_ kubetesting.Action) (bool, runtime.Object, error) {
			return true, nil, wantErr
		})
		service := k8s.NewPodService(mcli, log.Dummy, metrics.Dummy)

		list, err := service.ListPods(testns)
		require.Error(t, err)
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

			require.NoError(t, service.UpdatePodAnnotations(testns, "testpod", map[string]string{"safe-to-evict": "false"}))

			got, err := mcli.CoreV1().Pods(testns).Get(context.TODO(), "testpod", metav1.GetOptions{})
			require.NoError(t, err)
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

	require.NoError(t, service.RemovePodAnnotation(testns, "testpod", "safe-to-evict"))

	got, err := mcli.CoreV1().Pods(testns).Get(context.TODO(), "testpod", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"other": "kept"}, got.Annotations)

	err = service.RemovePodAnnotation(testns, "missing", "safe-to-evict")
	assert.True(t, kubeerrors.IsNotFound(err))
}
