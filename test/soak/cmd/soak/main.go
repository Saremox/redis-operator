// Command soak is a long-running tester for the redis-operator.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/saremox/redis-operator/test/soak/internal/config"
	"github.com/saremox/redis-operator/test/soak/internal/metrics"
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
	restCfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(), nil).ClientConfig()
	if err != nil {
		return err
	}
	kube, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	reg := prometheus.NewRegistry()
	m := metrics.New(reg)
	srv := &http.Server{Addr: listen, Handler: metrics.Handler(reg), ReadHeaderTimeout: 10 * time.Second}

	var wg sync.WaitGroup
	wg.Go(func() { reportBuildInfo(ctx, kube, cfg.Operator, m, log) })
	for _, in := range cfg.Instances {
		path := prober.MasterService(in, cfg.Probe.Timeout.Duration)
		for _, client := range []prober.Client{prober.Pooled, prober.Fresh} {
			p := prober.New(in, path, client, cfg.Probe, m, log)
			wg.Go(func() { p.Run(ctx) })
		}
	}
	wg.Go(func() {
		<-ctx.Done()
		_ = srv.Shutdown(context.Background())
	})

	log.Info("starting", "version", version, "instances", len(cfg.Instances), "listen", listen)
	err = srv.ListenAndServe()
	stop()
	wg.Wait()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// reportBuildInfo follows the operator's version, which changes when a
// newer release candidate is rolled out under the running tester.
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
