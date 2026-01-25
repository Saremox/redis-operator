// Package run implements the instance manager run command.
package run

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-redis/redis/v8"
)

const (
	defaultHealthPort    = 8080
	healthCheckInterval  = time.Second
	redisConnectTimeout  = 2 * time.Second
	redisCommandTimeout  = time.Second
)

// HealthServer provides HTTP health endpoints for the instance manager.
// It maintains a persistent connection to Redis and caches health status
// to minimize load on Redis.
type HealthServer struct {
	port       int
	redisAddr  string
	redisPort  string
	server     *http.Server
	client     *redis.Client

	// Cached status (updated every healthCheckInterval)
	mu            sync.RWMutex
	lastCheck     time.Time
	cachedInfo    map[string]string
	redisPid      int
	startTime     time.Time
	cleanupDone   bool

	// Atomic flags
	redisHealthy  atomic.Bool
	redisReady    atomic.Bool
}

// HealthResponse is the response for /healthz endpoint
type HealthResponse struct {
	Status        string `json:"status"`
	RedisPID      int    `json:"redis_pid,omitempty"`
	UptimeSeconds int64  `json:"uptime_seconds"`
	Error         string `json:"error,omitempty"`
}

// ReadyResponse is the response for /readyz endpoint
type ReadyResponse struct {
	Status           string `json:"status"`
	Role             string `json:"role,omitempty"`
	ConnectedClients int    `json:"connected_clients,omitempty"`
	Loading          bool   `json:"loading"`
	MasterSyncInProgress bool `json:"master_sync_in_progress,omitempty"`
	Error            string `json:"error,omitempty"`
}

// StatusResponse is the detailed response for /status endpoint
type StatusResponse struct {
	Redis           RedisStatus           `json:"redis"`
	Replication     ReplicationStatus     `json:"replication"`
	InstanceManager InstanceManagerStatus `json:"instance_manager"`
}

// RedisStatus contains Redis server status
type RedisStatus struct {
	PID                  int    `json:"pid"`
	Role                 string `json:"role"`
	ConnectedClients     int    `json:"connected_clients"`
	UsedMemory           string `json:"used_memory"`
	UsedMemoryHuman      string `json:"used_memory_human"`
	Loading              bool   `json:"loading"`
	RDBBgsaveInProgress  bool   `json:"rdb_bgsave_in_progress"`
	AOFRewriteInProgress bool   `json:"aof_rewrite_in_progress"`
}

// ReplicationStatus contains replication information
type ReplicationStatus struct {
	Role                  string `json:"role"`
	ConnectedSlaves       int    `json:"connected_slaves,omitempty"`
	MasterHost            string `json:"master_host,omitempty"`
	MasterPort            int    `json:"master_port,omitempty"`
	MasterLinkStatus      string `json:"master_link_status,omitempty"`
	MasterSyncInProgress  bool   `json:"master_sync_in_progress,omitempty"`
	SlaveReplOffset       int64  `json:"slave_repl_offset,omitempty"`
	MasterReplOffset      int64  `json:"master_repl_offset,omitempty"`
}

// InstanceManagerStatus contains instance manager status
type InstanceManagerStatus struct {
	Version               string `json:"version"`
	UptimeSeconds         int64  `json:"uptime_seconds"`
	StartupCleanupDone    bool   `json:"startup_cleanup_done"`
	HealthPort            int    `json:"health_port"`
}

// NewHealthServer creates a new health server
func NewHealthServer(port int, redisPort string) *HealthServer {
	return &HealthServer{
		port:       port,
		redisAddr:  "127.0.0.1",
		redisPort:  redisPort,
		startTime:  time.Now(),
		cachedInfo: make(map[string]string),
	}
}

// SetRedisPID updates the Redis process ID
func (h *HealthServer) SetRedisPID(pid int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.redisPid = pid
}

// SetCleanupDone marks startup cleanup as complete
func (h *HealthServer) SetCleanupDone(done bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cleanupDone = done
}

// Start begins the health server and background health checker
func (h *HealthServer) Start(ctx context.Context) error {
	// Create Redis client
	h.client = redis.NewClient(&redis.Options{
		Addr:         net.JoinHostPort(h.redisAddr, h.redisPort),
		DialTimeout:  redisConnectTimeout,
		ReadTimeout:  redisCommandTimeout,
		WriteTimeout: redisCommandTimeout,
	})

	// Set up HTTP routes
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", h.handleHealthz)
	mux.HandleFunc("/readyz", h.handleReadyz)
	mux.HandleFunc("/status", h.handleStatus)

	h.server = &http.Server{
		Addr:         fmt.Sprintf(":%d", h.port),
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
	}

	// Start background health checker
	go h.runHealthChecker(ctx)

	// Start HTTP server
	fmt.Printf("redis-instance: starting health server on port %d\n", h.port)
	go func() {
		if err := h.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Printf("redis-instance: health server error: %v\n", err)
		}
	}()

	return nil
}

// Stop gracefully stops the health server
func (h *HealthServer) Stop(ctx context.Context) error {
	if h.server != nil {
		if err := h.server.Shutdown(ctx); err != nil {
			return fmt.Errorf("health server shutdown error: %w", err)
		}
	}
	if h.client != nil {
		if err := h.client.Close(); err != nil {
			return fmt.Errorf("redis client close error: %w", err)
		}
	}
	return nil
}

// runHealthChecker periodically checks Redis health and caches the results
func (h *HealthServer) runHealthChecker(ctx context.Context) {
	ticker := time.NewTicker(healthCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.updateHealthStatus(ctx)
		}
	}
}

// updateHealthStatus checks Redis and updates cached status
func (h *HealthServer) updateHealthStatus(ctx context.Context) {
	checkCtx, cancel := context.WithTimeout(ctx, redisCommandTimeout)
	defer cancel()

	// Try to ping Redis
	if err := h.client.Ping(checkCtx).Err(); err != nil {
		h.redisHealthy.Store(false)
		h.redisReady.Store(false)
		return
	}
	h.redisHealthy.Store(true)

	// Get INFO for detailed status
	info, err := h.client.Info(checkCtx).Result()
	if err != nil {
		h.redisReady.Store(false)
		return
	}

	// Parse INFO output
	h.mu.Lock()
	h.cachedInfo = parseRedisInfo(info)
	h.lastCheck = time.Now()
	h.mu.Unlock()

	// Determine readiness
	// Not ready if: loading, syncing from master with no master link
	loading := h.cachedInfo["loading"] == "1"
	syncInProgress := h.cachedInfo["master_sync_in_progress"] == "1"
	masterLinkDown := h.cachedInfo["master_link_status"] == "down"

	// Replica is not ready if syncing or master link is down
	isReplica := h.cachedInfo["role"] == "slave"

	ready := !loading
	if isReplica {
		ready = ready && !syncInProgress && !masterLinkDown
	}

	h.redisReady.Store(ready)
}

// handleHealthz handles the /healthz liveness endpoint
func (h *HealthServer) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	h.mu.RLock()
	pid := h.redisPid
	h.mu.RUnlock()

	uptime := int64(time.Since(h.startTime).Seconds())

	resp := HealthResponse{
		RedisPID:      pid,
		UptimeSeconds: uptime,
	}

	if h.redisHealthy.Load() {
		resp.Status = "ok"
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
	} else {
		resp.Status = "unhealthy"
		resp.Error = "redis not responding to PING"
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
	}

	json.NewEncoder(w).Encode(resp)
}

// handleReadyz handles the /readyz readiness endpoint
func (h *HealthServer) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	h.mu.RLock()
	info := h.cachedInfo
	h.mu.RUnlock()

	resp := ReadyResponse{
		Role:                 info["role"],
		Loading:              info["loading"] == "1",
		MasterSyncInProgress: info["master_sync_in_progress"] == "1",
	}

	if clients, ok := info["connected_clients"]; ok {
		fmt.Sscanf(clients, "%d", &resp.ConnectedClients)
	}

	if h.redisReady.Load() {
		resp.Status = "ok"
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
	} else {
		resp.Status = "not ready"
		if resp.Loading {
			resp.Error = "redis is loading data"
		} else if resp.MasterSyncInProgress {
			resp.Error = "replica sync in progress"
		} else if info["master_link_status"] == "down" {
			resp.Error = "master link is down"
		} else if !h.redisHealthy.Load() {
			resp.Error = "redis not responding"
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
	}

	json.NewEncoder(w).Encode(resp)
}

// handleStatus handles the /status detailed status endpoint
func (h *HealthServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	h.mu.RLock()
	info := h.cachedInfo
	pid := h.redisPid
	cleanupDone := h.cleanupDone
	h.mu.RUnlock()

	resp := StatusResponse{
		Redis: RedisStatus{
			PID:                  pid,
			Role:                 info["role"],
			UsedMemory:           info["used_memory"],
			UsedMemoryHuman:      info["used_memory_human"],
			Loading:              info["loading"] == "1",
			RDBBgsaveInProgress:  info["rdb_bgsave_in_progress"] == "1",
			AOFRewriteInProgress: info["aof_rewrite_in_progress"] == "1",
		},
		Replication: ReplicationStatus{
			Role:                 info["role"],
			MasterHost:           info["master_host"],
			MasterLinkStatus:     info["master_link_status"],
			MasterSyncInProgress: info["master_sync_in_progress"] == "1",
		},
		InstanceManager: InstanceManagerStatus{
			Version:            "v1.7.0",
			UptimeSeconds:      int64(time.Since(h.startTime).Seconds()),
			StartupCleanupDone: cleanupDone,
			HealthPort:         h.port,
		},
	}

	// Parse integer fields
	fmt.Sscanf(info["connected_clients"], "%d", &resp.Redis.ConnectedClients)
	fmt.Sscanf(info["connected_slaves"], "%d", &resp.Replication.ConnectedSlaves)
	fmt.Sscanf(info["master_port"], "%d", &resp.Replication.MasterPort)
	fmt.Sscanf(info["slave_repl_offset"], "%d", &resp.Replication.SlaveReplOffset)
	fmt.Sscanf(info["master_repl_offset"], "%d", &resp.Replication.MasterReplOffset)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(resp)
}

// parseRedisInfo parses Redis INFO command output into a map
func parseRedisInfo(info string) map[string]string {
	result := make(map[string]string)
	lines := splitLines(info)

	for _, line := range lines {
		// Skip comments and empty lines
		if len(line) == 0 || line[0] == '#' {
			continue
		}

		// Parse key:value
		idx := indexByte(line, ':')
		if idx > 0 {
			key := line[:idx]
			value := line[idx+1:]
			result[key] = value
		}
	}

	return result
}

// splitLines splits a string by newlines
func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			line := s[start:i]
			// Remove trailing \r if present
			if len(line) > 0 && line[len(line)-1] == '\r' {
				line = line[:len(line)-1]
			}
			lines = append(lines, line)
			start = i + 1
		}
	}
	if start < len(s) {
		line := s[start:]
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}
		lines = append(lines, line)
	}
	return lines
}

// indexByte returns the index of the first occurrence of c in s, or -1
func indexByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}
