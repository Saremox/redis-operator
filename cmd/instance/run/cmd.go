// Package run implements the run command of the instance manager. The
// manager runs as PID 1, so it reaps zombie processes. It removes old RDB
// tempfiles before Redis starts, and it sends SIGKILL when Redis does not stop
// after SIGTERM. It does not restart Redis: when Redis exits, the manager
// exits too.
package run

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

// healthServer is the global health server instance
var healthServer *HealthServer

const (
	defaultDataDir      = "/data"
	defaultDBFilename   = "dump.rdb"
	defaultRedisConf    = "/redis/redis.conf"
	defaultRedisCommand = "redis-server"
)

// The shutdown sends SIGKILL after gracefulShutdownTimeout. maxShutdownTimeout
// is the default Kubernetes grace period of 30s. They are variables so that
// tests can make them short.
var (
	gracefulShutdownTimeout = 25 * time.Second
	maxShutdownTimeout      = 30 * time.Second
)

// redisCommand is a variable so that tests can run a stand-in process.
var redisCommand = defaultRedisCommand

// readDir is a variable so that tests can inject directory read faults.
var readDir = os.ReadDir

var (
	dataDir    string
	dbFilename string
	redisConf  string
	healthPort int
	redisPort  string
)

// NewCmd creates the run command
func NewCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run Redis with instance management",
		Long: `Run redis-server as a child process of this manager, which runs as PID 1.

At start, the command:
  1. Removes the .rdb files other than --db-filename, so that old
     tempfiles do not fill the disk.
  2. Starts the health server.
  3. Starts redis-server as a child process.

The manager reaps zombie processes, because it is PID 1. On SIGTERM, SIGINT
or SIGQUIT, it sends SIGTERM to Redis, and SIGKILL after 25s. The manager
does not restart Redis: when Redis exits, the manager exits too.`,
		RunE: runInstance,
	}

	cmd.Flags().StringVar(&dataDir, "data-dir", defaultDataDir, "Redis data directory")
	cmd.Flags().StringVar(&dbFilename, "db-filename", defaultDBFilename, "Main RDB filename to preserve during cleanup")
	cmd.Flags().StringVar(&redisConf, "redis-conf", defaultRedisConf, "Path to redis.conf")
	cmd.Flags().IntVar(&healthPort, "health-port", defaultHealthPort, "Port for health check endpoints")
	cmd.Flags().StringVar(&redisPort, "redis-port", "6379", "Redis port for health checks")

	return cmd
}

func runInstance(cmd *cobra.Command, args []string) error {
	fmt.Println("redis-instance: starting instance manager (CNPG-style)")
	fmt.Printf("redis-instance: PID %d running as process manager\n", os.Getpid())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// PID 1 must reap the orphaned child processes. The reaper also reaps Redis. It must run until the process loop
	// returns, because shutdownRedis needs it to see the Redis exit.
	reaper := newZombieReaper()
	reaperCtx, stopReaper := context.WithCancel(context.Background())
	defer stopReaper()
	go reaper.run(reaperCtx)

	cleanupErr := performStartupCleanup()
	if cleanupErr != nil {
		// Redis can start without the cleanup, so only log the error.
		fmt.Printf("redis-instance: warning: startup cleanup failed: %v\n", cleanupErr)
	}

	// The health server serves /healthz, /readyz and /status.
	redisPassword := os.Getenv("REDIS_PASSWORD")
	healthServer = NewHealthServer(healthPort, redisPort, redisPassword)
	healthServer.SetCleanupDone(cleanupErr == nil)
	if err := healthServer.Start(ctx); err != nil {
		// Without the health endpoints the probes fail for ever, so stop here.
		return fmt.Errorf("failed to start health server: %w", err)
	}
	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		if err := healthServer.Stop(shutdownCtx); err != nil {
			fmt.Printf("redis-instance: warning: health server stop error: %v\n", err)
		}
	}()

	return runProcessLoop(ctx, cancel, reaper)
}

// runProcessLoop starts Redis and returns when Redis exits, or after the
// shutdown on a signal. Each branch returns, so the loop does not restart
// Redis.
func runProcessLoop(ctx context.Context, cancel context.CancelFunc, reaper *zombieReaper) error {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGTERM, syscall.SIGINT, syscall.SIGQUIT)
	defer signal.Stop(sigChan)

	for {
		// The password is in no ConfigMap, so REDIS_PASSWORD gives
		// requirepass and masterauth as arguments.
		redisArgs := []string{redisConf}
		if pw := os.Getenv("REDIS_PASSWORD"); pw != "" {
			redisArgs = append(redisArgs, "--requirepass", pw, "--masterauth", pw)
		}
		// The reaper reaps Redis, so nothing calls redisCmd.Wait. Thus the
		// command has no context: CommandContext needs Wait to stop its watch
		// goroutine, and shutdownRedis already handles ctx cancellation.
		redisCmd := exec.Command(redisCommand, redisArgs...)
		redisCmd.Stdout = os.Stdout
		redisCmd.Stderr = os.Stderr
		redisCmd.Stdin = os.Stdin

		fmt.Printf("redis-instance: starting redis-server with config %s\n", redisConf)
		doneChan, err := reaper.start(redisCmd)
		if err != nil {
			return fmt.Errorf("failed to start redis-server: %w", err)
		}
		// Release replaces Wait for the process handle. Stdin, Stdout and
		// Stderr are *os.File values, so the Cmd holds no pipes and no copy
		// goroutines that Wait must close.
		defer func() { _ = redisCmd.Process.Release() }()

		redisPid := redisCmd.Process.Pid
		fmt.Printf("redis-instance: redis-server started with PID %d\n", redisPid)

		if healthServer != nil {
			healthServer.SetRedisPID(redisPid)
		}

		select {
		case sig := <-sigChan:
			fmt.Printf("redis-instance: received signal %v, initiating graceful shutdown\n", sig)
			return shutdownRedis(redisCmd, doneChan)

		case <-ctx.Done():
			fmt.Println("redis-instance: context cancelled, initiating shutdown")
			return shutdownRedis(redisCmd, doneChan)

		case err := <-doneChan:
			if err != nil {
				fmt.Printf("redis-instance: redis-server (PID %d) exited unexpectedly: %v\n", redisPid, err)

				// After a cancel, the exit is part of the shutdown.
				if ctx.Err() != nil {
					return nil
				}

				return fmt.Errorf("redis-server exited unexpectedly: %w", err)
			}
			fmt.Printf("redis-instance: redis-server (PID %d) exited cleanly\n", redisPid)
			return nil
		}
	}
}

// shutdownRedis sends SIGTERM to Redis, and SIGKILL when Redis does not exit
// in gracefulShutdownTimeout.
func shutdownRedis(cmd *exec.Cmd, doneChan <-chan error) error {
	if cmd.Process == nil {
		return nil
	}

	pid := cmd.Process.Pid

	fmt.Printf("redis-instance: sending SIGTERM to redis-server (PID %d)\n", pid)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		fmt.Printf("redis-instance: warning: failed to send SIGTERM: %v\n", err)
	}

	gracefulTimer := time.NewTimer(gracefulShutdownTimeout)
	defer gracefulTimer.Stop()

	select {
	case err := <-doneChan:
		if err != nil {
			fmt.Printf("redis-instance: redis-server exited with error during shutdown: %v\n", err)
		} else {
			fmt.Println("redis-instance: redis-server exited gracefully")
		}
		return nil

	case <-gracefulTimer.C:
		fmt.Printf("redis-instance: graceful shutdown timeout (%v), sending SIGKILL\n", gracefulShutdownTimeout)
		if err := cmd.Process.Kill(); err != nil {
			fmt.Printf("redis-instance: warning: failed to send SIGKILL: %v\n", err)
		}

		maxTimer := time.NewTimer(maxShutdownTimeout - gracefulShutdownTimeout)
		defer maxTimer.Stop()

		select {
		case <-doneChan:
			fmt.Println("redis-instance: redis-server terminated after SIGKILL")
			return nil
		case <-maxTimer.C:
			return fmt.Errorf("redis-server (PID %d) did not exit after SIGKILL", pid)
		}
	}
}

// zombieReaper reaps the child processes on SIGCHLD. In a container, PID 1
// must reap the orphaned processes, or the zombie processes collect.
//
// The reaper is the only caller of wait4 in this process. wait4(-1) also
// reaps the Redis process, so a second waiter such as exec.Cmd.Wait can lose
// the Redis exit status and fail with ECHILD. Start managed processes with
// start, and do not call Wait on them.
type zombieReaper struct {
	sigChan chan os.Signal

	mu      sync.Mutex
	waiters map[int]chan error
}

// newZombieReaper subscribes to SIGCHLD before any child starts, so that
// the reaper cannot miss a child exit.
func newZombieReaper() *zombieReaper {
	r := &zombieReaper{
		sigChan: make(chan os.Signal, 1),
		waiters: make(map[int]chan error),
	}
	signal.Notify(r.sigChan, syscall.SIGCHLD)
	return r
}

// start starts cmd and returns a channel that receives its exit result.
// The lock keeps the reaper from reaping the child before its PID is known.
func (r *zombieReaper) start(cmd *exec.Cmd) (<-chan error, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if err := cmd.Start(); err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	r.waiters[cmd.Process.Pid] = done
	return done, nil
}

// run reaps children on each SIGCHLD until ctx is done.
func (r *zombieReaper) run(ctx context.Context) {
	defer signal.Stop(r.sigChan)

	for {
		select {
		case <-ctx.Done():
			return
		case <-r.sigChan:
			r.reapAll()
		}
	}
}

// reapAll reaps all zombie children and sends the exit result of each
// managed process to its waiter.
func (r *zombieReaper) reapAll() {
	r.mu.Lock()
	defer r.mu.Unlock()

	for {
		var status syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
		if err == syscall.EINTR {
			continue
		}
		if pid <= 0 || err != nil {
			return
		}
		if done, ok := r.waiters[pid]; ok {
			delete(r.waiters, pid)
			done <- waitStatusError(status)
			continue
		}
		fmt.Printf("redis-instance: reaped zombie process PID %d (status: %d)\n", pid, status.ExitStatus())
	}
}

// waitStatusError returns the error that exec.Cmd.Wait gives for status.
func waitStatusError(status syscall.WaitStatus) error {
	switch {
	case status.Exited() && status.ExitStatus() == 0:
		return nil
	case status.Signaled():
		return fmt.Errorf("signal: %v", status.Signal())
	default:
		return fmt.Errorf("exit status %d", status.ExitStatus())
	}
}

// performStartupCleanup removes the .rdb files other than dbFilename before
// Redis starts. A BGSAVE writes temp-<pid>.rdb, and after each crash during a
// save, one more such file stays until the disk is full.
func performStartupCleanup() error {
	fmt.Printf("redis-instance: performing startup cleanup in %s\n", dataDir)

	info, err := os.Stat(dataDir)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Printf("redis-instance: data directory %s does not exist yet, skipping cleanup\n", dataDir)
			return nil
		}
		return fmt.Errorf("failed to stat data directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", dataDir)
	}

	entries, err := readDir(dataDir)
	if err != nil {
		return fmt.Errorf("failed to read data directory: %w", err)
	}

	var cleaned int
	var totalSize int64

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		name := entry.Name()

		if !strings.HasSuffix(name, ".rdb") {
			continue
		}

		if name == dbFilename {
			continue
		}

		filePath := filepath.Join(dataDir, name)

		fileInfo, err := entry.Info()
		if err != nil {
			fmt.Printf("redis-instance: warning: failed to get info for %s: %v\n", name, err)
			continue
		}

		if err := os.Remove(filePath); err != nil {
			fmt.Printf("redis-instance: warning: failed to remove %s: %v\n", filePath, err)
			continue
		}

		fmt.Printf("redis-instance: removed stale RDB file %s (%d bytes)\n", name, fileInfo.Size())
		cleaned++
		totalSize += fileInfo.Size()
	}

	if cleaned > 0 {
		fmt.Printf("redis-instance: cleaned up %d stale RDB file(s), freed %d bytes\n", cleaned, totalSize)
	} else {
		fmt.Println("redis-instance: no stale RDB files found")
	}

	return nil
}
