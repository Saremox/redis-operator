package redisfailover

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spotahome/kooper/v2/controller"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	clientfeatures "k8s.io/client-go/features"
	clientfeaturestesting "k8s.io/client-go/features/testing"
	fakekubernetes "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
)

type staticRFRetriever struct {
	items []redisfailoverv1.RedisFailover
}

func (r staticRFRetriever) List(context.Context, metav1.ListOptions) (runtime.Object, error) {
	return &redisfailoverv1.RedisFailoverList{Items: r.items}, nil
}

func (r staticRFRetriever) Watch(context.Context, metav1.ListOptions) (watch.Interface, error) {
	return watch.NewFake(), nil
}

type recordingHandler struct {
	mu      sync.Mutex
	calls   []string
	running atomic.Int32
	overlap atomic.Bool
	delay   time.Duration
}

func (h *recordingHandler) Handle(_ context.Context, obj runtime.Object) error {
	if h.running.Add(1) > 1 {
		h.overlap.Store(true)
	}
	defer h.running.Add(-1)
	time.Sleep(h.delay)
	rf := obj.(*redisfailoverv1.RedisFailover)
	h.mu.Lock()
	h.calls = append(h.calls, rf.Namespace+"/"+rf.Name)
	h.mu.Unlock()
	return nil
}

func (h *recordingHandler) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.calls)
}

func rfPod(name, rf string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:      name,
		Namespace: "ns",
		Labels:    map[string]string{rfLabelNameKey: rf},
	}}
}

func startController(t *testing.T, h controller.Handler, kube *fakekubernetes.Clientset, rfs ...redisfailoverv1.RedisFailover) *rfController {
	t.Helper()
	return startControllerWith(t, h, kube, time.Hour, metrics.Dummy, rfs...)
}

// startControllerWith returns once the pod watch is registered, so pods
// created afterwards produce events.
func startControllerWith(t *testing.T, h controller.Handler, kube *fakekubernetes.Clientset, resync time.Duration, mrec metrics.Recorder, rfs ...redisfailoverv1.RedisFailover) *rfController {
	t.Helper()
	clientfeaturestesting.SetFeatureDuringTest(t, clientfeatures.WatchListClient, false)
	watching := make(chan struct{})
	var once sync.Once
	kube.PrependWatchReactor("pods", func(action k8stesting.Action) (bool, watch.Interface, error) {
		w, err := kube.Tracker().Watch(action.GetResource(), action.GetNamespace())
		once.Do(func() { close(watching) })
		return true, w, err
	})
	c, err := newRFController(h, staticRFRetriever{items: rfs}, newPodListWatch(kube), resync, 3, nil, mrec, log.Dummy)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- c.Run(ctx) }()
	<-watching
	t.Cleanup(func() {
		cancel()
		require.NoError(t, <-done)
	})
	return c
}

func TestRFControllerPodEventsReconcileTheirRedisFailover(t *testing.T) {
	rf := redisfailoverv1.RedisFailover{ObjectMeta: metav1.ObjectMeta{Name: "rf", Namespace: "ns"}}
	kube := fakekubernetes.NewClientset()
	h := &recordingHandler{}
	startController(t, h, kube, rf)

	require.Eventually(t, func() bool { return h.count() == 1 }, 5*time.Second, 10*time.Millisecond)

	_, err := kube.CoreV1().Pods("ns").Create(context.Background(), rfPod("rfr-rf-0", "rf"), metav1.CreateOptions{})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return h.count() == 2 }, 5*time.Second, 10*time.Millisecond)

	ready := rfPod("rfr-rf-0", "rf")
	ready.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	_, err = kube.CoreV1().Pods("ns").UpdateStatus(context.Background(), ready, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return h.count() == 3 }, 5*time.Second, 10*time.Millisecond)

	_, err = kube.CoreV1().Pods("ns").Create(context.Background(), rfPod("rfr-other-0", "other"), metav1.CreateOptions{})
	require.NoError(t, err)
	require.NoError(t, kube.CoreV1().Pods("ns").Delete(context.Background(), "rfr-rf-0", metav1.DeleteOptions{}))
	require.Eventually(t, func() bool { return h.count() == 4 }, 5*time.Second, 10*time.Millisecond)
	time.Sleep(200 * time.Millisecond)

	h.mu.Lock()
	defer h.mu.Unlock()
	assert.Equal(t, []string{"ns/rf", "ns/rf", "ns/rf", "ns/rf"}, h.calls)
}

func TestRFControllerNeverReconcilesARedisFailoverConcurrently(t *testing.T) {
	rf := redisfailoverv1.RedisFailover{ObjectMeta: metav1.ObjectMeta{Name: "rf", Namespace: "ns"}}
	kube := fakekubernetes.NewClientset()
	h := &recordingHandler{delay: 50 * time.Millisecond}
	startController(t, h, kube, rf)
	require.Eventually(t, func() bool { return h.count() == 1 }, 5*time.Second, 10*time.Millisecond)

	for _, name := range []string{"rfr-rf-0", "rfr-rf-1", "rfr-rf-2", "rfs-rf-a", "rfs-rf-b"} {
		_, err := kube.CoreV1().Pods("ns").Create(context.Background(), rfPod(name, "rf"), metav1.CreateOptions{})
		require.NoError(t, err)
	}
	require.Eventually(t, func() bool { return h.count() >= 2 && h.running.Load() == 0 }, 5*time.Second, 10*time.Millisecond)
	time.Sleep(200 * time.Millisecond)

	assert.False(t, h.overlap.Load())
	assert.Less(t, h.count(), 7, "queued events for the same RedisFailover coalesce")
}

func TestPodOwnerKey(t *testing.T) {
	key, ok := podOwnerKey(rfPod("rfr-rf-0", "rf"))
	assert.True(t, ok)
	assert.Equal(t, "ns/rf", key)

	key, ok = podOwnerKey(cache.DeletedFinalStateUnknown{Key: "ns/rfr-rf-0", Obj: rfPod("rfr-rf-0", "rf")})
	assert.True(t, ok)
	assert.Equal(t, "ns/rf", key)

	_, ok = podOwnerKey(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "unlabelled", Namespace: "ns"}})
	assert.False(t, ok)

	_, ok = podOwnerKey("not an object")
	assert.False(t, ok)
}

func TestPodMetadataOnly(t *testing.T) {
	pod := rfPod("rfr-rf-0", "rf")
	pod.Spec.Containers = []corev1.Container{{Name: "redis"}}
	pod.Status.PodIP = "10.0.0.1"

	obj, err := podMetadataOnly(pod)
	require.NoError(t, err)
	assert.Equal(t, &corev1.Pod{ObjectMeta: pod.ObjectMeta}, obj)

	obj, err = podMetadataOnly("passthrough")
	require.NoError(t, err)
	assert.Equal(t, "passthrough", obj)
}

type failingQueueMetrics struct{ metrics.Recorder }

func (failingQueueMetrics) RegisterResourceQueueLengthFunc(string, func(context.Context) int) error {
	return errors.New("already registered")
}

func TestNewRFControllerReturnsMetricsRegistrationError(t *testing.T) {
	_, err := newRFController(&recordingHandler{}, staticRFRetriever{}, newPodListWatch(fakekubernetes.NewClientset()), 0, 0, nil, failingQueueMetrics{metrics.Dummy}, log.Dummy)
	assert.EqualError(t, err, "already registered")
}

type queueLengthMetrics struct {
	metrics.Recorder
	queueLen func(context.Context) int
}

func (m *queueLengthMetrics) RegisterResourceQueueLengthFunc(_ string, f func(context.Context) int) error {
	m.queueLen = f
	return nil
}

type recordingRunner struct{ ran bool }

func (r *recordingRunner) Run(f func() error) error {
	r.ran = true
	return f()
}

func TestRFControllerRunsUnderLeaderElection(t *testing.T) {
	runner := &recordingRunner{}
	mrec := &queueLengthMetrics{Recorder: metrics.Dummy}
	c, err := newRFController(&recordingHandler{}, staticRFRetriever{}, newPodListWatch(fakekubernetes.NewClientset()), time.Hour, 1, runner, mrec, log.Dummy)
	require.NoError(t, err)
	c.enqueue("ns/rf")
	assert.Equal(t, 1, mrec.queueLen(context.Background()))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	assert.EqualError(t, c.Run(ctx), "timed out waiting for caches to sync")
	assert.True(t, runner.ran)
}

type failingHandler struct{ calls atomic.Int32 }

func (h *failingHandler) Handle(context.Context, runtime.Object) error {
	h.calls.Add(1)
	return errors.New("boom")
}

func TestRFControllerKeepsWorkingAfterHandleErrors(t *testing.T) {
	rf := redisfailoverv1.RedisFailover{ObjectMeta: metav1.ObjectMeta{Name: "rf", Namespace: "ns"}}
	kube := fakekubernetes.NewClientset()
	h := &failingHandler{}
	startController(t, h, kube, rf)
	require.Eventually(t, func() bool { return h.calls.Load() == 1 }, 5*time.Second, 10*time.Millisecond)

	_, err := kube.CoreV1().Pods("ns").Create(context.Background(), rfPod("rfr-rf-0", "rf"), metav1.CreateOptions{})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return h.calls.Load() == 2 }, 5*time.Second, 10*time.Millisecond)
}

func (h *recordingHandler) countFor(key string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, c := range h.calls {
		if c == key {
			n++
		}
	}
	return n
}

func TestRFControllerReconcilesBothOwnersWhenAPodChangesOwner(t *testing.T) {
	a := redisfailoverv1.RedisFailover{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "ns"}}
	b := redisfailoverv1.RedisFailover{ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "ns"}}
	kube := fakekubernetes.NewClientset()
	h := &recordingHandler{}
	startController(t, h, kube, a, b)
	require.Eventually(t, func() bool { return h.count() == 2 }, 5*time.Second, 10*time.Millisecond)

	pod, err := kube.CoreV1().Pods("ns").Create(context.Background(), rfPod("rfr-a-0", "a"), metav1.CreateOptions{})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return h.countFor("ns/a") == 2 }, 5*time.Second, 10*time.Millisecond)

	pod.Labels[rfLabelNameKey] = "b"
	_, err = kube.CoreV1().Pods("ns").Update(context.Background(), pod, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return h.countFor("ns/a") == 3 && h.countFor("ns/b") == 2 }, 5*time.Second, 10*time.Millisecond)
}

func TestRFControllerReconcilesWithoutPodAccess(t *testing.T) {
	clientfeaturestesting.SetFeatureDuringTest(t, clientfeatures.WatchListClient, false)
	rf := redisfailoverv1.RedisFailover{ObjectMeta: metav1.ObjectMeta{Name: "rf", Namespace: "ns"}}
	kube := fakekubernetes.NewClientset()
	kube.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(corev1.Resource("pods"), "", errors.New("denied"))
	})
	h := &recordingHandler{}
	c, err := newRFController(h, staticRFRetriever{items: []redisfailoverv1.RedisFailover{rf}}, newPodListWatch(kube), time.Hour, 1, nil, metrics.Dummy, log.Dummy)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- c.Run(ctx) }()

	require.Eventually(t, func() bool { return h.count() == 1 }, 5*time.Second, 10*time.Millisecond)
	cancel()
	require.NoError(t, <-done)
}

func TestRFControllerPodOwnerIgnoresUnhandledRedisFailovers(t *testing.T) {
	c, err := newRFController(&recordingHandler{}, staticRFRetriever{}, newPodListWatch(fakekubernetes.NewClientset()), time.Hour, 1, nil, metrics.Dummy, log.Dummy)
	require.NoError(t, err)
	require.NoError(t, c.rfInformer.GetIndexer().Add(&redisfailoverv1.RedisFailover{ObjectMeta: metav1.ObjectMeta{Name: "rf", Namespace: "ns"}}))

	key, ok := c.podOwner(rfPod("rfr-rf-0", "rf"))
	assert.True(t, ok)
	assert.Equal(t, "ns/rf", key)

	_, ok = c.podOwner(rfPod("rfr-other-0", "other"))
	assert.False(t, ok)

	_, ok = c.podOwner(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "unlabelled", Namespace: "ns"}})
	assert.False(t, ok)
}

func TestRFControllerSkipsDeletedRedisFailovers(t *testing.T) {
	h := &recordingHandler{}
	c, err := newRFController(h, staticRFRetriever{}, newPodListWatch(fakekubernetes.NewClientset()), time.Hour, 1, nil, metrics.Dummy, log.Dummy)
	require.NoError(t, err)

	assert.NoError(t, c.process(context.Background(), "ns/gone"))
	assert.Zero(t, h.count())
}

func TestRFControllerResyncsRedisFailovers(t *testing.T) {
	rf := redisfailoverv1.RedisFailover{ObjectMeta: metav1.ObjectMeta{Name: "rf", Namespace: "ns"}}
	h := &recordingHandler{}
	startControllerWith(t, h, fakekubernetes.NewClientset(), 50*time.Millisecond, metrics.Dummy, rf)

	require.Eventually(t, func() bool { return h.count() >= 2 }, 5*time.Second, 10*time.Millisecond)
}

func TestNewRFControllerDefaultsWorkers(t *testing.T) {
	c, err := newRFController(&recordingHandler{}, staticRFRetriever{}, newPodListWatch(fakekubernetes.NewClientset()), 0, 0, nil, metrics.Dummy, log.Dummy)
	require.NoError(t, err)
	assert.Equal(t, 3, c.workers)
}

func TestPodListWatchSelectsRedisFailoverPods(t *testing.T) {
	kube := fakekubernetes.NewClientset()
	lw := newPodListWatch(kube)
	_, err := lw.ListWithContext(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	w, err := lw.WatchWithContext(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	w.Stop()

	actions := kube.Actions()
	require.Len(t, actions, 2)
	assert.Equal(t, rfLabelNameKey, actions[0].(k8stesting.ListActionImpl).GetListRestrictions().Labels.String())
	assert.Equal(t, rfLabelNameKey, actions[1].(k8stesting.WatchActionImpl).GetWatchRestrictions().Labels.String())
}

func TestRFControllerCachesPodMetadataOnly(t *testing.T) {
	rf := redisfailoverv1.RedisFailover{ObjectMeta: metav1.ObjectMeta{Name: "rf", Namespace: "ns"}}
	kube := fakekubernetes.NewClientset()
	h := &recordingHandler{}
	c := startController(t, h, kube, rf)
	require.Eventually(t, func() bool { return h.count() == 1 }, 5*time.Second, 10*time.Millisecond)

	pod := rfPod("rfr-rf-0", "rf")
	pod.Spec.Containers = []corev1.Container{{Name: "redis"}}
	_, err := kube.CoreV1().Pods("ns").Create(context.Background(), pod, metav1.CreateOptions{})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return len(c.podInformer.GetStore().List()) == 1 }, 5*time.Second, 10*time.Millisecond)

	assert.Empty(t, c.podInformer.GetStore().List()[0].(*corev1.Pod).Spec.Containers)
}

type recordingMetrics struct {
	metrics.Recorder
	mu        sync.Mutex
	queued    int
	inQueue   int
	processed []bool
}

func (m *recordingMetrics) IncResourceEventQueued(context.Context, string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.queued++
}

func (m *recordingMetrics) ObserveResourceInQueueDuration(context.Context, string, time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.inQueue++
}

func (m *recordingMetrics) ObserveResourceProcessingDuration(_ context.Context, _ string, success bool, _ time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.processed = append(m.processed, success)
}

func (m *recordingMetrics) snapshot() (int, int, []bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.queued, m.inQueue, append([]bool(nil), m.processed...)
}

func TestRFControllerRecordsMetrics(t *testing.T) {
	rf := redisfailoverv1.RedisFailover{ObjectMeta: metav1.ObjectMeta{Name: "rf", Namespace: "ns"}}
	kube := fakekubernetes.NewClientset()
	h := &failingHandler{}
	mrec := &recordingMetrics{Recorder: metrics.Dummy}
	startControllerWith(t, h, kube, time.Hour, mrec, rf)
	require.Eventually(t, func() bool { _, _, p := mrec.snapshot(); return len(p) == 1 }, 5*time.Second, 10*time.Millisecond)

	queued, inQueue, processed := mrec.snapshot()
	assert.Equal(t, 1, queued)
	assert.Equal(t, 1, inQueue)
	assert.Equal(t, []bool{false}, processed)
}

func TestRFControllerCountsOneEventPerUpdate(t *testing.T) {
	mrec := &recordingMetrics{Recorder: metrics.Dummy}
	c, err := newRFController(&recordingHandler{}, staticRFRetriever{}, newPodListWatch(fakekubernetes.NewClientset()), time.Hour, 1, nil, mrec, log.Dummy)
	require.NoError(t, err)
	a := &redisfailoverv1.RedisFailover{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "ns"}}
	b := &redisfailoverv1.RedisFailover{ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "ns"}}
	require.NoError(t, c.rfInformer.GetIndexer().Add(a))
	require.NoError(t, c.rfInformer.GetIndexer().Add(b))

	c.eventHandler(rfKey).OnUpdate(a, a)
	queued, _, _ := mrec.snapshot()
	assert.Equal(t, 1, queued)

	c.eventHandler(c.podOwner).OnUpdate(rfPod("rfr-a-0", "a"), rfPod("rfr-a-0", "b"))
	queued, _, _ = mrec.snapshot()
	assert.Equal(t, 3, queued)
	assert.Equal(t, 2, c.queue.Len())
}
