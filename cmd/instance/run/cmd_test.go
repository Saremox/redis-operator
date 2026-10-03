package run

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestRunProcessLoopReportsCleanExitWhenReaperReapsRedis makes the zombie
// reaper see the Redis exit before the process loop waits for it. The loop
// must still report a clean exit and not "no child processes".
func TestRunProcessLoopReportsCleanExitWhenReaperReapsRedis(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "redis.pid")
	script := filepath.Join(dir, "redis.sh")
	require.NoError(t, os.WriteFile(script, []byte("echo $$ > "+pidFile+"\nexit 0\n"), 0o600))

	// Run "sh <script>" in place of "redis-server <conf>".
	prevCommand, prevConf, prevHealthServer := redisCommand, redisConf, healthServer
	t.Cleanup(func() {
		redisCommand, redisConf, healthServer = prevCommand, prevConf, prevHealthServer
	})
	redisCommand = "sh"
	redisConf = script
	t.Setenv("REDIS_PASSWORD", "")

	// The loop calls SetRedisPID after it starts Redis and before it waits.
	// Hold the lock so that the loop stops there until the reaper is done.
	healthServer = NewHealthServer(0, "6379", "")
	healthServer.mu.Lock()
	locked := true
	defer func() {
		if locked {
			healthServer.mu.Unlock()
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	reaper := newZombieReaper()
	go reaper.run(ctx)

	result := make(chan error, 1)
	go func() {
		result <- runProcessLoop(ctx, cancel, reaper)
	}()

	var pid int
	require.Eventually(t, func() bool {
		data, err := os.ReadFile(pidFile)
		if err != nil {
			return false
		}
		pid, err = strconv.Atoi(strings.TrimSpace(string(data)))
		return err == nil
	}, 10*time.Second, 5*time.Millisecond, "managed process did not write its PID")

	// kill(pid, 0) works on a zombie and fails with ESRCH after the reap.
	require.Eventually(t, func() bool {
		return syscall.Kill(pid, 0) == syscall.ESRCH
	}, 10*time.Second, 5*time.Millisecond, "reaper did not reap the managed process")

	healthServer.mu.Unlock()
	locked = false

	select {
	case err := <-result:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("process loop did not see the managed process exit")
	}
}

func TestWaitStatusError(t *testing.T) {
	tests := []struct {
		name   string
		status syscall.WaitStatus
		want   string
	}{
		{"clean exit", syscall.WaitStatus(0), ""},
		{"exit code", syscall.WaitStatus(3 << 8), "exit status 3"},
		{"signal", syscall.WaitStatus(syscall.SIGKILL), "signal: killed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := waitStatusError(tt.status)
			if tt.want == "" {
				require.NoError(t, err)
				return
			}
			require.EqualError(t, err, tt.want)
		})
	}
}
