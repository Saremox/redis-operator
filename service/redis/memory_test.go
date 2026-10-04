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

// fillRedis writes n 1KiB keys.
func fillRedis(t *testing.T, rc *rediscli.Client, n int) {
	t.Helper()
	value := strings.Repeat("x", 1024)
	pipe := rc.Pipeline()
	for i := 0; i < n; i++ {
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
