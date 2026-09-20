package redisfailover

import (
	"context"
	"regexp"
	"time"

	"github.com/spotahome/kooper/v2/controller"
	"github.com/spotahome/kooper/v2/controller/leaderelection"
	kooperlog "github.com/spotahome/kooper/v2/log"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
	"github.com/saremox/redis-operator/service/k8s"
	"github.com/saremox/redis-operator/service/redis"
)

const (
	operatorName = "redis-operator"
	lockKey      = "redis-failover-lease"
)

// New will create an operator that is responsible for managing all the required stuff
// to create redis failovers.
func New(cfg Config, k8sService k8s.Services, k8sClient kubernetes.Interface, lockNamespace string, redisClient redis.Client, kooperMetricsRecorder metrics.Recorder, logger log.Logger) (controller.Controller, error) {
	// Create internal services.
	rfService := rfservice.NewRedisFailoverKubeClient(k8sService, logger, kooperMetricsRecorder)
	rfChecker := rfservice.NewRedisFailoverChecker(k8sService, redisClient, logger, kooperMetricsRecorder)
	rfHealer := rfservice.NewRedisFailoverHealer(k8sService, redisClient, logger)

	// Create the handlers.
	rfHandler := NewRedisFailoverHandler(cfg, rfService, rfChecker, rfHealer, k8sService, kooperMetricsRecorder, logger)
	rfRetriever := NewRedisFailoverRetriever(cfg, k8sService)

	kooperLogger := kooperlogger{Logger: logger.WithField("operator", "redisfailover")}
	// Leader election service.
	leSVC, err := leaderelection.NewDefault(lockKey, lockNamespace, k8sClient, kooperLogger)
	if err != nil {
		return nil, err
	}

	// Create our controller.
	return controller.New(&controller.Config{
		Handler:         rfHandler,
		Retriever:       rfRetriever,
		LeaderElector:   leSVC,
		MetricsRecorder: kooperMetricsRecorder,
		Logger:          kooperLogger,
		Name:            "redisfailover",
		ResyncInterval:  time.Duration(cfg.SyncInterval) * time.Second,
		// ProcessingJobRetries has to be > 0 for ErrReconcileIncomplete
		// (checker.go) to do anything at all: kooper only calls
		// queue.Requeue on a Handle() error when a retryProcessor is wired
		// in, which only happens when this is positive (controller.go's
		// setDefaults). Without it, every error - ours or a genuine one -
		// falls through unretried until the next watch event or resync,
		// which is the exact gap this whole change exists to close.
		//
		// CAVEAT (read before raising this number, and before relying on
		// this mechanism at all): kooper's queue.Requeue rejects a key once
		// workqueue's NumRequeues(key) reaches this value, and nothing in
		// kooper ever calls Forget() to reset that counter on a *successful*
		// Handle() call - not here, not in the base processor. NumRequeues
		// only resets by the process restarting. That means this is a
		// lifetime cap, shared across every reason a given RedisFailover's
		// key is ever requeued this way - both genuine transient errors and
		// deliberate ErrReconcileIncomplete signals accumulate against the
		// same counter for as long as the operator process runs. A
		// long-lived RedisFailover that goes through enough rollouts (or
		// enough retried errors) eventually exhausts it, at which point
		// queue.Requeue starts returning errMaxRetriesReached and this
		// object stops getting fast-retried at all until the operator
		// restarts - silently regressing back to today's behavior (rely on
		// the next incidental watch event or full resync) for exactly the
		// objects that have been reconciled the most.
		ProcessingJobRetries: 20,
		ConcurrentWorkers:    cfg.Concurrency,
	})
}

func NewRedisFailoverRetriever(cfg Config, cli k8s.Services) controller.Retriever {
	isNamespaceSupported := func(rf redisfailoverv1.RedisFailover) bool {
		match, _ := regexp.Match(cfg.SupportedNamespacesRegex, []byte(rf.Namespace))
		return match
	}
	// check in the startup whether the regex compiles

	return controller.MustRetrieverFromListerWatcher(&cache.ListWatch{
		ListWithContextFunc: func(ctx context.Context, options metav1.ListOptions) (runtime.Object, error) {
			rfList, err := cli.ListRedisFailovers(ctx, "", options)
			if err != nil {
				return rfList, err
			}

			targetRFList := make([]redisfailoverv1.RedisFailover, 0)
			for _, rf := range rfList.Items {
				if isNamespaceSupported(rf) {
					targetRFList = append(targetRFList, rf)
				}
			}
			rfList.Items = targetRFList

			return rfList, err
		},
		WatchFuncWithContext: func(ctx context.Context, options metav1.ListOptions) (watch.Interface, error) {
			watcher, err := cli.WatchRedisFailovers(ctx, "", options)
			if err != nil || watcher == nil {
				return watcher, err
			}
			watcher = watch.Filter(watcher, func(event watch.Event) (watch.Event, bool) {
				rf, ok := event.Object.(*redisfailoverv1.RedisFailover)
				if !ok {
					return event, false
				}
				return event, isNamespaceSupported(*rf)
			})
			return watcher, err
		},
	})
}

type kooperlogger struct {
	log.Logger
}

func (k kooperlogger) WithKV(kv kooperlog.KV) kooperlog.Logger {
	return kooperlogger{Logger: k.WithFields(kv)}
}
