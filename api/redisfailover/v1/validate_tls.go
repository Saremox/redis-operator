package v1

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

// tlsAvailable is false until the operator can change a running RedisFailover
// to TLS and renew its certificates in place. Until then, a certificate that
// expires in a running pod stops the replication.
var tlsAvailable = false

var errTLSNotAvailable = errors.New("tls: TLS is not available in this release of the operator")

// tlsTunableConfigs are the tls-* parameters that redis.customConfig can set
// with TLS. The operator sets the other tls-* parameters and the port.
var tlsTunableConfigs = []string{
	"tls-protocols",
	"tls-ciphers",
	"tls-ciphersuites",
	"tls-prefer-server-ciphers",
	"tls-session-caching",
	"tls-session-cache-size",
	"tls-session-cache-timeout",
}

var imageVersionPattern = regexp.MustCompile(`^v?(\d+)(?:\.(\d+))?`)

// validateTLS sets the defaults of the TLS settings and rejects a spec that
// the TLS support does not handle.
func (r *RedisFailover) validateTLS() error {
	t := r.Spec.TLS
	if t.Port == 0 {
		t.Port = DefaultTLSPort
	}
	if t.PlaintextPort == "" {
		t.PlaintextPort = TLSPlaintextPortEnabled
	}
	if t.ClientAuth == "" {
		t.ClientAuth = TLSClientAuthRequired
	}
	if t.ServerName == "" {
		t.ServerName = fmt.Sprintf("rfrm-%s.%s.svc", r.Name, r.Namespace)
	}
	if t.CA != nil && t.CA.Key == "" {
		t.CA.Key = DefaultTLSCAKey
	}

	switch {
	case len(validation.IsDNS1123Subdomain(t.SecretName)) > 0:
		return fmt.Errorf("tls.secretName %q must be the name of a Secret", t.SecretName)
	case t.CA != nil && (t.CA.SecretName == "") == (t.CA.ConfigMapName == ""):
		return errors.New("tls.ca must set one of secretName and configMapName")
	case t.CA != nil && len(validation.IsDNS1123Subdomain(t.CA.SecretName+t.CA.ConfigMapName)) > 0:
		return fmt.Errorf("tls.ca names %q, which is not a valid object name", t.CA.SecretName+t.CA.ConfigMapName)
	case t.CA != nil && len(validation.IsConfigMapKey(t.CA.Key)) > 0:
		return fmt.Errorf("tls.ca.key %q is not a valid key", t.CA.Key)
	case t.Port < 1 || t.Port > 65535:
		return fmt.Errorf("tls.port %d must be between 1 and 65535, or 0 for the default", t.Port)
	case t.Port == r.Spec.Redis.Port:
		return fmt.Errorf("tls.port %d must be different from redis.port", t.Port)
	case r.Spec.Redis.Exporter.Enabled && t.Port == r.redisExporterPort():
		return fmt.Errorf("tls.port %d must be different from the port of the Redis exporter", t.Port)
	case t.PlaintextPort != TLSPlaintextPortEnabled && t.PlaintextPort != TLSPlaintextPortDisabled:
		return fmt.Errorf("tls.plaintextPort %q must be Enabled or Disabled", t.PlaintextPort)
	case t.ClientAuth != TLSClientAuthRequired && t.ClientAuth != TLSClientAuthOptional && t.ClientAuth != TLSClientAuthNone:
		return fmt.Errorf("tls.clientAuth %q must be Required, Optional or None", t.ClientAuth)
	case len(validation.IsDNS1123Subdomain(t.ServerName)) > 0:
		return fmt.Errorf("tls.serverName %q must be a DNS name", t.ServerName)
	case r.SentinelEnabled():
		return errors.New("tls: the operator supports TLS only without Sentinel (sentinel.enabled false)")
	case r.Bootstrapping():
		return errors.New("tls: bootstrapNode cannot be used with TLS")
	case len(r.Spec.Redis.Command) > 0:
		return errors.New("tls: redis.command cannot be used with TLS, because the operator adds the TLS arguments to the default command")
	}

	for _, c := range r.Spec.Redis.CustomConfig {
		param, _, _ := strings.Cut(c, " ")
		param = strings.ToLower(param)
		if param == "port" || (strings.HasPrefix(param, "tls-") && !slices.Contains(tlsTunableConfigs, param)) {
			return fmt.Errorf("redis.customConfig cannot set %q with TLS, because the operator sets it", param)
		}
	}

	if major, minor, ok := imageVersion(r.Spec.Redis.Image); ok && (major < 6 || (major == 6 && minor >= 0 && minor < 2)) {
		return fmt.Errorf("tls needs Redis or Valkey 6.2 or later, and redis.image %q is older", r.Spec.Redis.Image)
	}
	return nil
}

func (r *RedisFailover) redisExporterPort() int32 {
	if r.Spec.Redis.Exporter.Port != 0 {
		return r.Spec.Redis.Exporter.Port
	}
	return DefaultRedisExporterPort
}

// imageVersion reads the major and minor version from the image tag. The
// minor version is -1 when the tag has none. ok is false when the tag does
// not start with a version, for example "latest" or a digest.
func imageVersion(image string) (major, minor int, ok bool) {
	image, _, _ = strings.Cut(image, "@")
	i := strings.LastIndex(image, ":")
	if i < 0 || strings.Contains(image[i:], "/") {
		return 0, 0, false
	}
	m := imageVersionPattern.FindStringSubmatch(image[i+1:])
	if m == nil {
		return 0, 0, false
	}
	major, _ = strconv.Atoi(m[1])
	minor = -1
	if m[2] != "" {
		minor, _ = strconv.Atoi(m[2])
	}
	return major, minor, true
}
