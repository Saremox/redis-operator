package redis

import (
	"context"
	"net"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	rediscli "github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// failoverPair starts a master on 127.0.0.1 and its replica on 127.0.0.2,
// on the same port, as pods of a RedisFailover use one port. The replica
// announces its address, because its connection to the master comes from
// 127.0.0.1 and FAILOVER TO looks the target up by address.
func failoverPair(t *testing.T, masterArgs ...string) (master, replica *redisProc) {
	t.Helper()
	requireRedisServer(t)
	port, err := findFreePort()
	require.NoError(t, err)
	master = startRedisProcessOnAddr(t, testLoopbackIP, port, masterArgs...)
	replica = startRedisProcessOnAddr(t, "127.0.0.2", port,
		"--replicaof", master.IP, strconv.Itoa(port), "--replica-announce-ip", "127.0.0.2")
	c := newTestClient()
	// SlaveIsReady treats a master on 127.0.0.1 as not set, so read the link.
	require.True(t, waitForCondition(t, 10*time.Second, func() bool {
		info, err := c.GetReplicationInfo(replica.IP, strconv.Itoa(port), "")
		return err == nil && info.MasterLinkStatus == "up" && !info.SyncInProgress
	}), "the replica did not sync")
	// The replica reports the link up before the master has it online, for
	// example while the RDB child of a diskless sync runs. FAILOVER TO needs
	// an online replica.
	m := rediscli.NewClient(redisOptions(master.Addr(), ""))
	defer closeClient(m)
	require.True(t, waitForCondition(t, 10*time.Second, func() bool {
		info, err := m.Info(context.Background(), "replication").Result()
		return err == nil && strings.Contains(info, "ip=127.0.0.2,port="+strconv.Itoa(port)+",state=online")
	}), "the master does not have the replica online")
	info, err := c.GetReplicationInfo(master.IP, strconv.Itoa(port), "")
	require.NoError(t, err)
	if info.FailoverState == "" {
		t.Skip("redis-server before 6.2 has no FAILOVER command")
	}
	return master, replica
}

func waitForFailoverEnd(t *testing.T, ip string, port int) *ReplicationInfo {
	t.Helper()
	var info *ReplicationInfo
	require.True(t, waitForCondition(t, 10*time.Second, func() bool {
		var err error
		info, err = newTestClient().GetReplicationInfo(ip, strconv.Itoa(port), "")
		return err == nil && info.FailoverState == "no-failover"
	}))
	return info
}

// The replica gets every write that the master acknowledged before the
// failover, and the old master follows it.
func TestFailoverToMovesTheMasterRole(t *testing.T) {
	master, replica := failoverPair(t)
	port := strconv.Itoa(master.Port)
	m := rediscli.NewClient(redisOptions(master.Addr(), ""))
	defer closeClient(m)
	for i := range 100 {
		require.NoError(t, m.Set(context.Background(), "k"+strconv.Itoa(i), i, 0).Err())
	}

	c := newTestClient()
	require.NoError(t, c.FailoverTo(master.IP, port, "", replica.IP, 2*time.Second))

	info := waitForFailoverEnd(t, master.IP, master.Port)
	assert.Equal(t, "slave", info.Role)
	assert.Equal(t, replica.IP, info.MasterHost)
	isMaster, err := c.IsMaster(replica.IP, port, "")
	require.NoError(t, err)
	assert.True(t, isMaster)
	r := rediscli.NewClient(redisOptions(replica.Addr(), ""))
	defer closeClient(r)
	assert.Equal(t, int64(100), r.DBSize(context.Background()).Val())
}

// Without FORCE, a target that does not reach the offset in time makes the
// master abort. The master then continues and accepts writes again.
func TestFailoverToAbortsOnTheTimeout(t *testing.T) {
	master, replica := failoverPair(t)
	port := strconv.Itoa(master.Port)
	require.NoError(t, replica.cmd.Process.Signal(syscall.SIGSTOP))
	defer func() { _ = replica.cmd.Process.Signal(syscall.SIGCONT) }()
	m := rediscli.NewClient(redisOptions(master.Addr(), ""))
	defer closeClient(m)
	require.NoError(t, m.Set(context.Background(), "after-stop", 1, 0).Err())

	c := newTestClient()
	require.NoError(t, c.FailoverTo(master.IP, port, "", replica.IP, 300*time.Millisecond))

	info := waitForFailoverEnd(t, master.IP, master.Port)
	assert.Equal(t, "master", info.Role)
	assert.NoError(t, m.Set(context.Background(), "after-abort", 1, 0).Err())
}

func TestFailoverToRefusesATargetThatIsNotAReplica(t *testing.T) {
	master, _ := failoverPair(t)

	err := newTestClient().FailoverTo(master.IP, strconv.Itoa(master.Port), "", "127.0.0.3", time.Second)

	require.Error(t, err)
	assert.Regexp(t, "^ERR FAILOVER target", err.Error())
}

func TestFailoverToRenamedCommand(t *testing.T) {
	requireRedisServer(t)
	// Redis before 6.2 does not start with a rename of an unknown command.
	plain := startRedisProcess(t)
	info, err := newTestClient().GetReplicationInfo(plain.IP, strconv.Itoa(plain.Port), "")
	require.NoError(t, err)
	if info.FailoverState == "" {
		t.Skip("redis-server before 6.2 has no FAILOVER command")
	}
	master := startRedisProcess(t, "--rename-command", "FAILOVER", "")

	err = newTestClient().FailoverTo(master.IP, strconv.Itoa(master.Port), "", "127.0.0.2", time.Second)

	require.Error(t, err)
	assert.Regexp(t, "^ERR unknown command", err.Error())
}

func TestFailoverToConnectionError(t *testing.T) {
	port, err := findFreePort()
	require.NoError(t, err)
	c := newTestClient()

	assert.Error(t, c.FailoverTo(testLoopbackIP, strconv.Itoa(port), "", "127.0.0.2", time.Second))
	assert.ErrorIs(t, c.FailoverTo("", strconv.Itoa(port), "", "127.0.0.2", time.Second), errNoIP)
}

// A writer runs during the failover. Each write that the old master
// acknowledged must be on the new master, and no write waits much longer
// than the pause.
func TestFailoverToLosesNoAcknowledgedWrite(t *testing.T) {
	master, replica := failoverPair(t)
	port := strconv.Itoa(master.Port)
	m := rediscli.NewClient(redisOptions(master.Addr(), ""))
	defer closeClient(m)

	stop := make(chan struct{})
	type result struct {
		acked   []string
		longest time.Duration
	}
	done := make(chan result)
	go func() {
		var res result
		for i := 0; ; i++ {
			select {
			case <-stop:
				done <- res
				return
			default:
			}
			key := "w" + strconv.Itoa(i)
			start := time.Now()
			err := m.Set(context.Background(), key, i, 0).Err()
			res.longest = max(res.longest, time.Since(start))
			if err == nil {
				res.acked = append(res.acked, key)
			}
		}
	}()
	time.Sleep(200 * time.Millisecond)

	const pause = 2 * time.Second
	require.NoError(t, newTestClient().FailoverTo(master.IP, port, "", replica.IP, pause))
	info := waitForFailoverEnd(t, master.IP, master.Port)
	time.Sleep(200 * time.Millisecond)
	close(stop)
	res := <-done

	require.Equal(t, "slave", info.Role)
	require.NotEmpty(t, res.acked)
	r := rediscli.NewClient(redisOptions(replica.Addr(), ""))
	defer closeClient(r)
	missing := 0
	for _, key := range res.acked {
		if r.Exists(context.Background(), key).Val() == 0 {
			missing++
		}
	}
	t.Logf("%d acknowledged writes, longest write %s", len(res.acked), res.longest)
	assert.Zero(t, missing, "acknowledged writes missing on the new master")
	assert.Less(t, res.longest, pause+time.Second)
}

// waitForAck waits until the master has the acknowledgement of the whole
// replication stream from the replica.
func waitForAck(t *testing.T, master *redisProc) {
	t.Helper()
	m := rediscli.NewClient(redisOptions(master.Addr(), ""))
	defer closeClient(m)
	require.True(t, waitForCondition(t, 10*time.Second, func() bool {
		info, err := m.Info(context.Background(), "replication").Result()
		if err != nil {
			return false
		}
		var offset, acked string
		for _, line := range strings.Split(info, "\r\n") {
			if v, ok := strings.CutPrefix(line, "master_repl_offset:"); ok {
				offset = v
			}
			if strings.HasPrefix(line, "slave0:") {
				if _, after, ok := strings.Cut(line, ",offset="); ok {
					acked, _, _ = strings.Cut(after, ",")
				}
			}
		}
		return offset != "" && offset == acked
	}), "the replica did not acknowledge the replication stream")
}

// A target that stops after the catch-up keeps the old master in the role
// change, a replica that accepts no writes. FAILOVER ABORT makes it the master
// again with all its data, and the target cannot take the role later.
func TestFailoverAbortEndsAStalledRoleChange(t *testing.T) {
	master, replica := failoverPair(t)
	port := strconv.Itoa(master.Port)
	m := rediscli.NewClient(redisOptions(master.Addr(), ""))
	defer closeClient(m)
	for i := range 150 {
		require.NoError(t, m.Set(context.Background(), "k"+strconv.Itoa(i), i, 0).Err())
	}
	waitForAck(t, master)
	require.NoError(t, replica.cmd.Process.Signal(syscall.SIGSTOP))
	stopped := true
	defer func() {
		if stopped {
			_ = replica.cmd.Process.Signal(syscall.SIGCONT)
		}
	}()

	c := newTestClient()
	require.NoError(t, c.FailoverTo(master.IP, port, "", replica.IP, 2*time.Second))
	require.True(t, waitForCondition(t, 5*time.Second, func() bool {
		info, err := c.GetReplicationInfo(master.IP, port, "")
		return err == nil && info.FailoverState == "failover-in-progress"
	}), "the role change did not start")

	require.NoError(t, c.FailoverAbort(master.IP, port, ""))

	info, err := c.GetReplicationInfo(master.IP, port, "")
	require.NoError(t, err)
	assert.Equal(t, "no-failover", info.FailoverState)
	assert.Equal(t, "master", info.Role)
	assert.Equal(t, int64(150), m.DBSize(context.Background()).Val())
	require.NoError(t, m.Set(context.Background(), "after-abort", 1, 0).Err())

	// The target continues as a replica, and the master keeps its role.
	require.NoError(t, replica.cmd.Process.Signal(syscall.SIGCONT))
	stopped = false
	time.Sleep(time.Second)
	isMaster, err := c.IsMaster(master.IP, port, "")
	require.NoError(t, err)
	assert.True(t, isMaster)
	assert.Equal(t, int64(151), m.DBSize(context.Background()).Val())
}

// An abort after the end of the failover gets an error, and the caller reads
// the state again.
func TestFailoverAbortWithoutAFailover(t *testing.T) {
	master, _ := failoverPair(t)

	err := newTestClient().FailoverAbort(master.IP, strconv.Itoa(master.Port), "")

	require.Error(t, err)
	assert.Regexp(t, "^ERR No failover in progress", err.Error())
}

func TestFailoverAbortConnectionError(t *testing.T) {
	port, err := findFreePort()
	require.NoError(t, err)
	c := newTestClient()

	assert.Error(t, c.FailoverAbort(testLoopbackIP, strconv.Itoa(port), ""))
	assert.ErrorIs(t, c.FailoverAbort("", strconv.Itoa(port), ""), errNoIP)
}

// The run ID stays the same until the server restarts, and changes then.
func TestGetRunID(t *testing.T) {
	requireRedisServer(t)
	port, err := findFreePort()
	require.NoError(t, err)
	server := startRedisProcessOnAddr(t, testLoopbackIP, port)
	c := newTestClient()

	first, err := c.GetRunID(server.IP, strconv.Itoa(port), "")
	require.NoError(t, err)
	second, err := c.GetRunID(server.IP, strconv.Itoa(port), "")
	require.NoError(t, err)
	killProc(server)
	restarted := startRedisProcessOnAddr(t, testLoopbackIP, port)
	third, err := c.GetRunID(restarted.IP, strconv.Itoa(port), "")
	require.NoError(t, err)

	assert.Len(t, first, 40)
	assert.Equal(t, first, second)
	assert.NotEqual(t, first, third)
}

func TestGetRunIDErrors(t *testing.T) {
	port, err := findFreePort()
	require.NoError(t, err)
	c := newTestClient()

	_, err = c.GetRunID(testLoopbackIP, strconv.Itoa(port), "")
	assert.Error(t, err)
	_, err = c.GetRunID("", strconv.Itoa(port), "")
	assert.ErrorIs(t, err, errNoIP)

	// A server that answers INFO without run_id.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = l.Close() }()
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = conn.Read(make([]byte, 1024))
		_, _ = conn.Write([]byte("$5\r\nfoo:1\r\n"))
	}()
	host, p, err := net.SplitHostPort(l.Addr().String())
	require.NoError(t, err)
	_, err = c.GetRunID(host, p, "")
	assert.ErrorContains(t, err, "no run_id")
}

func TestGetFailoverState(t *testing.T) {
	requireRedisServer(t)
	server := startRedisProcess(t)
	c := newTestClient()

	state, err := c.GetFailoverState(server.IP, strconv.Itoa(server.Port), "")

	require.NoError(t, err)
	if state == "" {
		t.Skip("redis-server before 6.2 has no master_failover_state")
	}
	assert.Equal(t, "no-failover", state)
}

// fakeServer accepts connections. With reply, it answers the first request
// with reply. Without reply, it never answers, as a frozen node.
func fakeServer(t *testing.T, reply string) (string, string) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				_, _ = conn.Read(make([]byte, 1024))
				if reply != "" {
					_, _ = conn.Write([]byte(reply))
				}
				time.Sleep(3 * time.Second)
			}()
		}
	}()
	host, port, err := net.SplitHostPort(l.Addr().String())
	require.NoError(t, err)
	return host, port
}

// The check runs at each reconcile, so a node that does not answer must not
// hold it for the 4s of the default options.
func TestGetFailoverStateOfAFrozenNode(t *testing.T) {
	host, port := fakeServer(t, "")

	start := time.Now()
	_, err := newTestClient().GetFailoverState(host, port, "")

	assert.Error(t, err)
	assert.Less(t, time.Since(start), 1500*time.Millisecond)
}

func TestGetFailoverStateErrors(t *testing.T) {
	port, err := findFreePort()
	require.NoError(t, err)
	c := newTestClient()

	_, err = c.GetFailoverState(testLoopbackIP, strconv.Itoa(port), "")
	assert.Error(t, err)
	_, err = c.GetFailoverState("", strconv.Itoa(port), "")
	assert.ErrorIs(t, err, errNoIP)

	// Redis before 6.2 has no master_failover_state.
	host, p := fakeServer(t, "$11\r\nrole:master\r\n")
	state, err := c.GetFailoverState(host, p, "")
	assert.NoError(t, err)
	assert.Empty(t, state)
}
