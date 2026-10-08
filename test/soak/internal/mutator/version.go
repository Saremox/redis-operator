package mutator

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/saremox/redis-operator/test/soak/internal/config"
	"github.com/saremox/redis-operator/test/soak/internal/observer"
)

// Results of a version change, the values of version_transition_total's
// result label.
const (
	transitionOK           = "ok"
	transitionFailedSafe   = "failed_safe"
	transitionFailedUnsafe = "failed_unsafe"
)

// invVersionTransition is the invariant label of the finding a version
// change that failed unsafely counts.
const invVersionTransition = "version_transition"

// invResetIncomplete is the invariant label of the finding a reset counts
// when its window did not end within its bound.
const invResetIncomplete = "reset_incomplete"

const sentinelName = "sentinel"

// transition is a planned version change of the redis image, or of the
// Sentinel image.
type transition struct {
	edge     config.Edge
	from, to config.Version
	sentinel bool
	// sentinelsStay is whether the Sentinels keep their pods while the change
	// runs, so that they must still agree on the master if it gets stuck.
	sentinelsStay bool
}

func (t *transition) container() string {
	if t.sentinel {
		return sentinelName
	}
	return redisName
}

// server is what a pod reports in INFO server, and in INFO replication for
// a redis pod.
type server struct {
	name, version string
	fields        map[string]string
	err           error
}

// planImage follows an edge of the chain from the version of the redis image.
// The random source of the step picks the edge. A Sentinel image that follows
// changes with it. At the end of the chain, it moves separate Sentinels first,
// and then resets the instance.
func (m *Mutator) planImage(r *rand.Rand, s state) plan {
	const kind = config.ImageUpgrade
	ch := m.in.Chain
	cur, ok := m.versions.VersionOf(s.rf.Spec.Redis.Image)
	if !ok {
		return skipped(kind, "redis image %s is no configured version", s.rf.Spec.Redis.Image)
	}
	edges := m.versions.EdgesFrom(cur.Name, ch)
	if len(edges) == 0 {
		if sv, _ := m.versions.VersionOf(s.rf.Spec.Sentinel.Image); ch.Sentinel == config.SentinelSeparate && sv.Name != cur.Name {
			return m.planSentinelImage(r, s)
		}
		return m.planReset(s, "the chain ends at "+cur.Name)
	}
	e := edges[r.IntN(len(edges))]
	to, _ := m.versions.VersionNamed(e.To)
	spec := map[string]any{"redis": map[string]any{"image": to.Image}}
	params := fmt.Sprintf("redis.image %s -> %s (%s)", e.From, e.To, e.Expect)
	sentinelImage := ""
	if ch.Sentinel == config.SentinelFollow {
		spec["sentinel"] = map[string]any{"image": to.Image}
		params += ", Sentinels too"
		sentinelImage = to.Image
	}
	return plan{
		kind:      kind,
		params:    params,
		patch:     mergePatch(spec),
		edge:      &transition{edge: e, from: cur, to: to, sentinelsStay: ch.Sentinel != config.SentinelFollow},
		fetch:     fetchOpts{servers: true},
		converged: convergedOn(m.versions, to.Image, sentinelImage),
	}
}

// planSentinelImage follows one of the chain's edges from the Sentinel
// image's version towards the redis image's; once the Sentinels caught up
// it changes the redis image instead.
func (m *Mutator) planSentinelImage(r *rand.Rand, s state) plan {
	const kind = config.SentinelImageUpgrade
	ch := m.in.Chain
	cur, ok := m.versions.VersionOf(s.rf.Spec.Sentinel.Image)
	data, dok := m.versions.VersionOf(s.rf.Spec.Redis.Image)
	switch {
	case !ok || !dok:
		return skipped(kind, "images %s and %s aren't both configured versions", s.rf.Spec.Sentinel.Image, s.rf.Spec.Redis.Image)
	case cur.Name == data.Name:
		return m.planImage(r, s)
	}
	var edges []config.Edge
	for _, e := range m.versions.EdgesFrom(cur.Name, ch) {
		if m.versions.Reaches(ch, e.To, data.Name) {
			edges = append(edges, e)
		}
	}
	if len(edges) == 0 {
		return skipped(kind, "no edge from %s leads to %s", cur.Name, data.Name)
	}
	e := edges[r.IntN(len(edges))]
	to, _ := m.versions.VersionNamed(e.To)
	return plan{
		kind:      kind,
		params:    fmt.Sprintf("sentinel.image %s -> %s (%s)", e.From, e.To, e.Expect),
		patch:     mergePatch(map[string]any{"sentinel": map[string]any{"image": to.Image}}),
		edge:      &transition{edge: e, from: cur, to: to, sentinel: true},
		fetch:     fetchOpts{servers: true},
		converged: convergedOn(m.versions, s.rf.Spec.Redis.Image, to.Image),
	}
}

// planSentinelFlip changes only the Sentinel image to a different value in
// sentinelImages. The random source of the step picks it. It is not an edge of
// the graph, because Sentinels hold no data to load.
func (m *Mutator) planSentinelFlip(r *rand.Rand, s state) plan {
	const kind = config.SentinelImageFlip
	if !s.rf.SentinelEnabled() {
		return skipped(kind, "Sentinel is off")
	}
	cur, ok := m.versions.VersionOf(s.rf.Spec.Sentinel.Image)
	if !ok {
		cur = config.Version{Name: "unknown", Image: s.rf.Spec.Sentinel.Image}
	}
	var choices []config.Version
	for _, name := range m.in.Mutations.SentinelImages {
		if v, _ := m.versions.VersionNamed(name); v.Image != cur.Image && !slices.Contains(choices, v) {
			choices = append(choices, v)
		}
	}
	to := choices[r.IntN(len(choices))]
	return plan{
		kind:      kind,
		params:    fmt.Sprintf("sentinel.image %s -> %s", cur.Name, to.Name),
		patch:     mergePatch(map[string]any{"sentinel": map[string]any{"image": to.Image}}),
		flip:      &transition{edge: config.Edge{From: cur.Name, To: to.Name}, from: cur, to: to, sentinel: true},
		fetch:     fetchOpts{servers: true},
		converged: convergedOn(m.versions, s.rf.Spec.Redis.Image, to.Image),
	}
}

// planReset recreates the instance from its template, on the chain's next
// start version.
func (m *Mutator) planReset(s state, why string) plan {
	const kind = config.Reset
	if m.instance == nil {
		return skipped(kind, "the instance has no template")
	}
	redis, sentinel := m.resetVersions()
	params := "recreate"
	if redis.Name != "" {
		params += " on " + redis.Name
	}
	if sentinel.Name != "" {
		params += ", Sentinels on " + sentinel.Name
	}
	old := s.rf.UID
	converged := convergedOn(m.versions, redis.Image, sentinel.Image)
	return plan{
		kind:   kind,
		params: params + " (" + why + ")",
		action: func(ctx context.Context) error { return m.reset(ctx, redis.Image, sentinel.Image) },
		reset:  true,
		fetch:  fetchOpts{servers: true},
		probe:  m.refilled,
		converged: func(s state) error {
			if s.rf.UID == old {
				return errors.New("the RedisFailover hasn't been recreated yet")
			}
			return converged(s)
		},
	}
}

// resetVersions returns the versions a reset recreates the instance on:
// a chain's next start, in turn, or the configured ones. An empty version
// keeps the template's image.
func (m *Mutator) resetVersions() (redis, sentinel config.Version) {
	if ch := m.in.Chain; ch != nil {
		redis, _ = m.versions.VersionNamed(ch.Start[(m.resets+1)%len(ch.Start)])
		if ch.Sentinel != "" {
			return redis, redis
		}
	} else {
		redis, _ = m.versions.VersionNamed(m.in.Version)
	}
	sentinel, _ = m.versions.VersionNamed(m.in.SentinelVersion)
	return redis, sentinel
}

// convergedOn holds when the redis StatefulSet runs its replicas, all ready on
// redisImage and with its version in INFO server, the Sentinels also on
// sentinelImage (any, if empty), and the RedisFailover is Healthy. An empty
// redisImage is the image of the spec.
func convergedOn(versions *config.Config, redisImage, sentinelImage string) func(state) error {
	return func(s state) error {
		image := redisImage
		if image == "" {
			image = s.rf.Spec.Redis.Image
		}
		replicas := s.rf.Spec.Redis.Replicas
		if err := statefulSetConverged(s.sts, replicas); err != nil {
			return err
		}
		if err := podsReady(redisName, s.redis, replicas); err != nil {
			return err
		}
		if err := podsOn(versions, s.redis, redisName, image, s.servers); err != nil {
			return err
		}
		if s.rf.SentinelEnabled() {
			if err := sentinelReplicasConverged(s.rf.Spec.Sentinel.Replicas)(s); err != nil {
				return err
			}
			if sentinelImage != "" {
				if err := podsOn(versions, s.sentinels, sentinelName, sentinelImage, s.servers); err != nil {
					return err
				}
			}
		}
		return healthy(s.rf)
	}
}

// podsOn checks that every pod's container runs image, and reports its
// version in INFO server if it is a configured one.
func podsOn(versions *config.Config, pods []corev1.Pod, container, image string, servers map[string]server) error {
	v, known := versions.VersionOf(image)
	var errs []error
	for i := range pods {
		p := &pods[i]
		if got := containerImage(p, container); got != image {
			errs = append(errs, fmt.Errorf("%s runs %s, want %s", p.Name, got, image))
			continue
		}
		if !known {
			continue
		}
		switch sv, ok := servers[p.Name]; {
		case !ok:
			errs = append(errs, fmt.Errorf("%s: INFO server not read", p.Name))
		case sv.err != nil:
			errs = append(errs, fmt.Errorf("%s: INFO server: %w", p.Name, sv.err))
		case sv.name != v.Server || sv.version != v.Release:
			errs = append(errs, fmt.Errorf("%s reports %s %s, want %s %s", p.Name, sv.name, sv.version, v.Server, v.Release))
		}
	}
	return errors.Join(errs...)
}

func containerImage(p *corev1.Pod, container string) string {
	for _, c := range p.Spec.Containers {
		if c.Name == container {
			return c.Image
		}
	}
	return ""
}

// mixedWindow measures how long an instance runs two versions of the image a
// transition changes: from the first pod that runs the new version and
// reports it in INFO server, until no pod runs the old one.
type mixedWindow struct {
	t     *transition
	start time.Time
}

// podImage is the image of a pod and the server and version that it reports,
// empty if it did not answer.
type podImage struct {
	image, server, version string
}

// observe returns the window's length once it ended.
func (w *mixedWindow) observe(now time.Time, pods []podImage) (time.Duration, bool) {
	if w.start.IsZero() {
		if !slices.ContainsFunc(pods, func(p podImage) bool {
			return p.image == w.t.to.Image && p.server == w.t.to.Server && p.version == w.t.to.Release
		}) {
			return 0, false
		}
		w.start = now
	}
	if slices.ContainsFunc(pods, func(p podImage) bool { return p.image == w.t.from.Image }) {
		return 0, false
	}
	return now.Sub(w.start), true
}

// end ends the window when the instance is deleted, if it had started.
func (w *mixedWindow) end(now time.Time) (time.Duration, bool) {
	if w.start.IsZero() {
		return 0, false
	}
	return now.Sub(w.start), true
}

// podImages returns the image and server of the pods a transition changes.
func podImages(t *transition, s state) []podImage {
	pods := s.redis
	if t.sentinel {
		pods = s.sentinels
	}
	out := make([]podImage, 0, len(pods))
	for i := range pods {
		p := &pods[i]
		pi := podImage{image: containerImage(p, t.container())}
		if sv, ok := s.servers[p.Name]; ok && sv.err == nil {
			pi.server, pi.version = sv.name, sv.version
		}
		out = append(out, pi)
	}
	return out
}

// observation is what a version change left behind when it was judged.
type observation struct {
	converged bool
	// verified is whether the data could be verified, and lost what it
	// lacked.
	verified bool
	lost     int
	// master is the pod of the single master, "" without one. masterOn is the
	// version of its image, writable its reply to a write, and loadError a
	// line of its log that says that it could not load the data.
	master    string
	masterOn  string
	writable  error
	loadError string
	// sentinels is why a Sentinel does not report the master, nil if they all
	// do.
	sentinels error
	// onNew are the redis pods that run the new image.
	onNew []string
	// pods describes what each pod of the changed image does.
	pods []string
}

// classify judges a version change. ok: it converged. failed_safe: it did not
// converge, but the rollout stopped and kept the data. The master stayed on
// the old version and writable, it loaded its data, no acknowledged write
// was lost, the Sentinels that stayed still report it, and at most one pod
// runs the new image. That pod is a replica that cannot load the data, so the
// operator did not replace another pod. failed_unsafe: all other cases.
func classify(t *transition, o observation) (string, []string) {
	if o.converged {
		return transitionOK, nil
	}
	var unsafe []string
	switch {
	case !o.verified:
		unsafe = append(unsafe, "the data couldn't be verified")
	case o.lost > 0:
		unsafe = append(unsafe, fmt.Sprintf("%d acknowledged writes lost", o.lost))
	}
	if o.master == "" {
		unsafe = append(unsafe, "no single master")
	} else {
		if o.writable != nil {
			unsafe = append(unsafe, fmt.Sprintf("the master %s refuses writes: %v", o.master, o.writable))
		}
		if !t.sentinel && o.masterOn != t.from.Name {
			unsafe = append(unsafe, fmt.Sprintf("the master %s runs %s, not %s", o.master, o.masterOn, t.from.Name))
		}
		if o.loadError != "" {
			unsafe = append(unsafe, fmt.Sprintf("the master %s couldn't load the data: %s", o.master, o.loadError))
		}
		if o.sentinels != nil {
			unsafe = append(unsafe, fmt.Sprintf("the Sentinels don't all report the master %s: %v", o.master, o.sentinels))
		}
	}
	if len(o.onNew) > 1 {
		unsafe = append(unsafe, fmt.Sprintf("%d pods on %s: %s", len(o.onNew), t.to.Name, strings.Join(o.onNew, ", ")))
	}
	if len(unsafe) > 0 {
		return transitionFailedUnsafe, unsafe
	}
	return transitionFailedSafe, o.pods
}

// sentinelsAgree checks that every Sentinel of the state reports the address
// of the master.
func sentinelsAgree(s state, master string) error {
	var errs []error
	for _, p := range s.sentinels {
		m := s.sentinelMasters[p.Name]
		if m.err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p.Name, m.err))
		} else if got := net.JoinHostPort(m.fields["ip"], m.fields["port"]); got != master {
			errs = append(errs, fmt.Errorf("%s reports %s", p.Name, got))
		}
	}
	return errors.Join(errs...)
}

// describePod says what a pod of a stuck version change does: its version,
// readiness, restarts, why its container waits, its replication link, and
// a log line about loading the data.
func describePod(versions *config.Config, p *corev1.Pod, container string, sv server, loadError string) string {
	image := containerImage(p, container)
	parts := []string{p.Name}
	if v, ok := versions.VersionOf(image); ok {
		parts = append(parts, "on "+v.Name)
	} else {
		parts = append(parts, "on "+image)
	}
	parts = append(parts, fmt.Sprintf("ready=%t", observer.Ready(p)))
	for _, cs := range p.Status.ContainerStatuses {
		if cs.Name != container {
			continue
		}
		parts = append(parts, fmt.Sprintf("restarts=%d", cs.RestartCount))
		if w := cs.State.Waiting; w != nil {
			parts = append(parts, "waiting="+w.Reason)
		}
	}
	switch {
	case sv.err != nil:
		parts = append(parts, "INFO: "+sv.err.Error())
	case sv.fields["master_link_status"] != "":
		parts = append(parts, "link="+sv.fields["master_link_status"])
	case sv.fields["role"] != "":
		parts = append(parts, "role="+sv.fields["role"])
	}
	if loadError != "" {
		parts = append(parts, "log: "+loadError)
	}
	return strings.Join(parts, " ")
}

// loadErrorLine returns the first line of a server log that says that the
// server could not load the data, without its pid and timestamp prefix.
func loadErrorLine(log string) string {
	for line := range strings.Lines(log) {
		l := strings.ToLower(line)
		for _, s := range loadErrors {
			if strings.Contains(l, s) {
				if i := strings.Index(line, " # "); i >= 0 {
					line = line[i+3:]
				}
				line = strings.TrimSpace(line)
				if len(line) > 200 {
					line = line[:200]
				}
				return line
			}
		}
	}
	return ""
}

// loadErrors are what Redis and Valkey log when they cannot load an RDB file,
// from disk or from a full sync.
var loadErrors = []string{
	"can't handle rdb format version",
	"wrong signature trying to load db",
	"short read or oom loading db",
	"fatal error loading the db",
	"failed trying to load the master synchronization db",
	"failed trying to load the primary synchronization db",
	"bad file format reading the append only file",
}
