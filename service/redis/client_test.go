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
// The aclfile finding surfaced a real bug in client.go, fixed in the same
// change as this test (see the comment on TestSetCustomRedisConfig_ACLFile).

import (
	"context"
	"errors"
	"net"
	"os"
	"strconv"
	"strings"
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

// newTestClientStruct returns the concrete *client type (rather than the
// Client interface newTestClient returns) so tests can call unexported
// methods directly - used for applyACLLoad to test it in isolation from
// SetCustomRedisConfig's own aclfile handling.
func newTestClientStruct() *client {
	return &client{metricsRecorder: metrics.Dummy}
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
// Connection-error branches: IsMaster / GetSlaveOf / SlaveIsReady /
// GetReplicationInfo / MakeMaster / MakeSlaveOfWithPort all dial the target
// redis instance at an explicit ip:port the caller provides, so pointing
// them at a port nothing is listening on deterministically exercises their
// `if err != nil` branch right after the connection/INFO/command attempt -
// no real redis-server process is needed for any of these.
// ---------------------------------------------------------------------

func TestIsMaster_ConnectionError(t *testing.T) {
	port, err := findFreePort()
	require.NoError(t, err)
	c := newTestClient()

	_, err = c.IsMaster(testLoopbackIP, strconv.Itoa(port), "")
	assert.Error(t, err)
}

func TestGetSlaveOf_ConnectionError(t *testing.T) {
	port, err := findFreePort()
	require.NoError(t, err)
	c := newTestClient()

	_, err = c.GetSlaveOf(testLoopbackIP, strconv.Itoa(port), "")
	assert.Error(t, err)
}

func TestSlaveIsReady_ConnectionError(t *testing.T) {
	port, err := findFreePort()
	require.NoError(t, err)
	c := newTestClient()

	_, err = c.SlaveIsReady(testLoopbackIP, strconv.Itoa(port), "")
	assert.Error(t, err)
}

func TestGetReplicationInfo_ConnectionError(t *testing.T) {
	port, err := findFreePort()
	require.NoError(t, err)
	c := newTestClient()

	info, err := c.GetReplicationInfo(testLoopbackIP, strconv.Itoa(port), "")
	assert.Error(t, err)
	assert.Nil(t, info)
}

// ---------------------------------------------------------------------
// MakeMaster / MakeSlaveOf / MakeSlaveOfWithPort
// (dedicated throwaway topologies - these mutate replication state)
// ---------------------------------------------------------------------

func TestMakeMaster_ConnectionError(t *testing.T) {
	port, err := findFreePort()
	require.NoError(t, err)
	c := newTestClient()

	err = c.MakeMaster(testLoopbackIP, strconv.Itoa(port), "")
	assert.Error(t, err)
}

// TestMakeSlaveOfWithPort_ConnectionError points at a port nothing is
// listening on to exercise MakeSlaveOfWithPort's own connection-error
// branch. It intentionally does not attempt to also reach a real master -
// see TestMakeSlaveOfWithPort_MismatchedTargetPort for why the address
// MakeSlaveOfWithPort actually connects to is (ip, masterPort), not
// (ip, targetPort).
func TestMakeSlaveOfWithPort_ConnectionError(t *testing.T) {
	port, err := findFreePort()
	require.NoError(t, err)
	c := newTestClient()

	err = c.MakeSlaveOfWithPort(testLoopbackIP, "10.0.0.1", strconv.Itoa(port), "")
	assert.Error(t, err)
}

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

// TestSetCustomRedisConfig_EmptyParameterSkipped covers the
// `if strings.TrimSpace(param) == "" { continue }` branch in
// SetCustomRedisConfig: a config line whose parameter name is empty (e.g. a
// leading space before the value) is silently skipped rather than attempted
// as a CONFIG SET call, which would otherwise fail. The subsequent config in
// the list should still be applied normally.
func TestSetCustomRedisConfig_EmptyParameterSkipped(t *testing.T) {
	requireRedisServer(t)
	r := startRedisProcess(t)
	c := newTestClient()

	err := c.SetCustomRedisConfig(r.IP, strconv.Itoa(r.Port), []string{" ignored-value", "maxmemory-policy allkeys-lru"}, "")
	require.NoError(t, err)

	rc := rediscli.NewClient(&rediscli.Options{Addr: r.Addr()})
	defer func() { _ = rc.Close() }()
	res, err := rc.ConfigGet(bgCtx(), "maxmemory-policy").Result()
	require.NoError(t, err)
	require.Len(t, res, 2)
	assert.Equal(t, "allkeys-lru", res[1])
}

// ---------------------------------------------------------------------
// applyACLLoad
// ---------------------------------------------------------------------

func TestApplyACLLoad_Success(t *testing.T) {
	requireRedisServer(t)
	dir := t.TempDir()
	aclPath := dir + "/users.acl"
	require.NoError(t, os.WriteFile(aclPath, []byte("user testuser on >testpass ~* +@all\n"), 0o600))

	// The aclfile must already be configured at boot - it's the only way
	// real Redis will accept it (see TestSetCustomRedisConfig_ACLFile).
	r := startRedisProcess(t, "--aclfile", aclPath)
	c := newTestClientStruct()

	rc := rediscli.NewClient(&rediscli.Options{Addr: r.Addr()})
	defer func() { _ = rc.Close() }()

	err := c.applyACLLoad(rc)
	require.NoError(t, err, "ACL LOAD should succeed once an aclfile is configured")
}

func TestApplyACLLoad_NoACLFileConfigured(t *testing.T) {
	requireRedisServer(t)
	r := startRedisProcess(t)
	c := newTestClientStruct()

	rc := rediscli.NewClient(&rediscli.Options{Addr: r.Addr()})
	defer func() { _ = rc.Close() }()

	err := c.applyACLLoad(rc)
	require.Error(t, err, "ACL LOAD should fail when the server has no aclfile configured")
	assert.Contains(t, err.Error(), "not configured to use an ACL file")
}

func TestApplyACLLoad_ConnectionError(t *testing.T) {
	port, err := findFreePort()
	require.NoError(t, err)
	c := newTestClientStruct()

	rc := rediscli.NewClient(&rediscli.Options{Addr: net.JoinHostPort(testLoopbackIP, strconv.Itoa(port))})
	defer func() { _ = rc.Close() }()

	err = c.applyACLLoad(rc)
	assert.Error(t, err)
}

// TestSetCustomRedisConfig_ACLFile covers a real bug found while building
// this test suite: `CONFIG SET aclfile <path>` is an *immutable* config in
// real Redis (7.0.15, confirmed here; Redis docs mark aclfile as requiring a
// server restart to change) - the server rejects it with "ERR CONFIG SET
// failed... can't set immutable config", regardless of whether the value
// being set matches the aclfile Redis was already started with.
//
// SetCustomRedisConfig now special-cases "aclfile": it skips the CONFIG SET
// for that parameter (it would always fail) and goes straight to ACL LOAD,
// which re-reads whatever aclfile Redis already has configured (set at boot,
// e.g. via a mounted ConfigMap + `--aclfile` flag). This test starts Redis
// with an aclfile already configured, changes the file's *contents* (not its
// path - only the path is immutable) and confirms a customConfig line of
// `aclfile <path>` triggers a live ACL reload, matching the exact scenario
// commit 8add5ac "Fix aclfile in CustomConfig silently not loading ACL
// users" (spotahome/redis-operator#693) was meant to fix.
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
	require.NoError(t, err, "aclfile in customConfig should trigger an ACL LOAD, not a (failing) CONFIG SET")

	rc := rediscli.NewClient(&rediscli.Options{Addr: r.Addr()})
	defer func() { _ = rc.Close() }()
	users, err := rc.Do(context.Background(), "ACL", "LIST").StringSlice()
	require.NoError(t, err)
	found := false
	for _, u := range users {
		if strings.HasPrefix(u, "user testuser ") && strings.Contains(u, "+@all") {
			found = true
			break
		}
	}
	assert.True(t, found, "expected an ACL LIST entry for testuser with +@all, got %v", users)
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

	// GetNumberSentinelSlavesInMemory shares the same isSentinelReady gate.
	_, err = c.GetNumberSentinelSlavesInMemory(env.sentinel.IP)
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

// TestMonitorRedisWithPort_InvalidPort passes a non-numeric port straight
// through to `SENTINEL MONITOR`, which real Sentinel rejects with a RESP
// error ("ERR Invalid port"). This exercises MonitorRedisWithPort's
// `if err != nil` branch right after the initial MONITOR command via a real
// redis-level protocol error rather than a connection failure.
func TestMonitorRedisWithPort_InvalidPort(t *testing.T) {
	env := getSharedEnv(t)
	c := newTestClient()
	t.Cleanup(func() { restoreSharedSentinel(t, env) })

	err := c.MonitorRedisWithPort(env.sentinel.IP, testLoopbackIP, "not-a-port", "1", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Invalid port")
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
// ---------------------------------------------------------------------
// getRedisError
//
// A pure string-classification function - no redis-server needed.
// ---------------------------------------------------------------------

func TestGetRedisError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"NOAUTH", errors.New("NOAUTH Authentication required."), metrics.NOAUTH},
		{"WRONGPASS", errors.New("WRONGPASS invalid username-password pair or user is disabled."), metrics.WRONG_PASSWORD_USED},
		{"NOPERM", errors.New("NOPERM this user has no permissions to run this command"), metrics.NOPERM},
		{"io timeout", errors.New("dial tcp 127.0.0.1:6379: i/o timeout"), metrics.IO_TIMEOUT},
		{"connection refused", errors.New("dial tcp 127.0.0.1:6379: connect: connection refused"), metrics.CONNECTION_REFUSED},
		{"unrecognized error falls back to MISC", errors.New("something else entirely"), "MISC"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, getRedisError(tt.err))
		})
	}
}

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

// TestSentinelFunctions_SentinelUnreachable exercises the connection-error
// branch of the various Sentinel-facing Client methods (the `if err != nil`
// branch immediately following the Info()/Process() call to the sentinel,
// before any redis-level reply parsing happens) by making the sentinel
// address genuinely unreachable.
//
// Every Sentinel-related Client method hardcodes sentinelPort as the port it
// connects to (see newSentinelProcessOnPort's doc comment in
// testutil_test.go), and this package runs exactly one sentinel process for
// the whole test binary (sharedEnv's, since only one process can bind that
// port at a time). That means the only way to make "connecting to the
// sentinel" itself fail - as opposed to a command reaching a live sentinel
// but getting an unexpected reply, which every other Sentinel test in this
// file covers - is to stop the one process that's listening there.
//
// This test does exactly that: it kills the shared sentinel process outright
// and, unlike every other test here that mutates sentinel state, never
// restarts or restores it. It MUST THEREFORE STAY THE LAST TEST DECLARED IN
// THIS FILE. `go test` runs the tests within a package sequentially, in the
// order they're declared (nothing in this package calls t.Parallel - every
// other sentinel-mutating test already relies on that same sequential
// ordering, see e.g. restoreSharedSentinel's doc comment), so as long as
// this stays last, no later test ever tries to reach a sentinel that no
// longer exists. Do not add a test after this one that touches env.sentinel.
func TestSentinelFunctions_SentinelUnreachable(t *testing.T) {
	env := getSharedEnv(t)
	c := newTestClient()

	killProc(env.sentinel)

	_, err := c.GetNumberSentinelsInMemory(env.sentinel.IP)
	assert.Error(t, err, "GetNumberSentinelsInMemory should fail once nothing is listening on the sentinel port")

	_, err = c.GetNumberSentinelSlavesInMemory(env.sentinel.IP)
	assert.Error(t, err, "GetNumberSentinelSlavesInMemory should fail once nothing is listening on the sentinel port")

	err = c.ResetSentinel(env.sentinel.IP)
	assert.Error(t, err, "ResetSentinel should fail once nothing is listening on the sentinel port")

	_, _, err = c.GetSentinelMonitor(env.sentinel.IP)
	assert.Error(t, err, "GetSentinelMonitor should fail once nothing is listening on the sentinel port")

	err = c.SetCustomSentinelConfig(env.sentinel.IP, []string{"down-after-milliseconds 1000"})
	assert.Error(t, err, "SetCustomSentinelConfig should fail once nothing is listening on the sentinel port")

	err = c.SentinelCheckQuorum(env.sentinel.IP)
	assert.Error(t, err, "SentinelCheckQuorum should fail once nothing is listening on the sentinel port")

	err = c.MonitorRedisWithPort(env.sentinel.IP, testLoopbackIP, redisPort, "1", "")
	assert.Error(t, err, "MonitorRedisWithPort should fail once nothing is listening on the sentinel port")
}
