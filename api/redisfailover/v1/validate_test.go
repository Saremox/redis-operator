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
			rfCommandRenames: []RedisCommandRename{{From: "KEYS", To: "MYKEYS"}, {From: "FLUSHALL", To: ""}},
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
			rfCommandRenames: []RedisCommandRename{{From: "KEYS", To: `" shutdown nosave #`}},
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

func TestValidateExporterPort(t *testing.T) {
	for _, port := range []int32{0, 1, 65535} {
		rf := generateRedisFailover("test", nil)
		rf.Spec.Redis.Exporter.Port = port
		rf.Spec.Sentinel.Exporter.Port = port
		assert.NoError(t, rf.Validate(), "port %d", port)
	}

	rf := generateRedisFailover("test", nil)
	rf.Spec.Redis.Exporter.Port = -1
	assert.EqualError(t, rf.Validate(), "redis.exporter.port -1 must be between 1 and 65535, or 0 for the default")

	rf = generateRedisFailover("test", nil)
	rf.Spec.Sentinel.Exporter.Port = 65536
	assert.EqualError(t, rf.Validate(), "sentinel.exporter.port 65536 must be between 1 and 65535, or 0 for the default")
}

func TestValidateCommandRenames(t *testing.T) {
	tests := []struct {
		name            string
		renames         []RedisCommandRename
		customConfig    []string
		sentinelEnabled bool
		expectedError   string
	}{
		{
			name:          "Rejects a rename of a command that the operator sends, in any case",
			renames:       []RedisCommandRename{{From: "FLUSHALL", To: ""}, {From: "config", To: "MYCONFIG"}},
			expectedError: `customCommandRenames: "config" cannot be renamed, because the operator, the pod scripts or the replicas send it (AUTH, CLIENT, CONFIG, INFO, PING, PSYNC, REPLCONF, REPLICAOF, SLAVEOF)`,
		},
		{
			name:          "Rejects disabling a command that the pod scripts send",
			renames:       []RedisCommandRename{{From: "Replicaof", To: ""}},
			expectedError: `customCommandRenames: "Replicaof" cannot be renamed, because the operator, the pod scripts or the replicas send it (AUTH, CLIENT, CONFIG, INFO, PING, PSYNC, REPLCONF, REPLICAOF, SLAVEOF)`,
		},
		{
			name:          "Rejects a rename of a command that the replicas send",
			renames:       []RedisCommandRename{{From: "psync", To: "MYPSYNC"}},
			expectedError: `customCommandRenames: "psync" cannot be renamed, because the operator, the pod scripts or the replicas send it (AUTH, CLIENT, CONFIG, INFO, PING, PSYNC, REPLCONF, REPLICAOF, SLAVEOF)`,
		},
		{
			name:    "Allows a rename of ACL and SAVE without an aclfile",
			renames: []RedisCommandRename{{From: "ACL", To: ""}, {From: "save", To: "MYSAVE"}},
		},
		{
			name:          "Rejects a rename of ACL with an aclfile",
			renames:       []RedisCommandRename{{From: "acl", To: ""}},
			customConfig:  []string{"aclfile /data/users.acl"},
			expectedError: `customCommandRenames: "acl" cannot be renamed, because the operator sends ACL LOAD to apply the aclfile of customConfig`,
		},
		{
			name:            "Rejects a rename of a command that Sentinel sends",
			renames:         []RedisCommandRename{{From: "Publish", To: ""}},
			sentinelEnabled: true,
			expectedError:   `customCommandRenames: "Publish" cannot be renamed, because Sentinel sends it (EXEC, MULTI, PUBLISH, SUBSCRIBE)`,
		},
		{
			name:    "Allows a rename of a Sentinel command without Sentinels",
			renames: []RedisCommandRename{{From: "PUBLISH", To: ""}, {From: "EXEC", To: "MYEXEC"}},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rf := generateRedisFailoverWithSentinel("test", &test.sentinelEnabled)
			rf.Spec.Redis.CustomConfig = test.customConfig
			rf.Spec.Redis.CustomCommandRenames = test.renames

			err := rf.Validate()

			if test.expectedError == "" {
				assert.NoError(t, err)
			} else {
				assert.EqualError(t, err, test.expectedError)
			}
		})
	}
}
