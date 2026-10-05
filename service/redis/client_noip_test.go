package redis

import (
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/saremox/redis-operator/metrics"
)

type countingRecorder struct {
	metrics.Recorder
	calls int
}

func (c *countingRecorder) RecordRedisOperation(string, string, string, string, string) {
	c.calls++
}

// A pod without an IP makes an empty host, which the dial resolves to the
// loopback address of the operator.
func TestEmptyIPReturnsAnErrorWithoutDialing(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = l.Close() }()
	port := strconv.Itoa(l.Addr().(*net.TCPAddr).Port)

	recorder := &countingRecorder{Recorder: metrics.Dummy}
	c := New(recorder)
	calls := map[string]func() error{
		"GetNumberSentinelsInMemory":      func() error { _, err := c.GetNumberSentinelsInMemory(""); return err },
		"GetNumberSentinelSlavesInMemory": func() error { _, err := c.GetNumberSentinelSlavesInMemory(""); return err },
		"ResetSentinel":                   func() error { return c.ResetSentinel("") },
		"GetSlaveOf":                      func() error { _, err := c.GetSlaveOf("", port, "pw"); return err },
		"IsMaster":                        func() error { _, err := c.IsMaster("", port, "pw"); return err },
		"MonitorRedis":                    func() error { return c.MonitorRedis("", "10.0.0.1", "2", "pw") },
		"MonitorRedisWithPort":            func() error { return c.MonitorRedisWithPort("", "10.0.0.1", port, "2", "pw") },
		"MakeMaster":                      func() error { return c.MakeMaster("", port, "pw") },
		"MakeSlaveOf":                     func() error { return c.MakeSlaveOf("", "10.0.0.1", "pw") },
		"MakeSlaveOfWithPort":             func() error { return c.MakeSlaveOfWithPort("", port, "10.0.0.1", port, "pw") },
		"DisconnectClients":               func() error { return c.DisconnectClients("", port, "pw") },
		"GetSentinelMonitor":              func() error { _, _, err := c.GetSentinelMonitor(""); return err },
		"GetSentinelReplicas":             func() error { _, err := c.GetSentinelReplicas(""); return err },
		"SentinelMasterDown":              func() error { _, err := c.SentinelMasterDown(""); return err },
		"SetCustomSentinelConfig":         func() error { return c.SetCustomSentinelConfig("", []string{"down-after-milliseconds 1000"}) },
		"SetCustomRedisConfig":            func() error { return c.SetCustomRedisConfig("", port, []string{"maxmemory 1mb"}, "pw") },
		"SlaveIsReady":                    func() error { _, err := c.SlaveIsReady("", port, "pw"); return err },
		"SentinelCheckQuorum":             func() error { return c.SentinelCheckQuorum("") },
		"GetReplicationInfo":              func() error { _, err := c.GetReplicationInfo("", port, "pw"); return err },
		"GetMemoryInfo":                   func() error { _, err := c.GetMemoryInfo("", port, "pw"); return err },
		"SetPassword":                     func() error { return c.SetPassword("", port, "pw", "new") },
		"SetSentinelAuthPass":             func() error { return c.SetSentinelAuthPass("", "pw") },
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			err := call()
			require.ErrorIs(t, err, errNoIP)
			// The callers skip a pod that does not answer, so they skip this pod too.
			assert.True(t, IsUnreachableError(err))
		})
	}

	assert.Zero(t, recorder.calls, "no metric for a call that did not leave the operator")
	require.NoError(t, l.(*net.TCPListener).SetDeadline(time.Now().Add(100*time.Millisecond)))
	conn, err := l.Accept()
	if err == nil {
		_ = conn.Close()
	}
	assert.Error(t, err, "no client connected to the loopback address")
}

func TestIsUnreachableErrorForAPodWithoutIP(t *testing.T) {
	assert.True(t, IsUnreachableError(errNoIP))
	assert.False(t, IsAuthError(errNoIP))
}
