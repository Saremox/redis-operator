// Package auth follows the password in the auth Secret of an instance, so that
// each connection of the tester authenticates as an application does.
package auth

import (
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
)

// passwordKey is the key of the Secret the operator reads the password from.
const passwordKey = "password"

// Lookup returns a Secret of the instance's namespace.
type Lookup func(name string) (*corev1.Secret, error)

// Source holds the Secret spec.auth.secretPath names, and reads the
// password from it on every call, so a changed Secret takes effect at once.
type Source struct {
	lookup Lookup

	mu         sync.Mutex
	uid        types.UID
	generation int64
	secretPath string
}

func New(lookup Lookup) *Source {
	return &Source{lookup: lookup}
}

// Update follows the spec.auth.secretPath of rf. It ignores an rf older than
// one that it saw before, so that a stale read cannot undo a change.
func (s *Source) Update(rf *redisfailoverv1.RedisFailover) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rf.UID == s.uid && rf.Generation < s.generation {
		return
	}
	s.uid, s.generation, s.secretPath = rf.UID, rf.Generation, rf.Spec.Auth.SecretPath
}

// SecretPath returns the Secret the RedisFailover authenticates with, ""
// without auth.
func (s *Source) SecretPath() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.secretPath
}

// Password returns the Secret's current password, "" without auth.
func (s *Source) Password() string {
	path := s.SecretPath()
	if path == "" {
		return ""
	}
	secret, err := s.lookup(path)
	if err != nil {
		return ""
	}
	return string(secret.Data[passwordKey])
}

// Provider authenticates every new connection with the current password.
func (s *Source) Provider() func() (string, string) {
	return func() (string, string) { return "", s.Password() }
}

// Fixed authenticates every new connection with password.
func Fixed(password string) func() (string, string) {
	return func() (string, string) { return "", password }
}

// Secret returns secret with its password set to password, or a new Secret
// of that name and namespace if secret is nil.
func Secret(secret *corev1.Secret, namespace, name, password string) *corev1.Secret {
	if secret == nil {
		secret = &corev1.Secret{}
		secret.Name, secret.Namespace = name, namespace
	} else {
		secret = secret.DeepCopy()
	}
	if secret.Data == nil {
		secret.Data = map[string][]byte{}
	}
	secret.Data[passwordKey] = []byte(password)
	return secret
}
