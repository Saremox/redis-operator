package service

// variables refering to the redis exporter port
const (
	exporterPort                  = 9121
	sentinelExporterPort          = 9355
	exporterPortName              = "http-metrics"
	exporterContainerName         = "redis-exporter"
	sentinelExporterContainerName = "sentinel-exporter"
	exporterDefaultRequestCPU     = "10m"
	exporterDefaultLimitCPU       = "1000m"
	exporterDefaultRequestMemory  = "50Mi"
	exporterDefaultLimitMemory    = "100Mi"
)

const (
	baseName                   = "rf"
	sentinelName               = "s"
	sentinelRoleName           = "sentinel"
	sentinelConfigFileName     = "sentinel.conf"
	redisConfigFileName        = "redis.conf"
	redisName                  = "r"
	redisMasterName            = "rm"
	redisSlaveName             = "rs"
	redisShutdownName          = "r-s"
	redisReadinessName         = "r-readiness"
	redisRoleName              = "redis"
	redisContainerName         = "redis"
	sentinelServiceAccountName = "s-sa"
	appLabel                   = "redis-failover"
	hostnameTopologyKey        = "kubernetes.io/hostname"
)

const (
	redisRoleLabelKey    = "redisfailovers-role"
	redisRoleLabelMaster = "master"
	redisRoleLabelSlave  = "slave"
)

// redisAuthSecretChecksumAnnotation holds an HMAC of the auth password on the
// Redis pod template, because a pod reads REDIS_PASSWORD only at start. A new
// value makes UpdateRedisesPods restart each pod onto the password that
// ApplyPassword already set in place.
const redisAuthSecretChecksumAnnotation = "redisfailovers.databases.spotahome.com/secret-checksum"

// resizeRequestedAnnotation holds when the operator last requested an
// in-place resize of the pod, and is cleared once the resize is applied.
const resizeRequestedAnnotation = "redisfailovers.databases.spotahome.com/resize-requested-at"

// masterSafeToEvictAnnotation is the cluster-autoscaler annotation used to keep
// the node running the redis master from being drained during scale-down.
const masterSafeToEvictAnnotation = "cluster-autoscaler.kubernetes.io/safe-to-evict"
