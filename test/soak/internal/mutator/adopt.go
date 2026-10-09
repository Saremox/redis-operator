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
//   - every other redis pod is Ready and runs one other configured image, the
//     master among them;
//   - the chain of the instance has an unknown or fail edge from the version
//     of the other pods to the new one. A stuck replica on an ok edge is a
//     finding.
func (m *Mutator) stuckChange(s state, master string) (*transition, *corev1.Pod) {
	ch := m.in.Chain
	to, known := m.versions.VersionOf(s.rf.Spec.Redis.Image)
	replicas := int(s.rf.Spec.Redis.Replicas)
	if ch == nil || !known || len(s.redis) != replicas {
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
		if e.To == to.Name && e.Expect != config.ExpectOK {
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

// adoptMargin is the time that the adoption keeps free before the startup
// window times out.
const adoptMargin = time.Minute

// adoptDeadline is the last time at which a stuck change can still show: the
// convergence timeout, minus the grace, the verification bound and adoptMargin,
// after the start.
func (m *Mutator) adoptDeadline(started time.Time) time.Time {
	return started.Add(m.convergeTimeout - m.grace - verifyBound - adoptMargin)
}

// findStuck reads the instance at each observer interval until it shows a
// stuck change, the instance is quiet, or the deadline passes. A restart can
// come before the operator replaced a pod, or before the new pod logged its
// load error.
func (m *Mutator) findStuck(ctx context.Context, deadline time.Time) (t *transition, pod, line string) {
	tick := time.NewTicker(m.observerCfg.Interval.Duration)
	defer tick.Stop()
	for !m.observer.Quiet() {
		if t, pod, line = m.stuck(ctx); t != nil {
			return t, pod, line
		}
		if time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return nil, "", ""
		case <-tick.C:
		}
	}
	return nil, "", ""
}

// adopt judges a version change that a former process started, and resets
// the instance if the change failed safely. started is the start of the
// tester. Without adoption, the instance stays in a violation that no window
// explains, and the mutator waits for it forever. If the master refuses writes
// or the ledger shows a lost write, the instance stays as it is, and the gauge
// shows the stall.
func (m *Mutator) adopt(ctx context.Context, started time.Time) {
	if m.in.Chain == nil || m.instance == nil {
		return
	}
	if !waitFor(ctx, func() bool { return !m.observer.Report().At.IsZero() }) {
		return
	}
	t, pod, line := m.findStuck(ctx, m.adoptDeadline(started))
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
	// The wait for the lock can span a mutation or a chaos action. Read again.
	unlock := m.lock.Shared()
	defer unlock()
	if t, _, _ = m.stuck(ctx); t == nil {
		return
	}
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
