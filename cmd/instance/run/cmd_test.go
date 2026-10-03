package run

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
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

	reaper := startReaper(t)

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

// startReaper runs a zombie reaper until the test ends. The cleanup waits
// for the reaper to stop, so that it cannot reap a child of the next test.
func startReaper(t *testing.T) *zombieReaper {
	t.Helper()
	reaper := newZombieReaper()
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		reaper.run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-stopped
	})
	return reaper
}

// waitForReapersToStop waits until no reaper goroutine runs. runInstance
// stops its reaper but does not wait for it, and a reaper that still runs
// can reap a child of the next test.
func waitForReapersToStop(t *testing.T) {
	t.Helper()
	require.Eventually(t, func() bool {
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		return !strings.Contains(string(buf[:n]), "(*zombieReaper).run(")
	}, 10*time.Second, 5*time.Millisecond, "a zombie reaper did not stop")
}

// useStandIn makes the process loop run "sh <script>" in place of
// "redis-server <conf>". It restores the package state after the test.
func useStandIn(t *testing.T, script string) {
	t.Helper()
	prevCommand, prevConf, prevHealthServer := redisCommand, redisConf, healthServer
	t.Cleanup(func() {
		redisCommand, redisConf, healthServer = prevCommand, prevConf, prevHealthServer
	})
	redisCommand = "sh"
	redisConf = script
	healthServer = nil
	t.Setenv("REDIS_PASSWORD", "")
}

// writeScript writes a stand-in script for Redis and returns its path.
func writeScript(t *testing.T, body string) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "redis.sh")
	require.NoError(t, os.WriteFile(script, []byte(body), 0o600))
	return script
}

// waitForPID waits until the stand-in writes its PID to pidFile.
func waitForPID(t *testing.T, pidFile string) int {
	t.Helper()
	var pid int
	require.Eventually(t, func() bool {
		data, err := os.ReadFile(pidFile)
		if err != nil {
			return false
		}
		pid, err = strconv.Atoi(strings.TrimSpace(string(data)))
		return err == nil
	}, 10*time.Second, 5*time.Millisecond, "stand-in did not write its PID")
	return pid
}

// setShutdownTimeouts makes the shutdown timeouts short for one test.
func setShutdownTimeouts(t *testing.T, graceful, total time.Duration) {
	t.Helper()
	prevGraceful, prevTotal := gracefulShutdownTimeout, maxShutdownTimeout
	t.Cleanup(func() {
		gracefulShutdownTimeout, maxShutdownTimeout = prevGraceful, prevTotal
	})
	gracefulShutdownTimeout, maxShutdownTimeout = graceful, total
}

// errOnlyContext reports an error from Err, but its Done channel never
// closes. It lets a test reach the code that runs when Redis exits after
// the context is cancelled, which a select cannot reach in a fixed order.
type errOnlyContext struct{ context.Context }

func (errOnlyContext) Err() error { return context.Canceled }

func TestRunProcessLoopReturnsStartError(t *testing.T) {
	useStandIn(t, "")
	redisCommand = filepath.Join(t.TempDir(), "no-such-redis-server")
	reaper := startReaper(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := runProcessLoop(ctx, cancel, reaper)

	require.ErrorContains(t, err, "failed to start redis-server")
}

func TestRunProcessLoopReturnsErrorOnUnexpectedExit(t *testing.T) {
	useStandIn(t, writeScript(t, "exit 3\n"))
	reaper := startReaper(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := runProcessLoop(ctx, cancel, reaper)

	require.EqualError(t, err, "redis-server exited unexpectedly: exit status 3")
}

func TestRunProcessLoopIgnoresExitErrorAfterCancel(t *testing.T) {
	useStandIn(t, writeScript(t, "exit 3\n"))
	reaper := startReaper(t)

	err := runProcessLoop(errOnlyContext{context.Background()}, func() {}, reaper)

	require.NoError(t, err)
}

func TestRunProcessLoopPassesPasswordAsArguments(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args")
	useStandIn(t, writeScript(t, "printf '%s\\n' \"$@\" > "+argsFile+"\n"))
	t.Setenv("REDIS_PASSWORD", "s3cret")
	reaper := startReaper(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	require.NoError(t, runProcessLoop(ctx, cancel, reaper))

	args, err := os.ReadFile(argsFile)
	require.NoError(t, err)
	assert.Equal(t, "--requirepass\ns3cret\n--masterauth\ns3cret\n", string(args))
}

// TestRunProcessLoopShutsDownOnSignal sends SIGTERM to the test process. The
// loop subscribes to SIGTERM before it starts the stand-in, so the PID file
// shows that the signal goes to the loop and does not stop the test.
func TestRunProcessLoopShutsDownOnSignal(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "redis.pid")
	useStandIn(t, writeScript(t, "echo $$ > "+pidFile+"\nexec sleep 30\n"))
	reaper := startReaper(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	result := make(chan error, 1)
	go func() { result <- runProcessLoop(ctx, cancel, reaper) }()

	pid := waitForPID(t, pidFile)
	require.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGTERM))

	select {
	case err := <-result:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("process loop did not shut down on SIGTERM")
	}
	assert.Equal(t, syscall.ESRCH, syscall.Kill(pid, 0), "stand-in still runs")
}

func TestRunProcessLoopShutsDownOnContextCancel(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "redis.pid")
	useStandIn(t, writeScript(t, "trap 'exit 0' TERM\necho $$ > "+pidFile+"\nwhile :; do sleep 0.01; done\n"))
	healthServer = NewHealthServer(0, "6379", "")
	reaper := startReaper(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	result := make(chan error, 1)
	go func() { result <- runProcessLoop(ctx, cancel, reaper) }()

	pid := waitForPID(t, pidFile)
	cancel()

	select {
	case err := <-result:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("process loop did not shut down on context cancel")
	}
	healthServer.mu.RLock()
	defer healthServer.mu.RUnlock()
	assert.Equal(t, pid, healthServer.redisPid)
}

func TestShutdownRedisWithoutProcess(t *testing.T) {
	require.NoError(t, shutdownRedis(&exec.Cmd{}, nil))
}

// TestShutdownRedisEscalatesToSIGKILL uses a stand-in that ignores SIGTERM.
// After the graceful timeout, shutdownRedis must kill it.
func TestShutdownRedisEscalatesToSIGKILL(t *testing.T) {
	setShutdownTimeouts(t, 50*time.Millisecond, 10*time.Second)
	pidFile := filepath.Join(t.TempDir(), "redis.pid")
	script := writeScript(t, "trap '' TERM\necho $$ > "+pidFile+"\nexec sleep 30\n")
	reaper := startReaper(t)

	cmd := exec.Command("sh", script)
	done, err := reaper.start(cmd)
	require.NoError(t, err)
	defer func() { _ = cmd.Process.Release() }()
	pid := waitForPID(t, pidFile)

	require.NoError(t, shutdownRedis(cmd, done))
	assert.Equal(t, syscall.ESRCH, syscall.Kill(pid, 0), "stand-in still runs")
}

// TestShutdownRedisFailsWhenProcessDoesNotExit gives shutdownRedis a process
// that is already reaped and a done channel that never fires. Both signals
// fail, and shutdownRedis must give up after the total timeout.
func TestShutdownRedisFailsWhenProcessDoesNotExit(t *testing.T) {
	setShutdownTimeouts(t, 10*time.Millisecond, 20*time.Millisecond)
	reaper := startReaper(t)

	cmd := exec.Command("sh", "-c", "exit 0")
	done, err := reaper.start(cmd)
	require.NoError(t, err)
	defer func() { _ = cmd.Process.Release() }()
	require.NoError(t, <-done)

	err = shutdownRedis(cmd, make(chan error))

	require.ErrorContains(t, err, "did not exit after SIGKILL")
}

// TestReaperReapsUnmanagedChild starts a child that the reaper does not
// manage, as a Redis fork for BGSAVE. The reaper must reap it.
func TestReaperReapsUnmanagedChild(t *testing.T) {
	startReaper(t)

	cmd := exec.Command("sh", "-c", "exit 0")
	require.NoError(t, cmd.Start())
	defer func() { _ = cmd.Process.Release() }()
	pid := cmd.Process.Pid

	// kill(pid, 0) works on a zombie and fails with ESRCH after the reap.
	require.Eventually(t, func() bool {
		return syscall.Kill(pid, 0) == syscall.ESRCH
	}, 10*time.Second, 5*time.Millisecond, "reaper did not reap the unmanaged child")
}

// fakeDirEntry is a directory entry that readDir can return. It lets a test
// inject faults that a real directory cannot give to the root user.
type fakeDirEntry struct {
	name    string
	info    fs.FileInfo
	infoErr error
}

func (e fakeDirEntry) Name() string               { return e.name }
func (e fakeDirEntry) IsDir() bool                { return false }
func (e fakeDirEntry) Type() fs.FileMode          { return 0 }
func (e fakeDirEntry) Info() (fs.FileInfo, error) { return e.info, e.infoErr }

// setCleanupFlags sets the cleanup flags and restores them after the test.
func setCleanupFlags(t *testing.T, dir, db string) {
	t.Helper()
	prevDataDir, prevDBFilename, prevReadDir := dataDir, dbFilename, readDir
	t.Cleanup(func() {
		dataDir, dbFilename, readDir = prevDataDir, prevDBFilename, prevReadDir
	})
	dataDir, dbFilename = dir, db
}

func TestPerformStartupCleanupRemovesOnlyStaleFiles(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"dump.rdb", "temp-1.rdb", "old.rdb", "appendonly.aof"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("data"), 0o600))
	}
	require.NoError(t, os.Mkdir(filepath.Join(dir, "backup.rdb"), 0o700))
	setCleanupFlags(t, dir, "dump.rdb")

	require.NoError(t, performStartupCleanup())

	assert.FileExists(t, filepath.Join(dir, "dump.rdb"))
	assert.FileExists(t, filepath.Join(dir, "appendonly.aof"))
	assert.DirExists(t, filepath.Join(dir, "backup.rdb"))
	assert.NoFileExists(t, filepath.Join(dir, "temp-1.rdb"))
	assert.NoFileExists(t, filepath.Join(dir, "old.rdb"))
}

func TestPerformStartupCleanupWithNoStaleFiles(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "dump.rdb"), []byte("data"), 0o600))
	setCleanupFlags(t, dir, "dump.rdb")

	require.NoError(t, performStartupCleanup())

	assert.FileExists(t, filepath.Join(dir, "dump.rdb"))
}

func TestPerformStartupCleanupSkipsMissingDirectory(t *testing.T) {
	setCleanupFlags(t, filepath.Join(t.TempDir(), "missing"), "dump.rdb")

	require.NoError(t, performStartupCleanup())
}

func TestPerformStartupCleanupReturnsErrors(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(file, nil, 0o600))

	tests := []struct {
		name    string
		dir     string
		readErr error
		want    string
	}{
		// ENOTDIR is not "does not exist", so cleanup must fail.
		{"stat error", filepath.Join(file, "data"), nil, "failed to stat data directory"},
		{"not a directory", file, nil, "is not a directory"},
		{"read error", t.TempDir(), errors.New("injected read error"), "failed to read data directory"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setCleanupFlags(t, tt.dir, "dump.rdb")
			if tt.readErr != nil {
				readDir = func(string) ([]fs.DirEntry, error) { return nil, tt.readErr }
			}

			require.ErrorContains(t, performStartupCleanup(), tt.want)
		})
	}
}

// TestPerformStartupCleanupSkipsEntriesThatFail injects an entry whose Info
// fails and an entry whose file is gone, so that Remove fails. Cleanup must
// skip both, continue, and remove the stale file that follows them.
func TestPerformStartupCleanupSkipsEntriesThatFail(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "temp-1.rdb")
	require.NoError(t, os.WriteFile(stale, []byte("stale"), 0o600))
	staleInfo, err := os.Stat(stale)
	require.NoError(t, err)

	setCleanupFlags(t, dir, "dump.rdb")
	readDir = func(string) ([]fs.DirEntry, error) {
		return []fs.DirEntry{
			fakeDirEntry{name: "no-info.rdb", infoErr: errors.New("injected info error")},
			fakeDirEntry{name: "gone.rdb", info: staleInfo},
			fakeDirEntry{name: "temp-1.rdb", info: staleInfo},
		}, nil
	}

	require.NoError(t, performStartupCleanup())

	assert.NoFileExists(t, stale)
}

func TestNewCmdRegistersFlagsWithDefaults(t *testing.T) {
	cmd := NewCmd()

	assert.Equal(t, "run", cmd.Use)
	assert.Equal(t, defaultDataDir, cmd.Flags().Lookup("data-dir").DefValue)
	assert.Equal(t, defaultDBFilename, cmd.Flags().Lookup("db-filename").DefValue)
	assert.Equal(t, defaultRedisConf, cmd.Flags().Lookup("redis-conf").DefValue)
	assert.Equal(t, strconv.Itoa(defaultHealthPort), cmd.Flags().Lookup("health-port").DefValue)
	assert.Equal(t, "6379", cmd.Flags().Lookup("redis-port").DefValue)
}

// setRunFlags runs runInstance with a stand-in script, a random health port
// and a Redis port that refuses connections. It restores the flags and
// waits for the reaper of runInstance to stop after the test.
func setRunFlags(t *testing.T, dir, script string) {
	t.Helper()
	useStandIn(t, script)
	setCleanupFlags(t, dir, "dump.rdb")
	prevHealthPort, prevRedisPort := healthPort, redisPort
	t.Cleanup(func() {
		healthPort, redisPort = prevHealthPort, prevRedisPort
		waitForReapersToStop(t)
	})
	healthPort, redisPort = 0, "0"
}

func TestRunInstanceCleansUpAndRunsRedis(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "temp-1.rdb")
	require.NoError(t, os.WriteFile(stale, []byte("stale"), 0o600))
	setRunFlags(t, dir, writeScript(t, "exit 0\n"))

	require.NoError(t, runInstance(nil, nil))

	assert.NoFileExists(t, stale)
	healthServer.mu.RLock()
	defer healthServer.mu.RUnlock()
	assert.True(t, healthServer.cleanupDone)
	assert.NotZero(t, healthServer.redisPid)
}

// TestRunInstanceStartsRedisWhenCleanupFails makes sure that a cleanup
// failure does not stop Redis. The health server reports the failure.
func TestRunInstanceStartsRedisWhenCleanupFails(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(file, nil, 0o600))
	setRunFlags(t, file, writeScript(t, "exit 0\n"))

	require.NoError(t, runInstance(nil, nil))

	healthServer.mu.RLock()
	defer healthServer.mu.RUnlock()
	assert.False(t, healthServer.cleanupDone)
	assert.NotZero(t, healthServer.redisPid)
}

func TestRunInstanceFailsWhenHealthPortIsInUse(t *testing.T) {
	occupied, err := net.Listen("tcp", ":0")
	require.NoError(t, err)
	defer func() { _ = occupied.Close() }()

	pidFile := filepath.Join(t.TempDir(), "redis.pid")
	setRunFlags(t, t.TempDir(), writeScript(t, "echo $$ > "+pidFile+"\n"))
	healthPort = occupied.Addr().(*net.TCPAddr).Port

	err = runInstance(nil, nil)

	require.ErrorContains(t, err, "failed to start health server")
	assert.NoFileExists(t, pidFile, "Redis must not start without health endpoints")
}
