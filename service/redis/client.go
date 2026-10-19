package redis

import (
	"net"
	"time"

	rediscli "github.com/go-redis/redis/v8"
	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
)

// Client sends the commands of the operator to Redis and Sentinel.
type Client interface {
	GetNumberSentinelsInMemory(ip string) (int32, error)
	GetNumberSentinelSlavesInMemory(ip string) (int32, error)
	ResetSentinel(ip string) error
	GetSlaveOf(ip, port, password string) (string, error)
	IsMaster(ip, port, password string) (bool, error)
	MonitorRedis(ip, monitor, quorum, password string) error
	MonitorRedisWithPort(ip, monitor, port, quorum, password string) error
	MakeMaster(ip, port, password string) error
	MakeSlaveOf(ip, masterIP, password string) error
	MakeSlaveOfWithPort(ip, port, masterIP, masterPort, password string) error
	DisconnectClients(ip, port, password string) error
	GetSentinelMonitor(ip string) (string, string, error)
	GetSentinelReplicas(ip string) ([]string, error)
	SentinelMasterDown(ip string) (bool, error)
	GetSentinelFailover(ip string) (SentinelFailover, error)
	GetSentinelMasterIP(ip string) (string, error)
	GetSentinelPeers(ip string) (SentinelPeers, error)
	SetCustomSentinelConfig(ip string, configs []string) error
	SetCustomRedisConfig(ip string, port string, configs []string, password string) error
	SlaveIsReady(ip, port, password string) (bool, error)
	SentinelCheckQuorum(ip string) error
	GetReplicationInfo(ip, port, password string) (*ReplicationInfo, error)
	GetMemoryInfo(ip, port, password string) (*MemoryInfo, error)
	SetPassword(ip, port, password, newPassword string) error
	SetSentinelAuthPass(ip, password string) error
	FailoverTo(ip, port, password, targetIP string, timeout time.Duration) error
	FailoverAbort(ip, port, password string) error
	GetRunID(ip, port, password string) (string, error)
	GetFailoverState(ip, port, password string) (string, error)
	GetAutoscalerInfo(ip, port, password string) (*AutoscalerInfo, error)
}

type client struct {
	metricsRecorder metrics.Recorder
}

// New returns a redis client.
func New(metricsRecorder metrics.Recorder) Client {
	return &client{
		metricsRecorder: metricsRecorder,
	}
}

const (
	sentinelsNumberREString = "sentinels=([0-9]+)"
	slaveNumberREString     = "slaves=([0-9]+)"
	sentinelStatusREString  = "status=([a-z]+)"
	redisRoleMaster         = "role:master"
	redisSyncing            = "master_sync_in_progress:1"
	redisMasterSillPending  = "master_host:127.0.0.1"
	redisLinkUp             = "master_link_status:up"
	redisPort               = "6379"
	sentinelPort            = "26379"
	masterName              = redisfailoverv1.DefaultSentinelMasterName
)

// Redis answers the operator in milliseconds. The go-redis defaults (3s read
// timeout, 5s dial timeout, 3 retries) let one unreachable pod hold a
// reconcile for 12-20s per call.
func redisOptions(addr, password string) *rediscli.Options {
	return &rediscli.Options{
		Addr:         addr,
		Password:     password,
		DialTimeout:  2 * time.Second,
		ReadTimeout:  2 * time.Second,
		WriteTimeout: 2 * time.Second,
		MaxRetries:   1,
	}
}

func closeClient(rClient *rediscli.Client) {
	if err := rClient.Close(); err != nil {
		log.Error(err.Error())
	}
}

// addrHost returns the host part of addr for a metric label. A split on ":"
// breaks an IPv6 address.
func addrHost(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}

// quickOptions are for a check whose caller treats a slow answer like no
// answer. Each step waits 500ms at most, and the client does not retry.
func quickOptions(addr, password string) *rediscli.Options {
	options := redisOptions(addr, password)
	options.DialTimeout, options.ReadTimeout, options.WriteTimeout = 500*time.Millisecond, 500*time.Millisecond, 500*time.Millisecond
	options.MaxRetries = -1
	return options
}
