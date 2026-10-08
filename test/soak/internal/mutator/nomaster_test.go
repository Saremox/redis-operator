package mutator

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	miniserver "github.com/alicebob/miniredis/v2/server"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	rffake "github.com/saremox/redis-operator/client/k8s/clientset/versioned/fake"
	"github.com/saremox/redis-operator/test/soak/internal/config"
	"github.com/saremox/redis-operator/test/soak/internal/metrics"
)

// calls records the steps of a mutation in the order that they happen.
type calls struct {
	mu     sync.Mutex
	events []string
	// epochs is the config-epoch that a fake Sentinel reports by address. A
	// Sentinel without an entry answers SENTINEL MASTER with an error.
	epochs map[string]string
}

func (c *calls) setEpoch(addr, epoch string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.epochs == nil {
		c.epochs = map[string]string{}
	}
	c.epochs[addr] = epoch
}

func (c *calls) epoch(addr string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.epochs[addr]
	return e, ok
}

func (c *calls) add(format string, a ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, fmt.Sprintf(format, a...))
}

func (c *calls) get() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.events)
}

// fakeSentinels starts one fake Sentinel for each loopback address, all on
// the same port, and returns the port. A Sentinel answers SENTINEL RESET
// with failing[addr], or with 1. SENTINEL MASTER mymaster answers with the
// epoch of rec. No other command is valid.
func fakeSentinels(t *testing.T, rec *calls, failing map[string]string) (port int, addrs []string) {
	t.Helper()
	addrs = []string{"127.0.0.1", "127.0.0.2", "127.0.0.3"}
	for range 10 {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port = l.Addr().(*net.TCPAddr).Port
		_ = l.Close()
		var started []*miniredis.Miniredis
		ok := true
		for _, addr := range addrs {
			m := miniredis.NewMiniRedis()
			if err := m.StartAddr(net.JoinHostPort(addr, fmt.Sprint(port))); err != nil {
				ok = false
				break
			}
			started = append(started, m)
			m.Server().SetPreHook(func(c *miniserver.Peer, cmd string, args ...string) bool {
				if !strings.EqualFold(cmd, "SENTINEL") {
					return false
				}
				if len(args) == 2 && strings.EqualFold(args[0], "MASTER") && args[1] == "mymaster" {
					epoch, ok := rec.epoch(addr)
					if !ok {
						c.WriteError("ERR no such master with that name")
						return true
					}
					rec.add("epoch %s", addr)
					c.WriteStrings([]string{"name", "mymaster", "flags", "master", "config-epoch", epoch})
					return true
				}
				if len(args) != 2 || !strings.EqualFold(args[0], "RESET") || args[1] != "*" {
					c.WriteError("ERR unexpected " + strings.Join(args, " "))
					return true
				}
				rec.add("reset %s", addr)
				if msg := failing[addr]; msg != "" {
					c.WriteError(msg)
					return true
				}
				c.WriteInt(1)
				return true
			})
		}
		if ok {
			t.Cleanup(func() {
				for _, m := range started {
					m.Close()
				}
			})
			return port, addrs
		}
		for _, m := range started {
			m.Close()
		}
	}
	t.Skip("no free port on the loopback addresses")
	return 0, nil
}

// noMasterEnv is a mutator with a fake Kubernetes client that records each
// pod deletion, and the state of a Sentinel instance whose Sentinels are the
// fake ones.
func noMasterEnv(t *testing.T, rec *calls, failing map[string]string) (*Mutator, state, int) {
	t.Helper()
	port, addrs := fakeSentinels(t, rec, failing)
	s := authState()
	s.rf.Spec.Auth.SecretPath = ""
	var objects []runtime.Object
	for i := range s.sentinels {
		s.sentinels[i].Status.PodIP = addrs[i]
	}
	for i := range s.redis {
		p := s.redis[i].DeepCopy()
		p.Namespace = "ns"
		objects = append(objects, p)
	}
	kube := fake.NewClientset(objects...)
	kube.PrependReactor("delete", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		d := a.(k8stesting.DeleteActionImpl)
		opts := d.GetDeleteOptions()
		var uid, grace string
		if opts.Preconditions != nil && opts.Preconditions.UID != nil {
			uid = string(*opts.Preconditions.UID)
		}
		if opts.GracePeriodSeconds != nil {
			grace = fmt.Sprintf(" grace=%d", *opts.GracePeriodSeconds)
		}
		rec.add("delete %s uid=%s%s", d.GetName(), uid, grace)
		return false, nil, nil
	})
	rfs := rffake.NewSimpleClientset(&redisfailoverv1.RedisFailover{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "ns"}})
	m := &Mutator{in: config.Instance{Name: "x", Namespace: "ns"}, kube: kube, rfs: rfs, timeout: time.Second}
	m.observerCfg.ConvergenceTimeout.Duration = time.Minute
	return m, s, port
}

func TestNoMasterOrder(t *testing.T) {
	rec := &calls{}
	m, s, port := noMasterEnv(t, rec, nil)
	p := m.planNoMaster(s, "rfr-x-0", port)
	if p.skip != "" || p.kind != config.SentinelResetKillMaster || p.action == nil || p.patch != nil || p.pod != "" {
		t.Fatalf("plan %+v", p)
	}
	if p.params != "SENTINEL RESET * on 3 Sentinels, delete pod rfr-x-0 (graceful)" {
		t.Errorf("params %q", p.params)
	}
	if err := m.apply(context.Background(), p, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	got := rec.get()
	// The Sentinels reset in parallel, so that only the order of the groups
	// is fixed. The pod delete is graceful and holds the UID of the plan.
	if len(got) != 4 || got[3] != "delete rfr-x-0 uid=u0" {
		t.Fatalf("calls %v, want 3 resets and then the delete of the master", got)
	}
	resets := got[:3]
	slices.Sort(resets)
	if want := []string{"reset 127.0.0.1", "reset 127.0.0.2", "reset 127.0.0.3"}; !slices.Equal(resets, want) {
		t.Errorf("resets %v, want %v", resets, want)
	}
	// No replica is touched.
	for _, e := range got {
		if strings.Contains(e, "rfr-x-1") || strings.Contains(e, "rfr-x-2") {
			t.Errorf("touched a replica: %v", got)
		}
	}
}

func TestNoMasterFollowsTheMaster(t *testing.T) {
	rec := &calls{}
	m, s, port := noMasterEnv(t, rec, nil)
	s.redis[0].Labels[roleLabel], s.redis[2].Labels[roleLabel] = roleReplica, roleMaster
	p := m.planNoMaster(s, "rfr-x-2", port)
	if err := m.apply(context.Background(), p, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	if got := rec.get(); got[len(got)-1] != "delete rfr-x-2 uid=u2" {
		t.Errorf("calls %v", got)
	}
}

// A Sentinel that does not accept the reset can still fail over. Then the
// mutation fails and the master stays.
func TestNoMasterResetFails(t *testing.T) {
	rec := &calls{}
	m, s, port := noMasterEnv(t, rec, map[string]string{"127.0.0.2": "ERR no"})
	err := m.apply(context.Background(), m.planNoMaster(s, "rfr-x-0", port), slog.New(slog.DiscardHandler))
	if err == nil || !strings.Contains(err.Error(), "SENTINEL RESET on rfs-x-b") || strings.Contains(err.Error(), "rfs-x-a") {
		t.Errorf("error %v, want the one Sentinel that failed", err)
	}
	for _, e := range rec.get() {
		if strings.HasPrefix(e, "delete") {
			t.Errorf("deleted a pod after a failed reset: %v", rec.get())
		}
	}
}

// A Sentinel without an address cannot reset, as other mutations report a
// pod that they cannot reach.
func TestNoMasterSentinelUnreachable(t *testing.T) {
	rec := &calls{}
	m, s, port := noMasterEnv(t, rec, nil)
	s.sentinels[2].Status.PodIP = ""
	err := m.apply(context.Background(), m.planNoMaster(s, "rfr-x-0", port), slog.New(slog.DiscardHandler))
	if err == nil || !strings.Contains(err.Error(), "rfs-x-c") || !strings.Contains(err.Error(), "no pod IP") {
		t.Errorf("error %v", err)
	}
	if slices.ContainsFunc(rec.get(), func(e string) bool { return strings.HasPrefix(e, "delete") }) {
		t.Errorf("deleted a pod after a failed reset: %v", rec.get())
	}
}

func TestNoMasterDeleteFails(t *testing.T) {
	rec := &calls{}
	m, s, port := noMasterEnv(t, rec, nil)
	denied := errors.New("denied")
	m.kube.(*fake.Clientset).PrependReactor("delete", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, denied
	})
	err := m.apply(context.Background(), m.planNoMaster(s, "rfr-x-0", port), slog.New(slog.DiscardHandler))
	if !errors.Is(err, denied) {
		t.Errorf("error %v, want the delete error", err)
	}
	if got := rec.get(); len(got) != 3 {
		t.Errorf("calls %v, want the 3 resets", got)
	}
}

func TestNoMasterSkips(t *testing.T) {
	m := &Mutator{}
	cases := []struct {
		name   string
		change func(*state)
		master string
		want   string
	}{
		{"Sentinel is off", func(s *state) { s.rf.Spec.Sentinel.Enabled = nil }, "rfr-x-0", "Sentinel is off"},
		{"one redis pod", func(s *state) { s.rf.Spec.Redis.Replicas = 1 }, "rfr-x-0", "no replica to promote"},
		{"no Sentinel pod", func(s *state) { s.sentinels = nil }, "rfr-x-0", "no sentinel pod"},
		{"the observer disagrees", func(*state) {}, "rfr-x-1", "the observer saw"},
		{"two labelled masters", func(s *state) { s.redis[1].Labels[roleLabel] = roleMaster }, "rfr-x-0", "2 pods are labelled master"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := authState()
			c.change(&s)
			p := m.planNoMaster(s, c.master, sentinelPort)
			if !strings.Contains(p.skip, c.want) || p.action != nil {
				t.Errorf("skip %q, want %q", p.skip, c.want)
			}
		})
	}
}

// The mutator reaches the plan through plan(), which reads the master of the
// observer. The observer of a new mutator has seen no master yet.
func TestNoMasterIsPlanned(t *testing.T) {
	m := failedResetMutator(t, fakeData{})
	p := m.plan(stepRand(1, m.in, 1), config.SentinelResetKillMaster, authState())
	if p.kind != config.SentinelResetKillMaster || !strings.Contains(p.skip, "the observer saw") {
		t.Errorf("plan %+v", p)
	}
}

// noMasterRecovered is the state after the recovery: the killed pod rfr-x-0
// is a new pod, and the Sentinels know the replicas again.
func noMasterRecovered() state {
	s := authState()
	s.sentinelMasters = map[string]sentinelMaster{}
	for _, p := range s.sentinels {
		s.sentinelMasters[p.Name] = sentinelMaster{fields: map[string]string{"flags": "master", "num-slaves": "2"}}
	}
	return s
}

func TestNoMasterConverged(t *testing.T) {
	runPredicateOn(t, noMasterRecovered, noMasterConverged("rfr-x-0", "old", 3, 3), []predicateCase{
		{"converged", func(*state) {}, true},
		{"the pod was not deleted yet", func(s *state) { s.redis[0].UID = "old" }, false},
		{"the pod is not back", func(s *state) { s.redis = s.redis[1:] }, false},
		{"a redis pod not ready", func(s *state) { s.redis[1].Status.Conditions[0].Status = corev1.ConditionFalse }, false},
		{"a Sentinel pod not ready", func(s *state) { s.sentinels[0].Status.Conditions[0].Status = corev1.ConditionFalse }, false},
		{"no pod is labelled master", func(s *state) { s.redis[1].Labels[roleLabel] = roleReplica; s.redis[0].Labels[roleLabel] = roleReplica }, false},
		{"two pods are labelled master", func(s *state) { s.redis[1].Labels[roleLabel] = roleMaster }, false},
		// The state of the report: a Sentinel that knows no replica cannot fail over.
		{"a Sentinel knows no replica", func(s *state) { s.sentinelMasters["rfs-x-b"].fields["num-slaves"] = "0" }, false},
		{"a Sentinel sees the master down", func(s *state) { s.sentinelMasters["rfs-x-a"].fields["flags"] = "master,o_down" }, false},
		{"a Sentinel was not read", func(s *state) { delete(s.sentinelMasters, "rfs-x-c") }, false},
		{"not Healthy", func(s *state) {
			s.rf.Status = redisfailoverv1.RedisFailoverStatus{State: redisfailoverv1.NotHealthyState, Message: "no master"}
		}, false},
	})
}

// The window of the mutation holds until the instance recovered, and it
// reads the Sentinels for that.
func TestNoMasterFetchesTheSentinels(t *testing.T) {
	m := &Mutator{}
	p := m.planNoMaster(authState(), "rfr-x-0", sentinelPort)
	if !p.fetch.sentinelMaster || p.converged == nil {
		t.Errorf("plan %+v", p)
	}
}

// A forced delete has a grace period of 0, a graceful one has none, and both
// hold the UID of the pod. A pod plan, as in kill_master_force, passes both
// on.
func TestDeletePod(t *testing.T) {
	ctx := context.Background()
	for _, force := range []bool{false, true} {
		want := "delete rfr-x-1 uid=u1"
		if force {
			want += " grace=0"
		}
		for name, del := range map[string]func(*Mutator) error{
			"deletePod": func(m *Mutator) error { return m.deletePod(ctx, "rfr-x-1", "u1", force) },
			"pod plan": func(m *Mutator) error {
				p := plan{kind: config.KillMasterForce, pod: "rfr-x-1", uid: "u1", force: force}
				return m.apply(ctx, p, slog.New(slog.DiscardHandler))
			},
		} {
			rec := &calls{}
			m, _, _ := noMasterEnv(t, rec, nil)
			if err := del(m); err != nil {
				t.Fatal(err)
			}
			if got := rec.get(); !slices.Equal(got, []string{want}) {
				t.Errorf("%s, force %v: calls %v, want %s", name, force, got, want)
			}
		}
	}
}

func TestEpochChange(t *testing.T) {
	type epochs = map[string]int64
	cases := []struct {
		name          string
		before, after epochs
		want          string
	}{
		{"every epoch rose", epochs{"a": 1, "b": 1, "c": 1}, epochs{"a": 2, "b": 2, "c": 2}, epochRose},
		{"one epoch rose", epochs{"a": 0, "b": 0, "c": 0}, epochs{"a": 1, "b": 0, "c": 0}, epochRose},
		{"every epoch fell to 0", epochs{"a": 3, "b": 3, "c": 3}, epochs{"a": 0, "b": 0, "c": 0}, epochFell},
		{"one epoch fell", epochs{"a": 3, "b": 3, "c": 3}, epochs{"a": 0, "b": 3, "c": 3}, epochFell},
		{"no epoch changed", epochs{"a": 3, "b": 3, "c": 3}, epochs{"a": 3, "b": 3, "c": 3}, epochUnchanged},
		{"no epoch changed from 0", epochs{"a": 0, "b": 0, "c": 0}, epochs{"a": 0, "b": 0, "c": 0}, epochUnchanged},
		{"one rose and one fell", epochs{"a": 3, "b": 3, "c": 3}, epochs{"a": 4, "b": 0, "c": 3}, epochMixed},
		{"the epochs before were not read", nil, epochs{"a": 1, "b": 1, "c": 1}, epochUnread},
		{"the epochs after were not read", epochs{"a": 1, "b": 1, "c": 1}, nil, epochUnread},
		{"fewer Sentinels after", epochs{"a": 1, "b": 1, "c": 1}, epochs{"a": 2, "b": 2}, epochUnread},
		{"another Sentinel after", epochs{"a": 1, "b": 1}, epochs{"a": 2, "c": 2}, epochUnread},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := epochChange(c.before, c.after); got != c.want {
				t.Errorf("change %q, want %q", got, c.want)
			}
		})
	}
}

func TestRecoveryPath(t *testing.T) {
	cases := []struct {
		elected bool
		change  string
		want    string
	}{
		{false, epochRose, pathSentinel},
		{false, epochFell, pathOperator},
		{false, epochUnchanged, pathUnknown},
		{false, epochMixed, pathUnknown},
		{false, epochUnread, pathUnknown},
		{true, epochUnchanged, pathOperator},
		{true, epochUnread, pathOperator},
		{true, epochMixed, pathOperator},
		{true, epochRose, pathOperator},
		{true, epochFell, pathOperator},
	}
	for _, c := range cases {
		if got := recoveryPath(c.elected, c.change); got != c.want {
			t.Errorf("message %v, epoch %s: path %q, want %q", c.elected, c.change, got, c.want)
		}
	}
}

// The tester matches the status text of the operator, so a change of the text
// must fail a test.
func TestOperatorElectedText(t *testing.T) {
	src, err := os.ReadFile("../../../../operator/redisfailover/checker.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), `"`+operatorElected+`%s"`) {
		t.Errorf("checker.go has no status message %q followed by the pod name", operatorElected)
	}
}

// setEpochs sets the config-epoch of all fake Sentinels.
func setEpochs(rec *calls, epoch string) {
	for _, addr := range []string{"127.0.0.1", "127.0.0.2", "127.0.0.3"} {
		rec.setEpoch(addr, epoch)
	}
}

// The mutation reads the epochs before the reset, so that the reset cannot
// change what it compares with.
func TestNoMasterReadsTheEpochsFirst(t *testing.T) {
	rec := &calls{}
	m, s, port := noMasterEnv(t, rec, nil)
	setEpochs(rec, "5")
	if err := m.apply(context.Background(), m.planNoMaster(s, "rfr-x-0", port), slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	got := rec.get()
	if len(got) != 7 || got[6] != "delete rfr-x-0 uid=u0" {
		t.Fatalf("calls %v, want 3 epoch reads, 3 resets and the delete", got)
	}
	for i, e := range got[:6] {
		if want := []string{"epoch", "reset"}[i/3]; !strings.HasPrefix(e, want) {
			t.Errorf("call %d is %q, want %s", i, e, want)
		}
	}
}

// setStatus sets the status message of the RedisFailover name in ns, which the
// fake client creates if it does not exist.
func setStatus(t *testing.T, m *Mutator, name, message string) {
	t.Helper()
	rfs := m.rfs.DatabasesV1().RedisFailovers("ns")
	rf, err := rfs.Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		rf = &redisfailoverv1.RedisFailover{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"}}
		rf.Status.Message = message
		_, err = rfs.Create(context.Background(), rf, metav1.CreateOptions{})
	} else {
		rf.Status.Message = message
		_, err = rfs.Update(context.Background(), rf, metav1.UpdateOptions{})
	}
	if err != nil {
		t.Fatal(err)
	}
}

const electionMessage = operatorElected + "rfr-x-1"

func TestNoMasterRecovery(t *testing.T) {
	cases := []struct {
		name   string
		before string
		// after changes the fake Sentinels, and the status of the
		// RedisFailover, once the mutation is applied.
		after  func(*testing.T, *Mutator, *calls)
		want   string
		change string
		sawMsg bool
	}{
		{"Sentinel failover", "3", func(_ *testing.T, _ *Mutator, r *calls) { setEpochs(r, "4") }, pathSentinel, epochRose, false},
		{"Sentinel failover seen on one Sentinel", "3", func(_ *testing.T, _ *Mutator, r *calls) { r.setEpoch("127.0.0.2", "4") }, pathSentinel, epochRose, false},
		{"operator election with the epoch reset", "3", func(_ *testing.T, _ *Mutator, r *calls) { setEpochs(r, "0") }, pathOperator, epochFell, false},
		{"no change", "3", func(*testing.T, *Mutator, *calls) {}, pathUnknown, epochUnchanged, false},
		{"a Sentinel does not answer after", "3", func(_ *testing.T, _ *Mutator, r *calls) { delete(r.epochs, "127.0.0.3") }, pathUnknown, epochUnread, false},
		{"a Sentinel answers no epoch after", "3", func(_ *testing.T, _ *Mutator, r *calls) { r.setEpoch("127.0.0.3", "") }, pathUnknown, epochUnread, false},
		{"operator election from epoch 0", "0", func(t *testing.T, m *Mutator, _ *calls) { setStatus(t, m, "x", electionMessage) }, pathOperator, epochUnchanged, true},
		{"the election message is replaced", "0", func(t *testing.T, m *Mutator, _ *calls) {
			setStatus(t, m, "x", "no master, waiting for the Sentinel failover")
			setStatus(t, m, "x", electionMessage)
			setStatus(t, m, "x", "")
		}, pathOperator, epochUnchanged, true},
		{"the election message and a rise", "3", func(t *testing.T, m *Mutator, r *calls) {
			setStatus(t, m, "x", electionMessage)
			setEpochs(r, "4")
		}, pathOperator, epochRose, true},
		{"the election message and no epoch read", "3", func(t *testing.T, m *Mutator, r *calls) {
			setStatus(t, m, "x", electionMessage)
			delete(r.epochs, "127.0.0.1")
		}, pathOperator, epochUnread, true},
		{"another message and a rise", "3", func(t *testing.T, m *Mutator, r *calls) {
			setStatus(t, m, "x", "no master, waiting for the Sentinel failover")
			setEpochs(r, "4")
		}, pathSentinel, epochRose, false},
		{"another message and no change", "0", func(t *testing.T, m *Mutator, _ *calls) {
			setStatus(t, m, "x", "no master, waiting for the Sentinel failover")
		}, pathUnknown, epochUnchanged, false},
		{"the message of another RedisFailover", "0", func(t *testing.T, m *Mutator, _ *calls) {
			setStatus(t, m, "other", electionMessage)
		}, pathUnknown, epochUnchanged, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := &calls{}
			m, s, port := noMasterEnv(t, rec, nil)
			setEpochs(rec, c.before)
			p := m.planNoMaster(s, "rfr-x-0", port)
			if err := m.apply(context.Background(), p, slog.New(slog.DiscardHandler)); err != nil {
				t.Fatal(err)
			}
			c.after(t, m, rec)
			path, attrs := p.recovery(context.Background())
			if path != c.want {
				t.Errorf("path %q, want %q", path, c.want)
			}
			if want := []any{"config_epoch", c.change, "operator_message", c.sawMsg}; !slices.Equal(attrs, want) {
				t.Errorf("attributes %v, want %v", attrs, want)
			}
		})
	}
}

// A watch that does not start does not fail the mutation, and the epochs
// alone decide the path.
func TestNoMasterWatchFails(t *testing.T) {
	for epoch, want := range map[string]string{"4": pathSentinel, "3": pathUnknown} {
		rec := &calls{}
		m, s, port := noMasterEnv(t, rec, nil)
		m.rfs.(*rffake.Clientset).PrependWatchReactor("redisfailovers", func(k8stesting.Action) (bool, watch.Interface, error) {
			return true, nil, errors.New("forbidden")
		})
		setEpochs(rec, "3")
		p := m.planNoMaster(s, "rfr-x-0", port)
		if err := m.apply(context.Background(), p, slog.New(slog.DiscardHandler)); err != nil {
			t.Fatal(err)
		}
		setEpochs(rec, epoch)
		if path, _ := p.recovery(context.Background()); path != want {
			t.Errorf("epoch %s: path %q, want %q", epoch, path, want)
		}
	}
}

// A watch that ended keeps the messages that it delivered.
func TestNoMasterWatchEnds(t *testing.T) {
	rec := &calls{}
	m, s, port := noMasterEnv(t, rec, nil)
	w := watch.NewRaceFreeFake()
	m.rfs.(*rffake.Clientset).PrependWatchReactor("redisfailovers", func(k8stesting.Action) (bool, watch.Interface, error) {
		return true, w, nil
	})
	setEpochs(rec, "0")
	p := m.planNoMaster(s, "rfr-x-0", port)
	if err := m.apply(context.Background(), p, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	rf := &redisfailoverv1.RedisFailover{ObjectMeta: metav1.ObjectMeta{Name: "x"}}
	rf.Status.Message = electionMessage
	w.Modify(rf)
	w.Stop()
	if path, _ := p.recovery(context.Background()); path != pathOperator {
		t.Errorf("path %q, want %q", path, pathOperator)
	}
}

// A Sentinel that does not answer before the reset does not fail the
// mutation. The path is then unknown, even if all Sentinels answer later.
func TestNoMasterEpochNotReadBefore(t *testing.T) {
	for name, epoch := range map[string]string{"error": "", "not a number": "x"} {
		t.Run(name, func(t *testing.T) {
			rec := &calls{}
			m, s, port := noMasterEnv(t, rec, nil)
			rec.setEpoch("127.0.0.1", "3")
			rec.setEpoch("127.0.0.2", "3")
			if epoch != "" {
				rec.setEpoch("127.0.0.3", epoch)
			}
			p := m.planNoMaster(s, "rfr-x-0", port)
			if err := m.apply(context.Background(), p, slog.New(slog.DiscardHandler)); err != nil {
				t.Fatalf("apply: %v", err)
			}
			if got := rec.get(); got[len(got)-1] != "delete rfr-x-0 uid=u0" {
				t.Errorf("calls %v, want the delete as the last call", got)
			}
			setEpochs(rec, "4")
			if got, _ := p.recovery(context.Background()); got != pathUnknown {
				t.Errorf("path %q, want %q", got, pathUnknown)
			}
		})
	}
}

// A Sentinel without an address cannot be read, as it cannot be reset.
func TestConfigEpochsNoIP(t *testing.T) {
	rec := &calls{}
	m, s, port := noMasterEnv(t, rec, nil)
	setEpochs(rec, "3")
	s.sentinels[0].Status.PodIP = ""
	if got := m.configEpochs(context.Background(), s.sentinels, port); got != nil {
		t.Errorf("epochs %v, want none", got)
	}
}

func TestRecordRecovery(t *testing.T) {
	labels := prometheus.Labels{"rf": "x", "namespace": "ns", "mode": "sentinel"}
	cases := []struct {
		name     string
		recovery func(context.Context) (string, []any)
		result   string
		counted  string
	}{
		{"converged", func(context.Context) (string, []any) { return pathOperator, []any{"why", "x"} }, resultConverged, pathOperator},
		{"timeout", func(context.Context) (string, []any) {
			t.Error("classified a mutation that did not converge")
			return pathUnknown, nil
		}, resultTimeout, ""},
		{"no classification", nil, resultConverged, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := &Mutator{recoveries: metrics.New(prometheus.NewRegistry(), time.Minute).NoMasterRecovery.MustCurryWith(labels)}
			var buf bytes.Buffer
			log := slog.New(slog.NewJSONHandler(&buf, nil))
			m.recordRecovery(context.Background(), plan{recovery: c.recovery}, c.result, log).Info("done")
			for _, path := range recoveryPaths {
				want := 0.0
				if path == c.counted {
					want = 1
				}
				if got := testutil.ToFloat64(m.recoveries.WithLabelValues(path)); got != want {
					t.Errorf("path %s counted %v, want %v", path, got, want)
				}
			}
			if got := strings.Contains(buf.String(), `"recovery_path":"`+c.counted+`"`); got != (c.counted != "") {
				t.Errorf("log %s, recovery_path logged: %v", buf.String(), got)
			}
			if got := strings.Contains(buf.String(), `"why":"x"`); got != (c.counted != "") {
				t.Errorf("log %s, attributes logged: %v", buf.String(), got)
			}
		})
	}
}

// Rates need a series that starts at 0, so New creates every path of an
// instance that runs the kind, and none for another instance.
func TestNewCreatesRecoverySeries(t *testing.T) {
	cfg, err := config.Parse([]byte(versionsConfig))
	if err != nil {
		t.Fatal(err)
	}
	in := cfg.Instances[slices.IndexFunc(cfg.Instances, func(in config.Instance) bool { return in.Name == "mixed" })]
	reg := prometheus.NewRegistry()
	mt := metrics.New(reg, time.Minute)
	log := slog.New(slog.DiscardHandler)
	New(in, cfg, nil, nil, nil, nil, nil, nil, nil, mt, log)
	if n := testutil.CollectAndCount(mt.NoMasterRecovery); n != 0 {
		t.Errorf("%d series for an instance without the kind", n)
	}
	in.Mutations.Kinds = map[config.Kind]int{config.SentinelResetKillMaster: 1}
	m := New(in, cfg, nil, nil, nil, nil, nil, nil, nil, mt, log)
	if n := testutil.CollectAndCount(mt.NoMasterRecovery); n != len(recoveryPaths) {
		t.Errorf("%d series, want %d", n, len(recoveryPaths))
	}
	for _, path := range recoveryPaths {
		if got := testutil.ToFloat64(m.recoveries.WithLabelValues(path)); got != 0 {
			t.Errorf("path %s starts at %v", path, got)
		}
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() == "redis_soak_no_master_recovery_total" {
			return
		}
	}
	t.Error("redis_soak_no_master_recovery_total is not registered")
}
