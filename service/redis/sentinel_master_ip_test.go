package redis

import (
	"net"
	"testing"
	"time"

	rediscli "github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetSentinelMasterIPReadsTheReply(t *testing.T) {
	startRawSentinel(t, "127.0.0.3", "*2\r\n$8\r\n10.0.0.1\r\n$4\r\n6379\r\n")
	ip, err := newTestClient().GetSentinelMasterIP("127.0.0.3")
	require.NoError(t, err)
	assert.Equal(t, "10.0.0.1", ip)
}

func TestGetSentinelMasterIPRejectsOtherReplies(t *testing.T) {
	cases := map[string]string{
		"null array":   "*-1\r\n",
		"empty array":  "*0\r\n",
		"one element":  "*1\r\n$8\r\n10.0.0.1\r\n",
		"an error":     "-ERR unknown subcommand\r\n",
		"array as ip":  "*2\r\n*0\r\n$4\r\n6379\r\n",
		"three fields": "*3\r\n$8\r\n10.0.0.1\r\n$4\r\n6379\r\n$1\r\nx\r\n",
	}
	for name, reply := range cases {
		t.Run(name, func(t *testing.T) {
			startRawSentinel(t, "127.0.0.3", reply)
			ip, err := newTestClient().GetSentinelMasterIP("127.0.0.3")
			require.Error(t, err)
			assert.Empty(t, ip)
		})
	}
}

func TestGetSentinelMasterIPWithoutAnIP(t *testing.T) {
	_, err := newTestClient().GetSentinelMasterIP("")
	assert.ErrorIs(t, err, errNoIP)
}

// TestGetSentinelMasterIPOfARealFailover reads a real Sentinel before and
// after a SENTINEL FAILOVER, and after SENTINEL REMOVE.
func TestGetSentinelMasterIPOfARealFailover(t *testing.T) {
	master, replica := failoverPair(t)
	const sentinelIP = "127.0.0.3"
	startSentinelOnAddr(t, sentinelIP, master)
	c := newTestClient()

	ip, err := c.GetSentinelMasterIP(sentinelIP)
	require.NoError(t, err)
	assert.Equal(t, master.IP, ip)

	rc := rediscli.NewSentinelClient(&rediscli.Options{Addr: net.JoinHostPort(sentinelIP, sentinelPort)})
	defer func() { _ = rc.Close() }()
	// The Sentinel refuses the failover (NOGOODSLAVE) until it reads the INFO
	// of the replica.
	require.True(t, waitForCondition(t, 20*time.Second, func() bool {
		return rc.Failover(bgCtx(), masterName).Err() == nil
	}), "the Sentinel refused the failover")
	assert.True(t, waitForCondition(t, 20*time.Second, func() bool {
		ip, err := c.GetSentinelMasterIP(sentinelIP)
		return err == nil && ip == replica.IP
	}), "the Sentinel does not give the promoted replica")

	require.NoError(t, rc.Process(bgCtx(), rediscli.NewBoolCmd(bgCtx(), "SENTINEL", "REMOVE", masterName)))
	_, err = c.GetSentinelMasterIP(sentinelIP)
	assert.Error(t, err)
}
