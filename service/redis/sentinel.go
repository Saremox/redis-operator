package redis

import (
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	rediscli "github.com/go-redis/redis/v8"
	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
)

// The malformed reply errors do not carry the reply, because the reply is not
// trusted.
var (
	errMalformedSentinelMaster = errors.New("malformed SENTINEL MASTER reply")
	errMalformedMasterAddr     = errors.New("malformed reply")
)

var (
	sentinelNumberRE = regexp.MustCompile(sentinelsNumberREString)
	sentinelStatusRE = regexp.MustCompile(sentinelStatusREString)
	slaveNumberRE    = regexp.MustCompile(slaveNumberREString)
)

// GetNumberSentinelsInMemory return the number of sentinels that the requested sentinel has.
func (c *client) GetNumberSentinelsInMemory(ip string) (int32, error) {
	return c.getSentinelCount(ip, sentinelsCount)
}

// GetNumberSentinelSlavesInMemory returns the number of slaves that the requested sentinel knows for its master.
func (c *client) GetNumberSentinelSlavesInMemory(ip string) (int32, error) {
	return c.getSentinelCount(ip, slavesCount)
}

// sentinelCount is a count field of INFO sentinel, with the metric operation
// and the error texts of its read.
type sentinelCount struct {
	operation string
	re        *regexp.Regexp
	notFound  string
	name      string
	plural    string
}

var (
	sentinelsCount = sentinelCount{
		operation: metrics.GET_NUM_SENTINELS_IN_MEM, re: sentinelNumberRE,
		notFound: "sentinel regex not found", name: "sentinel", plural: "sentinels",
	}
	slavesCount = sentinelCount{
		operation: metrics.GET_NUM_REDIS_SLAVES_IN_MEM, re: slaveNumberRE,
		notFound: "slaves regex not found", name: "replica", plural: "replicas",
	}
)

func (c *client) getSentinelCount(ip string, count sentinelCount) (int32, error) {
	if ip == "" {
		return 0, errNoIP
	}
	options := redisOptions(net.JoinHostPort(ip, sentinelPort), "")
	rClient := rediscli.NewClient(options)
	defer closeClient(rClient)
	info, err := rClient.Info(context.TODO(), "sentinel").Result()
	if err != nil {
		c.metricsRecorder.RecordRedisOperation(metrics.KIND_SENTINEL, ip, count.operation, metrics.FAIL, getRedisError(err))
		return 0, fmt.Errorf("INFO sentinel: %w", err)
	}
	if err2 := isSentinelReady(info); err2 != nil {
		c.metricsRecorder.RecordRedisOperation(metrics.KIND_SENTINEL, ip, count.operation, metrics.FAIL, metrics.SENTINEL_NOT_READY)
		return 0, err2
	}
	match := count.re.FindStringSubmatch(info)
	if len(match) == 0 {
		c.metricsRecorder.RecordRedisOperation(metrics.KIND_SENTINEL, ip, count.operation, metrics.FAIL, metrics.REGEX_NOT_FOUND)
		return 0, errors.New(count.notFound)
	}
	n, err := strconv.ParseUint(match[1], 10, 64)
	if err != nil {
		c.metricsRecorder.RecordRedisOperation(metrics.KIND_SENTINEL, ip, count.operation, metrics.FAIL, metrics.MISC)
		return 0, fmt.Errorf("read the %s count: %w", count.name, err)
	}
	if n > 65536 {
		c.metricsRecorder.RecordRedisOperation(metrics.KIND_SENTINEL, ip, count.operation, metrics.FAIL, metrics.SENTINEL_TOO_MANY)
		return 0, fmt.Errorf("sentinel reports %d %s, more than 65536", n, count.plural)
	}
	c.metricsRecorder.RecordRedisOperation(metrics.KIND_SENTINEL, ip, count.operation, metrics.SUCCESS, metrics.NOT_APPLICABLE)
	return int32(n), nil
}

func isSentinelReady(info string) error {
	matchStatus := sentinelStatusRE.FindStringSubmatch(info)
	if len(matchStatus) == 0 || matchStatus[1] != "ok" {
		return errors.New("sentinels not ready")
	}
	return nil
}

// ResetSentinel sends a sentinel reset * for the given sentinel.
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
		return fmt.Errorf("SENTINEL RESET: %w", err)
	}
	c.metricsRecorder.RecordRedisOperation(metrics.KIND_SENTINEL, ip, metrics.RESET_SENTINEL, metrics.SUCCESS, metrics.NOT_APPLICABLE)
	return nil
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
	// MONITOR runs even when REMOVE fails, because Sentinel must monitor the
	// Redis instances.
	cmd = rediscli.NewBoolCmd(context.TODO(), "SENTINEL", "MONITOR", masterName, monitor, port, quorum)
	err := rClient.Process(context.TODO(), cmd)
	if err != nil {
		c.metricsRecorder.RecordRedisOperation(metrics.KIND_SENTINEL, ip, metrics.MONITOR_REDIS_WITH_PORT, metrics.FAIL, getRedisError(err))
		return fmt.Errorf("SENTINEL MONITOR: %w", err)
	}

	if password != "" {
		cmd = rediscli.NewBoolCmd(context.TODO(), "SENTINEL", "SET", masterName, "auth-pass", password)
		err := rClient.Process(context.TODO(), cmd)
		if err != nil {
			c.metricsRecorder.RecordRedisOperation(metrics.KIND_SENTINEL, ip, metrics.MONITOR_REDIS_WITH_PORT, metrics.FAIL, getRedisError(err))
			return fmt.Errorf("SENTINEL SET auth-pass: %w", err)
		}
	}
	c.metricsRecorder.RecordRedisOperation(metrics.KIND_SENTINEL, ip, metrics.MONITOR_REDIS_WITH_PORT, metrics.SUCCESS, metrics.NOT_APPLICABLE)
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
		return "", "", fmt.Errorf("SENTINEL MASTER: %w", err)
	}
	// Process returns the error of cmd, so Val needs no error check.
	res := cmd.Val()
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
		replicas, err = sentinelReplicaIPs("SENTINEL REPLICAS", cmd.Val())
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

// GetSentinelMasterIP returns the master IP that the Sentinel gives to the
// clients. In a failover, the leader gives the promoted replica before the end
// of the failover, and the other Sentinels give the old master.
func (c *client) GetSentinelMasterIP(ip string) (string, error) {
	if ip == "" {
		return "", errNoIP
	}
	rClient := rediscli.NewClient(redisOptions(net.JoinHostPort(ip, sentinelPort), ""))
	defer closeClient(rClient)
	addr, err := rClient.Do(context.TODO(), "SENTINEL", "get-master-addr-by-name", masterName).StringSlice()
	if err == nil && len(addr) != 2 {
		err = errMalformedMasterAddr
	}
	if err != nil {
		c.metricsRecorder.RecordRedisOperation(metrics.KIND_SENTINEL, ip, metrics.GET_SENTINEL_MASTER_ADDR, metrics.FAIL, getRedisError(err))
		return "", fmt.Errorf("SENTINEL GET-MASTER-ADDR-BY-NAME: %w", err)
	}
	c.metricsRecorder.RecordRedisOperation(metrics.KIND_SENTINEL, ip, metrics.GET_SENTINEL_MASTER_ADDR, metrics.SUCCESS, metrics.NOT_APPLICABLE)
	return addr[0], nil
}

// SentinelFailover is the failover state of a Sentinel in SENTINEL MASTER.
type SentinelFailover struct {
	// InProgress is the failover_in_progress flag. A Sentinel has it while it
	// tries to fail over: as a candidate in the election, and as the leader.
	InProgress bool
	// State is the failover-state field, for example wait_promotion. It is
	// empty without a failover.
	State string
	// Timeout is the failover-timeout of the Sentinel, or 0 for a missing value.
	Timeout time.Duration
	// DownAfter is the down-after-milliseconds of the Sentinel, or 0 for a
	// missing value.
	DownAfter time.Duration
	// Master is the address of the master as ip:port. It stays the old master
	// until the failover ends.
	Master string
}

// IsNoSuchMasterError reports whether err is the reply of a Sentinel that
// does not monitor mymaster, for example after SENTINEL REMOVE.
func IsNoSuchMasterError(err error) bool {
	return err != nil && replyText(err) == "ERR No such master with that name"
}

// GetSentinelFailover returns the failover state of the Sentinel.
func (c *client) GetSentinelFailover(ip string) (SentinelFailover, error) {
	if ip == "" {
		return SentinelFailover{}, errNoIP
	}
	rClient := rediscli.NewClient(redisOptions(net.JoinHostPort(ip, sentinelPort), ""))
	defer closeClient(rClient)
	info, err := c.getSentinelMasterInfo(rClient)
	if err == nil {
		// Without flags, the reply cannot show a failover. Without the
		// address, the caller cannot tell two failovers apart. A truncated
		// reply can lose these fields.
		// Atoi gives 0 for a value that is not a number.
		port, _ := strconv.Atoi(info["port"])
		if info["flags"] == "" || info["ip"] == "" || port < 1 || port > 65535 {
			err = errMalformedSentinelMaster
		}
	}
	if err != nil {
		c.metricsRecorder.RecordRedisOperation(metrics.KIND_SENTINEL, ip, metrics.GET_SENTINEL_FAILOVER, metrics.FAIL, getRedisError(err))
		return SentinelFailover{}, err
	}
	c.metricsRecorder.RecordRedisOperation(metrics.KIND_SENTINEL, ip, metrics.GET_SENTINEL_FAILOVER, metrics.SUCCESS, metrics.NOT_APPLICABLE)
	// A missing or bad value gives 0, and the caller uses its default.
	ms, _ := strconv.ParseInt(info["failover-timeout"], 10, 64)
	downAfter, _ := strconv.ParseInt(info["down-after-milliseconds"], 10, 64)
	return SentinelFailover{
		InProgress: slices.Contains(strings.Split(info["flags"], ","), "failover_in_progress"),
		State:      info["failover-state"],
		Timeout:    time.Duration(max(ms, 0)) * time.Millisecond,
		DownAfter:  time.Duration(max(downAfter, 0)) * time.Millisecond,
		Master:     net.JoinHostPort(info["ip"], info["port"]),
	}, nil
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

// sentinelReplicaIPs reads the "ip" field of each entry in the reply of
// command, SENTINEL REPLICAS or SENTINEL SENTINELS. Each entry is a flat array
// of field names and values.
func sentinelReplicaIPs(command string, res []any) ([]string, error) {
	ips := make([]string, 0, len(res))
	for _, entry := range res {
		fields, ok := entry.([]any)
		if !ok {
			return nil, fmt.Errorf("unexpected %s entry %v", command, entry)
		}
		for i := 0; i+1 < len(fields); i += 2 {
			if fields[i] == "ip" {
				ip, ok := fields[i+1].(string)
				if !ok {
					return nil, fmt.Errorf("unexpected %s ip %v", command, fields[i+1])
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
	if IsUnreachableError(err) {
		// SENTINEL SET to the same Sentinel would fail too, for a lost pod after
		// one more timeout.
		c.metricsRecorder.RecordRedisOperation(metrics.KIND_SENTINEL, ip, metrics.APPLY_SENTINEL_CONFIG, metrics.FAIL, getRedisError(err))
		return err
	}
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
		return nil, fmt.Errorf("SENTINEL MASTER: %w", err)
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
		return fmt.Errorf("SENTINEL CKQUORUM: %w", err)
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

func (c *client) applySentinelConfig(parameter, value string, rClient *rediscli.Client) error {
	cmd := rediscli.NewStatusCmd(context.TODO(), "SENTINEL", "set", masterName, parameter, value)
	err := rClient.Process(context.TODO(), cmd)
	if err != nil {
		c.metricsRecorder.RecordRedisOperation(metrics.KIND_SENTINEL, addrHost(rClient.Options().Addr), metrics.APPLY_SENTINEL_CONFIG, metrics.FAIL, getRedisError(err))
		return fmt.Errorf("SENTINEL SET %s: %w", parameter, err)
	}
	c.metricsRecorder.RecordRedisOperation(metrics.KIND_SENTINEL, addrHost(rClient.Options().Addr), metrics.APPLY_SENTINEL_CONFIG, metrics.SUCCESS, metrics.NOT_APPLICABLE)
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
		return fmt.Errorf("SENTINEL SET auth-pass: %w", err)
	}
	c.metricsRecorder.RecordRedisOperation(metrics.KIND_SENTINEL, ip, metrics.SET_PASSWORD, metrics.SUCCESS, metrics.NOT_APPLICABLE)
	return nil
}
