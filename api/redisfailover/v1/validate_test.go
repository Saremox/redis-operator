package v1

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestValidate(t *testing.T) {
	tests := []struct {
		name                   string
		rfName                 string
		rfBootstrapNode        *BootstrapSettings
		rfRedisCustomConfig    []string
		rfSentinelCustomConfig []string
		rfCommandRenames       []RedisCommandRename
		expectedError          string
		expectedBootstrapNode  *BootstrapSettings
	}{
		{
			name:   "populates default values",
			rfName: "test",
		},
		{
			name:          "errors on too long of name",
			rfName:        "some-super-absurdely-unnecessarily-long-name-that-will-most-definitely-fail",
			expectedError: "name length can't be higher than 48",
		},
		{
			name:                   "SentinelCustomConfig provided",
			rfName:                 "test",
			rfSentinelCustomConfig: []string{"failover-timeout 500"},
		},
		{
			name:            "BootstrapNode provided without a host",
			rfName:          "test",
			rfBootstrapNode: &BootstrapSettings{},
			expectedError:   "BootstrapNode must include a host when provided",
		},
		{
			name:   "SentinelCustomConfig provided",
			rfName: "test",
		},
		{
			name:                  "Populates default bootstrap port when valid",
			rfName:                "test",
			rfBootstrapNode:       &BootstrapSettings{Host: "127.0.0.1"},
			expectedBootstrapNode: &BootstrapSettings{Host: "127.0.0.1", Port: "6379"},
		},
		{
			name:                  "Allows for specifying boostrap port",
			rfName:                "test",
			rfBootstrapNode:       &BootstrapSettings{Host: "127.0.0.1", Port: "6380"},
			expectedBootstrapNode: &BootstrapSettings{Host: "127.0.0.1", Port: "6380"},
		},
		{
			name:                "Appends applied custom config to default initial values",
			rfName:              "test",
			rfRedisCustomConfig: []string{"tcp-keepalive 60"},
		},
		{
			name:                  "Appends applied custom config to default initial values when bootstrapping",
			rfName:                "test",
			rfRedisCustomConfig:   []string{"tcp-keepalive 60"},
			rfBootstrapNode:       &BootstrapSettings{Host: "127.0.0.1"},
			expectedBootstrapNode: &BootstrapSettings{Host: "127.0.0.1", Port: "6379"},
		},
		{
			name:             "Allows valid command renames, including disabling a command",
			rfName:           "test",
			rfCommandRenames: []RedisCommandRename{{From: "CONFIG", To: "MYCONFIG"}, {From: "FLUSHALL", To: ""}},
		},
		{
			name:             "Rejects command rename injection via quotes in from",
			rfName:           "test",
			rfCommandRenames: []RedisCommandRename{{From: `CONFIG"` + "\n" + `slave-read-only no` + "\n" + `rename-command "FLUSHALL`, To: `""`}},
			expectedError:    `customCommandRenames: invalid "from" command name "CONFIG\"\nslave-read-only no\nrename-command \"FLUSHALL", must match ^[A-Za-z_]+$`,
		},
		{
			name:             "Rejects command rename injection via quotes in to",
			rfName:           "test",
			rfCommandRenames: []RedisCommandRename{{From: "CONFIG", To: `" shutdown nosave #`}},
			expectedError:    `customCommandRenames: invalid "to" command name "\" shutdown nosave #", must match ^[A-Za-z_]+$`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			rf := generateRedisFailover(test.rfName, test.rfBootstrapNode)
			rf.Spec.Redis.CustomConfig = test.rfRedisCustomConfig
			rf.Spec.Sentinel.CustomConfig = test.rfSentinelCustomConfig
			rf.Spec.Redis.CustomCommandRenames = test.rfCommandRenames

			err := rf.Validate()

			if test.expectedError == "" {
				assert.NoError(err)

				expectedRedisCustomConfig := []string{
					"replica-priority 100",
				}

				if test.rfBootstrapNode != nil {
					expectedRedisCustomConfig = []string{
						"replica-priority 0",
					}
				}

				expectedRedisCustomConfig = append(expectedRedisCustomConfig, test.rfRedisCustomConfig...)
				expectedSentinelCustomConfig := defaultSentinelCustomConfig
				if len(test.rfSentinelCustomConfig) > 0 {
					expectedSentinelCustomConfig = test.rfSentinelCustomConfig
				}

				expectedRF := &RedisFailover{
					ObjectMeta: metav1.ObjectMeta{
						Name:      test.rfName,
						Namespace: "namespace",
					},
					Spec: RedisFailoverSpec{
						Redis: RedisSettings{
							Image:    defaultImage,
							Replicas: defaultRedisNumber,
							Port:     defaultRedisPort,
							Exporter: Exporter{
								Image: defaultExporterImage,
							},
							CustomConfig:         expectedRedisCustomConfig,
							CustomCommandRenames: test.rfCommandRenames,
						},
						Sentinel: SentinelSettings{
							Image:        defaultImage,
							Replicas:     defaultSentinelNumber,
							CustomConfig: expectedSentinelCustomConfig,
							Exporter: Exporter{
								Image: defaultSentinelExporterImage,
							},
						},
						BootstrapNode: test.expectedBootstrapNode,
					},
					// Validate() must not touch Status: CheckAndHeal (checker.go)
					// captures rf.Status.State as "oldState" before resetting it
					// itself, and a premature reset here would make that always
					// read back as HealthyState regardless of what was actually
					// persisted, corrupting status.lastChanged on every reconcile
					// of an ongoing outage.
					Status: RedisFailoverStatus{},
				}
				assert.Equal(expectedRF, rf)
			} else {
				if assert.Error(err) {
					assert.Contains(test.expectedError, err.Error())
				}
			}
		})
	}
}

// TestValidatePreservesExistingStatus guards against Validate() reintroducing
// a Status reset. operator/redisfailover/checker.go's CheckAndHeal captures
// rf.Status.State as "oldState" immediately on entry, before resetting it
// itself and later comparing against the freshly computed state to decide
// whether to bump status.lastChanged. Handle() (operator/redisfailover/handler.go)
// calls Validate() before CheckAndHeal, so if Validate() ever reset Status
// again, "oldState" would always read back as whatever Validate() set it to,
// regardless of what was actually persisted - making lastChanged bump on
// every single reconcile of an ongoing outage instead of only at the real
// transition.
func TestValidatePreservesExistingStatus(t *testing.T) {
	assert := assert.New(t)

	rf := generateRedisFailover("test", nil)
	rf.Status = RedisFailoverStatus{
		State:       NotHealthyState,
		Message:     "unable to update redis pods",
		LastChanged: "2026-01-01T00:00:00Z",
	}

	err := rf.Validate()

	assert.NoError(err)
	assert.Equal(RedisFailoverStatus{
		State:       NotHealthyState,
		Message:     "unable to update redis pods",
		LastChanged: "2026-01-01T00:00:00Z",
	}, rf.Status)
}
