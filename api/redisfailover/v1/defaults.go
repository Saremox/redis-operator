package v1

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	defaultRedisNumber           = 3
	defaultSentinelNumber        = 3
	defaultSentinelExporterImage = "quay.io/oliver006/redis_exporter:v1.80.0-alpine"
	defaultExporterImage         = "quay.io/oliver006/redis_exporter:v1.80.0-alpine"
	defaultImage                 = "redis:7.2.12-alpine"
	defaultRedisPort             = 6379
	HealthyState                 = "Healthy"
	NotHealthyState              = "NotHealthy"

	// DefaultInstanceManagerImage is the default image used for the instance manager.
	// This image contains the redis-instance binary that runs as PID 1.
	// Users can override this per-RedisFailover via spec.redis.instanceManagerImage.
	DefaultInstanceManagerImage = "ghcr.io/buildio/redis-operator:4.0.0"
)

var (
	// DefaultSentinelEnabled is the default value for sentinel.enabled
	// Starting with 4.0.0, sentinel is DISABLED by default (operator-managed failover)
	// Set sentinel.enabled: true to use Redis Sentinel for failover
	DefaultSentinelEnabled = false
	// DefaultFailoverTimeout is the default timeout for operator-managed failover
	DefaultFailoverTimeout = metav1.Duration{Duration: 10 * time.Second}
)

var (
	defaultSentinelCustomConfig = []string{
		"down-after-milliseconds 5000",
		"failover-timeout 10000",
	}
	defaultRedisCustomConfig = []string{
		"replica-priority 100",
	}
	bootstrappingRedisCustomConfig = []string{
		"replica-priority 0",
	}
)
