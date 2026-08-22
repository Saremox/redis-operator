package redis

// This file provides a small test harness that spins up real redis-server
// (and redis-server --sentinel) subprocesses so the tests in client_test.go
// exercise the actual Redis/Sentinel wire protocol instead of a mock RESP
// server. A mock would let us assert whatever we assumed the protocol does;
// what bit the operator in production was an assumption about real
// redis/sentinel behaviour (see client_test.go's CKQUORUM and aclfile tests)
// that turned out to be wrong, so these tests talk to the real binaries.
//
// All tests here skip (t.Skip), not fail, when redis-server is not on PATH -
// see requireRedisServer. CI installs redis-server explicitly so it always
// runs there; this keeps `go test ./...` usable on a machine without it.
//
// A small, fixed set of long-lived processes (one master, one replica of it,
// one sentinel monitoring it) is started once for the whole test binary run
// in TestMain and shared read-only across tests - see sharedEnv. Tests that
// need to mutate replication topology start their own dedicated throwaway
// redis-server processes instead (via startRedisProcess), cleaned up
// per-test via t.Cleanup. Tests that need to mutate sentinel state repoint
// and restore the single shared sentinel process instead of starting their
// own - see newSentinelProcessOnPort's doc comment for why there can only
// ever be one.

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	rediscli "github.com/go-redis/redis/v8"
)

const testLoopbackIP = "127.0.0.1"

// bgCtx is a tiny convenience wrapper so test helpers read a little less
// noisily than repeating context.Background() everywhere.
func bgCtx() context.Context {
	return context.Background()
}

// requireRedisServer skips the calling test when redis-server isn't
// available on PATH, so `go test ./...` keeps working on a machine that
// doesn't have Redis installed.
func requireRedisServer(t *testing.T) {
	t.Helper()
	if !hasRedisServer() {
		t.Skip("redis-server not found on PATH; skipping test that needs a real redis-server subprocess")
	}
}

func hasRedisServer() bool {
	_, err := exec.LookPath("redis-server")
	return err == nil
}

// redisProc represents a running redis-server (plain or --sentinel) test
// subprocess, bound to an isolated temp working directory.
type redisProc struct {
	IP   string
	Port int
	dir  string
	cmd  *exec.Cmd
}

func (p *redisProc) Addr() string {
	return net.JoinHostPort(p.IP, strconv.Itoa(p.Port))
}

// findFreePort asks the OS for a currently-unused TCP port by briefly
// binding to :0 and releasing it. redis-server itself doesn't support
// OS-assigned ports, so callers pass the returned port to redis-server and
// retry in the unlikely case something else grabs it first.
func findFreePort() (int, error) {
	l, err := net.Listen("tcp", testLoopbackIP+":0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// waitForPingErr polls PING against proc until it succeeds or the timeout
// elapses. It also detects an early process exit (e.g. a port bind failure)
// so callers can retry with a different port instead of waiting out the
// full timeout.
func waitForPingErr(proc *redisProc, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	rc := rediscli.NewClient(&rediscli.Options{Addr: proc.Addr(), DialTimeout: 200 * time.Millisecond})
	defer func() { _ = rc.Close() }()
	for time.Now().Before(deadline) {
		if proc.cmd.ProcessState != nil {
			return fmt.Errorf("redis-server exited early (dir=%s)", proc.dir)
		}
		if err := rc.Ping(bgCtx()).Err(); err == nil {
			return nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for redis-server to respond to PING at %s", proc.Addr())
}

func killProc(proc *redisProc) {
	if proc == nil || proc.cmd == nil || proc.cmd.Process == nil {
		return
	}
	_ = proc.cmd.Process.Kill()
	_, _ = proc.cmd.Process.Wait()
}

// newRedisProcess starts a plain (non-sentinel) redis-server in its own temp
// dir, with persistence disabled for speed and to avoid leaving artifacts
// behind, retrying with a fresh port on bind failure. It has no *testing.T
// dependency so it can be used both from per-test helpers and from
// TestMain for the long-lived shared environment.
func newRedisProcess(extraArgs ...string) (*redisProc, error) {
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		dir, err := os.MkdirTemp("", "redis-operator-test-redis-")
		if err != nil {
			return nil, fmt.Errorf("mkdir temp dir: %w", err)
		}
		port, err := findFreePort()
		if err != nil {
			_ = os.RemoveAll(dir)
			return nil, fmt.Errorf("find free port: %w", err)
		}
		args := []string{
			"--port", strconv.Itoa(port),
			"--daemonize", "no",
			"--save", "",
			"--appendonly", "no",
			"--bind", testLoopbackIP,
			"--protected-mode", "no",
			"--dir", dir,
			"--logfile", filepath.Join(dir, "log.txt"),
		}
		args = append(args, extraArgs...)
		cmd := exec.Command("redis-server", args...)
		if err := cmd.Start(); err != nil {
			_ = os.RemoveAll(dir)
			return nil, fmt.Errorf("start redis-server: %w", err)
		}
		proc := &redisProc{IP: testLoopbackIP, Port: port, dir: dir, cmd: cmd}
		go func() { _ = cmd.Wait() }()
		if err := waitForPingErr(proc, 5*time.Second); err != nil {
			lastErr = err
			killProc(proc)
			_ = os.RemoveAll(dir)
			continue
		}
		return proc, nil
	}
	return nil, fmt.Errorf("could not start redis-server after retries: %w", lastErr)
}

// newSentinelProcessOnPort starts redis-server in --sentinel mode bound to
// an exact, caller-chosen port. Sentinel refuses to start without a config
// file it can rewrite to disk, so one is written into an isolated temp dir
// first. extraConfLines are appended to that config file verbatim (e.g.
// "sentinel monitor mymaster 127.0.0.1 6379 1"). Like newRedisProcess, this
// has no *testing.T dependency, so it's usable from TestMain.
//
// Every Sentinel-related method on Client (GetSentinelMonitor,
// GetNumberSentinelsInMemory, MonitorRedisWithPort, SentinelCheckQuorum,
// SetCustomSentinelConfig, ResetSentinel, ...) connects to the *hardcoded*
// sentinelPort constant rather than to any port passed in or discovered -
// confirmed by reading client.go, where every Sentinel function builds its
// Addr as `net.JoinHostPort(ip, sentinelPort)`. Unlike the plain-Redis
// methods (which all take an explicit port parameter), there is no way to
// reach a test sentinel on any other port through the Client interface.
// That's why this package starts exactly one sentinel process, on
// sentinelPort, for the whole test binary run (see TestMain) instead of a
// dedicated throwaway sentinel per test: only one process can bind
// 127.0.0.1:<sentinelPort> at a time. Tests that need a differently
// configured sentinel repoint and restore this single shared process - see
// pointSharedSentinelAt.
func newSentinelProcessOnPort(port int, extraConfLines ...string) (*redisProc, error) {
	dir, err := os.MkdirTemp("", "redis-operator-test-sentinel-")
	if err != nil {
		return nil, fmt.Errorf("mkdir temp dir: %w", err)
	}
	confPath := filepath.Join(dir, "sentinel.conf")
	conf := fmt.Sprintf("port %d\ndir %s\nsentinel resolve-hostnames no\n", port, dir)
	for _, line := range extraConfLines {
		conf += line + "\n"
	}
	if err := os.WriteFile(confPath, []byte(conf), 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("write sentinel config: %w", err)
	}
	cmd := exec.Command("redis-server", confPath, "--sentinel",
		"--daemonize", "no",
		"--logfile", filepath.Join(dir, "log.txt"),
	)
	if err := cmd.Start(); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("start redis-server --sentinel: %w", err)
	}
	proc := &redisProc{IP: testLoopbackIP, Port: port, dir: dir, cmd: cmd}
	go func() { _ = cmd.Wait() }()
	if err := waitForPingErr(proc, 5*time.Second); err != nil {
		killProc(proc)
		_ = os.RemoveAll(dir)
		return nil, err
	}
	return proc, nil
}

// startRedisProcess is the per-test wrapper around newRedisProcess: it fails
// the test on error and registers cleanup via t.Cleanup.
func startRedisProcess(t *testing.T, extraArgs ...string) *redisProc {
	t.Helper()
	proc, err := newRedisProcess(extraArgs...)
	if err != nil {
		t.Fatalf("%v", err)
	}
	t.Cleanup(func() {
		killProc(proc)
		_ = os.RemoveAll(proc.dir)
	})
	return proc
}

// startRedisProcessOnAddr starts a plain redis-server bound to an exact
// caller-chosen IP and port. Used together with nonLoopbackIPv4 to set up
// two independently-addressable instances sharing the same port number
// (mirroring how distinct pods in a real deployment share one Redis port),
// which MakeSlaveOfWithPort's connection-address logic actually requires -
// see the comment on TestMakeSlaveOfWithPort_MismatchedTargetPort.
func startRedisProcessOnAddr(t *testing.T, ip string, port int, extraArgs ...string) *redisProc {
	t.Helper()
	dir := t.TempDir()
	args := []string{
		"--port", strconv.Itoa(port),
		"--daemonize", "no",
		"--save", "",
		"--appendonly", "no",
		"--bind", ip,
		"--protected-mode", "no",
		"--dir", dir,
		"--logfile", filepath.Join(dir, "log.txt"),
	}
	args = append(args, extraArgs...)
	cmd := exec.Command("redis-server", args...)
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start redis-server on %s:%d: %v", ip, port, err)
	}
	proc := &redisProc{IP: ip, Port: port, dir: dir, cmd: cmd}
	go func() { _ = cmd.Wait() }()
	if err := waitForPingErr(proc, 5*time.Second); err != nil {
		killProc(proc)
		t.Fatalf("redis-server did not come up on %s:%d: %v", ip, port, err)
	}
	t.Cleanup(func() { killProc(proc) })
	return proc
}

// nonLoopbackIPv4 returns a non-loopback IPv4 address configured on this
// machine, if any, and true - or ("", false) if none is found. It exists
// so a couple of tests can set up two independently-addressable Redis
// instances (needed to exercise MakeSlaveOfWithPort correctly, and to avoid
// a documented client.go quirk where a replica whose master_host happens to
// be exactly "127.0.0.1" is never reported ready - see
// TestSlaveIsReady_LoopbackMasterHostNeverReady). Every CI runner and
// ordinary dev machine has one (e.g. eth0), but this skips gracefully
// rather than failing on an unusual sandboxed network namespace with only
// loopback.
func nonLoopbackIPv4() (string, bool) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "", false
	}
	for _, a := range addrs {
		ipNet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip4 := ipNet.IP.To4()
		if ip4 == nil || ip4.IsLoopback() {
			continue
		}
		return ip4.String(), true
	}
	return "", false
}

// startReplicaOf starts a fresh redis-server and points it at master via
// REPLICAOF, returning once the process is up (not once replication is
// synced - callers that need a synced/"up" link should poll, e.g. via
// waitForCondition on SlaveIsReady or GetReplicationInfo).
func startReplicaOf(t *testing.T, master *redisProc) *redisProc {
	t.Helper()
	return startRedisProcess(t, "--replicaof", master.IP, strconv.Itoa(master.Port))
}

// waitForCondition polls cond until it returns true or the timeout elapses.
func waitForCondition(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return cond()
}

// sharedEnv holds the small, fixed set of long-lived redis/sentinel
// processes shared read-only across tests that don't mutate topology or
// sentinel state, so we don't pay subprocess-startup cost per test case.
// It's started once in TestMain (not via a per-test t.Cleanup-guarded
// sync.Once - the processes must outlive whichever individual test happens
// to run first) and torn down after all tests in the package finish.
// Tests that mutate replication topology (MakeMaster, MakeSlaveOf, ...) must
// start their own dedicated throwaway redis-server processes instead of
// touching master/replica here. Tests that mutate sentinel state
// (SetCustomSentinelConfig, MonitorRedisWithPort, ResetSentinel, the
// NOQUORUM scenario, ...) repoint and restore the shared sentinel itself -
// see pointSharedSentinelAt/restoreSharedSentinel.
type sharedEnv struct {
	master   *redisProc
	replica  *redisProc
	sentinel *redisProc
}

var sharedEnvVal *sharedEnv

// TestMain starts the shared environment once (if redis-server is
// available) before any test runs, and tears it down after all tests in the
// package complete - see sharedEnv's doc comment for why this can't be a
// lazy sync.Once initialized from inside an individual test.
func TestMain(m *testing.M) {
	var cleanups []func()
	cleanupAll := func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}

	if hasRedisServer() {
		master, err := newRedisProcess()
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to start shared test master: %v\n", err)
			os.Exit(1)
		}
		cleanups = append(cleanups, func() { killProc(master); _ = os.RemoveAll(master.dir) })

		replica, err := newRedisProcess("--replicaof", master.IP, strconv.Itoa(master.Port))
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to start shared test replica: %v\n", err)
			cleanupAll()
			os.Exit(1)
		}
		cleanups = append(cleanups, func() { killProc(replica); _ = os.RemoveAll(replica.dir) })

		// Sentinel-related Client methods hardcode sentinelPort as the port
		// they connect to (see newSentinelProcessOnPort's doc comment), so
		// the shared test sentinel must bind exactly that port.
		fixedSentinelPort, err := strconv.Atoi(sentinelPort)
		if err != nil {
			fmt.Fprintf(os.Stderr, "client.go's sentinelPort constant %q is not numeric: %v\n", sentinelPort, err)
			cleanupAll()
			os.Exit(1)
		}
		sentinel, err := newSentinelProcessOnPort(fixedSentinelPort,
			fmt.Sprintf("sentinel monitor mymaster %s %d 1", master.IP, master.Port),
			"sentinel down-after-milliseconds mymaster 2000",
		)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to start shared test sentinel on port %d: %v\n", fixedSentinelPort, err)
			cleanupAll()
			os.Exit(1)
		}
		cleanups = append(cleanups, func() { killProc(sentinel); _ = os.RemoveAll(sentinel.dir) })

		// Wait for the replication link to come up so tests relying on
		// master-side connected_slaves / sentinel-side slave discovery
		// don't race the initial sync.
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			rc := rediscli.NewClient(&rediscli.Options{Addr: replica.Addr()})
			info, err := rc.Info(bgCtx(), "replication").Result()
			_ = rc.Close()
			if err == nil && strings.Contains(info, redisLinkUp) {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}

		sharedEnvVal = &sharedEnv{master: master, replica: replica, sentinel: sentinel}
	}

	code := m.Run()
	cleanupAll()
	os.Exit(code)
}

// getSharedEnv returns the shared master/replica/sentinel environment
// started in TestMain, skipping the calling test if redis-server wasn't
// available (and so the shared environment was never started).
func getSharedEnv(t *testing.T) *sharedEnv {
	t.Helper()
	requireRedisServer(t)
	if sharedEnvVal == nil {
		t.Fatal("shared redis/sentinel test environment failed to initialize (see TestMain output)")
	}
	return sharedEnvVal
}

// pointSharedSentinelAt repoints the single shared sentinel process (see
// newSentinelProcessOnPort's doc comment for why there's only one) at the
// given master/quorum via raw SENTINEL REMOVE+MONITOR, bypassing the
// Client under test so setup/teardown for tests that aren't themselves
// about MonitorRedisWithPort doesn't depend on it. It fails the test if the
// commands don't succeed.
func pointSharedSentinelAt(t *testing.T, sentinel *redisProc, masterIP string, masterPort int, quorum int) {
	t.Helper()
	rc := rediscli.NewSentinelClient(&rediscli.Options{Addr: sentinel.Addr()})
	defer func() { _ = rc.Close() }()
	// Best-effort removal of any existing "mymaster" entry, mirroring
	// MonitorRedisWithPort's own "continue even if it fails" approach.
	_ = rc.Process(bgCtx(), rediscli.NewBoolCmd(bgCtx(), "SENTINEL", "REMOVE", masterName))
	cmd := rediscli.NewBoolCmd(bgCtx(), "SENTINEL", "MONITOR", masterName, masterIP, strconv.Itoa(masterPort), strconv.Itoa(quorum))
	if err := rc.Process(bgCtx(), cmd); err != nil {
		t.Fatalf("failed to point shared sentinel at %s:%d: %v", masterIP, masterPort, err)
	}
	if _, err := cmd.Result(); err != nil {
		t.Fatalf("failed to point shared sentinel at %s:%d: %v", masterIP, masterPort, err)
	}
}

// restoreSharedSentinel points the shared sentinel back at the shared
// master with quorum 1, the state every read-only test expects to find it
// in. Tests that repoint the shared sentinel elsewhere must register this
// via t.Cleanup so later tests aren't affected - Go runs tests within a
// package sequentially unless they call t.Parallel (none here do), so this
// is safe as long as every mutating test restores what it changed.
func restoreSharedSentinel(t *testing.T, env *sharedEnv) {
	t.Helper()
	pointSharedSentinelAt(t, env.sentinel, env.master.IP, env.master.Port, 1)
}
