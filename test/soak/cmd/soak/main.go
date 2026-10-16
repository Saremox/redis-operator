// Command soak is a long-running tester for the redis-operator.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/saremox/redis-operator/client/k8s/clientset/versioned"
	"github.com/saremox/redis-operator/test/soak/internal/auth"
	"github.com/saremox/redis-operator/test/soak/internal/chaos"
	"github.com/saremox/redis-operator/test/soak/internal/config"
	"github.com/saremox/redis-operator/test/soak/internal/data"
	"github.com/saremox/redis-operator/test/soak/internal/global"
	"github.com/saremox/redis-operator/test/soak/internal/instances"
	"github.com/saremox/redis-operator/test/soak/internal/metrics"
	"github.com/saremox/redis-operator/test/soak/internal/mutator"
	"github.com/saremox/redis-operator/test/soak/internal/observer"
	"github.com/saremox/redis-operator/test/soak/internal/operator"
	"github.com/saremox/redis-operator/test/soak/internal/prober"
)

// version is set at build time.
var version = "dev"

func main() {
	configPath := flag.String("config", "/etc/soak/config.yaml", "path to the config file")
	listen := flag.String("listen", ":9090", "address for /metrics and /healthz")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	redis.SetLogger(redisLogger{log.With("component", "go-redis")})
	if err := run(*configPath, *listen, log); err != nil {
		log.Error("exiting", "error", err.Error())
		os.Exit(1)
	}
}

func run(configPath, listen string, log *slog.Logger) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	kube, rfs, err := clients()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	reg := prometheus.NewRegistry()
	// Convergence histograms reach the longest timeout of up to 5 pods.
	m := metrics.New(reg, cfg.LongestTimeout(5))
	srv := &http.Server{Addr: listen, Handler: metrics.Handler(reg), ReadHeaderTimeout: 10 * time.Second}

	insts, err := ensureInstances(ctx, kube, rfs, cfg, log)
	if err != nil {
		return err
	}
	sources, err := authSources(ctx, kube, rfs, cfg.Instances)
	if err != nil {
		return err
	}
	lock := &global.Lock{}
	observers, datas := newObservers(cfg, kube, rfs, sources, lock, m, log)

	var wg sync.WaitGroup
	wg.Go(func() { reportBuildInfo(ctx, kube, cfg.Operator, m, log) })
	wg.Go(func() { detectStalls(ctx, m.ProcessStalls, log) })
	var lane []chaos.Instance
	for _, in := range cfg.Instances {
		o, a := observers[in.Name], sources[in.Name]
		r := prober.NewRunner(in, cfg.Probe, a, m, log)
		var d mutator.Data
		if dd := datas[in.Name]; dd != nil {
			wg.Go(func() { dd.Run(ctx) })
			d = dd
		}
		if b := in.Bootstrap; b != nil {
			o.SetSource(observers[b.Source])
			rd := data.NewReplica(in, cfg, datas[b.Source], o, a, m, log)
			wg.Go(func() { rd.Run(ctx) })
			d = rd
		}
		var mu *mutator.Mutator
		if len(in.Mutations.Kinds) > 0 {
			mu = mutator.New(in, cfg, kube, rfs, o, d, a, lock, insts[in.Name], m, log)
			// Before Run, because the observer reads it without a lock.
			o.SetMutation(mu.Allowed)
			wg.Go(func() { mu.Run(ctx) })
		}
		wg.Go(func() { o.Run(ctx) })
		r.SetFull(o.NoevictionFull)
		r.SetEvent(outageEvent(lock, mu))
		wg.Go(func() { r.Run(ctx, func() []string { return prober.Names(in, o.SentinelPath()) }) })
		lane = append(lane, chaos.Instance{Name: in.Name, Namespace: in.Namespace, Observer: o, Data: d})
	}
	if cfg.ChaosOn() {
		l := chaos.New(cfg, kube, lock, lane, os.Getenv("NODE_NAME"), m, log)
		wg.Go(func() { l.Run(ctx) })
	}
	wg.Go(func() {
		<-ctx.Done()
		// ctx is done here, and the shutdown must wait for open requests.
		_ = srv.Shutdown(context.WithoutCancel(ctx))
	})

	log.Info("starting", "version", version, "instances", len(cfg.Instances), "listen", listen,
		"chaos", cfg.ChaosOn(), "seed", cfg.Mutation.Seed)
	err = srv.ListenAndServe()
	stop()
	wg.Wait()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// outageEvent returns the event of an outage: the chaos action, or else the
// mutation of mu, that ran when it started.
func outageEvent(lock *global.Lock, mu *mutator.Mutator) func() string {
	return func() string {
		if k := lock.Chaos(); k != "" || mu == nil {
			return k
		}
		return mu.Current()
	}
}

// clients returns the clients of the cluster that the kubeconfig or the
// in-cluster config names.
func clients() (*kubernetes.Clientset, *versioned.Clientset, error) {
	restCfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(), nil).ClientConfig()
	if err != nil {
		return nil, nil, err
	}
	// The observers list pods, EndpointSlices and RedisFailovers for every
	// instance every few seconds.
	restCfg.QPS, restCfg.Burst = 50, 100
	kube, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, nil, err
	}
	rfs, err := versioned.NewForConfig(restCfg)
	if err != nil {
		return nil, nil, err
	}
	return kube, rfs, nil
}

// newObservers returns the observer of each instance, and the data of each
// instance with data, by instance name.
func newObservers(cfg *config.Config, kube kubernetes.Interface, rfs versioned.Interface, sources map[string]*auth.Source, lock *global.Lock, m *metrics.Metrics, log *slog.Logger) (map[string]*observer.Observer, map[string]*data.Data) {
	observers := map[string]*observer.Observer{}
	datas := map[string]*data.Data{}
	for _, in := range cfg.Instances {
		o := observer.New(in, cfg, kube, rfs, sources[in.Name], lock, m, log)
		observers[in.Name] = o
		if in.Data != nil {
			datas[in.Name] = data.New(in, cfg, o, sources[in.Name], m, log)
		}
	}
	return observers, datas
}

// ensureInstances creates each instance with a template that does not exist
// yet, on its configured versions.
func ensureInstances(ctx context.Context, kube kubernetes.Interface, rfs versioned.Interface, cfg *config.Config, log *slog.Logger) (map[string]*instances.Instance, error) {
	out := map[string]*instances.Instance{}
	for _, in := range cfg.Instances {
		if in.Template == "" {
			continue
		}
		i, err := instances.New(in, kube, rfs, log)
		if err != nil {
			return nil, fmt.Errorf("instance %s: %w", in.Name, err)
		}
		redisVersion, _ := cfg.VersionNamed(in.Version)
		sentinel, _ := cfg.VersionNamed(in.SentinelVersion)
		if err := i.Ensure(ctx, redisVersion.Image, sentinel.Image); err != nil {
			return nil, fmt.Errorf("instance %s: %w", in.Name, err)
		}
		out[in.Name] = i
	}
	return out, nil
}

// authSources follows the auth Secret of each instance through an informer on
// the Secrets of its namespace. Each source starts on the current secretPath,
// so that the first probes authenticate.
func authSources(ctx context.Context, kube kubernetes.Interface, rfs versioned.Interface, configured []config.Instance) (map[string]*auth.Source, error) {
	factories := map[string]informers.SharedInformerFactory{}
	sources := map[string]*auth.Source{}
	for _, in := range configured {
		f, ok := factories[in.Namespace]
		if !ok {
			f = informers.NewSharedInformerFactoryWithOptions(kube, 0, informers.WithNamespace(in.Namespace))
			factories[in.Namespace] = f
		}
		sources[in.Name] = auth.New(f.Core().V1().Secrets().Lister().Secrets(in.Namespace).Get)
	}
	for ns, f := range factories {
		f.Start(ctx.Done())
		for informer, synced := range f.WaitForCacheSync(ctx.Done()) {
			if !synced {
				return nil, fmt.Errorf("syncing the Secrets of namespace %s: %v not synced", ns, informer)
			}
		}
	}
	for _, in := range configured {
		if rf, err := rfs.DatabasesV1().RedisFailovers(in.Namespace).Get(ctx, in.Name, metav1.GetOptions{}); err == nil {
			sources[in.Name].Update(rf)
		}
	}
	return sources, nil
}

// reportBuildInfo follows the operator version, because a new release
// candidate can roll out while the tester runs.
func reportBuildInfo(ctx context.Context, kube kubernetes.Interface, op config.Operator, m *metrics.Metrics, log *slog.Logger) {
	current := ""
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		v, err := operator.Version(ctx, kube, op.Namespace, op.Deployment)
		if err != nil {
			log.Warn("reading the operator version", "error", err.Error())
			v = "unknown"
		}
		if v != current {
			m.BuildInfo.Reset()
			m.BuildInfo.WithLabelValues(v, version).Set(1)
			log.Info("operator version", "operator_version", v)
			current = v
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// redisLogger sends the go-redis logs to debug level: the Sentinel client logs
// each Sentinel that it finds, and a fresh client for each probe repeats this
// every second.
type redisLogger struct{ log *slog.Logger }

func (l redisLogger) Printf(ctx context.Context, format string, v ...any) {
	l.log.DebugContext(ctx, fmt.Sprintf(format, v...))
}
