package redis

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"

	rediscli "github.com/go-redis/redis/v8"
	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
)

// MemoryInfo contains the memory figures of a Redis instance relevant to maxmemory.
type MemoryInfo struct {
	MaxMemory       int64
	MaxMemoryPolicy string
	UsedMemory      int64 // used_memory minus mem_not_counted_for_evict, as compared against maxmemory
	UsedMemoryRSS   int64 // memory that the process holds, which often keeps the memory that evictions free
	Role            string
	Loading         bool // used_memory does not show the whole dataset yet
}

// AutoscalerInfo holds the INFO fields that the memory autoscaler reads.
type AutoscalerInfo struct {
	UsedMemory      int64 // used_memory minus mem_not_counted_for_evict, as compared against maxmemory
	MaxMemory       int64
	MaxMemoryPolicy string
	EvictedKeys     int64
	UptimeSeconds   int64
	Loading         bool
	SyncInProgress  bool
	RunID           string
	Role            string
	// OOMErrors is the count of errorstat_OOM: the commands that Redis
	// refused because of maxmemory.
	OOMErrors int64
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
			return nil, fmt.Errorf("INFO %s: %w", section, err)
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

// GetAutoscalerInfo reads the default INFO sections with one call. It uses
// quickOptions, because the caller skips the sample on an error.
func (c *client) GetAutoscalerInfo(ip, port, password string) (*AutoscalerInfo, error) {
	if ip == "" {
		return nil, errNoIP
	}
	rClient := rediscli.NewClient(quickOptions(net.JoinHostPort(ip, port), password))
	defer closeClient(rClient)
	info, err := rClient.Info(context.TODO()).Result()
	if err != nil {
		c.metricsRecorder.RecordRedisOperation(metrics.KIND_REDIS, ip, metrics.GET_AUTOSCALER_INFO, metrics.FAIL, getRedisError(err))
		return nil, fmt.Errorf("INFO: %w", err)
	}
	c.metricsRecorder.RecordRedisOperation(metrics.KIND_REDIS, ip, metrics.GET_AUTOSCALER_INFO, metrics.SUCCESS, metrics.NOT_APPLICABLE)
	return parseAutoscalerInfo(info), nil
}

func parseAutoscalerInfo(info string) *AutoscalerInfo {
	ai := &AutoscalerInfo{}
	var notCounted int64
	for _, line := range strings.Split(info, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		n, _ := strconv.ParseInt(value, 10, 64)
		switch key {
		case "used_memory":
			ai.UsedMemory = n
		case "mem_not_counted_for_evict":
			notCounted = n
		case "maxmemory":
			ai.MaxMemory = n
		case "maxmemory_policy":
			ai.MaxMemoryPolicy = value
		case "evicted_keys":
			ai.EvictedKeys = n
		case "uptime_in_seconds":
			ai.UptimeSeconds = n
		case "loading":
			ai.Loading = n == 1
		case "master_sync_in_progress":
			ai.SyncInProgress = n == 1
		case "run_id":
			ai.RunID = value
		case "role":
			ai.Role = value
		case "errorstat_OOM":
			// The value is "count=<n>".
			ai.OOMErrors, _ = strconv.ParseInt(strings.TrimPrefix(value, "count="), 10, 64)
		}
	}
	ai.UsedMemory -= notCounted
	return ai
}
