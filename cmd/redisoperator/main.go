package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	_ "k8s.io/client-go/plugin/pkg/client/auth/oidc"

	"github.com/saremox/redis-operator/cmd/utils"
	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	"github.com/saremox/redis-operator/operator/redisfailover"
	"github.com/saremox/redis-operator/service/k8s"
	"github.com/saremox/redis-operator/service/redis"
	"github.com/saremox/redis-operator/version"
)

const (
	// shutdownTimeout bounds how long SIGTERM waits for running reconciles
	// and the lease release.
	shutdownTimeout  = 5 * time.Second
	metricsNamespace = "redis_operator"
)

// Main is the  main runner.
type Main struct {
	flags  *utils.CMDFlags
	logger log.Logger
}

// New returns a Main object.
func New(logger log.Logger) Main {
	// Init flags.
	flgs := &utils.CMDFlags{}
	flgs.Init()

	return Main{
		logger: logger,
		flags:  flgs,
	}
}

// Run execs the program.
func (m *Main) Run() error {
	errC := make(chan error, 1)

	m.logger.Infof("Starting redis-operator %s", version.Version)

	// Set correct logging.
	err := m.logger.Set(log.Level(strings.ToLower(m.flags.LogLevel)))
	if err != nil {
		return err
	}

	// Create the metrics client.
	metricsRecorder := metrics.NewRecorder(metricsNamespace, prometheus.DefaultRegisterer)

	// Serve metrics.
	go func() {
		log.Infof("Listening on %s for metrics exposure on URL %s", m.flags.ListenAddr, m.flags.MetricsPath)
		err := newHTTPServer(m.flags.ListenAddr, newHTTPHandler(m.flags.MetricsPath, m.flags.EnablePprof)).ListenAndServe()
		if err != nil {
			log.Fatal(err)
		}
	}()

	// Kubernetes clients.
	k8sClient, customClient, metadataClient, err := utils.CreateKubernetesClients(m.flags)
	if err != nil {
		return err
	}

	// Create kubernetes service.
	k8sservice := k8s.New(k8sClient, customClient, m.logger, metricsRecorder)

	// Create the redis clients
	redisClient := redis.New(metricsRecorder)

	// Get lease lock resource namespace
	lockNamespace := getNamespace()

	// Create operator and run.
	redisfailoverOperator, err := redisfailover.New(m.flags.ToRedisOperatorConfig(), k8sservice, k8sClient, metadataClient, lockNamespace, redisClient, metricsRecorder, m.logger)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		errC <- redisfailoverOperator.Run(ctx)
	}()

	// Await signals.
	sigC := m.createSignalCapturer()
	select {
	case <-sigC:
		m.logger.Infof("Signal captured, exiting...")
		// Let the operator finish its reconciles and release the lease.
		cancel()
		select {
		case err := <-errC:
			return err
		case <-time.After(shutdownTimeout):
			m.logger.Warningf("Operator did not stop within %s, exiting without releasing the leader lease", shutdownTimeout)
			return nil
		}
	case err := <-errC:
		m.logger.Errorf("Error received: %s, exiting...", err)
		return err
	}
}

// newHTTPServer limits the time and the size of a request, so that the server
// closes idle and slow connections. It sets no write timeout, because a CPU
// profile and a large scrape can take a long time.
func newHTTPServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
}

// newHTTPHandler serves the metrics, and the profiler only when enabled,
// because the profiler has no authentication.
func newHTTPHandler(metricsPath string, enablePprof bool) http.Handler {
	mux := http.NewServeMux()
	mux.Handle(metricsPath, promhttp.Handler())
	if enablePprof {
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	}
	return mux
}

func (m *Main) createSignalCapturer() <-chan os.Signal {
	sigC := make(chan os.Signal, 1)
	signal.Notify(sigC, syscall.SIGTERM, syscall.SIGINT)
	return sigC
}

// getNamespace returns the namespace of the leader-election lease: the
// POD_NAMESPACE env var, else the namespace of the service account token,
// else "default".
func getNamespace() string {
	if ns, ok := os.LookupEnv("POD_NAMESPACE"); ok {
		return ns
	}

	if data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
		if ns := strings.TrimSpace(string(data)); len(ns) > 0 {
			return ns
		}
	}

	return "default"
}

// Run app.
func main() {
	logger := log.Base()
	m := New(logger)

	if err := m.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "error executing: %s", err)
		os.Exit(1)
	}
	os.Exit(0)
}
