package redis

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// replyError is an error reply of Redis, as the client reads it.
type replyError string

func (e replyError) Error() string { return string(e) }

func (replyError) RedisError() {}

func TestReplyHasPrefix(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "a reply", err: replyError("ERR No failover in progress"), want: true},
		{name: "a wrapped reply", err: fmt.Errorf("FAILOVER ABORT: %w", replyError("ERR No failover in progress")), want: true},
		{name: "a wrapped reply with another text", err: fmt.Errorf("FAILOVER ABORT: %w", replyError("ERR other")), want: false},
		{name: "a plain error with the text", err: errors.New("ERR No failover in progress"), want: true},
		{name: "a plain error with the text after a prefix", err: errors.New("FAILOVER ABORT: ERR No failover in progress"), want: false},
		{name: "no error", err: nil, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, ReplyHasPrefix(test.err, "ERR No failover in progress"))
		})
	}
}

func TestIsNoSuchMasterErrorReadsTheReply(t *testing.T) {
	const reply = "ERR No such master with that name"
	assert.True(t, IsNoSuchMasterError(fmt.Errorf("SENTINEL MASTER: %w", replyError(reply))))
	assert.True(t, IsNoSuchMasterError(errors.New(reply)))
	assert.False(t, IsNoSuchMasterError(errors.New("SENTINEL MASTER: "+reply)))
	assert.False(t, IsNoSuchMasterError(fmt.Errorf("SENTINEL MASTER: %w", replyError("ERR other"))))
}

func TestGetNumberSentinelsInMemory_CountOutOfRange(t *testing.T) {
	const target = "127.0.0.2"
	startFakeSentinel(t, target, "# Sentinel\r\nsentinel_masters:1\r\n"+
		"master0:name=mymaster,status=ok,address=10.0.0.1:6379,slaves=99999999999999999999,sentinels=99999999999999999999\r\n")
	c := newTestClient()

	_, err := c.GetNumberSentinelsInMemory(target)
	require.EqualError(t, err, `read the sentinel count: strconv.ParseUint: parsing "99999999999999999999": value out of range`)

	_, err = c.GetNumberSentinelSlavesInMemory(target)
	assert.EqualError(t, err, `read the replica count: strconv.ParseUint: parsing "99999999999999999999": value out of range`)
}

func TestMonitorRedisWithPort_AuthPassError(t *testing.T) {
	const target = "127.0.0.2"
	// SENTINEL REMOVE, SENTINEL MONITOR, SENTINEL SET auth-pass.
	startRawSentinel(t, target, "+OK\r\n", "+OK\r\n", "-ERR refused\r\n")

	err := newTestClient().MonitorRedisWithPort(target, "10.0.0.1", "6379", "2", "secret")

	assert.EqualError(t, err, "SENTINEL SET auth-pass: ERR refused")
}

func TestSetCustomSentinelConfig_SetError(t *testing.T) {
	const target = "127.0.0.2"
	// SENTINEL MASTER fails, so the client sets each value.
	startRawSentinel(t, target, "-ERR refused\r\n")

	err := newTestClient().SetCustomSentinelConfig(target, []string{"down-after-milliseconds 1000"})

	assert.EqualError(t, err, "SENTINEL SET down-after-milliseconds: ERR refused")
}
