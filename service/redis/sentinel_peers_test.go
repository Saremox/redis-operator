package redis

import (
	"net"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sentinelsReply is a SENTINEL SENTINELS reply with one entry for each IP.
func sentinelsReply(ips ...string) string {
	reply := "*" + strconv.Itoa(len(ips)) + "\r\n"
	for _, ip := range ips {
		reply += sentinelMasterReply("name", ip+":26379", "ip", ip, "port", "26379", "flags", "sentinel")
	}
	return reply
}

func TestGetSentinelPeersReadsTheGroup(t *testing.T) {
	startRawSentinel(t, "127.0.0.3", sentinelMasterReply("name", masterName, "ip", "10.0.0.1", "port", "6379", "flags", "master"), sentinelsReply("10.0.1.2", "10.0.9.9"))

	got, err := newTestClient().GetSentinelPeers("127.0.0.3")

	require.NoError(t, err)
	assert.Equal(t, SentinelPeers{Master: "10.0.0.1", Peers: []string{"10.0.1.2", "10.0.9.9"}}, got)
}

func TestGetSentinelPeersErrors(t *testing.T) {
	tests := []struct {
		name    string
		replies []string
		err     string
	}{
		{
			name:    "a master without an ip",
			replies: []string{sentinelMasterReply("name", masterName, "port", "6379")},
			err:     errMalformedSentinelMaster.Error(),
		},
		{
			name:    "no master",
			replies: []string{"-ERR No such master with that name\r\n"},
			err:     "SENTINEL MASTER: ERR No such master with that name",
		},
		{
			name:    "an error of SENTINEL SENTINELS",
			replies: []string{sentinelMasterReply("ip", "10.0.0.1"), "-NOPERM no permissions\r\n"},
			err:     "SENTINEL SENTINELS: NOPERM no permissions",
		},
		{
			name:    "a malformed SENTINEL SENTINELS entry",
			replies: []string{sentinelMasterReply("ip", "10.0.0.1"), "*1\r\n$8\r\n10.0.1.2\r\n"},
			err:     "unexpected SENTINEL SENTINELS entry 10.0.1.2",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			startRawSentinel(t, "127.0.0.3", test.replies...)
			got, err := newTestClient().GetSentinelPeers("127.0.0.3")
			require.EqualError(t, err, test.err)
			assert.Equal(t, SentinelPeers{}, got)
		})
	}
	_, err := newTestClient().GetSentinelPeers("")
	assert.ErrorIs(t, err, errNoIP)
}

// TestGetSentinelPeersOfRealSentinels shows that two Sentinels of one master
// find each other through the hello messages of the master. Without
// announce-ip, both Sentinels announce the source address 127.0.0.1.
func TestGetSentinelPeersOfRealSentinels(t *testing.T) {
	requireRedisServer(t)
	master := startRedisProcess(t)
	startSentinelOnAddr(t, "127.0.0.3", master, "sentinel announce-ip 127.0.0.3")
	startSentinelOnAddr(t, "127.0.0.4", master, "sentinel announce-ip 127.0.0.4")

	var got SentinelPeers
	found := waitForCondition(t, 15*time.Second, func() bool {
		var err error
		got, err = newTestClient().GetSentinelPeers("127.0.0.3")
		return err == nil && slices.Equal(got.Peers, []string{"127.0.0.4"})
	})
	require.True(t, found, "the Sentinels did not find each other: %+v", got)
	assert.Equal(t, master.IP, got.Master)
}

// TestGetSentinelPeersOfAFrozenSentinel needs no long timeout or retry,
// because an error only leaves the Sentinel unread for one check.
func TestGetSentinelPeersOfAFrozenSentinel(t *testing.T) {
	l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.3", sentinelPort))
	if err != nil {
		t.Skipf("cannot listen on 127.0.0.3: %v", err)
	}
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
				time.Sleep(3 * time.Second)
			}()
		}
	}()

	start := time.Now()
	_, err = newTestClient().GetSentinelPeers("127.0.0.3")

	require.Error(t, err)
	assert.Less(t, time.Since(start), 1500*time.Millisecond)
}
