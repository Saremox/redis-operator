package mutator

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/client/k8s/clientset/versioned"
	rffake "github.com/saremox/redis-operator/client/k8s/clientset/versioned/fake"
	"github.com/saremox/redis-operator/test/soak/internal/auth"
	"github.com/saremox/redis-operator/test/soak/internal/config"
	"github.com/saremox/redis-operator/test/soak/internal/global"
	"github.com/saremox/redis-operator/test/soak/internal/metrics"
)

// windowHold is a convergence window that ends with the converged value its
// test gives it, or that cancels the run. It keeps the convergence check and
// the Secret password when the mutation is applied.
type windowHold struct {
	done      chan bool
	converged bool
	cancel    context.CancelFunc
	check     func(context.Context) error
	password  *string
	read      func() string
}

func (h *windowHold) Applied() {
	*h.password = h.read()
	if h.cancel != nil {
		h.cancel()
		return
	}
	h.done <- h.converged
}

func (h *windowHold) Done() <-chan bool { return h.done }

// rotation is a password_rotate of instance x from "old" in secret x-auth.
type rotation struct {
	m    *Mutator
	kube *fake.Clientset
	rfs  versioned.Interface
	// windows are the convergence windows the mutation opened.
	windows []*windowHold
	out     bytes.Buffer
	ctx     context.Context
}

// newRotation returns a rotation whose RedisFailover has status. Window i
// ends with converged[i]; a window after them cancels the run.
func newRotation(t *testing.T, status redisfailoverv1.RedisFailoverStatus, converged ...bool) *rotation {
	t.Helper()
	in := config.Instance{Name: "x", Namespace: "ns", Mode: config.ModeSentinel,
		Mutations: config.Mutations{Kinds: map[config.Kind]int{config.PasswordRotate: 1, config.KillMaster: 1}}}
	cfg := &config.Config{}
	cfg.Observer.ConvergenceTimeout.Duration = time.Minute
	cfg.Observer.Interval.Duration = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	r := &rotation{ctx: ctx}
	r.kube = fake.NewClientset(
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "x-auth", Namespace: "ns"}, Data: map[string][]byte{"password": []byte("old")}},
		&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "rfr-x", Namespace: "ns"}},
	)
	rf := &redisfailoverv1.RedisFailover{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "ns"}}
	rf.Spec.Auth.SecretPath = "x-auth"
	rf.Status = status
	r.rfs = rffake.NewSimpleClientset(rf)
	a := auth.New(func(name string) (*corev1.Secret, error) {
		return r.kube.CoreV1().Secrets("ns").Get(context.Background(), name, metav1.GetOptions{})
	})
	mt := metrics.New(prometheus.NewRegistry(), time.Minute)
	r.m = New(in, cfg, r.kube, r.rfs, nil, nil, a, &global.Lock{}, nil, mt, slog.New(slog.NewJSONHandler(&r.out, nil)))
	r.m.hold = func(_ time.Duration, check func(context.Context) error) holdHandle {
		h := &windowHold{done: make(chan bool, 1), check: check, password: new(string), read: r.password}
		if i := len(r.windows); i < len(converged) {
			h.converged = converged[i]
		} else {
			h.cancel = cancel
		}
		r.windows = append(r.windows, h)
		return h
	}
	return r
}

func (r *rotation) password() string {
	s, err := r.kube.CoreV1().Secrets("ns").Get(context.Background(), "x-auth", metav1.GetOptions{})
	if err != nil {
		return err.Error()
	}
	return string(s.Data["password"])
}

func (r *rotation) run(t *testing.T, kind config.Kind) {
	t.Helper()
	p := planAuth(kind, authState())
	if next := r.m.execute(r.ctx, 1, authState(), p, r.m.log); next != "" {
		t.Fatalf("next kind %q", next)
	}
}

func (r *rotation) results(t *testing.T, kind config.Kind) map[string]float64 {
	t.Helper()
	got := map[string]float64{}
	for _, res := range []string{resultConverged, resultTimeout, resultStuckRestored} {
		got[res] = testutil.ToFloat64(r.m.total.WithLabelValues(string(kind), res))
	}
	return got
}

func (r *rotation) logged(msg string) string {
	for line := range strings.SplitSeq(r.out.String(), "\n") {
		if strings.Contains(line, `"msg":"`+msg+`"`) {
			return line
		}
	}
	return ""
}

var stuck = redisfailoverv1.RedisFailoverStatus{State: redisfailoverv1.NotHealthyState, Message: passwordNotApplied}

// A rotation that the operator cannot apply puts the previous password back,
// converges in a second window, and counts as stuck_restored. The timeout
// stays in the log.
func TestStuckRotationRestored(t *testing.T) {
	r := newRotation(t, stuck, false, true)
	r.run(t, config.PasswordRotate)
	if len(r.windows) != 2 {
		t.Fatalf("%d windows, want 2", len(r.windows))
	}
	if first, second := *r.windows[0].password, *r.windows[1].password; first == "old" || second != "old" {
		t.Errorf("the Secret had %q in the rotation and %q in the restore, want a new one and old", first, second)
	}
	if got := r.password(); got != "old" {
		t.Errorf("the Secret has %q, want old", got)
	}
	want := map[string]float64{resultConverged: 0, resultTimeout: 0, resultStuckRestored: 1}
	if got := r.results(t, config.PasswordRotate); !maps.Equal(got, want) {
		t.Errorf("results %v, want %v", got, want)
	}
	if line := r.logged("rotation timed out, the tester sets the previous password again"); !strings.Contains(line, `"result":"timeout"`) ||
		!strings.Contains(line, `"status_message":"`+passwordNotApplied+`"`) {
		t.Errorf("the timeout log line is %q", line)
	}
	if line := r.logged("mutation done"); !strings.Contains(line, `"result":"stuck_restored"`) || !strings.Contains(line, `"level":"WARN"`) {
		t.Errorf("the result log line is %q", line)
	}

	// The restore window waits for Healthy.
	check := r.windows[1].check
	if err := check(context.Background()); err == nil {
		t.Error("the restore converged while NotHealthy")
	}
	rf, err := r.rfs.DatabasesV1().RedisFailovers("ns").Get(context.Background(), "x", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	rf.Status = redisfailoverv1.RedisFailoverStatus{State: redisfailoverv1.HealthyState}
	if _, err := r.rfs.DatabasesV1().RedisFailovers("ns").UpdateStatus(context.Background(), rf, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := check(context.Background()); err != nil {
		t.Errorf("the restore did not converge when Healthy: %v", err)
	}
	if err := r.kube.AppsV1().StatefulSets("ns").Delete(context.Background(), "rfr-x", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := check(context.Background()); err == nil {
		t.Error("the restore converged without a StatefulSet")
	}
}

// A restore that does not converge either is a timeout, and the instance
// keeps the previous password: the tester tries only once.
func TestStuckRotationNotRestored(t *testing.T) {
	r := newRotation(t, stuck, false, false)
	r.run(t, config.PasswordRotate)
	if len(r.windows) != 2 {
		t.Fatalf("%d windows, want 2", len(r.windows))
	}
	if got := r.password(); got != "old" {
		t.Errorf("the Secret has %q, want old", got)
	}
	want := map[string]float64{resultConverged: 0, resultTimeout: 1, resultStuckRestored: 0}
	if got := r.results(t, config.PasswordRotate); !maps.Equal(got, want) {
		t.Errorf("results %v, want %v", got, want)
	}
}

// Only the status of an operator that cannot apply the password starts a
// restore. A converged rotation needs none.
func TestRotationNotStuck(t *testing.T) {
	for name, c := range map[string]struct {
		status    redisfailoverv1.RedisFailoverStatus
		converged bool
		result    string
	}{
		"another message": {redisfailoverv1.RedisFailoverStatus{State: redisfailoverv1.NotHealthyState, Message: "unable to check the pods"}, false, resultTimeout},
		"Healthy":         {redisfailoverv1.RedisFailoverStatus{State: redisfailoverv1.HealthyState}, false, resultTimeout},
		"converged":       {stuck, true, resultConverged},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRotation(t, c.status, c.converged)
			r.run(t, config.PasswordRotate)
			if len(r.windows) != 1 {
				t.Errorf("%d windows, want 1", len(r.windows))
			}
			if got := r.password(); got == "old" {
				t.Error("the Secret has the previous password")
			}
			if got := r.results(t, config.PasswordRotate); got[c.result] != 1 || got[resultStuckRestored] != 0 {
				t.Errorf("results %v, want 1 %s", got, c.result)
			}
		})
	}
}

// Only password_rotate puts the previous password back. Scenario C does so
// itself, and the other auth kinds change more than the password.
func TestRestoreOnlyForRotation(t *testing.T) {
	m := mutations()
	p := planFor(t, config.PasswordRotate, m, authState(), "rfr-x-0")
	if p.restore == nil || p.restore.name != "x-auth" || p.restore.password != "old" {
		t.Errorf("password_rotate restores %+v", p.restore)
	}
	off := authState()
	off.rf.Spec.Auth.SecretPath = ""
	for _, c := range []struct {
		kind config.Kind
		s    state
	}{{config.PasswordRotateOffline, authState()}, {config.AuthRemove, authState()}, {config.AuthAdd, off}} {
		if p := planFor(t, c.kind, m, c.s, "rfr-x-0"); p.restore != nil {
			t.Errorf("%s restores %+v", c.kind, p.restore)
		}
	}

	r := newRotation(t, stuck, false, true)
	r.run(t, config.AuthRemove)
	if len(r.windows) != 1 {
		t.Errorf("auth_remove opened %d windows, want 1", len(r.windows))
	}
}

// The timeout alert uses increase(), which does not see a series that starts
// at 1. So New creates the stuck_restored series of password_rotate only.
func TestNewCreatesStuckRestoredSeries(t *testing.T) {
	r := newRotation(t, stuck)
	ch := make(chan prometheus.Metric, 32)
	r.m.total.Collect(ch)
	close(ch)
	var got []string
	for metric := range ch {
		var d dto.Metric
		if err := metric.Write(&d); err != nil {
			t.Fatal(err)
		}
		labels := map[string]string{}
		for _, l := range d.GetLabel() {
			labels[l.GetName()] = l.GetValue()
		}
		if labels["result"] == resultStuckRestored {
			got = append(got, labels["kind"])
		}
	}
	if want := []string{string(config.PasswordRotate)}; !slices.Equal(got, want) {
		t.Errorf("stuck_restored series of %v, want %v", got, want)
	}
}

func TestAcceptedAndHealthy(t *testing.T) {
	runPredicateOn(t, rotated, acceptedAndHealthy, []predicateCase{
		{"converged", func(*state) {}, true},
		{"a pod refuses the password", func(s *state) { s.auth["rfr-x-1"] = context.DeadlineExceeded }, false},
		{"not Healthy", func(s *state) { s.rf.Status = stuck }, false},
	})
}

// A run that ends in the restore records no result, as in any other window.
func TestStuckRotationCanceled(t *testing.T) {
	r := newRotation(t, stuck, false)
	r.run(t, config.PasswordRotate)
	if len(r.windows) != 2 {
		t.Fatalf("%d windows, want 2", len(r.windows))
	}
	want := map[string]float64{resultConverged: 0, resultTimeout: 0, resultStuckRestored: 0}
	if got := r.results(t, config.PasswordRotate); !maps.Equal(got, want) {
		t.Errorf("results %v, want %v", got, want)
	}
}

// A previous password that the tester cannot set leaves the restore to time
// out.
func TestStuckRotationSecretNotUpdated(t *testing.T) {
	r := newRotation(t, stuck, false, false)
	updates := 0
	r.kube.PrependReactor("update", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		if updates++; updates > 1 {
			return true, nil, errors.New("denied")
		}
		return false, nil, nil
	})
	r.run(t, config.PasswordRotate)
	if got := r.password(); got == "old" {
		t.Error("the Secret has the previous password")
	}
	if got := r.results(t, config.PasswordRotate); got[resultTimeout] != 1 {
		t.Errorf("results %v, want 1 timeout", got)
	}
	if line := r.logged("previous password not set"); !strings.Contains(line, "denied") {
		t.Errorf("the log line is %q", line)
	}
}
