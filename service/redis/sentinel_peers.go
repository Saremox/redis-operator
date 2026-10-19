package redis

import (
	"context"
	"fmt"
	"net"

	rediscli "github.com/go-redis/redis/v8"
	"github.com/saremox/redis-operator/metrics"
)

// SentinelPeers is the group that a Sentinel knows.
type SentinelPeers struct {
	// Master is the IP of the master in SENTINEL MASTER.
	Master string
	// Peers holds the IPs of the other Sentinels in SENTINEL SENTINELS.
	Peers []string
}

// GetSentinelPeers reads the master and the other Sentinels of the Sentinel
// on one connection. A Sentinel accepts each peer that sends the master name,
// so these IPs can belong to another group.
func (c *client) GetSentinelPeers(ip string) (SentinelPeers, error) {
	if ip == "" {
		return SentinelPeers{}, errNoIP
	}
	rClient := rediscli.NewClient(quickOptions(net.JoinHostPort(ip, sentinelPort), ""))
	defer closeClient(rClient)
	var peers []string
	info, err := c.getSentinelMasterInfo(rClient)
	if err == nil && info["ip"] == "" {
		err = errMalformedSentinelMaster
	}
	if err == nil {
		cmd := rediscli.NewSliceCmd(context.TODO(), "SENTINEL", "sentinels", masterName)
		if err = rClient.Process(context.TODO(), cmd); err != nil {
			err = fmt.Errorf("SENTINEL SENTINELS: %w", err)
		} else {
			peers, err = sentinelReplicaIPs("SENTINEL SENTINELS", cmd.Val())
		}
	}
	if err != nil {
		c.metricsRecorder.RecordRedisOperation(metrics.KIND_SENTINEL, ip, metrics.GET_SENTINEL_PEERS, metrics.FAIL, getRedisError(err))
		return SentinelPeers{}, err
	}
	c.metricsRecorder.RecordRedisOperation(metrics.KIND_SENTINEL, ip, metrics.GET_SENTINEL_PEERS, metrics.SUCCESS, metrics.NOT_APPLICABLE)
	return SentinelPeers{Master: info["ip"], Peers: peers}, nil
}
