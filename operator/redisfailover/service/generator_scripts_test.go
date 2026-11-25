package service_test

import (
	"cmp"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	mK8SService "github.com/saremox/redis-operator/mocks/service/k8s"
	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
)

func TestRedisShutdownScriptEndsInsideGracePeriod(t *testing.T) {
	if _, err := exec.LookPath("timeout"); err != nil {
		skipOrFail(t, "timeout is not installed")
	}

	rf := generateRF()
	rf.Spec.Redis.Port = 6379
	rf.Spec.Sentinel.Enabled = new(true)
	var script string
	ms := &mK8SService.Services{}
	ms.On("CreateOrUpdateConfigMap", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
		script = args.Get(1).(*corev1.ConfigMap).Data["shutdown.sh"]
	}).Return(nil)
	client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
	require.NoError(t, client.EnsureRedisShutdownConfigMap(rf, nil, []metav1.OwnerReference{}))

	// The fake Sentinel reports this pod (10.0.0.1) as the master on the
	// first query and 10.0.0.2 after it. The fake local Redis answers OK, or
	// refuses a password other than FAKE_REQUIREPASS as redis-cli does. Its
	// INFO shows one online replica behind the master for the first
	// FAKE_BEHIND calls, or FAKE_INFO. The first FAKE_FAILOVER_HANGS failover
	// requests hang, and the next ones answer after FAKE_FAILOVER_DELAY. The
	// same applies to the master lookups with FAKE_LOOKUP_HANGS and
	// FAKE_LOOKUP_DELAY. FAKE_INFO_HANGS=1 makes INFO hang.
	fakeCLI := `#!/bin/sh
echo "auth=$REDISCLI_AUTH $*" >>"$FAKE_LOG"
case "$*" in
*"-h "*) ;;
*) if [ -n "$FAKE_REQUIREPASS" ] && [ "$REDISCLI_AUTH" != "$FAKE_REQUIREPASS" ]; then
	echo "AUTH failed: WRONGPASS invalid username-password pair or user is disabled." >&2
	echo "NOAUTH Authentication required."
	exit 0
fi
if [ "$FAKE_NOPASS" = 1 ] && [ -n "$REDISCLI_AUTH" ]; then
	echo "AUTH failed: ERR AUTH <password> called without any password configured for the default user." >&2
fi ;;
esac
case "$*" in
*"-h "*) ;;
*"CLIENT PAUSE"*) echo "${FAKE_PAUSE:-OK}"; exit 0 ;;
*"INFO replication"*)
	[ "$FAKE_INFO_HANGS" = 1 ] && exec sleep 30
	if [ -n "$FAKE_INFO" ]; then
		printf '%b\n' "$FAKE_INFO"
		exit 0
	fi
	offset=200
	[ "$(grep -c "INFO replication" "$FAKE_LOG")" -le "${FAKE_BEHIND:-0}" ] && offset=150
	printf '# Replication\r\nrole:master\r\nconnected_slaves:3\r\n'
	printf 'slave0:ip=10.0.0.2,port=6379,state=online,offset=200,lag=0\r\n'
	printf 'slave1:ip=10.0.0.3,port=6379,state=online,offset=%s,lag=1,type=replica\r\n' "$offset"
	printf 'slave2:ip=10.0.0.4,port=6379,state=wait_bgsave,offset=0,lag=0\r\n'
	printf 'master_failover_state:no-failover\r\nmaster_repl_offset:200\r\n'
	exit 0 ;;
*) echo OK; exit 0 ;;
esac
[ "$FAKE_FROZEN" = 1 ] && exec sleep 30
case "$*" in
*get-master-addr-by-name*)
	n=$(grep -c get-master-addr-by-name "$FAKE_LOG")
	hangs=${FAKE_LOOKUP_HANGS:-0}
	[ "$n" -le "$hangs" ] && exec sleep 30
	[ "$n" -eq $((hangs + 1)) ] && [ -n "$FAKE_LOOKUP_DELAY" ] && sleep "$FAKE_LOOKUP_DELAY"
	if [ "$FAKE_STUCK" != 1 ] && [ "$n" -gt $((hangs + 1)) ]; then
		echo '"10.0.0.2","6379"'
	else
		echo '"10.0.0.1","6379"'
	fi ;;
*failover*)
	[ "$(grep -c "SENTINEL failover" "$FAKE_LOG")" -le "${FAKE_FAILOVER_HANGS:-0}" ] && exec sleep 30
	[ -n "$FAKE_FAILOVER_DELAY" ] && sleep "$FAKE_FAILOVER_DELAY"
	echo "${FAKE_FAILOVER:-OK}" ;;
esac
`
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "redis-cli"), []byte(fakeCLI), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hostname"), []byte("#!/bin/sh\necho 10.0.0.1\n"), 0o755))

	sentinel := "auth= -h " + rfservice.GetSentinelName(rf) + " -p 26379 "
	lookup := sentinel + "--csv SENTINEL get-master-addr-by-name mymaster"
	failover := sentinel + "SENTINEL failover mymaster"
	info := "auth= -p 6379 INFO replication"
	moved := []string{
		failover,
		lookup,
		"auth= -p 6379 REPLICAOF 10.0.0.2 6379",
		"auth= -p 6379 CLIENT UNPAUSE",
		"auth= -p 6379 save",
	}
	tests := []struct {
		name      string
		env       []string
		wantCalls []string
		// manyInfo means that the script repeats INFO until the end of the
		// wait for the replicas. wantCalls then has one INFO call.
		manyInfo   bool
		wantOutput string
		// minPause is the smallest CLIENT PAUSE time in ms, 17000 if unset.
		minPause int
		minTime  time.Duration
		maxTime  time.Duration
	}{
		{
			name: "master moves",
			env:  []string{"REDIS_PASSWORD=secret"},
			wantCalls: []string{
				lookup,
				"auth=secret -p 6379 CLIENT PAUSE <ms> WRITE",
				"auth=secret -p 6379 INFO replication",
				failover,
				lookup,
				"auth=secret -p 6379 REPLICAOF 10.0.0.2 6379",
				"auth=secret -p 6379 CLIENT UNPAUSE",
				"auth=secret -p 6379 save",
			},
			maxTime: 5 * time.Second,
		},
		{
			// Sentinel can promote a replica that did not get the last
			// writes. The script asks for the failover only after the
			// replica confirmed the offset of the master.
			name:      "replica behind",
			env:       []string{"FAKE_BEHIND=3"},
			wantCalls: append([]string{lookup, "auth= -p 6379 CLIENT PAUSE <ms> WRITE", info, info, info, info}, moved...),
			maxTime:   5 * time.Second,
		},
		{
			// A replica that does not confirm the writes must not stop the
			// failover: the pause of the writes ends at a fixed time.
			name:       "replica stays behind",
			env:        []string{"FAKE_BEHIND=1000"},
			wantCalls:  append([]string{lookup, "auth= -p 6379 CLIENT PAUSE <ms> WRITE", info}, moved...),
			manyInfo:   true,
			wantOutput: "shutdown.sh: not all online replicas confirmed the writes, the failover can lose writes (replicas behind: 1)",
			minTime:    3 * time.Second,
			maxTime:    9 * time.Second,
		},
		{
			// The failover requests time out twice after a replica that
			// stayed behind. A request that Sentinel accepts later leaves no
			// time for the REPLICAOF, and the old master accepts writes after
			// the pause. Thus the script asks for no more failovers 3s before
			// the deadline.
			name:       "late failover",
			env:        []string{"FAKE_BEHIND=1000", "FAKE_FAILOVER_HANGS=2", "FAKE_FAILOVER_DELAY=1.95"},
			wantCalls:  []string{lookup, "auth= -p 6379 CLIENT PAUSE <ms> WRITE", info, failover, failover, "auth= -p 6379 CLIENT UNPAUSE", "auth= -p 6379 save"},
			manyInfo:   true,
			wantOutput: "shutdown.sh: sentinel did not accept the failover after 2 attempts",
			minTime:    8 * time.Second,
			maxTime:    13 * time.Second,
		},
		{
			// The master lookups use the time of the wait for the replicas.
			// Then the script asks for the failover without the wait, and
			// does not call INFO, which can hang for 2s.
			name: "slow lookups",
			env:  []string{"FAKE_LOOKUP_HANGS=2", "FAKE_LOOKUP_DELAY=1.5", "FAKE_INFO_HANGS=1"},
			wantCalls: append([]string{lookup, lookup, lookup, "auth= -p 6379 CLIENT PAUSE <ms> WRITE"},
				moved...),
			wantOutput: "(replicas behind: unknown)",
			minPause:   10000,
			minTime:    7 * time.Second,
			maxTime:    12 * time.Second,
		},
		{
			name:       "info fails",
			env:        []string{"FAKE_INFO=NOAUTH Authentication required."},
			wantCalls:  append([]string{lookup, "auth= -p 6379 CLIENT PAUSE <ms> WRITE", info}, moved...),
			manyInfo:   true,
			wantOutput: "(replicas behind: unknown)",
			minTime:    3 * time.Second,
			maxTime:    9 * time.Second,
		},
		{
			// A replica in a full sync has no offset to compare.
			name: "no online replica",
			env: []string{
				`FAKE_INFO=role:master\r\nslave0:ip=10.0.0.2,port=6379,state=send_bulk,offset=0,lag=0\r\nmaster_repl_offset:200\r`,
			},
			wantCalls: append([]string{lookup, "auth= -p 6379 CLIENT PAUSE <ms> WRITE", info}, moved...),
			maxTime:   5 * time.Second,
		},
		{
			// Redis before 6.2 has no CLIENT PAUSE WRITE. A failover without
			// the pause loses the writes that the promoted replica did not
			// get, so the script leaves the failover to Sentinel.
			name: "pause fails",
			env:  []string{"FAKE_PAUSE=ERR syntax error"},
			wantCalls: []string{
				lookup,
				"auth= -p 6379 CLIENT PAUSE <ms> WRITE",
				"auth= -p 6379 save",
			},
			maxTime: 5 * time.Second,
		},
		{
			// The operator changed the password in place, and the env of the
			// pod still has the old one: no pause, so no failover.
			name: "stale password",
			env:  []string{"REDIS_PASSWORD=old", "FAKE_REQUIREPASS=new"},
			wantCalls: []string{
				lookup,
				"auth=old -p 6379 CLIENT PAUSE <ms> WRITE",
				"auth=old -p 6379 save",
			},
			maxTime: 5 * time.Second,
		},
		{
			// The operator removed the password in place. redis-cli warns on
			// stderr, but the commands work: the failover stays fast.
			name: "password removed",
			env:  []string{"REDIS_PASSWORD=old", "FAKE_NOPASS=1"},
			wantCalls: []string{
				lookup,
				"auth=old -p 6379 CLIENT PAUSE <ms> WRITE",
				"auth=old -p 6379 INFO replication",
				failover,
				lookup,
				"auth=old -p 6379 REPLICAOF 10.0.0.2 6379",
				"auth=old -p 6379 CLIENT UNPAUSE",
				"auth=old -p 6379 save",
			},
			maxTime: 5 * time.Second,
		},
		{
			name: "failover fails",
			env:  []string{"FAKE_FAILOVER=NOGOODSLAVE No suitable replica to promote"},
			wantCalls: []string{
				lookup,
				"auth= -p 6379 CLIENT PAUSE <ms> WRITE",
				info,
				failover,
				failover,
				failover,
				"auth= -p 6379 CLIENT UNPAUSE",
				"auth= -p 6379 save",
			},
			maxTime: 6 * time.Second,
		},
		{
			// The poll stops at the 12s deadline.
			name:    "no new master",
			env:     []string{"FAKE_STUCK=1"},
			maxTime: 16 * time.Second,
		},
		{
			// Three lookups of at most 2s each, and a 1s sleep after each.
			name: "frozen sentinel",
			env:  []string{"FAKE_FROZEN=1"},
			wantCalls: []string{
				lookup,
				lookup,
				lookup,
				"auth= -p 6379 save",
			},
			maxTime: 12 * time.Second,
		},
	}
	pause := regexp.MustCompile(`CLIENT PAUSE (\d+) WRITE`)
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			logFile := filepath.Join(t.TempDir(), "calls")
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "sh", "-c", script)
			cmd.WaitDelay = time.Second
			cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "FAKE_LOG="+logFile, "REDIS_PASSWORD=")
			cmd.Env = append(cmd.Env, test.env...)
			start := time.Now()
			out, err := cmd.CombinedOutput()
			elapsed := time.Since(start)
			require.NoError(t, err, string(out))
			assert.GreaterOrEqual(t, elapsed, test.minTime)
			assert.Less(t, elapsed, test.maxTime)
			assert.Contains(t, string(out), test.wantOutput)
			raw, err := os.ReadFile(logFile)
			require.NoError(t, err)
			calls := strings.Split(strings.TrimSpace(string(raw)), "\n")
			for i, call := range calls {
				m := pause.FindStringSubmatch(call)
				if m == nil {
					continue
				}
				// The pause ends 6s after the 12s deadline.
				ms, err := strconv.Atoi(m[1])
				require.NoError(t, err)
				assert.GreaterOrEqual(t, ms, cmp.Or(test.minPause, 17000))
				assert.LessOrEqual(t, ms, 18000)
				calls[i] = pause.ReplaceAllString(call, "CLIENT PAUSE <ms> WRITE")
			}
			if test.manyInfo {
				// One INFO call each 0.1s until 8s before the deadline.
				var collapsed []string
				for _, call := range calls {
					if strings.HasSuffix(call, " INFO replication") && len(collapsed) > 0 && collapsed[len(collapsed)-1] == call {
						continue
					}
					collapsed = append(collapsed, call)
				}
				assert.Greater(t, len(calls)-len(collapsed), 10)
				calls = collapsed
			}
			if test.wantCalls != nil {
				assert.Equal(t, test.wantCalls, calls)
				return
			}
			// The poll repeats the lookup until the deadline.
			require.GreaterOrEqual(t, len(calls), 7)
			assert.Equal(t, []string{lookup, "auth= -p 6379 CLIENT PAUSE <ms> WRITE", info, failover}, calls[:4])
			for _, call := range calls[4 : len(calls)-2] {
				assert.Equal(t, lookup, call)
			}
			assert.Equal(t, []string{"auth= -p 6379 CLIENT UNPAUSE", "auth= -p 6379 save"}, calls[len(calls)-2:])
		})
	}
}

// startShutdownTestRedis starts a redis-server on ip:port and reports whether
// it answers. The pods of a RedisFailover use one port, and the script gives
// that port to REPLICAOF.
func startShutdownTestRedis(t *testing.T, ip string, port int, args ...string) (*exec.Cmd, bool) {
	t.Helper()
	dir := t.TempDir()
	args = append([]string{"--bind", ip, "--port", strconv.Itoa(port), "--save", "", "--appendonly", "no",
		"--dir", dir, "--logfile", filepath.Join(dir, "log"), "--repl-diskless-sync-delay", "0"}, args...)
	cmd := exec.Command("redis-server", args...)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	for range 100 {
		out, err := exec.Command("redis-cli", "-h", ip, "-p", strconv.Itoa(port), "PING").Output()
		if err == nil && strings.TrimSpace(string(out)) == "PONG" {
			return cmd, true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return cmd, false
}

// startShutdownTestPair starts a master on 127.0.0.1 and its replica on
// 127.0.0.2. Another process can take the free port before redis-server
// binds it, so the pair gets a second attempt on another port.
func startShutdownTestPair(t *testing.T) (int, *exec.Cmd) {
	t.Helper()
	for range 2 {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		port := l.Addr().(*net.TCPAddr).Port
		_ = l.Close()
		if _, ok := startShutdownTestRedis(t, "127.0.0.1", port); !ok {
			continue
		}
		replica, ok := startShutdownTestRedis(t, "127.0.0.2", port,
			"--replicaof", "127.0.0.1", strconv.Itoa(port), "--replica-announce-ip", "127.0.0.2")
		if ok {
			return port, replica
		}
	}
	t.Fatal("redis-server did not start on 127.0.0.1 and 127.0.0.2")
	return 0, nil
}

func shutdownTestReplicationInfo(t *testing.T, ip string, port int) string {
	t.Helper()
	out, err := exec.Command("redis-cli", "-h", ip, "-p", strconv.Itoa(port), "INFO", "replication").Output()
	require.NoError(t, err)
	return strings.ReplaceAll(string(out), "\r", "")
}

// The script runs against a real master and replica. The fake Sentinel
// promotes the replica at once, as Sentinel does after a SENTINEL FAILOVER.
func TestRedisShutdownScriptWaitsForTheReplica(t *testing.T) {
	for _, bin := range []string{"redis-server", "redis-cli", "timeout"} {
		if _, err := exec.LookPath(bin); err != nil {
			skipOrFail(t, "%s is not installed", bin)
		}
	}
	version, err := exec.Command("redis-server", "--version").Output()
	require.NoError(t, err)
	m := regexp.MustCompile(`v=(\d+)\.(\d+)`).FindSubmatch(version)
	if m == nil {
		t.Skipf("unknown redis-server version: %s", version)
	}
	major, _ := strconv.Atoi(string(m[1]))
	minor, _ := strconv.Atoi(string(m[2]))
	if major < 6 || major == 6 && minor < 2 {
		t.Skipf("redis-server before 6.2 has no CLIENT PAUSE WRITE: %s", version)
	}
	// macOS has only 127.0.0.1 on the loopback interface.
	l, err := net.Listen("tcp", "127.0.0.2:0")
	if err != nil {
		t.Skipf("cannot bind 127.0.0.2: %v", err)
	}
	_ = l.Close()
	realCLI, err := exec.LookPath("redis-cli")
	require.NoError(t, err)
	port, replica := startShutdownTestPair(t)
	online := regexp.MustCompile(`slave0:ip=127\.0\.0\.2,port=\d+,state=online,offset=(\d+),`)
	top := regexp.MustCompile(`master_repl_offset:(\d+)`)
	require.Eventually(t, func() bool {
		info := shutdownTestReplicationInfo(t, "127.0.0.1", port)
		m, o := online.FindStringSubmatch(info), top.FindStringSubmatch(info)
		return m != nil && o != nil && m[1] == o[1]
	}, 10*time.Second, 50*time.Millisecond, "the replica did not sync")

	// The stopped replica does not get the writes that the master
	// acknowledges.
	require.NoError(t, replica.Process.Signal(syscall.SIGSTOP))
	t.Cleanup(func() { _ = replica.Process.Signal(syscall.SIGCONT) })
	var writes strings.Builder
	for i := range 100 {
		fmt.Fprintf(&writes, "SET k%d %d\n", i, i)
	}
	set := exec.Command("redis-cli", "-p", strconv.Itoa(port))
	set.Stdin = strings.NewReader(writes.String())
	require.NoError(t, set.Run())

	rf := generateRF()
	rf.Spec.Redis.Port = int32(port)
	rf.Spec.Sentinel.Enabled = new(true)
	var script string
	ms := &mK8SService.Services{}
	ms.On("CreateOrUpdateConfigMap", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
		script = args.Get(1).(*corev1.ConfigMap).Data["shutdown.sh"]
	}).Return(nil)
	client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
	require.NoError(t, client.EnsureRedisShutdownConfigMap(rf, nil, []metav1.OwnerReference{}))

	// The fake Sentinel records the offsets at the failover request, then
	// promotes the replica. The local calls go to the real master.
	dir := t.TempDir()
	fakeCLI := fmt.Sprintf(`#!/bin/sh
case "$*" in
*"-h "*) ;;
*) exec %[1]s "$@" ;;
esac
case "$*" in
*get-master-addr-by-name*)
	if [ -e %[2]s/promoted ]; then echo '"127.0.0.2","%[3]d"'; else echo '"127.0.0.1","%[3]d"'; fi ;;
*failover*)
	%[1]s -p %[3]d INFO replication >%[2]s/master
	%[1]s -h 127.0.0.2 -p %[3]d INFO replication >%[2]s/replica
	%[1]s -h 127.0.0.2 -p %[3]d REPLICAOF NO ONE >/dev/null
	touch %[2]s/promoted
	echo OK ;;
esac
`, realCLI, dir, port)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "redis-cli"), []byte(fakeCLI), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hostname"), []byte("#!/bin/sh\necho 127.0.0.1\n"), 0o755))

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", script)
	cmd.WaitDelay = time.Second
	cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "REDIS_PASSWORD=")
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	require.NoError(t, cmd.Start())
	time.Sleep(500 * time.Millisecond)
	require.NoError(t, replica.Process.Signal(syscall.SIGCONT))
	require.NoError(t, cmd.Wait(), out.String())

	raw, err := os.ReadFile(filepath.Join(dir, "master"))
	require.NoError(t, err, out.String())
	master := strings.ReplaceAll(string(raw), "\r", "")
	offset := top.FindStringSubmatch(master)
	require.NotNil(t, offset, master)
	acked := online.FindStringSubmatch(master)
	require.NotNil(t, acked, master)
	assert.Equal(t, offset[1], acked[1], "the master had writes that the replica did not confirm")
	raw, err = os.ReadFile(filepath.Join(dir, "replica"))
	require.NoError(t, err)
	assert.Contains(t, strings.ReplaceAll(string(raw), "\r", ""), "slave_repl_offset:"+offset[1]+"\n")

	dbsize, err := exec.Command("redis-cli", "-h", "127.0.0.2", "-p", strconv.Itoa(port), "DBSIZE").Output()
	require.NoError(t, err)
	assert.Equal(t, "100", strings.TrimSpace(string(dbsize)))
	assert.Contains(t, shutdownTestReplicationInfo(t, "127.0.0.1", port), "master_host:127.0.0.2\n")
}

// TestRedisReadinessScript runs ready.sh against a fake redis-cli that prints
// INFO replication as Redis 7.2-8 and Valkey 8-9 report it, or hangs like a
// frozen server that accepts the connection but never replies. Each case runs
// with and without timeout on PATH.
func TestRedisReadinessScript(t *testing.T) {
	if _, err := exec.LookPath("timeout"); err != nil {
		skipOrFail(t, "timeout is not installed")
	}
	replica := func(fields ...string) string {
		return strings.Join(append([]string{"# Replication", "role:slave"}, fields...), "\r\n") + "\r\n"
	}
	tests := []struct {
		name  string
		info  string
		ready bool
	}{
		{
			name:  "master",
			info:  "# Replication\r\nrole:master\r\nconnected_slaves:2\r\n",
			ready: true,
		},
		{
			name:  "refused password",
			info:  "NOAUTH Authentication required.\n",
			ready: true,
		},
		{
			name:  "replica of the config placeholder",
			info:  replica("master_host:127.0.0.1", "master_link_status:down", "master_sync_in_progress:0", "master_link_down_since_seconds:-1"),
			ready: false,
		},
		{
			name:  "full sync in progress",
			info:  replica("master_host:10.0.0.1", "master_link_status:down", "master_sync_in_progress:1", "master_link_down_since_seconds:-1"),
			ready: false,
		},
		{
			name:  "link up",
			info:  replica("master_host:10.0.0.1", "master_link_status:up", "master_last_io_seconds_ago:1", "master_sync_in_progress:0"),
			ready: true,
		},
		{
			name:  "never synced since start, e.g. unreadable RDB format",
			info:  replica("master_host:10.0.0.1", "master_link_status:down", "master_last_io_seconds_ago:-1", "master_sync_in_progress:0", "master_link_down_since_seconds:-1"),
			ready: false,
		},
		{
			name:  "link down briefly, e.g. during a failover",
			info:  replica("master_host:10.0.0.1", "master_link_status:down", "master_last_io_seconds_ago:-1", "master_sync_in_progress:0", "master_link_down_since_seconds:8"),
			ready: true,
		},
		{
			name:  "link down too long",
			info:  replica("master_host:10.0.0.1", "master_link_status:down", "master_last_io_seconds_ago:-1", "master_sync_in_progress:0", "master_link_down_since_seconds:71"),
			ready: false,
		},
		{
			name:  "frozen server",
			ready: false,
		},
	}

	rf := generateRF()
	var gotCM *corev1.ConfigMap
	ms := &mK8SService.Services{}
	ms.On("CreateOrUpdateConfigMap", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
		gotCM = args.Get(1).(*corev1.ConfigMap)
	}).Return(nil)
	client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
	require.NoError(t, client.EnsureRedisReadinessConfigMap(rf, nil, []metav1.OwnerReference{}))

	dir := t.TempDir()
	script := filepath.Join(dir, "ready.sh")
	require.NoError(t, os.WriteFile(script, []byte(gotCM.Data["ready.sh"]), 0o644))
	fakeCLI := "#!/bin/sh\n[ -s \"$FAKE_INFO\" ] || exec sleep 10\ncat \"$FAKE_INFO\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "redis-cli"), []byte(fakeCLI), 0o755))

	// An image with the script's other tools but no timeout.
	bare := t.TempDir()
	for _, tool := range []string{"sh", "xargs", "grep", "tr", "cut", "cat", "sleep"} {
		p, err := exec.LookPath(tool)
		require.NoError(t, err)
		require.NoError(t, os.Symlink(p, filepath.Join(bare, tool)))
	}
	require.NoError(t, os.WriteFile(filepath.Join(bare, "redis-cli"), []byte(fakeCLI), 0o755))

	paths := []struct{ name, path string }{
		{"", dir + ":" + os.Getenv("PATH")},
		{", without timeout", bare},
	}
	for _, test := range tests {
		for _, p := range paths {
			// Without timeout a frozen server still hangs the script.
			if test.info == "" && p.name != "" {
				continue
			}
			t.Run(test.name+p.name, func(t *testing.T) {
				info := filepath.Join(t.TempDir(), "info")
				require.NoError(t, os.WriteFile(info, []byte(test.info), 0o644))

				ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, "sh", script)
				cmd.Env = append(os.Environ(), "PATH="+p.path, "FAKE_INFO="+info)
				cmd.WaitDelay = time.Second
				start := time.Now()
				out, err := cmd.CombinedOutput()
				assert.Equal(t, test.ready, err == nil, "ready.sh output: %s", out)
				// The kubelet's probe timeout is 5s, and some runtimes do not
				// enforce it while a child still holds the output open.
				assert.Less(t, time.Since(start), 5*time.Second, "ready.sh must finish within the probe timeout")
			})
		}
	}
}

// TestProbeCommands runs the default Redis and Sentinel liveness commands and
// the Sentinel readiness command against a fake redis-cli that answers like a
// healthy server or hangs like a frozen one, with and without timeout on PATH.
// The Redis liveness command also gets the replies of a server that loads its
// dataset or refuses the login.
func TestProbeCommands(t *testing.T) {
	if _, err := exec.LookPath("timeout"); err != nil {
		skipOrFail(t, "timeout is not installed")
	}

	probes := map[string]*corev1.Probe{}
	ms := &mK8SService.Services{}
	ms.On("CreateOrUpdatePodDisruptionBudget", namespace, mock.Anything).Return(nil, nil)
	ms.On("CreateOrUpdateServiceAccount", namespace, mock.Anything).Return(nil)
	ms.On("CreateOrUpdateStatefulSet", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
		probes["redis liveness"] = args.Get(1).(*appsv1.StatefulSet).Spec.Template.Spec.Containers[0].LivenessProbe
	}).Return(nil)
	ms.On("CreateOrUpdateDeployment", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
		c := args.Get(1).(*appsv1.Deployment).Spec.Template.Spec.Containers[0]
		probes["sentinel liveness"] = c.LivenessProbe
		probes["sentinel readiness"] = c.ReadinessProbe
	}).Return(nil)
	client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
	require.NoError(t, client.EnsureRedisStatefulset(generateRF(), nil, []metav1.OwnerReference{}))
	require.NoError(t, client.EnsureSentinelDeployment(generateRF(), nil, []metav1.OwnerReference{}))

	fakeCLI := `#!/bin/sh
[ "$FAKE_FROZEN" = 1 ] && exec sleep 10
case "$*" in
*get-master-addr-by-name*) printf '10.0.0.1\n6379\n' ;;
*ckquorum*) echo "OK 3 usable Sentinels. Quorum and failover authorization can be reached" ;;
*)
	[ -n "$FAKE_STDERR" ] && echo "$FAKE_STDERR" >&2
	echo "${FAKE_REPLY:-PONG}" ;;
esac
`
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "redis-cli"), []byte(fakeCLI), 0o755))
	bare := t.TempDir()
	for _, tool := range []string{"sh", "grep", "head", "hostname", "sleep"} {
		p, err := exec.LookPath(tool)
		require.NoError(t, err)
		require.NoError(t, os.Symlink(p, filepath.Join(bare, tool)))
	}
	require.NoError(t, os.WriteFile(filepath.Join(bare, "redis-cli"), []byte(fakeCLI), 0o755))

	type probeTest struct {
		name  string
		path  string
		env   []string
		alive bool
	}
	tests := []probeTest{
		{name: "answers", path: dir + ":" + os.Getenv("PATH"), alive: true},
		{name: "frozen", path: dir + ":" + os.Getenv("PATH"), env: []string{"FAKE_FROZEN=1"}},
		{name: "answers, without timeout", path: bare, alive: true},
	}
	// redis-cli prints an error reply on stdout and exits 0. With a Redis
	// password, a wrong pinger password gives WRONGPASS on stderr, and then
	// NOAUTH for the PING.
	redisReplies := []probeTest{
		{
			name:  "loading",
			path:  dir + ":" + os.Getenv("PATH"),
			env:   []string{"FAKE_REPLY=LOADING Redis is loading the dataset in memory"},
			alive: true,
		},
		{
			name: "wrong password",
			path: dir + ":" + os.Getenv("PATH"),
			env: []string{
				"FAKE_STDERR=AUTH failed: WRONGPASS invalid username-password pair or user is disabled.",
				"FAKE_REPLY=NOAUTH Authentication required.",
			},
		},
	}
	for _, component := range []string{"redis liveness", "sentinel liveness", "sentinel readiness"} {
		probe := probes[component]
		require.Equal(t, "sh", probe.Exec.Command[0])
		componentTests := tests
		if component == "redis liveness" {
			componentTests = append(componentTests, redisReplies...)
		}
		for _, test := range componentTests {
			t.Run(component+", "+test.name, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, "sh", probe.Exec.Command[1:]...)
				cmd.Env = append(os.Environ(), "PATH="+test.path)
				cmd.Env = append(cmd.Env, test.env...)
				cmd.WaitDelay = time.Second
				start := time.Now()
				out, err := cmd.CombinedOutput()
				assert.Equal(t, test.alive, err == nil, "output: %s", out)
				assert.Less(t, time.Since(start), time.Duration(probe.TimeoutSeconds)*time.Second, "must finish within the probe timeout")
			})
		}
	}
}

// TestRedisReadinessLinkDownWindow checks that a replica stays ready through
// the configured failover: the window is 60s plus the failover's timing.
func TestRedisReadinessLinkDownWindow(t *testing.T) {
	tests := []struct {
		name   string
		modify func(rf *redisfailoverv1.RedisFailover)
		window string
	}{
		{
			name:   "operator mode, default failoverTimeout",
			modify: func(_ *redisfailoverv1.RedisFailover) {},
			window: "70",
		},
		{
			name: "operator mode, custom failoverTimeout",
			modify: func(rf *redisfailoverv1.RedisFailover) {
				rf.Spec.Sentinel.FailoverTimeout = &metav1.Duration{Duration: 2 * time.Minute}
			},
			window: "180",
		},
		{
			name: "sentinel mode, default config",
			modify: func(rf *redisfailoverv1.RedisFailover) {
				rf.Spec.Sentinel.Enabled = new(true)
			},
			window: "75",
		},
		{
			name: "sentinel mode, customConfig",
			modify: func(rf *redisfailoverv1.RedisFailover) {
				rf.Spec.Sentinel.Enabled = new(true)
				rf.Spec.Sentinel.CustomConfig = []string{"down-after-milliseconds 120000", "parallel-syncs 1", "failover-timeout 30000"}
			},
			window: "210",
		},
		{
			name: "sentinel mode, customConfig keys in upper case",
			modify: func(rf *redisfailoverv1.RedisFailover) {
				rf.Spec.Sentinel.Enabled = new(true)
				rf.Spec.Sentinel.CustomConfig = []string{"DOWN-AFTER-MILLISECONDS 120000", "Failover-Timeout 30000"}
			},
			window: "210",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rf := generateRF()
			test.modify(rf)
			var gotCM *corev1.ConfigMap
			ms := &mK8SService.Services{}
			ms.On("CreateOrUpdateConfigMap", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
				gotCM = args.Get(1).(*corev1.ConfigMap)
			}).Return(nil)
			client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
			require.NoError(t, client.EnsureRedisReadinessConfigMap(rf, nil, []metav1.OwnerReference{}))
			assert.Contains(t, gotCM.Data["ready.sh"], "\nMAX_LINK_DOWN_SECONDS="+test.window+"\n")
		})
	}
}

// skipOrFail skips a test whose tool is missing on a developer machine. In CI
// the tool must exist, so a missing tool fails the test, and the script tests
// cannot stop running without notice.
func skipOrFail(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv("CI") != "" {
		t.Fatalf(format, args...)
	}
	t.Skipf(format, args...)
}
