package redis

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	rediscli "github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sentinelMasterReply is a SENTINEL MASTER reply with the given fields.
func sentinelMasterReply(fields ...string) string {
	reply := "*" + strconv.Itoa(len(fields)) + "\r\n"
	for _, f := range fields {
		reply += "$" + strconv.Itoa(len(f)) + "\r\n" + f + "\r\n"
	}
	return reply
}

func TestGetSentinelFailoverReadsTheState(t *testing.T) {
	tests := []struct {
		name  string
		reply string
		want  SentinelFailover
	}{
		{
			name:  "a Sentinel that fails over",
			reply: sentinelMasterReply("ip", "10.0.0.1", "port", "6379", "flags", "master,failover_in_progress,force_failover", "failover-state", "wait_promotion", "failover-timeout", "10000", "down-after-milliseconds", "5000"),
			want:  SentinelFailover{InProgress: true, State: "wait_promotion", Timeout: 10 * time.Second, DownAfter: 5 * time.Second, Master: "10.0.0.1:6379"},
		},
		{
			name:  "no failover",
			reply: sentinelMasterReply("ip", "10.0.0.1", "port", "6379", "flags", "master", "failover-timeout", "180000"),
			want:  SentinelFailover{Timeout: 3 * time.Minute, Master: "10.0.0.1:6379"},
		},
		{
			name:  "no failover-timeout",
			reply: sentinelMasterReply("ip", "10.0.0.1", "port", "6379", "flags", "master,failover_in_progress"),
			want:  SentinelFailover{InProgress: true, Master: "10.0.0.1:6379"},
		},
		{
			name:  "a negative failover-timeout",
			reply: sentinelMasterReply("ip", "10.0.0.1", "port", "6379", "flags", "failover_in_progress,master", "failover-timeout", "-1", "down-after-milliseconds", "-1"),
			want:  SentinelFailover{InProgress: true, Master: "10.0.0.1:6379"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			startRawSentinel(t, "127.0.0.3", test.reply)
			got, err := newTestClient().GetSentinelFailover("127.0.0.3")
			require.NoError(t, err)
			assert.Equal(t, test.want, got)
		})
	}
}

// TestGetSentinelFailoverRejectsAnIncompleteReply covers replies without
// the fields that the operator needs. Such a reply must not read as a
// Sentinel without a failover.
func TestGetSentinelFailoverRejectsAnIncompleteReply(t *testing.T) {
	for name, reply := range map[string]string{
		"an empty array":           "*0\r\n",
		"an odd array with flags":  sentinelMasterReply("ip", "10.0.0.1", "port", "6379", "flags"),
		"no flags":                 sentinelMasterReply("ip", "10.0.0.1", "port", "6379"),
		"no ip":                    sentinelMasterReply("port", "6379", "flags", "master,failover_in_progress"),
		"no port":                  sentinelMasterReply("ip", "10.0.0.1", "flags", "master"),
		"empty flags":              sentinelMasterReply("ip", "10.0.0.1", "port", "6379", "flags", ""),
		"empty ip":                 sentinelMasterReply("ip", "", "port", "6379", "flags", "master"),
		"empty port":               sentinelMasterReply("ip", "10.0.0.1", "port", "", "flags", "master"),
		"port abc":                 sentinelMasterReply("ip", "10.0.0.1", "port", "abc", "flags", "master"),
		"port 0":                   sentinelMasterReply("ip", "10.0.0.1", "port", "0", "flags", "master"),
		"port 70000":               sentinelMasterReply("ip", "10.0.0.1", "port", "70000", "flags", "master"),
		"port above the int range": sentinelMasterReply("ip", "10.0.0.1", "port", "99999999999999999999", "flags", "master"),
		"a value that is a number": "*6\r\n$2\r\nip\r\n$8\r\n10.0.0.1\r\n$4\r\nport\r\n$4\r\n6379\r\n$5\r\nflags\r\n:1\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			startRawSentinel(t, "127.0.0.3", reply)
			_, err := newTestClient().GetSentinelFailover("127.0.0.3")
			require.ErrorIs(t, err, errMalformedSentinelMaster)
			assert.False(t, IsNoSuchMasterError(err))
			assert.False(t, IsUnreachableError(err))
		})
	}
}

func TestGetSentinelFailoverErrors(t *testing.T) {
	_, err := newTestClient().GetSentinelFailover("")
	require.ErrorIs(t, err, errNoIP)

	startRawSentinel(t, "127.0.0.3", "-ERR No such master with that name\r\n")
	_, err = newTestClient().GetSentinelFailover("127.0.0.3")
	require.EqualError(t, err, "SENTINEL MASTER: ERR No such master with that name")
	assert.True(t, IsNoSuchMasterError(err))
}

// startSentinelOnAddr starts a Sentinel on ip:sentinelPort that monitors
// master.
func startSentinelOnAddr(t *testing.T, ip string, master *redisProc, extraConf ...string) {
	t.Helper()
	l, err := net.Listen("tcp", net.JoinHostPort(ip, sentinelPort))
	if err != nil {
		t.Skipf("cannot listen on %s: %v", net.JoinHostPort(ip, sentinelPort), err)
	}
	_ = l.Close()
	dir := t.TempDir()
	conf := fmt.Sprintf("port %s\nbind %s\ndir %s\nsentinel resolve-hostnames no\nsentinel monitor %s %s %d 1\n", sentinelPort, ip, dir, masterName, master.IP, master.Port)
	for _, line := range extraConf {
		conf += line + "\n"
	}
	confPath := filepath.Join(dir, "sentinel.conf")
	require.NoError(t, os.WriteFile(confPath, []byte(conf), 0o600))
	cmd := exec.Command("redis-server", confPath, "--sentinel", "--daemonize", "no", "--logfile", filepath.Join(dir, "log.txt"))
	require.NoError(t, cmd.Start())
	proc := &redisProc{IP: ip, Port: 26379, dir: dir, cmd: cmd}
	go func() { _ = cmd.Wait() }()
	t.Cleanup(func() { killProc(proc) })
	require.NoError(t, waitForPingErr(proc, 5*time.Second))
}

// TestGetSentinelFailoverOfARealFailover reads the state of a real Sentinel
// during a SENTINEL FAILOVER. It also shows which commands the operator can
// send at this time. SENTINEL SET keeps the failover. A new monitor ends it,
// and the Sentinel forgets the epoch of the failover.
func TestGetSentinelFailoverOfARealFailover(t *testing.T) {
	master, replica := failoverPair(t)
	const sentinelIP = "127.0.0.3"
	startSentinelOnAddr(t, sentinelIP, master, fmt.Sprintf("sentinel failover-timeout %s 15000", masterName))
	c := newTestClient()
	require.True(t, waitForCondition(t, 15*time.Second, func() bool {
		replicas, err := c.GetSentinelReplicas(sentinelIP)
		return err == nil && len(replicas) == 1 && replicas[0] == replica.IP
	}), "the Sentinel does not know the replica")

	f, err := c.GetSentinelFailover(sentinelIP)
	require.NoError(t, err)
	oldMaster := net.JoinHostPort(master.IP, strconv.Itoa(master.Port))
	// The Sentinel has the built-in down-after-milliseconds of 30 seconds.
	assert.Equal(t, SentinelFailover{Timeout: 15 * time.Second, DownAfter: 30 * time.Second, Master: oldMaster}, f)

	rc := rediscli.NewSentinelClient(&rediscli.Options{Addr: net.JoinHostPort(sentinelIP, sentinelPort)})
	defer func() { _ = rc.Close() }()
	// The Sentinel refuses the failover (NOGOODSLAVE) until it reads the INFO
	// of the replica.
	require.True(t, waitForCondition(t, 20*time.Second, func() bool {
		return rc.Failover(bgCtx(), masterName).Err() == nil
	}), "the Sentinel refused the failover")
	f, err = c.GetSentinelFailover(sentinelIP)
	require.NoError(t, err)
	assert.True(t, f.InProgress)
	assert.NotEmpty(t, f.State)
	assert.Equal(t, oldMaster, f.Master, "the master changes at the end of the failover only")

	require.NoError(t, c.SetCustomSentinelConfig(sentinelIP, []string{"down-after-milliseconds 4000"}))
	require.NoError(t, c.SetSentinelAuthPass(sentinelIP, "s3cr3t"))
	f, err = c.GetSentinelFailover(sentinelIP)
	require.NoError(t, err)
	assert.True(t, f.InProgress, "SENTINEL SET keeps the failover")

	require.NoError(t, c.MonitorRedisWithPort(sentinelIP, replica.IP, strconv.Itoa(replica.Port), "1", ""))
	f, err = c.GetSentinelFailover(sentinelIP)
	require.NoError(t, err)
	assert.False(t, f.InProgress, "a new monitor ends the failover")
	info, err := rc.Master(bgCtx(), masterName).Result()
	require.NoError(t, err)
	assert.Equal(t, "0", info["config-epoch"], "the Sentinel forgets the epoch of the failover")
}

// TestGetSentinelFailoverWithoutTheMaster covers a real Sentinel after
// SENTINEL REMOVE. The error is a reply, not an unreachable Sentinel, so the
// operator can give the Sentinel a new monitor.
func TestGetSentinelFailoverWithoutTheMaster(t *testing.T) {
	requireRedisServer(t)
	// The shared Sentinel is gone after TestSentinelFunctions_SentinelUnreachable.
	const sentinelIP = "127.0.0.3"
	startSentinelOnAddr(t, sentinelIP, startRedisProcess(t))
	rc := rediscli.NewSentinelClient(&rediscli.Options{Addr: net.JoinHostPort(sentinelIP, sentinelPort)})
	defer func() { _ = rc.Close() }()
	require.NoError(t, rc.Process(bgCtx(), rediscli.NewBoolCmd(bgCtx(), "SENTINEL", "REMOVE", masterName)))

	_, err := newTestClient().GetSentinelFailover(sentinelIP)

	require.EqualError(t, err, "SENTINEL MASTER: ERR No such master with that name")
	assert.True(t, IsNoSuchMasterError(err))
	assert.False(t, IsUnreachableError(err))
	assert.False(t, IsNoSuchMasterError(nil))
	assert.False(t, IsNoSuchMasterError(errors.New("NOPERM this user has no permissions to run the 'sentinel|master' command")))
}
