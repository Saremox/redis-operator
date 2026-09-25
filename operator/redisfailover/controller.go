package redisfailover

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/spotahome/kooper/v2/controller"
	"github.com/spotahome/kooper/v2/controller/leaderelection"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	"github.com/saremox/redis-operator/log"
)

const controllerName = "redisfailover"

// rfController reconciles RedisFailovers from one queue keyed by RedisFailover.
// Events on a RedisFailover's pods queue that RedisFailover, so a pod change
// (deleted, recreated, ready) drives the next reconcile without waiting for
// the resync.
type rfController struct {
	handler     controller.Handler
	rfInformer  cache.SharedIndexInformer
	podInformer cache.SharedIndexInformer
	queue       workqueue.TypedInterface[string]
	workers     int
	leRunner    leaderelection.Runner
	metrics     controller.MetricsRecorder
	logger      log.Logger

	// queuedAt feeds the in-queue duration metric.
	mu       sync.Mutex
	queuedAt map[string]time.Time
}

func newRFController(handler controller.Handler, rfRetriever controller.Retriever, podLW cache.ListerWatcher, resync time.Duration, workers int, leRunner leaderelection.Runner, metrics controller.MetricsRecorder, logger log.Logger) (*rfController, error) {
	if resync <= 0 {
		resync = 3 * time.Minute
	}
	if workers <= 0 {
		workers = 3
	}
	c := &rfController{
		handler:     handler,
		rfInformer:  cache.NewSharedIndexInformer(listerWatcher(rfRetriever), nil, resync, cache.Indexers{}),
		podInformer: cache.NewSharedIndexInformer(podLW, &corev1.Pod{}, 0, cache.Indexers{}),
		queue:       workqueue.NewTyped[string](),
		workers:     workers,
		leRunner:    leRunner,
		metrics:     metrics,
		logger:      logger,
		queuedAt:    map[string]time.Time{},
	}

	// Only the metrics registration can fail here; the informers are new.
	_, rfErr := c.rfInformer.AddEventHandlerWithResyncPeriod(c.eventHandler(rfKey), resync)
	_, podErr := c.podInformer.AddEventHandler(c.eventHandler(c.podOwner))
	queueLen := func(context.Context) int { return c.queue.Len() }
	if err := errors.Join(
		c.podInformer.SetTransform(podMetadataOnly),
		rfErr,
		podErr,
		metrics.RegisterResourceQueueLengthFunc(controllerName, queueLen),
	); err != nil {
		return nil, err
	}
	return c, nil
}

func listerWatcher(r controller.Retriever) cache.ListerWatcher {
	return &cache.ListWatch{
		ListWithContextFunc:  r.List,
		WatchFuncWithContext: r.Watch,
	}
}

// podMetadataOnly keeps only what podOwnerKey needs in the pod cache.
func podMetadataOnly(obj any) (any, error) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return obj, nil
	}
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:            pod.Name,
		Namespace:       pod.Namespace,
		UID:             pod.UID,
		ResourceVersion: pod.ResourceVersion,
		Labels:          pod.Labels,
	}}, nil
}

func rfKey(obj any) (string, bool) {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	return key, err == nil
}

func podOwnerKey(obj any) (string, bool) {
	if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = tombstone.Obj
	}
	m, err := meta.Accessor(obj)
	if err != nil {
		return "", false
	}
	name := m.GetLabels()[rfLabelNameKey]
	if name == "" {
		return "", false
	}
	return m.GetNamespace() + "/" + name, true
}

// podOwner returns the key of the pod's RedisFailover, if this operator
// handles that RedisFailover.
func (c *rfController) podOwner(obj any) (string, bool) {
	key, ok := podOwnerKey(obj)
	if !ok {
		return "", false
	}
	_, exists, _ := c.rfInformer.GetIndexer().GetByKey(key)
	return key, exists
}

func (c *rfController) eventHandler(keyOf func(any) (string, bool)) cache.ResourceEventHandler {
	enqueue := func(obj any) {
		if key, ok := keyOf(obj); ok {
			c.enqueue(key)
		}
	}
	return cache.ResourceEventHandlerFuncs{
		AddFunc: enqueue,
		UpdateFunc: func(old, obj any) {
			// A pod whose owner label changed concerns both owners.
			oldKey, oldOK := keyOf(old)
			key, ok := keyOf(obj)
			if oldOK && (!ok || oldKey != key) {
				c.enqueue(oldKey)
			}
			if ok {
				c.enqueue(key)
			}
		},
		DeleteFunc: enqueue,
	}
}

func (c *rfController) enqueue(key string) {
	c.mu.Lock()
	if _, ok := c.queuedAt[key]; !ok {
		c.queuedAt[key] = time.Now()
	}
	c.mu.Unlock()
	c.metrics.IncResourceEventQueued(context.Background(), controllerName, false)
	c.queue.Add(key)
}

// Run satisfies controller.Controller.
func (c *rfController) Run(ctx context.Context) error {
	if c.leRunner == nil {
		return c.run(ctx)
	}
	return c.leRunner.Run(func() error { return c.run(ctx) })
}

func (c *rfController) run(ctx context.Context) error {
	defer c.queue.ShutDown()

	c.logger.Infof("starting controller")
	go c.rfInformer.RunWithContext(ctx)
	go c.podInformer.RunWithContext(ctx)
	// Pod events only speed reconciles up, so a pod watch that can't sync
	// (e.g. RBAC) must not block reconciling.
	if !cache.WaitForNamedCacheSyncWithContext(ctx, c.rfInformer.HasSynced) {
		return fmt.Errorf("timed out waiting for caches to sync")
	}

	for range c.workers {
		go wait.UntilWithContext(ctx, func(ctx context.Context) {
			for c.processNext(ctx) {
			}
		}, time.Second)
	}
	<-ctx.Done()
	c.logger.Infof("stopping controller")
	return nil
}

func (c *rfController) processNext(ctx context.Context) bool {
	key, shutdown := c.queue.Get()
	if shutdown {
		return false
	}
	defer c.queue.Done(key)

	c.mu.Lock()
	if queuedAt, ok := c.queuedAt[key]; ok {
		c.metrics.ObserveResourceInQueueDuration(ctx, controllerName, queuedAt)
		delete(c.queuedAt, key)
	}
	c.mu.Unlock()

	start := time.Now()
	err := c.process(ctx, key)
	c.metrics.ObserveResourceProcessingDuration(ctx, controllerName, err == nil, start)
	if err != nil {
		c.logger.WithField("object-key", key).Errorf("error on object processing: %v", err)
	}
	return true
}

func (c *rfController) process(ctx context.Context, key string) error {
	obj, exists, err := c.rfInformer.GetIndexer().GetByKey(key)
	if err != nil || !exists {
		return err
	}
	return c.handler.Handle(ctx, obj.(runtime.Object))
}

// newPodListWatch lists and watches the pods of every RedisFailover.
func newPodListWatch(k8sClient kubernetes.Interface) *cache.ListWatch {
	return &cache.ListWatch{
		ListWithContextFunc: func(ctx context.Context, options metav1.ListOptions) (runtime.Object, error) {
			options.LabelSelector = rfLabelNameKey
			return k8sClient.CoreV1().Pods("").List(ctx, options)
		},
		WatchFuncWithContext: func(ctx context.Context, options metav1.ListOptions) (watch.Interface, error) {
			options.LabelSelector = rfLabelNameKey
			return k8sClient.CoreV1().Pods("").Watch(ctx, options)
		},
	}
}
