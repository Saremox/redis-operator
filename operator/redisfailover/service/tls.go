package service

import (
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/service/k8s"
)

const (
	redisTLSVolumeName   = "redis-tls"
	redisTLSCAVolumeName = "redis-tls-ca"
	redisTLSPortName     = "redis-tls"
	redisTLSDir          = "/tls"
	redisTLSCADir        = "/tls-ca"
	tlsCertKey           = "tls.crt"
	tlsKeyKey            = "tls.key"
)

// minRSAKeyBits is the smallest RSA key that OpenSSL accepts at its default
// security level. Go accepts smaller keys, so the check of Go alone is not
// enough.
const minRSAKeyBits = 2048

// TLSMaterial is a certificate that CheckTLSMaterial accepted.
type TLSMaterial struct {
	Certificate tls.Certificate
	Roots       *x509.CertPool
	NotAfter    time.Time
	// sessions lets the operator resume TLS sessions, because each command
	// of the operator opens a new connection.
	sessions tls.ClientSessionCache
}

// ClientConfig is the TLS config of the operator for a Redis pod. It checks
// serverName and not the dialled pod IP, because pod IPs change.
func (m *TLSMaterial) ClientConfig(serverName string) *tls.Config {
	return &tls.Config{
		MinVersion:         tls.VersionTLS12,
		RootCAs:            m.Roots,
		Certificates:       []tls.Certificate{m.Certificate},
		ServerName:         serverName,
		ClientSessionCache: m.sessions,
	}
}

// CheckTLSMaterial accepts a certificate only when Redis, its replicas and
// the operator can all use it. The Redis pods and the operator present the
// same certificate as client, so clientAuth Required and Optional need the
// client auth usage.
func CheckTLSMaterial(certPEM, keyPEM, caPEM []byte, serverName, clientAuth string, now time.Time) (*TLSMaterial, error) {
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("tls.crt and tls.key are not a valid key pair: %w", err)
	}
	leaf := pair.Leaf
	intermediates := x509.NewCertPool()
	for _, der := range pair.Certificate[1:] {
		c, err := x509.ParseCertificate(der)
		if err != nil {
			return nil, fmt.Errorf("tls.crt has a chain certificate that cannot be parsed: %w", err)
		}
		intermediates.AddCert(c)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("the CA bundle has no PEM certificate")
	}

	if now.Before(leaf.NotBefore) {
		return nil, fmt.Errorf("the certificate is not valid before %s", leaf.NotBefore.UTC().Format(time.RFC3339))
	}
	if now.After(leaf.NotAfter) {
		return nil, fmt.Errorf("the certificate expired at %s", leaf.NotAfter.UTC().Format(time.RFC3339))
	}
	if key, ok := leaf.PublicKey.(*rsa.PublicKey); ok && key.N.BitLen() < minRSAKeyBits {
		return nil, fmt.Errorf("the RSA key has %d bits, and Redis needs at least %d", key.N.BitLen(), minRSAKeyBits)
	}
	usages := []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	if clientAuth != redisfailoverv1.TLSClientAuthNone {
		usages = append(usages, x509.ExtKeyUsageClientAuth)
	}
	for _, usage := range usages {
		if !hasExtKeyUsage(leaf, usage) {
			return nil, fmt.Errorf("the certificate needs the %s usage with clientAuth %s", extKeyUsageName(usage), clientAuth)
		}
	}
	if err := leaf.VerifyHostname(serverName); err != nil {
		return nil, fmt.Errorf("the certificate does not have the name %q of tls.serverName: %w", serverName, err)
	}
	for _, usage := range usages {
		if _, err := leaf.Verify(x509.VerifyOptions{
			Roots:         roots,
			Intermediates: intermediates,
			CurrentTime:   now,
			KeyUsages:     []x509.ExtKeyUsage{usage},
		}); err != nil {
			return nil, fmt.Errorf("the certificate does not verify against the CA bundle: %w", err)
		}
	}

	return &TLSMaterial{
		Certificate: pair,
		Roots:       roots,
		NotAfter:    leaf.NotAfter,
		sessions:    tls.NewLRUClientSessionCache(0),
	}, nil
}

// hasExtKeyUsage treats a certificate without extended key usages as valid
// for each usage, as Go and OpenSSL do.
func hasExtKeyUsage(c *x509.Certificate, usage x509.ExtKeyUsage) bool {
	if len(c.ExtKeyUsage) == 0 && len(c.UnknownExtKeyUsage) == 0 {
		return true
	}
	return slices.Contains(c.ExtKeyUsage, usage) || slices.Contains(c.ExtKeyUsage, x509.ExtKeyUsageAny)
}

func extKeyUsageName(usage x509.ExtKeyUsage) string {
	if usage == x509.ExtKeyUsageClientAuth {
		return "client auth"
	}
	return "server auth"
}

// LoadTLSMaterial reads the certificate and the CA bundle of rf.Spec.TLS and
// checks them with CheckTLSMaterial.
func LoadTLSMaterial(s k8s.Services, rf *redisfailoverv1.RedisFailover, now time.Time) (*TLSMaterial, error) {
	t := rf.Spec.TLS
	secret, err := s.GetSecret(rf.Namespace, t.SecretName)
	if err != nil {
		return nil, fmt.Errorf("tls.secretName: %w", err)
	}
	for _, key := range []string{tlsCertKey, tlsKeyKey} {
		if len(secret.Data[key]) == 0 {
			return nil, fmt.Errorf("secret %q has no %s", t.SecretName, key)
		}
	}
	ca, err := loadTLSCA(s, rf, secret)
	if err != nil {
		return nil, err
	}
	return CheckTLSMaterial(secret.Data[tlsCertKey], secret.Data[tlsKeyKey], ca, t.ServerName, t.ClientAuth, now)
}

func loadTLSCA(s k8s.Services, rf *redisfailoverv1.RedisFailover, secret *corev1.Secret) ([]byte, error) {
	src := rf.Spec.TLS.CA
	var ca []byte
	switch {
	case src == nil:
		ca = secret.Data[redisfailoverv1.DefaultTLSCAKey]
	case src.SecretName != "":
		caSecret, err := s.GetSecret(rf.Namespace, src.SecretName)
		if err != nil {
			return nil, fmt.Errorf("tls.ca.secretName: %w", err)
		}
		ca = caSecret.Data[src.Key]
	default:
		cm, err := s.GetConfigMap(rf.Namespace, src.ConfigMapName)
		if err != nil {
			return nil, fmt.Errorf("tls.ca.configMapName: %w", err)
		}
		ca = []byte(cm.Data[src.Key])
	}
	if len(ca) == 0 {
		return nil, errors.New("the CA bundle of tls is empty or missing")
	}
	return ca, nil
}

// redisTLSOn tells if the Redis pod template has TLS.
func redisTLSOn(rf *redisfailoverv1.RedisFailover) bool {
	return rf.Spec.TLS != nil
}

func redisTLSCAFile(rf *redisfailoverv1.RedisFailover) string {
	if rf.Spec.TLS.CA != nil {
		return redisTLSCADir + "/" + rf.Spec.TLS.CA.Key
	}
	return redisTLSDir + "/" + redisfailoverv1.DefaultTLSCAKey
}

// redisTLSArgs are the redis-server arguments that need the TLS volume.
// They are in the pod template and not in redis.conf, so that a pod without
// the volume never gets them.
func redisTLSArgs(rf *redisfailoverv1.RedisFailover) []string {
	return []string{
		"--tls-port", strconv.Itoa(int(rf.Spec.TLS.Port)),
		"--tls-cert-file", redisTLSDir + "/" + tlsCertKey,
		"--tls-key-file", redisTLSDir + "/" + tlsKeyKey,
		"--tls-ca-cert-file", redisTLSCAFile(rf),
	}
}

// redisTLSEnv tells ready.sh and shutdown.sh that the pod has TLS. Old and
// new pods share these ConfigMaps in a rollout, so each pod uses its own
// setting.
func redisTLSEnv(rf *redisfailoverv1.RedisFailover) []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: "REDIS_TLS_PORT", Value: strconv.Itoa(int(rf.Spec.TLS.Port))},
		{Name: "REDIS_TLS_CERT_FILE", Value: redisTLSDir + "/" + tlsCertKey},
		{Name: "REDIS_TLS_KEY_FILE", Value: redisTLSDir + "/" + tlsKeyKey},
		{Name: "REDIS_TLS_CA_FILE", Value: redisTLSCAFile(rf)},
		{Name: "REDIS_TLS_SERVER_NAME", Value: rf.Spec.TLS.ServerName},
	}
}

func redisTLSVolumeMounts(rf *redisfailoverv1.RedisFailover) []corev1.VolumeMount {
	mounts := []corev1.VolumeMount{{Name: redisTLSVolumeName, MountPath: redisTLSDir, ReadOnly: true}}
	if rf.Spec.TLS.CA != nil {
		mounts = append(mounts, corev1.VolumeMount{Name: redisTLSCAVolumeName, MountPath: redisTLSCADir, ReadOnly: true})
	}
	return mounts
}

// redisTLSVolumes mount the whole Secret, because the kubelet does not update
// a subPath mount after a renewal of the certificate.
func redisTLSVolumes(rf *redisfailoverv1.RedisFailover) []corev1.Volume {
	mode := int32(0440)
	volumes := []corev1.Volume{{
		Name: redisTLSVolumeName,
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{SecretName: rf.Spec.TLS.SecretName, DefaultMode: &mode},
		},
	}}
	ca := rf.Spec.TLS.CA
	switch {
	case ca == nil:
	case ca.SecretName != "":
		volumes = append(volumes, corev1.Volume{
			Name: redisTLSCAVolumeName,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: ca.SecretName, DefaultMode: &mode},
			},
		})
	default:
		volumes = append(volumes, corev1.Volume{
			Name: redisTLSCAVolumeName,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: ca.ConfigMapName}},
			},
		})
	}
	return volumes
}

// redisTLSConfig is the part of redis.conf that changes at runtime. It comes
// from the status, so that a restarted pod starts in the state that the
// operator last saw. Without spec.tls the pods go back to plaintext, and a
// pod without the TLS volume does not start with tls-replication yes.
func redisTLSConfig(rf *redisfailoverv1.RedisFailover) string {
	if !redisTLSOn(rf) {
		return ""
	}
	replication := "no"
	if rf.Status.TLS != nil && rf.Status.TLS.InternalLinks == redisfailoverv1.TLSStatusLinksTLS {
		replication = "yes"
	}
	return fmt.Sprintf("tls-replication %s\ntls-auth-clients %s\n", replication, tlsAuthClients(rf.Spec.TLS.ClientAuth))
}

func tlsAuthClients(clientAuth string) string {
	switch clientAuth {
	case redisfailoverv1.TLSClientAuthOptional:
		return "optional"
	case redisfailoverv1.TLSClientAuthNone:
		return "no"
	}
	return "yes"
}

// plaintextPortClosed needs spec.tls, because without the TLS port a pod
// with port 0 has no listener.
func plaintextPortClosed(rf *redisfailoverv1.RedisFailover) bool {
	return redisTLSOn(rf) && rf.Status.TLS != nil && rf.Status.TLS.PlaintextPort == redisfailoverv1.TLSStatusPlaintextClosed
}

// redisListenPort is the plaintext port in redis.conf: 0 after the operator
// closed it.
func redisListenPort(rf *redisfailoverv1.RedisFailover) int32 {
	if plaintextPortClosed(rf) {
		return 0
	}
	return rf.Spec.Redis.Port
}

func redisServicePorts(rf *redisfailoverv1.RedisFailover) []corev1.ServicePort {
	var ports []corev1.ServicePort
	if !plaintextPortClosed(rf) {
		ports = append(ports, corev1.ServicePort{
			Name:       "redis",
			Port:       rf.Spec.Redis.Port,
			TargetPort: intstr.FromString("redis"),
			Protocol:   corev1.ProtocolTCP,
		})
	}
	if redisTLSOn(rf) {
		ports = append(ports, corev1.ServicePort{
			Name:       redisTLSPortName,
			Port:       rf.Spec.TLS.Port,
			TargetPort: intstr.FromString(redisTLSPortName),
			Protocol:   corev1.ProtocolTCP,
		})
	}
	return ports
}

// tlsCLIArgs are the redis-cli arguments of ready.sh and shutdown.sh for a pod
// with REDIS_TLS_PORT.
const tlsCLIArgs = "--tls --cacert ${REDIS_TLS_CA_FILE} --cert ${REDIS_TLS_CERT_FILE} --key ${REDIS_TLS_KEY_FILE} -p ${REDIS_TLS_PORT}"

func redisLivenessCLIArgs(rf *redisfailoverv1.RedisFailover) string {
	if !redisTLSOn(rf) {
		return fmt.Sprintf("-p %d", rf.Spec.Redis.Port)
	}
	return fmt.Sprintf("--tls --cacert %s --cert %s/%s --key %s/%s -p %d", redisTLSCAFile(rf), redisTLSDir, tlsCertKey, redisTLSDir, tlsKeyKey, rf.Spec.TLS.Port)
}

// redisExporterTLSEnv turns off the check of the server certificate, because
// the exporter connects to 127.0.0.1 in the same pod, and the certificate
// does not need that address. Redis still checks the client certificate of
// the exporter.
func redisExporterTLSEnv(rf *redisfailoverv1.RedisFailover) []corev1.EnvVar {
	env := []corev1.EnvVar{
		{Name: "REDIS_EXPORTER_SKIP_TLS_VERIFICATION", Value: "true"},
		{Name: "REDIS_EXPORTER_TLS_CA_CERT_FILE", Value: redisTLSCAFile(rf)},
	}
	if rf.Spec.TLS.ClientAuth != redisfailoverv1.TLSClientAuthNone {
		env = append(env,
			corev1.EnvVar{Name: "REDIS_EXPORTER_TLS_CLIENT_CERT_FILE", Value: redisTLSDir + "/" + tlsCertKey},
			corev1.EnvVar{Name: "REDIS_EXPORTER_TLS_CLIENT_KEY_FILE", Value: redisTLSDir + "/" + tlsKeyKey},
		)
	}
	return env
}
