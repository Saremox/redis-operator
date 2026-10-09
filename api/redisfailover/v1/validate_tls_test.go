package v1

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

func tlsRF() *RedisFailover {
	return &RedisFailover{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns"},
		Spec:       RedisFailoverSpec{TLS: &TLSSettings{SecretName: "redis-tls"}},
	}
}

// TestValidateRejectsTLS checks that a valid spec.tls is still rejected while
// the operator cannot renew the certificates of a running RedisFailover.
func TestValidateRejectsTLS(t *testing.T) {
	assert.EqualError(t, tlsRF().Validate(), errTLSNotAvailable.Error())
}

func TestValidateTLS(t *testing.T) {
	tlsAvailable = true
	t.Cleanup(func() { tlsAvailable = false })

	tests := []struct {
		name    string
		modify  func(rf *RedisFailover)
		wantErr string
	}{
		{name: "defaults"},
		{
			name:    "no secret",
			modify:  func(rf *RedisFailover) { rf.Spec.TLS.SecretName = "" },
			wantErr: `tls.secretName "" must be the name of a Secret`,
		},
		{
			name:    "ca without a source",
			modify:  func(rf *RedisFailover) { rf.Spec.TLS.CA = &TLSCASource{} },
			wantErr: "tls.ca must set one of secretName and configMapName",
		},
		{
			name:    "ca with two sources",
			modify:  func(rf *RedisFailover) { rf.Spec.TLS.CA = &TLSCASource{SecretName: "a", ConfigMapName: "b"} },
			wantErr: "tls.ca must set one of secretName and configMapName",
		},
		{
			name:    "ca with an invalid name",
			modify:  func(rf *RedisFailover) { rf.Spec.TLS.CA = &TLSCASource{ConfigMapName: "Bad_Name"} },
			wantErr: `tls.ca names "Bad_Name", which is not a valid object name`,
		},
		{
			name:    "ca with an invalid key",
			modify:  func(rf *RedisFailover) { rf.Spec.TLS.CA = &TLSCASource{SecretName: "ca", Key: "a/b"} },
			wantErr: `tls.ca.key "a/b" is not a valid key`,
		},
		{
			name:    "port out of range",
			modify:  func(rf *RedisFailover) { rf.Spec.TLS.Port = 70000 },
			wantErr: "tls.port 70000 must be between 1 and 65535, or 0 for the default",
		},
		{
			name:    "port of redis",
			modify:  func(rf *RedisFailover) { rf.Spec.Redis.Port = 6380 },
			wantErr: "tls.port 6380 must be different from redis.port",
		},
		{
			name: "default port of the exporter",
			modify: func(rf *RedisFailover) {
				rf.Spec.Redis.Exporter.Enabled = true
				rf.Spec.TLS.Port = 9121
			},
			wantErr: "tls.port 9121 must be different from the port of the Redis exporter",
		},
		{
			name: "custom port of the exporter",
			modify: func(rf *RedisFailover) {
				rf.Spec.Redis.Exporter.Enabled = true
				rf.Spec.Redis.Exporter.Port = 6380
			},
			wantErr: "tls.port 6380 must be different from the port of the Redis exporter",
		},
		{
			name: "exporter port without the exporter",
			modify: func(rf *RedisFailover) {
				rf.Spec.TLS.Port = 9121
			},
		},
		{
			name:    "plaintextPort",
			modify:  func(rf *RedisFailover) { rf.Spec.TLS.PlaintextPort = "Off" },
			wantErr: `tls.plaintextPort "Off" must be Enabled or Disabled`,
		},
		{
			name:    "clientAuth",
			modify:  func(rf *RedisFailover) { rf.Spec.TLS.ClientAuth = "yes" },
			wantErr: `tls.clientAuth "yes" must be Required, Optional or None`,
		},
		{
			name:    "serverName",
			modify:  func(rf *RedisFailover) { rf.Spec.TLS.ServerName = "not a name" },
			wantErr: `tls.serverName "not a name" must be a DNS name`,
		},
		{
			name:    "sentinel",
			modify:  func(rf *RedisFailover) { rf.Spec.Sentinel.Enabled = ptr.To(true) },
			wantErr: "tls: the operator supports TLS only without Sentinel (sentinel.enabled false)",
		},
		{
			name:    "bootstrap",
			modify:  func(rf *RedisFailover) { rf.Spec.BootstrapNode = &BootstrapSettings{Host: "10.0.0.1"} },
			wantErr: "tls: bootstrapNode cannot be used with TLS",
		},
		{
			name:    "custom command",
			modify:  func(rf *RedisFailover) { rf.Spec.Redis.Command = []string{"redis-server"} },
			wantErr: "tls: redis.command cannot be used with TLS, because the operator adds the TLS arguments to the default command",
		},
		{
			name:    "customConfig port",
			modify:  func(rf *RedisFailover) { rf.Spec.Redis.CustomConfig = []string{"Port 7000"} },
			wantErr: `redis.customConfig cannot set "port" with TLS, because the operator sets it`,
		},
		{
			name:    "customConfig tls-replication",
			modify:  func(rf *RedisFailover) { rf.Spec.Redis.CustomConfig = []string{"tls-replication no"} },
			wantErr: `redis.customConfig cannot set "tls-replication" with TLS, because the operator sets it`,
		},
		{
			name:   "customConfig tls-protocols",
			modify: func(rf *RedisFailover) { rf.Spec.Redis.CustomConfig = []string{"tls-protocols TLSv1.2 TLSv1.3"} },
		},
		{
			name:    "customConfig tls-protocols without TLS 1.2",
			modify:  func(rf *RedisFailover) { rf.Spec.Redis.CustomConfig = []string{"tls-protocols TLSv1.3"} },
			wantErr: `redis.customConfig tls-protocols "TLSv1.3" must include TLSv1.2, because the operator connects with TLS 1.2`,
		},
		{
			name:    "user env with the prefix of the operator",
			modify:  func(rf *RedisFailover) { rf.Spec.Redis.Env = []corev1.EnvVar{{Name: "RFO_TLS_PORT", Value: "7000"}} },
			wantErr: `redis.env cannot set "RFO_TLS_PORT" with TLS, because the operator sets the RFO_TLS_ variables`,
		},
		{
			name:   "user env of the Bitnami image",
			modify: func(rf *RedisFailover) { rf.Spec.Redis.Env = []corev1.EnvVar{{Name: "REDIS_TLS_PORT", Value: "7000"}} },
		},
		{
			name:    "old image",
			modify:  func(rf *RedisFailover) { rf.Spec.Redis.Image = "redis:6.0.16-alpine" },
			wantErr: `tls needs Redis or Valkey 6.2 or later, and redis.image "redis:6.0.16-alpine" is older`,
		},
		{
			name:    "old major",
			modify:  func(rf *RedisFailover) { rf.Spec.Redis.Image = "registry:5000/redis:5" },
			wantErr: `tls needs Redis or Valkey 6.2 or later, and redis.image "registry:5000/redis:5" is older`,
		},
		{
			name:   "6.2",
			modify: func(rf *RedisFailover) { rf.Spec.Redis.Image = "redis:6.2.14" },
		},
		{
			name:   "major only",
			modify: func(rf *RedisFailover) { rf.Spec.Redis.Image = "redis:6" },
		},
		{
			name:   "unknown version",
			modify: func(rf *RedisFailover) { rf.Spec.Redis.Image = "registry:5000/redis@sha256:0123" },
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rf := tlsRF()
			if test.modify != nil {
				test.modify(rf)
			}
			err := rf.Validate()
			if test.wantErr != "" {
				assert.EqualError(t, err, test.wantErr)
				return
			}
			assert.NoError(t, err)
		})
	}
}

func TestValidateTLSDefaults(t *testing.T) {
	tlsAvailable = true
	t.Cleanup(func() { tlsAvailable = false })

	rf := tlsRF()
	rf.Spec.TLS.CA = &TLSCASource{ConfigMapName: "bundle"}
	require.NoError(t, rf.Validate())
	assert.Equal(t, &TLSSettings{
		SecretName:    "redis-tls",
		CA:            &TLSCASource{ConfigMapName: "bundle", Key: "ca.crt"},
		Port:          6380,
		PlaintextPort: "Enabled",
		ClientAuth:    "Required",
		ServerName:    "rfrm-test.ns.svc",
	}, rf.Spec.TLS)

	set := &TLSSettings{
		SecretName:    "redis-tls",
		CA:            &TLSCASource{SecretName: "ca", Key: "bundle.pem"},
		Port:          7000,
		PlaintextPort: "Disabled",
		ClientAuth:    "None",
		ServerName:    "redis.example.com",
	}
	rf = tlsRF()
	rf.Spec.TLS = set.DeepCopy()
	require.NoError(t, rf.Validate())
	assert.Equal(t, set, rf.Spec.TLS)
}

func TestImageVersion(t *testing.T) {
	tests := []struct {
		image        string
		major, minor int
		ok           bool
	}{
		{"redis:7.2.12-alpine", 7, 2, true},
		{"valkey/valkey:8.1", 8, 1, true},
		{"redis:v6.2", 6, 2, true},
		{"redis:6", 6, -1, true},
		{"redis:latest", 0, 0, false},
		{"redis", 0, 0, false},
		{"registry:5000/redis", 0, 0, false},
		{"redis@sha256:abc", 0, 0, false},
		{"redis:7.0@sha256:abc", 7, 0, true},
	}
	for _, test := range tests {
		major, minor, ok := imageVersion(test.image)
		assert.Equal(t, test.ok, ok, test.image)
		if test.ok {
			assert.Equal(t, test.major, major, test.image)
			assert.Equal(t, test.minor, minor, test.image)
		}
	}
}

func TestAllowsTLS12(t *testing.T) {
	tests := map[string]bool{
		"TLSv1.2":               true,
		"tlsv1.2 TLSv1.3":       true,
		"TLSv1.3 TLSv1.2":       true,
		"TLSv1.3,TLSv1.2":       true,
		`"TLSv1.2 TLSv1.3"`:     true,
		`""`:                    true,
		"":                      true,
		"TLSv1.3":               false,
		"TLSv1.1 TLSv1.3":       false,
		"TLSv1.20":              false,
		"TLSv1.3 TLSv1.2.extra": false,
	}
	for value, want := range tests {
		assert.Equal(t, want, allowsTLS12(value), value)
	}
}
