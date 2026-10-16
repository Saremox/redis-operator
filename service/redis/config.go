package redis

import (
	"context"
	"fmt"
	"net"
	"strings"

	rediscli "github.com/go-redis/redis/v8"
	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
)

func (c *client) SetCustomRedisConfig(ip, port string, configs []string, password string) error {
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
		return fmt.Errorf("ACL LOAD: %w", err)
	}
	if _, err := cmd.Result(); err != nil {
		c.metricsRecorder.RecordRedisOperation(metrics.KIND_REDIS, addrHost(rClient.Options().Addr), metrics.APPLY_REDIS_CONFIG, metrics.FAIL, getRedisError(err))
		return fmt.Errorf("ACL LOAD: %w", err)
	}
	c.metricsRecorder.RecordRedisOperation(metrics.KIND_REDIS, addrHost(rClient.Options().Addr), metrics.APPLY_REDIS_CONFIG, metrics.SUCCESS, metrics.NOT_APPLICABLE)
	return nil
}

func (c *client) applyRedisConfig(parameter, value string, rClient *rediscli.Client) error {
	result := rClient.ConfigSet(context.TODO(), parameter, value)
	if nil != result.Err() {
		c.metricsRecorder.RecordRedisOperation(metrics.KIND_REDIS, addrHost(rClient.Options().Addr), metrics.APPLY_REDIS_CONFIG, metrics.FAIL, getRedisError(result.Err()))
		return fmt.Errorf("CONFIG SET %s: %w", parameter, result.Err())
	}
	c.metricsRecorder.RecordRedisOperation(metrics.KIND_REDIS, addrHost(rClient.Options().Addr), metrics.APPLY_REDIS_CONFIG, metrics.SUCCESS, metrics.NOT_APPLICABLE)
	return nil
}

func (c *client) getConfigParameters(config string) (parameter, value string, err error) {
	s := strings.Split(config, " ")
	if len(s) < 2 {
		return "", "", fmt.Errorf("configuration '%s' malformed", config)
	}
	if len(s) == 2 && s[1] == `""` {
		return s[0], "", nil
	}
	return s[0], strings.Join(s[1:], " "), nil
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
			return fmt.Errorf("CONFIG SET %s: %w", param, err)
		}
	}
	c.metricsRecorder.RecordRedisOperation(metrics.KIND_REDIS, ip, metrics.SET_PASSWORD, metrics.SUCCESS, metrics.NOT_APPLICABLE)
	return nil
}
