package redis

import (
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"

	rediscli "github.com/go-redis/redis/v8"
	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
)

// ReplicationInfo contains replication information for a Redis instance
type ReplicationInfo struct {
	Role             string // "master" or "slave"
	MasterHost       string // only for slaves
	MasterPort       string // only for slaves
	MasterLinkStatus string // "up" or "down"
	SlaveReplOffset  int64  // replication offset for slaves
	MasterReplOffset int64  // replication offset for masters
	ConnectedSlaves  int    // number of connected slaves (for masters)
	SyncInProgress   bool   // true if slave is syncing
}

// MemoryInfo contains the memory figures of a Redis instance relevant to maxmemory
type MemoryInfo struct {
	MaxMemory       int64
	MaxMemoryPolicy string
	UsedMemory      int64 // used_memory minus mem_not_counted_for_evict, as compared against maxmemory
	UsedMemoryRSS   int64 // memory that the process holds, which often keeps the memory that evictions free
	Role            string
	Loading         bool // used_memory does not show the whole dataset yet
}

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
	SetCustomSentinelConfig(ip string, configs []string) error
	SetCustomRedisConfig(ip string, port string, configs []string, password string) error
	SlaveIsReady(ip, port, password string) (bool, error)
	SentinelCheckQuorum(ip string) error
	GetReplicationInfo(ip, port, password string) (*ReplicationInfo, error)
	GetMemoryInfo(ip, port, password string) (*MemoryInfo, error)
	SetPassword(ip, port, password, newPassword string) error
	SetSentinelAuthPass(ip, password string) error
}

type client struct {
	metricsRecorder metrics.Recorder
}

// New returns a redis client
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
	masterName              = "mymaster"
)

// errMalformedSentinelMaster does not carry the reply, because the reply is
// not trusted.
var errMalformedSentinelMaster = errors.New("malformed SENTINEL MASTER reply")

// errNoIP is the error for a pod that has no IP yet. An empty host makes the
// client dial the loopback address of the operator and send the password there.
// IsUnreachableError treats it as an unreachable node.
var errNoIP = errors.New("pod has no IP yet")

var (
	sentinelNumberRE = regexp.MustCompile(sentinelsNumberREString)
	sentinelStatusRE = regexp.MustCompile(sentinelStatusREString)
	slaveNumberRE    = regexp.MustCompile(slaveNumberREString)
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

// GetNumberSentinelsInMemory return the number of sentinels that the requested sentinel has
func (c *client) GetNumberSentinelsInMemory(ip string) (int32, error) {
	if ip == "" {
		return 0, errNoIP
	}
	options := redisOptions(net.JoinHostPort(ip, sentinelPort), "")
	rClient := rediscli.NewClient(options)
	defer func(rClient *rediscli.Client) {
		err := rClient.Close()
		if err != nil {
			log.Error(err.Error())
		}
	}(rClient)
	info, err := rClient.Info(context.TODO(), "sentinel").Result()
	if err != nil {
		c.metricsRecorder.RecordRedisOperation(metrics.KIND_SENTINEL, ip, metrics.GET_NUM_SENTINELS_IN_MEM, metrics.FAIL, getRedisError(err))
		return 0, err
	}
	if err2 := isSentinelReady(info); err2 != nil {
		c.metricsRecorder.RecordRedisOperation(metrics.KIND_SENTINEL, ip, metrics.GET_NUM_SENTINELS_IN_MEM, metrics.FAIL, metrics.SENTINEL_NOT_READY)
		return 0, err2
	}
	match := sentinelNumberRE.FindStringSubmatch(info)
	if len(match) == 0 {
		c.metricsRecorder.RecordRedisOperation(metrics.KIND_SENTINEL, ip, metrics.GET_NUM_SENTINELS_IN_MEM, metrics.FAIL, metrics.REGEX_NOT_FOUND)
		return 0, errors.New("sentinel regex not found")
	}
	nSentinels, err := strconv.Atoi(match[1])
	if err != nil {
		c.metricsRecorder.RecordRedisOperation(metrics.KIND_SENTINEL, ip, metrics.GET_NUM_SENTINELS_IN_MEM, metrics.FAIL, metrics.MISC)
		return 0, err
	}
	if nSentinels > 65536 {
		c.metricsRecorder.RecordRedisOperation(metrics.KIND_SENTINEL, ip, metrics.GET_NUM_SENTINELS_IN_MEM, metrics.FAIL, metrics.SENTINEL_TOO_MANY)
		return 0, fmt.Errorf("sentinel reports %d sentinels, more than 65536", nSentinels)
	}
	c.metricsRecorder.RecordRedisOperation(metrics.KIND_SENTINEL, ip, metrics.GET_NUM_SENTINELS_IN_MEM, metrics.SUCCESS, metrics.NOT_APPLICABLE)
	return int32(nSentinels), nil
}

// GetNumberSentinelSlavesInMemory returns the number of slaves that the requested sentinel knows for its master
func (c *client) GetNumberSentinelSlavesInMemory(ip string) (int32, error) {
	if ip == "" {
		return 0, errNoIP
	}
	options := redisOptions(net.JoinHostPort(ip, sentinelPort), "")
	rClient := rediscli.NewClient(options)
	defer func(rClient *rediscli.Client) {
		err := rClient.Close()
		if err != nil {
			log.Error(err.Error())
		}
	}(rClient)
	info, err := rClient.Info(context.TODO(), "sentinel").Result()
	if err != nil {
		c.metricsRecorder.RecordRedisOperation(metrics.KIND_SENTINEL, ip, metrics.GET_NUM_REDIS_SLAVES_IN_MEM, metrics.FAIL, getRedisError(err))
		return 0, err
	}
	if err2 := isSentinelReady(info); err2 != nil {
		c.metricsRecorder.RecordRedisOperation(metrics.KIND_SENTINEL, ip, metrics.GET_NUM_REDIS_SLAVES_IN_MEM, metrics.FAIL, metrics.SENTINEL_NOT_READY)
		return 0, err2
	}
	match := slaveNumberRE.FindStringSubmatch(info)
	if len(match) == 0 {
		c.metricsRecorder.RecordRedisOperation(metrics.KIND_SENTINEL, ip, metrics.GET_NUM_REDIS_SLAVES_IN_MEM, metrics.FAIL, metrics.REGEX_NOT_FOUND)
		return 0, errors.New("slaves regex not found")
	}
	nSlaves, err := strconv.Atoi(match[1])
	if err != nil {
		c.metricsRecorder.RecordRedisOperation(metrics.KIND_SENTINEL, ip, metrics.GET_NUM_REDIS_SLAVES_IN_MEM, metrics.FAIL, metrics.MISC)
		return 0, err
	}
	if nSlaves > 65536 {
		c.metricsRecorder.RecordRedisOperation(metrics.KIND_SENTINEL, ip, metrics.GET_NUM_REDIS_SLAVES_IN_MEM, metrics.FAIL, metrics.SENTINEL_TOO_MANY)
		return 0, fmt.Errorf("sentinel reports %d replicas, more than 65536", nSlaves)
	}
	c.metricsRecorder.RecordRedisOperation(metrics.KIND_SENTINEL, ip, metrics.GET_NUM_REDIS_SLAVES_IN_MEM, metrics.SUCCESS, metrics.NOT_APPLICABLE)
	return int32(nSlaves), nil
}

func isSentinelReady(info string) error {
	matchStatus := sentinelStatusRE.FindStringSubmatch(info)
	if len(matchStatus) == 0 || matchStatus[1] != "ok" {
		return errors.New("sentinels not ready")
	}
	return nil
}

// ResetSentinel sends a sentinel reset * for the given sentinel
func (c *client) ResetSentinel(ip string) error {
	if ip == "" {
		return errNoIP
	}
	options := redisOptions(net.JoinHostPort(ip, sentinelPort), "")
	rClient := rediscli.NewClient(options)
	defer func(rClient *rediscli.Client) {
		err := rClient.Close()
		if err != nil {
			log.Error(err.Error())
		}
	}(rClient)
	cmd := rediscli.NewIntCmd(context.TODO(), "SENTINEL", "reset", "*")
	err := rClient.Process(context.TODO(), cmd)
	if err != nil {
		c.metricsRecorder.RecordRedisOperation(metrics.KIND_SENTINEL, ip, metrics.RESET_SENTINEL, metrics.FAIL, getRedisError(err))
		return err
	}
	_, err = cmd.Result()
	if err != nil {
		c.metricsRecorder.RecordRedisOperation(metrics.KIND_SENTINEL, ip, metrics.RESET_SENTINEL, metrics.FAIL, getRedisError(err))
		return err
	}
	c.metricsRecorder.RecordRedisOperation(metrics.KIND_SENTINEL, ip, metrics.RESET_SENTINEL, metrics.SUCCESS, metrics.NOT_APPLICABLE)
	return nil
}

// GetSlaveOf returns the master_host of the given Redis, or "" for a master.
func (c *client) GetSlaveOf(ip, port, password string) (string, error) {
	if ip == "" {
		return "", errNoIP
	}
	options := redisOptions(net.JoinHostPort(ip, port), password)
	rClient := rediscli.NewClient(options)
	defer func(rClient *rediscli.Client) {
		err := rClient.Close()
		if err != nil {
			log.Error(err.Error())
		}
	}(rClient)
	info, err := rClient.Info(context.TODO(), "replication").Result()
	if err != nil {
		c.metricsRecorder.RecordRedisOperation(metrics.KIND_REDIS, ip, metrics.GET_SLAVE_OF, metrics.FAIL, getRedisError(err))
		log.Errorf("error while getting masterIP : Failed to get info replication while querying redis instance %v", ip)
		return "", err
	}
	c.metricsRecorder.RecordRedisOperation(metrics.KIND_REDIS, ip, metrics.GET_SLAVE_OF, metrics.SUCCESS, metrics.NOT_APPLICABLE)
	// master_host can be an IPv6 address or a hostname, so take the full value.
	for _, line := range strings.Split(info, "\n") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), "master_host:"); ok {
			return value, nil
		}
	}
	return "", nil
}

func (c *client) IsMaster(ip, port, password string) (bool, error) {
	if ip == "" {
		return false, errNoIP
	}
	options := redisOptions(net.JoinHostPort(ip, port), password)
	rClient := rediscli.NewClient(options)
	defer func(rClient *rediscli.Client) {
		err := rClient.Close()
		if err != nil {
			log.Error(err.Error())
		}
	}(rClient)
	info, err := rClient.Info(context.TODO(), "replication").Result()
	if err != nil {
		c.metricsRecorder.RecordRedisOperation(metrics.KIND_REDIS, ip, metrics.IS_MASTER, metrics.FAIL, getRedisError(err))
		return false, err
	}
	c.metricsRecorder.RecordRedisOperation(metrics.KIND_REDIS, ip, metrics.IS_MASTER, metrics.SUCCESS, metrics.NOT_APPLICABLE)
	return strings.Contains(info, redisRoleMaster), nil
}

func (c *client) MonitorRedis(ip, monitor, quorum, password string) error {
	return c.MonitorRedisWithPort(ip, monitor, redisPort, quorum, password)
}

func (c *client) MonitorRedisWithPort(ip, monitor, port, quorum, password string) error {
	if ip == "" {
		return errNoIP
	}
	options := redisOptions(net.JoinHostPort(ip, sentinelPort), "")
	rClient := rediscli.NewClient(options)
	defer func(rClient *rediscli.Client) {
		err := rClient.Close()
		if err != nil {
			log.Error(err.Error())
		}
	}(rClient)
	cmd := rediscli.NewBoolCmd(context.TODO(), "SENTINEL", "REMOVE", masterName)
	_ = rClient.Process(context.TODO(), cmd)
	// We'll continue even if it fails, the priority is to have the redises monitored
	cmd = rediscli.NewBoolCmd(context.TODO(), "SENTINEL", "MONITOR", masterName, monitor, port, quorum)
	err := rClient.Process(context.TODO(), cmd)
	if err != nil {
		c.metricsRecorder.RecordRedisOperation(metrics.KIND_REDIS, ip, metrics.MONITOR_REDIS_WITH_PORT, metrics.FAIL, getRedisError(err))
		return err
	}
	_, err = cmd.Result()
	if err != nil {
		c.metricsRecorder.RecordRedisOperation(metrics.KIND_REDIS, ip, metrics.MONITOR_REDIS_WITH_PORT, metrics.FAIL, getRedisError(err))
		return err
	}

	if password != "" {
		cmd = rediscli.NewBoolCmd(context.TODO(), "SENTINEL", "SET", masterName, "auth-pass", password)
		err := rClient.Process(context.TODO(), cmd)
		if err != nil {
			c.metricsRecorder.RecordRedisOperation(metrics.KIND_REDIS, ip, metrics.MONITOR_REDIS_WITH_PORT, metrics.FAIL, getRedisError(err))
			return err
		}
		_, err = cmd.Result()
		if err != nil {
			c.metricsRecorder.RecordRedisOperation(metrics.KIND_REDIS, ip, metrics.MONITOR_REDIS_WITH_PORT, metrics.FAIL, getRedisError(err))
			return err
		}
	}
	c.metricsRecorder.RecordRedisOperation(metrics.KIND_REDIS, ip, metrics.MONITOR_REDIS_WITH_PORT, metrics.SUCCESS, metrics.NOT_APPLICABLE)
	return nil
}

func (c *client) MakeMaster(ip string, port string, password string) error {
	if ip == "" {
		return errNoIP
	}
	options := redisOptions(net.JoinHostPort(ip, port), password)
	rClient := rediscli.NewClient(options)
	defer func(rClient *rediscli.Client) {
		err := rClient.Close()
		if err != nil {
			log.Error(err.Error())
		}
	}(rClient)
	if res := rClient.SlaveOf(context.TODO(), "NO", "ONE"); res.Err() != nil {
		c.metricsRecorder.RecordRedisOperation(metrics.KIND_REDIS, ip, metrics.MAKE_MASTER, metrics.FAIL, getRedisError(res.Err()))
		return res.Err()
	}
	c.metricsRecorder.RecordRedisOperation(metrics.KIND_REDIS, ip, metrics.MAKE_MASTER, metrics.SUCCESS, metrics.NOT_APPLICABLE)
	return nil
}

func (c *client) MakeSlaveOf(ip, masterIP, password string) error {
	return c.MakeSlaveOfWithPort(ip, redisPort, masterIP, redisPort, password)
}

// MakeSlaveOfWithPort reconfigures the Redis instance at ip:port to become a
// replica of masterIP:masterPort. port is the target's own listening port -
// it must not be assumed to equal masterPort, since a target and its master
// can be configured with different ports (e.g. Bootstrapping mode's
// externally supplied master port).
func (c *client) MakeSlaveOfWithPort(ip, port, masterIP, masterPort, password string) error {
	if ip == "" {
		return errNoIP
	}
	options := redisOptions(net.JoinHostPort(ip, port), password)
	rClient := rediscli.NewClient(options)
	defer func(rClient *rediscli.Client) {
		err := rClient.Close()
		if err != nil {
			log.Error(err.Error())
		}
	}(rClient)
	if res := rClient.SlaveOf(context.TODO(), masterIP, masterPort); res.Err() != nil {
		c.metricsRecorder.RecordRedisOperation(metrics.KIND_REDIS, ip, metrics.MAKE_SLAVE_OF, metrics.FAIL, getRedisError(res.Err()))
		return res.Err()
	}
	c.metricsRecorder.RecordRedisOperation(metrics.KIND_REDIS, ip, metrics.MAKE_SLAVE_OF, metrics.SUCCESS, metrics.NOT_APPLICABLE)
	return nil
}

func closeClient(rClient *rediscli.Client) {
	if err := rClient.Close(); err != nil {
		log.Error(err.Error())
	}
}

// DisconnectClients closes every normal and pub/sub client connection on the
// given instance. Replication links are left alone.
func (c *client) DisconnectClients(ip, port, password string) error {
	if ip == "" {
		return errNoIP
	}
	options := redisOptions(net.JoinHostPort(ip, port), password)
	rClient := rediscli.NewClient(options)
	defer closeClient(rClient)

	var errs []error
	for _, clientType := range []string{"normal", "pubsub"} {
		if err := rClient.ClientKillByFilter(context.TODO(), "TYPE", clientType).Err(); err != nil {
			errs = append(errs, fmt.Errorf("CLIENT KILL TYPE %s: %w", clientType, err))
			if IsUnreachableError(err) {
				break
			}
		}
	}
	if err := errors.Join(errs...); err != nil {
		c.metricsRecorder.RecordRedisOperation(metrics.KIND_REDIS, ip, metrics.DISCONNECT_CLIENTS, metrics.FAIL, getRedisError(errs[0]))
		return err
	}
	c.metricsRecorder.RecordRedisOperation(metrics.KIND_REDIS, ip, metrics.DISCONNECT_CLIENTS, metrics.SUCCESS, metrics.NOT_APPLICABLE)
	return nil
}

func (c *client) GetSentinelMonitor(ip string) (string, string, error) {
	if ip == "" {
		return "", "", errNoIP
	}
	options := redisOptions(net.JoinHostPort(ip, sentinelPort), "")
	rClient := rediscli.NewClient(options)
	defer func(rClient *rediscli.Client) {
		err := rClient.Close()
		if err != nil {
			log.Error(err.Error())
		}
	}(rClient)
	cmd := rediscli.NewSliceCmd(context.TODO(), "SENTINEL", "master", masterName)
	err := rClient.Process(context.TODO(), cmd)
	if err != nil {
		c.metricsRecorder.RecordRedisOperation(metrics.KIND_SENTINEL, ip, metrics.GET_SENTINEL_MONITOR, metrics.FAIL, getRedisError(err))
		return "", "", err
	}
	res, err := cmd.Result()
	if err != nil {
		c.metricsRecorder.RecordRedisOperation(metrics.KIND_SENTINEL, ip, metrics.GET_SENTINEL_MONITOR, metrics.FAIL, getRedisError(err))
		return "", "", err
	}
	// The reply comes from the Sentinel image, which a tenant can choose.
	if len(res) < 6 {
		c.metricsRecorder.RecordRedisOperation(metrics.KIND_SENTINEL, ip, metrics.GET_SENTINEL_MONITOR, metrics.FAIL, metrics.MISC)
		return "", "", errMalformedSentinelMaster
	}
	masterIP, okIP := res[3].(string)
	masterPort, okPort := res[5].(string)
	if !okIP || !okPort {
		c.metricsRecorder.RecordRedisOperation(metrics.KIND_SENTINEL, ip, metrics.GET_SENTINEL_MONITOR, metrics.FAIL, metrics.MISC)
		return "", "", errMalformedSentinelMaster
	}
	c.metricsRecorder.RecordRedisOperation(metrics.KIND_SENTINEL, ip, metrics.GET_SENTINEL_MONITOR, metrics.SUCCESS, metrics.NOT_APPLICABLE)
	return masterIP, masterPort, nil
}

// GetSentinelReplicas returns the IPs of the replicas that the Sentinel knows
// for its master. The Sentinel fails over only to a replica that it knows.
func (c *client) GetSentinelReplicas(ip string) ([]string, error) {
	if ip == "" {
		return nil, errNoIP
	}
	options := redisOptions(net.JoinHostPort(ip, sentinelPort), "")
	rClient := rediscli.NewClient(options)
	defer closeClient(rClient)
	cmd := rediscli.NewSliceCmd(context.TODO(), "SENTINEL", "replicas", masterName)
	var replicas []string
	err := rClient.Process(context.TODO(), cmd)
	if err == nil {
		replicas, err = sentinelReplicaIPs(cmd.Val())
	}
	if err != nil {
		c.metricsRecorder.RecordRedisOperation(metrics.KIND_SENTINEL, ip, metrics.GET_SENTINEL_REPLICAS, metrics.FAIL, getRedisError(err))
		return nil, err
	}
	c.metricsRecorder.RecordRedisOperation(metrics.KIND_SENTINEL, ip, metrics.GET_SENTINEL_REPLICAS, metrics.SUCCESS, metrics.NOT_APPLICABLE)
	return replicas, nil
}

// SentinelMasterDown reports whether the Sentinel flags its master as down
// (s_down or o_down). A Sentinel that still reaches its master does not.
func (c *client) SentinelMasterDown(ip string) (bool, error) {
	if ip == "" {
		return false, errNoIP
	}
	rClient := rediscli.NewClient(redisOptions(net.JoinHostPort(ip, sentinelPort), ""))
	defer closeClient(rClient)
	info, err := c.getSentinelMasterInfo(rClient)
	if err != nil {
		c.metricsRecorder.RecordRedisOperation(metrics.KIND_SENTINEL, ip, metrics.GET_SENTINEL_MASTER_DOWN, metrics.FAIL, getRedisError(err))
		return false, err
	}
	c.metricsRecorder.RecordRedisOperation(metrics.KIND_SENTINEL, ip, metrics.GET_SENTINEL_MASTER_DOWN, metrics.SUCCESS, metrics.NOT_APPLICABLE)
	return flagsDown(info["flags"]), nil
}

// flagsDown reports whether the flags of SENTINEL MASTER mark the master down.
func flagsDown(flags string) bool {
	for _, f := range strings.Split(flags, ",") {
		if f == "s_down" || f == "o_down" {
			return true
		}
	}
	return false
}

// sentinelReplicaIPs reads the "ip" field of each replica in a SENTINEL
// REPLICAS reply. Each replica is a flat array of field names and values.
func sentinelReplicaIPs(res []interface{}) ([]string, error) {
	ips := make([]string, 0, len(res))
	for _, entry := range res {
		fields, ok := entry.([]interface{})
		if !ok {
			return nil, fmt.Errorf("unexpected SENTINEL REPLICAS entry %v", entry)
		}
		for i := 0; i+1 < len(fields); i += 2 {
			if fields[i] == "ip" {
				ip, ok := fields[i+1].(string)
				if !ok {
					return nil, fmt.Errorf("unexpected SENTINEL REPLICAS ip %v", fields[i+1])
				}
				ips = append(ips, ip)
			}
		}
	}
	return ips, nil
}

func (c *client) SetCustomSentinelConfig(ip string, configs []string) error {
	if ip == "" {
		return errNoIP
	}
	options := redisOptions(net.JoinHostPort(ip, sentinelPort), "")
	rClient := rediscli.NewClient(options)
	defer func(rClient *rediscli.Client) {
		err := rClient.Close()
		if err != nil {
			log.Error(err.Error())
		}
	}(rClient)

	// SENTINEL SET writes the config file of Sentinel also for an unchanged
	// value. Each reconcile then gives a disk write and a log line. Thus the
	// client reads the current values and sets only the changed ones. When
	// the read fails, current is nil and all the values are set.
	current, err := c.getSentinelMasterInfo(rClient)
	if err != nil {
		current = nil
	}

	toApply, err := c.sentinelConfigsToApply(current, configs)
	if err != nil {
		return err
	}
	for _, p := range toApply {
		if err := c.applySentinelConfig(p.param, p.value, rClient); err != nil {
			return err
		}
	}
	return nil
}

// sentinelConfigParam is a single "SENTINEL SET <param> <value>" pair.
type sentinelConfigParam struct {
	param string
	value string
}

// sentinelConfigsToApply returns the "param value" configs whose value
// differs from current. A nil current returns all the configs, so that no
// change is lost when the read failed.
func (c *client) sentinelConfigsToApply(current map[string]string, configs []string) ([]sentinelConfigParam, error) {
	var toApply []sentinelConfigParam
	for _, config := range configs {
		param, value, err := c.getConfigParameters(config)
		if err != nil {
			return nil, err
		}
		// SENTINEL SET takes the name in any case. SENTINEL MASTER gives it in
		// lower case.
		if current != nil && current[strings.ToLower(param)] == value {
			continue
		}
		toApply = append(toApply, sentinelConfigParam{param: param, value: value})
	}
	return toApply, nil
}

// getSentinelMasterInfo returns the reply of SENTINEL MASTER as a map of
// field name to value.
func (c *client) getSentinelMasterInfo(rClient *rediscli.Client) (map[string]string, error) {
	cmd := rediscli.NewSliceCmd(context.TODO(), "SENTINEL", "master", masterName)
	if err := rClient.Process(context.TODO(), cmd); err != nil {
		return nil, err
	}
	// Process returns the error of cmd, so Val needs no error check.
	res := cmd.Val()
	info := make(map[string]string, len(res)/2)
	for i := 0; i+1 < len(res); i += 2 {
		// SENTINEL MASTER gives each name and value as a bulk string, as in
		// GetSentinelMonitor.
		name, okName := res[i].(string)
		value, okValue := res[i+1].(string)
		if !okName || !okValue {
			return nil, errMalformedSentinelMaster
		}
		info[name] = value
	}
	return info, nil
}

func (c *client) SentinelCheckQuorum(ip string) error {
	if ip == "" {
		return errNoIP
	}
	options := redisOptions(net.JoinHostPort(ip, sentinelPort), "")
	rClient := rediscli.NewSentinelClient(options)
	defer func(rClient *rediscli.SentinelClient) {
		err := rClient.Close()
		if err != nil {
			log.Error(err.Error())
		}
	}(rClient)
	cmd := rClient.CkQuorum(context.TODO(), masterName)
	res, err := cmd.Result()

	if err != nil {
		// SENTINEL CKQUORUM gives NOQUORUM as an error reply, not as a
		// string reply, so only this branch can see it.
		if strings.Contains(err.Error(), "NOQUORUM") {
			log.Debugf("SentinelCheckQuorum: quorum not available: %s", err.Error())
			c.metricsRecorder.RecordRedisOperation(metrics.KIND_SENTINEL, ip, metrics.CHECK_SENTINEL_QUORUM, metrics.SUCCESS, "NOQUORUM")
			return fmt.Errorf("quorum Not available")
		}
		log.Warnf("Unable to get result for CKQUORUM comand")
		c.metricsRecorder.RecordRedisOperation(metrics.KIND_SENTINEL, ip, metrics.CHECK_SENTINEL_QUORUM, metrics.FAIL, getRedisError(err))
		return err
	}
	log.Debugf("SentinelCheckQuorum cmd result: %s", res)
	s := strings.Split(res, " ")
	status := s[0]

	if status == "OK" {
		c.metricsRecorder.RecordRedisOperation(metrics.KIND_SENTINEL, ip, metrics.CHECK_SENTINEL_QUORUM, metrics.SUCCESS, "QUORUM")
		return nil
	} else {
		log.Errorf("quorum command status unexpected !!!")
		c.metricsRecorder.RecordRedisOperation(metrics.KIND_SENTINEL, ip, metrics.CHECK_SENTINEL_QUORUM, metrics.FAIL, "quorum command status unexpected output")
		return fmt.Errorf("quorum status unexpected %s", status)
	}

}
func (c *client) SetCustomRedisConfig(ip string, port string, configs []string, password string) error {
	if ip == "" {
		return errNoIP
	}
	options := redisOptions(net.JoinHostPort(ip, port), password)
	rClient := rediscli.NewClient(options)
	defer func(rClient *rediscli.Client) {
		err := rClient.Close()
		if err != nil {
			log.Error(err.Error())
		}
	}(rClient)

	needsACLLoad := false
	for _, config := range configs {
		param, value, err := c.getConfigParameters(config)
		if err != nil {
			return err
		}
		// Skip an empty line. An empty value, as in `save ""`, is valid.
		if strings.TrimSpace(param) == "" {
			continue
		}
		// Redis refuses CONFIG SET for aclfile, also with the same path. A new
		// path needs a restart. ACL LOAD reads the configured aclfile again,
		// so the client sends ACL LOAD in place of CONFIG SET.
		if strings.EqualFold(param, "aclfile") {
			needsACLLoad = true
			continue
		}
		if err := c.applyRedisConfig(param, value, rClient); err != nil {
			return err
		}
	}
	if needsACLLoad {
		if err := c.applyACLLoad(rClient); err != nil {
			return err
		}
	}
	return nil
}

func (c *client) applyACLLoad(rClient *rediscli.Client) error {
	cmd := rediscli.NewStatusCmd(context.TODO(), "ACL", "LOAD")
	err := rClient.Process(context.TODO(), cmd)
	if err != nil {
		c.metricsRecorder.RecordRedisOperation(metrics.KIND_REDIS, addrHost(rClient.Options().Addr), metrics.APPLY_REDIS_CONFIG, metrics.FAIL, getRedisError(err))
		return err
	}
	if _, err := cmd.Result(); err != nil {
		c.metricsRecorder.RecordRedisOperation(metrics.KIND_REDIS, addrHost(rClient.Options().Addr), metrics.APPLY_REDIS_CONFIG, metrics.FAIL, getRedisError(err))
		return err
	}
	c.metricsRecorder.RecordRedisOperation(metrics.KIND_REDIS, addrHost(rClient.Options().Addr), metrics.APPLY_REDIS_CONFIG, metrics.SUCCESS, metrics.NOT_APPLICABLE)
	return nil
}

func (c *client) applyRedisConfig(parameter string, value string, rClient *rediscli.Client) error {
	result := rClient.ConfigSet(context.TODO(), parameter, value)
	if nil != result.Err() {
		c.metricsRecorder.RecordRedisOperation(metrics.KIND_REDIS, addrHost(rClient.Options().Addr), metrics.APPLY_REDIS_CONFIG, metrics.FAIL, getRedisError(result.Err()))
		return result.Err()
	}
	c.metricsRecorder.RecordRedisOperation(metrics.KIND_REDIS, addrHost(rClient.Options().Addr), metrics.APPLY_REDIS_CONFIG, metrics.SUCCESS, metrics.NOT_APPLICABLE)
	return result.Err()
}

func (c *client) applySentinelConfig(parameter string, value string, rClient *rediscli.Client) error {
	cmd := rediscli.NewStatusCmd(context.TODO(), "SENTINEL", "set", masterName, parameter, value)
	err := rClient.Process(context.TODO(), cmd)
	if err != nil {
		c.metricsRecorder.RecordRedisOperation(metrics.KIND_SENTINEL, addrHost(rClient.Options().Addr), metrics.APPLY_SENTINEL_CONFIG, metrics.FAIL, getRedisError(err))
		return err
	}
	c.metricsRecorder.RecordRedisOperation(metrics.KIND_SENTINEL, addrHost(rClient.Options().Addr), metrics.APPLY_SENTINEL_CONFIG, metrics.SUCCESS, metrics.NOT_APPLICABLE)
	return cmd.Err()
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

func (c *client) getConfigParameters(config string) (parameter string, value string, err error) {
	s := strings.Split(config, " ")
	if len(s) < 2 {
		return "", "", fmt.Errorf("configuration '%s' malformed", config)
	}
	if len(s) == 2 && s[1] == `""` {
		return s[0], "", nil
	}
	return s[0], strings.Join(s[1:], " "), nil
}

func (c *client) SlaveIsReady(ip, port, password string) (bool, error) {
	if ip == "" {
		return false, errNoIP
	}
	options := redisOptions(net.JoinHostPort(ip, port), password)
	rClient := rediscli.NewClient(options)
	defer func(rClient *rediscli.Client) {
		err := rClient.Close()
		if err != nil {
			log.Error(err.Error())
		}
	}(rClient)
	info, err := rClient.Info(context.TODO(), "replication").Result()
	if err != nil {
		c.metricsRecorder.RecordRedisOperation(metrics.KIND_REDIS, addrHost(rClient.Options().Addr), metrics.SLAVE_IS_READY, metrics.FAIL, getRedisError(err))
		return false, err
	}

	ok := !strings.Contains(info, redisSyncing) &&
		!strings.Contains(info, redisMasterSillPending) &&
		strings.Contains(info, redisLinkUp)
	c.metricsRecorder.RecordRedisOperation(metrics.KIND_REDIS, addrHost(rClient.Options().Addr), metrics.SLAVE_IS_READY, metrics.SUCCESS, metrics.NOT_APPLICABLE)
	return ok, nil
}

// GetReplicationInfo returns detailed replication information for a Redis instance.
// This is used for operator-managed failover to select the best replica for promotion.
func (c *client) GetReplicationInfo(ip, port, password string) (*ReplicationInfo, error) {
	if ip == "" {
		return nil, errNoIP
	}
	options := redisOptions(net.JoinHostPort(ip, port), password)
	rClient := rediscli.NewClient(options)
	defer func(rClient *rediscli.Client) {
		err := rClient.Close()
		if err != nil {
			log.Error(err.Error())
		}
	}(rClient)

	info, err := rClient.Info(context.TODO(), "replication").Result()
	if err != nil {
		c.metricsRecorder.RecordRedisOperation(metrics.KIND_REDIS, ip, metrics.GET_REPLICATION_INFO, metrics.FAIL, getRedisError(err))
		return nil, err
	}

	replInfo := &ReplicationInfo{}

	// Parse the INFO replication output
	lines := strings.Split(info, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])

		switch key {
		case "role":
			replInfo.Role = value
		case "master_host":
			replInfo.MasterHost = value
		case "master_port":
			replInfo.MasterPort = value
		case "master_link_status":
			replInfo.MasterLinkStatus = value
		case "slave_repl_offset":
			if offset, err := strconv.ParseInt(value, 10, 64); err == nil {
				replInfo.SlaveReplOffset = offset
			}
		case "master_repl_offset":
			if offset, err := strconv.ParseInt(value, 10, 64); err == nil {
				replInfo.MasterReplOffset = offset
			}
		case "connected_slaves":
			if count, err := strconv.Atoi(value); err == nil {
				replInfo.ConnectedSlaves = count
			}
		case "master_sync_in_progress":
			replInfo.SyncInProgress = value == "1"
		}
	}

	c.metricsRecorder.RecordRedisOperation(metrics.KIND_REDIS, ip, metrics.GET_REPLICATION_INFO, metrics.SUCCESS, metrics.NOT_APPLICABLE)
	return replInfo, nil
}

// SetPassword changes the password a running Redis requires and the one it
// uses to authenticate to its master. Connections already authenticated,
// including replication links, stay up.
func (c *client) SetPassword(ip, port, password, newPassword string) error {
	if ip == "" {
		return errNoIP
	}
	rClient := rediscli.NewClient(redisOptions(net.JoinHostPort(ip, port), password))
	defer func(rClient *rediscli.Client) {
		if err := rClient.Close(); err != nil {
			log.Error(err.Error())
		}
	}(rClient)
	for _, param := range []string{"masterauth", "requirepass"} {
		if err := rClient.ConfigSet(context.TODO(), param, newPassword).Err(); err != nil {
			c.metricsRecorder.RecordRedisOperation(metrics.KIND_REDIS, ip, metrics.SET_PASSWORD, metrics.FAIL, getRedisError(err))
			return err
		}
	}
	c.metricsRecorder.RecordRedisOperation(metrics.KIND_REDIS, ip, metrics.SET_PASSWORD, metrics.SUCCESS, metrics.NOT_APPLICABLE)
	return nil
}

// SetSentinelAuthPass sets the password a Sentinel uses to authenticate to the
// Redis it monitors.
func (c *client) SetSentinelAuthPass(ip, password string) error {
	if ip == "" {
		return errNoIP
	}
	rClient := rediscli.NewClient(redisOptions(net.JoinHostPort(ip, sentinelPort), ""))
	defer func(rClient *rediscli.Client) {
		if err := rClient.Close(); err != nil {
			log.Error(err.Error())
		}
	}(rClient)
	if err := rClient.Do(context.TODO(), "SENTINEL", "SET", masterName, "auth-pass", password).Err(); err != nil {
		c.metricsRecorder.RecordRedisOperation(metrics.KIND_SENTINEL, ip, metrics.SET_PASSWORD, metrics.FAIL, getRedisError(err))
		return err
	}
	c.metricsRecorder.RecordRedisOperation(metrics.KIND_SENTINEL, ip, metrics.SET_PASSWORD, metrics.SUCCESS, metrics.NOT_APPLICABLE)
	return nil
}

// GetMemoryInfo returns the maxmemory settings, memory usage and role of a Redis instance.
func (c *client) GetMemoryInfo(ip, port, password string) (*MemoryInfo, error) {
	if ip == "" {
		return nil, errNoIP
	}
	rClient := rediscli.NewClient(redisOptions(net.JoinHostPort(ip, port), password))
	defer func(rClient *rediscli.Client) {
		if err := rClient.Close(); err != nil {
			log.Error(err.Error())
		}
	}(rClient)

	mi := &MemoryInfo{}
	var notCounted int64
	for _, section := range []string{"memory", "replication", "persistence"} {
		info, err := rClient.Info(context.TODO(), section).Result()
		if err != nil {
			c.metricsRecorder.RecordRedisOperation(metrics.KIND_REDIS, ip, metrics.GET_MEMORY_INFO, metrics.FAIL, getRedisError(err))
			return nil, err
		}
		for _, line := range strings.Split(info, "\n") {
			key, value, ok := strings.Cut(strings.TrimSpace(line), ":")
			if !ok {
				continue
			}
			n, _ := strconv.ParseInt(value, 10, 64)
			switch key {
			case "maxmemory":
				mi.MaxMemory = n
			case "maxmemory_policy":
				mi.MaxMemoryPolicy = value
			case "role":
				mi.Role = value
			case "used_memory":
				mi.UsedMemory = n
			case "used_memory_rss":
				mi.UsedMemoryRSS = n
			case "mem_not_counted_for_evict":
				notCounted = n
			case "loading":
				mi.Loading = n == 1
			}
		}
	}
	mi.UsedMemory -= notCounted

	c.metricsRecorder.RecordRedisOperation(metrics.KIND_REDIS, ip, metrics.GET_MEMORY_INFO, metrics.SUCCESS, metrics.NOT_APPLICABLE)
	return mi, nil
}

func getRedisError(err error) string {
	if strings.Contains(err.Error(), "NOAUTH") {
		return metrics.NOAUTH
	} else if strings.Contains(err.Error(), "WRONGPASS") {
		return metrics.WRONG_PASSWORD_USED
	} else if strings.Contains(err.Error(), "NOPERM") {
		return metrics.NOPERM
	} else if strings.Contains(err.Error(), "i/o timeout") {
		return metrics.IO_TIMEOUT
	} else if strings.Contains(err.Error(), "connection refused") {
		return metrics.CONNECTION_REFUSED
	} else {
		return "MISC"
	}
}

// IsAuthError reports whether Redis refused the password (WRONGPASS), needs
// a password (NOAUTH), or got a password while it has none.
func IsAuthError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "WRONGPASS") ||
		strings.Contains(msg, "NOAUTH") ||
		IsNoPasswordError(err)
}

// IsNoPasswordError reports whether Redis was given a password while it has
// none configured.
func IsNoPasswordError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "without any password configured")
}

// IsUnreachableError reports whether the client could not reach the Redis
// node, for example after a dial error, a timeout or a reset. The callers skip
// such a node, but return the errors of a command, for example a bad config
// value or a wrong password.
func IsUnreachableError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errNoIP) {
		return true
	}

	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}

	msg := err.Error()
	return strings.Contains(msg, "i/o timeout") ||
		strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "no route to host") ||
		strings.Contains(msg, "network is unreachable") ||
		strings.Contains(msg, "connection reset")
}
