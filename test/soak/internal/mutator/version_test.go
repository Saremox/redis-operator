package mutator

import (
	"errors"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"

	"github.com/saremox/redis-operator/test/soak/internal/config"
	"github.com/saremox/redis-operator/test/soak/internal/instances"
	"github.com/saremox/redis-operator/test/soak/internal/metrics"
)

const versionsConfig = `
versions:
  - {name: redis-6.2, image: "redis:6.2.24-alpine"}
  - {name: redis-7.2, image: "redis:7.2.16-alpine"}
  - {name: redis-7.4, image: "redis:7.4.11-alpine"}
  - {name: redis-8, image: "redis:8.10.2-alpine"}
  - {name: valkey-7.2, image: "valkey/valkey:7.2.14-alpine"}
  - {name: valkey-8, image: "valkey/valkey:8.1.10-alpine"}
  - {name: valkey-9, image: "valkey/valkey:9.1.2-alpine"}
edges:
  - {from: redis-6.2, to: redis-7.2, expect: ok}
  - {from: redis-7.2, to: redis-7.4, expect: ok}
  - {from: redis-7.4, to: redis-8, expect: ok}
  - {from: valkey-7.2, to: valkey-8, expect: ok}
  - {from: valkey-8, to: valkey-9, expect: ok}
  - {from: redis-7.2, to: valkey-7.2, expect: ok}
  - {from: redis-7.2, to: valkey-8, expect: ok}
  - {from: redis-7.2, to: valkey-9, expect: ok}
  - {from: redis-7.4, to: valkey-8, expect: unknown}
  - {from: redis-8, to: valkey-8, expect: unknown}
instances:
  - name: chain
    namespace: ns
    mode: sentinel
    template: ../instances/testdata/chain.yaml
    chain: {start: [redis-7.2], versions: [redis-7.2, redis-7.4, redis-8], sentinel: follow}
  - name: migrate
    namespace: ns
    mode: sentinel
    template: ../instances/testdata/chain.yaml
    chain: {start: [redis-7.2], versions: [redis-7.2, valkey-7.2, valkey-8, valkey-9], sentinel: separate}
  - name: edge
    namespace: ns
    template: ../instances/testdata/chain.yaml
    chain: {start: [redis-7.4, redis-8], versions: [redis-7.4, redis-8, valkey-8], expect: [unknown]}
  - name: mixed
    namespace: ns
    mode: sentinel
    mutations:
      kinds: {sentinel_image_flip: 1}
      sentinelImages: [valkey-9, redis-7.2, valkey-8]
`

// versionMutator returns the mutator of a configured instance, as far as
// planning a version change needs one.
func versionMutator(t *testing.T, name string) *Mutator {
	t.Helper()
	cfg, err := config.Parse([]byte(versionsConfig))
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(cfg.Instances, func(in config.Instance) bool { return in.Name == name })
	in := cfg.Instances[i]
	m := &Mutator{in: in, versions: cfg}
	if in.Template != "" {
		if m.instance, err = instances.New(in, nil, nil, slog.New(slog.DiscardHandler)); err != nil {
			t.Fatal(err)
		}
	}
	return m
}

// onImages is testState with the redis and Sentinel images, and every pod
// on them.
func onImages(redis, sentinel string) state {
	s := testState()
	s.rf.Spec.Redis.Image, s.rf.Spec.Sentinel.Image = redis, sentinel
	s.rf.UID = "uid"
	for i := range s.redis {
		s.redis[i].Spec.Containers[0].Image = redis
	}
	for i := range s.sentinels {
		s.sentinels[i].Spec.Containers = []corev1.Container{{Name: sentinelName, Image: sentinel}}
	}
	return s
}

func TestPickEdge(t *testing.T) {
	m := versionMutator(t, "migrate")
	s := onImages("redis:7.2.16-alpine", "redis:7.2.16-alpine")
	picked := map[string]int{}
	for step := 1; step <= 300; step++ {
		p := m.planImage(stepRand(5, m.in, step), s)
		if again := m.planImage(stepRand(5, m.in, step), s); again.params != p.params {
			t.Fatalf("step %d picked %q, then %q", step, p.params, again.params)
		}
		if p.edge == nil || p.edge.from.Name != "redis-7.2" {
			t.Fatalf("plan %+v", p)
		}
		picked[p.edge.edge.String()]++
	}
	// The three migrations, never redis-7.4: not one of migrate's versions.
	if len(picked) != 3 || picked["redis-7.2 -> redis-7.4"] > 0 {
		t.Errorf("picked %v", picked)
	}
	for e, n := range picked {
		if n < 70 {
			t.Errorf("%s picked %d of 300 times", e, n)
		}
	}
}

func TestPlanImage(t *testing.T) {
	m := versionMutator(t, "chain")
	p := m.planImage(stepRand(1, m.in, 1), onImages("redis:7.2.16-alpine", "redis:7.2.16-alpine"))
	if want := `{"spec":{"redis":{"image":"redis:7.4.11-alpine"},"sentinel":{"image":"redis:7.4.11-alpine"}}}`; string(p.patch) != want {
		t.Errorf("patch %s, want %s", p.patch, want)
	}
	if p.kind != config.ImageUpgrade || p.params != "redis.image redis-7.2 -> redis-7.4 (ok), Sentinels too" || !p.fetch.servers {
		t.Errorf("plan %+v", p)
	}

	// The end of the chain resets, on the start.
	p = m.planImage(stepRand(1, m.in, 2), onImages("redis:8.10.2-alpine", "redis:8.10.2-alpine"))
	if p.kind != config.Reset || !p.reset || p.action == nil || p.params != "recreate on redis-7.2, Sentinels on redis-7.2 (the chain ends at redis-8)" {
		t.Errorf("plan at the end %+v", p)
	}

	if p := m.planImage(stepRand(1, m.in, 3), onImages("redis:6-alpine", "")); p.skip == "" {
		t.Error("an image that is no configured version isn't skipped")
	}
}

// Separate Sentinels follow the data image along the chain, and the chain
// resets only once they caught up; neither kind is skipped while the other
// can move on.
func TestPlanSentinelImage(t *testing.T) {
	m := versionMutator(t, "migrate")
	if p := m.planSentinelImage(stepRand(1, m.in, 1), onImages("redis:7.2.16-alpine", "redis:7.2.16-alpine")); p.kind != config.ImageUpgrade || p.edge == nil || p.edge.sentinel {
		t.Errorf("Sentinels on the data's version: %+v", p)
	}
	// From redis-7.2 only valkey-7.2 and valkey-8 lead to valkey-8.
	s := onImages("valkey/valkey:8.1.10-alpine", "redis:7.2.16-alpine")
	picked := map[string]bool{}
	for step := 1; step <= 50; step++ {
		p := m.planSentinelImage(stepRand(1, m.in, step), s)
		if p.edge == nil || !p.edge.sentinel || !strings.HasPrefix(string(p.patch), `{"spec":{"sentinel":{"image":`) {
			t.Fatalf("plan %+v", p)
		}
		picked[p.edge.edge.To] = true
	}
	if len(picked) != 2 || !picked["valkey-7.2"] || !picked["valkey-8"] {
		t.Errorf("picked %v", picked)
	}

	end := onImages("valkey/valkey:9.1.2-alpine", "valkey/valkey:8.1.10-alpine")
	if p := m.planImage(stepRand(1, m.in, 1), end); p.kind != config.SentinelImageUpgrade || p.edge == nil || p.edge.edge.To != "valkey-9" {
		t.Errorf("at the end before the Sentinels caught up: %+v", p)
	}
	if p := m.planSentinelImage(stepRand(1, m.in, 1), end); p.edge == nil || p.edge.edge.To != "valkey-9" {
		t.Errorf("plan %+v", p)
	}
	end = onImages("valkey/valkey:9.1.2-alpine", "valkey/valkey:9.1.2-alpine")
	if p := m.planImage(stepRand(1, m.in, 1), end); p.kind != config.Reset {
		t.Errorf("plan at the end %+v", p)
	}
}

// edge's resets take its starts in turn, and only unknown edges.
func TestResetStarts(t *testing.T) {
	m := versionMutator(t, "edge")
	var starts []string
	for range 4 {
		p := m.planReset(onImages("valkey/valkey:8.1.10-alpine", ""), "test")
		starts = append(starts, strings.Fields(p.params)[2])
		m.resets++
	}
	if !slices.Equal(starts, []string{"redis-8", "redis-7.4", "redis-8", "redis-7.4"}) {
		t.Errorf("starts %v", starts)
	}
	p := m.planImage(stepRand(1, m.in, 1), onImages("redis:7.4.11-alpine", ""))
	if p.edge == nil || p.edge.edge.String() != "redis-7.4 -> valkey-8" {
		t.Errorf("plan %+v", p)
	}
}

func TestConvergedOn(t *testing.T) {
	m := versionMutator(t, "chain")
	const image = "redis:7.4.11-alpine"
	s := onImages(image, image)
	s.rf.Status.State = "Healthy"
	s.rf.Spec.Sentinel.Enabled = new(bool)
	*s.rf.Spec.Sentinel.Enabled = true
	s.servers = map[string]server{}
	for _, p := range append(slices.Clone(s.redis), s.sentinels...) {
		s.servers[p.Name] = server{name: "redis", version: "7.4.11"}
	}
	converged := convergedOn(m.versions, image, image)
	if err := converged(s); err != nil {
		t.Fatal(err)
	}
	s.servers["rfr-x-1"] = server{name: "redis", version: "7.2.16"}
	if err := converged(s); err == nil || !strings.Contains(err.Error(), "rfr-x-1 reports redis 7.2.16") {
		t.Errorf("a pod on the old version: %v", err)
	}
	s.servers["rfr-x-1"] = server{name: "redis", version: "7.4.11"}
	s.sentinels[2].Spec.Containers[0].Image = "redis:7.2.16-alpine"
	if err := converged(s); err == nil || !strings.Contains(err.Error(), "rfs-x-c runs redis:7.2.16-alpine") {
		t.Errorf("a Sentinel on the old image: %v", err)
	}
	if err := convergedOn(m.versions, image, "")(s); err != nil {
		t.Errorf("the Sentinel image isn't changed: %v", err)
	}
}

func TestMixedWindow(t *testing.T) {
	from := config.Version{Name: "redis-7.2", Image: "redis:7.2.16-alpine", Server: "redis", Release: "7.2.16"}
	to := config.Version{Name: "valkey-8", Image: "valkey/valkey:8.1.10-alpine", Server: "valkey", Release: "8.1.10"}
	old := podImage{from.Image, "redis", "7.2.16"}
	starting := podImage{to.Image, "", ""}
	running := podImage{to.Image, "valkey", "8.1.10"}
	at := func(s int) time.Time { return time.Unix(1000, 0).Add(time.Duration(s) * time.Second) }
	w := &mixedWindow{t: &transition{from: from, to: to}}
	for _, step := range []struct {
		at   int
		pods []podImage
	}{
		{0, []podImage{old, old, old}},
		// A replica on the new image that doesn't answer yet.
		{5, []podImage{old, old, starting}},
		{10, []podImage{old, old, running}},
		{60, []podImage{old, running, running}},
		// The old master's pod is terminating.
		{90, []podImage{running, running, {from.Image, "", ""}}},
	} {
		if _, ended := w.observe(at(step.at), step.pods); ended {
			t.Fatalf("ended at %d", step.at)
		}
		if step.at == 5 && !w.start.IsZero() {
			t.Error("started before a pod answered on the new version")
		}
	}
	if !w.start.Equal(at(10)) {
		t.Errorf("started at %s", w.start)
	}
	// It's gone, and its replacement not up yet.
	if d, ended := w.observe(at(100), []podImage{running, running, starting}); !ended || d != 90*time.Second {
		t.Errorf("ended %t after %s", ended, d)
	}

	// A change that never got a pod running ended no window; one that got
	// stuck ends when the instance is deleted.
	w = &mixedWindow{t: &transition{from: from, to: to}}
	if _, ok := w.end(at(10)); ok {
		t.Error("never mixed, but ended")
	}
	w.observe(at(20), []podImage{old, running})
	if d, ok := w.end(at(320)); !ok || d != 5*time.Minute {
		t.Errorf("deleted: %t after %s", ok, d)
	}
}

func TestClassify(t *testing.T) {
	tr := &transition{edge: config.Edge{From: "redis-8", To: "valkey-8", Expect: config.ExpectUnknown},
		from: config.Version{Name: "redis-8"}, to: config.Version{Name: "valkey-8"}}
	stuck := observation{
		verified: true,
		master:   "rfr-x-0",
		masterOn: "redis-8",
		pods:     []string{"rfr-x-1 on valkey-8 ready=true restarts=0 link=down log: Can't handle RDB format version 12"},
	}
	cases := []struct {
		name    string
		t       *transition
		o       func(o observation) observation
		result  string
		reasons string
	}{
		{"converged", tr, func(o observation) observation { return observation{converged: true, verified: true} }, transitionOK, ""},
		// An unknown edge that didn't converge within its bound, or got stuck.
		{"stuck, stopped safely", tr, func(o observation) observation { return o }, transitionFailedSafe, "RDB format version 12"},
		{"lost writes", tr, func(o observation) observation { o.lost = 3; return o }, transitionFailedUnsafe, "3 acknowledged writes lost"},
		{"not verified", tr, func(o observation) observation { o.verified = false; return o }, transitionFailedUnsafe, "couldn't be verified"},
		{"no master", tr, func(o observation) observation { o.master = ""; return o }, transitionFailedUnsafe, "no single master"},
		{"read-only master", tr, func(o observation) observation { o.writable = errors.New("READONLY"); return o }, transitionFailedUnsafe, "refuses writes"},
		{"promoted the new version", tr, func(o observation) observation { o.masterOn = "valkey-8"; return o }, transitionFailedUnsafe, "runs valkey-8, not redis-8"},
		{"promoted an empty pod", tr, func(o observation) observation {
			o.loadError = "Can't handle RDB format version 12"
			return o
		}, transitionFailedUnsafe, "couldn't load the data"},
		// A stuck Sentinel change leaves the redis master where it is.
		{"Sentinels", &transition{edge: tr.edge, from: tr.from, to: tr.to, sentinel: true}, func(o observation) observation {
			o.masterOn = "valkey-9"
			return o
		}, transitionFailedSafe, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			result, reasons := classify(c.t, c.o(stuck))
			if result != c.result || !strings.Contains(strings.Join(reasons, "; "), c.reasons) {
				t.Errorf("classify = %s %v, want %s with %q", result, reasons, c.result, c.reasons)
			}
		})
	}
}

func TestLoadErrorLine(t *testing.T) {
	// Captured from a valkey/valkey:8.1.10-alpine replica of redis:7.4.11.
	log := `7:S 01 Oct 2026 21:49:55.530 * PRIMARY <-> REPLICA sync: receiving streamed RDB from primary with EOF to disk
7:S 01 Oct 2026 21:49:55.532 # Can't handle RDB format version 12
7:S 01 Oct 2026 21:49:55.532 # Failed trying to load the PRIMARY synchronization DB from disk, check server logs.
`
	if got := loadErrorLine(log); got != "Can't handle RDB format version 12" {
		t.Errorf("loadErrorLine = %q", got)
	}
	if got := loadErrorLine("1:M * Ready to accept connections tcp\n"); got != "" {
		t.Errorf("loadErrorLine = %q", got)
	}
}

func TestDescribePod(t *testing.T) {
	m := versionMutator(t, "edge")
	p := readyPod("rfr-x-1", "u1", roleReplica)
	p.Spec.Containers = []corev1.Container{{Name: redisName, Image: "valkey/valkey:8.1.10-alpine"}}
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: redisName, RestartCount: 2,
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}}}
	got := describePod(m.versions, &p, redisName, server{fields: map[string]string{"master_link_status": "down"}}, "Can't handle RDB format version 12")
	if want := "rfr-x-1 on valkey-8 ready=true restarts=2 waiting=CrashLoopBackOff link=down log: Can't handle RDB format version 12"; got != want {
		t.Errorf("describePod =\n %q\nwant\n %q", got, want)
	}
}

// sentinel_image_flip changes only the Sentinel image, to another of
// sentinelImages, and measures the Sentinels' mixed window.
func TestPlanSentinelFlip(t *testing.T) {
	m := versionMutator(t, "mixed")
	s := onImages("redis:7.2.16-alpine", "valkey/valkey:9.1.2-alpine")
	s.rf.Spec.Sentinel.Enabled = new(true)
	picked := map[string]int{}
	for step := 1; step <= 100; step++ {
		p := m.planSentinelFlip(stepRand(3, m.in, step), s)
		if again := m.planSentinelFlip(stepRand(3, m.in, step), s); again.params != p.params {
			t.Fatalf("step %d picked %q, then %q", step, p.params, again.params)
		}
		if p.kind != config.SentinelImageFlip || p.edge != nil || p.flip == nil || !p.flip.sentinel ||
			p.flip.from.Name != "valkey-9" || !p.fetch.servers {
			t.Fatalf("plan %+v", p)
		}
		want := `{"spec":{"sentinel":{"image":"` + p.flip.to.Image + `"}}}`
		if string(p.patch) != want {
			t.Fatalf("patch %s, want %s", p.patch, want)
		}
		picked[p.flip.to.Name]++
	}
	if len(picked) != 2 || picked["redis-7.2"] < 30 || picked["valkey-8"] < 30 {
		t.Errorf("picked %v", picked)
	}

	p := m.planSentinelFlip(stepRand(3, m.in, 1), s)
	to := onImages("redis:7.2.16-alpine", p.flip.to.Image)
	to.rf.Spec.Sentinel.Enabled = new(true)
	to.rf.Status.State = "Healthy"
	to.servers = map[string]server{}
	for _, pod := range to.redis {
		to.servers[pod.Name] = server{name: "redis", version: "7.2.16"}
	}
	for _, pod := range to.sentinels {
		to.servers[pod.Name] = server{name: p.flip.to.Server, version: p.flip.to.Release}
	}
	if err := p.converged(to); err != nil {
		t.Errorf("Sentinels on the new image: %v", err)
	}
	if err := p.converged(s); err == nil {
		t.Error("converged with the Sentinels on the old image")
	}

	unknown := onImages("redis:7.2.16-alpine", "redis:6.2-alpine")
	unknown.rf.Spec.Sentinel.Enabled = new(true)
	if p := m.planSentinelFlip(stepRand(3, m.in, 1), unknown); p.flip == nil || p.flip.from.Name != "unknown" || p.flip.from.Image != "redis:6.2-alpine" {
		t.Errorf("from an unconfigured image: %+v", p)
	}
	s.rf.Spec.Sentinel.Enabled = new(false)
	if p := m.planSentinelFlip(stepRand(3, m.in, 1), s); p.skip == "" {
		t.Error("not skipped with Sentinel off")
	}
}

// version_mixed_seconds tells the redis pods' mixed window from the
// Sentinels'.
func TestMixedComponent(t *testing.T) {
	reg := prometheus.NewRegistry()
	mm := metrics.New(reg, time.Minute)
	m := versionMutator(t, "mixed")
	m.log = slog.New(slog.DiscardHandler)
	m.mixedSeconds = mm.VersionMixed.MustCurryWith(prometheus.Labels{"rf": "mixed", "namespace": "ns", "mode": "sentinel"})
	redis72, _ := m.versions.VersionNamed("redis-7.2")
	valkey9, _ := m.versions.VersionNamed("valkey-9")
	for _, sentinel := range []bool{false, true, true} {
		m.mixed = &mixedWindow{t: &transition{edge: config.Edge{From: "redis-7.2", To: "valkey-9"}, from: redis72, to: valkey9, sentinel: sentinel}}
		m.recordMixed(30 * time.Second)
	}
	if m.mixed != nil {
		t.Error("the window is still open")
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]uint64{}
	for _, f := range families {
		if f.GetName() != "redis_soak_version_mixed_seconds" {
			continue
		}
		for _, metric := range f.GetMetric() {
			for _, l := range metric.GetLabel() {
				if l.GetName() == "component" {
					got[l.GetValue()] = metric.GetHistogram().GetSampleCount()
				}
			}
		}
	}
	if want := map[string]uint64{"redis": 1, "sentinel": 2}; !maps.Equal(got, want) {
		t.Errorf("windows by component %v, want %v", got, want)
	}
}

// The Redis 6.2 chains of deploy/config.yaml walk to Redis 8, and then reset
// on Redis 6.2.
func TestChains62(t *testing.T) {
	cfg, err := config.Load("../../deploy/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name     string
		sentinel bool
	}{{"redis-chain-62", false}, {"redis-chain-62-sent", true}} {
		i := slices.IndexFunc(cfg.Instances, func(in config.Instance) bool { return in.Name == c.name })
		if i < 0 {
			t.Fatalf("%s is not in deploy/config.yaml", c.name)
		}
		in := cfg.Instances[i]
		m := &Mutator{in: in, versions: cfg}
		if m.instance, err = instances.New(in, nil, nil, slog.New(slog.DiscardHandler)); err != nil {
			t.Fatal(err)
		}
		cur, _ := cfg.VersionNamed(in.Version)
		if cur.Name != "redis-6.2" {
			t.Fatalf("%s starts on %q", c.name, in.Version)
		}
		var walked []string
		for step := 1; ; step++ {
			sentinel := ""
			if c.sentinel {
				sentinel = cur.Image
			}
			p := m.planImage(stepRand(1, in, step), onImages(cur.Image, sentinel))
			if p.kind == config.Reset {
				if !strings.HasPrefix(p.params, "recreate on redis-6.2") {
					t.Errorf("%s resets with %q", c.name, p.params)
				}
				break
			}
			if p.edge == nil || step > 10 {
				t.Fatalf("%s step %d: %+v", c.name, step, p)
			}
			walked = append(walked, p.edge.edge.String())
			cur = p.edge.to
		}
		want := []string{"redis-6.2 -> redis-7.2", "redis-7.2 -> redis-7.4", "redis-7.4 -> redis-8"}
		if !slices.Equal(walked, want) {
			t.Errorf("%s walked %v, want %v", c.name, walked, want)
		}
	}
}
