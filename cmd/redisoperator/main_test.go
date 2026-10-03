package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/saremox/redis-operator/cmd/utils"
	"github.com/saremox/redis-operator/log"
	mLog "github.com/saremox/redis-operator/mocks/log"
	"github.com/saremox/redis-operator/version"
)

func TestNewHTTPHandler(t *testing.T) {
	tests := map[string]struct {
		enablePprof bool
		wantPprof   int
	}{
		"profiler off": {enablePprof: false, wantPprof: http.StatusNotFound},
		"profiler on":  {enablePprof: true, wantPprof: http.StatusOK},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			h := newHTTPHandler("/metrics", test.enablePprof)

			for path, want := range map[string]int{
				"/metrics":                       http.StatusOK,
				"/debug/pprof/":                  test.wantPprof,
				"/debug/pprof/cmdline":           test.wantPprof,
				"/debug/pprof/symbol":            test.wantPprof,
				"/debug/pprof/profile?seconds=1": test.wantPprof,
				"/debug/pprof/trace?seconds=1":   test.wantPprof,
			} {
				w := httptest.NewRecorder()
				h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
				assert.Equal(t, want, w.Code, path)
			}
		})
	}
}

func TestRunServesTheHTTPHandler(t *testing.T) {
	prevRegisterer := prometheus.DefaultRegisterer
	t.Cleanup(func() { prometheus.DefaultRegisterer = prevRegisterer })
	prometheus.DefaultRegisterer = prometheus.NewRegistry()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().String()
	require.NoError(t, l.Close())

	m := Main{
		flags: &utils.CMDFlags{
			LogLevel:    "info",
			ListenAddr:  addr,
			MetricsPath: "/metrics",
			EnablePprof: false,
			Development: true,
			KubeConfig:  t.TempDir() + "/missing",
		},
		logger: log.Dummy,
	}

	// Run starts the HTTP server, then fails on the missing kubeconfig.
	assert.Error(t, m.Run())

	for path, want := range map[string]int{
		"/metrics":      http.StatusOK,
		"/debug/pprof/": http.StatusNotFound,
		"/debug/vars":   http.StatusNotFound,
	} {
		assert.EventuallyWithT(t, func(c *assert.CollectT) {
			resp, err := http.Get("http://" + addr + path)
			if !assert.NoError(c, err) {
				return
			}
			defer func() { _ = resp.Body.Close() }()
			assert.Equal(c, want, resp.StatusCode)
		}, 5*time.Second, 50*time.Millisecond, path)
	}
}

func TestRunLogsTheVersion(t *testing.T) {
	prev := version.Version
	t.Cleanup(func() { version.Version = prev })
	version.Version = "v1.2.3"

	logger := &mLog.Logger{}
	logger.On("Infof", "Starting redis-operator %s", "v1.2.3").Once().Return()
	logger.On("Set", log.Level("info")).Once().Return(assert.AnError)
	m := Main{flags: &utils.CMDFlags{LogLevel: "info"}, logger: logger}

	err := m.Run()

	assert.ErrorIs(t, err, assert.AnError)
	logger.AssertExpectations(t)
}

const unreachableKubeConfig = `apiVersion: v1
kind: Config
clusters: [{name: c, cluster: {server: "https://127.0.0.1:1"}}]
users: [{name: u, user: {token: x}}]
contexts: [{name: c, context: {cluster: c, user: u}}]
current-context: c
`

func TestRunStopsOnSIGTERM(t *testing.T) {
	prevRegisterer := prometheus.DefaultRegisterer
	t.Cleanup(func() { prometheus.DefaultRegisterer = prevRegisterer })
	prometheus.DefaultRegisterer = prometheus.NewRegistry()
	t.Setenv("POD_NAMESPACE", "default")

	// This channel keeps SIGTERM from stopping the test binary before Run
	// listens for it.
	sigC := make(chan os.Signal, 1)
	signal.Notify(sigC, syscall.SIGTERM)
	t.Cleanup(func() { signal.Stop(sigC) })

	kubeConfig := filepath.Join(t.TempDir(), "kubeconfig")
	require.NoError(t, os.WriteFile(kubeConfig, []byte(unreachableKubeConfig), 0o600))
	m := Main{
		flags: &utils.CMDFlags{
			LogLevel:                 "info",
			ListenAddr:               "127.0.0.1:0",
			MetricsPath:              "/metrics",
			Development:              true,
			KubeConfig:               kubeConfig,
			SupportedNamespacesRegex: ".*",
			Concurrency:              1,
			SyncInterval:             30,
		},
		logger: log.Dummy,
	}

	errC := make(chan error, 1)
	go func() { errC <- m.Run() }()

	// Run registers for SIGTERM after it creates the operator, so send it
	// until Run returns.
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	timeout := time.After(15 * time.Second)
	for {
		select {
		case err := <-errC:
			assert.NoError(t, err)
			return
		case <-tick.C:
			require.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGTERM))
		case <-timeout:
			t.Fatal("Run did not return after SIGTERM")
		}
	}
}
