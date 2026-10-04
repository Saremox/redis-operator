package redis

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The Sentinel image is tenant-controlled, so a reply can have any shape.
// Each test sends one raw RESP reply for every command.
const (
	respHead  = "$4\r\nname\r\n$8\r\nmymaster\r\n$2\r\nip\r\n"
	respIP    = "$8\r\n10.0.0.1\r\n"
	respPort  = "$4\r\nport\r\n"
	respValue = "$4\r\n6379\r\n"
)

func TestGetSentinelMonitorRejectsMalformedReplies(t *testing.T) {
	cases := map[string]string{
		"empty array":     "*0\r\n",
		"short array":     "*4\r\n" + respHead + respIP,
		"integer as ip":   "*6\r\n" + respHead + ":5\r\n" + respPort + respValue,
		"integer as port": "*6\r\n" + respHead + respIP + respPort + ":6379\r\n",
		"nil as port":     "*6\r\n" + respHead + respIP + respPort + "$-1\r\n",
		"array as ip":     "*6\r\n" + respHead + "*0\r\n" + respPort + respValue,
	}
	for name, reply := range cases {
		t.Run(name, func(t *testing.T) {
			startRawSentinel(t, "127.0.0.3", reply)
			_, _, err := newTestClient().GetSentinelMonitor("127.0.0.3")
			assert.ErrorIs(t, err, errMalformedSentinelMaster)
		})
	}
}

func TestSentinelMasterDownRejectsMalformedReplies(t *testing.T) {
	cases := map[string]string{
		"integer as name":  "*2\r\n:1\r\n$1\r\nx\r\n",
		"integer as value": "*2\r\n$1\r\nx\r\n:1\r\n",
		"nil as name":      "*2\r\n$-1\r\n$1\r\nx\r\n",
		"array as value":   "*2\r\n$1\r\nx\r\n*0\r\n",
	}
	for name, reply := range cases {
		t.Run(name, func(t *testing.T) {
			startRawSentinel(t, "127.0.0.3", reply)
			_, err := newTestClient().SentinelMasterDown("127.0.0.3")
			assert.ErrorIs(t, err, errMalformedSentinelMaster)
		})
	}
}

func TestSentinelMasterDownReadsAWellFormedReply(t *testing.T) {
	startRawSentinel(t, "127.0.0.3", "*2\r\n$5\r\nflags\r\n$13\r\nmaster,s_down\r\n")
	down, err := newTestClient().SentinelMasterDown("127.0.0.3")
	assert.NoError(t, err)
	assert.True(t, down)
}
