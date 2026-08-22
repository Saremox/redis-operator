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
	sentinelServiceAccountName = "s-sa"
	appLabel                   = "redis-failover"
	hostnameTopologyKey        = "kubernetes.io/hostname"
)

const (
	redisRoleLabelKey    = "redisfailovers-role"
	redisRoleLabelMaster = "master"
	redisRoleLabelSlave  = "slave"
)

// redisAuthSecretChecksumAnnotation is set on the Redis StatefulSet's pod
// template to a checksum of the current auth password. Kubernetes doesn't
// restart running pods when a Secret's data changes in place (only the
// mounted file content is updated), so without this the operator would keep
// using the new password from the Secret against pods still running with the
// old one loaded in memory, and would never detect that those pods need to
// be replaced. Changing the annotation's value changes the StatefulSet's pod
// template hash, which the existing revision-based staleness check in
// UpdateRedisesPods already uses to roll pods one at a time.
const redisAuthSecretChecksumAnnotation = "redisfailovers.databases.spotahome.com/secret-checksum"
