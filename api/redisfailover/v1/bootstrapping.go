package v1

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Bootstrapping returns true when a BootstrapNode is provided to the RedisFailover spec. Otherwise, it returns false.
func (r *RedisFailover) Bootstrapping() bool {
	return r.Spec.BootstrapNode != nil
}

// SentinelEnabled returns true if Sentinel is enabled (the default behavior).
// Returns false when sentinel.enabled is explicitly set to false.
func (r *RedisFailover) SentinelEnabled() bool {
	if r.Spec.Sentinel.Enabled == nil {
		return DefaultSentinelEnabled
	}
	return *r.Spec.Sentinel.Enabled
}

// SentinelsAllowed returns true if sentinels should be deployed.
// Sentinels are allowed when:
// - sentinel.enabled is true (or not specified, defaults to true), AND
// - either not bootstrapping, or bootstrapping with AllowSentinels=true
func (r *RedisFailover) SentinelsAllowed() bool {
	if !r.SentinelEnabled() {
		return false
	}
	bootstrapping := r.Bootstrapping()
	return !bootstrapping || (bootstrapping && r.Spec.BootstrapNode.AllowSentinels)
}

// OperatorManagedFailover returns true when the operator should handle failover
// instead of Sentinel. This is the case when sentinel.enabled is explicitly false.
func (r *RedisFailover) OperatorManagedFailover() bool {
	return !r.SentinelEnabled()
}

// GetFailoverTimeout returns the failover timeout duration.
// Returns the configured value or the default (10s) if not specified.
func (r *RedisFailover) GetFailoverTimeout() metav1.Duration {
	if r.Spec.Sentinel.FailoverTimeout == nil {
		return DefaultFailoverTimeout
	}
	return *r.Spec.Sentinel.FailoverTimeout
}

// GetFailoverTimeoutDuration returns the failover timeout as a time.Duration
func (r *RedisFailover) GetFailoverTimeoutDuration() time.Duration {
	return r.GetFailoverTimeout().Duration
}
