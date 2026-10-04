package v1

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Bootstrapping reports whether the Redis pods replicate from an external bootstrap node.
func (r *RedisFailover) Bootstrapping() bool {
	return r.Spec.BootstrapNode != nil
}

// SentinelEnabled reports whether Sentinel does the failover. When
// sentinel.enabled is not set, the operator does the failover.
func (r *RedisFailover) SentinelEnabled() bool {
	if r.Spec.Sentinel.Enabled == nil {
		return DefaultSentinelEnabled
	}
	return *r.Spec.Sentinel.Enabled
}

// SentinelsAllowed reports whether the operator deploys the Sentinels. In
// bootstrap mode, they monitor the external node, so they also need
// bootstrapNode.allowSentinels.
func (r *RedisFailover) SentinelsAllowed() bool {
	if !r.SentinelEnabled() {
		return false
	}
	bootstrapping := r.Bootstrapping()
	return !bootstrapping || (bootstrapping && r.Spec.BootstrapNode.AllowSentinels)
}

// OperatorManagedFailover reports whether the operator does the failover in
// place of Sentinel. This is the case when sentinel.enabled is false or not set.
func (r *RedisFailover) OperatorManagedFailover() bool {
	return !r.SentinelEnabled()
}

// GetFailoverTimeout returns DefaultFailoverTimeout (10s) when
// sentinel.failoverTimeout is not set.
func (r *RedisFailover) GetFailoverTimeout() metav1.Duration {
	if r.Spec.Sentinel.FailoverTimeout == nil {
		return DefaultFailoverTimeout
	}
	return *r.Spec.Sentinel.FailoverTimeout
}

func (r *RedisFailover) GetFailoverTimeoutDuration() time.Duration {
	return r.GetFailoverTimeout().Duration
}
