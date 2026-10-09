package redis

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	rediscli "github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/saremox/redis-operator/metrics"
)

const testTLSServerName = "rfrm-test.ns.svc"

// testPKI is a CA and one certificate with both TLS usages, as the operator
// and the Redis pods share it.
type testPKI struct {
	dir  string
	pool *x509.CertPool
	pair tls.Certificate
}

func writePEM(t *testing.T, file, typ string, der []byte) {
	t.Helper()
	require.NoError(t, os.WriteFile(file, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o600))
}

func newTestPKI(t *testing.T) *testPKI {
	t.Helper()
	dir := t.TempDir()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, caKey.Public(), caKey)
	require.NoError(t, err)
	ca, err := x509.ParseCertificate(caDER)
	require.NoError(t, err)

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "redis"},
		DNSNames:     []string{testTLSServerName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}, ca, key.Public(), caKey)
	require.NoError(t, err)
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)

	writePEM(t, filepath.Join(dir, "ca.crt"), "CERTIFICATE", caDER)
	writePEM(t, filepath.Join(dir, "tls.crt"), "CERTIFICATE", der)
	writePEM(t, filepath.Join(dir, "tls.key"), "PRIVATE KEY", keyDER)
	pair, err := tls.LoadX509KeyPair(filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"))
	require.NoError(t, err)
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return &testPKI{dir: dir, pool: pool, pair: pair}
}

func (p *testPKI) config(serverName string) *tls.Config {
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		RootCAs:      p.pool,
		Certificates: []tls.Certificate{p.pair},
		ServerName:   serverName,
	}
}

// startTLSRedis starts redis-server with the plaintext port of the harness
// and a TLS port. It skips the test when redis-server has no TLS support.
func startTLSRedis(t *testing.T, pki *testPKI, extraArgs ...string) (*redisProc, string) {
	t.Helper()
	requireRedisServer(t)
	port, err := findFreePort()
	require.NoError(t, err)
	args := append([]string{
		"--tls-port", strconv.Itoa(port),
		"--tls-cert-file", filepath.Join(pki.dir, "tls.crt"),
		"--tls-key-file", filepath.Join(pki.dir, "tls.key"),
		"--tls-ca-cert-file", filepath.Join(pki.dir, "ca.crt"),
	}, extraArgs...)
	proc, err := newRedisProcess(args...)
	if err != nil {
		t.Skipf("redis-server does not start with TLS: %v", err)
	}
	t.Cleanup(func() {
		killProc(proc)
		_ = os.RemoveAll(proc.dir)
	})
	return proc, strconv.Itoa(port)
}

func tlsClient(pki *testPKI, ip, port, serverName string) Client {
	return New(metrics.Dummy).(*client).ForTLS(&TLSTargets{
		Port:    port,
		Configs: map[string]*tls.Config{ip: pki.config(serverName)},
	})
}

// TestForTLS dials a pod of the targets on the TLS port with the pinned
// server name, also when the plaintext port is closed.
func TestForTLS(t *testing.T) {
	pki := newTestPKI(t)
	proc, tlsPort := startTLSRedis(t, pki)
	plainPort := strconv.Itoa(proc.Port)
	c := tlsClient(pki, proc.IP, tlsPort, testTLSServerName)

	require.NoError(t, c.SetConfig(proc.IP, plainPort, "", "port", "0"))
	port, err := c.GetConfig(proc.IP, plainPort, "", "port")
	require.NoError(t, err)
	assert.Equal(t, "0", port)
	version, err := c.GetServerVersion(proc.IP, plainPort, "")
	require.NoError(t, err)
	assert.Regexp(t, `^\d+\.\d+\.\d+`, version)

	// The client of other pods and the base client stay on plaintext.
	other := New(metrics.Dummy).(*client).ForTLS(&TLSTargets{Port: tlsPort, Configs: map[string]*tls.Config{"10.0.0.1": pki.config(testTLSServerName)}})
	for _, plain := range []Client{other, New(metrics.Dummy)} {
		_, err = plain.GetConfig(proc.IP, plainPort, "", "port")
		require.Error(t, err)
		assert.True(t, IsUnreachableError(err), err)
		assert.False(t, IsTLSError(err), err)
	}
}

// TestTLSErrorsAreNotUnreachable checks that a node that answers with a TLS
// failure is not skipped as unreachable.
func TestTLSErrorsAreNotUnreachable(t *testing.T) {
	pki := newTestPKI(t)
	proc, tlsPort := startTLSRedis(t, pki)
	plainPort := strconv.Itoa(proc.Port)
	otherPKI := newTestPKI(t)

	tests := map[string]Client{
		"unknown CA":        New(metrics.Dummy).(*client).ForTLS(&TLSTargets{Port: tlsPort, Configs: map[string]*tls.Config{proc.IP: otherPKI.config(testTLSServerName)}}),
		"other server name": tlsClient(pki, proc.IP, tlsPort, "other.example.com"),
		"no client certificate, TLS 1.2": New(metrics.Dummy).(*client).ForTLS(&TLSTargets{Port: tlsPort, Configs: map[string]*tls.Config{proc.IP: {
			MinVersion: tls.VersionTLS12, MaxVersion: tls.VersionTLS12, RootCAs: pki.pool, ServerName: testTLSServerName,
		}}}),
	}
	for name, c := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := c.GetServerVersion(proc.IP, plainPort, "")
			require.Error(t, err)
			assert.True(t, IsTLSError(err), err)
			assert.False(t, IsUnreachableError(err), err)
			assert.Equal(t, metrics.TLS_ERROR, getRedisError(err))
		})
	}
}

func TestIsTLSError(t *testing.T) {
	assert.False(t, IsTLSError(nil))
	assert.False(t, IsTLSError(errors.New("dial tcp 10.0.0.1:6379: connect: connection refused")))
	assert.True(t, IsTLSError(&tls.CertificateVerificationError{Err: errors.New("bad")}))
	assert.True(t, IsTLSError(tls.RecordHeaderError{Msg: "bad"}))
	assert.True(t, IsTLSError(&net.OpError{Op: "remote error", Err: errors.New("tls: bad certificate")}))
	assert.True(t, IsTLSError(errors.New("x509: certificate signed by unknown authority")))
}

func TestGetConfigAndSetConfig(t *testing.T) {
	requireRedisServer(t)
	proc := startRedisProcess(t)
	port := strconv.Itoa(proc.Port)
	c := newTestClient()

	require.NoError(t, c.SetConfig(proc.IP, port, "", "hz", "20"))
	value, err := c.GetConfig(proc.IP, port, "", "hz")
	require.NoError(t, err)
	assert.Equal(t, "20", value)

	_, err = c.GetConfig(proc.IP, port, "", "no-such-parameter")
	assert.EqualError(t, err, "CONFIG GET no-such-parameter: unknown parameter")
	assert.Error(t, c.SetConfig(proc.IP, port, "", "port", "not-a-port"))

	free, err := findFreePort()
	require.NoError(t, err)
	_, err = c.GetConfig(testLoopbackIP, strconv.Itoa(free), "", "port")
	assert.True(t, IsUnreachableError(err))
}

// TestSetConfigTLSParameter changes a TLS parameter, which a server without
// TLS does not know.
func TestSetConfigTLSParameter(t *testing.T) {
	pki := newTestPKI(t)
	proc, tlsPort := startTLSRedis(t, pki)
	c := tlsClient(pki, proc.IP, tlsPort, testTLSServerName)
	port := strconv.Itoa(proc.Port)

	require.NoError(t, c.SetConfig(proc.IP, port, "", "tls-auth-clients", "optional"))
	value, err := c.GetConfig(proc.IP, port, "", "tls-auth-clients")
	require.NoError(t, err)
	assert.Equal(t, "optional", value)
}

// TestKillClientsOnPort closes the normal and pub/sub clients of the plaintext
// port, and keeps the clients of the TLS port.
func TestKillClientsOnPort(t *testing.T) {
	pki := newTestPKI(t)
	proc, tlsPort := startTLSRedis(t, pki)
	plainPort := strconv.Itoa(proc.Port)

	plain := rediscli.NewClient(&rediscli.Options{Addr: proc.Addr()})
	t.Cleanup(func() { _ = plain.Close() })
	require.NoError(t, plain.Ping(bgCtx()).Err())
	sub := rediscli.NewClient(&rediscli.Options{Addr: proc.Addr()}).Subscribe(bgCtx(), "channel")
	t.Cleanup(func() { _ = sub.Close() })
	_, err := sub.Receive(bgCtx())
	require.NoError(t, err)
	secure := rediscli.NewClient(&rediscli.Options{Addr: net.JoinHostPort(proc.IP, tlsPort), TLSConfig: pki.config(testTLSServerName)})
	t.Cleanup(func() { _ = secure.Close() })
	require.NoError(t, secure.Ping(bgCtx()).Err())

	c := tlsClient(pki, proc.IP, tlsPort, testTLSServerName)
	killed, err := c.KillClientsOnPort(proc.IP, plainPort, "", plainPort)
	require.NoError(t, err)
	assert.Equal(t, 2, killed)

	// A client on the plaintext port does not close its own connection.
	require.NoError(t, plain.Ping(bgCtx()).Err())
	killed, err = newTestClient().KillClientsOnPort(proc.IP, plainPort, "", plainPort)
	require.NoError(t, err)
	assert.Equal(t, 1, killed)

	list, err := secure.ClientList(bgCtx()).Result()
	require.NoError(t, err)
	assert.Empty(t, clientIDsOnPort(list, plainPort))
	assert.Len(t, clientIDsOnPort(list, tlsPort), 1, "the TLS client stays connected")
}

func TestKillClientsOnPortErrors(t *testing.T) {
	requireRedisServer(t)
	for _, denied := range []string{"-client|kill", "-client|list"} {
		t.Run(denied, func(t *testing.T) {
			proc := startRedisProcess(t, "--user", "default", "on", "nopass", "~*", "&*", "+@all", denied)
			port := strconv.Itoa(proc.Port)
			idle := rediscli.NewClient(&rediscli.Options{Addr: proc.Addr()})
			t.Cleanup(func() { _ = idle.Close() })
			require.NoError(t, idle.Ping(bgCtx()).Err())

			_, err := newTestClient().KillClientsOnPort(proc.IP, port, "", port)
			assert.ErrorContains(t, err, "NOPERM")
		})
	}

	free, err := findFreePort()
	require.NoError(t, err)
	_, err = newTestClient().KillClientsOnPort(testLoopbackIP, strconv.Itoa(free), "", "6379")
	assert.True(t, IsUnreachableError(err))
}

func TestClientIDsOnPort(t *testing.T) {
	list := "id=3 addr=10.0.0.9:5000 laddr=10.0.0.1:6379 fd=8 name=\n" +
		"id=4 addr=10.0.0.9:5001 laddr=10.0.0.1:6380 fd=9 name=\n" +
		"id=5 addr=127.0.0.1:5002 laddr=127.0.0.1:6379 fd=10\n" +
		"id=6 addr=10.0.0.9:5003 laddr=10.0.0.1:16379 fd=11\n" +
		"id=7 addr=10.0.0.9:5004 fd=12\n"
	assert.Equal(t, []string{"3", "5"}, clientIDsOnPort(list, "6379"))
}

func TestGetServerVersionErrors(t *testing.T) {
	l, err := net.Listen("tcp", net.JoinHostPort(testLoopbackIP, "0"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				buf := make([]byte, 4096)
				for {
					if _, err := conn.Read(buf); err != nil {
						return
					}
					if _, err := conn.Write([]byte("$8\r\n# Server\r\n")); err != nil {
						return
					}
				}
			}()
		}
	}()
	port := strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
	_, err = newTestClient().GetServerVersion(testLoopbackIP, port, "")
	assert.EqualError(t, err, "INFO server has no redis_version")

	free, err := findFreePort()
	require.NoError(t, err)
	_, err = newTestClient().GetServerVersion(testLoopbackIP, strconv.Itoa(free), "")
	assert.True(t, IsUnreachableError(err))
}

// TestKillClientsOnPortWithoutLaddr fails when CLIENT LIST has no laddr, as
// before Redis 6.2, because then no client matches and all of them stay.
func TestKillClientsOnPortWithoutLaddr(t *testing.T) {
	l, err := net.Listen("tcp", net.JoinHostPort(testLoopbackIP, "0"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				buf := make([]byte, 4096)
				for {
					n, err := conn.Read(buf)
					if err != nil {
						return
					}
					reply := ":5\r\n"
					if strings.Contains(string(buf[:n]), "LIST") {
						line := "id=5 addr=127.0.0.1:5000 fd=8 name=\n"
						reply = "$" + strconv.Itoa(len(line)) + "\r\n" + line + "\r\n"
					}
					if _, err := conn.Write([]byte(reply)); err != nil {
						return
					}
				}
			}()
		}
	}()
	port := strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
	_, err = newTestClient().KillClientsOnPort(testLoopbackIP, port, "", "6379")
	assert.EqualError(t, err, "CLIENT LIST has no laddr field")
}
