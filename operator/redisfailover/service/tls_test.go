package service_test

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	mK8SService "github.com/saremox/redis-operator/mocks/service/k8s"
	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
)

var testNow = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

type testCert struct {
	cert *x509.Certificate
	key  crypto.Signer
	pem  []byte
}

func (c testCert) keyPEM(t *testing.T) []byte {
	der, err := x509.MarshalPKCS8PrivateKey(c.key)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

// newTestCert signs tmpl with parent, or makes it self-signed without parent.
func newTestCert(t *testing.T, tmpl *x509.Certificate, parent *testCert, key crypto.Signer) testCert {
	t.Helper()
	if key == nil {
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		require.NoError(t, err)
		key = k
	}
	if tmpl.SerialNumber == nil {
		tmpl.SerialNumber = big.NewInt(time.Now().UnixNano())
	}
	if tmpl.NotBefore.IsZero() {
		tmpl.NotBefore = testNow.Add(-time.Hour)
		tmpl.NotAfter = testNow.Add(24 * time.Hour)
	}
	signer, signerCert := key, tmpl
	if parent != nil {
		signer, signerCert = parent.key, parent.cert
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, signerCert, key.Public(), signer)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return testCert{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

func newTestCA(t *testing.T) testCert {
	return newTestCert(t, &x509.Certificate{
		Subject:               pkix.Name{CommonName: "test CA"},
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}, nil, nil)
}

func leafTemplate(names ...string) *x509.Certificate {
	return &x509.Certificate{
		Subject:     pkix.Name{CommonName: "redis"},
		DNSNames:    names,
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
}

const testServerName = "rfrm-test.testns.svc"

func TestCheckTLSMaterial(t *testing.T) {
	ca := newTestCA(t)
	otherCA := newTestCA(t)
	good := newTestCert(t, leafTemplate(testServerName), &ca, nil)

	intermediate := newTestCert(t, &x509.Certificate{
		Subject:               pkix.Name{CommonName: "intermediate"},
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}, &ca, nil)
	viaIntermediate := newTestCert(t, leafTemplate(testServerName), &intermediate, nil)

	serverOnly := leafTemplate(testServerName)
	serverOnly.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	noEKU := leafTemplate(testServerName)
	noEKU.ExtKeyUsage = nil
	expired := leafTemplate(testServerName)
	expired.NotBefore, expired.NotAfter = testNow.Add(-48*time.Hour), testNow.Add(-24*time.Hour)
	future := leafTemplate(testServerName)
	future.NotBefore, future.NotAfter = testNow.Add(time.Hour), testNow.Add(48*time.Hour)
	anyUsage := leafTemplate(testServerName)
	anyUsage.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageAny}
	certSignOnly := leafTemplate(testServerName)
	certSignOnly.KeyUsage = x509.KeyUsageCertSign
	smallKey, err := rsa.GenerateKey(rand.Reader, 1024)
	require.NoError(t, err)

	tests := []struct {
		name       string
		cert       testCert
		certPEM    []byte
		keyPEM     []byte
		ca         []byte
		clientAuth string
		wantErr    string
	}{
		{name: "valid", cert: good},
		{name: "chain in tls.crt", cert: viaIntermediate, certPEM: append(append([]byte{}, viaIntermediate.pem...), intermediate.pem...)},
		{name: "no extended key usage", cert: newTestCert(t, noEKU, &ca, nil)},
		{name: "server auth only with None", cert: newTestCert(t, serverOnly, &ca, nil), clientAuth: redisfailoverv1.TLSClientAuthNone},
		{
			name:    "server auth only with Required",
			cert:    newTestCert(t, serverOnly, &ca, nil),
			wantErr: "the certificate needs the client auth usage with clientAuth Required",
		},
		{
			name:       "server auth only with Optional",
			cert:       newTestCert(t, serverOnly, &ca, nil),
			clientAuth: redisfailoverv1.TLSClientAuthOptional,
			wantErr:    "the certificate needs the client auth usage with clientAuth Optional",
		},
		{
			name: "client auth only",
			cert: newTestCert(t, func() *x509.Certificate {
				c := leafTemplate(testServerName)
				c.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
				return c
			}(), &ca, nil),
			wantErr: "the certificate needs the server auth usage with clientAuth Required",
		},
		{
			name:    "key of another certificate",
			cert:    good,
			keyPEM:  newTestCert(t, leafTemplate(testServerName), &ca, nil).keyPEM(t),
			wantErr: "tls.crt and tls.key are not a valid key pair: tls: private key does not match public key",
		},
		{
			name:    "chain certificate that cannot be parsed",
			cert:    good,
			certPEM: append(append([]byte{}, good.pem...), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("junk")})...),
			wantErr: "tls.crt has a chain certificate that cannot be parsed",
		},
		{
			// OpenSSL refuses anyExtendedKeyUsage for TLS, and Go accepts it.
			name:    "any extended key usage",
			cert:    newTestCert(t, anyUsage, &ca, nil),
			wantErr: "the certificate needs the server auth usage with clientAuth Required",
		},
		{
			name:    "key usage without digital signature",
			cert:    newTestCert(t, certSignOnly, &ca, nil),
			wantErr: "the certificate needs the digital signature key usage",
		},
		{name: "no CA", cert: good, ca: []byte("none"), wantErr: "the CA bundle has no PEM certificate"},
		{name: "expired", cert: newTestCert(t, expired, &ca, nil), wantErr: "the certificate expired at 2025-12-31T00:00:00Z"},
		{name: "not yet valid", cert: newTestCert(t, future, &ca, nil), wantErr: "the certificate is not valid before 2026-01-01T01:00:00Z"},
		{
			name:    "small RSA key",
			cert:    newTestCert(t, leafTemplate(testServerName), &ca, smallKey),
			wantErr: "the RSA key has 1024 bits, and Redis needs at least 2048",
		},
		{
			name:    "missing name",
			cert:    newTestCert(t, leafTemplate("other.example.com"), &ca, nil),
			wantErr: `the certificate does not have the name "rfrm-test.testns.svc" of tls.serverName`,
		},
		{name: "other CA", cert: good, ca: otherCA.pem, wantErr: "the certificate does not verify against the CA bundle"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			certPEM, keyPEM, caPEM := test.cert.pem, test.cert.keyPEM(t), ca.pem
			if test.certPEM != nil {
				certPEM = test.certPEM
			}
			if test.keyPEM != nil {
				keyPEM = test.keyPEM
			}
			if test.ca != nil {
				caPEM = test.ca
			}
			clientAuth := test.clientAuth
			if clientAuth == "" {
				clientAuth = redisfailoverv1.TLSClientAuthRequired
			}
			m, err := rfservice.CheckTLSMaterial(certPEM, keyPEM, caPEM, testServerName, clientAuth, testNow)
			if test.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), test.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, test.cert.cert.NotAfter, m.NotAfter)
		})
	}
}

func TestTLSMaterialClientConfig(t *testing.T) {
	ca := newTestCA(t)
	leaf := newTestCert(t, leafTemplate(testServerName), &ca, nil)
	m, err := rfservice.CheckTLSMaterial(leaf.pem, leaf.keyPEM(t), ca.pem, testServerName, redisfailoverv1.TLSClientAuthRequired, testNow)
	require.NoError(t, err)

	cfg := m.ClientConfig(testServerName, "10.0.0.1")
	assert.Equal(t, testServerName, cfg.ServerName)
	assert.Equal(t, uint16(tls.VersionTLS12), cfg.MinVersion)
	assert.Same(t, m.Roots, cfg.RootCAs)
	require.Len(t, cfg.Certificates, 1)
	// The session cache uses the server name as key, which is the same for
	// all pods. Thus each pod IP needs its own cache.
	assert.Same(t, cfg.ClientSessionCache, m.ClientConfig(testServerName, "10.0.0.1").ClientSessionCache)
	assert.NotSame(t, cfg.ClientSessionCache, m.ClientConfig(testServerName, "10.0.0.2").ClientSessionCache)
}

func TestTLSMaterialSessionCacheLimit(t *testing.T) {
	ca := newTestCA(t)
	leaf := newTestCert(t, leafTemplate(testServerName), &ca, nil)
	m, err := rfservice.CheckTLSMaterial(leaf.pem, leaf.keyPEM(t), ca.pem, testServerName, redisfailoverv1.TLSClientAuthRequired, testNow)
	require.NoError(t, err)

	first := m.ClientConfig(testServerName, "ip-0").ClientSessionCache
	for i := 1; i <= 256; i++ {
		m.ClientConfig(testServerName, "ip-"+strconv.Itoa(i))
	}
	assert.NotSame(t, first, m.ClientConfig(testServerName, "ip-0").ClientSessionCache, "old caches are dropped at the limit")
}

// TestTLSMaterialClientConfigUsesTLS12 checks that the operator negotiates
// TLS 1.2 with a server that offers TLS 1.3.
func TestTLSMaterialClientConfigUsesTLS12(t *testing.T) {
	// The handshake checks the certificates at the current time.
	now := time.Now()
	caTmpl := &x509.Certificate{
		Subject:               pkix.Name{CommonName: "test CA"},
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(time.Hour),
	}
	ca := newTestCert(t, caTmpl, nil, nil)
	leafTmpl := leafTemplate(testServerName)
	leafTmpl.NotBefore, leafTmpl.NotAfter = now.Add(-time.Hour), now.Add(time.Hour)
	leaf := newTestCert(t, leafTmpl, &ca, nil)
	m, err := rfservice.CheckTLSMaterial(leaf.pem, leaf.keyPEM(t), ca.pem, testServerName, redisfailoverv1.TLSClientAuthRequired, now)
	require.NoError(t, err)
	pair, err := tls.X509KeyPair(leaf.pem, leaf.keyPEM(t))
	require.NoError(t, err)
	l, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{pair}, MaxVersion: tls.VersionTLS13})
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		conn, err := l.Accept()
		if err == nil {
			_ = conn.(*tls.Conn).Handshake()
			_ = conn.Close()
		}
	}()

	conn, err := tls.Dial("tcp", l.Addr().String(), m.ClientConfig(testServerName, "127.0.0.1"))
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	assert.Equal(t, uint16(tls.VersionTLS12), conn.ConnectionState().Version)
}

func tlsTestRF() *redisfailoverv1.RedisFailover {
	rf := generateRF()
	rf.Spec.Redis.Port = 6379
	rf.Spec.TLS = &redisfailoverv1.TLSSettings{
		SecretName:    "redis-tls",
		Port:          6380,
		PlaintextPort: redisfailoverv1.TLSPlaintextPortEnabled,
		ClientAuth:    redisfailoverv1.TLSClientAuthRequired,
		ServerName:    testServerName,
	}
	return rf
}

func TestLoadTLSMaterial(t *testing.T) {
	ca := newTestCA(t)
	leaf := newTestCert(t, leafTemplate(testServerName), &ca, nil)
	data := map[string][]byte{"tls.crt": leaf.pem, "tls.key": leaf.keyPEM(t), "ca.crt": ca.pem}
	notFound := errors.New("not found")

	tests := []struct {
		name    string
		ca      *redisfailoverv1.TLSCASource
		setup   func(ms *mK8SService.Services)
		wantErr string
	}{
		{
			name: "ca.crt of the Secret",
			setup: func(ms *mK8SService.Services) {
				ms.On("GetSecret", namespace, "redis-tls").Return(&corev1.Secret{Data: data}, nil)
			},
		},
		{
			name: "CA Secret",
			ca:   &redisfailoverv1.TLSCASource{SecretName: "bundle", Key: "root.pem"},
			setup: func(ms *mK8SService.Services) {
				ms.On("GetSecret", namespace, "redis-tls").Return(&corev1.Secret{Data: map[string][]byte{"tls.crt": data["tls.crt"], "tls.key": data["tls.key"]}}, nil)
				ms.On("GetSecret", namespace, "bundle").Return(&corev1.Secret{Data: map[string][]byte{"root.pem": ca.pem}}, nil)
			},
		},
		{
			name: "CA ConfigMap",
			ca:   &redisfailoverv1.TLSCASource{ConfigMapName: "bundle", Key: "ca.crt"},
			setup: func(ms *mK8SService.Services) {
				ms.On("GetSecret", namespace, "redis-tls").Return(&corev1.Secret{Data: data}, nil)
				ms.On("GetConfigMap", namespace, "bundle").Return(&corev1.ConfigMap{Data: map[string]string{"ca.crt": string(ca.pem)}}, nil)
			},
		},
		{
			name: "no Secret",
			setup: func(ms *mK8SService.Services) {
				ms.On("GetSecret", namespace, "redis-tls").Return(nil, notFound)
			},
			wantErr: "tls.secretName: not found",
		},
		{
			name: "no key",
			setup: func(ms *mK8SService.Services) {
				ms.On("GetSecret", namespace, "redis-tls").Return(&corev1.Secret{Data: map[string][]byte{"tls.crt": data["tls.crt"]}}, nil)
			},
			wantErr: `secret "redis-tls" has no tls.key`,
		},
		{
			name: "no ca.crt",
			setup: func(ms *mK8SService.Services) {
				ms.On("GetSecret", namespace, "redis-tls").Return(&corev1.Secret{Data: map[string][]byte{"tls.crt": data["tls.crt"], "tls.key": data["tls.key"]}}, nil)
			},
			wantErr: "the CA bundle of tls is empty or missing",
		},
		{
			name: "no CA Secret",
			ca:   &redisfailoverv1.TLSCASource{SecretName: "bundle", Key: "ca.crt"},
			setup: func(ms *mK8SService.Services) {
				ms.On("GetSecret", namespace, "redis-tls").Return(&corev1.Secret{Data: data}, nil)
				ms.On("GetSecret", namespace, "bundle").Return(nil, notFound)
			},
			wantErr: "tls.ca.secretName: not found",
		},
		{
			name: "no CA ConfigMap",
			ca:   &redisfailoverv1.TLSCASource{ConfigMapName: "bundle", Key: "ca.crt"},
			setup: func(ms *mK8SService.Services) {
				ms.On("GetSecret", namespace, "redis-tls").Return(&corev1.Secret{Data: data}, nil)
				ms.On("GetConfigMap", namespace, "bundle").Return(nil, notFound)
			},
			wantErr: "tls.ca.configMapName: not found",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rf := tlsTestRF()
			rf.Spec.TLS.CA = test.ca
			ms := &mK8SService.Services{}
			test.setup(ms)
			m, err := rfservice.LoadTLSMaterial(ms, rf, testNow)
			if test.wantErr != "" {
				assert.EqualError(t, err, test.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, leaf.cert.NotAfter, m.NotAfter)
		})
	}
}

// TestTLSDefaultServerName checks that the default serverName is the DNS name
// of the master Service, so that one SAN serves the operator and the
// clients of that Service.
func TestTLSDefaultServerName(t *testing.T) {
	rf := generateRF()
	rf.Spec.TLS = &redisfailoverv1.TLSSettings{SecretName: "redis-tls"}
	_ = rf.Validate()
	assert.Equal(t, rfservice.GetRedisMasterName(rf)+"."+namespace+".svc", rf.Spec.TLS.ServerName)
}

func generateTLSStatefulSet(t *testing.T, rf *redisfailoverv1.RedisFailover) *appsv1.StatefulSet {
	t.Helper()
	var ss *appsv1.StatefulSet
	ms := &mK8SService.Services{}
	ms.On("GetSecret", namespace, "auth").Return(&corev1.Secret{Data: map[string][]byte{"password": []byte("pw")}}, nil)
	ms.On("CreateOrUpdatePodDisruptionBudget", namespace, mock.Anything).Return(nil)
	ms.On("CreateOrUpdateStatefulSet", namespace, mock.Anything).Run(func(args mock.Arguments) {
		ss = args.Get(1).(*appsv1.StatefulSet)
	}).Return(nil)
	client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
	require.NoError(t, client.EnsureRedisStatefulset(rf, nil, nil))
	return ss
}

func envValue(env []corev1.EnvVar, name string) (string, bool) {
	value, found := "", false
	for _, e := range env {
		if e.Name == name {
			value, found = e.Value, true
		}
	}
	return value, found
}

func TestRedisStatefulSetTLS(t *testing.T) {
	mode := int32(0440)
	tests := []struct {
		name        string
		modify      func(rf *redisfailoverv1.RedisFailover)
		wantVolumes []corev1.Volume
		wantMounts  []corev1.VolumeMount
		wantCA      string
		wantCommand []string
	}{
		{
			name: "ca.crt of the Secret",
			wantVolumes: []corev1.Volume{
				{Name: "redis-tls", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "redis-tls", DefaultMode: &mode}}},
			},
			wantMounts: []corev1.VolumeMount{{Name: "redis-tls", MountPath: "/tls", ReadOnly: true}},
			wantCA:     "/tls/ca.crt",
			wantCommand: []string{"redis-server", "/redis/redis.conf",
				"--tls-port", "6380", "--tls-cert-file", "/tls/tls.crt", "--tls-key-file", "/tls/tls.key", "--tls-ca-cert-file", "/tls/ca.crt"},
		},
		{
			name: "CA Secret and password",
			modify: func(rf *redisfailoverv1.RedisFailover) {
				rf.Spec.Auth.SecretPath = "auth"
				rf.Spec.TLS.CA = &redisfailoverv1.TLSCASource{SecretName: "bundle", Key: "root.pem"}
			},
			wantVolumes: []corev1.Volume{
				{Name: "redis-tls", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "redis-tls", DefaultMode: &mode}}},
				{Name: "redis-tls-ca", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "bundle", DefaultMode: &mode}}},
			},
			wantMounts: []corev1.VolumeMount{
				{Name: "redis-tls", MountPath: "/tls", ReadOnly: true},
				{Name: "redis-tls-ca", MountPath: "/tls-ca", ReadOnly: true},
			},
			wantCA: "/tls-ca/root.pem",
			wantCommand: []string{"sh", "-c", `exec redis-server /redis/redis.conf --requirepass "$REDIS_PASSWORD" --masterauth "$REDIS_PASSWORD"` +
				" --tls-port 6380 --tls-cert-file /tls/tls.crt --tls-key-file /tls/tls.key --tls-ca-cert-file /tls-ca/root.pem"},
		},
		{
			name: "CA ConfigMap",
			modify: func(rf *redisfailoverv1.RedisFailover) {
				rf.Spec.TLS.CA = &redisfailoverv1.TLSCASource{ConfigMapName: "bundle", Key: "ca.crt"}
			},
			wantVolumes: []corev1.Volume{
				{Name: "redis-tls", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "redis-tls", DefaultMode: &mode}}},
				{Name: "redis-tls-ca", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "bundle"}}}},
			},
			wantMounts: []corev1.VolumeMount{
				{Name: "redis-tls", MountPath: "/tls", ReadOnly: true},
				{Name: "redis-tls-ca", MountPath: "/tls-ca", ReadOnly: true},
			},
			wantCA: "/tls-ca/ca.crt",
			wantCommand: []string{"redis-server", "/redis/redis.conf",
				"--tls-port", "6380", "--tls-cert-file", "/tls/tls.crt", "--tls-key-file", "/tls/tls.key", "--tls-ca-cert-file", "/tls-ca/ca.crt"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rf := tlsTestRF()
			if test.modify != nil {
				test.modify(rf)
			}
			ss := generateTLSStatefulSet(t, rf)
			spec := ss.Spec.Template.Spec
			redis := spec.Containers[0]

			assert.Equal(t, test.wantCommand, redis.Command)
			assert.Equal(t, []corev1.ContainerPort{
				{Name: "redis", ContainerPort: 6379, Protocol: corev1.ProtocolTCP},
				{Name: "redis-tls", ContainerPort: 6380, Protocol: corev1.ProtocolTCP},
			}, redis.Ports)
			assert.Equal(t, test.wantVolumes, spec.Volumes[len(spec.Volumes)-len(test.wantVolumes):])
			assert.Equal(t, test.wantMounts, redis.VolumeMounts[len(redis.VolumeMounts)-len(test.wantMounts):])
			for name, want := range map[string]string{
				"RFO_TLS_PORT":        "6380",
				"RFO_TLS_CERT_FILE":   "/tls/tls.crt",
				"RFO_TLS_KEY_FILE":    "/tls/tls.key",
				"RFO_TLS_CA_FILE":     test.wantCA,
				"RFO_TLS_SERVER_NAME": testServerName,
				"REDIS_ADDR":          "redis://127.0.0.1:6379",
			} {
				got, _ := envValue(redis.Env, name)
				assert.Equal(t, want, got, name)
			}
			assert.Equal(t, "t=; command -v timeout >/dev/null 2>&1 && t=\"timeout 2\"; $t redis-cli -h $(hostname) --tls --cacert "+test.wantCA+
				" --cert /tls/tls.crt --key /tls/tls.key -p 6380 --user pinger --pass pingpass --no-auth-warning ping | grep -qE '^(PONG|LOADING)'",
				redis.LivenessProbe.Exec.Command[2])
		})
	}
}

func TestRedisExporterTLS(t *testing.T) {
	for _, clientAuth := range []string{redisfailoverv1.TLSClientAuthRequired, redisfailoverv1.TLSClientAuthOptional, redisfailoverv1.TLSClientAuthNone} {
		t.Run(clientAuth, func(t *testing.T) {
			rf := tlsTestRF()
			rf.Spec.TLS.ClientAuth = clientAuth
			rf.Spec.Redis.Exporter.Enabled = true
			rf.Spec.Redis.Exporter.Image = "exporter"
			exporter := generateTLSStatefulSet(t, rf).Spec.Template.Spec.Containers[1]

			want := map[string]string{
				"REDIS_ADDR":                           "rediss://127.0.0.1:6380",
				"REDIS_PORT":                           "6380",
				"REDIS_EXPORTER_SKIP_TLS_VERIFICATION": "true",
				"REDIS_EXPORTER_TLS_CA_CERT_FILE":      "/tls/ca.crt",
			}
			if clientAuth != redisfailoverv1.TLSClientAuthNone {
				want["REDIS_EXPORTER_TLS_CLIENT_CERT_FILE"] = "/tls/tls.crt"
				want["REDIS_EXPORTER_TLS_CLIENT_KEY_FILE"] = "/tls/tls.key"
			}
			for _, name := range []string{"REDIS_ADDR", "REDIS_PORT", "REDIS_EXPORTER_SKIP_TLS_VERIFICATION", "REDIS_EXPORTER_TLS_CA_CERT_FILE",
				"REDIS_EXPORTER_TLS_CLIENT_CERT_FILE", "REDIS_EXPORTER_TLS_CLIENT_KEY_FILE"} {
				got, found := envValue(exporter.Env, name)
				wantValue, wantFound := want[name]
				assert.Equal(t, wantFound, found, name)
				assert.Equal(t, wantValue, got, name)
			}
			assert.Equal(t, []corev1.VolumeMount{{Name: "redis-tls", MountPath: "/tls", ReadOnly: true}}, exporter.VolumeMounts)
		})
	}
}

func TestRedisConfigAndServicesTLS(t *testing.T) {
	plain := corev1.ServicePort{Name: "redis", Port: 6379, TargetPort: intstr.FromString("redis"), Protocol: corev1.ProtocolTCP}
	tlsPort := corev1.ServicePort{Name: "redis-tls", Port: 6380, TargetPort: intstr.FromString("redis-tls"), Protocol: corev1.ProtocolTCP}
	tests := []struct {
		name      string
		spec      bool
		status    *redisfailoverv1.TLSStatus
		auth      string
		wantPort  string
		wantTLS   string
		wantPorts []corev1.ServicePort
	}{
		{
			name:      "spec without status",
			spec:      true,
			wantPort:  "port 6379\n",
			wantTLS:   "tls-replication no\ntls-auth-clients yes\n",
			wantPorts: []corev1.ServicePort{plain, tlsPort},
		},
		{
			name:      "links on TLS",
			spec:      true,
			auth:      redisfailoverv1.TLSClientAuthOptional,
			status:    &redisfailoverv1.TLSStatus{InternalLinks: "TLS", PlaintextPort: "Open"},
			wantPort:  "port 6379\n",
			wantTLS:   "tls-replication yes\ntls-auth-clients optional\n",
			wantPorts: []corev1.ServicePort{plain, tlsPort},
		},
		{
			name:      "plaintext closed",
			spec:      true,
			auth:      redisfailoverv1.TLSClientAuthNone,
			status:    &redisfailoverv1.TLSStatus{InternalLinks: "TLS", PlaintextPort: "Closed"},
			wantPort:  "port 0\n",
			wantTLS:   "tls-replication yes\ntls-auth-clients no\n",
			wantPorts: []corev1.ServicePort{tlsPort},
		},
		{
			// Without spec.tls, the pods go back to plaintext.
			name:      "TLS removed",
			status:    &redisfailoverv1.TLSStatus{InternalLinks: "TLS", PlaintextPort: "Closed"},
			wantPort:  "port 6379\n",
			wantPorts: []corev1.ServicePort{plain},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rf := tlsTestRF()
			if !test.spec {
				rf.Spec.TLS = nil
			} else if test.auth != "" {
				rf.Spec.TLS.ClientAuth = test.auth
			}
			rf.Status.TLS = test.status

			var cm *corev1.ConfigMap
			var services []*corev1.Service
			ms := &mK8SService.Services{}
			ms.On("CreateOrUpdateConfigMap", namespace, mock.Anything).Run(func(args mock.Arguments) {
				cm = args.Get(1).(*corev1.ConfigMap)
			}).Return(nil)
			ms.On("CreateOrUpdateService", namespace, mock.Anything).Run(func(args mock.Arguments) {
				services = append(services, args.Get(1).(*corev1.Service))
			}).Return(nil)
			client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
			require.NoError(t, client.EnsureRedisConfigMap(rf, nil, nil))
			require.NoError(t, client.EnsureRedisMasterService(rf, nil, nil))
			require.NoError(t, client.EnsureRedisSlaveService(rf, nil, nil))

			conf := cm.Data["redis.conf"]
			assert.True(t, strings.HasPrefix(conf, "slaveof 127.0.0.1 6379\n"+test.wantPort), conf)
			if test.wantTLS == "" {
				assert.NotContains(t, conf, "tls-")
			} else {
				assert.True(t, strings.HasSuffix(conf, "\n"+test.wantTLS), conf)
			}
			require.Len(t, services, 2)
			for _, svc := range services {
				assert.Equal(t, test.wantPorts, svc.Spec.Ports, svc.Name)
			}
		})
	}
}

// TestRedisScriptsTLS runs ready.sh and shutdown.sh against a fake redis-cli
// that logs its arguments. A pod with RFO_TLS_PORT uses the TLS port, and an
// older pod without it uses the plaintext port of the same script.
func TestRedisScriptsTLS(t *testing.T) {
	rf := tlsTestRF()
	scripts := map[string]string{}
	ms := &mK8SService.Services{}
	ms.On("CreateOrUpdateConfigMap", namespace, mock.Anything).Run(func(args mock.Arguments) {
		for k, v := range args.Get(1).(*corev1.ConfigMap).Data {
			scripts[k] = v
		}
	}).Return(nil)
	client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
	require.NoError(t, client.EnsureRedisReadinessConfigMap(rf, nil, nil))
	require.NoError(t, client.EnsureRedisShutdownConfigMap(rf, nil, nil))

	dir := t.TempDir()
	fakeCLI := "#!/bin/sh\necho \"$*\" >>\"$FAKE_LOG\"\nprintf 'role:master\\r\\n'\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "redis-cli"), []byte(fakeCLI), 0o755))
	tlsEnv := []string{"RFO_TLS_PORT=6380", "RFO_TLS_CA_FILE=/tls-ca/ca.crt", "RFO_TLS_CERT_FILE=/tls/tls.crt", "RFO_TLS_KEY_FILE=/tls/tls.key"}
	// The env of a pod without TLS can have names of the image, for example
	// of the Bitnami image. The scripts must ignore them.
	bitnamiEnv := []string{"REDIS_TLS_PORT=6380", "REDIS_TLS_CA_FILE=/ca", "REDIS_TLS_CERT_FILE=/crt", "REDIS_TLS_KEY_FILE=/key"}
	tlsCLI := "--tls --cacert /tls-ca/ca.crt --cert /tls/tls.crt --key /tls/tls.key -p 6380 "

	tests := []struct {
		script string
		env    []string
		want   string
	}{
		{"ready.sh", tlsEnv, tlsCLI + "info replication"},
		{"ready.sh", nil, "-p 6379 info replication"},
		{"ready.sh", bitnamiEnv, "-p 6379 info replication"},
		{"shutdown.sh", bitnamiEnv, "-p 6379 save"},
		{"shutdown.sh", tlsEnv, tlsCLI + "save"},
		{"shutdown.sh", nil, "-p 6379 save"},
	}
	for _, test := range tests {
		t.Run(test.script+" "+strings.Join(test.env, ","), func(t *testing.T) {
			file := filepath.Join(t.TempDir(), test.script)
			require.NoError(t, os.WriteFile(file, []byte(scripts[test.script]), 0o644))
			logFile := filepath.Join(t.TempDir(), "calls")
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "sh", file)
			cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "FAKE_LOG="+logFile, "REDIS_PASSWORD=")
			cmd.Env = append(cmd.Env, test.env...)
			out, err := cmd.CombinedOutput()
			require.NoError(t, err, string(out))
			calls, err := os.ReadFile(logFile)
			require.NoError(t, err)
			for _, call := range strings.Split(strings.TrimSpace(string(calls)), "\n") {
				assert.Equal(t, test.want, call)
			}
		})
	}
}

// TestRedisTLSEnvComesLast checks that the RFO_TLS_ values of the operator
// come after the env of the user. Kubernetes uses the last value of a name.
func TestRedisTLSEnvComesLast(t *testing.T) {
	rf := tlsTestRF()
	rf.Spec.Redis.Env = []corev1.EnvVar{{Name: "RFO_TLS_PORT", Value: "7000"}}
	env := generateTLSStatefulSet(t, rf).Spec.Template.Spec.Containers[0].Env
	got, _ := envValue(env, "RFO_TLS_PORT")
	assert.Equal(t, "6380", got)
}
