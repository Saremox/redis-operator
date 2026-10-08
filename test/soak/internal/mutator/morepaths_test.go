package mutator

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	miniserver "github.com/alicebob/miniredis/v2/server"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	rffake "github.com/saremox/redis-operator/client/k8s/clientset/versioned/fake"
	"github.com/saremox/redis-operator/test/soak/internal/auth"
	"github.com/saremox/redis-operator/test/soak/internal/config"
	"github.com/saremox/redis-operator/test/soak/internal/global"
	"github.com/saremox/redis-operator/test/soak/internal/instances"
	"github.com/saremox/redis-operator/test/soak/internal/metrics"
	"github.com/saremox/redis-operator/test/soak/internal/observer"
)

// shippedMutator returns the mutator of an instance of deploy/config.yaml, as
// far as planning a version change needs one.
func shippedMutator(t *testing.T, cfg *config.Config, name string) *Mutator {
	t.Helper()
	i := slices.IndexFunc(cfg.Instances, func(in config.Instance) bool { return in.Name == name })
	if i < 0 {
		t.Fatalf("%s is not in deploy/config.yaml", name)
	}
	in := cfg.Instances[i]
	m := &Mutator{in: in, versions: cfg}
	var err error
	if m.instance, err = instances.New(in, nil, nil, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	return m
}

// walk follows the chain of an instance from one of its starts until it
// resets, and returns the edges and the reset. The seed picks the edges.
func walk(t *testing.T, m *Mutator, cfg *config.Config, start config.Version, seed int64) (edges []string, reset string) {
	t.Helper()
	cur, sentinel := start, start
	sentinelOn := func() string {
		if m.in.Mode == config.ModeSentinel {
			return sentinel.Image
		}
		return ""
	}
	for step := 1; step <= 10; step++ {
		p := m.planImage(stepRand(seed, m.in, step), onImages(cur.Image, sentinelOn()))
		switch {
		case p.kind == config.Reset:
			return edges, p.params
		case p.edge == nil:
			t.Fatalf("%s step %d: %+v", m.in.Name, step, p)
		case p.edge.sentinel:
			sentinel = p.edge.to
		case p.kind == config.ImageUpgrade:
			cur = p.edge.to
			if m.in.Chain.Sentinel == config.SentinelFollow {
				sentinel = cur
			}
		}
		edges = append(edges, p.edge.edge.String())
	}
	t.Fatalf("%s does not reset from %s", m.in.Name, start.Name)
	return nil, ""
}

// Each new instance takes the edges of its group from each of its starts, and
// resets at the end of a walk. The walks together take every edge of the group.
func TestMorePathsChains(t *testing.T) {
	cfg, err := config.Load("../../deploy/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string][]string{
		"skip": {"redis-6.2 -> redis-7.4", "redis-6.2 -> redis-8", "redis-7.2 -> redis-8", "valkey-7.2 -> valkey-9"},
		"downgrade": {
			"redis-7.4 -> redis-7.2", "redis-8 -> redis-7.4", "redis-8 -> redis-7.2",
			"valkey-8 -> valkey-7.2", "valkey-9 -> valkey-8", "valkey-9 -> valkey-7.2",
		},
	} {
		t.Run(name, func(t *testing.T) {
			m := shippedMutator(t, cfg, name)
			taken := map[string]bool{}
			for _, s := range m.in.Chain.Start {
				v, _ := cfg.VersionNamed(s)
				for seed := int64(1); seed <= 40; seed++ {
					edges, reset := walk(t, m, cfg, v, seed)
					if len(edges) == 0 || !strings.HasPrefix(reset, "recreate on ") {
						t.Fatalf("walk from %s: %v, then %q", s, edges, reset)
					}
					if edges[0][:strings.Index(edges[0], " ")] != s {
						t.Fatalf("walk from %s starts with %s", s, edges[0])
					}
					for _, e := range edges {
						taken[e] = true
					}
				}
			}
			var got []string
			for e := range taken {
				got = append(got, e)
			}
			slices.Sort(got)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Errorf("%s took %v, want %v", name, got, want)
			}
		})
	}
}

// The resets of skip and downgrade take their starts in turn.
func TestMorePathsStarts(t *testing.T) {
	cfg, err := config.Load("../../deploy/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string][]string{
		"skip":      {"redis-7.2", "valkey-7.2", "redis-6.2", "redis-7.2"},
		"downgrade": {"redis-8", "valkey-8", "valkey-9", "redis-7.4", "redis-8"},
	} {
		m := shippedMutator(t, cfg, name)
		var got []string
		for m.resets = 0; m.resets < len(want); m.resets++ {
			redis, _ := m.resetVersions()
			got = append(got, redis.Name)
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s resets on %v, want %v", name, got, want)
		}
	}
}

// An instance of the main graph never takes a skip or a downgrade, from any
// version of its chain.
func TestMainGraphKeepsItsEdges(t *testing.T) {
	cfg, err := config.Load("../../deploy/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, in := range cfg.Instances {
		if in.Chain == nil || in.Chain.Group != "" {
			continue
		}
		m := shippedMutator(t, cfg, in.Name)
		for _, name := range in.Chain.Versions {
			v, _ := cfg.VersionNamed(name)
			for seed := int64(1); seed <= 30; seed++ {
				p := m.planImage(stepRand(seed, in, 1), onImages(v.Image, v.Image))
				if p.edge != nil && p.edge.edge.Group != "" {
					t.Errorf("%s takes %s from %s", in.Name, p.edge.edge, name)
				}
			}
		}
		checked++
	}
	if checked < 7 {
		t.Errorf("checked %d chains", checked)
	}
}

// edge-sent takes the unknown edges into Valkey as edge does. The Sentinels
// move after the data image, and the chain resets once they caught up.
func TestEdgeSentWalk(t *testing.T) {
	cfg, err := config.Load("../../deploy/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	m := shippedMutator(t, cfg, "edge-sent")
	redis74, _ := cfg.VersionNamed("redis-7.4")
	valkey8, _ := cfg.VersionNamed("valkey-8")

	p := m.planImage(stepRand(1, m.in, 1), onImages(redis74.Image, redis74.Image))
	if p.kind != config.ImageUpgrade || p.edge == nil || p.edge.sentinel || p.edge.edge.Expect != config.ExpectUnknown ||
		p.edge.edge.From != "redis-7.4" || !strings.HasPrefix(p.edge.edge.To, "valkey-") ||
		!strings.Contains(string(p.patch), `"redis":{"image":"`+p.edge.to.Image+`"}`) || strings.Contains(string(p.patch), "sentinel") {
		t.Errorf("data change %+v", p)
	}
	if !p.edge.sentinelsStay {
		t.Error("the Sentinels of edge-sent stay as they are while the data image changes")
	}

	// The data image converged: the Sentinels follow it along the same edge.
	p = m.planImage(stepRand(1, m.in, 2), onImages(valkey8.Image, redis74.Image))
	if p.kind != config.SentinelImageUpgrade || p.edge == nil || !p.edge.sentinel ||
		p.edge.edge.String() != "redis-7.4 -> valkey-8" || p.edge.sentinelsStay {
		t.Errorf("Sentinel change %+v", p)
	}
	p = m.planImage(stepRand(1, m.in, 3), onImages(valkey8.Image, valkey8.Image))
	if p.kind != config.Reset || !strings.HasPrefix(p.params, "recreate on redis-8") {
		t.Errorf("reset %+v", p)
	}
}

// redis-chain-big walks the Redis versions as redis-chain does.
func TestChainBigWalk(t *testing.T) {
	cfg, err := config.Load("../../deploy/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	m := shippedMutator(t, cfg, "redis-chain-big")
	start, _ := cfg.VersionNamed("redis-7.2")
	edges, reset := walk(t, m, cfg, start, 1)
	if want := []string{"redis-7.2 -> redis-7.4", "redis-7.4 -> redis-8"}; !slices.Equal(edges, want) || !strings.HasPrefix(reset, "recreate on redis-7.2") {
		t.Errorf("walked %v, then %q", edges, reset)
	}
}

// A downgrade is judged by the same rules as an upgrade: the master must stay
// on the version that the change leaves.
func TestClassifyDowngrade(t *testing.T) {
	tr := &transition{edge: config.Edge{From: "redis-8", To: "redis-7.4", Expect: config.ExpectUnknown, Group: "downgrade"},
		from: config.Version{Name: "redis-8"}, to: config.Version{Name: "redis-7.4"}}
	stuck := observation{verified: true, master: "rfr-x-0", masterOn: "redis-8",
		pods: []string{"rfr-x-2 on redis-7.4 ready=false restarts=3 waiting=CrashLoopBackOff log: Can't handle RDB format version 13"}}
	for name, c := range map[string]struct {
		o       func(o observation) observation
		result  string
		reasons string
	}{
		"the older replica cannot load the data": {func(o observation) observation { return o }, transitionFailedSafe, "CrashLoopBackOff"},
		"the older replica became the master": {func(o observation) observation {
			o.masterOn = "redis-7.4"
			return o
		}, transitionFailedUnsafe, "runs redis-7.4, not redis-8"},
		"the master lost writes": {func(o observation) observation { o.lost = 2; return o }, transitionFailedUnsafe, "2 acknowledged writes lost"},
		"the master refuses writes": {func(o observation) observation {
			o.writable = errors.New("READONLY")
			return o
		}, transitionFailedUnsafe, "refuses writes"},
		"the master could not load its data": {func(o observation) observation {
			o.loadError = "Can't handle RDB format version 13"
			return o
		}, transitionFailedUnsafe, "couldn't load the data"},
		"the downgrade converged": {func(observation) observation { return observation{converged: true, verified: true} }, transitionOK, ""},
	} {
		t.Run(name, func(t *testing.T) {
			result, reasons := classify(tr, c.o(stuck))
			if result != c.result || !strings.Contains(strings.Join(reasons, "; "), c.reasons) {
				t.Errorf("classify = %s %v, want %s with %q", result, reasons, c.result, c.reasons)
			}
		})
	}
}

// A change that leaves the Sentinels as they are failed unsafely if a Sentinel
// does not report the master.
func TestClassifySentinels(t *testing.T) {
	tr := &transition{edge: config.Edge{From: "redis-8", To: "valkey-8", Expect: config.ExpectUnknown},
		from: config.Version{Name: "redis-8"}, to: config.Version{Name: "valkey-8"}, sentinelsStay: true}
	o := observation{verified: true, master: "rfr-x-0", masterOn: "redis-8"}
	if result, _ := classify(tr, o); result != transitionFailedSafe {
		t.Errorf("Sentinels agree: %s", result)
	}
	o.sentinels = errors.New("rfs-x-b reports 10.0.0.9:6379")
	result, reasons := classify(tr, o)
	if joined := strings.Join(reasons, "; "); result != transitionFailedUnsafe || !strings.Contains(joined, "rfs-x-b reports 10.0.0.9:6379") {
		t.Errorf("Sentinels disagree: %s %v", result, reasons)
	}
}

func TestSentinelsAgree(t *testing.T) {
	master := "10.0.0.1:6379"
	report := func(ip string) sentinelMaster {
		return sentinelMaster{fields: map[string]string{"ip": ip, "port": "6379"}}
	}
	s := testState()
	s.sentinelMasters = map[string]sentinelMaster{
		"rfs-x-a": report("10.0.0.1"),
		"rfs-x-b": report("10.0.0.1"),
		"rfs-x-c": report("10.0.0.1"),
	}
	if err := sentinelsAgree(s, master); err != nil {
		t.Errorf("all agree: %v", err)
	}
	s.sentinelMasters["rfs-x-b"] = report("10.0.0.2")
	s.sentinelMasters["rfs-x-c"] = sentinelMaster{err: errors.New("no master known")}
	err := sentinelsAgree(s, master)
	if err == nil || !strings.Contains(err.Error(), "rfs-x-b reports 10.0.0.2:6379") || !strings.Contains(err.Error(), "rfs-x-c: no master known") ||
		strings.Contains(err.Error(), "rfs-x-a") {
		t.Errorf("two Sentinels disagree: %v", err)
	}
	if err := sentinelsAgree(state{}, master); err != nil {
		t.Errorf("no Sentinels: %v", err)
	}
	s.sentinelMasters = nil
	if err := sentinelsAgree(s, master); err == nil {
		t.Error("no Sentinel was read")
	}
}

// sentinelServer answers as the Sentinels, and reports ip and port as the
// master. It returns its port.
func sentinelServer(t *testing.T, ip string, port int) int {
	t.Helper()
	srv := miniredis.RunT(t)
	srv.Server().SetPreHook(func(c *miniserver.Peer, cmd string, args ...string) bool {
		switch {
		case strings.EqualFold(cmd, "INFO"):
			c.WriteBulk("# Server\r\nredis_version:7.4.11\r\nredis_mode:sentinel\r\n")
		case strings.EqualFold(cmd, "SENTINEL") && len(args) > 0 && strings.EqualFold(args[0], "get-master-addr-by-name"):
			c.WriteStrings([]string{ip, strconv.Itoa(port)})
		case strings.EqualFold(cmd, "SENTINEL"):
			c.WriteStrings([]string{"name", "mymaster", "ip", ip, "port", strconv.Itoa(port), "flags", "master"})
		default:
			return false
		}
		return true
	})
	sentinelPort, err := strconv.Atoi(srv.Port())
	if err != nil {
		t.Fatal(err)
	}
	return sentinelPort
}

// stuckInstance returns the mutator of a Sentinel instance with one redis pod
// that is the master, and one Sentinel pod for each element of sentinelIPs
// (its IP, "" for a pod without one). The Sentinels with an IP answer as
// sentinelServer says, and report sentinelSays as the master. Each element of
// replicaImages is a replica pod without an IP that runs that image, "" for a
// pod without a redis container. The observer knows the master.
func stuckInstance(t *testing.T, sentinelIPs []string, sentinelSays string, replicaImages ...string) *Mutator {
	t.Helper()
	srv := miniredis.RunT(t)
	srv.Server().SetPreHook(func(c *miniserver.Peer, cmd string, _ ...string) bool {
		if !strings.EqualFold(cmd, "INFO") {
			return false
		}
		c.WriteBulk("# Server\r\nredis_version:7.4.11\r\n# Replication\r\nrole:master\r\n")
		return true
	})
	port, err := strconv.Atoi(srv.Port())
	if err != nil {
		t.Fatal(err)
	}
	sentinelsAt := 0
	if slices.ContainsFunc(sentinelIPs, func(ip string) bool { return ip != "" }) {
		sentinelsAt = sentinelServer(t, sentinelSays, port)
	}
	cfg, err := config.Parse([]byte(versionsConfig))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Observer.Interval.Duration = 10 * time.Millisecond
	in := cfg.Instances[slices.IndexFunc(cfg.Instances, func(in config.Instance) bool { return in.Name == "migrate" })]
	in.Port = port
	labels := func(component string) map[string]string {
		return map[string]string{"app.kubernetes.io/part-of": "redis-failover", "app.kubernetes.io/name": in.Name, "app.kubernetes.io/component": component}
	}
	ready := []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	redis74, _ := cfg.VersionNamed("redis-7.4")
	objects := []runtime.Object{
		&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "rfr-" + in.Name, Namespace: in.Namespace}},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "rfr-migrate-0", Namespace: in.Namespace, Labels: labels("redis"), UID: "r0"},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: redisName, Image: redis74.Image}}},
			Status:     corev1.PodStatus{PodIP: "127.0.0.1", Phase: corev1.PodRunning, Conditions: ready},
		},
	}
	for i, image := range replicaImages {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "rfr-migrate-" + strconv.Itoa(i+1), Namespace: in.Namespace, Labels: labels("redis"), UID: types.UID("r" + strconv.Itoa(i+1))},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning, Conditions: ready},
		}
		if image != "" {
			pod.Spec.Containers = []corev1.Container{{Name: redisName, Image: image}}
		}
		objects = append(objects, pod)
	}
	for i, ip := range sentinelIPs {
		n := strconv.Itoa(i)
		objects = append(objects, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "rfs-migrate-" + n, Namespace: in.Namespace, Labels: labels("sentinel"), UID: types.UID("s" + n)},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: sentinelName, Image: redis74.Image}}},
			Status:     corev1.PodStatus{PodIP: ip, Phase: corev1.PodRunning, Conditions: ready},
		})
	}
	kube := fake.NewClientset(objects...)
	enabled := len(sentinelIPs) > 0
	rf := &redisfailoverv1.RedisFailover{
		ObjectMeta: metav1.ObjectMeta{Name: in.Name, Namespace: in.Namespace, UID: "uid"},
		Spec: redisfailoverv1.RedisFailoverSpec{
			Redis:    redisfailoverv1.RedisSettings{Replicas: int32(1 + len(replicaImages)), Port: int32(port), Image: redis74.Image},
			Sentinel: redisfailoverv1.SentinelSettings{Enabled: &enabled, Replicas: 3},
		},
	}
	rfs := rffake.NewSimpleClientset(rf)
	a := auth.New(func(name string) (*corev1.Secret, error) {
		return kube.CoreV1().Secrets(in.Namespace).Get(context.Background(), name, metav1.GetOptions{})
	})
	log := slog.New(slog.DiscardHandler)
	mt := metrics.New(prometheus.NewRegistry(), time.Minute)
	lock := &global.Lock{}
	o := observer.New(in, cfg, kube, rfs, a, lock, mt, log)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go o.Run(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for o.MasterAddr() == "" {
		if time.Now().After(deadline) {
			t.Fatal("the observer found no master")
		}
		time.Sleep(10 * time.Millisecond)
	}
	m := New(in, cfg, kube, rfs, o, fakeData{}, a, lock, nil, mt, log)
	if sentinelsAt != 0 {
		m.sentinelPort = sentinelsAt
	}
	return m
}

// observe reads the Sentinels of a change that did not converge, and judge
// classifies it. A Sentinel that does not report the master, or that has no
// pod IP, makes the change unsafe only if the Sentinels stay as they are.
func TestJudgeSentinels(t *testing.T) {
	redis74 := config.Version{Name: "redis-7.4"}
	for name, c := range map[string]struct {
		ips    []string
		says   string
		stay   bool
		reason string
		result string
	}{
		"Sentinels that report the master":       {[]string{"127.0.0.1", "127.0.0.1", "127.0.0.1"}, "127.0.0.1", true, "", transitionFailedSafe},
		"Sentinels that report another master":   {[]string{"127.0.0.1", "127.0.0.1", "127.0.0.1"}, "10.0.0.9", true, "reports 10.0.0.9:", transitionFailedUnsafe},
		"a Sentinel without a pod IP":            {[]string{"127.0.0.1", "", "127.0.0.1"}, "127.0.0.1", true, "rfs-migrate-1: no pod IP", transitionFailedUnsafe},
		"Sentinels that restart with the change": {[]string{"", "", ""}, "", false, "", transitionFailedSafe},
		"no Sentinels":                           {nil, "", true, "", transitionFailedSafe},
	} {
		t.Run(name, func(t *testing.T) {
			m := stuckInstance(t, c.ips, c.says)
			tr := &transition{edge: config.Edge{From: "redis-7.4", To: "valkey-8", Expect: config.ExpectUnknown},
				from: redis74, to: config.Version{Name: "valkey-8"}, sentinelsStay: c.stay}
			o := m.observe(context.Background(), tr)
			switch {
			case c.reason == "" && o.sentinels != nil:
				t.Errorf("observe found: %v", o.sentinels)
			case c.reason != "" && (o.sentinels == nil || !strings.Contains(o.sentinels.Error(), c.reason)):
				t.Errorf("observe found: %v, want %q", o.sentinels, c.reason)
			}
			if got := m.judge(context.Background(), tr, false, 0, nil, slog.New(slog.DiscardHandler)); got != c.result {
				t.Errorf("judge = %s, want %s", got, c.result)
			}
			findings := testutil.ToFloat64(m.findings.WithLabelValues(invVersionTransition))
			if want := map[string]float64{transitionFailedUnsafe: 1}[c.result]; findings != want {
				t.Errorf("findings = %v, want %v", findings, want)
			}
		})
	}
}

// A change that did not converge leaves at most one pod on the new image: the
// replica that cannot load the data. The master and the other replicas stay.
func TestJudgeReplicas(t *testing.T) {
	cfg, err := config.Parse([]byte(versionsConfig))
	if err != nil {
		t.Fatal(err)
	}
	version := func(name string) config.Version {
		v, _ := cfg.VersionNamed(name)
		return v
	}
	old, valkey, redis72 := version("redis-7.4").Image, version("valkey-8").Image, version("redis-7.2").Image
	up := func() *transition {
		return &transition{edge: config.Edge{From: "redis-7.4", To: "valkey-8", Expect: config.ExpectUnknown},
			from: version("redis-7.4"), to: version("valkey-8"), sentinelsStay: true}
	}
	down := func() *transition {
		return &transition{edge: config.Edge{From: "redis-7.4", To: "redis-7.2", Expect: config.ExpectUnknown, Group: "downgrade"},
			from: version("redis-7.4"), to: version("redis-7.2"), sentinelsStay: true}
	}
	sentinels := func() *transition {
		return &transition{edge: config.Edge{From: "valkey-8", To: "redis-7.4", Expect: config.ExpectUnknown},
			from: version("valkey-8"), to: version("redis-7.4"), sentinel: true}
	}
	local := []string{"127.0.0.1", "127.0.0.1", "127.0.0.1"}
	for name, c := range map[string]struct {
		t         func() *transition
		replicas  []string
		sentinels []string
		onNew     []string
		result    string
	}{
		"one stuck replica":                  {up, []string{valkey, old}, nil, []string{"rfr-migrate-1"}, transitionFailedSafe},
		"one stuck replica with Sentinels":   {up, []string{old, valkey}, local, []string{"rfr-migrate-2"}, transitionFailedSafe},
		"no pod on the new image":            {up, []string{old, old}, nil, nil, transitionFailedSafe},
		"two pods on the new image":          {up, []string{valkey, valkey}, nil, []string{"rfr-migrate-1", "rfr-migrate-2"}, transitionFailedUnsafe},
		"two pods with Sentinels":            {up, []string{valkey, valkey}, local, []string{"rfr-migrate-1", "rfr-migrate-2"}, transitionFailedUnsafe},
		"a pod without an image":             {up, []string{"", valkey}, nil, []string{"rfr-migrate-2"}, transitionFailedSafe},
		"a pod with another image":           {up, []string{"busybox:1", valkey}, nil, []string{"rfr-migrate-2"}, transitionFailedSafe},
		"a pod without an image and two new": {up, []string{"", valkey, valkey}, nil, []string{"rfr-migrate-2", "rfr-migrate-3"}, transitionFailedUnsafe},
		"one stuck replica of a downgrade":   {down, []string{redis72, old}, nil, []string{"rfr-migrate-1"}, transitionFailedSafe},
		"two pods of a downgrade":            {down, []string{redis72, redis72}, nil, []string{"rfr-migrate-1", "rfr-migrate-2"}, transitionFailedUnsafe},
		"a Sentinel change":                  {sentinels, []string{old, old}, local, nil, transitionFailedSafe},
	} {
		t.Run(name, func(t *testing.T) {
			m := stuckInstance(t, c.sentinels, "127.0.0.1", c.replicas...)
			tr := c.t()
			o := m.observe(context.Background(), tr)
			if !slices.Equal(o.onNew, c.onNew) {
				t.Errorf("pods on the new image: %v, want %v", o.onNew, c.onNew)
			}
			if got := m.judge(context.Background(), tr, false, 0, nil, slog.New(slog.DiscardHandler)); got != c.result {
				t.Errorf("judge = %s, want %s", got, c.result)
			}
			findings := testutil.ToFloat64(m.findings.WithLabelValues(invVersionTransition))
			if want := map[string]float64{transitionFailedUnsafe: 1}[c.result]; findings != want {
				t.Errorf("findings = %v, want %v", findings, want)
			}
		})
	}
}

func TestClassifyReplicas(t *testing.T) {
	tr := &transition{edge: config.Edge{From: "redis-8", To: "valkey-8", Expect: config.ExpectUnknown},
		from: config.Version{Name: "redis-8"}, to: config.Version{Name: "valkey-8"}}
	stuck := observation{verified: true, master: "rfr-x-0", masterOn: "redis-8", onNew: []string{"rfr-x-2"}}
	for name, c := range map[string]struct {
		o       func(o observation) observation
		result  string
		reasons string
	}{
		"one stuck replica": {func(o observation) observation { return o }, transitionFailedSafe, ""},
		"two pods on the new image": {func(o observation) observation {
			o.onNew = []string{"rfr-x-1", "rfr-x-2"}
			return o
		}, transitionFailedUnsafe, "2 pods on valkey-8: rfr-x-1, rfr-x-2"},
		"the master on the new image": {func(o observation) observation {
			o.masterOn, o.onNew = "valkey-8", []string{"rfr-x-0"}
			return o
		}, transitionFailedUnsafe, "runs valkey-8, not redis-8"},
		"a converged change": {func(observation) observation {
			return observation{converged: true, verified: true, onNew: []string{"rfr-x-0", "rfr-x-1", "rfr-x-2"}}
		}, transitionOK, ""},
	} {
		t.Run(name, func(t *testing.T) {
			result, reasons := classify(tr, c.o(stuck))
			if joined := strings.Join(reasons, "; "); result != c.result || (c.reasons != "" && !strings.Contains(joined, c.reasons)) {
				t.Errorf("classify = %s %v, want %s with %q", result, reasons, c.result, c.reasons)
			}
		})
	}
}
