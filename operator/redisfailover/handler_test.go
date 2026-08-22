package redisfailover_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	mRFService "github.com/saremox/redis-operator/mocks/operator/redisfailover/service"
	mK8SService "github.com/saremox/redis-operator/mocks/service/k8s"
	rfOperator "github.com/saremox/redis-operator/operator/redisfailover"
)

// skipReconcileAnnotationKey mirrors the unexported constant of the same name
// defined in operator/redisfailover/handler.go.
const skipReconcileAnnotationKey = "redisfailovers.databases.spotahome.com/skip-reconcile"

// TestHandleSkipReconcileAnnotation verifies that Handle short-circuits (returns
// nil without calling Ensure/CheckAndHeal) when the skip-reconcile annotation is
// set to "true", and otherwise proceeds with normal reconciliation.
func TestHandleSkipReconcileAnnotation(t *testing.T) {
	tests := []struct {
		name          string
		hasAnnotation bool
		annotationVal string
		expectSkip    bool
	}{
		{
			name:          "annotation absent reconciles normally",
			hasAnnotation: false,
			expectSkip:    false,
		},
		{
			name:          "annotation true skips reconciliation",
			hasAnnotation: true,
			annotationVal: "true",
			expectSkip:    true,
		},
		{
			name:          "annotation false reconciles normally",
			hasAnnotation: true,
			annotationVal: "false",
			expectSkip:    false,
		},
		{
			name:          "annotation empty reconciles normally",
			hasAnnotation: true,
			annotationVal: "",
			expectSkip:    false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)

			// Bootstrapping, no exporter: the minimal path through Ensure and
			// CheckAndHeal, matching the "Only ensure Redis when bootstrapping"
			// case already exercised in ensurer_test.go.
			rf := generateRF(false, true)
			if test.hasAnnotation {
				rf.Annotations = map[string]string{
					skipReconcileAnnotationKey: test.annotationVal,
				}
			}

			config := generateConfig()
			mk := &mK8SService.Services{}
			mrfc := &mRFService.RedisFailoverCheck{}
			mrfh := &mRFService.RedisFailoverHeal{}
			mrfs := &mRFService.RedisFailoverClient{}

			if !test.expectSkip {
				// Minimal Ensure() expectations for bootstrapping without exporter
				// or sentinels (see TestEnsure in ensurer_test.go).
				mrfs.On("EnsureNotPresentRedisService", rf).Once().Return(nil)
				mrfs.On("EnsureNotPresentSentinelResources", rf).Once().Return(nil)
				mrfs.On("EnsureRedisMasterService", rf, mock.Anything, mock.Anything).Once().Return(nil)
				mrfs.On("EnsureRedisSlaveService", rf, mock.Anything, mock.Anything).Once().Return(nil)
				mrfs.On("EnsureRedisConfigMap", rf, mock.Anything, mock.Anything).Once().Return(nil)
				mrfs.On("EnsureRedisShutdownConfigMap", rf, mock.Anything, mock.Anything).Once().Return(nil)
				mrfs.On("EnsureRedisReadinessConfigMap", rf, mock.Anything, mock.Anything).Once().Return(nil)
				mrfs.On("EnsureRedisStatefulset", rf, mock.Anything, mock.Anything).Once().Return(nil)

				// Minimal CheckAndHeal() expectation: bootstrap mode bails out
				// early once IsRedisRunning reports false.
				mrfc.On("IsRedisRunning", rf).Once().Return(false)
			}

			handler := rfOperator.NewRedisFailoverHandler(config, mrfs, mrfc, mrfh, mk, metrics.Dummy, log.Dummy)
			err := handler.Handle(context.Background(), rf)

			assert.NoError(err)

			if test.expectSkip {
				// None of Ensure/CheckAndHeal's underlying calls should have
				// happened.
				mrfs.AssertNotCalled(t, "EnsureNotPresentRedisService", mock.Anything)
				mrfs.AssertNotCalled(t, "EnsureRedisStatefulset", mock.Anything, mock.Anything, mock.Anything)
				mrfc.AssertNotCalled(t, "IsRedisRunning", mock.Anything)
				mrfh.AssertNotCalled(t, "SetOldestAsMaster", mock.Anything)
			} else {
				mrfs.AssertExpectations(t)
				mrfc.AssertExpectations(t)
			}
		})
	}
}

// TestHandleNotARedisFailover ensures Handle still rejects objects that are not
// a *RedisFailover, independent of the skip-reconcile short-circuit added above.
func TestHandleNotARedisFailover(t *testing.T) {
	assert := assert.New(t)

	config := generateConfig()
	mk := &mK8SService.Services{}
	mrfc := &mRFService.RedisFailoverCheck{}
	mrfh := &mRFService.RedisFailoverHeal{}
	mrfs := &mRFService.RedisFailoverClient{}

	handler := rfOperator.NewRedisFailoverHandler(config, mrfs, mrfc, mrfh, mk, metrics.Dummy, log.Dummy)
	err := handler.Handle(context.Background(), nil)

	assert.Error(err)
}
