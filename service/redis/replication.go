package redis

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	rediscli "github.com/go-redis/redis/v8"
	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
)

// ReplicationInfo contains replication information for a Redis instance.
type ReplicationInfo struct {
	Role             string // "master" or "slave"
	MasterHost       string // only for slaves
	MasterPort       string // only for slaves
	MasterLinkStatus string // "up" or "down"
	SlaveReplOffset  int64  // replication offset for slaves
	MasterReplOffset int64  // replication offset for masters
	ConnectedSlaves  int    // number of connected slaves (for masters)
	SyncInProgress   bool   // true if slave is syncing
	// FailoverState is master_failover_state. It is empty before Redis 6.2,
	// which has no FAILOVER command.
	FailoverState string
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
		return "", fmt.Errorf("INFO replication: %w", err)
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
		return false, fmt.Errorf("INFO replication: %w", err)
	}
	c.metricsRecorder.RecordRedisOperation(metrics.KIND_REDIS, ip, metrics.IS_MASTER, metrics.SUCCESS, metrics.NOT_APPLICABLE)
	return strings.Contains(info, redisRoleMaster), nil
}

func (c *client) MakeMaster(ip, port, password string) error {
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
		return fmt.Errorf("SLAVEOF NO ONE: %w", res.Err())
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
		return fmt.Errorf("SLAVEOF: %w", res.Err())
	}
	c.metricsRecorder.RecordRedisOperation(metrics.KIND_REDIS, ip, metrics.MAKE_SLAVE_OF, metrics.SUCCESS, metrics.NOT_APPLICABLE)
	return nil
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
		return false, fmt.Errorf("INFO replication: %w", err)
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
		return nil, fmt.Errorf("INFO replication: %w", err)
	}

	replInfo := parseReplicationInfo(info)
	c.metricsRecorder.RecordRedisOperation(metrics.KIND_REDIS, ip, metrics.GET_REPLICATION_INFO, metrics.SUCCESS, metrics.NOT_APPLICABLE)
	return replInfo, nil
}

func parseReplicationInfo(info string) *ReplicationInfo {
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
		setReplicationField(replInfo, key, value)
	}
	return replInfo
}

func setReplicationField(replInfo *ReplicationInfo, key, value string) {
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
	case "master_failover_state":
		replInfo.FailoverState = value
	}
}

// FailoverTo starts FAILOVER on the master at ip. The target is the replica at
// targetIP, on the same port. Redis runs the failover in the background, so
// the caller reads master_failover_state for the result.
func (c *client) FailoverTo(ip, port, password, targetIP string, timeout time.Duration) error {
	if ip == "" {
		return errNoIP
	}
	options := redisOptions(net.JoinHostPort(ip, port), password)
	// A retry after a lost reply gets "FAILOVER already in progress".
	options.MaxRetries = -1
	rClient := rediscli.NewClient(options)
	defer closeClient(rClient)
	err := rClient.Do(context.TODO(), "FAILOVER", "TO", targetIP, port, "TIMEOUT", strconv.FormatInt(timeout.Milliseconds(), 10)).Err()
	if err != nil {
		c.metricsRecorder.RecordRedisOperation(metrics.KIND_REDIS, ip, metrics.FAILOVER_TO, metrics.FAIL, getRedisError(err))
		return fmt.Errorf("FAILOVER TO: %w", err)
	}
	c.metricsRecorder.RecordRedisOperation(metrics.KIND_REDIS, ip, metrics.FAILOVER_TO, metrics.SUCCESS, metrics.NOT_APPLICABLE)
	return nil
}

// FailoverAbort ends the FAILOVER that runs on the master at ip. The master
// then is the master, with all its data, and ends the write pause.
func (c *client) FailoverAbort(ip, port, password string) error {
	if ip == "" {
		return errNoIP
	}
	options := redisOptions(net.JoinHostPort(ip, port), password)
	// A retry after a lost reply gets "No failover in progress".
	options.MaxRetries = -1
	rClient := rediscli.NewClient(options)
	defer closeClient(rClient)
	err := rClient.Do(context.TODO(), "FAILOVER", "ABORT").Err()
	if err != nil {
		c.metricsRecorder.RecordRedisOperation(metrics.KIND_REDIS, ip, metrics.FAILOVER_ABORT, metrics.FAIL, getRedisError(err))
		return fmt.Errorf("FAILOVER ABORT: %w", err)
	}
	c.metricsRecorder.RecordRedisOperation(metrics.KIND_REDIS, ip, metrics.FAILOVER_ABORT, metrics.SUCCESS, metrics.NOT_APPLICABLE)
	return nil
}

// GetFailoverState returns master_failover_state of the Redis at ip, or ""
// before Redis 6.2. It uses quickOptions.
func (c *client) GetFailoverState(ip, port, password string) (string, error) {
	if ip == "" {
		return "", errNoIP
	}
	rClient := rediscli.NewClient(quickOptions(net.JoinHostPort(ip, port), password))
	defer closeClient(rClient)
	info, err := rClient.Info(context.TODO(), "replication").Result()
	if err != nil {
		c.metricsRecorder.RecordRedisOperation(metrics.KIND_REDIS, ip, metrics.GET_FAILOVER_STATE, metrics.FAIL, getRedisError(err))
		return "", fmt.Errorf("INFO replication: %w", err)
	}
	c.metricsRecorder.RecordRedisOperation(metrics.KIND_REDIS, ip, metrics.GET_FAILOVER_STATE, metrics.SUCCESS, metrics.NOT_APPLICABLE)
	for _, line := range strings.Split(info, "\n") {
		if state, ok := strings.CutPrefix(strings.TrimSpace(line), "master_failover_state:"); ok {
			return state, nil
		}
	}
	return "", nil
}

// GetRunID returns the run_id of the Redis at ip, which changes at each
// restart. It uses quickOptions, because the caller treats a slow answer as a
// stop.
func (c *client) GetRunID(ip, port, password string) (string, error) {
	if ip == "" {
		return "", errNoIP
	}
	rClient := rediscli.NewClient(quickOptions(net.JoinHostPort(ip, port), password))
	defer closeClient(rClient)
	info, err := rClient.Info(context.TODO(), "server").Result()
	if err != nil {
		c.metricsRecorder.RecordRedisOperation(metrics.KIND_REDIS, ip, metrics.GET_RUN_ID, metrics.FAIL, getRedisError(err))
		return "", fmt.Errorf("INFO server: %w", err)
	}
	c.metricsRecorder.RecordRedisOperation(metrics.KIND_REDIS, ip, metrics.GET_RUN_ID, metrics.SUCCESS, metrics.NOT_APPLICABLE)
	for _, line := range strings.Split(info, "\n") {
		if id, ok := strings.CutPrefix(strings.TrimSpace(line), "run_id:"); ok {
			return id, nil
		}
	}
	return "", errors.New("INFO server has no run_id")
}
