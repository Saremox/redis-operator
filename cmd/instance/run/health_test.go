package run

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/saremox/redis-operator/version"
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
	prev := version.Version
	t.Cleanup(func() { version.Version = prev })
	version.Version = "v1.2.3"

	h := NewHealthServer(8080, "6379", "")
	h.startTime = time.Now().Add(-120 * time.Second)
	h.SetRedisPID(5678)
	h.SetCleanupDone(true)

	h.mu.Lock()
	h.cachedInfo = map[string]string{
		"role":                    "master",
		"connected_clients":       "15",
		"used_memory":             "2048000",
		"used_memory_human":       "2M",
		"loading":                 "0",
		"rdb_bgsave_in_progress":  "0",
		"aof_rewrite_in_progress": "0",
		"connected_slaves":        "2",
		"master_repl_offset":      "99999",
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
	assert.Equal(t, "v1.2.3", resp.InstanceManager.Version)
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
		{"a\r\nb\r", []string{"a", "b"}},
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

func TestHealthServerStartReturnsBindError(t *testing.T) {
	occupied, err := net.Listen("tcp", ":0")
	require.NoError(t, err)
	defer func() { _ = occupied.Close() }()

	tests := []struct {
		name string
		port int
	}{
		{"port in use", occupied.Addr().(*net.TCPAddr).Port},
		{"invalid port", 70000},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			h := NewHealthServer(tt.port, "6379", "")
			err := h.Start(ctx)
			if err == nil {
				stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer stopCancel()
				_ = h.Stop(stopCtx)
			}
			assert.Error(t, err)
		})
	}
}

func TestHealthServerStartServesHealthz(t *testing.T) {
	// Port "0" makes the Redis ping fail, so /healthz reports unhealthy.
	h := NewHealthServer(0, "0", "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	require.NoError(t, h.Start(ctx))
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCancel()
		assert.NoError(t, h.Stop(stopCtx))
	}()

	port := h.listenAddr.(*net.TCPAddr).Port
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", port))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)

	var body HealthResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	assert.Equal(t, "unhealthy", body.Status)
}

// fakeRedis is a tiny Redis stand-in. It answers PING, and it answers INFO
// with info, or with an error when infoErr is set.
type fakeRedis struct {
	info    string
	infoErr bool
}

// start serves the fake on a random local port until the test ends and
// returns the port.
func (f *fakeRedis) start(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	var (
		mu    sync.Mutex
		conns []net.Conn
		wg    sync.WaitGroup
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
			wg.Add(1)
			go func() {
				defer wg.Done()
				f.serve(conn)
			}()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		for _, conn := range conns {
			_ = conn.Close()
		}
		mu.Unlock()
		wg.Wait()
	})

	return strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
}

// serve reads RESP commands from conn and writes the replies.
func (f *fakeRedis) serve(conn net.Conn) {
	r := bufio.NewReader(conn)
	for {
		args, err := readCommand(r)
		if err != nil {
			return
		}
		var reply string
		switch strings.ToUpper(args[0]) {
		case "PING":
			reply = "+PONG\r\n"
		case "INFO":
			if f.infoErr {
				reply = "-ERR injected INFO error\r\n"
			} else {
				reply = fmt.Sprintf("$%d\r\n%s\r\n", len(f.info), f.info)
			}
		default:
			reply = "-ERR unknown command\r\n"
		}
		if _, err := io.WriteString(conn, reply); err != nil {
			return
		}
	}
}

// readCommand reads one RESP array of bulk strings.
func readCommand(r *bufio.Reader) ([]string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "*")))
	if err != nil || n < 1 {
		return nil, fmt.Errorf("bad array header %q", line)
	}
	args := make([]string, n)
	for i := range args {
		header, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		size, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(header, "$")))
		if err != nil {
			return nil, fmt.Errorf("bad bulk header %q", header)
		}
		buf := make([]byte, size+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		args[i] = string(buf[:size])
	}
	return args, nil
}

// newTestClient returns a Redis client for port that closes after the test.
func newTestClient(t *testing.T, port string) *redis.Client {
	t.Helper()
	client := redis.NewClient(&redis.Options{
		Addr:        net.JoinHostPort("127.0.0.1", port),
		DialTimeout: redisConnectTimeout,
		ReadTimeout: redisCommandTimeout,
		MaxRetries:  -1,
	})
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func TestUpdateHealthStatus(t *testing.T) {
	tests := []struct {
		name        string
		redis       *fakeRedis // nil means that no Redis listens
		wantHealthy bool
		wantReady   bool
	}{
		{"redis down", nil, false, false},
		{"info fails", &fakeRedis{infoErr: true}, true, false},
		{"master", &fakeRedis{info: "# Replication\r\nrole:master\r\nloading:0\r\n"}, true, true},
		{"master loading", &fakeRedis{info: "role:master\r\nloading:1\r\n"}, true, false},
		{"replica in sync", &fakeRedis{info: "role:slave\r\nloading:0\r\nmaster_host:10.0.0.1\r\nmaster_link_status:up\r\nmaster_sync_in_progress:0\r\n"}, true, true},
		{"replica syncing", &fakeRedis{info: "role:slave\r\nmaster_host:10.0.0.1\r\nmaster_link_status:up\r\nmaster_sync_in_progress:1\r\n"}, true, false},
		{"replica link down", &fakeRedis{info: "role:slave\r\nmaster_host:10.0.0.1\r\nmaster_link_status:down\r\n"}, true, false},
		{"replica of itself", &fakeRedis{info: "role:slave\r\nmaster_host:127.0.0.1\r\nmaster_link_status:up\r\n"}, true, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Port "0" refuses connections, so PING fails.
			port := "0"
			if tt.redis != nil {
				port = tt.redis.start(t)
			}
			h := NewHealthServer(0, port, "")
			h.client = newTestClient(t, port)
			// Start from the opposite state, so that each result is a change.
			h.redisHealthy.Store(!tt.wantHealthy)
			h.redisReady.Store(!tt.wantReady)

			h.updateHealthStatus(context.Background())

			assert.Equal(t, tt.wantHealthy, h.redisHealthy.Load(), "healthy")
			assert.Equal(t, tt.wantReady, h.redisReady.Load(), "ready")
		})
	}
}

// TestHealthServerReportsReadyRedis runs the full health server against a
// fake Redis. The background checker must mark Redis healthy and ready.
func TestHealthServerReportsReadyRedis(t *testing.T) {
	port := (&fakeRedis{info: "role:master\r\nloading:0\r\nconnected_clients:3\r\n"}).start(t)
	h := NewHealthServer(0, port, "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	require.NoError(t, h.Start(ctx))
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCancel()
		assert.NoError(t, h.Stop(stopCtx))
	}()

	url := fmt.Sprintf("http://127.0.0.1:%d/readyz", h.listenAddr.(*net.TCPAddr).Port)
	var body ReadyResponse
	require.Eventually(t, func() bool {
		resp, err := http.Get(url)
		if err != nil {
			return false
		}
		defer func() { _ = resp.Body.Close() }()
		body = ReadyResponse{}
		return resp.StatusCode == http.StatusOK && json.NewDecoder(resp.Body).Decode(&body) == nil
	}, 10*time.Second, 50*time.Millisecond, "health server did not report Redis ready")

	assert.Equal(t, "ok", body.Status)
	assert.Equal(t, "master", body.Role)
	assert.Equal(t, 3, body.ConnectedClients)
	assert.True(t, h.redisHealthy.Load())
}

// TestHealthServerStopReturnsShutdownError keeps a request open and stops
// the server with a cancelled context, so that Shutdown fails.
func TestHealthServerStopReturnsShutdownError(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	h := NewHealthServer(0, "6379", "")
	h.server = &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(entered)
			<-release
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = h.server.Serve(ln)
	}()

	requested := make(chan struct{})
	go func() {
		defer close(requested)
		resp, err := http.Get("http://" + ln.Addr().String() + "/")
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-entered

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = h.Stop(ctx)

	close(release)
	<-requested
	<-served
	require.ErrorContains(t, err, "health server shutdown error")
	require.ErrorIs(t, err, context.Canceled)
}

func TestHealthServerStopReturnsClientCloseError(t *testing.T) {
	h := NewHealthServer(0, "6379", "")
	h.client = redis.NewClient(&redis.Options{Addr: "127.0.0.1:0"})
	require.NoError(t, h.client.Close())

	err := h.Stop(context.Background())

	require.ErrorContains(t, err, "redis client close error")
}

func TestHealthServerReadyzNotReadyReasons(t *testing.T) {
	tests := []struct {
		name    string
		healthy bool
		info    map[string]string
		want    string
	}{
		{"redis not responding", false, map[string]string{}, "redis not responding"},
		{"no known reason", true, map[string]string{"role": "master"}, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := NewHealthServer(8080, "6379", "")
			h.redisReady.Store(false)
			h.redisHealthy.Store(tt.healthy)
			h.mu.Lock()
			h.cachedInfo = tt.info
			h.mu.Unlock()

			w := httptest.NewRecorder()
			h.handleReadyz(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))

			assert.Equal(t, http.StatusServiceUnavailable, w.Code)
			var resp ReadyResponse
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
			assert.Equal(t, "not ready", resp.Status)
			assert.Equal(t, tt.want, resp.Error)
		})
	}
}
