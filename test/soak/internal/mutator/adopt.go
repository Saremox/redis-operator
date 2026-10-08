package mutator

import (
	"context"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/saremox/redis-operator/test/soak/internal/config"
	"github.com/saremox/redis-operator/test/soak/internal/observer"
)

// stuckChange returns the stuck version change that the state s shows, and the
// pod on the new version. The tester keeps a version change in its own process
// only, so a new process reads the change from the cluster. All of these must
// hold:
//   - exactly one redis pod runs the image of the spec, is not Ready and does
//     not replicate;
//   - every other redis pod is Ready and runs one older image, the master
//     among them;
//   - the chain of the instance has an unknown edge from the older version to
//     the new one. A stuck replica on an ok edge is a finding.
func (m *Mutator) stuckChange(s state, master string) (*transition, *corev1.Pod) {
	ch := m.in.Chain
	to, known := m.versions.VersionOf(s.rf.Spec.Redis.Image)
	replicas := int(s.rf.Spec.Redis.Replicas)
	if ch == nil || !known || replicas < 2 || len(s.redis) != replicas {
		return nil, nil
	}
	var stuck *corev1.Pod
	var from config.Version
	for i := range s.redis {
		p := &s.redis[i]
		image := containerImage(p, redisName)
		if image == to.Image {
			if stuck != nil || observer.Ready(p) {
				return nil, nil
			}
			stuck = p
			continue
		}
		v, known := m.versions.VersionOf(image)
		if !known || !observer.Ready(p) || from.Name != "" && v != from {
			return nil, nil
		}
		from = v
	}
	if stuck == nil || from.Name == "" || master == stuck.Name || !slices.ContainsFunc(s.redis, func(p corev1.Pod) bool { return p.Name == master }) {
		return nil, nil
	}
	if sv := s.servers[stuck.Name]; sv.err == nil && sv.fields["master_link_status"] == "up" {
		return nil, nil
	}
	for _, e := range m.versions.EdgesFrom(from.Name, ch) {
		if e.To == to.Name && e.Expect == config.ExpectUnknown {
			return &transition{edge: e, from: from, to: to}, stuck
		}
	}
	return nil, nil
}

// stuck reads the instance and returns the change that it shows, the pod
// that is stuck and the line of its log that says why. A log line is the
// proof that the pod cannot load the data: a pod of an ordinary rollout is
// also not Ready for a while.
func (m *Mutator) stuck(ctx context.Context) (t *transition, pod, line string) {
	s, err := m.fetch(ctx, fetchOpts{servers: true})
	if err != nil {
		return nil, "", ""
	}
	t, p := m.stuckChange(s, m.observer.Master())
	if t == nil {
		return nil, "", ""
	}
	if line = m.loadError(ctx, p, redisName); line == "" {
		return nil, "", ""
	}
	return t, p.Name, line
}

// adopt judges a version change that a former process started, and resets
// the instance if the change failed safely. Without it, the instance stays
// in a violation that no window explains, and the mutator waits for it
// forever.
func (m *Mutator) adopt(ctx context.Context) {
	if m.in.Chain == nil || m.instance == nil {
		return
	}
	if !waitFor(ctx, func() bool { return !m.observer.Report().At.IsZero() }) {
		return
	}
	t, pod, line := m.stuck(ctx)
	if t == nil {
		return
	}
	log := m.log.With("kind", config.ImageUpgrade, "step", 0, "adopted", true)
	log.Info("rollout stuck", "from", t.edge.From, "to", t.edge.To, "pod", pod, "log", line,
		"grace_seconds", m.grace.Seconds())
	select {
	case <-ctx.Done():
		return
	case <-time.After(m.grace):
	}
	if again, _, _ := m.stuck(ctx); again == nil {
		return
	}
	unlock := m.lock.Shared()
	defer unlock()
	vctx, cancel := context.WithTimeout(ctx, verifyBound)
	lost, verr := m.verify(vctx, string(config.ImageUpgrade), 0, false)
	cancel()
	o := m.observe(ctx, t)
	o.lost, o.verified = lost, verr == nil
	if result, reasons := classify(t, o); result != transitionFailedSafe {
		log.Warn("rollout not adopted", "result", result, "reasons", reasons)
		return
	}
	m.record(t, o, lost, verr, log)
	m.resetWhy = "after " + t.edge.String() + " didn't converge, found after a restart"
	m.mutate(ctx, 0, stepRand(m.cfg.Seed, m.in, 0), config.Reset)
}
