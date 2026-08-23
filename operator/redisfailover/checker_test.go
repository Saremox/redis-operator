package redisfailover_test

import (
	"errors"
	"fmt"
	v1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	mRFService "github.com/saremox/redis-operator/mocks/operator/redisfailover/service"
	mK8SService "github.com/saremox/redis-operator/mocks/service/k8s"
	rfOperator "github.com/saremox/redis-operator/operator/redisfailover"
	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
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
			mk := &mK8SService.Services{}
			mrfs := &mRFService.RedisFailoverClient{}
			mrfc := &mRFService.RedisFailoverCheck{}
			mrfh := &mRFService.RedisFailoverHeal{}

			if test.redisCheckNumberOK {
				mrfc.On("IsRedisRunning", rf).Once().Return(true)
			} else {
				continueTests = false
				mrfc.On("IsRedisRunning", rf).Once().Return(false)
			}

			if allowSentinels {
				mrfc.On("IsSentinelRunning", rf).Once().Return(true)
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

			if expErr {
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
	sentinelDisabled := false
	rf.Spec.Sentinel.Enabled = &sentinelDisabled
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
		wantErr     bool
		wantErrIs   error
		wantState   string
		wantMessage string
	}{
		{
			name: "redis not running - waits for statefulset reconcile",
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("IsRedisRunning", rf).Once().Return(false)
			},
			wantErr:     false,
			wantState:   v1.NotHealthyState,
			wantMessage: "not all replicas running",
		},
		{
			name: "GetNumberMasters errors",
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("IsRedisRunning", rf).Once().Return(true)
				mrfc.On("GetNumberMasters", rf).Once().Return(0, errors.New("num masters err"))
			},
			wantErr:     true,
			wantState:   v1.NotHealthyState,
			wantMessage: "unable to get number of masters",
		},
		{
			name: "no master - best replica found and promoted successfully",
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("IsRedisRunning", rf).Once().Return(true)
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
				mrfc.On("IsRedisRunning", rf).Once().Return(true)
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
				mrfc.On("IsRedisRunning", rf).Once().Return(true)
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
				mrfc.On("IsRedisRunning", rf).Once().Return(true)
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
				mrfc.On("IsRedisRunning", rf).Once().Return(true)
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
				mrfc.On("IsRedisRunning", rf).Once().Return(true)
				mrfc.On("GetNumberMasters", rf).Once().Return(1, nil)
				mrfc.On("CheckMasterHealth", rf).Once().Return(false, "", errors.New("health check err"))
			},
			wantErr:     true,
			wantState:   v1.NotHealthyState,
			wantMessage: "unable to check master health",
		},
		{
			name: "single master - unhealthy, replica found and promoted successfully",
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("IsRedisRunning", rf).Once().Return(true)
				mrfc.On("GetNumberMasters", rf).Once().Return(1, nil)
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
				mrfc.On("IsRedisRunning", rf).Once().Return(true)
				mrfc.On("GetNumberMasters", rf).Once().Return(1, nil)
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
				mrfc.On("IsRedisRunning", rf).Once().Return(true)
				mrfc.On("GetNumberMasters", rf).Once().Return(1, nil)
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
				mrfc.On("IsRedisRunning", rf).Once().Return(true)
				mrfc.On("GetNumberMasters", rf).Once().Return(1, nil)
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
				mrfc.On("IsRedisRunning", rf).Once().Return(true)
				mrfc.On("GetNumberMasters", rf).Once().Return(1, nil)
				mrfc.On("CheckMasterHealth", rf).Once().Return(true, master, nil)
				mrfc.On("CheckAllSlavesFromMaster", master, rf).Once().Return(nil)
				setupSharedSuccess(mrfc, mrfh, rf)
			},
			wantErr:   false,
			wantState: v1.HealthyState,
		},
		{
			name: "single master - healthy, slaves fixed, config and pods updated",
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("IsRedisRunning", rf).Once().Return(true)
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
				mrfc.On("IsRedisRunning", rf).Once().Return(true)
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
				mrfc.On("IsRedisRunning", rf).Once().Return(true)
				mrfc.On("GetNumberMasters", rf).Once().Return(2, nil)
			},
			wantErr:     true,
			wantState:   v1.NotHealthyState,
			wantMessage: "multiple masters detected, fix manually",
		},
		{
			name: "single master - healthy, applying custom config fails",
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("IsRedisRunning", rf).Once().Return(true)
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
				mrfc.On("IsRedisRunning", rf).Once().Return(true)
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
			mk := &mK8SService.Services{}
			mrfs := &mRFService.RedisFailoverClient{}
			mrfc := &mRFService.RedisFailoverCheck{}
			mrfh := &mRFService.RedisFailoverHeal{}

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
			name: "redis not running - waits for statefulset reconcile",
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("IsRedisRunning", rf).Once().Return(false)
			},
			wantErr:     false,
			wantState:   v1.NotHealthyState,
			wantMessage: "not all replicas running",
		},
		{
			name: "sentinel not running - waits for deployment reconcile",
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("IsRedisRunning", rf).Once().Return(true)
				mrfc.On("IsSentinelRunning", rf).Once().Return(false)
			},
			wantErr:     false,
			wantState:   v1.NotHealthyState,
			wantMessage: "not all replicas running",
		},
		{
			name: "GetNumberMasters errors",
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("IsRedisRunning", rf).Once().Return(true)
				mrfc.On("IsSentinelRunning", rf).Once().Return(true)
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
				mrfc.On("IsRedisRunning", rf).Once().Return(true)
				mrfc.On("IsSentinelRunning", rf).Once().Return(true)
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
				mrfc.On("IsRedisRunning", rf).Once().Return(true)
				mrfc.On("IsSentinelRunning", rf).Once().Return(true)
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
				mrfc.On("IsRedisRunning", rf).Once().Return(true)
				mrfc.On("IsSentinelRunning", rf).Once().Return(true)
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
				mrfc.On("IsRedisRunning", rf).Once().Return(true)
				mrfc.On("IsSentinelRunning", rf).Once().Return(true)
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
				mrfc.On("IsRedisRunning", rf).Once().Return(true)
				mrfc.On("IsSentinelRunning", rf).Once().Return(true)
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
				mrfc.On("IsRedisRunning", rf).Once().Return(true)
				mrfc.On("IsSentinelRunning", rf).Once().Return(true)
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
				mrfc.On("IsRedisRunning", rf).Once().Return(true)
				mrfc.On("IsSentinelRunning", rf).Once().Return(true)
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
				mrfc.On("IsRedisRunning", rf).Once().Return(true)
				mrfc.On("IsSentinelRunning", rf).Once().Return(true)
				mrfc.On("GetNumberMasters", rf).Once().Return(1, nil)
				mrfc.On("GetMasterIP", rf).Once().Return(master, nil)
				mrfc.On("CheckAllSlavesFromMaster", master, rf).Once().Return(errors.New("wrong master"))
				mrfc.On("GetMasterIP", rf).Once().Return(master, nil)
				mrfh.On("SetMasterOnAll", master, rf).Once().Return(errors.New("set fail"))
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
				mrfc.On("IsRedisRunning", rf).Once().Return(true)
				mrfc.On("IsSentinelRunning", rf).Once().Return(true)
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
				mrfc.On("IsRedisRunning", rf).Once().Return(true)
				mrfc.On("IsSentinelRunning", rf).Once().Return(true)
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
			name: "GetSentinelsIPs fails",
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("IsRedisRunning", rf).Once().Return(true)
				mrfc.On("IsSentinelRunning", rf).Once().Return(true)
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
				mrfc.On("IsRedisRunning", rf).Once().Return(true)
				mrfc.On("IsSentinelRunning", rf).Once().Return(true)
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
				mrfc.On("IsRedisRunning", rf).Once().Return(true)
				mrfc.On("IsSentinelRunning", rf).Once().Return(true)
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
				mrfc.On("GetMasterIP", rf).Once().Return(master, nil)
				mrfh.On("NewSentinelMonitor", sentinel, master, rf).Once().Return(errors.New("new monitor err"))
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
				mrfc.On("IsRedisRunning", rf).Once().Return(true)
				mrfc.On("IsSentinelRunning", rf).Once().Return(true)
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
				mrfc.On("IsRedisRunning", rf).Once().Return(true)
				mrfc.On("IsSentinelRunning", rf).Once().Return(true)
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
				mrfc.On("IsRedisRunning", rf).Once().Return(true)
				mrfc.On("IsSentinelRunning", rf).Once().Return(true)
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
			mk := &mK8SService.Services{}
			mrfs := &mRFService.RedisFailoverClient{}
			mrfc := &mRFService.RedisFailoverCheck{}
			mrfh := &mRFService.RedisFailoverHeal{}

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
			mk := &mK8SService.Services{}
			mrfs := &mRFService.RedisFailoverClient{}
			mrfc := &mRFService.RedisFailoverCheck{}
			mrfh := &mRFService.RedisFailoverHeal{}

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

			for _, pod := range test.pods {
				if !pod.master {
					mrfc.On("CheckRedisSlavesReady", pod.pod.Status.PodIP, rf).Once().Return(pod.ready, nil)
				}
				if !pod.ready {
					next = false
					break
				}
			}
			mrfh := &mRFService.RedisFailoverHeal{}

			if next {
				replicas := []string{"slave1", "slave2"}
				if test.bootstrapping || test.noMaster {
					replicas = append(replicas, "slave3")
				}
				mrfc.On("GetStatefulSetUpdateRevision", rf).Once().Return(test.ssVersion, nil)
				mrfc.On("GetRedisesSlavesPods", rf).Once().Return(replicas, nil)

				for _, pod := range test.pods {
					mrfc.On("GetRedisRevisionHash", pod.pod.Name, rf).Once().Return(pod.pod.Labels[appsv1.ControllerRevisionHashLabelKey], nil)
					if pod.pod.Labels[appsv1.ControllerRevisionHashLabelKey] != test.ssVersion {
						mrfh.On("DeletePod", pod.pod.Name, rf).Once().Return(nil)
						if pod.master == false {
							next = false
							break
						}
					}
				}
				fmt.Printf("%v - %v\n", test.name, next)
				if next && !test.bootstrapping {
					if test.noMaster {
						mrfc.On("GetRedisesMasterPod", rf).Once().Return("", errors.New(""))
					} else {
						mrfc.On("GetRedisesMasterPod", rf).Once().Return("master", nil)
					}
				}
			}

			mk := &mK8SService.Services{}

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
			name: "DeletePod fails for a stale master pod",
			setup: func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
				mrfc.On("GetRedisesIPs", rf).Once().Return([]string{master}, nil)
				mrfc.On("GetMasterIP", rf).Once().Return(master, nil)
				mrfc.On("GetStatefulSetUpdateRevision", rf).Once().Return("1", nil)
				mrfc.On("GetRedisesSlavesPods", rf).Once().Return([]string{}, nil)
				mrfc.On("GetRedisesMasterPod", rf).Once().Return(master, nil)
				mrfc.On("GetRedisRevisionHash", master, rf).Once().Return("stale", nil)
				mrfh.On("DeletePod", master, rf).Once().Return(errors.New("delete master err"))
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertTest := assert.New(t)

			rf := generateRF(false, false)

			config := generateConfig()
			mk := &mK8SService.Services{}
			mrfs := &mRFService.RedisFailoverClient{}
			mrfc := &mRFService.RedisFailoverCheck{}
			mrfh := &mRFService.RedisFailoverHeal{}

			test.setup(mrfc, mrfh, rf)

			handler := rfOperator.NewRedisFailoverHandler(config, mrfs, mrfc, mrfh, mk, metrics.Dummy, log.Dummy)
			err := handler.UpdateRedisesPods(rf)

			assertTest.Error(err)

			mrfc.AssertExpectations(t)
			mrfh.AssertExpectations(t)
		})
	}
}
