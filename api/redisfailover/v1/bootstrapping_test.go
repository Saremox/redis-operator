package v1

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func generateRedisFailover(name string, bootstrapNode *BootstrapSettings) *RedisFailover {
	return &RedisFailover{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "namespace",
		},
		Spec: RedisFailoverSpec{
			BootstrapNode: bootstrapNode,
		},
	}
}

func generateRedisFailoverWithSentinel(name string, sentinelEnabled *bool) *RedisFailover {
	return &RedisFailover{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "namespace",
		},
		Spec: RedisFailoverSpec{
			Sentinel: SentinelSettings{
				Enabled: sentinelEnabled,
			},
		},
	}
}

func TestBootstrapping(t *testing.T) {
	tests := []struct {
		name              string
		expectation       bool
		bootstrapSettings *BootstrapSettings
	}{
		{
			name:        "without BootstrapSettings",
			expectation: false,
		},
		{
			name:        "with BootstrapSettings",
			expectation: true,
			bootstrapSettings: &BootstrapSettings{
				Host: "127.0.0.1",
				Port: "6379",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rf := generateRedisFailover("test", test.bootstrapSettings)
			assert.Equal(t, test.expectation, rf.Bootstrapping())
		})
	}
}

func TestSentinelsAllowed(t *testing.T) {
	tests := []struct {
		name              string
		expectation       bool
		bootstrapSettings *BootstrapSettings
	}{
		{
			name:        "without BootstrapSettings",
			expectation: true,
		},
		{
			name:        "with BootstrapSettings",
			expectation: false,
			bootstrapSettings: &BootstrapSettings{
				Host: "127.0.0.1",
				Port: "6379",
			},
		},
		{
			name:        "with BootstrapSettings that allows sentinels",
			expectation: true,
			bootstrapSettings: &BootstrapSettings{
				Host:           "127.0.0.1",
				Port:           "6379",
				AllowSentinels: true,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rf := generateRedisFailover("test", test.bootstrapSettings)
			assert.Equal(t, test.expectation, rf.SentinelsAllowed())
		})
	}
}

func TestSentinelEnabled(t *testing.T) {
	trueVal := true
	falseVal := false

	tests := []struct {
		name            string
		sentinelEnabled *bool
		expectation     bool
	}{
		{
			name:            "nil (default true)",
			sentinelEnabled: nil,
			expectation:     true,
		},
		{
			name:            "explicitly true",
			sentinelEnabled: &trueVal,
			expectation:     true,
		},
		{
			name:            "explicitly false",
			sentinelEnabled: &falseVal,
			expectation:     false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rf := generateRedisFailoverWithSentinel("test", test.sentinelEnabled)
			assert.Equal(t, test.expectation, rf.SentinelEnabled())
		})
	}
}

func TestOperatorManagedFailover(t *testing.T) {
	trueVal := true
	falseVal := false

	tests := []struct {
		name            string
		sentinelEnabled *bool
		expectation     bool
	}{
		{
			name:            "nil (default - Sentinel enabled, operator NOT managing)",
			sentinelEnabled: nil,
			expectation:     false,
		},
		{
			name:            "sentinel explicitly enabled - operator NOT managing",
			sentinelEnabled: &trueVal,
			expectation:     false,
		},
		{
			name:            "sentinel explicitly disabled - operator IS managing",
			sentinelEnabled: &falseVal,
			expectation:     true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rf := generateRedisFailoverWithSentinel("test", test.sentinelEnabled)
			assert.Equal(t, test.expectation, rf.OperatorManagedFailover())
		})
	}
}

func TestSentinelsAllowedWithSentinelEnabled(t *testing.T) {
	trueVal := true
	falseVal := false

	tests := []struct {
		name              string
		sentinelEnabled   *bool
		bootstrapSettings *BootstrapSettings
		expectation       bool
	}{
		{
			name:            "sentinel disabled - sentinels not allowed",
			sentinelEnabled: &falseVal,
			expectation:     false,
		},
		{
			name:            "sentinel enabled (default) - sentinels allowed",
			sentinelEnabled: nil,
			expectation:     true,
		},
		{
			name:            "sentinel enabled explicitly - sentinels allowed",
			sentinelEnabled: &trueVal,
			expectation:     true,
		},
		{
			name:            "sentinel enabled but bootstrapping without allow",
			sentinelEnabled: &trueVal,
			bootstrapSettings: &BootstrapSettings{
				Host: "127.0.0.1",
				Port: "6379",
			},
			expectation: false,
		},
		{
			name:            "sentinel disabled with bootstrapping",
			sentinelEnabled: &falseVal,
			bootstrapSettings: &BootstrapSettings{
				Host:           "127.0.0.1",
				Port:           "6379",
				AllowSentinels: true,
			},
			expectation: false, // sentinel.enabled=false takes precedence
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rf := &RedisFailover{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test",
					Namespace: "namespace",
				},
				Spec: RedisFailoverSpec{
					Sentinel: SentinelSettings{
						Enabled: test.sentinelEnabled,
					},
					BootstrapNode: test.bootstrapSettings,
				},
			}
			assert.Equal(t, test.expectation, rf.SentinelsAllowed())
		})
	}
}

func TestGetFailoverTimeout(t *testing.T) {
	customTimeout := metav1.Duration{Duration: 30 * time.Second}

	tests := []struct {
		name            string
		failoverTimeout *metav1.Duration
		expectation     time.Duration
	}{
		{
			name:            "nil (default 10s)",
			failoverTimeout: nil,
			expectation:     10 * time.Second,
		},
		{
			name:            "custom 30s",
			failoverTimeout: &customTimeout,
			expectation:     30 * time.Second,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rf := &RedisFailover{
				Spec: RedisFailoverSpec{
					Sentinel: SentinelSettings{
						FailoverTimeout: test.failoverTimeout,
					},
				},
			}
			assert.Equal(t, test.expectation, rf.GetFailoverTimeoutDuration())
		})
	}
}
