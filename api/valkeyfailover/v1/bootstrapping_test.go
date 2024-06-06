package v1

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func generateValkeyFailover(name string, bootstrapNode *BootstrapSettings) *ValkeyFailover {
	return &ValkeyFailover{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "namespace",
		},
		Spec: ValkeyFailoverSpec{
			BootstrapNode: bootstrapNode,
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
			rf := generateValkeyFailover("test", test.bootstrapSettings)
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
			rf := generateValkeyFailover("test", test.bootstrapSettings)
			assert.Equal(t, test.expectation, rf.SentinelsAllowed())
		})
	}
}
