package redis

import (
	"context"
	"strconv"
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
	master := startRedisProcess(t, "--rename-command", "FAILOVER", "")

	err := newTestClient().FailoverTo(master.IP, strconv.Itoa(master.Port), "", "127.0.0.2", time.Second)

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
