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
	// Peers holds the other Sentinels in SENTINEL SENTINELS.
	Peers []SentinelPeer
}

// SentinelPeer is an entry of SENTINEL SENTINELS.
type SentinelPeer struct {
	IP string
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
	var peers []SentinelPeer
	info, err := c.getSentinelMasterInfo(rClient)
	if err == nil && info["ip"] == "" {
		err = errMalformedSentinelMaster
	}
	if err == nil {
		cmd := rediscli.NewSliceCmd(context.TODO(), "SENTINEL", "sentinels", masterName)
		if err = rClient.Process(context.TODO(), cmd); err != nil {
			err = fmt.Errorf("SENTINEL SENTINELS: %w", err)
		} else {
			peers, err = sentinelPeerEntries(cmd.Val())
		}
	}
	if err != nil {
		c.metricsRecorder.RecordRedisOperation(metrics.KIND_SENTINEL, ip, metrics.GET_SENTINEL_PEERS, metrics.FAIL, getRedisError(err))
		return SentinelPeers{}, err
	}
	c.metricsRecorder.RecordRedisOperation(metrics.KIND_SENTINEL, ip, metrics.GET_SENTINEL_PEERS, metrics.SUCCESS, metrics.NOT_APPLICABLE)
	return SentinelPeers{Master: info["ip"], Peers: peers}, nil
}

// sentinelPeerEntries reads the entries of a SENTINEL SENTINELS reply. Each
// entry is a flat array of field names and values. An entry without an ip
// field is no peer, so it is skipped.
func sentinelPeerEntries(res []any) ([]SentinelPeer, error) {
	peers := make([]SentinelPeer, 0, len(res))
	for _, entry := range res {
		fields, ok := entry.([]any)
		if !ok {
			return nil, fmt.Errorf("unexpected SENTINEL SENTINELS entry %v", entry)
		}
		var peer SentinelPeer
		hasIP := false
		for i := 0; i+1 < len(fields); i += 2 {
			if fields[i] != "ip" {
				continue
			}
			value, ok := fields[i+1].(string)
			if !ok {
				return nil, fmt.Errorf("unexpected SENTINEL SENTINELS ip %v", fields[i+1])
			}
			peer.IP, hasIP = value, true
		}
		if hasIP {
			peers = append(peers, peer)
		}
	}
	return peers, nil
}
