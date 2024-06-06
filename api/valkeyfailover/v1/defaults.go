package v1

const (
	defaultValkeyNumber          = 3
	defaultSentinelNumber        = 3
	defaultSentinelExporterImage = "quay.io/oliver006/redis_exporter:v1.43.0"
	defaultExporterImage         = "quay.io/oliver006/redis_exporter:v1.43.0"
	defaultImage                 = "valkey/valkey:7.2.5-alpine"
	defaultValkeyPort            = 6379
	HealthyState                 = "Healthy"
	NotHealthyState              = "NotHealthy"
)

var (
	defaultSentinelCustomConfig = []string{
		"down-after-milliseconds 5000",
		"failover-timeout 10000",
	}
	defaultValkeyCustomConfig = []string{
		"replica-priority 100",
	}
	bootstrappingValkeyCustomConfig = []string{
		"replica-priority 0",
	}
)
