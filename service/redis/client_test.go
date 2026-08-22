package redis

// Tests in this file exercise service/redis's Client against real
// redis-server / redis-server --sentinel subprocesses (see testutil_test.go
// for the harness). This package previously had zero test coverage even
// though it is the entire wire-protocol layer the operator uses to talk to
// Redis and Sentinel.
//
// Two behaviours here were verified empirically against real Redis 7.0.15
// rather than assumed, per the investigation that prompted this test suite:
//   - SENTINEL CKQUORUM's NOQUORUM outcome (see TestSentinelCheckQuorum_NoQuorum).
//   - CONFIG SET aclfile's immutability (see TestSetCustomRedisConfig_ACLFile).
// Both surfaced apparent bugs in client.go; see the comments on those tests.
// Per the task instructions, these are reported, not fixed, here.

import (
	"net"
	"os"
	"strconv"
	"testing"
	"time"

	rediscli "github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/saremox/redis-operator/metrics"
)

func newTestClient() Client {
	return New(metrics.Dummy)
}

// ---------------------------------------------------------------------
// IsMaster / GetSlaveOf / SlaveIsReady / GetReplicationInfo
// (shared read-only master+replica+sentinel environment)
// ---------------------------------------------------------------------

func TestIsMaster(t *testing.T) {
	env := getSharedEnv(t)
	c := newTestClient()

	isMaster, err := c.IsMaster(env.master.IP, strconv.Itoa(env.master.Port), "")
	require.NoError(t, err)
	assert.True(t, isMaster, "a freshly started standalone redis-server should report role:master")

	isMaster, err = c.IsMaster(env.replica.IP, strconv.Itoa(env.replica.Port), "")
	require.NoError(t, err)
	assert.False(t, isMaster, "a replica should not report role:master")
}

func TestGetSlaveOf(t *testing.T) {
	env := getSharedEnv(t)
	c := newTestClient()

	masterOf, err := c.GetSlaveOf(env.master.IP, strconv.Itoa(env.master.Port), "")
	require.NoError(t, err)
	assert.Empty(t, masterOf, "a master has no master_host, so GetSlaveOf should return empty string")

	masterOf, err = c.GetSlaveOf(env.replica.IP, strconv.Itoa(env.replica.Port), "")
	require.NoError(t, err)
	assert.Equal(t, env.master.IP, masterOf)
}

// TestSlaveIsReady_LoopbackMasterHostNeverReady documents a real quirk found
// while building this test suite (not fixed here, per instructions - see the
// task summary for the full report):
//
// SlaveIsReady's readiness check is:
//
//	ok := !strings.Contains(info, redisSyncing) &&
//	    !strings.Contains(info, redisMasterSillPending) &&
//	    strings.Contains(info, redisLinkUp)
//
// where redisMasterSillPending is the literal string "master_host:127.0.0.1".
// That's presumably meant to catch a transitional state (a pod that hasn't
// picked up its real master's address yet), but it's implemented as an exact
// match on the loopback address rather than on some other placeholder value.
// The practical effect: a replica whose master genuinely *is* reachable at
// 127.0.0.1 is reported "not ready" by SlaveIsReady forever, even once
// replication is fully caught up (master_link_status:up) - because the
// string "master_host:127.0.0.1" is present in INFO replication regardless
// of sync state. This is exactly the shared test master/replica pair in this
// package (everything here binds to 127.0.0.1), so it's confirmed directly
// below rather than asserted as a TODO.
func TestSlaveIsReady_LoopbackMasterHostNeverReady(t *testing.T) {
	env := getSharedEnv(t)
	c := newTestClient()

	// Give replication as long as we reasonably can to fully catch up, to
	// make sure what we're observing is the master_host:127.0.0.1 quirk and
	// not merely "still syncing".
	waitForCondition(t, 10*time.Second, func() bool {
		info, err := c.GetReplicationInfo(env.replica.IP, strconv.Itoa(env.replica.Port), "")
		return err == nil && info.MasterLinkStatus == "up" && !info.SyncInProgress
	})
	info, err := c.GetReplicationInfo(env.replica.IP, strconv.Itoa(env.replica.Port), "")
	require.NoError(t, err)
	require.Equal(t, "up", info.MasterLinkStatus, "replication should be fully caught up before this assertion is meaningful")

	ok, err := c.SlaveIsReady(env.replica.IP, strconv.Itoa(env.replica.Port), "")
	require.NoError(t, err)
	assert.False(t, ok, "SlaveIsReady incorrectly reports not-ready whenever master_host is literally 127.0.0.1, regardless of actual sync state - see comment above")

	// A master has no master_link_status line at all, so SlaveIsReady should
	// report false for it too (but for the entirely unrelated, correct
	// reason that it just isn't a slave).
	ok, err = c.SlaveIsReady(env.master.IP, strconv.Itoa(env.master.Port), "")
	require.NoError(t, err)
	assert.False(t, ok)
}

// TestSlaveIsReady_NonLoopbackMaster is the positive-path counterpart to
// TestSlaveIsReady_LoopbackMasterHostNeverReady: with a master reachable at
// a non-loopback address, SlaveIsReady works as intended. Skipped if this
// machine has no non-loopback IPv4 address (see nonLoopbackIPv4).
func TestSlaveIsReady_NonLoopbackMaster(t *testing.T) {
	requireRedisServer(t)
	ip, ok := nonLoopbackIPv4()
	if !ok {
		t.Skip("no non-loopback IPv4 address available on this machine")
	}
	port, err := findFreePort()
	require.NoError(t, err)
	master := startRedisProcessOnAddr(t, ip, port)
	replica := startReplicaOf(t, master)
	c := newTestClient()

	ready := waitForCondition(t, 10*time.Second, func() bool {
		ready, err := c.SlaveIsReady(replica.IP, strconv.Itoa(replica.Port), "")
		return err == nil && ready
	})
	assert.True(t, ready, "replica of a non-loopback master should eventually report ready")
}

func TestGetReplicationInfo_Master(t *testing.T) {
	env := getSharedEnv(t)
	c := newTestClient()

	// Make sure the replica has been discovered as connected before
	// asserting on connected_slaves.
	waitForCondition(t, 10*time.Second, func() bool {
		info, err := c.GetReplicationInfo(env.master.IP, strconv.Itoa(env.master.Port), "")
		return err == nil && info.ConnectedSlaves >= 1
	})

	info, err := c.GetReplicationInfo(env.master.IP, strconv.Itoa(env.master.Port), "")
	require.NoError(t, err)
	assert.Equal(t, "master", info.Role)
	assert.GreaterOrEqual(t, info.ConnectedSlaves, 1)
	assert.GreaterOrEqual(t, info.MasterReplOffset, int64(0))
	assert.False(t, info.SyncInProgress)
}

func TestGetReplicationInfo_Replica(t *testing.T) {
	env := getSharedEnv(t)
	c := newTestClient()

	var info *ReplicationInfo
	waitForCondition(t, 10*time.Second, func() bool {
		var err error
		info, err = c.GetReplicationInfo(env.replica.IP, strconv.Itoa(env.replica.Port), "")
		return err == nil && info.MasterLinkStatus == "up"
	})

	require.NotNil(t, info)
	assert.Equal(t, "slave", info.Role)
	assert.Equal(t, env.master.IP, info.MasterHost)
	assert.Equal(t, strconv.Itoa(env.master.Port), info.MasterPort)
	assert.Equal(t, "up", info.MasterLinkStatus)
	assert.False(t, info.SyncInProgress)
	assert.GreaterOrEqual(t, info.SlaveReplOffset, int64(0))
}

// ---------------------------------------------------------------------
// MakeMaster / MakeSlaveOf / MakeSlaveOfWithPort
// (dedicated throwaway topologies - these mutate replication state)
// ---------------------------------------------------------------------

func TestMakeMaster(t *testing.T) {
	requireRedisServer(t)
	master := startRedisProcess(t)
	replica := startReplicaOf(t, master)
	c := newTestClient()

	// Wait for the replica to actually become a slave before flipping it
	// back, so the test exercises a real slave->master transition.
	waitForCondition(t, 5*time.Second, func() bool {
		isMaster, err := c.IsMaster(replica.IP, strconv.Itoa(replica.Port), "")
		return err == nil && !isMaster
	})

	err := c.MakeMaster(replica.IP, strconv.Itoa(replica.Port), "")
	require.NoError(t, err)

	waitForCondition(t, 5*time.Second, func() bool {
		isMaster, err := c.IsMaster(replica.IP, strconv.Itoa(replica.Port), "")
		return err == nil && isMaster
	})
	isMaster, err := c.IsMaster(replica.IP, strconv.Itoa(replica.Port), "")
	require.NoError(t, err)
	assert.True(t, isMaster)
}

// TestMakeSlaveOfWithPort_SamePortDifferentIP is the positive-path test for
// MakeSlaveOfWithPort, using two independently-addressable instances that
// happen to share one port number - the real-world shape (distinct pod IPs,
// identical configured Redis port) that MakeSlaveOfWithPort's connection
// logic actually requires. See the comment on
// TestMakeSlaveOfWithPort_MismatchedTargetPort for why this matters and is
// tested separately from the mismatched-port case. Skipped if this machine
// has no non-loopback IPv4 address.
func TestMakeSlaveOfWithPort_SamePortDifferentIP(t *testing.T) {
	requireRedisServer(t)
	otherIP, ok := nonLoopbackIPv4()
	if !ok {
		t.Skip("no non-loopback IPv4 address available on this machine")
	}
	port, err := findFreePort()
	require.NoError(t, err)

	a := startRedisProcessOnAddr(t, testLoopbackIP, port)
	b := startRedisProcessOnAddr(t, otherIP, port)
	c := newTestClient()

	// Sanity check: both start out as independent masters.
	isMaster, err := c.IsMaster(a.IP, strconv.Itoa(a.Port), "")
	require.NoError(t, err)
	require.True(t, isMaster)

	err = c.MakeSlaveOfWithPort(a.IP, b.IP, strconv.Itoa(b.Port), "")
	require.NoError(t, err)

	waitForCondition(t, 5*time.Second, func() bool {
		masterOf, err := c.GetSlaveOf(a.IP, strconv.Itoa(a.Port), "")
		return err == nil && masterOf == b.IP
	})
	masterOf, err := c.GetSlaveOf(a.IP, strconv.Itoa(a.Port), "")
	require.NoError(t, err)
	assert.Equal(t, b.IP, masterOf)

	linkUp := waitForCondition(t, 10*time.Second, func() bool {
		info, err := c.GetReplicationInfo(a.IP, strconv.Itoa(a.Port), "")
		return err == nil && info.MasterLinkStatus == "up"
	})
	assert.True(t, linkUp)
}

// TestMakeSlaveOfWithPort_MismatchedTargetPort documents a real bug found
// while building this test suite (not fixed here, per instructions - see
// the task summary for the full report):
//
// MakeSlaveOfWithPort(ip, masterIP, masterPort, password) connects to the
// *target* redis instance (the one it's about to issue SLAVEOF against) at
// `net.JoinHostPort(ip, masterPort)` - i.e. it uses the *master's* port to
// reach the target, with no separate parameter for the target's own port.
// That's only correct when the target and the master happen to listen on
// the same port number (true in this operator's normal deployment model,
// where every managed Redis instance uses one shared configured port across
// different pod IPs - see the SamePortDifferentIP test above for that
// case). When the target's actual port differs from the master's port, this
// silently does the wrong thing rather than failing loudly:
//
// Here `a` (the intended target) and `b` (the intended master) share the
// same IP (both loopback) but listen on *different* ports. Calling
// MakeSlaveOfWithPort(a.IP, b.IP, b.Port, "") computes the connection
// address as (a.IP, b.Port) - since a.IP == b.IP, that address actually
// belongs to `b`'s own server, not `a`'s. The client ends up connected to
// `b` and issues `SLAVEOF b.IP b.Port` *to b itself*. Real Redis accepts
// `SLAVEOF <self>` without error (confirmed manually against 7.0.15) and
// becomes a slave of itself with master_link_status permanently "down" -
// so the call returns a misleading nil error, `b` is left in a broken
// self-replicating state, and `a` (the actual intended target) is never
// contacted at all and remains an untouched master.
func TestMakeSlaveOfWithPort_MismatchedTargetPort(t *testing.T) {
	requireRedisServer(t)
	a := startRedisProcess(t)
	b := startRedisProcess(t)
	c := newTestClient()
	require.NotEqual(t, a.Port, b.Port, "test setup requires distinct ports to reproduce the bug")

	err := c.MakeSlaveOfWithPort(a.IP, b.IP, strconv.Itoa(b.Port), "")
	require.NoError(t, err, "the call itself does not surface an error - that's the point of this bug")

	// `a`, the intended target, was never actually reached.
	masterOf, err := c.GetSlaveOf(a.IP, strconv.Itoa(a.Port), "")
	require.NoError(t, err)
	assert.Empty(t, masterOf, "the intended target should be untouched, still a master")

	// `b` was told to replicate from itself instead.
	waitForCondition(t, 2*time.Second, func() bool {
		masterOf, err := c.GetSlaveOf(b.IP, strconv.Itoa(b.Port), "")
		return err == nil && masterOf == b.IP
	})
	masterOf, err = c.GetSlaveOf(b.IP, strconv.Itoa(b.Port), "")
	require.NoError(t, err)
	assert.Equal(t, b.IP, masterOf, "the master ends up pointed at itself instead of the target being reconfigured")
}

// TestMakeSlaveOf covers the MakeSlaveOf convenience wrapper, which hardcodes
// the master port to redisPort ("6379") rather than accepting one - see
// MakeSlaveOfWithPort. Like TestMakeSlaveOfWithPort_SamePortDifferentIP, it
// needs the target and master on the same port at different IPs for
// MakeSlaveOfWithPort's connection-address logic to reach the right
// instance (see TestMakeSlaveOfWithPort_MismatchedTargetPort). Skipped if
// this machine has no non-loopback IPv4 address, or if port 6379 is
// already in use.
func TestMakeSlaveOf(t *testing.T) {
	requireRedisServer(t)
	otherIP, ok := nonLoopbackIPv4()
	if !ok {
		t.Skip("no non-loopback IPv4 address available on this machine")
	}
	if !isPortFree(t, testLoopbackIP, 6379) || !isPortFree(t, otherIP, 6379) {
		t.Skip("port 6379 is already in use on this machine; skipping MakeSlaveOf (hardcoded redisPort) test")
	}

	master := startRedisProcessOnAddr(t, otherIP, 6379)
	slave := startRedisProcessOnAddr(t, testLoopbackIP, 6379)
	c := newTestClient()

	err := c.MakeSlaveOf(slave.IP, master.IP, "")
	require.NoError(t, err)

	waitForCondition(t, 5*time.Second, func() bool {
		masterOf, err := c.GetSlaveOf(slave.IP, strconv.Itoa(slave.Port), "")
		return err == nil && masterOf == master.IP
	})
	masterOf, err := c.GetSlaveOf(slave.IP, strconv.Itoa(slave.Port), "")
	require.NoError(t, err)
	assert.Equal(t, master.IP, masterOf)
}

func isPortFree(t *testing.T, ip string, port int) bool {
	t.Helper()
	l, err := net.Listen("tcp", net.JoinHostPort(ip, strconv.Itoa(port)))
	if err != nil {
		return false
	}
	_ = l.Close()
	return true
}

// ---------------------------------------------------------------------
// SetCustomRedisConfig
// ---------------------------------------------------------------------

func TestSetCustomRedisConfig_Basic(t *testing.T) {
	requireRedisServer(t)
	r := startRedisProcess(t)
	c := newTestClient()

	err := c.SetCustomRedisConfig(r.IP, strconv.Itoa(r.Port), []string{"maxmemory-policy allkeys-lru"}, "")
	require.NoError(t, err)

	rc := rediscli.NewClient(&rediscli.Options{Addr: r.Addr()})
	defer func() { _ = rc.Close() }()
	res, err := rc.ConfigGet(bgCtx(), "maxmemory-policy").Result()
	require.NoError(t, err)
	require.Len(t, res, 2)
	assert.Equal(t, "allkeys-lru", res[1])
}

// TestSetCustomRedisConfig_EmptyValue covers the `config set save ""` case
// called out explicitly in a comment in SetCustomRedisConfig.
func TestSetCustomRedisConfig_EmptyValue(t *testing.T) {
	requireRedisServer(t)
	r := startRedisProcess(t)
	c := newTestClient()

	err := c.SetCustomRedisConfig(r.IP, strconv.Itoa(r.Port), []string{`save ""`}, "")
	require.NoError(t, err)

	rc := rediscli.NewClient(&rediscli.Options{Addr: r.Addr()})
	defer func() { _ = rc.Close() }()
	res, err := rc.ConfigGet(bgCtx(), "save").Result()
	require.NoError(t, err)
	require.Len(t, res, 2)
	assert.Equal(t, "", res[1])
}

func TestSetCustomRedisConfig_Malformed(t *testing.T) {
	requireRedisServer(t)
	r := startRedisProcess(t)
	c := newTestClient()

	err := c.SetCustomRedisConfig(r.IP, strconv.Itoa(r.Port), []string{"onewordonly"}, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "malformed")
}

// TestSetCustomRedisConfig_ACLFile documents a real bug found while building
// this test suite (not fixed here, per instructions - see the task summary
// for the full report):
//
// `CONFIG SET aclfile <path>` is an *immutable* config in real Redis (7.0.15,
// confirmed here; Redis docs mark aclfile as requiring a server restart to
// change) - the server rejects it with "ERR CONFIG SET failed... can't set
// immutable config", regardless of whether the value being set matches the
// aclfile Redis was already started with. This is true even when the target
// file exists.
//
// client.go's SetCustomRedisConfig loop calls applyRedisConfig (CONFIG SET)
// for every parameter *before* checking whether it was "aclfile" and
// triggering applyACLLoad (ACL LOAD). Against real Redis, the CONFIG SET
// aclfile call above fails and SetCustomRedisConfig returns that error
// immediately - meaning applyACLLoad is unreachable dead code whenever the
// custom config list contains an "aclfile" line, and RedisFailover CRs that
// set `spec.redis.customConfig` to include an aclfile line (the exact
// scenario commit 8add5ac "Fix aclfile in CustomConfig silently not loading
// ACL users" was meant to fix) will always fail this call in practice.
func TestSetCustomRedisConfig_ACLFile(t *testing.T) {
	requireRedisServer(t)
	dir := t.TempDir()
	aclPath := dir + "/users.acl"
	require.NoError(t, os.WriteFile(aclPath, []byte("user testuser on >testpass ~* +@all\n"), 0o600))

	// Start with the aclfile already configured at boot, which is the only
	// way Redis will accept it at all.
	r := startRedisProcess(t, "--aclfile", aclPath)
	c := newTestClient()

	err := c.SetCustomRedisConfig(r.IP, strconv.Itoa(r.Port), []string{"aclfile " + aclPath}, "")
	require.Error(t, err, "CONFIG SET aclfile is immutable in real Redis; this documents current (buggy) behavior, see comment above")
	assert.Contains(t, err.Error(), "immutable")
}

// ---------------------------------------------------------------------
// Sentinel: GetSentinelMonitor / GetNumberSentinelsInMemory /
// GetNumberSentinelSlavesInMemory (shared read-only sentinel)
// ---------------------------------------------------------------------

func TestGetSentinelMonitor(t *testing.T) {
	env := getSharedEnv(t)
	c := newTestClient()

	ip, port, err := c.GetSentinelMonitor(env.sentinel.IP)
	require.NoError(t, err)
	assert.Equal(t, env.master.IP, ip)
	assert.Equal(t, strconv.Itoa(env.master.Port), port)
}

func TestGetNumberSentinelsInMemory(t *testing.T) {
	env := getSharedEnv(t)
	c := newTestClient()

	n, err := c.GetNumberSentinelsInMemory(env.sentinel.IP)
	require.NoError(t, err)
	assert.EqualValues(t, 1, n, "only one sentinel process is running, monitoring itself")
}

func TestGetNumberSentinelSlavesInMemory(t *testing.T) {
	env := getSharedEnv(t)
	c := newTestClient()

	// Sentinel discovers replicas by periodically polling INFO on the
	// master it monitors; this can take several seconds after the replica
	// first attaches.
	ok := waitForCondition(t, 20*time.Second, func() bool {
		n, err := c.GetNumberSentinelSlavesInMemory(env.sentinel.IP)
		return err == nil && n >= 1
	})
	require.True(t, ok, "sentinel should eventually discover the replica")

	n, err := c.GetNumberSentinelSlavesInMemory(env.sentinel.IP)
	require.NoError(t, err)
	assert.EqualValues(t, 1, n)
}

// TestGetNumberSentinelsInMemory_NotMonitoringAnything temporarily strips
// the shared sentinel's only monitored master (SENTINEL REMOVE) rather than
// starting a second sentinel process - see newSentinelProcessOnPort's doc
// comment for why there can only be one. It restores the shared sentinel's
// normal monitoring state via t.Cleanup so later tests aren't affected.
func TestGetNumberSentinelsInMemory_NotMonitoringAnything(t *testing.T) {
	env := getSharedEnv(t)
	c := newTestClient()
	t.Cleanup(func() { restoreSharedSentinel(t, env) })

	rc := rediscli.NewSentinelClient(&rediscli.Options{Addr: env.sentinel.Addr()})
	defer func() { _ = rc.Close() }()
	removeCmd := rediscli.NewBoolCmd(bgCtx(), "SENTINEL", "REMOVE", masterName)
	require.NoError(t, rc.Process(bgCtx(), removeCmd))
	_, err := removeCmd.Result()
	require.NoError(t, err)

	// A sentinel monitoring nothing has no "status=" line to report ok for,
	// so isSentinelReady should reject it.
	_, err = c.GetNumberSentinelsInMemory(env.sentinel.IP)
	assert.Error(t, err)
}

// ---------------------------------------------------------------------
// MonitorRedisWithPort / SetCustomSentinelConfig / ResetSentinel /
// SentinelCheckQuorum
//
// These all mutate sentinel state, but per newSentinelProcessOnPort's doc
// comment there can only be one sentinel process reachable through Client
// at a time (it always connects to the hardcoded sentinelPort). So each of
// these repoints the single shared sentinel at its own dedicated throwaway
// master (rather than starting a second sentinel process) and restores it
// back to the shared master via t.Cleanup. Go runs tests within a package
// sequentially by default (none of these call t.Parallel), so this is safe.
// ---------------------------------------------------------------------

// TestMonitorRedis covers the MonitorRedis convenience wrapper, which
// hardcodes the monitored master's port to redisPort ("6379") when calling
// MonitorRedisWithPort. Unlike MakeSlaveOf's use of the same constant, this
// doesn't require anything actually listening on 6379: the port is only
// ever used as a text argument to `SENTINEL MONITOR`, never as a connection
// address (MonitorRedisWithPort always connects to the sentinel itself, on
// sentinelPort), so this can safely run even where port 6379 is unavailable
// or already in use.
func TestMonitorRedis(t *testing.T) {
	env := getSharedEnv(t)
	c := newTestClient()
	t.Cleanup(func() { restoreSharedSentinel(t, env) })

	err := c.MonitorRedis(env.sentinel.IP, testLoopbackIP, "1", "")
	require.NoError(t, err)

	ip, port, err := c.GetSentinelMonitor(env.sentinel.IP)
	require.NoError(t, err)
	assert.Equal(t, testLoopbackIP, ip)
	assert.Equal(t, redisPort, port)
}

func TestMonitorRedisWithPort(t *testing.T) {
	env := getSharedEnv(t)
	master := startRedisProcess(t)
	c := newTestClient()
	t.Cleanup(func() { restoreSharedSentinel(t, env) })

	err := c.MonitorRedisWithPort(env.sentinel.IP, master.IP, strconv.Itoa(master.Port), "1", "")
	require.NoError(t, err)

	ip, port, err := c.GetSentinelMonitor(env.sentinel.IP)
	require.NoError(t, err)
	assert.Equal(t, master.IP, ip)
	assert.Equal(t, strconv.Itoa(master.Port), port)
}

func TestMonitorRedisWithPort_WithPassword(t *testing.T) {
	env := getSharedEnv(t)
	master := startRedisProcess(t)
	c := newTestClient()
	t.Cleanup(func() { restoreSharedSentinel(t, env) })

	// MonitorRedisWithPort unconditionally issues `SENTINEL SET mymaster
	// auth-pass <password>` when a password is given; this only asserts
	// that call itself succeeds against sentinel (it does not require the
	// master to actually be configured with a matching requirepass).
	err := c.MonitorRedisWithPort(env.sentinel.IP, master.IP, strconv.Itoa(master.Port), "1", "s3cr3t")
	require.NoError(t, err)
}

func TestSetCustomSentinelConfig(t *testing.T) {
	env := getSharedEnv(t)
	master := startRedisProcess(t)
	c := newTestClient()
	t.Cleanup(func() { restoreSharedSentinel(t, env) })
	pointSharedSentinelAt(t, env.sentinel, master.IP, master.Port, 1)

	err := c.SetCustomSentinelConfig(env.sentinel.IP, []string{"down-after-milliseconds 12345"})
	require.NoError(t, err)

	rc := rediscli.NewSentinelClient(&rediscli.Options{Addr: env.sentinel.Addr()})
	defer func() { _ = rc.Close() }()
	res, err := rc.Master(bgCtx(), masterName).Result()
	require.NoError(t, err)
	assert.Equal(t, "12345", res["down-after-milliseconds"])
}

func TestResetSentinel(t *testing.T) {
	env := getSharedEnv(t)
	master := startRedisProcess(t)
	c := newTestClient()
	t.Cleanup(func() { restoreSharedSentinel(t, env) })
	pointSharedSentinelAt(t, env.sentinel, master.IP, master.Port, 1)

	err := c.ResetSentinel(env.sentinel.IP)
	require.NoError(t, err)
}

func TestSentinelCheckQuorum_OK(t *testing.T) {
	env := getSharedEnv(t)
	c := newTestClient()

	err := c.SentinelCheckQuorum(env.sentinel.IP)
	assert.NoError(t, err)
}

// TestSentinelCheckQuorum_NoQuorum documents a real bug found while building
// this test suite (not fixed here, per instructions - see the task summary
// for the full report):
//
// Real Sentinel's CKQUORUM reply for the NOQUORUM case comes back as a RESP
// *error* reply (confirmed here against real Redis 7.0.15: `redis-cli
// --no-raw` shows `(error) NOQUORUM ...`), not as a successful string reply
// whose text happens to start with the literal characters "(error)". Since
// go-redis's SentinelClient.CkQuorum uses a StringCmd, that RESP error
// becomes cmd.Err()/the err returned by cmd.Result() - so
// client.go's SentinelCheckQuorum takes its `if err != nil { ... return err
// }` branch immediately.
//
// The subsequent code - `s := strings.Split(res, " ")` followed by `if
// status == "(error)" && quorum == "NOQUORUM"` - can therefore never
// execute on a real NOQUORUM response: `res` is only non-empty when err is
// nil, i.e. only for the OK case, so status will always be "OK" whenever
// that branch is reached. In other words, the code's own intended NOQUORUM
// handling (returning `errors.New("quorum Not available")`) is dead code;
// what actually gets returned to callers today is the raw driver error
// (whose message happens to also mention NOQUORUM, so callers checking
// err != nil for failure still work correctly - this is a
// dead-code/messaging bug, not a functional regression for existing
// callers as far as we can tell).
func TestSentinelCheckQuorum_NoQuorum(t *testing.T) {
	env := getSharedEnv(t)
	master := startRedisProcess(t)
	c := newTestClient()
	t.Cleanup(func() { restoreSharedSentinel(t, env) })

	// Ask sentinel to monitor with a quorum of 2, which a single sentinel
	// process can never satisfy.
	err := c.MonitorRedisWithPort(env.sentinel.IP, master.IP, strconv.Itoa(master.Port), "2", "")
	require.NoError(t, err)

	err = c.SentinelCheckQuorum(env.sentinel.IP)
	require.Error(t, err, "CKQUORUM should fail when quorum exceeds the number of known sentinels")
}
