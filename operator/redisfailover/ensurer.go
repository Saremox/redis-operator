package redisfailover

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/metrics"
)

// Ensure is called to ensure all of the resources associated with a RedisFailover are created
func (r *RedisFailoverHandler) Ensure(rf *redisfailoverv1.RedisFailover, labels map[string]string, or []metav1.OwnerReference, metricsClient metrics.Recorder) error {
	if rf.Spec.Redis.Exporter.Enabled {
		if err := r.rfService.EnsureRedisService(rf, labels, or); err != nil {
			return err
		}
	} else {
		if err := r.rfService.EnsureNotPresentRedisService(rf); err != nil {
			return err
		}
	}

	sentinelsAllowed := rf.SentinelsAllowed()
	if sentinelsAllowed {
		if err := r.rfService.EnsureSentinelService(rf, labels, or); err != nil {
			return err
		}
		if err := r.rfService.EnsureSentinelConfigMap(rf, labels, or); err != nil {
			return err
		}
	} else {
		// Clean up Sentinel resources when Sentinel is disabled
		if err := r.rfService.EnsureNotPresentSentinelResources(rf); err != nil {
			return err
		}
	}

	if err := r.rfService.EnsureRedisMasterService(rf, labels, or); err != nil {
		return err
	}

	if err := r.rfService.EnsureRedisSlaveService(rf, labels, or); err != nil {
		return err
	}

	if err := r.rfService.EnsureRedisShutdownConfigMap(rf, labels, or); err != nil {
		return err
	}
	if err := r.rfService.EnsureRedisReadinessConfigMap(rf, labels, or); err != nil {
		return err
	}
	if err := r.rfService.EnsureRedisConfigMap(rf, labels, or); err != nil {
		return err
	}
	if err := r.rfService.EnsureRedisStatefulset(rf, labels, or); err != nil {
		return err
	}

	if sentinelsAllowed {
		if err := r.rfService.EnsureSentinelDeployment(rf, labels, or); err != nil {
			return err
		}
	}

	return nil
}
