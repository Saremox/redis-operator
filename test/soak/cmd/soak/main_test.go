package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/client-go/kubernetes/fake"

	rffake "github.com/saremox/redis-operator/client/k8s/clientset/versioned/fake"
	"github.com/saremox/redis-operator/test/soak/internal/auth"
	"github.com/saremox/redis-operator/test/soak/internal/config"
	"github.com/saremox/redis-operator/test/soak/internal/global"
	"github.com/saremox/redis-operator/test/soak/internal/metrics"
	"github.com/saremox/redis-operator/test/soak/internal/mutator"
)

// run returns the error of the config, and of the client config, before it
// starts anything.
func TestRunEarlyErrors(t *testing.T) {
	dir := t.TempDir()
	log := slog.New(slog.DiscardHandler)
	if err := run(filepath.Join(dir, "missing.yaml"), ":0", log); err == nil || !strings.Contains(err.Error(), "no such file or directory") {
		t.Errorf("missing config: %v", err)
	}
	cfg := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfg, []byte("instances: [{name: a}]"), 0o600); err != nil {
		t.Fatal(err)
	}
	kubeconfig := filepath.Join(dir, "kubeconfig")
	if err := os.WriteFile(kubeconfig, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", kubeconfig)
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	if err := run(cfg, ":0", log); err == nil || !strings.Contains(err.Error(), kubeconfig) {
		t.Errorf("bad kubeconfig: %v", err)
	}
}

// clients reads the kubeconfig and raises the rate limit of the observers.
func TestClients(t *testing.T) {
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	const body = `apiVersion: v1
kind: Config
clusters: [{name: c, cluster: {server: "https://127.0.0.1:1"}}]
users: [{name: u, user: {token: t}}]
contexts: [{name: x, context: {cluster: c, user: u}}]
current-context: x
`
	if err := os.WriteFile(kubeconfig, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", kubeconfig)
	kube, rfs, err := clients()
	if err != nil {
		t.Fatal(err)
	}
	if qps := kube.CoreV1().RESTClient().GetRateLimiter().QPS(); qps != 50 || rfs == nil {
		t.Errorf("qps %v, rfs %v", qps, rfs)
	}
}

// An outage counts for the chaos action, or else the mutation, that ran when
// it started.
func TestOutageEvent(t *testing.T) {
	lock := &global.Lock{}
	if got := outageEvent(lock, nil)(); got != "" {
		t.Errorf("no event: %q", got)
	}
	if got := outageEvent(lock, &mutator.Mutator{})(); got != "" {
		t.Errorf("no mutation: %q", got)
	}
	lock.SetChaos("operator_restart")
	for _, mu := range []*mutator.Mutator{nil, {}} {
		if got := outageEvent(lock, mu)(); got != "operator_restart" {
			t.Errorf("chaos: %q", got)
		}
	}
}

// newObservers makes an observer of each instance, and the data of the
// instances with data.
func TestNewObservers(t *testing.T) {
	cfg, err := config.Parse([]byte("instances: [{name: a, data: {fill: {}}}, {name: b}]"))
	if err != nil {
		t.Fatal(err)
	}
	sources := map[string]*auth.Source{"a": auth.New(nil), "b": auth.New(nil)}
	m := metrics.New(prometheus.NewRegistry(), time.Minute)
	observers, datas := newObservers(cfg, fake.NewClientset(), rffake.NewSimpleClientset(), sources, &global.Lock{}, m, slog.New(slog.DiscardHandler))
	if len(observers) != 2 || observers["a"] == nil || observers["b"] == nil {
		t.Errorf("observers %v", observers)
	}
	if len(datas) != 1 || datas["a"] == nil {
		t.Errorf("data %v", datas)
	}
}
