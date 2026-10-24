package redis

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	rediscli "github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// oomTestMargin is how far TestGetAutoscalerInfo puts maxmemory below the
// memory in use, so that the next write is over the limit.
const oomTestMargin = 256 * 1024

// fillRedis writes n 1KiB keys.
func fillRedis(t *testing.T, rc *rediscli.Client, n int) {
	t.Helper()
	value := strings.Repeat("x", 1024)
	pipe := rc.Pipeline()
	for i := range n {
		pipe.Set(bgCtx(), fmt.Sprintf("key:%d", i), value, 0)
	}
	_, err := pipe.Exec(bgCtx())
	require.NoError(t, err)
}

func TestGetMemoryInfo(t *testing.T) {
	requireRedisServer(t)
	r := startRedisProcess(t)
	rc := rediscli.NewClient(&rediscli.Options{Addr: r.Addr()})
	defer func() { _ = rc.Close() }()
	fillRedis(t, rc, 100)
	c := newTestClient()
	require.NoError(t, c.SetCustomRedisConfig(r.IP, strconv.Itoa(r.Port), []string{"maxmemory 104857600", "maxmemory-policy allkeys-lru"}, ""))

	mi, err := c.GetMemoryInfo(r.IP, strconv.Itoa(r.Port), "")
	require.NoError(t, err)
	assert.Equal(t, int64(104857600), mi.MaxMemory)
	assert.Equal(t, "allkeys-lru", mi.MaxMemoryPolicy)
	assert.Equal(t, "master", mi.Role)
	assert.Greater(t, mi.UsedMemory, int64(200*1024))
	assert.Greater(t, mi.UsedMemoryRSS, mi.UsedMemory)
}

func TestGetMemoryInfo_ConnectionError(t *testing.T) {
	port, err := findFreePort()
	require.NoError(t, err)
	_, err = newTestClient().GetMemoryInfo(testLoopbackIP, strconv.Itoa(port), "")
	assert.Error(t, err)
}

// Lowering maxmemory below the memory in use makes Redis evict right away
// under allkeys-*; EnsureRedisMaxMemory relies on that.
func TestLoweringMaxMemoryEvicts(t *testing.T) {
	requireRedisServer(t)
	r := startRedisProcess(t)
	rc := rediscli.NewClient(&rediscli.Options{Addr: r.Addr()})
	defer func() { _ = rc.Close() }()
	fillRedis(t, rc, 4000)
	c := newTestClient()
	port := strconv.Itoa(r.Port)
	before, err := c.GetMemoryInfo(r.IP, port, "")
	require.NoError(t, err)
	target := before.UsedMemory - 1<<20
	require.NoError(t, c.SetCustomRedisConfig(r.IP, port, []string{"maxmemory-policy allkeys-lru", fmt.Sprintf("maxmemory %d", target)}, ""))

	assert.Eventually(t, func() bool {
		after, err := c.GetMemoryInfo(r.IP, port, "")
		return err == nil && after.UsedMemory <= target
	}, time.Second, 50*time.Millisecond)
}

func TestGetMemoryInfoWhileLoading(t *testing.T) {
	requireRedisServer(t)
	r := startRedisProcess(t, "--enable-debug-command", "yes", "--key-load-delay", "500", "--loading-process-events-interval-bytes", "1024")
	rc := rediscli.NewClient(&rediscli.Options{Addr: r.Addr()})
	defer func() { _ = rc.Close() }()
	fillRedis(t, rc, 2000)
	c := newTestClient()
	port := strconv.Itoa(r.Port)
	mi, err := c.GetMemoryInfo(r.IP, port, "")
	require.NoError(t, err)
	assert.False(t, mi.Loading)

	reloaded := make(chan error, 1)
	go func() { reloaded <- rc.Do(bgCtx(), "debug", "reload").Err() }()
	assert.Eventually(t, func() bool {
		mi, err := c.GetMemoryInfo(r.IP, port, "")
		return err == nil && mi.Loading
	}, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, <-reloaded)
}

func TestGetAutoscalerInfo(t *testing.T) {
	requireRedisServer(t)
	r := startRedisProcess(t)
	rc := rediscli.NewClient(&rediscli.Options{Addr: r.Addr()})
	defer func() { _ = rc.Close() }()
	fillRedis(t, rc, 1000)
	c := newTestClient()
	port := strconv.Itoa(r.Port)

	before, err := c.GetAutoscalerInfo(r.IP, port, "")
	require.NoError(t, err)
	assert.Equal(t, "master", before.Role)
	assert.Len(t, before.RunID, 40)
	assert.Equal(t, "noeviction", before.MaxMemoryPolicy)
	assert.Zero(t, before.MaxMemory)
	assert.Greater(t, before.UsedMemory, int64(100*1024))
	assert.False(t, before.Loading)
	assert.False(t, before.SyncInProgress)
	assert.Zero(t, before.OOMErrors)

	// A write above maxmemory without a key to evict counts in errorstat_OOM.
	//
	// The limit comes from the memory in use, not from a constant. Most of
	// the memory of a small server is its own baseline, and that differs
	// between the builds. A constant that one build passes, another build
	// stays under, and then the write below gets no OOM.
	//
	// The margin is smaller than the dataset, so the eviction below frees
	// enough, and larger than the jitter between two INFO calls.
	maxMemory := before.UsedMemory - oomTestMargin
	require.NoError(t, c.SetCustomRedisConfig(r.IP, port, []string{fmt.Sprintf("maxmemory %d", maxMemory), "maxmemory-policy volatile-lru"}, ""))
	require.ErrorContains(t, rc.Set(bgCtx(), "k", "v", 0).Err(), "OOM")
	after, err := c.GetAutoscalerInfo(r.IP, port, "")
	require.NoError(t, err)
	assert.Equal(t, int64(1), after.OOMErrors)
	assert.Equal(t, maxMemory, after.MaxMemory)
	assert.Equal(t, "volatile-lru", after.MaxMemoryPolicy)
	assert.Equal(t, before.RunID, after.RunID)
	assert.Zero(t, after.EvictedKeys)

	require.NoError(t, c.SetCustomRedisConfig(r.IP, port, []string{"maxmemory-policy allkeys-lru"}, ""))
	require.NoError(t, rc.Set(bgCtx(), "k", "v", 0).Err())
	assert.Eventually(t, func() bool {
		ai, err := c.GetAutoscalerInfo(r.IP, port, "")
		return err == nil && ai.EvictedKeys > 0 && ai.UptimeSeconds > 0
	}, 3*time.Second, 100*time.Millisecond)

	// Only a replica has master_sync_in_progress.
	replica := startReplicaOf(t, r)
	assert.Eventually(t, func() bool {
		ai, err := c.GetAutoscalerInfo(replica.IP, strconv.Itoa(replica.Port), "")
		return err == nil && ai.Role == "slave" && !ai.SyncInProgress
	}, 5*time.Second, 100*time.Millisecond)
}

// maxmemory does not count the replica output buffers and the AOF buffer, so
// the ratio must not count them either.
func TestParseAutoscalerInfo(t *testing.T) {
	info := "# Server\r\nrun_id:abc\r\nuptime_in_seconds:700\r\n\r\n# Memory\r\nused_memory:5000000\r\nmaxmemory:8000000\r\n" +
		"maxmemory_policy:volatile-lru\r\nmem_not_counted_for_evict:1000000\r\n\r\n# Persistence\r\nloading:0\r\n\r\n" +
		"# Stats\r\nevicted_keys:4\r\n\r\n# Replication\r\nrole:slave\r\nmaster_sync_in_progress:1\r\n\r\n# Errorstats\r\nerrorstat_OOM:count=3\r\n"
	assert.Equal(t, &AutoscalerInfo{
		UsedMemory:      4000000,
		MaxMemory:       8000000,
		MaxMemoryPolicy: "volatile-lru",
		EvictedKeys:     4,
		UptimeSeconds:   700,
		SyncInProgress:  true,
		RunID:           "abc",
		Role:            "slave",
		OOMErrors:       3,
	}, parseAutoscalerInfo(info))
}

func TestGetAutoscalerInfoErrors(t *testing.T) {
	port, err := findFreePort()
	require.NoError(t, err)
	_, err = newTestClient().GetAutoscalerInfo(testLoopbackIP, strconv.Itoa(port), "")
	require.Error(t, err)
	_, err = newTestClient().GetAutoscalerInfo("", strconv.Itoa(port), "")
	assert.ErrorIs(t, err, errNoIP)
}
