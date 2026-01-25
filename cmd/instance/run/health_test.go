package run

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseRedisInfo(t *testing.T) {
	info := `# Server
redis_version:7.0.0
redis_git_sha1:00000000
process_id:1234

# Clients
connected_clients:5

# Memory
used_memory:1024000
used_memory_human:1000K

# Replication
role:master
connected_slaves:2

# Persistence
loading:0
rdb_bgsave_in_progress:0
aof_rewrite_in_progress:0
`

	result := parseRedisInfo(info)

	assert.Equal(t, "7.0.0", result["redis_version"])
	assert.Equal(t, "1234", result["process_id"])
	assert.Equal(t, "5", result["connected_clients"])
	assert.Equal(t, "1024000", result["used_memory"])
	assert.Equal(t, "1000K", result["used_memory_human"])
	assert.Equal(t, "master", result["role"])
	assert.Equal(t, "2", result["connected_slaves"])
	assert.Equal(t, "0", result["loading"])
	assert.Equal(t, "0", result["rdb_bgsave_in_progress"])
}

func TestParseRedisInfoReplica(t *testing.T) {
	info := `# Replication
role:slave
master_host:10.0.0.1
master_port:6379
master_link_status:up
master_sync_in_progress:0
slave_repl_offset:12345
`

	result := parseRedisInfo(info)

	assert.Equal(t, "slave", result["role"])
	assert.Equal(t, "10.0.0.1", result["master_host"])
	assert.Equal(t, "6379", result["master_port"])
	assert.Equal(t, "up", result["master_link_status"])
	assert.Equal(t, "0", result["master_sync_in_progress"])
	assert.Equal(t, "12345", result["slave_repl_offset"])
}

func TestParseRedisInfoLoading(t *testing.T) {
	info := `# Persistence
loading:1
loading_total_bytes:1000000
loading_loaded_bytes:500000
`

	result := parseRedisInfo(info)

	assert.Equal(t, "1", result["loading"])
	assert.Equal(t, "1000000", result["loading_total_bytes"])
	assert.Equal(t, "500000", result["loading_loaded_bytes"])
}

func TestHealthServerHealthzHealthy(t *testing.T) {
	h := NewHealthServer(8080, "6379", "")
	h.startTime = time.Now().Add(-60 * time.Second) // 60 seconds ago
	h.SetRedisPID(1234)
	h.redisHealthy.Store(true)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()

	h.handleHealthz(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp HealthResponse
	err := json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(t, err)

	assert.Equal(t, "ok", resp.Status)
	assert.Equal(t, 1234, resp.RedisPID)
	assert.GreaterOrEqual(t, resp.UptimeSeconds, int64(60))
}

func TestHealthServerHealthzUnhealthy(t *testing.T) {
	h := NewHealthServer(8080, "6379", "")
	h.redisHealthy.Store(false)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()

	h.handleHealthz(w, req)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)

	var resp HealthResponse
	err := json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(t, err)

	assert.Equal(t, "unhealthy", resp.Status)
	assert.Contains(t, resp.Error, "not responding")
}

func TestHealthServerReadyzReady(t *testing.T) {
	h := NewHealthServer(8080, "6379", "")
	h.redisReady.Store(true)
	h.redisHealthy.Store(true)

	h.mu.Lock()
	h.cachedInfo = map[string]string{
		"role":              "master",
		"connected_clients": "10",
		"loading":           "0",
	}
	h.mu.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()

	h.handleReadyz(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp ReadyResponse
	err := json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(t, err)

	assert.Equal(t, "ok", resp.Status)
	assert.Equal(t, "master", resp.Role)
	assert.Equal(t, 10, resp.ConnectedClients)
	assert.False(t, resp.Loading)
}

func TestHealthServerReadyzNotReadyLoading(t *testing.T) {
	h := NewHealthServer(8080, "6379", "")
	h.redisReady.Store(false)
	h.redisHealthy.Store(true)

	h.mu.Lock()
	h.cachedInfo = map[string]string{
		"role":    "master",
		"loading": "1",
	}
	h.mu.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()

	h.handleReadyz(w, req)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)

	var resp ReadyResponse
	err := json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(t, err)

	assert.Equal(t, "not ready", resp.Status)
	assert.True(t, resp.Loading)
	assert.Contains(t, resp.Error, "loading")
}

func TestHealthServerReadyzNotReadySyncing(t *testing.T) {
	h := NewHealthServer(8080, "6379", "")
	h.redisReady.Store(false)
	h.redisHealthy.Store(true)

	h.mu.Lock()
	h.cachedInfo = map[string]string{
		"role":                    "slave",
		"loading":                 "0",
		"master_sync_in_progress": "1",
		"master_link_status":      "up",
	}
	h.mu.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()

	h.handleReadyz(w, req)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)

	var resp ReadyResponse
	err := json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(t, err)

	assert.Equal(t, "not ready", resp.Status)
	assert.True(t, resp.MasterSyncInProgress)
	assert.Contains(t, resp.Error, "sync")
}

func TestHealthServerReadyzNotReadyMasterLinkDown(t *testing.T) {
	h := NewHealthServer(8080, "6379", "")
	h.redisReady.Store(false)
	h.redisHealthy.Store(true)

	h.mu.Lock()
	h.cachedInfo = map[string]string{
		"role":                    "slave",
		"loading":                 "0",
		"master_sync_in_progress": "0",
		"master_link_status":      "down",
	}
	h.mu.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()

	h.handleReadyz(w, req)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)

	var resp ReadyResponse
	err := json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(t, err)

	assert.Contains(t, resp.Error, "master link")
}

func TestHealthServerReadyzNotReadyNoMasterConfigured(t *testing.T) {
	h := NewHealthServer(8080, "6379", "")
	h.redisReady.Store(false)
	h.redisHealthy.Store(true)

	h.mu.Lock()
	h.cachedInfo = map[string]string{
		"role":                    "slave",
		"loading":                 "0",
		"master_sync_in_progress": "0",
		"master_link_status":      "up",
		"master_host":             "127.0.0.1", // No real master, pointing to self
	}
	h.mu.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()

	h.handleReadyz(w, req)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)

	var resp ReadyResponse
	err := json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(t, err)

	assert.Contains(t, resp.Error, "no master configured")
}

func TestHealthServerStatus(t *testing.T) {
	h := NewHealthServer(8080, "6379", "")
	h.startTime = time.Now().Add(-120 * time.Second)
	h.SetRedisPID(5678)
	h.SetCleanupDone(true)

	h.mu.Lock()
	h.cachedInfo = map[string]string{
		"role":                   "master",
		"connected_clients":      "15",
		"used_memory":            "2048000",
		"used_memory_human":      "2M",
		"loading":                "0",
		"rdb_bgsave_in_progress": "0",
		"aof_rewrite_in_progress": "0",
		"connected_slaves":       "2",
		"master_repl_offset":     "99999",
	}
	h.mu.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	w := httptest.NewRecorder()

	h.handleStatus(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp StatusResponse
	err := json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(t, err)

	// Redis status
	assert.Equal(t, 5678, resp.Redis.PID)
	assert.Equal(t, "master", resp.Redis.Role)
	assert.Equal(t, 15, resp.Redis.ConnectedClients)
	assert.Equal(t, "2M", resp.Redis.UsedMemoryHuman)
	assert.False(t, resp.Redis.Loading)
	assert.False(t, resp.Redis.RDBBgsaveInProgress)

	// Replication status
	assert.Equal(t, "master", resp.Replication.Role)
	assert.Equal(t, 2, resp.Replication.ConnectedSlaves)
	assert.Equal(t, int64(99999), resp.Replication.MasterReplOffset)

	// Instance manager status
	assert.Equal(t, "4.0.0", resp.InstanceManager.Version)
	assert.GreaterOrEqual(t, resp.InstanceManager.UptimeSeconds, int64(120))
	assert.True(t, resp.InstanceManager.StartupCleanupDone)
	assert.Equal(t, 8080, resp.InstanceManager.HealthPort)
}

func TestHealthServerMethodNotAllowed(t *testing.T) {
	h := NewHealthServer(8080, "6379", "")

	endpoints := []string{"/healthz", "/readyz", "/status"}
	methods := []string{http.MethodPost, http.MethodPut, http.MethodDelete}

	for _, endpoint := range endpoints {
		for _, method := range methods {
			req := httptest.NewRequest(method, endpoint, nil)
			w := httptest.NewRecorder()

			switch endpoint {
			case "/healthz":
				h.handleHealthz(w, req)
			case "/readyz":
				h.handleReadyz(w, req)
			case "/status":
				h.handleStatus(w, req)
			}

			assert.Equal(t, http.StatusMethodNotAllowed, w.Code,
				"expected 405 for %s %s", method, endpoint)
		}
	}
}

func TestSplitLines(t *testing.T) {
	tests := []struct {
		input    string
		expected []string
	}{
		{"a\nb\nc", []string{"a", "b", "c"}},
		{"a\r\nb\r\nc", []string{"a", "b", "c"}},
		{"single", []string{"single"}},
		{"a\nb", []string{"a", "b"}},
	}

	for _, tt := range tests {
		result := splitLines(tt.input)
		assert.Equal(t, tt.expected, result, "input: %q", tt.input)
	}
}

func TestIndexByte(t *testing.T) {
	assert.Equal(t, 3, indexByte("foo:bar", ':'))
	assert.Equal(t, -1, indexByte("foobar", ':'))
	assert.Equal(t, 0, indexByte(":foo", ':'))
}

func TestHealthServerStartStop(t *testing.T) {
	h := NewHealthServer(0, "6379", "") // Port 0 = random available port

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start should not error (even without Redis)
	err := h.Start(ctx)
	assert.NoError(t, err)

	// Stop should not error
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	err = h.Stop(stopCtx)
	assert.NoError(t, err)
}
