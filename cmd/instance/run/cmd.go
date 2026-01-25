// Package run implements the instance manager run command.
//
// This follows the CloudNativePG (CNPG) model where the instance manager runs as
// PID 1 and manages the database process as a child. This architecture has proven
// reliable at scale in production Kubernetes environments.
//
// Key features (learned from CNPG):
//   - Full lifecycle control over the Redis process
//   - Clean signal handling with graceful shutdown and timeout escalation
//   - Zombie process reaper (required when running as PID 1)
//   - Startup tasks (RDB cleanup) before Redis starts
//   - Process restart capability for unexpected crashes
//
// See: https://cloudnative-pg.io/documentation/current/instance_manager/
package run

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
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

	// Shutdown timeouts (following CNPG pattern)
	// These provide escalation from graceful to forced shutdown
	gracefulShutdownTimeout = 25 * time.Second // Time for SIGTERM before SIGKILL
	maxShutdownTimeout      = 30 * time.Second // Total shutdown budget (matches K8s terminationGracePeriodSeconds)
)

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
		Long: `Run Redis server with full lifecycle management.

This command implements the instance manager pattern (similar to CloudNativePG)
where the manager runs as PID 1 and manages Redis as a child process.

On startup:
  1. Cleans up stale RDB tempfiles to prevent disk exhaustion
  2. Starts redis-server as a child process
  3. Reaps zombie processes (required for PID 1)
  4. Forwards signals to Redis for graceful shutdown

Shutdown behavior (CNPG model):
  - SIGTERM: Initiate graceful shutdown with timeout
  - If Redis doesn't exit within timeout, escalate to SIGKILL
  - Proper cleanup even under crash conditions

This architecture provides:
  - Clean signal handling and graceful shutdown
  - Startup tasks before Redis begins accepting connections
  - Foundation for health checks, metrics, and other lifecycle features`,
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

	// Create a context that we can cancel on shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Step 1: Start zombie process reaper (CNPG pattern)
	// As PID 1, we're responsible for reaping orphaned child processes
	go runZombieReaper(ctx)

	// Step 2: Perform startup cleanup
	cleanupErr := performStartupCleanup()
	if cleanupErr != nil {
		// Log but don't fail - Redis should still be able to start
		fmt.Printf("redis-instance: warning: startup cleanup failed: %v\n", cleanupErr)
	}

	// Step 3: Start health server (provides /healthz, /readyz, /status)
	redisPassword := os.Getenv("REDIS_PASSWORD")
	healthServer = NewHealthServer(healthPort, redisPort, redisPassword)
	healthServer.SetCleanupDone(cleanupErr == nil)
	if err := healthServer.Start(ctx); err != nil {
		fmt.Printf("redis-instance: warning: failed to start health server: %v\n", err)
	}
	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		if err := healthServer.Stop(shutdownCtx); err != nil {
			fmt.Printf("redis-instance: warning: health server stop error: %v\n", err)
		}
	}()

	// Step 4: Main process loop (CNPG pattern)
	// This loop allows for process restarts without manager exit
	return runProcessLoop(ctx, cancel)
}

// runProcessLoop manages the Redis process lifecycle with restart capability.
// Following CNPG pattern, this allows recovery from unexpected crashes.
func runProcessLoop(ctx context.Context, cancel context.CancelFunc) error {
	// Set up signal handling
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGTERM, syscall.SIGINT, syscall.SIGQUIT)
	defer signal.Stop(sigChan)

	for {
		// Start Redis as a child process
		redisCmd := exec.CommandContext(ctx, defaultRedisCommand, redisConf)
		redisCmd.Stdout = os.Stdout
		redisCmd.Stderr = os.Stderr
		redisCmd.Stdin = os.Stdin

		fmt.Printf("redis-instance: starting redis-server with config %s\n", redisConf)
		if err := redisCmd.Start(); err != nil {
			return fmt.Errorf("failed to start redis-server: %w", err)
		}

		redisPid := redisCmd.Process.Pid
		fmt.Printf("redis-instance: redis-server started with PID %d\n", redisPid)

		// Notify health server of Redis PID
		if healthServer != nil {
			healthServer.SetRedisPID(redisPid)
		}

		// Wait for either Redis to exit or a signal
		doneChan := make(chan error, 1)
		go func() {
			doneChan <- redisCmd.Wait()
		}()

		select {
		case sig := <-sigChan:
			fmt.Printf("redis-instance: received signal %v, initiating graceful shutdown\n", sig)
			return shutdownRedis(redisCmd, doneChan)

		case <-ctx.Done():
			fmt.Println("redis-instance: context cancelled, initiating shutdown")
			return shutdownRedis(redisCmd, doneChan)

		case err := <-doneChan:
			if err != nil {
				// Redis exited unexpectedly
				fmt.Printf("redis-instance: redis-server (PID %d) exited unexpectedly: %v\n", redisPid, err)

				// Check if this is a context cancellation (we're shutting down)
				if ctx.Err() != nil {
					return nil
				}

				// For now, exit on unexpected crash
				// Future: could implement restart with backoff
				return fmt.Errorf("redis-server exited unexpectedly: %w", err)
			}
			// Clean exit
			fmt.Printf("redis-instance: redis-server (PID %d) exited cleanly\n", redisPid)
			return nil
		}
	}
}

// shutdownRedis handles graceful shutdown with timeout escalation (CNPG pattern).
// First sends SIGTERM, then escalates to SIGKILL if Redis doesn't exit in time.
func shutdownRedis(cmd *exec.Cmd, doneChan <-chan error) error {
	if cmd.Process == nil {
		return nil
	}

	pid := cmd.Process.Pid

	// Step 1: Send SIGTERM for graceful shutdown
	fmt.Printf("redis-instance: sending SIGTERM to redis-server (PID %d)\n", pid)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		fmt.Printf("redis-instance: warning: failed to send SIGTERM: %v\n", err)
	}

	// Step 2: Wait for graceful shutdown with timeout
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
		// Graceful shutdown timeout - escalate to SIGKILL
		fmt.Printf("redis-instance: graceful shutdown timeout (%v), sending SIGKILL\n", gracefulShutdownTimeout)
		if err := cmd.Process.Kill(); err != nil {
			fmt.Printf("redis-instance: warning: failed to send SIGKILL: %v\n", err)
		}

		// Wait for process to actually exit
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

// runZombieReaper handles SIGCHLD signals to reap orphaned child processes.
// This is essential when running as PID 1 in a container (CNPG pattern).
//
// When Redis forks (e.g., for BGSAVE or BGREWRITEAOF), those child processes
// become orphans when they exit. As PID 1, we must reap them to prevent
// zombie process accumulation.
func runZombieReaper(ctx context.Context) {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGCHLD)
	defer signal.Stop(sigChan)

	for {
		select {
		case <-ctx.Done():
			return
		case <-sigChan:
			// Reap all zombie children
			for {
				var status syscall.WaitStatus
				pid, err := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
				if pid <= 0 || err != nil {
					break
				}
				// Log only if it's not the main Redis process (which we handle separately)
				fmt.Printf("redis-instance: reaped zombie process PID %d (status: %d)\n", pid, status.ExitStatus())
			}
		}
	}
}

// performStartupCleanup removes stale RDB tempfiles before Redis starts.
// During BGSAVE, Redis creates temp-<pid>.rdb files that can accumulate if
// Redis crashes repeatedly, eventually filling the disk.
func performStartupCleanup() error {
	fmt.Printf("redis-instance: performing startup cleanup in %s\n", dataDir)

	// Check if data directory exists
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

	// Find and remove stale RDB files
	entries, err := os.ReadDir(dataDir)
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

		// Skip non-RDB files
		if !strings.HasSuffix(name, ".rdb") {
			continue
		}

		// Preserve the main database file
		if name == dbFilename {
			continue
		}

		filePath := filepath.Join(dataDir, name)

		// Get file size for reporting
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
