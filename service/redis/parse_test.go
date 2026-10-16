package redis

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/saremox/redis-operator/metrics"
	mMetrics "github.com/saremox/redis-operator/mocks/metrics"
)

// TestSentinelCountErrors pins the error text and the metric of each failed
// read of GetNumberSentinelsInMemory and GetNumberSentinelSlavesInMemory.
func TestSentinelCountErrors(t *testing.T) {
	const target = "127.0.0.2"
	const head = "# Sentinel\r\nsentinel_masters:1\r\nmaster0:name=mymaster,"
	for _, tc := range []struct {
		name, info             string
		reason                 string
		sentinelErr, slavesErr string
	}{
		{
			name:        "not ready",
			info:        head + "status=sdown,address=10.0.0.1:6379,slaves=1,sentinels=3\r\n",
			reason:      metrics.SENTINEL_NOT_READY,
			sentinelErr: "sentinels not ready",
			slavesErr:   "sentinels not ready",
		},
		{
			name:        "no count",
			info:        head + "status=ok,address=10.0.0.1:6379\r\n",
			reason:      metrics.REGEX_NOT_FOUND,
			sentinelErr: "sentinel regex not found",
			slavesErr:   "slaves regex not found",
		},
		{
			name:        "too many",
			info:        head + "status=ok,address=10.0.0.1:6379,slaves=65537,sentinels=65537\r\n",
			reason:      metrics.SENTINEL_TOO_MANY,
			sentinelErr: "sentinel reports 65537 sentinels, more than 65536",
			slavesErr:   "sentinel reports 65537 replicas, more than 65536",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			startFakeSentinel(t, target, tc.info)
			rec := &mMetrics.Recorder{}
			rec.On("RecordRedisOperation", metrics.KIND_SENTINEL, target, metrics.GET_NUM_SENTINELS_IN_MEM, metrics.FAIL, tc.reason).Once()
			rec.On("RecordRedisOperation", metrics.KIND_SENTINEL, target, metrics.GET_NUM_REDIS_SLAVES_IN_MEM, metrics.FAIL, tc.reason).Once()
			c := New(rec)

			n, err := c.GetNumberSentinelsInMemory(target)
			require.EqualError(t, err, tc.sentinelErr)
			assert.Zero(t, n)
			n, err = c.GetNumberSentinelSlavesInMemory(target)
			require.EqualError(t, err, tc.slavesErr)
			assert.Zero(t, n)
			rec.AssertExpectations(t)
		})
	}
}

// TestSentinelCountReadsEachField checks that each method reads its own field.
func TestSentinelCountReadsEachField(t *testing.T) {
	const target = "127.0.0.2"
	startFakeSentinel(t, target, "# Sentinel\r\nsentinel_masters:1\r\n"+
		"master0:name=mymaster,status=ok,address=10.0.0.1:6379,slaves=2,sentinels=5\r\n")
	c := newTestClient()

	n, err := c.GetNumberSentinelsInMemory(target)
	require.NoError(t, err)
	assert.EqualValues(t, 5, n)
	n, err = c.GetNumberSentinelSlavesInMemory(target)
	require.NoError(t, err)
	assert.EqualValues(t, 2, n)
}

// TestGetReplicationInfoParsesEachField checks the parse of each field, of
// lines that have no value and of numbers that do not parse.
func TestGetReplicationInfoParsesEachField(t *testing.T) {
	for _, tc := range []struct {
		name, info string
		want       ReplicationInfo
	}{
		{
			name: "replica",
			info: "# Replication\r\nrole:slave\r\nmaster_host:10.0.0.1\r\nmaster_port:6380\r\n" +
				"master_link_status:down\r\nmaster_sync_in_progress:1\r\nslave_repl_offset:42\r\n" +
				"master_repl_offset:43\r\nconnected_slaves:2\r\nmaster_failover_state:no-failover\r\n",
			want: ReplicationInfo{
				Role: "slave", MasterHost: "10.0.0.1", MasterPort: "6380", MasterLinkStatus: "down",
				SyncInProgress: true, SlaveReplOffset: 42, MasterReplOffset: 43, ConnectedSlaves: 2,
				FailoverState: "no-failover",
			},
		},
		{
			name: "spaces, a line without a value and bad numbers",
			info: "\r\n # comment\r\nrole : master \r\nnovalue\r\nslave_repl_offset:x\r\n" +
				"master_repl_offset:y\r\nconnected_slaves:z\r\nmaster_sync_in_progress:0\r\nmaster_host:a:b\r\n",
			want: ReplicationInfo{Role: "master", MasterHost: "a:b"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host, port := fakeServer(t, "$"+strconv.Itoa(len(tc.info))+"\r\n"+tc.info+"\r\n")
			rec := &mMetrics.Recorder{}
			rec.On("RecordRedisOperation", metrics.KIND_REDIS, host, metrics.GET_REPLICATION_INFO, metrics.SUCCESS, metrics.NOT_APPLICABLE).Once()

			info, err := New(rec).GetReplicationInfo(host, port, "")

			require.NoError(t, err)
			assert.Equal(t, tc.want, *info)
			rec.AssertExpectations(t)
		})
	}
}
