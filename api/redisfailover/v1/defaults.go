package v1

import (
	"fmt"
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
	defaultMaxMemoryPercent      = 75
	defaultMaxMemoryPolicy       = "noeviction"
	HealthyState                 = "Healthy"
	NotHealthyState              = "NotHealthy"
)

// The Sentinel timeouts that apply when sentinel.customConfig does not set them.
// sentinel.conf and the readiness script of the Redis pods use the same values.
const (
	DefaultSentinelDownAfterMilliseconds       = 5000
	DefaultSentinelFailoverTimeoutMilliseconds = 10000
)

var (
	// DefaultSentinelEnabled is the value of sentinel.enabled when it is not
	// set. Without Sentinel, the operator does the failover.
	DefaultSentinelEnabled = false
	// DefaultFailoverTimeout is the value of sentinel.failoverTimeout when it is not set.
	DefaultFailoverTimeout = metav1.Duration{Duration: 10 * time.Second}
)

var (
	defaultSentinelCustomConfig = []string{
		fmt.Sprintf("down-after-milliseconds %d", DefaultSentinelDownAfterMilliseconds),
		fmt.Sprintf("failover-timeout %d", DefaultSentinelFailoverTimeoutMilliseconds),
	}
	defaultRedisCustomConfig = []string{
		"replica-priority 100",
	}
	bootstrappingRedisCustomConfig = []string{
		"replica-priority 0",
	}
)
