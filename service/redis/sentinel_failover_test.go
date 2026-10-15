package redis

import (
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

func TestSentinelFailoverInProgressReadsTheFlag(t *testing.T) {
	tests := []struct {
		name        string
		reply       string
		wantFlag    bool
		wantTimeout time.Duration
	}{
		{
			name:        "the leader of a failover",
			reply:       sentinelMasterReply("flags", "master,failover_in_progress,force_failover", "failover-timeout", "10000"),
			wantFlag:    true,
			wantTimeout: 10 * time.Second,
		},
		{
			name:        "no failover",
			reply:       sentinelMasterReply("flags", "master", "failover-timeout", "180000"),
			wantTimeout: 3 * time.Minute,
		},
		{
			name:     "no failover-timeout",
			reply:    sentinelMasterReply("flags", "master,failover_in_progress"),
			wantFlag: true,
		},
		{
			name:     "a negative failover-timeout",
			reply:    sentinelMasterReply("flags", "failover_in_progress,master", "failover-timeout", "-1"),
			wantFlag: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			startRawSentinel(t, "127.0.0.3", test.reply)
			inProgress, timeout, err := newTestClient().SentinelFailoverInProgress("127.0.0.3")
			require.NoError(t, err)
			assert.Equal(t, test.wantFlag, inProgress)
			assert.Equal(t, test.wantTimeout, timeout)
		})
	}
}

func TestSentinelFailoverInProgressErrors(t *testing.T) {
	_, _, err := newTestClient().SentinelFailoverInProgress("")
	require.ErrorIs(t, err, errNoIP)

	startRawSentinel(t, "127.0.0.3", "-ERR No such master with that name\r\n")
	_, _, err = newTestClient().SentinelFailoverInProgress("127.0.0.3")
	require.EqualError(t, err, "ERR No such master with that name")
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

// TestSentinelFailoverInProgressOfARealFailover reads the flag of a real
// Sentinel during a SENTINEL FAILOVER. It also shows why the operator must not
// send a new monitor at this time: the new monitor ends the failover, and the
// Sentinel forgets the epoch of the failover.
func TestSentinelFailoverInProgressOfARealFailover(t *testing.T) {
	master, replica := failoverPair(t)
	const sentinelIP = "127.0.0.3"
	startSentinelOnAddr(t, sentinelIP, master, fmt.Sprintf("sentinel failover-timeout %s 15000", masterName))
	c := newTestClient()
	require.True(t, waitForCondition(t, 15*time.Second, func() bool {
		replicas, err := c.GetSentinelReplicas(sentinelIP)
		return err == nil && len(replicas) == 1 && replicas[0] == replica.IP
	}), "the Sentinel does not know the replica")

	inProgress, timeout, err := c.SentinelFailoverInProgress(sentinelIP)
	require.NoError(t, err)
	assert.False(t, inProgress)
	assert.Equal(t, 15*time.Second, timeout)

	rc := rediscli.NewSentinelClient(&rediscli.Options{Addr: net.JoinHostPort(sentinelIP, sentinelPort)})
	defer func() { _ = rc.Close() }()
	// The Sentinel refuses the failover (NOGOODSLAVE) until it reads the INFO
	// of the replica.
	require.True(t, waitForCondition(t, 20*time.Second, func() bool {
		return rc.Failover(bgCtx(), masterName).Err() == nil
	}), "the Sentinel refused the failover")
	inProgress, _, err = c.SentinelFailoverInProgress(sentinelIP)
	require.NoError(t, err)
	assert.True(t, inProgress)

	require.NoError(t, c.MonitorRedisWithPort(sentinelIP, replica.IP, strconv.Itoa(replica.Port), "1", ""))
	inProgress, _, err = c.SentinelFailoverInProgress(sentinelIP)
	require.NoError(t, err)
	assert.False(t, inProgress, "a new monitor ends the failover")
	info, err := rc.Master(bgCtx(), masterName).Result()
	require.NoError(t, err)
	assert.Equal(t, "0", info["config-epoch"], "the Sentinel forgets the epoch of the failover")
}
