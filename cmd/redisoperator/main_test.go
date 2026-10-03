package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/saremox/redis-operator/cmd/utils"
	"github.com/saremox/redis-operator/log"
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
				"/metrics":             http.StatusOK,
				"/debug/pprof/":        test.wantPprof,
				"/debug/pprof/cmdline": test.wantPprof,
				"/debug/pprof/symbol":  test.wantPprof,
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
			EnablePprof: true,
			Development: true,
			KubeConfig:  t.TempDir() + "/missing",
		},
		logger: log.Dummy,
	}

	// Run starts the HTTP server, then fails on the missing kubeconfig.
	assert.Error(t, m.Run())

	for _, path := range []string{"/metrics", "/debug/pprof/"} {
		assert.EventuallyWithT(t, func(c *assert.CollectT) {
			resp, err := http.Get("http://" + addr + path)
			if !assert.NoError(c, err) {
				return
			}
			defer func() { _ = resp.Body.Close() }()
			assert.Equal(c, http.StatusOK, resp.StatusCode)
		}, 5*time.Second, 50*time.Millisecond, path)
	}
}
