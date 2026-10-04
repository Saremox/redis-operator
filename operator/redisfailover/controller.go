package redisfailover

import (
	"context"
	"errors"
	"runtime/debug"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
)

const controllerName = "redisfailover"

// Controller runs the operator until its context is done.
type Controller interface {
	Run(ctx context.Context) error
}

// Handler reconciles one object.
type Handler interface {
	Handle(ctx context.Context, obj runtime.Object) error
}

// rfController reconciles RedisFailovers from one queue keyed by RedisFailover.
// Events on a RedisFailover's pods queue that RedisFailover, so a pod change
// (deleted, recreated, ready) drives the next reconcile without waiting for
// the resync. With a Secret watch, an auth Secret event also queues its
// RedisFailovers.
type rfController struct {
	handler        Handler
	rfInformer     cache.SharedIndexInformer
	podInformer    cache.SharedIndexInformer
	secretInformer cache.SharedIndexInformer // nil without a Secret watch
	queue          workqueue.TypedInterface[string]
	workers        int
	leRunner       leaderRunner
	metrics        metrics.ControllerRecorder
	logger         log.Logger

	// queuedAt feeds the in-queue duration metric.
	mu       sync.Mutex
	queuedAt map[string]time.Time
}

func newRFController(handler Handler, rfLW, podLW, secretLW cache.ListerWatcher, resync time.Duration, workers int, leRunner leaderRunner, mrec metrics.ControllerRecorder, logger log.Logger) (*rfController, error) {
	if resync <= 0 {
		resync = 3 * time.Minute
	}
	if workers <= 0 {
		workers = 3
	}
	c := &rfController{
		handler:     handler,
		rfInformer:  cache.NewSharedIndexInformer(rfLW, nil, resync, cache.Indexers{authSecretIndex: authSecretKey}),
		podInformer: cache.NewSharedIndexInformer(podLW, &corev1.Pod{}, 0, cache.Indexers{}),
		queue:       workqueue.NewTyped[string](),
		workers:     workers,
		leRunner:    leRunner,
		metrics:     mrec,
		logger:      logger,
		queuedAt:    map[string]time.Time{},
	}

	// Only the metrics registration can fail here; the informers are new.
	_, rfErr := c.rfInformer.AddEventHandlerWithResyncPeriod(c.eventHandler(rfKey), resync)
	_, podErr := c.podInformer.AddEventHandler(c.eventHandler(c.podOwner))
	var secretErr error
	if secretLW != nil {
		c.secretInformer = cache.NewSharedIndexInformer(secretLW, &metav1.PartialObjectMetadata{}, 0, cache.Indexers{})
		_, err := c.secretInformer.AddEventHandler(c.secretEventHandler())
		secretErr = errors.Join(c.secretInformer.SetTransform(secretKeyOnly), err)
	}
	queueLen := func(context.Context) int { return c.queue.Len() }
	if err := errors.Join(
		c.podInformer.SetTransform(podMetadataOnly),
		rfErr,
		podErr,
		secretErr,
		mrec.RegisterResourceQueueLengthFunc(controllerName, queueLen),
	); err != nil {
		return nil, err
	}
	return c, nil
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

// secretKeyOnly keeps only the key and the resource version, because the
// cache holds every Secret in the cluster.
func secretKeyOnly(obj any) (any, error) {
	secret, ok := obj.(*metav1.PartialObjectMetadata)
	if !ok {
		return obj, nil
	}
	return &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{
		Name:            secret.Name,
		Namespace:       secret.Namespace,
		UID:             secret.UID,
		ResourceVersion: secret.ResourceVersion,
	}}, nil
}

// authSecretIndex lets a Secret event find its RedisFailovers without a scan.
const authSecretIndex = "authSecret"

func authSecretKey(obj any) ([]string, error) {
	rf, ok := obj.(*redisfailoverv1.RedisFailover)
	if !ok || rf.Spec.Auth.SecretPath == "" {
		return nil, nil
	}
	return []string{rf.Namespace + "/" + rf.Spec.Auth.SecretPath}, nil
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

// secretEventHandler queues each RedisFailover that uses the changed Secret.
// The RedisFailover informer holds only supported namespaces, so other
// Secrets queue nothing.
func (c *rfController) secretEventHandler() cache.ResourceEventHandler {
	enqueue := func(obj any) {
		key, ok := rfKey(obj)
		if !ok {
			return
		}
		rfKeys, _ := c.rfInformer.GetIndexer().IndexKeys(authSecretIndex, key)
		for _, k := range rfKeys {
			c.enqueue(k)
		}
	}
	return cache.ResourceEventHandlerFuncs{
		AddFunc: enqueue,
		UpdateFunc: func(old, obj any) {
			// A relist sends unchanged Secrets again.
			if old.(*metav1.PartialObjectMetadata).ResourceVersion != obj.(*metav1.PartialObjectMetadata).ResourceVersion {
				enqueue(obj)
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

// enqueueAfter uses a timer, because the queue has no delay.
func (c *rfController) enqueueAfter(key string, d time.Duration) {
	time.AfterFunc(d, func() { c.enqueue(key) })
}

// Run satisfies Controller.
func (c *rfController) Run(ctx context.Context) error {
	if c.leRunner == nil {
		return c.run(ctx)
	}
	return c.leRunner.Run(ctx, c.run)
}

func (c *rfController) run(ctx context.Context) error {
	c.logger.Infof("starting controller")
	go c.rfInformer.RunWithContext(ctx)
	go c.podInformer.RunWithContext(ctx)
	if c.secretInformer != nil {
		go c.secretInformer.RunWithContext(ctx)
	}
	// Pod and Secret events only make reconciles faster, so a watch that cannot
	// sync (for example, without RBAC access) must not block them.
	// The wait only fails once ctx is done, i.e. on shutdown.
	if !cache.WaitForNamedCacheSyncWithContext(ctx, c.rfInformer.HasSynced) {
		c.logger.Infof("controller stopped before its cache synced")
		return nil
	}

	var workers sync.WaitGroup
	for range c.workers {
		workers.Go(func() {
			wait.UntilWithContext(ctx, func(ctx context.Context) {
				for c.processNext(ctx) {
				}
			}, time.Second)
		})
	}
	<-ctx.Done()
	c.logger.Infof("stopping controller, waiting for running reconciles")
	c.queue.ShutDown()
	workers.Wait()
	c.logger.Infof("controller stopped")
	return nil
}

func (c *rfController) processNext(ctx context.Context) bool {
	key, shutdown := c.queue.Get()
	if shutdown {
		return false
	}
	defer c.queue.Done(key)
	// A shut down queue still hands out what it holds; drop it once stopping.
	if ctx.Err() != nil {
		return false
	}

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

// process turns a panic of one reconcile into an error. Replies from Redis
// and Sentinel come from tenant images, and one bad reply must not stop the
// operator for all tenants. The next resync reconciles the object again.
func (c *rfController) process(ctx context.Context, key string) (err error) {
	defer func() {
		if r := recover(); r != nil {
			c.logger.WithField("object-key", key).Errorf("panic on object processing: %v\n%s", r, debug.Stack())
			err = errors.New("panic on object processing")
		}
	}()
	obj, exists, err := c.rfInformer.GetIndexer().GetByKey(key)
	if err != nil || !exists {
		return err
	}
	// The handler changes the object, and the informer cache shares it.
	return c.handler.Handle(ctx, obj.(runtime.Object).DeepCopyObject())
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

// newSecretListWatch requests only metadata. The metadata can still contain
// Secret data in the last-applied-configuration annotation, so the transform
// keeps only the key fields. No selector can find only the auth Secrets, so it
// watches all.
func newSecretListWatch(metaClient metadata.Interface) *cache.ListWatch {
	secrets := metaClient.Resource(corev1.SchemeGroupVersion.WithResource("secrets"))
	return &cache.ListWatch{
		ListWithContextFunc: func(ctx context.Context, options metav1.ListOptions) (runtime.Object, error) {
			return secrets.List(ctx, options)
		},
		WatchFuncWithContext: func(ctx context.Context, options metav1.ListOptions) (watch.Interface, error) {
			return secrets.Watch(ctx, options)
		},
	}
}
