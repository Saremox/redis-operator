package mutator

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/test/soak/internal/config"
)

// authState is testState in Sentinel mode with auth from secret x-auth,
// whose pods and Sentinels run the password "old".
func authState() state {
	s := testState()
	enabled := true
	s.rf.Spec.Sentinel.Enabled = &enabled
	s.rf.Spec.Auth.SecretPath = "x-auth"
	s.rf.Status.State = redisfailoverv1.HealthyState
	s.password, s.authSecret = "old", "x-auth"
	s.sts.Spec.Template.Annotations = map[string]string{secretChecksum: "sum-old"}
	for i := range s.redis {
		s.redis[i].Annotations = map[string]string{secretChecksum: "sum-old"}
	}
	return s
}

func TestAuthPlans(t *testing.T) {
	m := mutations()
	p := planFor(t, config.PasswordRotate, m, authState(), "rfr-x-0")
	if p.secret == nil || p.secret.name != "x-auth" || p.secret.password == "" || p.secret.password == "old" || p.patch != nil {
		t.Errorf("password_rotate: %+v", p)
	}
	if *p.fetch.password != p.secret.password || !p.fetch.sentinelMaster {
		t.Errorf("password_rotate checks %+v", p.fetch)
	}
	if again := planFor(t, config.PasswordRotate, m, authState(), "rfr-x-0"); again.secret.password == p.secret.password {
		t.Error("the same password twice")
	}

	p = planFor(t, config.AuthRemove, m, authState(), "rfr-x-0")
	if string(p.patch) != `{"spec":{"auth":{"secretPath":null}}}` || p.secret != nil || *p.fetch.password != "" {
		t.Errorf("auth_remove: %s %+v", p.patch, p)
	}

	off := authState()
	off.rf.Spec.Auth.SecretPath = ""
	off.authSecret = "x-new"
	p = planFor(t, config.AuthAdd, m, off, "rfr-x-0")
	if string(p.patch) != `{"spec":{"auth":{"secretPath":"x-new"}}}` || p.secret == nil || p.secret.name != "x-new" ||
		*p.fetch.password != p.secret.password || p.secret.password == "" {
		t.Errorf("auth_add: %s %+v", p.patch, p)
	}

	p = planFor(t, config.PasswordRotateOffline, m, authState(), "rfr-x-0")
	o := p.offline
	if o == nil || o.secret != "x-auth" || o.previous != "old" || o.first == "" || o.second == "" ||
		o.first == o.second || o.first == "old" || *p.fetch.password != o.second || p.secret != nil || p.patch != nil {
		t.Errorf("password_rotate_offline: %+v %+v", p, o)
	}

	// Passwords never show in what is logged.
	for _, k := range []config.Kind{config.PasswordRotate, config.AuthRemove, config.PasswordRotateOffline} {
		p := planFor(t, k, m, authState(), "rfr-x-0")
		for _, pw := range []string{"old", *p.fetch.password} {
			if pw != "" && strings.Contains(p.params, pw) {
				t.Errorf("%s params %q show a password", k, p.params)
			}
		}
	}

	for _, c := range []struct {
		kind config.Kind
		s    state
	}{{config.AuthAdd, authState()}, {config.AuthRemove, off}, {config.PasswordRotate, off}, {config.PasswordRotateOffline, off}} {
		if p := planFor(t, c.kind, m, c.s, "rfr-x-0"); p.skip == "" {
			t.Errorf("%s not skipped: %+v", c.kind, p)
		}
	}
}

func TestSentinelTogglePlan(t *testing.T) {
	p := planFor(t, config.SentinelToggle, mutations(), authState(), "rfr-x-0")
	if string(p.patch) != `{"spec":{"sentinel":{"enabled":false}}}` || !p.fetch.sentinelObjects || p.params != "sentinel.enabled true -> false" {
		t.Errorf("off: %s %+v", p.patch, p)
	}
	p = planFor(t, config.SentinelToggle, mutations(), testState(), "rfr-x-0")
	if string(p.patch) != `{"spec":{"sentinel":{"enabled":true}}}` {
		t.Errorf("on: %s", p.patch)
	}
}

// rotated is authState once the operator applied a new password and rolled
// the pods onto it.
func rotated() state {
	s := authState()
	s.sts.Spec.Template.Annotations[secretChecksum] = "sum-new"
	s.auth = map[string]error{}
	for i := range s.redis {
		s.redis[i].Annotations[secretChecksum] = "sum-new"
		s.auth[s.redis[i].Name] = nil
	}
	s.sentinelMasters = map[string]sentinelMaster{}
	for _, p := range s.sentinels {
		s.sentinelMasters[p.Name] = sentinelMaster{fields: map[string]string{"flags": "master", "num-slaves": "2"}}
	}
	return s
}

func TestAuthConverged(t *testing.T) {
	wrongpass := errors.New("WRONGPASS invalid username-password pair or user is disabled.")
	runPredicateOn(t, rotated, authConverged("sum-old", 3), []predicateCase{
		{"converged", func(*state) {}, true},
		{"a pod refuses the password", func(s *state) { s.auth["rfr-x-1"] = wrongpass }, false},
		{"a pod wasn't checked", func(s *state) { delete(s.auth, "rfr-x-2") }, false},
		{"passwords not checked", func(s *state) { s.auth = nil }, false},
		{"the operator hasn't changed the template", func(s *state) {
			s.sts.Spec.Template.Annotations[secretChecksum] = "sum-old"
			for i := range s.redis {
				s.redis[i].Annotations[secretChecksum] = "sum-old"
			}
		}, false},
		{"a pod not rolled yet", func(s *state) { s.redis[0].Annotations[secretChecksum] = "sum-old" }, false},
		{"a pod not ready", func(s *state) { s.redis[2].Status.Conditions[0].Status = corev1.ConditionFalse }, false},
		{"rollout not observed", func(s *state) { s.sts.Generation = 3 }, false},
		{"a Sentinel sees the master down", func(s *state) {
			s.sentinelMasters["rfs-x-a"].fields["flags"] = "master,s_down"
		}, false},
		{"a Sentinel is disconnected", func(s *state) {
			s.sentinelMasters["rfs-x-b"].fields["flags"] = "master,disconnected"
		}, false},
		{"a Sentinel misses a replica", func(s *state) {
			s.sentinelMasters["rfs-x-c"].fields["num-slaves"] = "1"
		}, false},
		{"a Sentinel unreachable", func(s *state) {
			s.sentinelMasters["rfs-x-c"] = sentinelMaster{err: errors.New("connection refused")}
		}, false},
		{"not Healthy", func(s *state) {
			s.rf.Status = redisfailoverv1.RedisFailoverStatus{State: redisfailoverv1.NotHealthyState, Message: passwordNotApplied}
		}, false},
		{"operator mode has no Sentinels to ask", func(s *state) {
			s.rf.Spec.Sentinel.Enabled = nil
			s.sentinelMasters = nil
		}, true},
	})
}

func TestToggleConverged(t *testing.T) {
	on := func() state {
		s := authState()
		s.sentinelService, s.sentinelConfigMap = true, true
		return s
	}
	runPredicateOn(t, on, toggleConverged(true, 3, 3), []predicateCase{
		{"converged", func(*state) {}, true},
		{"not switched yet", func(s *state) { s.rf.Spec.Sentinel.Enabled = nil }, false},
		{"no Service", func(s *state) { s.sentinelService = false }, false},
		{"no ConfigMap", func(s *state) { s.sentinelConfigMap = false }, false},
		{"no Deployment", func(s *state) { s.sentinel = nil }, false},
		{"a Sentinel not ready", func(s *state) { s.sentinels[1].Status.Conditions[0].Status = corev1.ConditionFalse }, false},
		{"not Healthy", func(s *state) { s.rf.Status.State = redisfailoverv1.NotHealthyState }, false},
	})
	off := func() state {
		s := authState()
		s.rf.Spec.Sentinel.Enabled = nil
		s.sentinel, s.sentinels = nil, nil
		return s
	}
	runPredicateOn(t, off, toggleConverged(false, 3, 3), []predicateCase{
		{"converged", func(*state) {}, true},
		{"Deployment left", func(s *state) { s.sentinel = authState().sentinel }, false},
		{"Service left", func(s *state) { s.sentinelService = true }, false},
		{"ConfigMap left", func(s *state) { s.sentinelConfigMap = true }, false},
		{"a Sentinel pod left", func(s *state) { s.sentinels = authState().sentinels[:1] }, false},
		{"a redis pod not ready", func(s *state) { s.redis[0].Status.Conditions[0].Status = corev1.ConditionFalse }, false},
	})
}

func TestSetPassword(t *testing.T) {
	ctx := context.Background()
	kube := fake.NewClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "x-auth", Namespace: "ns"},
		Data:       map[string][]byte{"password": []byte("old"), "other": []byte("kept")},
	})
	m := &Mutator{in: testInstance, kube: kube, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := m.setPassword(ctx, "x-auth", "new"); err != nil {
		t.Fatal(err)
	}
	if err := m.setPassword(ctx, "x-created", "pw"); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]map[string]string{
		"x-auth":    {"password": "new", "other": "kept"},
		"x-created": {"password": "pw"},
	} {
		s, err := kube.CoreV1().Secrets("ns").Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if len(s.Data) != len(want) {
			t.Errorf("%s: %v", name, s.Data)
		}
		for k, v := range want {
			if string(s.Data[k]) != v {
				t.Errorf("%s: %s = %q, want %q", name, k, s.Data[k], v)
			}
		}
	}
}
