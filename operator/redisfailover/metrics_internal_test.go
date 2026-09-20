package redisfailover

// Internal test (package redisfailover) because setRedisCheckerMetrics is
// unexported; the main checker_test.go is package redisfailover_test.

import (
	"errors"
	"testing"

	"github.com/saremox/redis-operator/metrics"
	mMetrics "github.com/saremox/redis-operator/mocks/metrics"
)

// TestSetRedisCheckerMetricsRoutesByModeAndStatus verifies setRedisCheckerMetrics
// (operator/redisfailover/checker.go) records against the right series -
// RecordRedisCheck for "redis", RecordSentinelCheck for "sentinel" - with
// STATUS_HEALTHY/STATUS_UNHEALTHY picked from whether err is nil, and does
// nothing for any other mode. Every operator test elsewhere uses
// metrics.Dummy, so this is the only place metric emission is actually
// asserted rather than just exercised as a no-op.
func TestSetRedisCheckerMetricsRoutesByModeAndStatus(t *testing.T) {
	const (
		namespace = "testns"
		name      = "test"
		property  = "some-property"
		ip        = "10.0.0.1"
	)

	tests := []struct {
		name       string
		mode       string
		err        error
		wantMethod string
		wantStatus string
	}{
		{
			name:       "redis healthy",
			mode:       "redis",
			err:        nil,
			wantMethod: "RecordRedisCheck",
			wantStatus: metrics.STATUS_HEALTHY,
		},
		{
			name:       "redis unhealthy",
			mode:       "redis",
			err:        errors.New("boom"),
			wantMethod: "RecordRedisCheck",
			wantStatus: metrics.STATUS_UNHEALTHY,
		},
		{
			name:       "sentinel healthy",
			mode:       "sentinel",
			err:        nil,
			wantMethod: "RecordSentinelCheck",
			wantStatus: metrics.STATUS_HEALTHY,
		},
		{
			name:       "sentinel unhealthy",
			mode:       "sentinel",
			err:        errors.New("boom"),
			wantMethod: "RecordSentinelCheck",
			wantStatus: metrics.STATUS_UNHEALTHY,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mrec := &mMetrics.Recorder{}
			mrec.On(test.wantMethod, namespace, name, property, ip, test.wantStatus).Once()

			setRedisCheckerMetrics(mrec, test.mode, namespace, name, property, ip, test.err)

			mrec.AssertExpectations(t)
		})
	}

	t.Run("unknown mode records nothing", func(t *testing.T) {
		// No .On(...) stubs configured at all: since the mock is strict, any
		// call setRedisCheckerMetrics made here would panic the test.
		mrec := &mMetrics.Recorder{}

		setRedisCheckerMetrics(mrec, "unknown", namespace, name, property, ip, nil)

		mrec.AssertExpectations(t)
	})
}
