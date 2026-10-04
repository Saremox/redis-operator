package service

import (
	"fmt"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
)

// GetRedisShutdownConfigMapName returns the name of the ConfigMap with the
// shutdown script: redis.shutdownConfigMap, or the operator default.
func GetRedisShutdownConfigMapName(rf *redisfailoverv1.RedisFailover) string {
	if rf.Spec.Redis.ShutdownConfigMap != "" {
		return rf.Spec.Redis.ShutdownConfigMap
	}
	return GetRedisShutdownName(rf)
}

// GetRedisName returns the name of the Redis StatefulSet, the Redis
// ConfigMap and the exporter Service.
func GetRedisName(rf *redisfailoverv1.RedisFailover) string {
	return generateName(redisName, rf.Name)
}

// GetRedisShutdownName returns the name of the shutdown script ConfigMap
// that the operator creates.
func GetRedisShutdownName(rf *redisfailoverv1.RedisFailover) string {
	return generateName(redisShutdownName, rf.Name)
}

// GetRedisReadinessName returns the name of the readiness script ConfigMap.
func GetRedisReadinessName(rf *redisfailoverv1.RedisFailover) string {
	return generateName(redisReadinessName, rf.Name)
}

// GetSentinelName returns the name for sentinel resources
func GetSentinelName(rf *redisfailoverv1.RedisFailover) string {
	return generateName(sentinelName, rf.Name)
}

// GetSentinelServiceAccountName returns the name of the ServiceAccount that
// the operator creates for Sentinel when sentinel.serviceAccountName is empty.
func GetSentinelServiceAccountName(rf *redisfailoverv1.RedisFailover) string {
	return generateName(sentinelServiceAccountName, rf.Name)
}

func GetRedisMasterName(rf *redisfailoverv1.RedisFailover) string {
	return generateName(redisMasterName, rf.Name)
}

func GetRedisSlaveName(rf *redisfailoverv1.RedisFailover) string {
	return generateName(redisSlaveName, rf.Name)
}

func generateName(typeName, metaName string) string {
	return fmt.Sprintf("%s%s-%s", baseName, typeName, metaName)
}
