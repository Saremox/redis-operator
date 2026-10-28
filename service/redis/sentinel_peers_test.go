package redis

import (
	"net"
	"strconv"
	"syscall"
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
	assert.Equal(t, SentinelPeers{Master: "10.0.0.1", Peers: []SentinelPeer{{IP: "10.0.1.2", LastHello: -1}, {IP: "10.0.9.9", LastHello: -1}}}, got)
}

// TestGetSentinelPeersReadsTheEntries reads the run ID, the down flags and the
// age of the last hello of each peer. A value that is not valid gives the zero
// value, or -1 for the age. An entry without an ip field gives no peer.
func TestGetSentinelPeersReadsTheEntries(t *testing.T) {
	entry := func(ip, runID, flags, hello string) string {
		return sentinelMasterReply("name", ip+":26379", "ip", ip, "port", "26379", "runid", runID, "flags", flags, "last-hello-message", hello)
	}
	peers := "*6\r\n" +
		entry("10.0.1.1", "r1", "sentinel", "1500") +
		entry("10.0.1.2", "r2", "s_down,sentinel,disconnected", "30000") +
		entry("10.0.1.3", "r3", "sentinel,disconnected", "x") +
		entry("10.0.1.4", "r4", "s_down,sentinel", "-5") +
		// An integer is not a valid value.
		"*4\r\n$2\r\nip\r\n$8\r\n10.0.1.5\r\n$18\r\nlast-hello-message\r\n:7\r\n" +
		// An entry without an ip field is skipped.
		sentinelMasterReply("runid", "r6", "flags", "sentinel")
	tests := []struct {
		flags string
		down  bool
	}{
		{flags: "master"},
		{flags: "master,s_down", down: true},
		{flags: "master,s_down,o_down", down: true},
		{flags: "master,disconnected", down: true},
	}
	for _, test := range tests {
		t.Run(test.flags, func(t *testing.T) {
			startRawSentinel(t, "127.0.0.3", sentinelMasterReply("name", masterName, "ip", "10.0.0.1", "port", "6379", "flags", test.flags), peers)

			got, err := newTestClient().GetSentinelPeers("127.0.0.3")

			require.NoError(t, err)
			assert.Equal(t, SentinelPeers{Master: "10.0.0.1", MasterDown: test.down, Peers: []SentinelPeer{
				{IP: "10.0.1.1", RunID: "r1", LastHello: 1500 * time.Millisecond},
				{IP: "10.0.1.2", RunID: "r2", Down: true, LastHello: 30 * time.Second},
				{IP: "10.0.1.3", RunID: "r3", Down: true, LastHello: -1},
				{IP: "10.0.1.4", RunID: "r4", Down: true, LastHello: -1},
				{IP: "10.0.1.5", LastHello: -1},
			}}, got)
		})
	}
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
		{
			name:    "an ip that is no string",
			replies: []string{sentinelMasterReply("ip", "10.0.0.1"), "*1\r\n*2\r\n$2\r\nip\r\n:7\r\n"},
			err:     "unexpected SENTINEL SENTINELS ip 7",
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
		return err == nil && len(got.Peers) == 1 && got.Peers[0].IP == "127.0.0.4"
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

// peerAt returns the entry of ip in the peers of the Sentinel sip.
func peerAt(t *testing.T, sip, ip string) (SentinelPeer, bool) {
	t.Helper()
	group, err := newTestClient().GetSentinelPeers(sip)
	if err != nil {
		return SentinelPeer{}, false
	}
	for _, peer := range group.Peers {
		if peer.IP == ip {
			return peer, true
		}
	}
	return SentinelPeer{}, false
}

// TestGetSentinelPeersShowsAForeignSentinelAtAnOldIP shows the merge of two
// groups with real Sentinels. A Sentinel of group B gets the address of a
// deleted Sentinel of group A. The count of group A stays correct, but the
// entry at that address gets the run ID of the new Sentinel and fresh hellos.
// A reset removes the entry only for a moment, because the Sentinel of group
// B knows the Sentinels of group A and sends them hellos.
func TestGetSentinelPeersShowsAForeignSentinelAtAnOldIP(t *testing.T) {
	requireRedisServer(t)
	const a1, a2, a3 = "127.0.0.61", "127.0.0.62", "127.0.0.63"
	masterA, masterB := startRedisProcess(t), startRedisProcess(t)
	fast := "sentinel down-after-milliseconds " + masterName + " 1000"
	startSentinelOnAddr(t, a1, masterA, "sentinel announce-ip "+a1, fast)
	startSentinelOnAddr(t, a2, masterA, "sentinel announce-ip "+a2, fast)
	oldA3 := startSentinelOnAddr(t, a3, masterA, "sentinel announce-ip "+a3, fast)
	c := newTestClient()

	var old SentinelPeer
	require.True(t, waitForCondition(t, 10*time.Second, func() bool {
		n, err := c.GetNumberSentinelsInMemory(a1)
		var ok bool
		old, ok = peerAt(t, a1, a3)
		_, ok2 := peerAt(t, a1, a2)
		return err == nil && n == 3 && ok && ok2 && !old.Down
	}), "the Sentinels of group A did not find each other")
	require.NotEmpty(t, old.RunID)
	assert.Less(t, old.LastHello, 5*time.Second)

	killProc(oldA3)
	require.True(t, waitForCondition(t, 5*time.Second, func() bool {
		dead, ok := peerAt(t, a1, a3)
		return ok && dead.Down
	}), "the deleted Sentinel is not disconnected")

	b1 := startSentinelOnAddr(t, a3, masterB, "sentinel announce-ip "+a3, fast)
	var foreign SentinelPeer
	require.True(t, waitForCondition(t, 10*time.Second, func() bool {
		var ok bool
		foreign, ok = peerAt(t, a1, a3)
		return ok && foreign.RunID != old.RunID && !foreign.Down
	}), "the Sentinel of group B did not replace the old entry")
	assert.Less(t, foreign.LastHello, 5*time.Second)
	n, err := c.GetNumberSentinelsInMemory(a1)
	require.NoError(t, err)
	assert.Equal(t, int32(3), n, "the count does not show the Sentinel of group B")

	// The stopped Sentinel of group B sends no hello during the check.
	require.NoError(t, b1.cmd.Process.Signal(syscall.SIGSTOP))
	require.NoError(t, c.ResetSentinel(a1))
	_, listed := peerAt(t, a1, a3)
	assert.False(t, listed, "the reset removes the entry")
	_, listed = peerAt(t, a2, a3)
	assert.True(t, listed, "the reset of a1 does not change a2")
	require.NoError(t, b1.cmd.Process.Signal(syscall.SIGCONT))
	require.True(t, waitForCondition(t, 5*time.Second, func() bool {
		back, ok := peerAt(t, a1, a3)
		return ok && back.RunID == foreign.RunID
	}), "the Sentinel of group B did not come back after the reset")
}
