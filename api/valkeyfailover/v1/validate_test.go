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
		rfValkeyCustomConfig   []string
		rfSentinelCustomConfig []string
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
			name:                 "Appends applied custom config to default initial values",
			rfName:               "test",
			rfValkeyCustomConfig: []string{"tcp-keepalive 60"},
		},
		{
			name:                  "Appends applied custom config to default initial values when bootstrapping",
			rfName:                "test",
			rfValkeyCustomConfig:  []string{"tcp-keepalive 60"},
			rfBootstrapNode:       &BootstrapSettings{Host: "127.0.0.1"},
			expectedBootstrapNode: &BootstrapSettings{Host: "127.0.0.1", Port: "6379"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			rf := generateValkeyFailover(test.rfName, test.rfBootstrapNode)
			rf.Spec.Valkey.CustomConfig = test.rfValkeyCustomConfig
			rf.Spec.Sentinel.CustomConfig = test.rfSentinelCustomConfig

			err := rf.Validate()

			if test.expectedError == "" {
				assert.NoError(err)

				expectedValkeyCustomConfig := []string{
					"replica-priority 100",
				}

				if test.rfBootstrapNode != nil {
					expectedValkeyCustomConfig = []string{
						"replica-priority 0",
					}
				}

				expectedValkeyCustomConfig = append(expectedValkeyCustomConfig, test.rfValkeyCustomConfig...)
				expectedSentinelCustomConfig := defaultSentinelCustomConfig
				if len(test.rfSentinelCustomConfig) > 0 {
					expectedSentinelCustomConfig = test.rfSentinelCustomConfig
				}

				expectedRF := &ValkeyFailover{
					ObjectMeta: metav1.ObjectMeta{
						Name:      test.rfName,
						Namespace: "namespace",
					},
					Spec: ValkeyFailoverSpec{
						Valkey: ValkeySettings{
							Image:    defaultImage,
							Replicas: defaultValkeyNumber,
							Port:     defaultValkeyPort,
							Exporter: Exporter{
								Image: defaultExporterImage,
							},
							CustomConfig: expectedValkeyCustomConfig,
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
					Status: ValkeyFailoverStatus{
						State:       HealthyState,
						LastChanged: "",
						Message:     "",
					},
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
