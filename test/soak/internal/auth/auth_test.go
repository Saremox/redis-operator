package auth

import (
	"context"
	"sync"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
)

type secrets struct {
	mu sync.Mutex
	m  map[string]*corev1.Secret
}

func (s *secrets) get(name string) (*corev1.Secret, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sec, ok := s.m[name]; ok {
		return sec, nil
	}
	return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, name)
}

func (s *secrets) set(name, password string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[name] = Secret(s.m[name], "ns", name, password)
}

func rf(uid types.UID, generation int64, secretPath string) *redisfailoverv1.RedisFailover {
	r := &redisfailoverv1.RedisFailover{ObjectMeta: metav1.ObjectMeta{UID: uid, Generation: generation}}
	r.Spec.Auth.SecretPath = secretPath
	return r
}

func TestProviderFollowsTheSecret(t *testing.T) {
	ctx := context.Background()
	m := miniredis.RunT(t)
	sec := &secrets{m: map[string]*corev1.Secret{}}
	src := New(sec.get)
	ping := func(credentials func() (string, string)) error {
		c := redis.NewClient(&redis.Options{Addr: m.Addr(), CredentialsProvider: credentials, MaxRetries: -1})
		defer func() { _ = c.Close() }()
		return c.Ping(ctx).Err()
	}

	src.Update(rf("a", 1, ""))
	if err := ping(src.Provider()); err != nil || src.Password() != "" {
		t.Fatalf("without auth: %v, password %q", err, src.Password())
	}

	// auth added: every new connection authenticates.
	sec.set("x-auth", "one")
	src.Update(rf("a", 2, "x-auth"))
	m.RequireAuth("one")
	if err := ping(src.Provider()); err != nil {
		t.Fatalf("with the password: %v", err)
	}
	pooled := redis.NewClient(&redis.Options{Addr: m.Addr(), CredentialsProvider: src.Provider(), MaxRetries: -1, PoolSize: 1})
	defer func() { _ = pooled.Close() }()
	if err := pooled.Ping(ctx).Err(); err != nil {
		t.Fatal(err)
	}

	// Rotated: new connections use the new password at once, open ones
	// stay authenticated.
	sec.set("x-auth", "two")
	m.RequireAuth("two")
	if src.Password() != "two" {
		t.Errorf("password not followed")
	}
	if err := ping(src.Provider()); err != nil {
		t.Errorf("new connection: %v", err)
	}
	if err := pooled.Ping(ctx).Err(); err != nil {
		t.Errorf("open connection: %v", err)
	}
	if err := ping(Fixed("one")); err == nil {
		t.Error("the old password still works")
	}

	// A read from before a change can't undo it; a new RedisFailover can.
	src.Update(rf("a", 1, ""))
	if src.SecretPath() != "x-auth" {
		t.Errorf("an older generation changed the secretPath")
	}
	src.Update(rf("b", 1, ""))
	if src.SecretPath() != "" || src.Password() != "" {
		t.Errorf("a recreated RedisFailover wasn't followed")
	}

	// A missing Secret is no password.
	src.Update(rf("b", 2, "missing"))
	if src.Password() != "" {
		t.Errorf("password of a missing secret: %q", src.Password())
	}
	var none *Source
	if none.Password() != "" || none.SecretPath() != "" {
		t.Error("a nil source has a password")
	}
}

func TestSecret(t *testing.T) {
	s := Secret(nil, "ns", "x-auth", "pw")
	if s.Name != "x-auth" || s.Namespace != "ns" || string(s.Data["password"]) != "pw" || len(s.Data) != 1 {
		t.Errorf("new secret: %+v", s)
	}
	old := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "x-auth", Namespace: "ns", ResourceVersion: "7"},
		Data: map[string][]byte{"password": []byte("pw"), "other": []byte("kept")}}
	s = Secret(old, "ns", "x-auth", "new")
	if string(s.Data["password"]) != "new" || string(s.Data["other"]) != "kept" || s.ResourceVersion != "7" {
		t.Errorf("updated secret: %+v", s)
	}
	if string(old.Data["password"]) != "pw" {
		t.Error("the original was changed")
	}
}
