package main

import (
	"context"
	"fmt"
	"net/http"
	_ "net/http/pprof"
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
		http.Handle(m.flags.MetricsPath, promhttp.Handler())
		err := http.ListenAndServe(m.flags.ListenAddr, nil)
		if err != nil {
			log.Fatal(err)
		}
	}()

	// Kubernetes clients.
	k8sClient, customClient, err := utils.CreateKubernetesClients(m.flags)
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
	redisfailoverOperator, err := redisfailover.New(m.flags.ToRedisOperatorConfig(), k8sservice, k8sClient, lockNamespace, redisClient, metricsRecorder, m.logger)
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

func (m *Main) createSignalCapturer() <-chan os.Signal {
	sigC := make(chan os.Signal, 1)
	signal.Notify(sigC, syscall.SIGTERM, syscall.SIGINT)
	return sigC
}

func getNamespace() string {
	// This way assumes you've set the POD_NAMESPACE environment
	// variable using the downward API.  This check has to be done first
	// for backwards compatibility with the way InClusterConfig was
	// originally set up
	if ns, ok := os.LookupEnv("POD_NAMESPACE"); ok {
		return ns
	}

	// Fall back to the namespace associated with the service account
	// token, if available
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
