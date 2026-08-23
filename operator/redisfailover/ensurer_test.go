package redisfailover_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	mRFService "github.com/saremox/redis-operator/mocks/operator/redisfailover/service"
	mK8SService "github.com/saremox/redis-operator/mocks/service/k8s"
	rfOperator "github.com/saremox/redis-operator/operator/redisfailover"
)

const (
	name      = "test"
	namespace = "testns"
)

func generateConfig() rfOperator.Config {
	return rfOperator.Config{
		ListenAddress: "1234",
		MetricsPath:   "/awesome",
	}
}

func generateRF(enableExporter bool, bootstrapping bool) *redisfailoverv1.RedisFailover {
	// Explicitly enable sentinel for tests that expect sentinel behavior
	// (sentinel is disabled by default in v4.0.0+)
	sentinelEnabled := true
	return &redisfailoverv1.RedisFailover{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: redisfailoverv1.RedisFailoverSpec{
			Redis: redisfailoverv1.RedisSettings{
				Replicas: int32(3),
				Exporter: redisfailoverv1.Exporter{
					Enabled: enableExporter,
				},
			},
			Sentinel: redisfailoverv1.SentinelSettings{
				Enabled:  &sentinelEnabled,
				Replicas: int32(3),
			},
			BootstrapNode: generateRFBootstrappingNode(bootstrapping),
		},
	}
}

func generateRFBootstrappingNode(bootstrapping bool) *redisfailoverv1.BootstrapSettings {
	if bootstrapping {
		return &redisfailoverv1.BootstrapSettings{
			Host: "127.0.0.1",
			Port: "6379",
		}
	}
	return nil
}

func TestEnsure(t *testing.T) {
	tests := []struct {
		name                        string
		exporter                    bool
		bootstrapping               bool
		bootstrappingAllowSentinels bool
	}{
		{
			name:                        "Call everything, use exporter",
			exporter:                    true,
			bootstrapping:               false,
			bootstrappingAllowSentinels: false,
		},
		{
			name:                        "Call everything, don't use exporter",
			exporter:                    false,
			bootstrapping:               false,
			bootstrappingAllowSentinels: false,
		},
		{
			name:                        "Only ensure Redis when bootstrapping",
			exporter:                    false,
			bootstrapping:               true,
			bootstrappingAllowSentinels: false,
		},
		{
			name:                        "call everything when bootstrapping allows sentinels",
			exporter:                    false,
			bootstrapping:               true,
			bootstrappingAllowSentinels: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)

			rf := generateRF(test.exporter, test.bootstrapping)
			if test.bootstrapping {
				rf.Spec.BootstrapNode.AllowSentinels = test.bootstrappingAllowSentinels
			}

			config := generateConfig()
			mk := &mK8SService.Services{}
			mrfc := &mRFService.RedisFailoverCheck{}
			mrfh := &mRFService.RedisFailoverHeal{}
			mrfs := &mRFService.RedisFailoverClient{}
			if test.exporter {
				mrfs.On("EnsureRedisService", rf, mock.Anything, mock.Anything).Once().Return(nil)
			} else {
				mrfs.On("EnsureNotPresentRedisService", rf).Once().Return(nil)
			}

			if !test.bootstrapping || test.bootstrappingAllowSentinels {
				mrfs.On("EnsureSentinelService", rf, mock.Anything, mock.Anything).Once().Return(nil)
				mrfs.On("EnsureSentinelConfigMap", rf, mock.Anything, mock.Anything).Once().Return(nil)
				mrfs.On("EnsureSentinelDeployment", rf, mock.Anything, mock.Anything).Once().Return(nil)
			} else {
				// When bootstrapping without allowing sentinels, cleanup is called
				mrfs.On("EnsureNotPresentSentinelResources", rf).Once().Return(nil)
			}

			mrfs.On("EnsureRedisMasterService", rf, mock.Anything, mock.Anything).Once().Return(nil)
			mrfs.On("EnsureRedisSlaveService", rf, mock.Anything, mock.Anything).Once().Return(nil)
			mrfs.On("EnsureRedisConfigMap", rf, mock.Anything, mock.Anything).Once().Return(nil)
			mrfs.On("EnsureRedisShutdownConfigMap", rf, mock.Anything, mock.Anything).Once().Return(nil)
			mrfs.On("EnsureRedisReadinessConfigMap", rf, mock.Anything, mock.Anything).Once().Return(nil)
			mrfs.On("EnsureRedisStatefulset", rf, mock.Anything, mock.Anything).Once().Return(nil)

			// Create the Kops client and call the valid logic.
			handler := rfOperator.NewRedisFailoverHandler(config, mrfs, mrfc, mrfh, mk, metrics.Dummy, log.Dummy)
			err := handler.Ensure(rf, map[string]string{}, []metav1.OwnerReference{}, metrics.Dummy)

			assert.NoError(err)
			mrfs.AssertExpectations(t)
		})
	}
}

// ensureStepSetup returns a closure that registers the mock expectation for
// the named Ensure* / EnsureNotPresent* call performed by Ensure() in
// ensurer.go, returning the given error (nil for a success stub).
func ensureStepSetup(step string) func(m *mRFService.RedisFailoverClient, rf *redisfailoverv1.RedisFailover, err error) {
	// Calls taking only rf (the "NotPresent" cleanup calls).
	rfOnly := map[string]bool{
		"EnsureNotPresentRedisService":      true,
		"EnsureNotPresentSentinelResources": true,
	}
	if rfOnly[step] {
		return func(m *mRFService.RedisFailoverClient, rf *redisfailoverv1.RedisFailover, err error) {
			m.On(step, rf).Once().Return(err)
		}
	}
	return func(m *mRFService.RedisFailoverClient, rf *redisfailoverv1.RedisFailover, err error) {
		m.On(step, rf, mock.Anything, mock.Anything).Once().Return(err)
	}
}

// TestEnsureErrorBranches exercises every early-return error branch in
// Ensure() (operator/redisfailover/ensurer.go): each of the EnsureX /
// EnsureNotPresentX calls it makes can fail, and Ensure() must stop and
// propagate that error immediately without invoking any of the calls that
// would normally follow. Two RF configurations are used because several
// steps are mutually exclusive depending on exporter/sentinel settings:
//   - modeA (exporter enabled, sentinels allowed) walks the "everything is
//     enabled" branch of every either/or pair.
//   - modeB (exporter disabled, sentinels disabled) walks the "cleanup"
//     branch of the same either/or pairs.
func TestEnsureErrorBranches(t *testing.T) {
	modeA := []string{
		"EnsureRedisService",
		"EnsureSentinelService",
		"EnsureSentinelConfigMap",
		"EnsureRedisMasterService",
		"EnsureRedisSlaveService",
		"EnsureRedisShutdownConfigMap",
		"EnsureRedisReadinessConfigMap",
		"EnsureRedisConfigMap",
		"EnsureRedisStatefulset",
		"EnsureSentinelDeployment",
	}
	modeB := []string{
		"EnsureNotPresentRedisService",
		"EnsureNotPresentSentinelResources",
		"EnsureRedisMasterService",
		"EnsureRedisSlaveService",
		"EnsureRedisShutdownConfigMap",
		"EnsureRedisReadinessConfigMap",
		"EnsureRedisConfigMap",
		"EnsureRedisStatefulset",
	}

	tests := []struct {
		name             string
		exporter         bool
		sentinelsAllowed bool
		steps            []string
		failAt           int
	}{
		{"EnsureRedisService fails", true, true, modeA, 0},
		{"EnsureSentinelService fails", true, true, modeA, 1},
		{"EnsureSentinelConfigMap fails", true, true, modeA, 2},
		{"EnsureRedisMasterService fails", true, true, modeA, 3},
		{"EnsureRedisSlaveService fails", true, true, modeA, 4},
		{"EnsureRedisShutdownConfigMap fails", true, true, modeA, 5},
		{"EnsureRedisReadinessConfigMap fails", true, true, modeA, 6},
		{"EnsureRedisConfigMap fails", true, true, modeA, 7},
		{"EnsureRedisStatefulset fails", true, true, modeA, 8},
		{"EnsureSentinelDeployment fails", true, true, modeA, 9},
		{"EnsureNotPresentRedisService fails", false, false, modeB, 0},
		{"EnsureNotPresentSentinelResources fails", false, false, modeB, 1},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)

			rf := generateRF(test.exporter, false)
			if !test.sentinelsAllowed {
				disabled := false
				rf.Spec.Sentinel.Enabled = &disabled
			}

			config := generateConfig()
			mk := &mK8SService.Services{}
			mrfc := &mRFService.RedisFailoverCheck{}
			mrfh := &mRFService.RedisFailoverHeal{}
			mrfs := &mRFService.RedisFailoverClient{}

			wantErr := errors.New(test.steps[test.failAt] + " failed")
			for i, step := range test.steps {
				if i > test.failAt {
					// Steps after the failure must never be called: leave
					// them unmocked so an unexpected call fails the test.
					break
				}
				setup := ensureStepSetup(step)
				if i == test.failAt {
					setup(mrfs, rf, wantErr)
				} else {
					setup(mrfs, rf, nil)
				}
			}

			handler := rfOperator.NewRedisFailoverHandler(config, mrfs, mrfc, mrfh, mk, metrics.Dummy, log.Dummy)
			err := handler.Ensure(rf, map[string]string{}, []metav1.OwnerReference{}, metrics.Dummy)

			assert.Equal(wantErr, err)
			mrfs.AssertExpectations(t)
		})
	}
}
