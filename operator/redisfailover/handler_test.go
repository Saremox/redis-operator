package redisfailover_test

import (
	"context"
	"errors"
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

// TestHandleValidateError verifies that Handle rejects a RedisFailover that
// fails Validate() (here, a name longer than the 48-char limit) before ever
// reaching Ensure or CheckAndHeal.
func TestHandleValidateError(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF(false, true)
	rf.Name = "this-name-is-far-too-long-to-pass-the-forty-eight-character-limit"

	config := generateConfig()
	mk := &mK8SService.Services{}
	mrfc := &mRFService.RedisFailoverCheck{}
	mrfh := &mRFService.RedisFailoverHeal{}
	mrfs := &mRFService.RedisFailoverClient{}

	handler := rfOperator.NewRedisFailoverHandler(config, mrfs, mrfc, mrfh, mk, metrics.Dummy, log.Dummy)
	err := handler.Handle(context.Background(), rf)

	assert.Error(err)
	mrfs.AssertNotCalled(t, "EnsureNotPresentRedisService", mock.Anything)
	mrfc.AssertNotCalled(t, "IsRedisRunning", mock.Anything)
}

// TestHandleEnsureError verifies that Handle propagates an error from Ensure
// (the first sub-call it makes) without ever calling CheckAndHeal.
func TestHandleEnsureError(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF(false, true) // bootstrapping, no exporter, no sentinels allowed
	ensureErr := errors.New("ensure boom")

	config := generateConfig()
	mk := &mK8SService.Services{}
	mrfc := &mRFService.RedisFailoverCheck{}
	mrfh := &mRFService.RedisFailoverHeal{}
	mrfs := &mRFService.RedisFailoverClient{}

	// Only the very first Ensure() call is mocked, and it fails - nothing
	// after it (in Ensure or CheckAndHeal) should ever be invoked.
	mrfs.On("EnsureNotPresentRedisService", rf).Once().Return(ensureErr)

	handler := rfOperator.NewRedisFailoverHandler(config, mrfs, mrfc, mrfh, mk, metrics.Dummy, log.Dummy)
	err := handler.Handle(context.Background(), rf)

	assert.Equal(ensureErr, err)
	mrfc.AssertNotCalled(t, "IsRedisRunning", mock.Anything)
	mrfs.AssertExpectations(t)
}

// TestHandleCheckAndHealError verifies that Handle propagates an error from
// CheckAndHeal once Ensure has completed successfully.
func TestHandleCheckAndHealError(t *testing.T) {
	assert := assert.New(t)

	// Operator-managed mode (sentinel disabled), not bootstrapping: the
	// minimal Ensure() call graph (see TestEnsure "don't use exporter" case,
	// with sentinels also disabled).
	rf := generateRF(false, false)
	disabled := false
	rf.Spec.Sentinel.Enabled = &disabled

	checkErr := errors.New("get number masters boom")

	config := generateConfig()
	mk := &mK8SService.Services{}
	mrfc := &mRFService.RedisFailoverCheck{}
	mrfh := &mRFService.RedisFailoverHeal{}
	mrfs := &mRFService.RedisFailoverClient{}

	mrfs.On("EnsureNotPresentRedisService", rf).Once().Return(nil)
	mrfs.On("EnsureNotPresentSentinelResources", rf).Once().Return(nil)
	mrfs.On("EnsureRedisMasterService", rf, mock.Anything, mock.Anything).Once().Return(nil)
	mrfs.On("EnsureRedisSlaveService", rf, mock.Anything, mock.Anything).Once().Return(nil)
	mrfs.On("EnsureRedisShutdownConfigMap", rf, mock.Anything, mock.Anything).Once().Return(nil)
	mrfs.On("EnsureRedisReadinessConfigMap", rf, mock.Anything, mock.Anything).Once().Return(nil)
	mrfs.On("EnsureRedisConfigMap", rf, mock.Anything, mock.Anything).Once().Return(nil)
	mrfs.On("EnsureRedisStatefulset", rf, mock.Anything, mock.Anything).Once().Return(nil)

	// CheckAndHeal routes to checkAndHealOperatorManagedMode and fails at
	// GetNumberMasters.
	mrfc.On("IsRedisRunning", rf).Once().Return(true)
	mrfc.On("GetNumberMasters", rf).Once().Return(0, checkErr)

	handler := rfOperator.NewRedisFailoverHandler(config, mrfs, mrfc, mrfh, mk, metrics.Dummy, log.Dummy)
	err := handler.Handle(context.Background(), rf)

	assert.Equal(checkErr, err)
	mrfs.AssertExpectations(t)
	mrfc.AssertExpectations(t)
}

// rfLabelManagedByKeyMirror, rfLabelNameKeyMirror and operatorNameMirror
// mirror the unexported constants of the same purpose defined in
// operator/redisfailover/{handler,factory}.go, so tests in this external
// test package can assert on the labels getLabels() produces.
const (
	rfLabelManagedByKeyMirror = "app.kubernetes.io/managed-by"
	rfLabelNameKeyMirror      = "redisfailovers.databases.spotahome.com/name"
	operatorNameMirror        = "redis-operator"
)

// TestHandleGetLabelsWhitelistFiltering exercises getLabels' LabelWhitelist
// handling (operator/redisfailover/handler.go). getLabels is unexported, so
// it is reached indirectly through Handle, capturing the labels map Handle
// passes into Ensure's EnsureRedisMasterService call (which every Ensure()
// path calls unconditionally).
func TestHandleGetLabelsWhitelistFiltering(t *testing.T) {
	tests := []struct {
		name           string
		rfLabels       map[string]string
		whitelist      []string
		wantIncluded   map[string]string
		wantExcludedAt []string
	}{
		{
			name:           "no whitelist keeps every custom label",
			rfLabels:       map[string]string{"team": "cache", "app": "myapp"},
			whitelist:      nil,
			wantIncluded:   map[string]string{"team": "cache", "app": "myapp"},
			wantExcludedAt: nil,
		},
		{
			name:           "whitelist keeps only matching labels",
			rfLabels:       map[string]string{"team": "cache", "app": "myapp", "unrelated": "x"},
			whitelist:      []string{"^team$"},
			wantIncluded:   map[string]string{"team": "cache"},
			wantExcludedAt: []string{"app", "unrelated"},
		},
		{
			name:           "invalid regex entries are skipped, valid ones still applied",
			rfLabels:       map[string]string{"app": "myapp", "other": "y"},
			whitelist:      []string{"(invalid", "^app$"},
			wantIncluded:   map[string]string{"app": "myapp"},
			wantExcludedAt: []string{"other"},
		},
		{
			name:           "whitelist matching nothing yields no custom labels",
			rfLabels:       map[string]string{"app": "myapp"},
			whitelist:      []string{"^nomatch$"},
			wantIncluded:   map[string]string{},
			wantExcludedAt: []string{"app"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)

			// Bootstrapping, no exporter, no sentinels: minimal Ensure()
			// call graph (mirrors TestHandleSkipReconcileAnnotation).
			rf := generateRF(false, true)
			rf.Labels = test.rfLabels
			rf.Spec.LabelWhitelist = test.whitelist

			config := generateConfig()
			mk := &mK8SService.Services{}
			mrfc := &mRFService.RedisFailoverCheck{}
			mrfh := &mRFService.RedisFailoverHeal{}
			mrfs := &mRFService.RedisFailoverClient{}

			var gotLabels map[string]string
			captureLabels := mock.MatchedBy(func(labels map[string]string) bool {
				gotLabels = labels
				return true
			})

			mrfs.On("EnsureNotPresentRedisService", rf).Once().Return(nil)
			mrfs.On("EnsureNotPresentSentinelResources", rf).Once().Return(nil)
			mrfs.On("EnsureRedisMasterService", rf, captureLabels, mock.Anything).Once().Return(nil)
			mrfs.On("EnsureRedisSlaveService", rf, mock.Anything, mock.Anything).Once().Return(nil)
			mrfs.On("EnsureRedisConfigMap", rf, mock.Anything, mock.Anything).Once().Return(nil)
			mrfs.On("EnsureRedisShutdownConfigMap", rf, mock.Anything, mock.Anything).Once().Return(nil)
			mrfs.On("EnsureRedisReadinessConfigMap", rf, mock.Anything, mock.Anything).Once().Return(nil)
			mrfs.On("EnsureRedisStatefulset", rf, mock.Anything, mock.Anything).Once().Return(nil)

			mrfc.On("IsRedisRunning", rf).Once().Return(false)

			handler := rfOperator.NewRedisFailoverHandler(config, mrfs, mrfc, mrfh, mk, metrics.Dummy, log.Dummy)
			err := handler.Handle(context.Background(), rf)
			assert.NoError(err)

			if assert.NotNil(gotLabels) {
				assert.Equal(operatorNameMirror, gotLabels[rfLabelManagedByKeyMirror])
				assert.Equal(rf.Name, gotLabels[rfLabelNameKeyMirror])
				for k, v := range test.wantIncluded {
					assert.Equal(v, gotLabels[k], "expected label %q=%q to be kept", k, v)
				}
				for _, k := range test.wantExcludedAt {
					_, ok := gotLabels[k]
					assert.False(ok, "expected label %q to be filtered out", k)
				}
			}

			mrfs.AssertExpectations(t)
			mrfc.AssertExpectations(t)
		})
	}
}
