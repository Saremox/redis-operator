package redisfailover_test

import (
	"errors"
	"fmt"
	v1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	mRFService "github.com/saremox/redis-operator/mocks/operator/redisfailover/service"
	mK8SService "github.com/saremox/redis-operator/mocks/service/k8s"
	mRedisService "github.com/saremox/redis-operator/mocks/service/redis"
	rfOperator "github.com/saremox/redis-operator/operator/redisfailover"
	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
	"github.com/saremox/redis-operator/service/redis"
)

func TestCheckAndHeal(t *testing.T) {
	tests := []struct {
		name                           string
		nMasters                       int
		nRedis                         int
		forceNewMasterNoQrm            bool
		forceNewMasterFirstBoot        bool
		singleMasterTest               bool
		slavesOK                       bool
		sentinelMonitorOK              bool
		sentinelNumberInMemoryOK       bool
		sentinelSlavesNumberInMemoryOK bool
		redisCheckNumberOK             bool
		redisSetMasterOnAllOK          bool
		bootstrapping                  bool
		allowSentinels                 bool
		// wantState overrides the default expErr-derived expected state
		// (NotHealthy-with-error / Healthy-with-no-error) for cases where the
		// function legitimately returns nil while still being unhealthy - e.g.
		// checkAndHealBootstrapMode's "not all replicas running" gate, which
		// waits for the next reconcile rather than erroring.
		wantState string
	}{
		{
			name:                           "Everything ok, no need to heal",
			nMasters:                       1,
			nRedis:                         3,
			singleMasterTest:               false,
			forceNewMasterNoQrm:            false,
			forceNewMasterFirstBoot:        false,
			slavesOK:                       true,
			sentinelMonitorOK:              true,
			sentinelNumberInMemoryOK:       true,
			sentinelSlavesNumberInMemoryOK: true,
			redisCheckNumberOK:             true,
			redisSetMasterOnAllOK:          true,
			bootstrapping:                  false,
			allowSentinels:                 false,
		},
		{
			name:                           "Multiple masters",
			nMasters:                       2,
			nRedis:                         3,
			singleMasterTest:               false,
			forceNewMasterNoQrm:            false,
			forceNewMasterFirstBoot:        false,
			slavesOK:                       true,
			sentinelMonitorOK:              true,
			sentinelNumberInMemoryOK:       true,
			sentinelSlavesNumberInMemoryOK: true,
			redisCheckNumberOK:             true,
			redisSetMasterOnAllOK:          true,
			bootstrapping:                  false,
			allowSentinels:                 false,
		},
		{
			name:                           "No masters but wait",
			nMasters:                       0,
			nRedis:                         3,
			singleMasterTest:               false,
			forceNewMasterNoQrm:            false,
			forceNewMasterFirstBoot:        false,
			slavesOK:                       true,
			sentinelMonitorOK:              true,
			sentinelNumberInMemoryOK:       true,
			sentinelSlavesNumberInMemoryOK: true,
			redisCheckNumberOK:             true,
			redisSetMasterOnAllOK:          true,
			bootstrapping:                  false,
			allowSentinels:                 false,

			// There is no master until the Sentinel failover ends.
			wantState: v1.NotHealthyState,
		},
		{
			name:                           "No masters, only one redis available, make master",
			nMasters:                       0,
			nRedis:                         1,
			singleMasterTest:               true,
			forceNewMasterNoQrm:            false,
			forceNewMasterFirstBoot:        false,
			slavesOK:                       true,
			sentinelMonitorOK:              true,
			sentinelNumberInMemoryOK:       true,
			sentinelSlavesNumberInMemoryOK: true,
			redisCheckNumberOK:             true,
			redisSetMasterOnAllOK:          true,
			bootstrapping:                  false,
			allowSentinels:                 false,
		},
		{
			name:                           "No masters,No sentinel quorum set random",
			nMasters:                       0,
			nRedis:                         3,
			singleMasterTest:               false,
			forceNewMasterNoQrm:            true,
			forceNewMasterFirstBoot:        false,
			slavesOK:                       true,
			sentinelMonitorOK:              true,
			sentinelNumberInMemoryOK:       true,
			redisCheckNumberOK:             true,
			redisSetMasterOnAllOK:          true,
			sentinelSlavesNumberInMemoryOK: true,
			allowSentinels:                 false,
		},
		{
			name:                           "No masters,Sentinel Quorum but slave of local host set random",
			nMasters:                       0,
			nRedis:                         3,
			singleMasterTest:               false,
			forceNewMasterNoQrm:            false,
			forceNewMasterFirstBoot:        true,
			slavesOK:                       true,
			sentinelMonitorOK:              true,
			sentinelNumberInMemoryOK:       true,
			redisCheckNumberOK:             true,
			redisSetMasterOnAllOK:          true,
			sentinelSlavesNumberInMemoryOK: true,
			allowSentinels:                 false,
		},
		{
			name:                           "Slaves from master wrong",
			nMasters:                       1,
			nRedis:                         3,
			singleMasterTest:               false,
			forceNewMasterNoQrm:            false,
			forceNewMasterFirstBoot:        false,
			slavesOK:                       false,
			sentinelMonitorOK:              true,
			sentinelNumberInMemoryOK:       true,
			sentinelSlavesNumberInMemoryOK: true,
			redisCheckNumberOK:             true,
			redisSetMasterOnAllOK:          true,
			bootstrapping:                  false,
			allowSentinels:                 false,
		},
		{
			name:                           "Sentinels not pointing correct monitor",
			nMasters:                       1,
			nRedis:                         3,
			singleMasterTest:               false,
			forceNewMasterNoQrm:            false,
			forceNewMasterFirstBoot:        false,
			slavesOK:                       true,
			sentinelMonitorOK:              false,
			sentinelNumberInMemoryOK:       true,
			sentinelSlavesNumberInMemoryOK: true,
			redisCheckNumberOK:             true,
			redisSetMasterOnAllOK:          true,
			bootstrapping:                  false,
			allowSentinels:                 false,
		},
		{
			name:                           "Sentinels with wrong number of sentinels",
			nMasters:                       1,
			nRedis:                         3,
			singleMasterTest:               false,
			forceNewMasterNoQrm:            false,
			forceNewMasterFirstBoot:        false,
			slavesOK:                       true,
			sentinelMonitorOK:              true,
			sentinelNumberInMemoryOK:       false,
			sentinelSlavesNumberInMemoryOK: true,
			redisCheckNumberOK:             true,
			redisSetMasterOnAllOK:          true,
			bootstrapping:                  false,
			allowSentinels:                 false,
		},
		{
			name:                           "Sentinels with wrong number of slaves",
			nMasters:                       1,
			nRedis:                         3,
			singleMasterTest:               false,
			forceNewMasterNoQrm:            false,
			forceNewMasterFirstBoot:        false,
			slavesOK:                       true,
			sentinelMonitorOK:              true,
			sentinelNumberInMemoryOK:       true,
			sentinelSlavesNumberInMemoryOK: false,
			redisCheckNumberOK:             true,
			redisSetMasterOnAllOK:          true,
			bootstrapping:                  false,
			allowSentinels:                 false,
		},
		{
			name:                  "Bootstrapping Mode",
			nMasters:              1,
			nRedis:                3,
			redisCheckNumberOK:    true,
			redisSetMasterOnAllOK: true,
			bootstrapping:         true,
			allowSentinels:        false,
		},
		{
			name:                  "Bootstrapping Mode with failure to check redis number",
			nMasters:              1,
			nRedis:                3,
			redisCheckNumberOK:    false,
			redisSetMasterOnAllOK: true,
			bootstrapping:         true,
			allowSentinels:        false,
			// checkAndHealBootstrapMode's redis-not-running gate must report
			// NotHealthy even though it returns nil (it's waiting for the next
			// reconcile, not failing outright).
			wantState: v1.NotHealthyState,
		},
		{
			name:                  "Bootstrapping Mode with failure to set master on all",
			nMasters:              1,
			nRedis:                3,
			redisCheckNumberOK:    true,
			redisSetMasterOnAllOK: false,
			bootstrapping:         true,
			allowSentinels:        false,
		},
		{
			name:                           "Bootstrapping Mode that allows sentinels",
			nMasters:                       1,
			nRedis:                         3,
			redisCheckNumberOK:             true,
			redisSetMasterOnAllOK:          true,
			sentinelMonitorOK:              true,
			sentinelNumberInMemoryOK:       true,
			sentinelSlavesNumberInMemoryOK: true,
			bootstrapping:                  true,
			allowSentinels:                 true,
		},
		{
			name:                           "Bootstrapping Mode that allows sentinels sentinel monitor fails",
			nMasters:                       1,
			nRedis:                         3,
			redisCheckNumberOK:             true,
			redisSetMasterOnAllOK:          true,
			sentinelMonitorOK:              false,
			sentinelNumberInMemoryOK:       true,
			sentinelSlavesNumberInMemoryOK: true,
			bootstrapping:                  true,
			allowSentinels:                 true,
		},
		{
			name:                           "Bootstrapping Mode that allows sentinels sentinel with wrong number of sentinels",
			nMasters:                       1,
			nRedis:                         3,
			redisCheckNumberOK:             true,
			redisSetMasterOnAllOK:          true,
			sentinelMonitorOK:              true,
			sentinelNumberInMemoryOK:       false,
			sentinelSlavesNumberInMemoryOK: true,
			bootstrapping:                  true,
			allowSentinels:                 true,
		},
		{
			name:                           "Bootstrapping Mode that allows sentinels sentinel with wrong number of slaves",
			nMasters:                       1,
			nRedis:                         3,
			redisCheckNumberOK:             true,
			redisSetMasterOnAllOK:          true,
			sentinelMonitorOK:              true,
			sentinelNumberInMemoryOK:       true,
			sentinelSlavesNumberInMemoryOK: false,
			bootstrapping:                  true,
			allowSentinels:                 true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertTest := assert.New(t)

			allowSentinels := true
			bootstrappingTests := test.bootstrapping
			bootstrapMaster := "127.0.0.1"
			bootstrapMasterPort := "6379"

			rf := generateRF(false, bootstrappingTests)
			if bootstrappingTests {
				allowSentinels = test.allowSentinels
				rf.Spec.BootstrapNode.AllowSentinels = allowSentinels
			}
			if test.singleMasterTest {
				rf.Spec.Redis.Replicas = 1
			}

			expErr := false
			continueTests := true

			master := "0.0.0.0"
			sentinel := "1.1.1.1"

			config := generateConfig()
			mk := settledK8sServices()
			// CheckAndHeal always defers updateStatus, on every return path.
			mk.On("UpdateRedisFailoverStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return()
			mrfs := &mRFService.RedisFailoverClient{}
			mrfc := &mRFService.RedisFailoverCheck{}
			mrfh := &mRFService.RedisFailoverHeal{}
			mrfh.On("ApplyPassword", mock.Anything, mock.Anything, mock.Anything).Maybe().Return(true, nil)
			mrfh.On("ApplySentinelPassword", mock.Anything, mock.Anything).Maybe().Return(true, nil)

			// Normal CheckAndHeal gates on a quorum; bootstrap mode still gates on
			// the full set, so route the mock to whichever the code under test calls.
			redisRunningMethod := "IsRedisRunningQuorum"
			sentinelRunningMethod := "IsSentinelRunningQuorum"
			if bootstrappingTests {
				redisRunningMethod = "IsRedisRunning"
				sentinelRunningMethod = "IsSentinelRunning"
			}

			if test.redisCheckNumberOK {
				mrfc.On(redisRunningMethod, rf).Once().Return(true)
			} else {
				continueTests = false
				mrfc.On(redisRunningMethod, rf).Once().Return(false)
			}

			if allowSentinels {
				mrfc.On(sentinelRunningMethod, rf).Once().Return(true)
			}

			if bootstrappingTests && continueTests {
				// once to get ips for config update, once for the UpdateRedisesPods go right
				mrfc.On("GetRedisesIPs", rf).Twice().Return([]string{"0.0.0.1", "0.0.0.2", "0.0.0.3"}, nil)
				mrfh.On("SetRedisCustomConfig", "0.0.0.1", rf).Once().Return(nil)
				mrfh.On("SetRedisCustomConfig", "0.0.0.2", rf).Once().Return(nil)
				mrfh.On("SetRedisCustomConfig", "0.0.0.3", rf).Once().Return(nil)
				mrfc.On("CheckRedisSlavesReady", "0.0.0.1", rf).Once().Return(true, nil)
				mrfc.On("CheckRedisSlavesReady", "0.0.0.2", rf).Once().Return(true, nil)
				mrfc.On("CheckRedisSlavesReady", "0.0.0.3", rf).Once().Return(true, nil)
				mrfc.On("GetStatefulSetUpdateRevision", rf).Once().Return("1", nil)
				mrfc.On("GetRedisesSlavesPods", rf).Once().Return([]string{}, nil)

				if test.redisSetMasterOnAllOK {
					mrfh.On("SetExternalMasterOnAll", bootstrapMaster, bootstrapMasterPort, rf).Once().Return(nil)
				} else {
					expErr = true
					mrfh.On("SetExternalMasterOnAll", bootstrapMaster, bootstrapMasterPort, rf).Once().Return(errors.New(""))
				}
			} else if continueTests {
				mrfc.On("GetNumberMasters", rf).Once().Return(test.nMasters, nil)
				switch test.nMasters {
				case 0:
					//mrfc.On("GetRedisesIPs", rf).Once().Return(make([]string, test.nRedis), nil)
					if rf.Spec.Redis.Replicas == 1 {
						mrfh.On("SetOldestAsMaster", rf).Once().Return(nil)
						continueTests = false
						break
					}
					mrfc.On("GetMaxRedisPodTime", rf).Once().Return(1*time.Hour, nil)
					if test.forceNewMasterNoQrm {
						mrfc.On("CheckSentinelQuorum", rf).Once().Return(1, errors.New(""))
						mrfh.On("SetOldestAsMaster", rf).Once().Return(nil)
					} else if test.forceNewMasterFirstBoot {
						mrfc.On("CheckSentinelQuorum", rf).Once().Return(3, nil)
						mrfc.On("CheckIfMasterLocalhost", rf).Once().Return(true, nil)
						mrfh.On("SetOldestAsMaster", rf).Once().Return(nil)
					} else {
						mrfc.On("CheckSentinelQuorum", rf).Once().Return(3, nil)
						mrfc.On("CheckIfMasterLocalhost", rf).Once().Return(false, nil)
						mrfc.On("CheckSentinelsCannotFailover", rf).Once().Return(false, nil)
						continueTests = false
					}

				case 1:
					break
				default:
					// always expect error
					expErr = true
				}
				if !expErr && continueTests {
					// checker.go re-resolves GetMasterIP right before SetMasterOnAll
					// (when slaves are wrong) and right before NewSentinelMonitor
					// (when a sentinel isn't monitoring the right master), on top
					// of the base call in checkAndHeal and the one inside
					// UpdateRedisesPods.
					getMasterIPCalls := 2
					if !test.slavesOK {
						getMasterIPCalls++
					}
					if !test.sentinelMonitorOK {
						getMasterIPCalls++
					}
					mrfc.On("GetMasterIP", rf).Times(getMasterIPCalls).Return(master, nil)
					if test.slavesOK {
						mrfc.On("CheckAllSlavesFromMaster", master, rf).Once().Return(nil)
					} else {
						mrfc.On("CheckAllSlavesFromMaster", master, rf).Once().Return(errors.New(""))
						if test.redisSetMasterOnAllOK {
							mrfh.On("SetMasterOnAll", master, rf).Once().Return(nil)
						} else {
							expErr = true
							mrfh.On("SetMasterOnAll", master, rf).Once().Return(errors.New(""))
						}

					}
					mrfc.On("GetRedisesIPs", rf).Twice().Return([]string{master}, nil)
					mrfc.On("GetStatefulSetUpdateRevision", rf).Once().Return("1", nil)
					mrfc.On("GetRedisesSlavesPods", rf).Once().Return([]string{}, nil)
					mrfc.On("GetRedisesMasterPod", rf).Once().Return(master, nil)
					mrfc.On("GetRedisRevisionHash", master, rf).Once().Return("1", nil)
					mrfh.On("SetRedisCustomConfig", master, rf).Once().Return(nil)
				}
			}

			if allowSentinels && !expErr && continueTests {
				mrfc.On("GetSentinelsIPs", rf).Once().Return([]string{sentinel}, nil)
				if test.sentinelMonitorOK {
					if test.bootstrapping {
						mrfc.On("CheckSentinelMonitor", sentinel, bootstrapMaster, bootstrapMasterPort).Once().Return(nil)
					} else {
						mrfc.On("CheckSentinelMonitor", sentinel, master, "0").Once().Return(nil)
					}
				} else {
					if test.bootstrapping {
						mrfc.On("CheckSentinelMonitor", sentinel, bootstrapMaster, bootstrapMasterPort).Once().Return(errors.New(""))
						mrfh.On("NewSentinelMonitorWithPort", sentinel, bootstrapMaster, bootstrapMasterPort, rf).Once().Return(nil)
					} else {
						mrfc.On("CheckSentinelMonitor", sentinel, master, "0").Once().Return(errors.New(""))
						mrfh.On("NewSentinelMonitor", sentinel, master, rf).Once().Return(nil)
					}
				}
				if test.sentinelNumberInMemoryOK {
					mrfc.On("CheckSentinelNumberInMemory", sentinel, rf).Once().Return(nil)
				} else {
					mrfc.On("CheckSentinelNumberInMemory", sentinel, rf).Once().Return(errors.New(""))
					mrfh.On("RestoreSentinel", sentinel).Once().Return(nil)
				}
				if test.sentinelSlavesNumberInMemoryOK {
					mrfc.On("CheckSentinelSlavesNumberInMemory", sentinel, rf).Once().Return(nil)
				} else {
					mrfc.On("CheckSentinelSlavesNumberInMemory", sentinel, rf).Once().Return(errors.New(""))
					mrfh.On("RestoreSentinel", sentinel).Once().Return(nil)
				}
				mrfh.On("SetSentinelCustomConfig", sentinel, rf).Once().Return(nil)
			}

			handler := rfOperator.NewRedisFailoverHandler(config, mrfs, mrfc, mrfh, mk, metrics.Dummy, log.Dummy)
			err := handler.CheckAndHeal(rf)

			if test.wantState != "" {
				assertTest.NoError(err)
				assertTest.Equal(test.wantState, rf.Status.State)
			} else if expErr {
				assertTest.Error(err)
				assertTest.Equal(v1.NotHealthyState, rf.Status.State)
			} else {
				assertTest.NoError(err)
				assertTest.Equal(v1.HealthyState, rf.Status.State)
			}
			mrfc.AssertExpectations(t)
			mrfh.AssertExpectations(t)
		})
	}
}

// operatorManagedRF returns a RedisFailover configured with Sentinel disabled
// (sentinel.enabled=false) and no BootstrapNode, so that CheckAndHeal routes
// into checkAndHealOperatorManagedMode.
func operatorManagedRF() *v1.RedisFailover {
	rf := generateRF(false, false)
	rf.Spec.Sentinel.Enabled = ptr.To(false)
	return rf
}

// TestCheckAndHealOperatorManagedMode exercises checkAndHealOperatorManagedMode
// (operator/redisfailover/checker.go), the failover path used whenever Sentinel
// is disabled (the default since v4.0.0). It is reached only via the exported
// CheckAndHeal entrypoint, since checkAndHealOperatorManagedMode is unexported
// and this file lives in the external redisfailover_test package.
func TestCheckAndHealOperatorManagedMode(t *testing.T) {
	const (
		master     = "10.0.0.1"
		promotedIP = "10.0.0.2"
	)

	// wrappedPartialErr simulates PromoteBestReplica returning an error that
	// wraps rfservice.ErrPartialReconciliation, as heal.go's PromoteBestReplica
	// does when the promotion itself succeeds but replica reconfiguration fails.
	wrappedPartialErr := fmt.Errorf("reconfigure replicas: %w", rfservice.ErrPartialReconciliation)

	// setupSharedSuccess wires up the calls made by applyRedisCustomConfig and
	// UpdateRedisesPods (shared helpers, already covered elsewhere) so that both
	// succeed cleanly with a single redis IP that is also the master.
	setupSharedSuccess := func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
		mrfc.On("GetRedisesIPs", rf).Twice().Return([]string{master}, nil)
		mrfh.On("SetRedisCustomConfig", master, rf).Once().Return(nil)
		mrfc.On("GetMasterIP", rf).Once().Return(master, nil)
		mrfc.On("GetStatefulSetUpdateRevision", rf).Once().Return("1", nil)
		mrfc.On("GetRedisesSlavesPods", rf).Once().Return([]string{}, nil)
		mrfc.On("GetRedisesMasterPod", rf).Once().Return(master, nil)
		mrfc.On("GetRedisRevisionHash", master, rf).Once().Return("1", nil)
	}

	tests := []struct {
		name        string
		setup       func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover)
		masterPod   bool
		wantErr     bool
		wantErrIs   error
		wantState   string
		wantMessage string
		// wantMessageRegexp matches a message that contains the current time.
		wantMessageRegexp string
	}{
		{
			name: "redis quorum not running - waits for statefulset reconcile",
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("IsRedisRunningQuorum", rf).Once().Return(false)
			},
			wantErr:     false,
			wantState:   v1.NotHealthyState,
			wantMessage: "redis quorum not running",
		},
		{
			name: "GetNumberMasters errors",
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("IsRedisRunningQuorum", rf).Once().Return(true)
				mrfc.On("GetNumberMasters", rf).Once().Return(0, errors.New("num masters err"))
			},
			wantErr:     true,
			wantState:   v1.NotHealthyState,
			wantMessage: "unable to get number of masters",
		},
		{
			name: "no master - best replica found and promoted successfully",
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("IsRedisRunningQuorum", rf).Once().Return(true)
				mrfc.On("GetNumberMasters", rf).Once().Return(0, nil)
				mrfc.On("GetBestReplicaForPromotion", rf).Once().Return(&rfservice.ReplicaInfo{IP: promotedIP}, nil)
				mrfh.On("PromoteBestReplica", promotedIP, rf).Once().Return(nil)
				// NOTE: on success this branch returns nil immediately (see the
				// `return nil` at the end of the nMasters==0 case in checker.go) -
				// it does NOT continue on to applyRedisCustomConfig/UpdateRedisesPods
				// in the same reconcile pass, unlike what a first read of the task
				// might suggest. No further mock calls are expected here.
			},
			wantErr:   false,
			wantState: v1.HealthyState,
		},
		{
			name: "no master - best replica found but promotion fails",
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("IsRedisRunningQuorum", rf).Once().Return(true)
				mrfc.On("GetNumberMasters", rf).Once().Return(0, nil)
				mrfc.On("GetBestReplicaForPromotion", rf).Once().Return(&rfservice.ReplicaInfo{IP: promotedIP}, nil)
				mrfh.On("PromoteBestReplica", promotedIP, rf).Once().Return(errors.New("promote fail"))
			},
			wantErr:     true,
			wantState:   v1.NotHealthyState,
			wantMessage: "failed to promote replica",
		},
		{
			name: "no master - best replica found but promotion partially fails",
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("IsRedisRunningQuorum", rf).Once().Return(true)
				mrfc.On("GetNumberMasters", rf).Once().Return(0, nil)
				mrfc.On("GetBestReplicaForPromotion", rf).Once().Return(&rfservice.ReplicaInfo{IP: promotedIP}, nil)
				mrfh.On("PromoteBestReplica", promotedIP, rf).Once().Return(wrappedPartialErr)
			},
			wantErr:     true,
			wantErrIs:   rfservice.ErrPartialReconciliation,
			wantState:   v1.NotHealthyState,
			wantMessage: "failover incomplete: replica reconfiguration failed",
		},
		{
			name: "no master - best replica lookup fails, falls back to oldest and succeeds",
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("IsRedisRunningQuorum", rf).Once().Return(true)
				mrfc.On("GetNumberMasters", rf).Once().Return(0, nil)
				mrfc.On("GetBestReplicaForPromotion", rf).Once().Return(nil, errors.New("no replica info"))
				mrfh.On("SetOldestAsMaster", rf).Once().Return(nil)
				// Same as above: success here returns nil immediately, no shared
				// config apply/pod update calls in this reconcile pass.
			},
			wantErr:   false,
			wantState: v1.HealthyState,
		},
		{
			name: "no master - best replica lookup fails, fallback to oldest also fails",
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("IsRedisRunningQuorum", rf).Once().Return(true)
				mrfc.On("GetNumberMasters", rf).Once().Return(0, nil)
				mrfc.On("GetBestReplicaForPromotion", rf).Once().Return(nil, errors.New("no replica info"))
				mrfh.On("SetOldestAsMaster", rf).Once().Return(errors.New("elect fail"))
			},
			wantErr:     true,
			wantState:   v1.NotHealthyState,
			wantMessage: "failed to elect master",
		},
		{
			name: "single master - health check errors",
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("IsRedisRunningQuorum", rf).Once().Return(true)
				mrfc.On("GetNumberMasters", rf).Once().Return(1, nil)
				mrfc.On("CheckMasterHealth", rf).Once().Return(false, "", errors.New("health check err"))
			},
			wantErr:     true,
			wantState:   v1.NotHealthyState,
			wantMessage: "unable to check master health",
		},
		{
			// The mocks expect no promotion, so a promotion fails the test.
			name: "single master - unhealthy, waits for the failover timeout",
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("IsRedisRunningQuorum", rf).Once().Return(true)
				mrfc.On("GetNumberMasters", rf).Once().Return(1, nil)
				mrfc.On("CheckMasterHealth", rf).Once().Return(false, master, nil)
			},
			masterPod:         true,
			wantErr:           false,
			wantState:         v1.NotHealthyState,
			wantMessageRegexp: `^master unreachable since \S+Z, failing over after 10s$`,
		},
		{
			name: "single master - unhealthy, replica found and promoted successfully",
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("IsRedisRunningQuorum", rf).Once().Return(true)
				mrfc.On("GetNumberMasters", rf).Once().Return(1, nil)
				rf.Spec.Sentinel.FailoverTimeout = &metav1.Duration{}
				mrfc.On("CheckMasterHealth", rf).Once().Return(false, master, nil)
				mrfc.On("GetBestReplicaForPromotion", rf).Once().Return(&rfservice.ReplicaInfo{IP: promotedIP}, nil)
				mrfh.On("PromoteBestReplica", promotedIP, rf).Once().Return(nil)
				// Returns nil immediately on success here too - no shared
				// config apply/pod update calls expected.
			},
			wantErr:   false,
			wantState: v1.HealthyState,
		},
		{
			name: "single master - unhealthy, no replica available for failover",
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("IsRedisRunningQuorum", rf).Once().Return(true)
				mrfc.On("GetNumberMasters", rf).Once().Return(1, nil)
				rf.Spec.Sentinel.FailoverTimeout = &metav1.Duration{}
				mrfc.On("CheckMasterHealth", rf).Once().Return(false, master, nil)
				mrfc.On("GetBestReplicaForPromotion", rf).Once().Return(nil, errors.New("no replica info"))
			},
			wantErr:     true,
			wantState:   v1.NotHealthyState,
			wantMessage: "no healthy replica available for failover",
		},
		{
			name: "single master - unhealthy, promotion fails",
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("IsRedisRunningQuorum", rf).Once().Return(true)
				mrfc.On("GetNumberMasters", rf).Once().Return(1, nil)
				rf.Spec.Sentinel.FailoverTimeout = &metav1.Duration{}
				mrfc.On("CheckMasterHealth", rf).Once().Return(false, master, nil)
				mrfc.On("GetBestReplicaForPromotion", rf).Once().Return(&rfservice.ReplicaInfo{IP: promotedIP}, nil)
				mrfh.On("PromoteBestReplica", promotedIP, rf).Once().Return(errors.New("promote fail"))
			},
			wantErr:     true,
			wantState:   v1.NotHealthyState,
			wantMessage: "failover failed",
		},
		{
			name: "single master - unhealthy, promotion partially fails",
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("IsRedisRunningQuorum", rf).Once().Return(true)
				mrfc.On("GetNumberMasters", rf).Once().Return(1, nil)
				rf.Spec.Sentinel.FailoverTimeout = &metav1.Duration{}
				mrfc.On("CheckMasterHealth", rf).Once().Return(false, master, nil)
				mrfc.On("GetBestReplicaForPromotion", rf).Once().Return(&rfservice.ReplicaInfo{IP: promotedIP}, nil)
				mrfh.On("PromoteBestReplica", promotedIP, rf).Once().Return(wrappedPartialErr)
			},
			wantErr:     true,
			wantErrIs:   rfservice.ErrPartialReconciliation,
			wantState:   v1.NotHealthyState,
			wantMessage: "failover incomplete: replica reconfiguration failed",
		},
		{
			name: "single master - healthy, slaves already correct, config and pods updated",
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("IsRedisRunningQuorum", rf).Once().Return(true)
				mrfc.On("GetNumberMasters", rf).Once().Return(1, nil)
				mrfc.On("CheckMasterHealth", rf).Once().Return(true, master, nil)
				mrfc.On("CheckAllSlavesFromMaster", master, rf).Once().Return(nil)
				setupSharedSuccess(mrfc, mrfh, rf)
			},
			wantErr:   false,
			wantState: v1.HealthyState,
		},
		{
			// No UpdateRedisesPods expectations: replacing pods would panic the mock.
			name: "single master - maxmemory holds the pod rollout",
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				rf.Spec.Redis.MaxMemory = &v1.MaxMemorySettings{Percent: 75, Policy: "noeviction"}
				mrfc.On("IsRedisRunningQuorum", rf).Once().Return(true)
				mrfc.On("GetNumberMasters", rf).Once().Return(1, nil)
				mrfc.On("CheckMasterHealth", rf).Once().Return(true, master, nil)
				mrfc.On("CheckAllSlavesFromMaster", master, rf).Once().Return(nil)
				mrfc.On("GetRedisesIPs", rf).Twice().Return([]string{master}, nil)
				mrfh.On("SetRedisCustomConfig", master, rf).Once().Return(nil)
				mrfc.On("GetMasterIP", rf).Once().Return(master, nil)
				mrfh.On("EnsureRedisMaxMemory", rf, master, []string{master}).Once().Return(rfservice.MaxMemoryResult{Message: "maxmemory kept", HoldRollout: true}, nil)
			},
			wantErr:     false,
			wantState:   v1.HealthyState,
			wantMessage: "maxmemory kept",
		},
		{
			name: "single master - healthy, slaves fixed, config and pods updated",
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("IsRedisRunningQuorum", rf).Once().Return(true)
				mrfc.On("GetNumberMasters", rf).Once().Return(1, nil)
				mrfc.On("CheckMasterHealth", rf).Once().Return(true, master, nil)
				mrfc.On("CheckAllSlavesFromMaster", master, rf).Once().Return(errors.New("wrong master"))
				mrfh.On("SetMasterOnAll", master, rf).Once().Return(nil)
				setupSharedSuccess(mrfc, mrfh, rf)
			},
			wantErr:   false,
			wantState: v1.HealthyState,
		},
		{
			name: "single master - healthy, slaves wrong, fixing them fails",
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("IsRedisRunningQuorum", rf).Once().Return(true)
				mrfc.On("GetNumberMasters", rf).Once().Return(1, nil)
				mrfc.On("CheckMasterHealth", rf).Once().Return(true, master, nil)
				mrfc.On("CheckAllSlavesFromMaster", master, rf).Once().Return(errors.New("wrong master"))
				mrfh.On("SetMasterOnAll", master, rf).Once().Return(errors.New("set fail"))
			},
			wantErr:     true,
			wantState:   v1.NotHealthyState,
			wantMessage: "failed to configure slaves",
		},
		{
			name: "multiple masters detected",
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("IsRedisRunningQuorum", rf).Once().Return(true)
				mrfc.On("GetNumberMasters", rf).Once().Return(2, nil)
			},
			wantErr:     true,
			wantState:   v1.NotHealthyState,
			wantMessage: "multiple masters detected, fix manually",
		},
		{
			name: "single master - healthy, applying custom config fails",
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("IsRedisRunningQuorum", rf).Once().Return(true)
				mrfc.On("GetNumberMasters", rf).Once().Return(1, nil)
				mrfc.On("CheckMasterHealth", rf).Once().Return(true, master, nil)
				mrfc.On("CheckAllSlavesFromMaster", master, rf).Once().Return(nil)
				mrfc.On("GetRedisesIPs", rf).Once().Return([]string{master}, nil)
				mrfh.On("SetRedisCustomConfig", master, rf).Once().Return(errors.New("cfg err"))
			},
			wantErr:     true,
			wantState:   v1.NotHealthyState,
			wantMessage: "unable to apply custom config",
		},
		{
			name: "single master - healthy, updating redis pods fails",
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("IsRedisRunningQuorum", rf).Once().Return(true)
				mrfc.On("GetNumberMasters", rf).Once().Return(1, nil)
				mrfc.On("CheckMasterHealth", rf).Once().Return(true, master, nil)
				mrfc.On("CheckAllSlavesFromMaster", master, rf).Once().Return(nil)
				mrfc.On("GetRedisesIPs", rf).Twice().Return([]string{master}, nil)
				mrfh.On("SetRedisCustomConfig", master, rf).Once().Return(nil)
				mrfc.On("GetMasterIP", rf).Once().Return(master, nil)
				mrfc.On("GetStatefulSetUpdateRevision", rf).Once().Return("", errors.New("ssur err"))
			},
			wantErr:     true,
			wantState:   v1.NotHealthyState,
			wantMessage: "unable to update redis pods",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertTest := assert.New(t)

			rf := operatorManagedRF()

			config := generateConfig()
			mk := settledK8sServices()
			if test.masterPod {
				pod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "rfr-0"}, Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: master}}
				mk = &mK8SService.Services{}
				mk.On("GetStatefulSetPods", mock.Anything, mock.Anything).Return(&corev1.PodList{Items: []corev1.Pod{pod}}, nil)
				mk.On("UpdatePodAnnotations", rf.Namespace, "rfr-0", mock.Anything).Once().Return(nil)
			}
			// CheckAndHeal always defers updateStatus, on every return path.
			mk.On("UpdateRedisFailoverStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return()
			mrfs := &mRFService.RedisFailoverClient{}
			mrfc := &mRFService.RedisFailoverCheck{}
			mrfh := &mRFService.RedisFailoverHeal{}
			mrfh.On("ApplyPassword", mock.Anything, mock.Anything, mock.Anything).Maybe().Return(true, nil)
			mrfh.On("ApplySentinelPassword", mock.Anything, mock.Anything).Maybe().Return(true, nil)

			test.setup(mrfc, mrfh, rf)

			handler := rfOperator.NewRedisFailoverHandler(config, mrfs, mrfc, mrfh, mk, metrics.Dummy, log.Dummy)
			err := handler.CheckAndHeal(rf)

			if test.wantErr {
				assertTest.Error(err)
			} else {
				assertTest.NoError(err)
			}
			if test.wantErrIs != nil {
				assertTest.True(errors.Is(err, test.wantErrIs), "expected error to wrap %v, got %v", test.wantErrIs, err)
			}
			assertTest.Equal(test.wantState, rf.Status.State)
			if test.wantMessage != "" {
				assertTest.Equal(test.wantMessage, rf.Status.Message)
			}
			if test.wantMessageRegexp != "" {
				assertTest.Regexp(test.wantMessageRegexp, rf.Status.Message)
			}

			mrfc.AssertExpectations(t)
			mrfh.AssertExpectations(t)
		})
	}
}

// TestUpdateStatusLastChanged exercises updateStatus's (operator/redisfailover/checker.go)
// LastChanged stamping: it must be refreshed to "now" only when the health
// state actually transitions, and otherwise preserve whatever value was
// already recorded - not the zero value checkAndHealOperatorManagedMode's
// branches leave behind when they rebuild rf.Status without carrying it
// forward. Routed through the exported CheckAndHeal, since updateStatus and
// checkAndHealOperatorManagedMode are both unexported and this file lives in
// the external redisfailover_test package.
func TestUpdateStatusLastChanged(t *testing.T) {
	const master = "10.0.0.1"

	// setupHealthyPass wires up a full, successful single-master pass (mirrors
	// the "slaves already correct" case in TestCheckAndHealOperatorManagedMode),
	// so CheckAndHeal always lands on HealthyState here.
	setupHealthyPass := func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
		mrfc.On("IsRedisRunningQuorum", rf).Once().Return(true)
		mrfc.On("GetNumberMasters", rf).Once().Return(1, nil)
		mrfc.On("CheckMasterHealth", rf).Once().Return(true, master, nil)
		mrfc.On("CheckAllSlavesFromMaster", master, rf).Once().Return(nil)
		mrfc.On("GetRedisesIPs", rf).Twice().Return([]string{master}, nil)
		mrfh.On("SetRedisCustomConfig", master, rf).Once().Return(nil)
		mrfc.On("GetMasterIP", rf).Once().Return(master, nil)
		mrfc.On("GetStatefulSetUpdateRevision", rf).Once().Return("1", nil)
		mrfc.On("GetRedisesSlavesPods", rf).Once().Return([]string{}, nil)
		mrfc.On("GetRedisesMasterPod", rf).Once().Return(master, nil)
		mrfc.On("GetRedisRevisionHash", master, rf).Once().Return("1", nil)
	}

	t.Run("state transition stamps LastChanged to now", func(t *testing.T) {
		assertTest := assert.New(t)

		rf := operatorManagedRF()
		rf.Status.State = v1.NotHealthyState
		rf.Status.LastChanged = "2020-01-01T00:00:00Z"

		config := generateConfig()
		mk := settledK8sServices()
		mk.On("UpdateRedisFailoverStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return()
		mrfs := &mRFService.RedisFailoverClient{}
		mrfc := &mRFService.RedisFailoverCheck{}
		mrfh := &mRFService.RedisFailoverHeal{}
		mrfh.On("ApplyPassword", mock.Anything, mock.Anything, mock.Anything).Maybe().Return(true, nil)
		mrfh.On("ApplySentinelPassword", mock.Anything, mock.Anything).Maybe().Return(true, nil)

		setupHealthyPass(mrfc, mrfh, rf)

		before := time.Now()
		handler := rfOperator.NewRedisFailoverHandler(config, mrfs, mrfc, mrfh, mk, metrics.Dummy, log.Dummy)
		err := handler.CheckAndHeal(rf)
		assertTest.NoError(err)

		assertTest.Equal(v1.HealthyState, rf.Status.State)
		stamped, parseErr := time.Parse(time.RFC3339, rf.Status.LastChanged)
		assertTest.NoError(parseErr)
		assertTest.False(stamped.Before(before.Add(-time.Second)), "LastChanged should be stamped to roughly now, got %s", rf.Status.LastChanged)
	})

	t.Run("no state transition preserves the previous LastChanged", func(t *testing.T) {
		assertTest := assert.New(t)

		const previousLastChanged = "2020-01-01T00:00:00Z"
		rf := operatorManagedRF()
		rf.Status.State = v1.HealthyState
		rf.Status.LastChanged = previousLastChanged

		config := generateConfig()
		mk := settledK8sServices()
		mk.On("UpdateRedisFailoverStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return()
		mrfs := &mRFService.RedisFailoverClient{}
		mrfc := &mRFService.RedisFailoverCheck{}
		mrfh := &mRFService.RedisFailoverHeal{}
		mrfh.On("ApplyPassword", mock.Anything, mock.Anything, mock.Anything).Maybe().Return(true, nil)
		mrfh.On("ApplySentinelPassword", mock.Anything, mock.Anything).Maybe().Return(true, nil)

		setupHealthyPass(mrfc, mrfh, rf)

		handler := rfOperator.NewRedisFailoverHandler(config, mrfs, mrfc, mrfh, mk, metrics.Dummy, log.Dummy)
		err := handler.CheckAndHeal(rf)
		assertTest.NoError(err)

		assertTest.Equal(v1.HealthyState, rf.Status.State)
		assertTest.Equal(previousLastChanged, rf.Status.LastChanged, "LastChanged must not be reset to empty on a no-op reconcile")
	})
}

// A reconcile writes the status at most once, and a steady-state reconcile
// does not write it. Each status change queues the RedisFailover again, so
// more writes make the RedisFailover reconcile without a pause.
func TestCheckAndHealSteadyStateWritesNoStatus(t *testing.T) {
	const (
		master        = "0.0.0.0"
		sentinel      = "1.1.1.1"
		bootstrapHost = "127.0.0.1"
		bootstrapPort = "6379"
	)
	healthySentinels := func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
		mrfc.On("GetSentinelsIPs", rf).Return([]string{sentinel}, nil)
		mrfc.On("CheckSentinelNumberInMemory", sentinel, rf).Return(nil)
		mrfc.On("CheckSentinelSlavesNumberInMemory", sentinel, rf).Return(nil)
		mrfh.On("SetSentinelCustomConfig", sentinel, rf).Return(nil)
	}
	bootstrapRedis := func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
		mrfc.On("IsRedisRunning", rf).Return(true)
		mrfc.On("GetRedisesIPs", rf).Return([]string{master}, nil)
		mrfh.On("SetRedisCustomConfig", master, rf).Return(nil)
		mrfc.On("CheckRedisSlavesReady", master, rf).Return(true, nil)
		mrfc.On("GetStatefulSetUpdateRevision", rf).Return("1", nil)
		mrfc.On("GetRedisesSlavesPods", rf).Return([]string{}, nil)
		mrfh.On("SetExternalMasterOnAll", bootstrapHost, bootstrapPort, rf).Return(nil)
	}

	tests := []struct {
		name          string
		bootstrapping bool
		setup         func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover)
		wantState     string
	}{
		{
			name: "sentinel mode",
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("IsRedisRunningQuorum", rf).Return(true)
				mrfc.On("IsSentinelRunningQuorum", rf).Return(true)
				mrfc.On("GetNumberMasters", rf).Return(1, nil)
				mrfc.On("GetMasterIP", rf).Return(master, nil)
				mrfc.On("CheckAllSlavesFromMaster", master, rf).Return(nil)
				mrfc.On("GetRedisesIPs", rf).Return([]string{master}, nil)
				mrfh.On("SetRedisCustomConfig", master, rf).Return(nil)
				mrfc.On("GetStatefulSetUpdateRevision", rf).Return("1", nil)
				mrfc.On("GetRedisesSlavesPods", rf).Return([]string{}, nil)
				mrfc.On("GetRedisesMasterPod", rf).Return(master, nil)
				mrfc.On("GetRedisRevisionHash", master, rf).Return("1", nil)
				mrfc.On("CheckSentinelMonitor", sentinel, master, "0").Return(nil)
				healthySentinels(mrfc, mrfh, rf)
			},
			wantState: v1.HealthyState,
		},
		{
			name:          "bootstrap mode, redis pods not running",
			bootstrapping: true,
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("IsRedisRunning", rf).Return(false)
			},
			wantState: v1.NotHealthyState,
		},
		{
			name:          "bootstrap mode, sentinels not running",
			bootstrapping: true,
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				bootstrapRedis(mrfc, mrfh, rf)
				mrfc.On("IsSentinelRunning", rf).Return(false)
			},
			wantState: v1.NotHealthyState,
		},
		{
			name:          "bootstrap mode, sentinels running",
			bootstrapping: true,
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				bootstrapRedis(mrfc, mrfh, rf)
				mrfc.On("IsSentinelRunning", rf).Return(true)
				mrfc.On("CheckSentinelMonitor", sentinel, bootstrapHost, bootstrapPort).Return(nil)
				healthySentinels(mrfc, mrfh, rf)
			},
			wantState: v1.HealthyState,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rf := generateRF(false, test.bootstrapping)
			if test.bootstrapping {
				rf.Spec.BootstrapNode.AllowSentinels = true
			}

			writes := 0
			mk := settledK8sServices()
			mk.On("UpdateRedisFailoverStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Run(func(mock.Arguments) { writes++ }).Return()
			mrfc := &mRFService.RedisFailoverCheck{}
			mrfh := &mRFService.RedisFailoverHeal{}
			mrfh.On("ApplyPassword", mock.Anything, mock.Anything, mock.Anything).Maybe().Return(true, nil)
			mrfh.On("ApplySentinelPassword", mock.Anything, mock.Anything).Maybe().Return(true, nil)
			test.setup(mrfc, mrfh, rf)
			handler := rfOperator.NewRedisFailoverHandler(generateConfig(), &mRFService.RedisFailoverClient{}, mrfc, mrfh, mk, metrics.Dummy, log.Dummy)

			assert.NoError(t, handler.CheckAndHeal(rf))
			assert.Equal(t, test.wantState, rf.Status.State)
			assert.Equal(t, 1, writes, "the first reconcile changes the status")

			writes = 0
			assert.NoError(t, handler.CheckAndHeal(rf))
			assert.Equal(t, test.wantState, rf.Status.State)
			assert.Zero(t, writes, "a steady-state reconcile writes no status")
			mrfc.AssertExpectations(t)
			mrfh.AssertExpectations(t)
		})
	}
}

// TestCheckAndHealPlainModeErrorBranches exercises early-return error
// branches of CheckAndHeal (operator/redisfailover/checker.go) in the
// "plain" (non-bootstrapping, Sentinel-managed) mode that the large
// table-driven TestCheckAndHeal above does not reach - mostly sub-call
// errors that TestCheckAndHeal's table always configures to succeed.
func TestCheckAndHealPlainModeErrorBranches(t *testing.T) {
	const (
		master   = "0.0.0.0"
		sentinel = "1.1.1.1"
		port     = "0" // getRedisPort(rf.Spec.Redis.Port) with the zero-value Port used by generateRF
		// newMaster is the master that the re-check before a heal finds,
		// because the master can change during the reconcile.
		newMaster = "2.2.2.2"
	)

	tests := []struct {
		name        string
		rfMod       func(rf *v1.RedisFailover)
		setup       func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover)
		wantErr     bool
		wantState   string
		wantMessage string
	}{
		{
			name: "redis quorum not running - waits for statefulset reconcile",
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("IsRedisRunningQuorum", rf).Once().Return(false)
			},
			wantErr:     false,
			wantState:   v1.NotHealthyState,
			wantMessage: "redis quorum not running",
		},
		{
			name: "sentinel quorum not running - waits for deployment reconcile",
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("IsRedisRunningQuorum", rf).Once().Return(true)
				mrfc.On("IsSentinelRunningQuorum", rf).Once().Return(false)
			},
			wantErr:     false,
			wantState:   v1.NotHealthyState,
			wantMessage: "sentinel quorum not running",
		},
		{
			name: "GetNumberMasters errors",
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("IsRedisRunningQuorum", rf).Once().Return(true)
				mrfc.On("IsSentinelRunningQuorum", rf).Once().Return(true)
				mrfc.On("GetNumberMasters", rf).Once().Return(0, errors.New("num masters err"))
			},
			wantErr:     true,
			wantState:   v1.NotHealthyState,
			wantMessage: "unable to get number of masters",
		},
		{
			name: "no master, single replica - SetOldestAsMaster fails",
			rfMod: func(rf *v1.RedisFailover) {
				rf.Spec.Redis.Replicas = 1
			},
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("IsRedisRunningQuorum", rf).Once().Return(true)
				mrfc.On("IsSentinelRunningQuorum", rf).Once().Return(true)
				mrfc.On("GetNumberMasters", rf).Once().Return(0, nil)
				mrfh.On("SetOldestAsMaster", rf).Once().Return(errors.New("oldest err"))
			},
			wantErr:     true,
			wantState:   v1.NotHealthyState,
			wantMessage: "Error in Setting oldest Pod as master",
		},
		{
			name: "no master - GetMaxRedisPodTime fails",
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("IsRedisRunningQuorum", rf).Once().Return(true)
				mrfc.On("IsSentinelRunningQuorum", rf).Once().Return(true)
				mrfc.On("GetNumberMasters", rf).Once().Return(0, nil)
				mrfc.On("GetMaxRedisPodTime", rf).Once().Return(time.Duration(0), errors.New("uptime err"))
			},
			wantErr:     true,
			wantState:   v1.NotHealthyState,
			wantMessage: "unable to get Redis POD time",
		},
		{
			name: "no master, no quorum - SetOldestAsMaster fails",
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("IsRedisRunningQuorum", rf).Once().Return(true)
				mrfc.On("IsSentinelRunningQuorum", rf).Once().Return(true)
				mrfc.On("GetNumberMasters", rf).Once().Return(0, nil)
				mrfc.On("GetMaxRedisPodTime", rf).Once().Return(1*time.Hour, nil)
				mrfc.On("CheckSentinelQuorum", rf).Once().Return(1, errors.New("no quorum"))
				mrfh.On("SetOldestAsMaster", rf).Once().Return(errors.New("oldest err2"))
			},
			wantErr:     true,
			wantState:   v1.NotHealthyState,
			wantMessage: "Error in Setting oldest Pod as master",
		},
		{
			name: "no master, has quorum - CheckIfMasterLocalhost fails",
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("IsRedisRunningQuorum", rf).Once().Return(true)
				mrfc.On("IsSentinelRunningQuorum", rf).Once().Return(true)
				mrfc.On("GetNumberMasters", rf).Once().Return(0, nil)
				mrfc.On("GetMaxRedisPodTime", rf).Once().Return(1*time.Hour, nil)
				mrfc.On("CheckSentinelQuorum", rf).Once().Return(3, nil)
				mrfc.On("CheckIfMasterLocalhost", rf).Once().Return(false, errors.New("localhost check err"))
			},
			wantErr:     true,
			wantState:   v1.NotHealthyState,
			wantMessage: "unable to check if master localhost",
		},
		{
			name: "no master, has quorum, localhost true - SetOldestAsMaster fails",
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("IsRedisRunningQuorum", rf).Once().Return(true)
				mrfc.On("IsSentinelRunningQuorum", rf).Once().Return(true)
				mrfc.On("GetNumberMasters", rf).Once().Return(0, nil)
				mrfc.On("GetMaxRedisPodTime", rf).Once().Return(1*time.Hour, nil)
				mrfc.On("CheckSentinelQuorum", rf).Once().Return(3, nil)
				mrfc.On("CheckIfMasterLocalhost", rf).Once().Return(true, nil)
				mrfh.On("SetOldestAsMaster", rf).Once().Return(errors.New("oldest err3"))
			},
			wantErr:     true,
			wantState:   v1.NotHealthyState,
			wantMessage: "Error in Setting oldest Pod as master",
		},
		{
			name: "single master - GetMasterIP fails",
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("IsRedisRunningQuorum", rf).Once().Return(true)
				mrfc.On("IsSentinelRunningQuorum", rf).Once().Return(true)
				mrfc.On("GetNumberMasters", rf).Once().Return(1, nil)
				mrfc.On("GetMasterIP", rf).Once().Return("", errors.New("master ip err"))
			},
			wantErr:     true,
			wantState:   v1.NotHealthyState,
			wantMessage: "unable to get master IP",
		},
		{
			name: "slaves wrong - re-verifying master IP fails",
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("IsRedisRunningQuorum", rf).Once().Return(true)
				mrfc.On("IsSentinelRunningQuorum", rf).Once().Return(true)
				mrfc.On("GetNumberMasters", rf).Once().Return(1, nil)
				mrfc.On("GetMasterIP", rf).Once().Return(master, nil)
				mrfc.On("CheckAllSlavesFromMaster", master, rf).Once().Return(errors.New("wrong master"))
				mrfc.On("GetMasterIP", rf).Once().Return("", errors.New("reverify err"))
			},
			wantErr:     true,
			wantState:   v1.NotHealthyState,
			wantMessage: "unable to re-verify master IP",
		},
		{
			name: "slaves wrong - SetMasterOnAll fails",
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("IsRedisRunningQuorum", rf).Once().Return(true)
				mrfc.On("IsSentinelRunningQuorum", rf).Once().Return(true)
				mrfc.On("GetNumberMasters", rf).Once().Return(1, nil)
				mrfc.On("GetMasterIP", rf).Once().Return(master, nil)
				mrfc.On("CheckAllSlavesFromMaster", master, rf).Once().Return(errors.New("wrong master"))
				mrfc.On("GetMasterIP", rf).Once().Return(newMaster, nil)
				mrfh.On("SetMasterOnAll", newMaster, rf).Once().Return(errors.New("set fail"))
			},
			wantErr:   true,
			wantState: v1.NotHealthyState,
			// This branch (checker.go's SetMasterOnAll error handling) sets
			// only State, not Message - unlike almost every other error
			// branch in CheckAndHeal.
			wantMessage: "",
		},
		{
			name: "applyRedisCustomConfig fails",
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("IsRedisRunningQuorum", rf).Once().Return(true)
				mrfc.On("IsSentinelRunningQuorum", rf).Once().Return(true)
				mrfc.On("GetNumberMasters", rf).Once().Return(1, nil)
				mrfc.On("GetMasterIP", rf).Once().Return(master, nil)
				mrfc.On("CheckAllSlavesFromMaster", master, rf).Once().Return(nil)
				// applyRedisCustomConfig's own GetRedisesIPs error branch.
				mrfc.On("GetRedisesIPs", rf).Once().Return(nil, errors.New("ips err"))
			},
			wantErr:     true,
			wantState:   v1.NotHealthyState,
			wantMessage: "unable to apply custom config",
		},
		{
			name: "UpdateRedisesPods fails",
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("IsRedisRunningQuorum", rf).Once().Return(true)
				mrfc.On("IsSentinelRunningQuorum", rf).Once().Return(true)
				mrfc.On("GetNumberMasters", rf).Once().Return(1, nil)
				mrfc.On("GetMasterIP", rf).Once().Return(master, nil)
				mrfc.On("CheckAllSlavesFromMaster", master, rf).Once().Return(nil)
				// First GetRedisesIPs call is applyRedisCustomConfig's (succeeds);
				// second is UpdateRedisesPods' own call (fails).
				mrfc.On("GetRedisesIPs", rf).Once().Return([]string{master}, nil)
				mrfh.On("SetRedisCustomConfig", master, rf).Once().Return(nil)
				mrfc.On("GetRedisesIPs", rf).Once().Return(nil, errors.New("update pods ips err"))
			},
			wantErr:     true,
			wantState:   v1.NotHealthyState,
			wantMessage: "unable to update redis PODs",
		},
		{
			// No UpdateRedisesPods expectations: replacing pods would panic the mock.
			name: "maxmemory holds the pod rollout",
			rfMod: func(rf *v1.RedisFailover) {
				rf.Spec.Redis.MaxMemory = &v1.MaxMemorySettings{Percent: 75, Policy: "noeviction"}
			},
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("IsRedisRunningQuorum", rf).Once().Return(true)
				mrfc.On("IsSentinelRunningQuorum", rf).Once().Return(true)
				mrfc.On("GetNumberMasters", rf).Once().Return(1, nil)
				mrfc.On("GetMasterIP", rf).Twice().Return(master, nil)
				mrfc.On("CheckAllSlavesFromMaster", master, rf).Once().Return(nil)
				mrfc.On("GetRedisesIPs", rf).Twice().Return([]string{master}, nil)
				mrfh.On("SetRedisCustomConfig", master, rf).Once().Return(nil)
				mrfh.On("EnsureRedisMaxMemory", rf, master, []string{master}).Once().Return(rfservice.MaxMemoryResult{Message: "maxmemory kept", HoldRollout: true}, nil)
				mrfc.On("GetSentinelsIPs", rf).Once().Return(nil, errors.New("sentinels ips err"))
			},
			wantErr:     true,
			wantState:   v1.NotHealthyState,
			wantMessage: "unable to get sentinels IPs",
		},
		{
			name: "GetSentinelsIPs fails",
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("IsRedisRunningQuorum", rf).Once().Return(true)
				mrfc.On("IsSentinelRunningQuorum", rf).Once().Return(true)
				mrfc.On("GetNumberMasters", rf).Once().Return(1, nil)
				mrfc.On("GetMasterIP", rf).Once().Return(master, nil)
				mrfc.On("CheckAllSlavesFromMaster", master, rf).Once().Return(nil)
				mrfc.On("GetRedisesIPs", rf).Twice().Return([]string{master}, nil)
				mrfh.On("SetRedisCustomConfig", master, rf).Once().Return(nil)
				mrfc.On("GetStatefulSetUpdateRevision", rf).Once().Return("1", nil)
				mrfc.On("GetRedisesSlavesPods", rf).Once().Return([]string{}, nil)
				mrfc.On("GetRedisesMasterPod", rf).Once().Return(master, nil)
				mrfc.On("GetRedisRevisionHash", master, rf).Once().Return("1", nil)
				// UpdateRedisesPods' own internal GetMasterIP call (it is not
				// bootstrapping, so it resolves masterIP itself, ignoring errors).
				mrfc.On("GetMasterIP", rf).Once().Return(master, nil)
				mrfc.On("GetSentinelsIPs", rf).Once().Return(nil, errors.New("sentinels ips err"))
			},
			wantErr:     true,
			wantState:   v1.NotHealthyState,
			wantMessage: "unable to get sentinels IPs",
		},
		{
			name: "sentinel monitor wrong - re-verifying master IP fails",
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("IsRedisRunningQuorum", rf).Once().Return(true)
				mrfc.On("IsSentinelRunningQuorum", rf).Once().Return(true)
				mrfc.On("GetNumberMasters", rf).Once().Return(1, nil)
				mrfc.On("GetMasterIP", rf).Once().Return(master, nil)
				mrfc.On("CheckAllSlavesFromMaster", master, rf).Once().Return(nil)
				mrfc.On("GetRedisesIPs", rf).Twice().Return([]string{master}, nil)
				mrfh.On("SetRedisCustomConfig", master, rf).Once().Return(nil)
				mrfc.On("GetStatefulSetUpdateRevision", rf).Once().Return("1", nil)
				mrfc.On("GetRedisesSlavesPods", rf).Once().Return([]string{}, nil)
				mrfc.On("GetRedisesMasterPod", rf).Once().Return(master, nil)
				mrfc.On("GetRedisRevisionHash", master, rf).Once().Return("1", nil)
				// UpdateRedisesPods' own internal GetMasterIP call.
				mrfc.On("GetMasterIP", rf).Once().Return(master, nil)
				mrfc.On("GetSentinelsIPs", rf).Once().Return([]string{sentinel}, nil)
				mrfc.On("CheckSentinelMonitor", sentinel, master, port).Once().Return(errors.New("mon err"))
				mrfc.On("GetMasterIP", rf).Once().Return("", errors.New("reverify master err"))
			},
			wantErr:     true,
			wantState:   v1.NotHealthyState,
			wantMessage: "unable to re-verify master IP",
		},
		{
			name: "sentinel monitor wrong - NewSentinelMonitor fails",
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("IsRedisRunningQuorum", rf).Once().Return(true)
				mrfc.On("IsSentinelRunningQuorum", rf).Once().Return(true)
				mrfc.On("GetNumberMasters", rf).Once().Return(1, nil)
				mrfc.On("GetMasterIP", rf).Once().Return(master, nil)
				mrfc.On("CheckAllSlavesFromMaster", master, rf).Once().Return(nil)
				mrfc.On("GetRedisesIPs", rf).Twice().Return([]string{master}, nil)
				mrfh.On("SetRedisCustomConfig", master, rf).Once().Return(nil)
				mrfc.On("GetStatefulSetUpdateRevision", rf).Once().Return("1", nil)
				mrfc.On("GetRedisesSlavesPods", rf).Once().Return([]string{}, nil)
				mrfc.On("GetRedisesMasterPod", rf).Once().Return(master, nil)
				mrfc.On("GetRedisRevisionHash", master, rf).Once().Return("1", nil)
				// UpdateRedisesPods' own internal GetMasterIP call.
				mrfc.On("GetMasterIP", rf).Once().Return(master, nil)
				mrfc.On("GetSentinelsIPs", rf).Once().Return([]string{sentinel}, nil)
				mrfc.On("CheckSentinelMonitor", sentinel, master, port).Once().Return(errors.New("mon err"))
				mrfc.On("GetMasterIP", rf).Once().Return(newMaster, nil)
				mrfc.On("CheckSentinelMonitor", sentinel, newMaster, port).Once().Return(errors.New("mon err"))
				mrfh.On("NewSentinelMonitor", sentinel, newMaster, rf).Once().Return(errors.New("new monitor err"))
			},
			wantErr:   true,
			wantState: v1.NotHealthyState,
			// Same as the SetMasterOnAll branch above: only State is set.
			wantMessage: "",
		},
		{
			// checkAndHealSentinels (called as the final statement of
			// CheckAndHeal) used to return errors without setting rf.Status,
			// unlike every preceding branch in CheckAndHeal - the HealthyState
			// set at the top would stick despite the reconcile actually
			// failing. Fixed to set NotHealthyState on each error path,
			// matching the rest of the file.
			name: "sentinel number-in-memory mismatch - RestoreSentinel fails",
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("IsRedisRunningQuorum", rf).Once().Return(true)
				mrfc.On("IsSentinelRunningQuorum", rf).Once().Return(true)
				mrfc.On("GetNumberMasters", rf).Once().Return(1, nil)
				mrfc.On("GetMasterIP", rf).Once().Return(master, nil)
				mrfc.On("CheckAllSlavesFromMaster", master, rf).Once().Return(nil)
				mrfc.On("GetRedisesIPs", rf).Twice().Return([]string{master}, nil)
				mrfh.On("SetRedisCustomConfig", master, rf).Once().Return(nil)
				mrfc.On("GetStatefulSetUpdateRevision", rf).Once().Return("1", nil)
				mrfc.On("GetRedisesSlavesPods", rf).Once().Return([]string{}, nil)
				mrfc.On("GetRedisesMasterPod", rf).Once().Return(master, nil)
				mrfc.On("GetRedisRevisionHash", master, rf).Once().Return("1", nil)
				// UpdateRedisesPods' own internal GetMasterIP call.
				mrfc.On("GetMasterIP", rf).Once().Return(master, nil)
				mrfc.On("GetSentinelsIPs", rf).Once().Return([]string{sentinel}, nil)
				mrfc.On("CheckSentinelMonitor", sentinel, master, port).Once().Return(nil)
				mrfc.On("CheckSentinelNumberInMemory", sentinel, rf).Once().Return(errors.New("mismatch"))
				mrfh.On("RestoreSentinel", sentinel).Once().Return(errors.New("restore err"))
			},
			wantErr:     true,
			wantState:   v1.NotHealthyState,
			wantMessage: "unable to restore sentinel",
		},
		{
			name: "sentinel slaves-number-in-memory mismatch - RestoreSentinel fails",
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("IsRedisRunningQuorum", rf).Once().Return(true)
				mrfc.On("IsSentinelRunningQuorum", rf).Once().Return(true)
				mrfc.On("GetNumberMasters", rf).Once().Return(1, nil)
				mrfc.On("GetMasterIP", rf).Once().Return(master, nil)
				mrfc.On("CheckAllSlavesFromMaster", master, rf).Once().Return(nil)
				mrfc.On("GetRedisesIPs", rf).Twice().Return([]string{master}, nil)
				mrfh.On("SetRedisCustomConfig", master, rf).Once().Return(nil)
				mrfc.On("GetStatefulSetUpdateRevision", rf).Once().Return("1", nil)
				mrfc.On("GetRedisesSlavesPods", rf).Once().Return([]string{}, nil)
				mrfc.On("GetRedisesMasterPod", rf).Once().Return(master, nil)
				mrfc.On("GetRedisRevisionHash", master, rf).Once().Return("1", nil)
				// UpdateRedisesPods' own internal GetMasterIP call.
				mrfc.On("GetMasterIP", rf).Once().Return(master, nil)
				mrfc.On("GetSentinelsIPs", rf).Once().Return([]string{sentinel}, nil)
				mrfc.On("CheckSentinelMonitor", sentinel, master, port).Once().Return(nil)
				mrfc.On("CheckSentinelNumberInMemory", sentinel, rf).Once().Return(nil)
				mrfc.On("CheckSentinelSlavesNumberInMemory", sentinel, rf).Once().Return(errors.New("mismatch"))
				mrfh.On("RestoreSentinel", sentinel).Once().Return(errors.New("restore err"))
			},
			wantErr:     true,
			wantState:   v1.NotHealthyState,
			wantMessage: "unable to restore sentinel",
		},
		{
			name: "SetSentinelCustomConfig fails",
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("IsRedisRunningQuorum", rf).Once().Return(true)
				mrfc.On("IsSentinelRunningQuorum", rf).Once().Return(true)
				mrfc.On("GetNumberMasters", rf).Once().Return(1, nil)
				mrfc.On("GetMasterIP", rf).Once().Return(master, nil)
				mrfc.On("CheckAllSlavesFromMaster", master, rf).Once().Return(nil)
				mrfc.On("GetRedisesIPs", rf).Twice().Return([]string{master}, nil)
				mrfh.On("SetRedisCustomConfig", master, rf).Once().Return(nil)
				mrfc.On("GetStatefulSetUpdateRevision", rf).Once().Return("1", nil)
				mrfc.On("GetRedisesSlavesPods", rf).Once().Return([]string{}, nil)
				mrfc.On("GetRedisesMasterPod", rf).Once().Return(master, nil)
				mrfc.On("GetRedisRevisionHash", master, rf).Once().Return("1", nil)
				// UpdateRedisesPods' own internal GetMasterIP call.
				mrfc.On("GetMasterIP", rf).Once().Return(master, nil)
				mrfc.On("GetSentinelsIPs", rf).Once().Return([]string{sentinel}, nil)
				mrfc.On("CheckSentinelMonitor", sentinel, master, port).Once().Return(nil)
				mrfc.On("CheckSentinelNumberInMemory", sentinel, rf).Once().Return(nil)
				mrfc.On("CheckSentinelSlavesNumberInMemory", sentinel, rf).Once().Return(nil)
				mrfh.On("SetSentinelCustomConfig", sentinel, rf).Once().Return(errors.New("set config err"))
			},
			wantErr:     true,
			wantState:   v1.NotHealthyState,
			wantMessage: "unable to set sentinel custom config",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertTest := assert.New(t)

			rf := generateRF(false, false)
			if test.rfMod != nil {
				test.rfMod(rf)
			}

			config := generateConfig()
			mk := settledK8sServices()
			// CheckAndHeal always defers updateStatus, on every return path.
			mk.On("UpdateRedisFailoverStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return()
			mrfs := &mRFService.RedisFailoverClient{}
			mrfc := &mRFService.RedisFailoverCheck{}
			mrfh := &mRFService.RedisFailoverHeal{}
			mrfh.On("ApplyPassword", mock.Anything, mock.Anything, mock.Anything).Maybe().Return(true, nil)
			mrfh.On("ApplySentinelPassword", mock.Anything, mock.Anything).Maybe().Return(true, nil)

			test.setup(mrfc, mrfh, rf)

			handler := rfOperator.NewRedisFailoverHandler(config, mrfs, mrfc, mrfh, mk, metrics.Dummy, log.Dummy)
			err := handler.CheckAndHeal(rf)

			if test.wantErr {
				assertTest.Error(err)
			} else {
				assertTest.NoError(err)
			}
			assertTest.Equal(test.wantState, rf.Status.State)
			if test.wantMessage != "" {
				assertTest.Equal(test.wantMessage, rf.Status.Message)
			}

			mrfc.AssertExpectations(t)
			mrfh.AssertExpectations(t)
		})
	}
}

// TestSentinelModeElectsWhenSentinelKnowsNoReplica covers a Sentinel that
// cannot fail over: it monitors a master that is gone and knows no replica.
// The operator promotes the best replica, and the Sentinels then monitor it.
func TestSentinelModeElectsWhenSentinelKnowsNoReplica(t *testing.T) {
	const (
		newMaster = "10.0.0.2"
		sentinel  = "1.1.1.1"
		port      = "0"
	)
	best := &rfservice.ReplicaInfo{IP: newMaster, PodName: "rfr-test-1", PodReady: true}
	tests := []struct {
		name string
		// setup mocks the calls after CheckIfMasterLocalhost.
		setup func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover)
		// secondPods and secondErr are the pod list of the check for a
		// stopping master right before the election.
		secondPods  *corev1.PodList
		secondErr   error
		wantErr     bool
		wantPromote bool
		wantMessage string
	}{
		{
			name: "no Sentinel knows a replica: promote the best replica and monitor it",
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("CheckSentinelsCannotFailover", rf).Once().Return(true, nil)
				mrfc.On("GetNumberMasters", rf).Once().Return(0, nil)
				mrfc.On("GetBestReplicaForPromotion", rf).Once().Return(best, nil)
				mrfh.On("PromoteBestReplica", newMaster, rf).Once().Return(nil)
				// The checks after the election find the new master.
				mrfc.On("GetMasterIP", rf).Times(3).Return(newMaster, nil)
				mrfc.On("CheckAllSlavesFromMaster", newMaster, rf).Once().Return(nil)
				mrfc.On("GetRedisesIPs", rf).Twice().Return([]string{newMaster}, nil)
				mrfh.On("SetRedisCustomConfig", newMaster, rf).Once().Return(nil)
				mrfc.On("GetStatefulSetUpdateRevision", rf).Once().Return("1", nil)
				mrfc.On("GetRedisesSlavesPods", rf).Once().Return([]string{}, nil)
				mrfc.On("GetRedisesMasterPod", rf).Once().Return(newMaster, nil)
				mrfc.On("GetRedisRevisionHash", newMaster, rf).Once().Return("1", nil)
				mrfc.On("GetSentinelsIPs", rf).Once().Return([]string{sentinel}, nil)
				mrfc.On("CheckSentinelMonitor", sentinel, newMaster, port).Once().Return(errors.New("monitors 10.0.0.9"))
				mrfh.On("NewSentinelMonitor", sentinel, newMaster, rf).Once().Return(nil)
				mrfc.On("CheckSentinelNumberInMemory", sentinel, rf).Once().Return(nil)
				mrfc.On("CheckSentinelSlavesNumberInMemory", sentinel, rf).Once().Return(nil)
				mrfh.On("SetSentinelCustomConfig", sentinel, rf).Once().Return(nil)
			},
			wantPromote: true,
			wantMessage: "Sentinel knew no replica to fail over to, the operator promoted rfr-test-1",
		},
		{
			name: "a Sentinel can fail over: wait",
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("CheckSentinelsCannotFailover", rf).Once().Return(false, nil)
			},
			wantMessage: "no master, waiting for the Sentinel failover",
		},
		{
			name: "the Sentinel check fails",
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("CheckSentinelsCannotFailover", rf).Once().Return(false, errors.New("list err"))
			},
			wantErr:     true,
			wantMessage: "unable to check whether Sentinel can fail over",
		},
		{
			name: "a master appeared before the election: no promotion",
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("CheckSentinelsCannotFailover", rf).Once().Return(true, nil)
				mrfc.On("GetNumberMasters", rf).Once().Return(1, nil)
			},
			wantMessage: "a master appeared before the election, checking again",
		},
		{
			name: "the master pod started to stop before the election: no promotion",
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("CheckSentinelsCannotFailover", rf).Once().Return(true, nil)
				mrfc.On("GetNumberMasters", rf).Once().Return(0, nil)
			},
			secondPods:  &corev1.PodList{Items: []corev1.Pod{masterPod(redisPod("1", true, true))}},
			wantMessage: "no master, waiting for the stopping master pod to exit",
		},
		{
			name: "the second check for a stopping master fails",
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("CheckSentinelsCannotFailover", rf).Once().Return(true, nil)
				mrfc.On("GetNumberMasters", rf).Once().Return(0, nil)
			},
			secondErr:   errors.New("list err"),
			wantErr:     true,
			wantMessage: "unable to check whether the master is stopping",
		},
		{
			name: "the second count of the masters fails",
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("CheckSentinelsCannotFailover", rf).Once().Return(true, nil)
				mrfc.On("GetNumberMasters", rf).Once().Return(0, errors.New("count err"))
			},
			wantErr:     true,
			wantMessage: "unable to get number of masters",
		},
		{
			name: "no replica is available for promotion",
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("CheckSentinelsCannotFailover", rf).Once().Return(true, nil)
				mrfc.On("GetNumberMasters", rf).Once().Return(0, nil)
				mrfc.On("GetBestReplicaForPromotion", rf).Once().Return(nil, errors.New("no replicas available for promotion"))
			},
			wantErr:     true,
			wantMessage: "Sentinel cannot fail over and no replica is available for promotion",
		},
		{
			name: "the promotion fails",
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("CheckSentinelsCannotFailover", rf).Once().Return(true, nil)
				mrfc.On("GetNumberMasters", rf).Once().Return(0, nil)
				mrfc.On("GetBestReplicaForPromotion", rf).Once().Return(best, nil)
				mrfh.On("PromoteBestReplica", newMaster, rf).Once().Return(errors.New("replicaof err"))
			},
			wantErr:     true,
			wantPromote: true,
			wantMessage: "Sentinel cannot fail over and the promotion of a replica failed",
		},
		{
			name: "a replica does not follow the new master",
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("CheckSentinelsCannotFailover", rf).Once().Return(true, nil)
				mrfc.On("GetNumberMasters", rf).Once().Return(0, nil)
				mrfc.On("GetBestReplicaForPromotion", rf).Once().Return(best, nil)
				mrfh.On("PromoteBestReplica", newMaster, rf).Once().Return(fmt.Errorf("reconfigure: %w", rfservice.ErrPartialReconciliation))
			},
			wantErr:     true,
			wantPromote: true,
			wantMessage: "failover incomplete: replica reconfiguration failed",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rf := generateRF(false, false)
			mk := &mK8SService.Services{}
			lists := 0
			mk.On("GetStatefulSetPods", mock.Anything, mock.Anything).Return(func(string, string) (*corev1.PodList, error) {
				lists++
				if lists == 2 && (test.secondPods != nil || test.secondErr != nil) {
					return test.secondPods, test.secondErr
				}
				return &corev1.PodList{Items: make([]corev1.Pod, 5)}, nil
			})
			mk.On("UpdateRedisFailoverStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return()
			mrfc := &mRFService.RedisFailoverCheck{}
			mrfh := &mRFService.RedisFailoverHeal{}
			mrfh.On("ApplyPassword", mock.Anything, mock.Anything, mock.Anything).Maybe().Return(true, nil)
			mrfh.On("ApplySentinelPassword", mock.Anything, mock.Anything).Maybe().Return(true, nil)
			mrfc.On("IsRedisRunningQuorum", rf).Once().Return(true)
			mrfc.On("IsSentinelRunningQuorum", rf).Once().Return(true)
			mrfc.On("GetNumberMasters", rf).Once().Return(0, nil)
			mrfc.On("GetMaxRedisPodTime", rf).Once().Return(time.Hour, nil)
			// Sentinel has a quorum, and not every pod replicates from localhost.
			mrfc.On("CheckSentinelQuorum", rf).Once().Return(0, nil)
			mrfc.On("CheckIfMasterLocalhost", rf).Once().Return(false, nil)
			test.setup(mrfc, mrfh, rf)

			handler := rfOperator.NewRedisFailoverHandler(generateConfig(), &mRFService.RedisFailoverClient{}, mrfc, mrfh, mk, metrics.Dummy, log.Dummy)
			err := handler.CheckAndHeal(rf)

			if test.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
			assert.Equal(t, v1.NotHealthyState, rf.Status.State)
			assert.Equal(t, test.wantMessage, rf.Status.Message)
			if !test.wantPromote {
				mrfh.AssertNotCalled(t, "PromoteBestReplica", mock.Anything, mock.Anything)
			}
			mrfh.AssertNotCalled(t, "SetOldestAsMaster", mock.Anything)
			mrfc.AssertExpectations(t)
			mrfh.AssertExpectations(t)
		})
	}
}

// TestCheckAndHealSentinelMonitorUsesRefreshedMaster checks that after a
// master refresh, the Sentinels are checked against the new master. A new
// monitor for a Sentinel that already monitors the new master resets its state.
func TestCheckAndHealSentinelMonitorUsesRefreshedMaster(t *testing.T) {
	const (
		oldMaster = "0.0.0.0"
		newMaster = "0.0.0.1"
		sentinel1 = "1.1.1.1"
		sentinel2 = "1.1.1.2"
		port      = "0"
	)
	tests := []struct {
		name string
		// sentinel1Err is the result of the check of sentinel1 against the new master.
		sentinel1Err    error
		wantNewMonitor1 bool
	}{
		{name: "the first Sentinel monitors the new master"},
		{name: "the first Sentinel monitors another master", sentinel1Err: errors.New("monitors 0.0.0.2"), wantNewMonitor1: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			rf := generateRF(false, false)

			mk := settledK8sServices()
			mk.On("UpdateRedisFailoverStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return()
			mrfc := &mRFService.RedisFailoverCheck{}
			mrfh := &mRFService.RedisFailoverHeal{}
			mrfh.On("ApplyPassword", mock.Anything, mock.Anything, mock.Anything).Maybe().Return(true, nil)
			mrfh.On("ApplySentinelPassword", mock.Anything, mock.Anything).Maybe().Return(true, nil)

			mrfc.On("IsRedisRunningQuorum", rf).Once().Return(true)
			mrfc.On("IsSentinelRunningQuorum", rf).Once().Return(true)
			mrfc.On("GetNumberMasters", rf).Once().Return(1, nil)
			mrfc.On("GetMasterIP", rf).Once().Return(oldMaster, nil)
			mrfc.On("CheckAllSlavesFromMaster", oldMaster, rf).Once().Return(nil)
			mrfc.On("GetRedisesIPs", rf).Twice().Return([]string{oldMaster}, nil)
			mrfh.On("SetRedisCustomConfig", oldMaster, rf).Once().Return(nil)
			mrfc.On("GetStatefulSetUpdateRevision", rf).Once().Return("1", nil)
			mrfc.On("GetRedisesSlavesPods", rf).Once().Return([]string{}, nil)
			mrfc.On("GetRedisesMasterPod", rf).Once().Return(oldMaster, nil)
			mrfc.On("GetRedisRevisionHash", oldMaster, rf).Once().Return("1", nil)
			// UpdateRedisesPods resolves the master itself.
			mrfc.On("GetMasterIP", rf).Once().Return(oldMaster, nil)
			mrfc.On("GetSentinelsIPs", rf).Once().Return([]string{sentinel1, sentinel2}, nil)
			// The master changed before the Sentinel checks.
			mrfc.On("CheckSentinelMonitor", sentinel1, oldMaster, port).Once().Return(errors.New("monitors " + newMaster))
			mrfc.On("GetMasterIP", rf).Once().Return(newMaster, nil)
			mrfc.On("CheckSentinelMonitor", sentinel1, newMaster, port).Once().Return(test.sentinel1Err)
			if test.wantNewMonitor1 {
				mrfh.On("NewSentinelMonitor", sentinel1, newMaster, rf).Once().Return(nil)
			}
			mrfc.On("CheckSentinelMonitor", sentinel2, newMaster, port).Once().Return(nil)
			for _, s := range []string{sentinel1, sentinel2} {
				mrfc.On("CheckSentinelNumberInMemory", s, rf).Once().Return(nil)
				mrfc.On("CheckSentinelSlavesNumberInMemory", s, rf).Once().Return(nil)
				mrfh.On("SetSentinelCustomConfig", s, rf).Once().Return(nil)
			}

			handler := rfOperator.NewRedisFailoverHandler(generateConfig(), &mRFService.RedisFailoverClient{}, mrfc, mrfh, mk, metrics.Dummy, log.Dummy)
			assert.NoError(handler.CheckAndHeal(rf))

			if !test.wantNewMonitor1 {
				mrfh.AssertNotCalled(t, "NewSentinelMonitor", sentinel1, mock.Anything, mock.Anything)
			}
			mrfh.AssertNotCalled(t, "NewSentinelMonitor", sentinel2, mock.Anything, mock.Anything)
			mrfc.AssertExpectations(t)
			mrfh.AssertExpectations(t)
		})
	}
}

// TestCheckAndHealBootstrapModeErrorBranches exercises early-return error
// branches of checkAndHealBootstrapMode (operator/redisfailover/checker.go)
// not already covered by the "Bootstrapping Mode..." cases in the
// TestCheckAndHeal table above.
func TestCheckAndHealBootstrapModeErrorBranches(t *testing.T) {
	const (
		bootstrapMaster     = "127.0.0.1"
		bootstrapMasterPort = "6379"
		sentinel            = "1.1.1.1"
	)

	// setupUpdateAndConfigSuccess wires up a full, successful pass through
	// UpdateRedisesPods and applyRedisCustomConfig for the given redis IPs,
	// as checkAndHealBootstrapMode calls them (masterIP stays "" while
	// bootstrapping, so every IP is treated as a slave needing a
	// CheckRedisSlavesReady call).
	setupUpdateAndConfigSuccess := func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover, ips []string) {
		mrfc.On("GetRedisesIPs", rf).Twice().Return(ips, nil)
		for _, ip := range ips {
			mrfc.On("CheckRedisSlavesReady", ip, rf).Once().Return(true, nil)
			mrfh.On("SetRedisCustomConfig", ip, rf).Once().Return(nil)
		}
		mrfc.On("GetStatefulSetUpdateRevision", rf).Once().Return("1", nil)
		mrfc.On("GetRedisesSlavesPods", rf).Once().Return([]string{}, nil)
	}

	tests := []struct {
		name           string
		allowSentinels bool
		setup          func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover)
		wantErr        bool
		wantState      string
		wantMessage    string
	}{
		{
			// checkAndHealBootstrapMode's UpdateRedisesPods error handling
			// used to set rf.Status to NotHealthyState without a `return
			// err` afterwards, unlike every other error branch in this
			// file - execution fell through into applyRedisCustomConfig
			// and beyond, so a real failure here could be swallowed and
			// reported as success. Fixed to return immediately, matching
			// every other branch; this now asserts the fixed behavior.
			name: "UpdateRedisesPods fails",
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("IsRedisRunning", rf).Once().Return(true)
				mrfc.On("GetRedisesIPs", rf).Once().Return(nil, errors.New("update pods ips err"))
			},
			wantErr:     true,
			wantState:   v1.NotHealthyState,
			wantMessage: "unable to update Redis PODs",
		},
		{
			name: "applyRedisCustomConfig fails",
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("IsRedisRunning", rf).Once().Return(true)
				ips := []string{"1.1.1.1", "1.1.1.2", "1.1.1.3"}
				mrfc.On("GetRedisesIPs", rf).Twice().Return(ips, nil)
				for _, ip := range ips {
					mrfc.On("CheckRedisSlavesReady", ip, rf).Once().Return(true, nil)
				}
				mrfc.On("GetStatefulSetUpdateRevision", rf).Once().Return("1", nil)
				mrfc.On("GetRedisesSlavesPods", rf).Once().Return([]string{}, nil)
				mrfh.On("SetRedisCustomConfig", "1.1.1.1", rf).Once().Return(errors.New("cfg err"))
			},
			wantErr:     true,
			wantState:   v1.NotHealthyState,
			wantMessage: "unable to set Redis custom config",
		},
		{
			name: "EnsureRedisMaxMemory fails",
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				rf.Spec.Redis.MaxMemory = &v1.MaxMemorySettings{Percent: 75, Policy: "noeviction"}
				mrfc.On("IsRedisRunning", rf).Once().Return(true)
				mrfc.On("GetRedisesIPs", rf).Once().Return([]string{bootstrapMaster}, nil)
				mrfh.On("EnsureRedisMaxMemory", rf, "", []string{bootstrapMaster}).Once().Return(rfservice.MaxMemoryResult{}, errors.New("maxmemory err"))
			},
			wantErr:     true,
			wantState:   v1.NotHealthyState,
			wantMessage: "unable to set Redis maxmemory",
		},
		{
			// No UpdateRedisesPods expectations: replacing pods would panic the mock.
			name: "maxmemory holds the pod rollout",
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				rf.Spec.Redis.MaxMemory = &v1.MaxMemorySettings{Percent: 75, Policy: "noeviction"}
				mrfc.On("IsRedisRunning", rf).Once().Return(true)
				mrfc.On("GetRedisesIPs", rf).Twice().Return([]string{bootstrapMaster}, nil)
				mrfh.On("EnsureRedisMaxMemory", rf, "", []string{bootstrapMaster}).Once().Return(rfservice.MaxMemoryResult{Message: "maxmemory kept", HoldRollout: true}, nil)
				mrfh.On("SetRedisCustomConfig", bootstrapMaster, rf).Once().Return(nil)
				mrfh.On("SetExternalMasterOnAll", bootstrapMaster, bootstrapMasterPort, rf).Once().Return(nil)
			},
			wantErr:     false,
			wantState:   v1.HealthyState,
			wantMessage: "maxmemory kept",
		},
		{
			name:           "sentinels allowed but not running",
			allowSentinels: true,
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("IsRedisRunning", rf).Once().Return(true)
				setupUpdateAndConfigSuccess(mrfc, mrfh, rf, []string{bootstrapMaster})
				mrfh.On("SetExternalMasterOnAll", bootstrapMaster, bootstrapMasterPort, rf).Once().Return(nil)
				mrfc.On("IsSentinelRunning", rf).Once().Return(false)
			},
			wantErr:     false,
			wantState:   v1.NotHealthyState,
			wantMessage: "not all replicas running",
		},
		{
			name:           "sentinels allowed - GetSentinelsIPs fails",
			allowSentinels: true,
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("IsRedisRunning", rf).Once().Return(true)
				setupUpdateAndConfigSuccess(mrfc, mrfh, rf, []string{bootstrapMaster})
				mrfh.On("SetExternalMasterOnAll", bootstrapMaster, bootstrapMasterPort, rf).Once().Return(nil)
				mrfc.On("IsSentinelRunning", rf).Once().Return(true)
				mrfc.On("GetSentinelsIPs", rf).Once().Return(nil, errors.New("sentinels ips err"))
			},
			wantErr:     true,
			wantState:   v1.NotHealthyState,
			wantMessage: "unable to get sentinels IPs",
		},
		{
			name:           "sentinels allowed - NewSentinelMonitorWithPort fails",
			allowSentinels: true,
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("IsRedisRunning", rf).Once().Return(true)
				setupUpdateAndConfigSuccess(mrfc, mrfh, rf, []string{bootstrapMaster})
				mrfh.On("SetExternalMasterOnAll", bootstrapMaster, bootstrapMasterPort, rf).Once().Return(nil)
				mrfc.On("IsSentinelRunning", rf).Once().Return(true)
				mrfc.On("GetSentinelsIPs", rf).Once().Return([]string{sentinel}, nil)
				mrfc.On("CheckSentinelMonitor", sentinel, bootstrapMaster, bootstrapMasterPort).Once().Return(errors.New("mon err"))
				mrfh.On("NewSentinelMonitorWithPort", sentinel, bootstrapMaster, bootstrapMasterPort, rf).Once().Return(errors.New("new monitor err"))
			},
			wantErr:     true,
			wantState:   v1.NotHealthyState,
			wantMessage: "unable to check sentinel monitor",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertTest := assert.New(t)

			rf := generateRF(false, true)
			rf.Spec.BootstrapNode.AllowSentinels = test.allowSentinels

			config := generateConfig()
			mk := settledK8sServices()
			// CheckAndHeal always defers updateStatus, on every return path.
			mk.On("UpdateRedisFailoverStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return()
			mrfs := &mRFService.RedisFailoverClient{}
			mrfc := &mRFService.RedisFailoverCheck{}
			mrfh := &mRFService.RedisFailoverHeal{}
			mrfh.On("ApplyPassword", mock.Anything, mock.Anything, mock.Anything).Maybe().Return(true, nil)
			mrfh.On("ApplySentinelPassword", mock.Anything, mock.Anything).Maybe().Return(true, nil)

			test.setup(mrfc, mrfh, rf)

			handler := rfOperator.NewRedisFailoverHandler(config, mrfs, mrfc, mrfh, mk, metrics.Dummy, log.Dummy)
			err := handler.CheckAndHeal(rf)

			if test.wantErr {
				assertTest.Error(err)
			} else {
				assertTest.NoError(err)
			}
			assertTest.Equal(test.wantState, rf.Status.State)
			if test.wantMessage != "" {
				assertTest.Equal(test.wantMessage, rf.Status.Message)
			}

			mrfc.AssertExpectations(t)
			mrfh.AssertExpectations(t)
		})
	}
}

func TestUpdate(t *testing.T) {
	type podStatus struct {
		pod    corev1.Pod
		ready  bool
		master bool
	}
	tests := []struct {
		name          string
		pods          []podStatus
		ssVersion     string
		errExpected   bool
		bootstrapping bool
		noMaster      bool
		// sentinelSlavesShort makes the sentinels report fewer slaves than
		// expected, so the master must not be replaced yet.
		sentinelSlavesShort bool
	}{
		{
			name: "all ok, no change needed",
			pods: []podStatus{
				{
					pod: corev1.Pod{
						ObjectMeta: metav1.ObjectMeta{
							Name: "slave1",
							Labels: map[string]string{
								appsv1.ControllerRevisionHashLabelKey: "10",
							},
						},
						Status: corev1.PodStatus{
							PodIP: "0.0.0.0",
						},
					},
					master: false,
					ready:  true,
				},
				{
					pod: corev1.Pod{
						ObjectMeta: metav1.ObjectMeta{
							Name: "slave2",
							Labels: map[string]string{
								appsv1.ControllerRevisionHashLabelKey: "10",
							},
						},
						Status: corev1.PodStatus{
							PodIP: "0.0.0.1",
						},
					},
					master: false,
					ready:  true,
				},
				{
					pod: corev1.Pod{
						ObjectMeta: metav1.ObjectMeta{
							Name: "master",
							Labels: map[string]string{
								appsv1.ControllerRevisionHashLabelKey: "10",
							},
						},
						Status: corev1.PodStatus{
							PodIP: "1.1.1.1",
						},
					},
					master: true,
					ready:  true,
				},
			},
			ssVersion:     "10",
			errExpected:   false,
			bootstrapping: false,
		},
		{
			name: "syncing",
			pods: []podStatus{
				{
					pod: corev1.Pod{
						ObjectMeta: metav1.ObjectMeta{
							Name: "slave1",
							Labels: map[string]string{
								appsv1.ControllerRevisionHashLabelKey: "10",
							},
						},
						Status: corev1.PodStatus{
							PodIP: "0.0.0.0",
						},
					},
					master: false,
					ready:  true,
				},
				{
					pod: corev1.Pod{
						ObjectMeta: metav1.ObjectMeta{
							Name: "slave2",
							Labels: map[string]string{
								appsv1.ControllerRevisionHashLabelKey: "10",
							},
						},
						Status: corev1.PodStatus{
							PodIP: "0.0.0.1",
						},
					},
					master: false,
					ready:  false,
				},
				{
					pod: corev1.Pod{
						ObjectMeta: metav1.ObjectMeta{
							Name: "master",
							Labels: map[string]string{
								appsv1.ControllerRevisionHashLabelKey: "10",
							},
						},
						Status: corev1.PodStatus{
							PodIP: "1.1.1.1",
						},
					},
					master: true,
					ready:  true,
				},
			},
			ssVersion:     "10",
			errExpected:   false,
			bootstrapping: false,
		},
		{
			name: "pod version incorrect",
			pods: []podStatus{
				{
					pod: corev1.Pod{
						ObjectMeta: metav1.ObjectMeta{
							Name: "slave1",
							Labels: map[string]string{
								appsv1.ControllerRevisionHashLabelKey: "10",
							},
						},
						Status: corev1.PodStatus{
							PodIP: "0.0.0.0",
						},
					},
					master: false,
					ready:  true,
				},
				{
					pod: corev1.Pod{
						ObjectMeta: metav1.ObjectMeta{
							Name: "slave2",
							Labels: map[string]string{
								appsv1.ControllerRevisionHashLabelKey: "10",
							},
						},
						Status: corev1.PodStatus{
							PodIP: "0.0.0.1",
						},
					},
					master: false,
					ready:  true,
				},
				{
					pod: corev1.Pod{
						ObjectMeta: metav1.ObjectMeta{
							Name: "master",
							Labels: map[string]string{
								appsv1.ControllerRevisionHashLabelKey: "10",
							},
						},
						Status: corev1.PodStatus{
							PodIP: "1.1.1.1",
						},
					},
					master: true,
					ready:  true,
				},
			},
			ssVersion:     "1",
			errExpected:   false,
			bootstrapping: false,
		},
		{
			name: "master version incorrect",
			pods: []podStatus{
				{
					pod: corev1.Pod{
						ObjectMeta: metav1.ObjectMeta{
							Name: "slave1",
							Labels: map[string]string{
								appsv1.ControllerRevisionHashLabelKey: "10",
							},
						},
						Status: corev1.PodStatus{
							PodIP: "0.0.0.0",
						},
					},
					master: false,
					ready:  true,
				},
				{
					pod: corev1.Pod{
						ObjectMeta: metav1.ObjectMeta{
							Name: "slave2",
							Labels: map[string]string{
								appsv1.ControllerRevisionHashLabelKey: "10",
							},
						},
						Status: corev1.PodStatus{
							PodIP: "0.0.0.1",
						},
					},
					master: false,
					ready:  true,
				},
				{
					pod: corev1.Pod{
						ObjectMeta: metav1.ObjectMeta{
							Name: "master",
							Labels: map[string]string{
								appsv1.ControllerRevisionHashLabelKey: "1",
							},
						},
						Status: corev1.PodStatus{
							PodIP: "1.1.1.1",
						},
					},
					master: true,
					ready:  true,
				},
			},
			ssVersion:     "10",
			errExpected:   false,
			bootstrapping: false,
		},
		{
			// Master is stale, all slaves are redis-ready, but the sentinels have
			// not discovered the slaves yet. The master must not be replaced or the
			// failover it triggers would fail with NOGOODSLAVE.
			name: "master version incorrect but sentinels lack slaves",
			pods: []podStatus{
				{
					pod: corev1.Pod{
						ObjectMeta: metav1.ObjectMeta{
							Name:   "slave1",
							Labels: map[string]string{appsv1.ControllerRevisionHashLabelKey: "10"},
						},
						Status: corev1.PodStatus{PodIP: "0.0.0.0"},
					},
					master: false,
					ready:  true,
				},
				{
					pod: corev1.Pod{
						ObjectMeta: metav1.ObjectMeta{
							Name:   "slave2",
							Labels: map[string]string{appsv1.ControllerRevisionHashLabelKey: "10"},
						},
						Status: corev1.PodStatus{PodIP: "0.0.0.1"},
					},
					master: false,
					ready:  true,
				},
				{
					pod: corev1.Pod{
						ObjectMeta: metav1.ObjectMeta{
							Name:   "master",
							Labels: map[string]string{appsv1.ControllerRevisionHashLabelKey: "1"},
						},
						Status: corev1.PodStatus{PodIP: "1.1.1.1"},
					},
					master: true,
					ready:  true,
				},
			},
			ssVersion:           "10",
			errExpected:         false,
			bootstrapping:       false,
			sentinelSlavesShort: true,
		},
		{
			name: "all ok, no change needed when in bootstrap mode",
			pods: []podStatus{
				{
					pod: corev1.Pod{
						ObjectMeta: metav1.ObjectMeta{
							Name: "slave1",
							Labels: map[string]string{
								appsv1.ControllerRevisionHashLabelKey: "10",
							},
						},
						Status: corev1.PodStatus{
							PodIP: "0.0.0.0",
						},
					},
					master: false,
					ready:  true,
				},
				{
					pod: corev1.Pod{
						ObjectMeta: metav1.ObjectMeta{
							Name: "slave2",
							Labels: map[string]string{
								appsv1.ControllerRevisionHashLabelKey: "10",
							},
						},
						Status: corev1.PodStatus{
							PodIP: "0.0.0.1",
						},
					},
					master: false,
					ready:  true,
				},
				{
					pod: corev1.Pod{
						ObjectMeta: metav1.ObjectMeta{
							Name: "slave3",
							Labels: map[string]string{
								appsv1.ControllerRevisionHashLabelKey: "10",
							},
						},
						Status: corev1.PodStatus{
							PodIP: "1.1.1.1",
						},
					},
					master: false,
					ready:  true,
				},
			},
			ssVersion:     "10",
			errExpected:   false,
			bootstrapping: true,
		},
		{
			name: "syncing when in bootstrap mode",
			pods: []podStatus{
				{
					pod: corev1.Pod{
						ObjectMeta: metav1.ObjectMeta{
							Name: "slave1",
							Labels: map[string]string{
								appsv1.ControllerRevisionHashLabelKey: "10",
							},
						},
						Status: corev1.PodStatus{
							PodIP: "0.0.0.0",
						},
					},
					master: false,
					ready:  true,
				},
				{
					pod: corev1.Pod{
						ObjectMeta: metav1.ObjectMeta{
							Name: "slave2",
							Labels: map[string]string{
								appsv1.ControllerRevisionHashLabelKey: "10",
							},
						},
						Status: corev1.PodStatus{
							PodIP: "0.0.0.1",
						},
					},
					master: false,
					ready:  false,
				},
				{
					pod: corev1.Pod{
						ObjectMeta: metav1.ObjectMeta{
							Name: "slave3",
							Labels: map[string]string{
								appsv1.ControllerRevisionHashLabelKey: "10",
							},
						},
						Status: corev1.PodStatus{
							PodIP: "1.1.1.1",
						},
					},
					master: false,
					ready:  true,
				},
			},
			ssVersion:     "10",
			errExpected:   false,
			bootstrapping: true,
		},
		{
			name: "pod version incorrect when in bootstrap mode",
			pods: []podStatus{
				{
					pod: corev1.Pod{
						ObjectMeta: metav1.ObjectMeta{
							Name: "slave1",
							Labels: map[string]string{
								appsv1.ControllerRevisionHashLabelKey: "10",
							},
						},
						Status: corev1.PodStatus{
							PodIP: "0.0.0.0",
						},
					},
					master: false,
					ready:  true,
				},
				{
					pod: corev1.Pod{
						ObjectMeta: metav1.ObjectMeta{
							Name: "slave2",
							Labels: map[string]string{
								appsv1.ControllerRevisionHashLabelKey: "10",
							},
						},
						Status: corev1.PodStatus{
							PodIP: "0.0.0.1",
						},
					},
					master: false,
					ready:  true,
				},
				{
					pod: corev1.Pod{
						ObjectMeta: metav1.ObjectMeta{
							Name: "slave3",
							Labels: map[string]string{
								appsv1.ControllerRevisionHashLabelKey: "10",
							},
						},
						Status: corev1.PodStatus{
							PodIP: "1.1.1.1",
						},
					},
					master: false,
					ready:  true,
				},
			},
			ssVersion:     "1",
			errExpected:   false,
			bootstrapping: true,
		},
		{
			name: "when no master exists",
			pods: []podStatus{
				{
					pod: corev1.Pod{
						ObjectMeta: metav1.ObjectMeta{
							Name: "slave1",
							Labels: map[string]string{
								appsv1.ControllerRevisionHashLabelKey: "10",
							},
						},
						Status: corev1.PodStatus{
							PodIP: "0.0.0.0",
						},
					},
					master: false,
					ready:  true,
				},
				{
					pod: corev1.Pod{
						ObjectMeta: metav1.ObjectMeta{
							Name: "slave2",
							Labels: map[string]string{
								appsv1.ControllerRevisionHashLabelKey: "10",
							},
						},
						Status: corev1.PodStatus{
							PodIP: "0.0.0.1",
						},
					},
					master: false,
					ready:  true,
				},
				{
					pod: corev1.Pod{
						ObjectMeta: metav1.ObjectMeta{
							Name: "slave3",
							Labels: map[string]string{
								appsv1.ControllerRevisionHashLabelKey: "10",
							},
						},
						Status: corev1.PodStatus{
							PodIP: "1.1.1.1",
						},
					},
					master: false,
					ready:  true,
				},
			},
			ssVersion:     "10",
			errExpected:   true,
			bootstrapping: false,
			noMaster:      true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertTest := assert.New(t)

			rf := generateRF(false, test.bootstrapping)

			config := generateConfig()
			mrfs := &mRFService.RedisFailoverClient{}

			mrfc := &mRFService.RedisFailoverCheck{}
			mrfc.On("GetRedisesIPs", rf).Once().Return([]string{"0.0.0.0", "0.0.0.1", "1.1.1.1"}, nil)

			next := true
			if !test.bootstrapping {
				master := "1.1.1.1"
				if test.noMaster {
					master = ""
				}
				mrfc.On("GetMasterIP", rf).Once().Return(master, nil)
			}

			mk := settledK8sServices()
			for _, pod := range test.pods {
				if !pod.master {
					mrfc.On("CheckRedisSlavesReady", pod.pod.Status.PodIP, rf).Once().Return(pod.ready, nil)
				}
				if !pod.ready {
					// The report of the wait reads the update revision and
					// the pods a second time. Without a master, only the
					// report reads them.
					reads := 2
					if test.bootstrapping {
						reads = 1
					} else {
						mrfc.On("GetRedisRevisionHash", pod.pod.Name, rf).Once().Return(pod.pod.Labels[appsv1.ControllerRevisionHashLabelKey], nil)
					}
					mrfc.On("GetStatefulSetUpdateRevision", rf).Times(reads).Return(test.ssVersion, nil)
					mk = &mK8SService.Services{}
					mk.On("GetStatefulSetPods", rf.Namespace, rfservice.GetRedisName(rf)).Times(reads).Return(&corev1.PodList{Items: []corev1.Pod{pod.pod}}, nil)
					next = false
					break
				}
			}
			mrfh := &mRFService.RedisFailoverHeal{}
			mrfh.On("ApplyPassword", mock.Anything, mock.Anything, mock.Anything).Maybe().Return(true, nil)
			mrfh.On("ApplySentinelPassword", mock.Anything, mock.Anything).Maybe().Return(true, nil)

			if next {
				replicas := []string{"slave1", "slave2"}
				if test.bootstrapping || test.noMaster {
					replicas = append(replicas, "slave3")
				}
				mrfc.On("GetStatefulSetUpdateRevision", rf).Once().Return(test.ssVersion, nil)
				mrfc.On("GetRedisesSlavesPods", rf).Once().Return(replicas, nil)

				for _, pod := range test.pods {
					mrfc.On("GetRedisRevisionHash", pod.pod.Name, rf).Once().Return(pod.pod.Labels[appsv1.ControllerRevisionHashLabelKey], nil)
					// A stale slave is deleted immediately in the slave loop; the
					// master is deleted later and only after the sentinel gate, so
					// its DeletePod expectation is set in the master block below.
					if pod.pod.Labels[appsv1.ControllerRevisionHashLabelKey] != test.ssVersion && !pod.master {
						mrfh.On("ResizePodInPlace", rf, pod.pod.Name, mock.Anything).Once().Return(rfservice.ResizeResult{}, nil)
						mrfh.On("DeletePod", pod.pod.Name, rf).Once().Return(nil)
						next = false
						break
					}
				}
				fmt.Printf("%v - %v\n", test.name, next)
				if next && !test.bootstrapping {
					if test.noMaster {
						mrfc.On("GetRedisesMasterPod", rf).Once().Return("", errors.New(""))
					} else {
						mrfc.On("GetRedisesMasterPod", rf).Once().Return("master", nil)

						masterStale := false
						for _, pod := range test.pods {
							if pod.master && pod.pod.Labels[appsv1.ControllerRevisionHashLabelKey] != test.ssVersion {
								masterStale = true
							}
						}
						if masterStale {
							// An in-place resize is tried first; before recreating the
							// master the operator checks every sentinel has a quorum of
							// the slaves in memory.
							mrfh.On("ResizePodInPlace", rf, "master", mock.Anything).Once().Return(rfservice.ResizeResult{}, nil)
							mrfc.On("GetSentinelsIPs", rf).Once().Return([]string{"sentinel0"}, nil)
							if test.sentinelSlavesShort {
								mrfc.On("CheckSentinelSlavesNumberQuorumInMemory", "sentinel0", rf).Once().Return(errors.New("redis slaves in sentinel memory below quorum"))
							} else {
								mrfc.On("CheckSentinelSlavesNumberQuorumInMemory", "sentinel0", rf).Once().Return(nil)
								mrfh.On("DeletePod", "master", rf).Once().Return(nil)
							}
						}
					}
				}
			}

			handler := rfOperator.NewRedisFailoverHandler(config, mrfs, mrfc, mrfh, mk, metrics.Dummy, log.Dummy)
			err := handler.UpdateRedisesPods(rf)

			if test.errExpected {
				assertTest.Error(err)
			} else {
				assertTest.NoError(err)
			}

			mrfc.AssertExpectations(t)
			mrfh.AssertExpectations(t)

		})
	}
}

// TestUpdateRedisesPodsOperatorManagedModeSkipsSentinelGate guards the fix
// for the operator-managed-mode outage this reproduces: once Sentinel is
// disabled (sentinel.enabled: false, or simply left unset - disabled by
// default since v4.0.0) and its Deployment/Service/ConfigMap/PDB are torn
// down by EnsureNotPresentSentinelResources, a RedisFailover whose redis
// StatefulSet needs its master pod replaced must not get stuck: before this
// fix, UpdateRedisesPods unconditionally called GetSentinelsIPs to gate the
// replacement on sentinel quorum, which 404'd against the now-nonexistent
// Sentinel Deployment and left the RedisFailover permanently NotHealthy
// ("unable to update redis pods") on every single reconcile, since the
// stale master pod could never be deleted.
func TestUpdateRedisesPodsOperatorManagedModeSkipsSentinelGate(t *testing.T) {
	assertTest := assert.New(t)

	rf := operatorManagedRF()
	config := generateConfig()
	mrfs := &mRFService.RedisFailoverClient{}
	mrfc := &mRFService.RedisFailoverCheck{}
	mrfh := &mRFService.RedisFailoverHeal{}
	mrfh.On("ApplyPassword", mock.Anything, mock.Anything, mock.Anything).Maybe().Return(true, nil)
	mrfh.On("ApplySentinelPassword", mock.Anything, mock.Anything).Maybe().Return(true, nil)

	mrfc.On("GetRedisesIPs", rf).Once().Return([]string{"1.1.1.1"}, nil)
	mrfc.On("GetMasterIP", rf).Once().Return("1.1.1.1", nil)
	mrfc.On("GetStatefulSetUpdateRevision", rf).Once().Return("10", nil)
	mrfc.On("GetRedisesSlavesPods", rf).Once().Return([]string{}, nil)
	mrfc.On("GetRedisesMasterPod", rf).Once().Return("master", nil)
	mrfc.On("GetRedisRevisionHash", "master", rf).Once().Return("9", nil) // stale
	mrfh.On("ResizePodInPlace", rf, "master", mock.Anything).Once().Return(rfservice.ResizeResult{}, nil)
	mrfc.On("GetBestReplicaForPromotion", rf).Once().Return(&rfservice.ReplicaInfo{IP: "1.1.1.2", Synced: true}, nil)
	mrfh.On("HandOverMaster", "1.1.1.1", "1.1.1.2", rf).Once().Return(rfservice.HandoverDone, nil)
	mrfh.On("PromoteBestReplica", "1.1.1.2", rf).Once().Return(nil)

	mk := settledK8sServices()
	handler := rfOperator.NewRedisFailoverHandler(config, mrfs, mrfc, mrfh, mk, metrics.Dummy, log.Dummy)
	err := handler.UpdateRedisesPods(rf)

	assertTest.NoError(err)
	// The point of the fix: no sentinel-related call is ever made.
	mrfc.AssertNotCalled(t, "GetSentinelsIPs", mock.Anything)
	mrfc.AssertNotCalled(t, "CheckSentinelSlavesNumberQuorumInMemory", mock.Anything, mock.Anything)
	// A replica takes the master role, and the master pod stays.
	mrfh.AssertNotCalled(t, "DeletePod", mock.Anything, mock.Anything)
	mrfc.AssertExpectations(t)
	mrfh.AssertExpectations(t)
}

// A deleted master leaves the clients without a master until a replica is
// promoted. In operator-managed mode, the master hands its role over with
// FAILOVER first, and a later reconcile replaces the old master pod as a stale
// replica.
func TestUpdateRedisesPodsHandsTheMasterRoleOver(t *testing.T) {
	errBoom := errors.New("boom")
	partial := fmt.Errorf("reconfigure replicas: %w", rfservice.ErrPartialReconciliation)
	synced := &rfservice.ReplicaInfo{IP: "1.1.1.2", PodName: "replica", Synced: true}
	tests := []struct {
		name        string
		sentinel    bool
		replicas    int32
		best        *rfservice.ReplicaInfo
		bestErr     error
		result      rfservice.HandoverResult
		handoverErr error
		promoteErr  error
		wantErr     string
		wantMessage string
		wantDelete  bool
		wantPromote bool
	}{
		{name: "the role moves, then the labels", replicas: 3, best: synced, wantPromote: true},
		{name: "a partial reconciliation after the role moved", replicas: 3, best: synced, promoteErr: partial, wantPromote: true},
		{name: "the relabel fails after the role moved", replicas: 3, best: synced, promoteErr: errBoom, wantErr: "boom", wantPromote: true},
		{name: "Redis aborts the failover", replicas: 3, best: synced, result: rfservice.HandoverAborted, wantMessage: "the handover of the master role to pod replica was aborted, because the replica did not catch up with the master in time, the next attempt is after 30s"},
		{name: "the master refuses the target", replicas: 3, best: synced, result: rfservice.HandoverRefused, wantMessage: "the master refused the handover of the master role to pod replica (1 of 3), the next attempt is after 30s"},
		{name: "a master without FAILOVER is deleted", replicas: 3, best: synced, result: rfservice.HandoverUnsupported, wantDelete: true},
		{name: "FAILOVER fails", replicas: 3, best: synced, handoverErr: errBoom, wantErr: "boom"},
		{name: "no replica to promote", replicas: 3, bestErr: errBoom, wantErr: "boom"},
		{name: "the best replica is not synced", replicas: 3, best: &rfservice.ReplicaInfo{IP: "1.1.1.2"}, wantErr: "no synced replica"},
		{name: "one pod has no replica to promote", replicas: 1, wantDelete: true},
		{name: "Sentinel fails over after the delete", sentinel: true, replicas: 3, wantDelete: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rf := operatorManagedRF()
			if test.sentinel {
				rf = generateRF(false, false)
			}
			rf.Spec.Redis.Replicas = test.replicas
			mrfc := &mRFService.RedisFailoverCheck{}
			mrfh := &mRFService.RedisFailoverHeal{}
			mrfc.On("GetRedisesIPs", rf).Once().Return([]string{"1.1.1.1"}, nil)
			mrfc.On("GetMasterIP", rf).Once().Return("1.1.1.1", nil)
			mrfc.On("GetStatefulSetUpdateRevision", rf).Once().Return("10", nil)
			mrfc.On("GetRedisesSlavesPods", rf).Once().Return([]string{}, nil)
			mrfc.On("GetRedisesMasterPod", rf).Once().Return("master", nil)
			mrfc.On("GetRedisRevisionHash", "master", rf).Once().Return("9", nil)
			mrfh.On("ResizePodInPlace", rf, "master", "10").Once().Return(rfservice.ResizeResult{}, nil)
			if test.sentinel {
				mrfc.On("GetSentinelsIPs", rf).Once().Return([]string{"11.0.0.1"}, nil)
				mrfc.On("CheckSentinelSlavesNumberQuorumInMemory", "11.0.0.1", rf).Once().Return(nil)
			}
			if test.replicas > 1 && !test.sentinel {
				mrfc.On("GetBestReplicaForPromotion", rf).Once().Return(test.best, test.bestErr)
			}
			if test.best != nil && test.best.Synced {
				mrfh.On("HandOverMaster", "1.1.1.1", "1.1.1.2", rf).Once().Return(test.result, test.handoverErr)
			}
			if test.wantDelete {
				mrfh.On("DeletePod", "master", rf).Once().Return(nil)
			}
			if test.wantPromote {
				mrfh.On("PromoteBestReplica", "1.1.1.2", rf).Once().Return(test.promoteErr)
			}

			handler := rfOperator.NewRedisFailoverHandler(generateConfig(), &mRFService.RedisFailoverClient{}, mrfc, mrfh, settledK8sServices(), metrics.Dummy, log.Dummy)
			err := handler.UpdateRedisesPods(rf)

			if test.wantErr != "" {
				assert.ErrorContains(t, err, test.wantErr)
			} else {
				assert.NoError(t, err)
			}
			if test.wantMessage != "" {
				assert.Equal(t, v1.NotHealthyState, rf.Status.State)
				assert.Equal(t, test.wantMessage, rf.Status.Message)
			}
			if !test.wantDelete {
				mrfh.AssertNotCalled(t, "DeletePod", mock.Anything, mock.Anything)
			}
			if !test.wantPromote {
				mrfh.AssertNotCalled(t, "PromoteBestReplica", mock.Anything, mock.Anything)
			}
			mrfc.AssertExpectations(t)
			mrfh.AssertExpectations(t)
		})
	}
}

// TestUpdateRedisesPodsErrorBranches exercises the remaining early-return
// error branches of UpdateRedisesPods (operator/redisfailover/checker.go)
// not already covered by TestUpdate above: every sub-call it makes can
// fail, and it must stop and propagate the error immediately.
func TestUpdateRedisesPodsErrorBranches(t *testing.T) {
	const (
		slave  = "1.1.1.1"
		master = "2.2.2.2"
	)

	tests := []struct {
		name  string
		setup func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover)
	}{
		{
			name: "GetRedisesIPs fails",
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("GetRedisesIPs", rf).Once().Return(nil, errors.New("ips err"))
			},
		},
		{
			name: "CheckRedisSlavesReady fails",
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("GetRedisesIPs", rf).Once().Return([]string{slave, master}, nil)
				mrfc.On("GetMasterIP", rf).Once().Return(master, nil)
				mrfc.On("CheckRedisSlavesReady", slave, rf).Once().Return(false, errors.New("check err"))
			},
		},
		{
			name: "GetRedisesSlavesPods fails",
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("GetRedisesIPs", rf).Once().Return([]string{master}, nil)
				mrfc.On("GetMasterIP", rf).Once().Return(master, nil)
				mrfc.On("GetStatefulSetUpdateRevision", rf).Once().Return("1", nil)
				mrfc.On("GetRedisesSlavesPods", rf).Once().Return(nil, errors.New("slaves pods err"))
			},
		},
		{
			name: "GetRedisRevisionHash fails for a slave pod",
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("GetRedisesIPs", rf).Once().Return([]string{master}, nil)
				mrfc.On("GetMasterIP", rf).Once().Return(master, nil)
				mrfc.On("GetStatefulSetUpdateRevision", rf).Once().Return("1", nil)
				mrfc.On("GetRedisesSlavesPods", rf).Once().Return([]string{"slave1"}, nil)
				mrfc.On("GetRedisRevisionHash", "slave1", rf).Once().Return("", errors.New("revision err"))
			},
		},
		{
			name: "DeletePod fails for a stale slave pod",
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("GetRedisesIPs", rf).Once().Return([]string{master}, nil)
				mrfc.On("GetMasterIP", rf).Once().Return(master, nil)
				mrfc.On("GetStatefulSetUpdateRevision", rf).Once().Return("1", nil)
				mrfc.On("GetRedisesSlavesPods", rf).Once().Return([]string{"slave1"}, nil)
				mrfc.On("GetRedisRevisionHash", "slave1", rf).Once().Return("stale", nil)
				mrfh.On("ResizePodInPlace", rf, "slave1", mock.Anything).Once().Return(rfservice.ResizeResult{}, nil)
				mrfh.On("DeletePod", "slave1", rf).Once().Return(errors.New("delete err"))
			},
		},
		{
			name: "GetRedisRevisionHash fails for the master pod",
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("GetRedisesIPs", rf).Once().Return([]string{master}, nil)
				mrfc.On("GetMasterIP", rf).Once().Return(master, nil)
				mrfc.On("GetStatefulSetUpdateRevision", rf).Once().Return("1", nil)
				mrfc.On("GetRedisesSlavesPods", rf).Once().Return([]string{}, nil)
				mrfc.On("GetRedisesMasterPod", rf).Once().Return(master, nil)
				mrfc.On("GetRedisRevisionHash", master, rf).Once().Return("", errors.New("master revision err"))
			},
		},
		{
			name: "GetSentinelsIPs fails before replacing a stale master pod",
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("GetRedisesIPs", rf).Once().Return([]string{master}, nil)
				mrfc.On("GetMasterIP", rf).Once().Return(master, nil)
				mrfc.On("GetStatefulSetUpdateRevision", rf).Once().Return("1", nil)
				mrfc.On("GetRedisesSlavesPods", rf).Once().Return([]string{}, nil)
				mrfc.On("GetRedisesMasterPod", rf).Once().Return(master, nil)
				mrfc.On("GetRedisRevisionHash", master, rf).Once().Return("stale", nil)
				mrfh.On("ResizePodInPlace", rf, master, mock.Anything).Once().Return(rfservice.ResizeResult{}, nil)
				mrfc.On("GetSentinelsIPs", rf).Once().Return(nil, errors.New("sentinels ips err"))
			},
		},
		{
			name: "DeletePod fails for a stale master pod",
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("GetRedisesIPs", rf).Once().Return([]string{master}, nil)
				mrfc.On("GetMasterIP", rf).Once().Return(master, nil)
				mrfc.On("GetStatefulSetUpdateRevision", rf).Once().Return("1", nil)
				mrfc.On("GetRedisesSlavesPods", rf).Once().Return([]string{}, nil)
				mrfc.On("GetRedisesMasterPod", rf).Once().Return(master, nil)
				mrfc.On("GetRedisRevisionHash", master, rf).Once().Return("stale", nil)
				mrfc.On("GetSentinelsIPs", rf).Once().Return([]string{"sentinel0"}, nil)
				mrfc.On("CheckSentinelSlavesNumberQuorumInMemory", "sentinel0", rf).Once().Return(nil)
				mrfh.On("ResizePodInPlace", rf, master, mock.Anything).Once().Return(rfservice.ResizeResult{}, nil)
				mrfh.On("DeletePod", master, rf).Once().Return(errors.New("delete master err"))
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertTest := assert.New(t)

			rf := generateRF(false, false)

			config := generateConfig()
			mk := settledK8sServices()
			mrfs := &mRFService.RedisFailoverClient{}
			mrfc := &mRFService.RedisFailoverCheck{}
			mrfh := &mRFService.RedisFailoverHeal{}
			mrfh.On("ApplyPassword", mock.Anything, mock.Anything, mock.Anything).Maybe().Return(true, nil)
			mrfh.On("ApplySentinelPassword", mock.Anything, mock.Anything).Maybe().Return(true, nil)

			test.setup(mrfc, mrfh, rf)

			handler := rfOperator.NewRedisFailoverHandler(config, mrfs, mrfc, mrfh, mk, metrics.Dummy, log.Dummy)
			err := handler.UpdateRedisesPods(rf)

			assertTest.Error(err)

			mrfc.AssertExpectations(t)
			mrfh.AssertExpectations(t)
		})
	}
}

// settledK8sServices returns a k8s service mock whose redis pods are all up,
// so the pod-replacement and master-election waits don't hold anything back.
func settledK8sServices() *mK8SService.Services {
	mk := &mK8SService.Services{}
	mk.On("GetStatefulSetPods", mock.Anything, mock.Anything).Maybe().Return(&corev1.PodList{Items: make([]corev1.Pod, 5)}, nil)
	return mk
}

func redisPod(revision string, ready, deleting bool) corev1.Pod {
	pod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{appsv1.ControllerRevisionHashLabelKey: revision}}}
	if ready {
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	}
	if deleting {
		pod.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	}
	return pod
}

func masterPod(pod corev1.Pod) corev1.Pod {
	pod.Labels["redisfailovers-role"] = "master"
	return pod
}

func TestUpdateRedisesPodsWaitsForTheLastReplacement(t *testing.T) {
	tests := []struct {
		name       string
		pods       []corev1.Pod
		podsErr    error
		wantDelete bool
	}{
		{
			name:       "all pods up",
			pods:       []corev1.Pod{redisPod("old", true, false), redisPod("old", true, false), redisPod("new", true, false)},
			wantDelete: true,
		},
		{
			name: "a pod is being deleted",
			pods: []corev1.Pod{redisPod("old", true, false), redisPod("old", true, false), redisPod("old", true, true)},
		},
		{
			name: "the deleted pod is not recreated yet",
			pods: []corev1.Pod{redisPod("old", true, false), redisPod("old", true, false)},
		},
		{
			name: "the recreated pod is not ready yet",
			pods: []corev1.Pod{redisPod("old", true, false), redisPod("old", true, false), redisPod("new", false, false)},
		},
		{
			name:       "an old pod that is not ready doesn't block",
			pods:       []corev1.Pod{redisPod("old", true, false), redisPod("old", false, false), redisPod("new", true, false)},
			wantDelete: true,
		},
		{
			name:    "listing pods fails",
			podsErr: errors.New("list err"),
		},
	}

	for _, test := range tests {
		for _, stale := range []string{"slave", "master"} {
			t.Run(test.name+"/"+stale, func(t *testing.T) {
				rf := operatorManagedRF()
				rf.Spec.Redis.Replicas = 3
				mrfc := &mRFService.RedisFailoverCheck{}
				mrfh := &mRFService.RedisFailoverHeal{}
				mrfh.On("ApplyPassword", mock.Anything, mock.Anything, mock.Anything).Maybe().Return(true, nil)
				mrfh.On("ApplySentinelPassword", mock.Anything, mock.Anything).Maybe().Return(true, nil)
				mk := &mK8SService.Services{}
				mrfc.On("GetRedisesIPs", rf).Once().Return([]string{"10.0.0.1"}, nil)
				mrfc.On("GetMasterIP", rf).Once().Return("10.0.0.1", nil)
				mrfc.On("GetStatefulSetUpdateRevision", rf).Once().Return("new", nil)
				if stale == "slave" {
					mrfc.On("GetRedisesSlavesPods", rf).Once().Return([]string{"slave"}, nil)
				} else {
					mrfc.On("GetRedisesSlavesPods", rf).Once().Return([]string{}, nil)
					mrfc.On("GetRedisesMasterPod", rf).Once().Return("master", nil)
				}
				mrfc.On("GetRedisRevisionHash", stale, rf).Once().Return("old", nil)
				mk.On("GetStatefulSetPods", rf.Namespace, rfservice.GetRedisName(rf)).Once().Return(&corev1.PodList{Items: test.pods}, test.podsErr)
				if test.wantDelete {
					mrfh.On("ResizePodInPlace", rf, stale, mock.Anything).Once().Return(rfservice.ResizeResult{}, nil)
					if stale == "slave" {
						mrfh.On("DeletePod", stale, rf).Once().Return(nil)
					} else {
						mrfc.On("GetBestReplicaForPromotion", rf).Once().Return(&rfservice.ReplicaInfo{IP: "10.0.0.2", Synced: true}, nil)
						mrfh.On("HandOverMaster", "10.0.0.1", "10.0.0.2", rf).Once().Return(rfservice.HandoverDone, nil)
						mrfh.On("PromoteBestReplica", "10.0.0.2", rf).Once().Return(nil)
					}
				}

				handler := rfOperator.NewRedisFailoverHandler(generateConfig(), &mRFService.RedisFailoverClient{}, mrfc, mrfh, mk, metrics.Dummy, log.Dummy)
				err := handler.UpdateRedisesPods(rf)

				assert.Equal(t, test.podsErr, err)
				mrfc.AssertExpectations(t)
				mrfh.AssertExpectations(t)
				mk.AssertExpectations(t)
			})
		}
	}
}

func TestOperatorManagedModeWaitsForAStoppingMasterBeforeElecting(t *testing.T) {
	tests := []struct {
		name      string
		pods      []corev1.Pod
		podsErr   error
		wantElect bool
	}{
		{
			name:      "no pod is stopping",
			pods:      []corev1.Pod{redisPod("1", true, false), redisPod("1", true, false)},
			wantElect: true,
		},
		{
			name: "the old master is stopping and still ready",
			pods: []corev1.Pod{redisPod("1", true, false), masterPod(redisPod("1", true, true))},
		},
		{
			name:      "a stopping master that is not ready (lost node) doesn't block",
			pods:      []corev1.Pod{redisPod("1", true, false), masterPod(redisPod("1", false, true))},
			wantElect: true,
		},
		{
			name:      "a stopping replica doesn't block",
			pods:      []corev1.Pod{redisPod("1", true, false), redisPod("1", true, true)},
			wantElect: true,
		},
		{
			name:    "listing pods fails",
			podsErr: errors.New("list err"),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rf := operatorManagedRF()
			mrfc := &mRFService.RedisFailoverCheck{}
			mrfh := &mRFService.RedisFailoverHeal{}
			mrfh.On("ApplyPassword", mock.Anything, mock.Anything, mock.Anything).Maybe().Return(true, nil)
			mrfh.On("ApplySentinelPassword", mock.Anything, mock.Anything).Maybe().Return(true, nil)
			mk := &mK8SService.Services{}
			mk.On("UpdateRedisFailoverStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return()
			mrfc.On("IsRedisRunningQuorum", rf).Once().Return(true)
			mrfc.On("GetNumberMasters", rf).Once().Return(0, nil)
			mk.On("GetStatefulSetPods", rf.Namespace, rfservice.GetRedisName(rf)).Return(&corev1.PodList{Items: test.pods}, test.podsErr)
			if test.wantElect {
				mrfc.On("GetBestReplicaForPromotion", rf).Once().Return(&rfservice.ReplicaInfo{IP: "10.0.0.2"}, nil)
				mrfh.On("PromoteBestReplica", "10.0.0.2", rf).Once().Return(nil)
			}

			handler := rfOperator.NewRedisFailoverHandler(generateConfig(), &mRFService.RedisFailoverClient{}, mrfc, mrfh, mk, metrics.Dummy, log.Dummy)
			err := handler.CheckAndHeal(rf)

			assert.Equal(t, test.podsErr, err)
			if test.podsErr != nil {
				assert.Equal(t, v1.NotHealthyState, rf.Status.State)
				assert.Equal(t, "unable to check whether the master is stopping", rf.Status.Message)
			} else if !test.wantElect {
				assert.Equal(t, v1.NotHealthyState, rf.Status.State)
				assert.Equal(t, "no master, waiting for the stopping master pod to exit", rf.Status.Message)
			}
			mrfc.AssertExpectations(t)
			mrfh.AssertExpectations(t)
			mk.AssertExpectations(t)
		})
	}
}

// GetNumberMasters can still count a master whose pod starts stopping before
// CheckMasterHealth looks for it (a scale-down removing the master). Its
// failover must wait for the pod to stop, as with no master counted.
func TestOperatorManagedModeWaitsForAStoppingMasterBeforeFailover(t *testing.T) {
	tests := []struct {
		name      string
		masterIP  string
		pods      []corev1.Pod
		podsErr   error
		wantElect bool
	}{
		{
			name:      "no pod is stopping",
			pods:      []corev1.Pod{redisPod("1", true, false), redisPod("1", true, false)},
			wantElect: true,
		},
		{
			name: "the master is stopping and still ready",
			pods: []corev1.Pod{redisPod("1", true, false), masterPod(redisPod("1", true, true))},
		},
		{
			name:      "a stopping master that is not ready (lost node) doesn't block",
			pods:      []corev1.Pod{redisPod("1", true, false), masterPod(redisPod("1", false, true))},
			wantElect: true,
		},
		{
			// It fails over because failoverTimeout is 0s here.
			name:     "an unreachable master on a live pod fails over",
			masterIP: "10.0.0.1",
			pods: []corev1.Pod{{
				ObjectMeta: metav1.ObjectMeta{Name: "rfr-0"},
				Status:     corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.0.0.1"},
			}},
			wantElect: true,
		},
		{
			name:    "listing pods fails",
			podsErr: errors.New("list err"),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rf := operatorManagedRF()
			mrfc := &mRFService.RedisFailoverCheck{}
			mrfh := &mRFService.RedisFailoverHeal{}
			mrfh.On("ApplyPassword", mock.Anything, mock.Anything, mock.Anything).Maybe().Return(true, nil)
			mrfh.On("ApplySentinelPassword", mock.Anything, mock.Anything).Maybe().Return(true, nil)
			mk := &mK8SService.Services{}
			mk.On("UpdateRedisFailoverStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return()
			mrfc.On("IsRedisRunningQuorum", rf).Once().Return(true)
			mrfc.On("GetNumberMasters", rf).Once().Return(1, nil)
			mrfc.On("CheckMasterHealth", rf).Once().Return(false, test.masterIP, nil)
			mk.On("GetStatefulSetPods", rf.Namespace, rfservice.GetRedisName(rf)).Return(&corev1.PodList{Items: test.pods}, test.podsErr)
			if test.masterIP != "" {
				rf.Spec.Sentinel.FailoverTimeout = &metav1.Duration{}
			}
			if test.wantElect {
				mrfc.On("GetBestReplicaForPromotion", rf).Once().Return(&rfservice.ReplicaInfo{IP: "10.0.0.2"}, nil)
				mrfh.On("PromoteBestReplica", "10.0.0.2", rf).Once().Return(nil)
			}

			handler := rfOperator.NewRedisFailoverHandler(generateConfig(), &mRFService.RedisFailoverClient{}, mrfc, mrfh, mk, metrics.Dummy, log.Dummy)
			err := handler.CheckAndHeal(rf)

			assert.Equal(t, test.podsErr, err)
			if test.podsErr != nil {
				assert.Equal(t, v1.NotHealthyState, rf.Status.State)
				assert.Equal(t, "unable to check whether the master is stopping", rf.Status.Message)
			} else if !test.wantElect {
				assert.Equal(t, v1.NotHealthyState, rf.Status.State)
				assert.Equal(t, "no master, waiting for the stopping master pod to exit", rf.Status.Message)
			}
			mrfc.AssertExpectations(t)
			mrfh.AssertExpectations(t)
			mk.AssertExpectations(t)
		})
	}
}

func TestSentinelModeWaitsForAStoppingMasterBeforeElecting(t *testing.T) {
	tests := []struct {
		name      string
		replicas  int32
		firstBoot bool
		pods      []corev1.Pod
		podsErr   error
		wantElect bool
	}{
		{
			name:      "single replica, no pod is stopping",
			replicas:  1,
			pods:      []corev1.Pod{redisPod("1", true, false)},
			wantElect: true,
		},
		{
			name:     "single replica, the old master is stopping and still ready",
			replicas: 1,
			pods:     []corev1.Pod{redisPod("1", true, false), masterPod(redisPod("1", true, true))},
		},
		{
			name:      "single replica, a stopping master that is not ready (lost node) does not block",
			replicas:  1,
			pods:      []corev1.Pod{redisPod("1", true, false), masterPod(redisPod("1", false, true))},
			wantElect: true,
		},
		{
			name:      "first boot",
			replicas:  3,
			firstBoot: true,
			pods:      []corev1.Pod{redisPod("1", true, false), redisPod("1", true, false), redisPod("1", true, false)},
			wantElect: true,
		},
		{
			name:     "no sentinel quorum, the old master is stopping and still ready",
			replicas: 3,
			pods:     []corev1.Pod{redisPod("1", true, false), redisPod("1", true, false), masterPod(redisPod("1", true, true))},
		},
		{
			name:      "no sentinel quorum, a stopping master that is not ready (lost node) does not block",
			replicas:  3,
			pods:      []corev1.Pod{redisPod("1", true, false), redisPod("1", true, false), masterPod(redisPod("1", false, true))},
			wantElect: true,
		},
		{
			name:      "no sentinel quorum, a stopping replica does not block",
			replicas:  3,
			pods:      []corev1.Pod{redisPod("1", true, false), redisPod("1", true, false), redisPod("1", true, true)},
			wantElect: true,
		},
		{
			name:     "listing pods fails",
			replicas: 3,
			podsErr:  errors.New("list err"),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rf := generateRF(false, false)
			rf.Spec.Redis.Replicas = test.replicas
			mrfc := &mRFService.RedisFailoverCheck{}
			mrfh := &mRFService.RedisFailoverHeal{}
			mrfh.On("ApplyPassword", mock.Anything, mock.Anything, mock.Anything).Maybe().Return(true, nil)
			mrfh.On("ApplySentinelPassword", mock.Anything, mock.Anything).Maybe().Return(true, nil)
			mk := &mK8SService.Services{}
			mk.On("UpdateRedisFailoverStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return()
			mrfc.On("IsRedisRunningQuorum", rf).Once().Return(true)
			mrfc.On("IsSentinelRunningQuorum", rf).Once().Return(true)
			mrfc.On("GetNumberMasters", rf).Once().Return(0, nil)
			mk.On("GetStatefulSetPods", rf.Namespace, rfservice.GetRedisName(rf)).Once().Return(&corev1.PodList{Items: test.pods}, test.podsErr)
			if test.wantElect {
				if test.replicas > 1 {
					mrfc.On("GetMaxRedisPodTime", rf).Once().Return(time.Minute, nil)
					if test.firstBoot {
						mrfc.On("CheckSentinelQuorum", rf).Once().Return(0, nil)
						mrfc.On("CheckIfMasterLocalhost", rf).Once().Return(true, nil)
					} else {
						mrfc.On("CheckSentinelQuorum", rf).Once().Return(3, errors.New("no quorum"))
					}
				}
				mrfh.On("SetOldestAsMaster", rf).Once().Return(nil)
				if test.replicas > 1 {
					// An error after the election ends CheckAndHeal, so the later checks need no mocks.
					mrfc.On("GetMasterIP", rf).Once().Return("", errors.New("stop here"))
				}
			}

			handler := rfOperator.NewRedisFailoverHandler(generateConfig(), &mRFService.RedisFailoverClient{}, mrfc, mrfh, mk, metrics.Dummy, log.Dummy)
			err := handler.CheckAndHeal(rf)

			switch {
			case test.podsErr != nil:
				assert.Equal(t, test.podsErr, err)
				assert.Equal(t, v1.NotHealthyState, rf.Status.State)
				assert.Equal(t, "unable to check whether the master is stopping", rf.Status.Message)
			case test.wantElect && test.replicas > 1:
				assert.EqualError(t, err, "stop here")
			case !test.wantElect:
				assert.NoError(t, err)
				assert.Equal(t, v1.NotHealthyState, rf.Status.State)
				assert.Equal(t, "no master, waiting for the stopping master pod to exit", rf.Status.Message)
			default:
				assert.NoError(t, err)
			}
			mrfc.AssertExpectations(t)
			mrfh.AssertExpectations(t)
			mk.AssertExpectations(t)
		})
	}
}

// A pod resized in place is neither deleted nor, as master, failed over.
func TestUpdateRedisesPodsResizesInPlace(t *testing.T) {
	for _, stale := range []string{"slave", "master"} {
		for _, action := range []rfservice.ResizeAction{rfservice.ResizeWaiting, rfservice.ResizeDone} {
			t.Run(fmt.Sprintf("%s/%d", stale, action), func(t *testing.T) {
				rf := generateRF(false, false)
				mrfc := &mRFService.RedisFailoverCheck{}
				mrfh := &mRFService.RedisFailoverHeal{}
				mrfh.On("ApplyPassword", mock.Anything, mock.Anything, mock.Anything).Maybe().Return(true, nil)
				mrfh.On("ApplySentinelPassword", mock.Anything, mock.Anything).Maybe().Return(true, nil)
				mrfc.On("GetRedisesIPs", rf).Once().Return([]string{"10.0.0.1"}, nil)
				mrfc.On("GetMasterIP", rf).Once().Return("10.0.0.1", nil)
				mrfc.On("GetStatefulSetUpdateRevision", rf).Once().Return("new", nil)
				if stale == "slave" {
					mrfc.On("GetRedisesSlavesPods", rf).Once().Return([]string{"slave"}, nil)
				} else {
					mrfc.On("GetRedisesSlavesPods", rf).Once().Return([]string{}, nil)
					mrfc.On("GetRedisesMasterPod", rf).Once().Return("master", nil)
				}
				mrfc.On("GetRedisRevisionHash", stale, rf).Once().Return("old", nil)
				// No DeletePod or sentinel expectations: calling them would panic the mock.
				mrfh.On("ResizePodInPlace", rf, stale, "new").Once().Return(rfservice.ResizeResult{Action: action, Message: "resizing"}, nil)

				handler := rfOperator.NewRedisFailoverHandler(generateConfig(), &mRFService.RedisFailoverClient{}, mrfc, mrfh, settledK8sServices(), metrics.Dummy, log.Dummy)
				assert.NoError(t, handler.UpdateRedisesPods(rf))
				if action == rfservice.ResizeWaiting {
					assert.Equal(t, "resizing", rf.Status.Message)
				}
				mrfc.AssertExpectations(t)
				mrfh.AssertExpectations(t)
			})
		}
	}
}

func TestUpdateRedisesPodsResizeError(t *testing.T) {
	rf := generateRF(false, false)
	mrfc := &mRFService.RedisFailoverCheck{}
	mrfh := &mRFService.RedisFailoverHeal{}
	mrfh.On("ApplyPassword", mock.Anything, mock.Anything, mock.Anything).Maybe().Return(true, nil)
	mrfh.On("ApplySentinelPassword", mock.Anything, mock.Anything).Maybe().Return(true, nil)
	mrfc.On("GetRedisesIPs", rf).Once().Return([]string{"10.0.0.1"}, nil)
	mrfc.On("GetMasterIP", rf).Once().Return("10.0.0.1", nil)
	mrfc.On("GetStatefulSetUpdateRevision", rf).Once().Return("new", nil)
	mrfc.On("GetRedisesSlavesPods", rf).Once().Return([]string{"slave"}, nil)
	mrfc.On("GetRedisRevisionHash", "slave", rf).Once().Return("old", nil)
	mrfh.On("ResizePodInPlace", rf, "slave", "new").Once().Return(rfservice.ResizeResult{}, errors.New("resize err"))

	handler := rfOperator.NewRedisFailoverHandler(generateConfig(), &mRFService.RedisFailoverClient{}, mrfc, mrfh, settledK8sServices(), metrics.Dummy, log.Dummy)
	assert.EqualError(t, handler.UpdateRedisesPods(rf), "resize err")
	mrfh.AssertExpectations(t)
}

// A wait for an unsynced replica on a stale revision can be infinite, for
// example after an image revert. The rollout replaces such a replica.
func TestUpdateRedisesPodsReplacesUnsyncedStaleReplicas(t *testing.T) {
	type redis struct {
		name, ip, revision string
		synced             bool
	}
	master := redis{name: "master", ip: "10.0.0.1", revision: "new"}
	unsyncedStale := []redis{master, {"r1", "10.0.0.2", "new", true}, {"r2", "10.0.0.3", "old", false}}
	tests := []struct {
		name          string
		redises       []redis
		noMaster      bool
		bootstrapping bool
		unsettled     bool
		// editR2 changes the r2 pod in the GetStatefulSetPods result.
		editR2 func(*corev1.Pod)
		// promoted makes r2 a master before the delete.
		promoted   bool
		errOn      string
		wantDelete string
	}{
		{
			name:       "an unsynced stale replica is replaced",
			redises:    []redis{master, {"r1", "10.0.0.2", "new", true}, {"r2", "10.0.0.3", "old", false}},
			wantDelete: "r2",
		},
		{
			name:       "unsynced stale replicas go before synced ones",
			redises:    []redis{master, {"r1", "10.0.0.2", "old", true}, {"r2", "10.0.0.3", "old", false}},
			wantDelete: "r2",
		},
		{
			name:    "a replica syncing on the update revision is waited for",
			redises: []redis{master, {"r1", "10.0.0.2", "old", true}, {"r2", "10.0.0.3", "new", false}},
		},
		{
			name:    "a replica syncing on the update revision is waited for before an unsynced stale one is replaced",
			redises: []redis{master, {"r1", "10.0.0.2", "old", false}, {"r2", "10.0.0.3", "new", false}},
		},
		{
			name:     "the master is never replaced",
			redises:  []redis{{"master", "10.0.0.1", "old", false}, {"r1", "10.0.0.2", "new", true}, {"r2", "10.0.0.3", "new", true}},
			noMaster: true,
		},
		{
			name:      "the last replacement has to settle first",
			redises:   unsyncedStale,
			unsettled: true,
		},
		{
			name:          "an unsynced stale replica is waited for while bootstrapping",
			redises:       []redis{{"r0", "10.0.0.1", "new", true}, {"r1", "10.0.0.2", "new", true}, {"r2", "10.0.0.3", "old", false}},
			noMaster:      true,
			bootstrapping: true,
		},
		{
			name:    "an unsynced replica without a pod is waited for",
			redises: unsyncedStale,
			editR2:  func(p *corev1.Pod) { p.Status.PodIP = "" },
		},
		{
			name:    "an unsynced replica whose pod is terminating is waited for",
			redises: unsyncedStale,
			editR2:  func(p *corev1.Pod) { p.DeletionTimestamp = &metav1.Time{Time: time.Now()} },
		},
		{
			name:    "getting the update revision fails",
			redises: unsyncedStale,
			errOn:   "GetStatefulSetUpdateRevision",
		},
		{
			name:    "listing the pods fails",
			redises: unsyncedStale,
			errOn:   "GetStatefulSetPods",
		},
		{
			name:    "getting the replica's revision fails",
			redises: unsyncedStale,
			errOn:   "GetRedisRevisionHash",
		},
		{
			name:     "an unsynced stale replica promoted since is not replaced",
			redises:  unsyncedStale,
			promoted: true,
		},
		{
			name:    "the replica's role can't be read before it is replaced",
			redises: unsyncedStale,
			errOn:   "GetRedisesSlavesPods",
		},
		{
			name:    "getting the update revision fails while reporting the wait",
			redises: []redis{master, {"r1", "10.0.0.2", "new", true}, {"r2", "10.0.0.3", "new", false}},
			errOn:   "second GetStatefulSetUpdateRevision",
		},
		{
			name:    "listing the pods fails while reporting the wait",
			redises: []redis{master, {"r1", "10.0.0.2", "new", true}, {"r2", "10.0.0.3", "new", false}},
			errOn:   "second GetStatefulSetPods",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rf := operatorManagedRF()
			if test.bootstrapping {
				rf = generateRF(false, true)
			}
			rf.Spec.Redis.Replicas = int32(len(test.redises))
			if test.unsettled {
				rf.Spec.Redis.Replicas++
			}
			mrfc := &mRFService.RedisFailoverCheck{}
			mrfh := &mRFService.RedisFailoverHeal{}
			mk := &mK8SService.Services{}
			masterIP := master.ip
			if test.noMaster {
				masterIP = ""
			}
			errBoom := errors.New(test.errOn + " err")
			switch test.errOn {
			case "GetStatefulSetUpdateRevision":
				mrfc.On("GetStatefulSetUpdateRevision", rf).Once().Return("", errBoom)
			case "GetStatefulSetPods":
				mk.On("GetStatefulSetPods", rf.Namespace, rfservice.GetRedisName(rf)).Once().Return(nil, errBoom)
			case "GetRedisRevisionHash":
				mrfc.On("GetRedisRevisionHash", "r2", rf).Once().Return("", errBoom)
			case "GetRedisesSlavesPods":
				mrfc.On("GetRedisesSlavesPods", rf).Once().Return(nil, errBoom)
			case "second GetStatefulSetUpdateRevision":
				mrfc.On("GetStatefulSetUpdateRevision", rf).Once().Return("new", nil)
				mrfc.On("GetStatefulSetUpdateRevision", rf).Once().Return("", errBoom)
			}
			ips, replicas, pods := []string{}, []string{}, []corev1.Pod{}
			for _, r := range test.redises {
				ips = append(ips, r.ip)
				if r.name != "master" && (r.name != "r2" || !test.promoted) {
					replicas = append(replicas, r.name)
				}
				if r.ip != masterIP {
					mrfc.On("CheckRedisSlavesReady", r.ip, rf).Maybe().Return(r.synced, nil)
				}
				mrfc.On("GetRedisRevisionHash", r.name, rf).Maybe().Return(r.revision, nil)
				pod := redisPod(r.revision, true, false)
				pod.Name = r.name
				pod.Status.PodIP = r.ip
				if r.name == "r2" && test.editR2 != nil {
					test.editR2(&pod)
				}
				pods = append(pods, pod)
			}
			mrfc.On("GetRedisesIPs", rf).Once().Return(ips, nil)
			mrfc.On("GetMasterIP", rf).Maybe().Return(masterIP, nil)
			mrfc.On("GetStatefulSetUpdateRevision", rf).Maybe().Return("new", nil)
			mrfc.On("GetRedisesSlavesPods", rf).Maybe().Return(replicas, nil)
			if test.errOn == "second GetStatefulSetPods" {
				mk.On("GetStatefulSetPods", rf.Namespace, rfservice.GetRedisName(rf)).Once().Return(&corev1.PodList{Items: pods}, nil)
				mk.On("GetStatefulSetPods", rf.Namespace, rfservice.GetRedisName(rf)).Once().Return(nil, errBoom)
			}
			mk.On("GetStatefulSetPods", rf.Namespace, rfservice.GetRedisName(rf)).Maybe().Return(&corev1.PodList{Items: pods}, nil)
			// The mock panics on any other DeletePod.
			mrfh.On("ResizePodInPlace", rf, mock.Anything, "new").Maybe().Return(rfservice.ResizeResult{Action: rfservice.ResizeRecreate}, nil)
			if test.wantDelete != "" {
				mrfh.On("DeletePod", test.wantDelete, rf).Once().Return(nil)
			}

			handler := rfOperator.NewRedisFailoverHandler(generateConfig(), &mRFService.RedisFailoverClient{}, mrfc, mrfh, mk, metrics.Dummy, log.Dummy)
			if test.errOn != "" {
				assert.Equal(t, errBoom, handler.UpdateRedisesPods(rf))
			} else {
				assert.NoError(t, handler.UpdateRedisesPods(rf))
			}
			mrfc.AssertExpectations(t)
			mrfh.AssertExpectations(t)
			mk.AssertExpectations(t)
		})
	}
}

// The master answers the first count and then stalls. The operator must not
// replace it in this reconcile: it can still take writes, and REPLICAOF to it
// fails, so two pods keep the master label. Its wait starts at this check.
func TestOperatorManagedModeDoesNotReplaceAStalledMaster(t *testing.T) {
	const (
		masterIP  = "10.0.0.1"
		replicaIP = "10.0.0.2"
	)
	tests := []struct {
		name        string
		ready       bool
		wantErr     error
		wantMessage string
	}{
		{
			name:        "the master pod is ready",
			ready:       true,
			wantErr:     rfservice.ErrRedisNotAnswering,
			wantMessage: "unable to check master health",
		},
		{
			name:        "the master pod is not ready",
			wantMessage: `^master unreachable since \S+, failing over after 10s$`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rf := operatorManagedRF()
			rf.Spec.Redis.Replicas = 2
			master := masterPod(redisPod("1", test.ready, false))
			master.Name, master.Status.Phase, master.Status.PodIP = "rfr-0", corev1.PodRunning, masterIP
			replica := redisPod("1", true, false)
			replica.Name, replica.Status.Phase, replica.Status.PodIP = "rfr-1", corev1.PodRunning, replicaIP

			mk := &mK8SService.Services{}
			mk.On("UpdateRedisFailoverStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return()
			mk.On("GetStatefulSetPods", rf.Namespace, rfservice.GetRedisName(rf)).Return(&corev1.PodList{Items: []corev1.Pod{master, replica}}, nil)
			mk.On("UpdatePodAnnotations", rf.Namespace, "rfr-0", mock.MatchedBy(func(a map[string]string) bool {
				return a["redisfailovers.databases.spotahome.com/unreachable-since"] != ""
			})).Once().Return(nil)
			stall := errors.New("i/o timeout")
			mr := &mRedisService.Client{}
			mr.On("IsMaster", masterIP, "0", "").Once().Return(true, nil)
			mr.On("IsMaster", masterIP, "0", "").Return(false, stall)
			mr.On("IsMaster", replicaIP, "0", "").Return(false, nil)
			mr.On("GetReplicationInfo", masterIP, "0", "").Maybe().Return(nil, stall)
			mr.On("GetReplicationInfo", replicaIP, "0", "").Maybe().Return(&redis.ReplicationInfo{Role: "slave", SlaveReplOffset: 100, MasterLinkStatus: "up"}, nil)
			mrfh := &mRFService.RedisFailoverHeal{}
			mrfh.On("ApplyPassword", mock.Anything, mock.Anything, mock.Anything).Maybe().Return(true, nil)
			mrfh.On("PromoteBestReplica", replicaIP, rf).Maybe().Return(nil)

			checker := rfservice.NewRedisFailoverChecker(mk, mr, log.Dummy, metrics.Dummy)
			handler := rfOperator.NewRedisFailoverHandler(generateConfig(), &mRFService.RedisFailoverClient{}, checker, mrfh, mk, metrics.Dummy, log.Dummy)

			err := handler.CheckAndHeal(rf)

			if test.wantErr != nil {
				assert.ErrorIs(t, err, test.wantErr)
			} else {
				assert.NoError(t, err)
			}
			assert.Equal(t, v1.NotHealthyState, rf.Status.State)
			assert.Regexp(t, test.wantMessage, rf.Status.Message)
			mrfh.AssertNotCalled(t, "PromoteBestReplica", replicaIP, rf)
			mk.AssertExpectations(t)
		})
	}
}

// A failed lookup of the master pod must not give a promotion without the wait.
func TestOperatorManagedModeDoesNotReplaceAStalledMasterWhenThePodLookupFails(t *testing.T) {
	rf := operatorManagedRF()
	listErr := errors.New("list err")
	mrfc := &mRFService.RedisFailoverCheck{}
	mrfh := &mRFService.RedisFailoverHeal{}
	mrfh.On("ApplyPassword", mock.Anything, mock.Anything, mock.Anything).Maybe().Return(true, nil)
	mk := &mK8SService.Services{}
	mk.On("UpdateRedisFailoverStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return()
	mrfc.On("IsRedisRunningQuorum", rf).Once().Return(true)
	mrfc.On("GetNumberMasters", rf).Once().Return(1, nil)
	mrfc.On("CheckMasterHealth", rf).Once().Return(false, "", nil)
	// masterPodStopping lists the pods, then unreachableMasterPod fails.
	mk.On("GetStatefulSetPods", rf.Namespace, rfservice.GetRedisName(rf)).Once().Return(&corev1.PodList{Items: []corev1.Pod{redisPod("1", true, false)}}, nil)
	mk.On("GetStatefulSetPods", rf.Namespace, rfservice.GetRedisName(rf)).Once().Return(nil, listErr)

	handler := rfOperator.NewRedisFailoverHandler(generateConfig(), &mRFService.RedisFailoverClient{}, mrfc, mrfh, mk, metrics.Dummy, log.Dummy)
	err := handler.CheckAndHeal(rf)

	assert.ErrorIs(t, err, listErr)
	assert.Equal(t, v1.NotHealthyState, rf.Status.State)
	assert.Equal(t, "unable to look up the master pod", rf.Status.Message)
	mrfc.AssertExpectations(t)
	mrfh.AssertExpectations(t)
	mk.AssertExpectations(t)
}

// After a failover, the old master can come back as a master. The pod
// labelled master is the master that the operator elected.
func TestOperatorManagedModeDemotesTheMastersThatAreNotLabelledMaster(t *testing.T) {
	pod := func(name, ip string, master bool, phase corev1.PodPhase, deleting bool) corev1.Pod {
		p := redisPod("1", true, deleting)
		if master {
			p = masterPod(p)
		}
		p.Name, p.Status.Phase, p.Status.PodIP = name, phase, ip
		return p
	}
	listErr := errors.New("list err")
	setErr := errors.New("set err")
	tests := []struct {
		name        string
		pods        []corev1.Pod
		podsErr     error
		setErr      error
		wantDemote  string
		wantErr     error
		wantMessage string
		wantStatus  string
	}{
		{
			name: "one pod is labelled master",
			pods: []corev1.Pod{
				pod("rfr-0", "10.0.0.1", false, corev1.PodRunning, false),
				pod("rfr-1", "10.0.0.2", true, corev1.PodRunning, false),
				pod("rfr-2", "10.0.0.3", false, corev1.PodRunning, false),
			},
			wantDemote: "10.0.0.2",
			wantStatus: "multiple masters detected, made the other masters replicas of rfr-1",
		},
		{
			name: "a labelled master pod that does not run or is in deletion does not count",
			pods: []corev1.Pod{
				pod("rfr-0", "10.0.0.1", true, corev1.PodPending, false),
				pod("rfr-1", "10.0.0.2", true, corev1.PodRunning, false),
				pod("rfr-2", "10.0.0.3", true, corev1.PodRunning, true),
			},
			wantDemote: "10.0.0.2",
			wantStatus: "multiple masters detected, made the other masters replicas of rfr-1",
		},
		{
			name: "the demotion fails",
			pods: []corev1.Pod{
				pod("rfr-0", "10.0.0.1", false, corev1.PodRunning, false),
				pod("rfr-1", "10.0.0.2", true, corev1.PodRunning, false),
			},
			setErr:      setErr,
			wantDemote:  "10.0.0.2",
			wantErr:     setErr,
			wantMessage: "unable to make the other masters replicas of the labelled master",
		},
		{
			name: "two pods are labelled master",
			pods: []corev1.Pod{
				pod("rfr-0", "10.0.0.1", true, corev1.PodRunning, false),
				pod("rfr-1", "10.0.0.2", true, corev1.PodRunning, false),
			},
			wantMessage: "multiple masters detected, fix manually",
		},
		{
			name: "no pod is labelled master",
			pods: []corev1.Pod{
				pod("rfr-0", "10.0.0.1", false, corev1.PodRunning, false),
				pod("rfr-1", "10.0.0.2", false, corev1.PodRunning, false),
			},
			wantMessage: "multiple masters detected, fix manually",
		},
		{
			name:        "listing pods fails",
			podsErr:     listErr,
			wantErr:     listErr,
			wantMessage: "unable to look up the master pod",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rf := operatorManagedRF()
			mrfc := &mRFService.RedisFailoverCheck{}
			mrfh := &mRFService.RedisFailoverHeal{}
			mrfh.On("ApplyPassword", mock.Anything, mock.Anything, mock.Anything).Maybe().Return(true, nil)
			mrfh.On("ApplySentinelPassword", mock.Anything, mock.Anything).Maybe().Return(true, nil)
			mk := &mK8SService.Services{}
			mk.On("UpdateRedisFailoverStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return()
			mk.On("GetStatefulSetPods", rf.Namespace, rfservice.GetRedisName(rf)).Once().Return(&corev1.PodList{Items: test.pods}, test.podsErr)
			mrfc.On("IsRedisRunningQuorum", rf).Once().Return(true)
			mrfc.On("GetNumberMasters", rf).Once().Return(2, nil)
			if test.wantDemote != "" {
				mrfh.On("SetMasterOnAll", test.wantDemote, rf).Once().Return(test.setErr)
			}

			handler := rfOperator.NewRedisFailoverHandler(generateConfig(), &mRFService.RedisFailoverClient{}, mrfc, mrfh, mk, metrics.Dummy, log.Dummy)
			err := handler.CheckAndHeal(rf)

			switch {
			case test.wantErr != nil:
				assert.ErrorIs(t, err, test.wantErr)
			case test.wantMessage != "":
				assert.EqualError(t, err, test.wantMessage)
			default:
				assert.NoError(t, err)
			}
			wantStatus := test.wantStatus
			if wantStatus == "" {
				wantStatus = test.wantMessage
			}
			assert.Equal(t, v1.NotHealthyState, rf.Status.State)
			assert.Equal(t, wantStatus, rf.Status.Message)
			mrfc.AssertExpectations(t)
			mrfh.AssertExpectations(t)
			mk.AssertExpectations(t)
		})
	}
}

// A SENTINEL RESET while the master pod stops can leave Sentinel without a
// replica to promote, so the Sentinel heal waits until the master pod exits.
func TestSentinelModeDoesNotHealSentinelsWhileTheMasterStops(t *testing.T) {
	const (
		master   = "0.0.0.0"
		sentinel = "1.1.1.1"
		port     = "0"
	)
	tests := []struct {
		name       string
		masterPod  corev1.Pod
		listErr    error
		wantHeal   bool
		wantErr    bool
		wantStatus v1.RedisFailoverStatus
	}{
		{
			name:       "the master pod is stopping and still ready",
			masterPod:  masterPod(redisPod("1", true, true)),
			wantStatus: v1.RedisFailoverStatus{State: v1.NotHealthyState, Message: "waiting for the stopping master pod to exit"},
		},
		{
			name:       "a stopping master pod that is not ready (lost node) does not block",
			masterPod:  masterPod(redisPod("1", false, true)),
			wantHeal:   true,
			wantStatus: v1.RedisFailoverStatus{State: v1.HealthyState},
		},
		{
			name:       "listing the pods fails",
			masterPod:  masterPod(redisPod("1", true, false)),
			listErr:    errors.New("list err"),
			wantErr:    true,
			wantStatus: v1.RedisFailoverStatus{State: v1.NotHealthyState, Message: "unable to check whether the master is stopping"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			rf := generateRF(false, false)
			pods := &corev1.PodList{Items: []corev1.Pod{redisPod("1", true, false), redisPod("1", true, false), test.masterPod}}
			// The rollout is done; the next pod list is the check of the guard.
			rolloutDone := false

			mk := &mK8SService.Services{}
			mk.On("GetStatefulSetPods", mock.Anything, mock.Anything).Return(func(string, string) (*corev1.PodList, error) {
				if rolloutDone && test.listErr != nil {
					return nil, test.listErr
				}
				return pods, nil
			})
			mk.On("UpdateRedisFailoverStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return()
			mrfc := &mRFService.RedisFailoverCheck{}
			mrfh := &mRFService.RedisFailoverHeal{}
			mrfh.On("ApplyPassword", mock.Anything, mock.Anything, mock.Anything).Maybe().Return(true, nil)
			mrfh.On("ApplySentinelPassword", mock.Anything, mock.Anything).Maybe().Return(true, nil)
			mrfc.On("IsRedisRunningQuorum", rf).Once().Return(true)
			mrfc.On("IsSentinelRunningQuorum", rf).Once().Return(true)
			mrfc.On("GetNumberMasters", rf).Once().Return(1, nil)
			mrfc.On("GetMasterIP", rf).Twice().Return(master, nil)
			mrfc.On("CheckAllSlavesFromMaster", master, rf).Once().Return(nil)
			mrfc.On("GetRedisesIPs", rf).Twice().Return([]string{master}, nil)
			mrfh.On("SetRedisCustomConfig", master, rf).Once().Return(nil)
			mrfc.On("GetStatefulSetUpdateRevision", rf).Once().Return("1", nil)
			mrfc.On("GetRedisesSlavesPods", rf).Once().Return([]string{}, nil)
			mrfc.On("GetRedisesMasterPod", rf).Once().Return(master, nil)
			mrfc.On("GetRedisRevisionHash", master, rf).Once().Run(func(mock.Arguments) { rolloutDone = true }).Return("1", nil)
			if test.wantHeal {
				mrfc.On("GetSentinelsIPs", rf).Once().Return([]string{sentinel}, nil)
				mrfc.On("CheckSentinelMonitor", sentinel, master, port).Once().Return(nil)
				mrfc.On("CheckSentinelNumberInMemory", sentinel, rf).Once().Return(nil)
				mrfc.On("CheckSentinelSlavesNumberInMemory", sentinel, rf).Once().Return(nil)
				mrfh.On("SetSentinelCustomConfig", sentinel, rf).Once().Return(nil)
			}

			handler := rfOperator.NewRedisFailoverHandler(generateConfig(), &mRFService.RedisFailoverClient{}, mrfc, mrfh, mk, metrics.Dummy, log.Dummy)
			err := handler.CheckAndHeal(rf)

			if test.wantErr {
				assert.ErrorIs(err, test.listErr)
			} else {
				assert.NoError(err)
			}
			assert.Equal(test.wantStatus.State, rf.Status.State)
			assert.Equal(test.wantStatus.Message, rf.Status.Message)
			if !test.wantHeal {
				mrfc.AssertNotCalled(t, "GetSentinelsIPs", mock.Anything)
				mrfh.AssertNotCalled(t, "RestoreSentinel", mock.Anything)
			}
			mrfc.AssertExpectations(t)
			mrfh.AssertExpectations(t)
		})
	}
}
