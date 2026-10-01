package mutator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/saremox/redis-operator/test/soak/internal/prober"
)

// judge classifies a version change, counts it, and counts a finding if it
// failed unsafely.
func (m *Mutator) judge(ctx context.Context, t *transition, converged bool, lost int, verifyErr error, log *slog.Logger) string {
	o := observation{converged: true}
	if !converged {
		o = m.observe(ctx, t)
	}
	o.lost, o.verified = lost, verifyErr == nil
	result, reasons := classify(t, o)
	m.transitions.WithLabelValues(t.edge.From, t.edge.To, t.edge.Expect, result).Inc()
	log = log.With("from", t.edge.From, "to", t.edge.To, "expect", t.edge.Expect, "sentinel", t.sentinel,
		"result", result, "lost", lost, "verified", verifyErr == nil)
	if !converged {
		writable := "ok"
		if o.writable != nil {
			writable = prober.Classify(o.writable)
		}
		log = log.With("master", o.master, "master_version", o.masterOn, "master_writable", writable, "reasons", reasons, "pods", o.pods)
	}
	switch result {
	case transitionOK:
		log.Info("version transition")
	case transitionFailedSafe:
		log.Warn("version transition")
	default:
		m.findings.WithLabelValues(invVersionTransition).Inc()
		log.Warn("version transition", "finding", true)
	}
	return result
}

// observe gathers what a version change that didn't converge left behind:
// the master, its version and whether it takes writes, and for every pod
// of the changed image its state and any log line about loading the data.
func (m *Mutator) observe(ctx context.Context, t *transition) observation {
	var o observation
	s, err := m.fetch(ctx, fetchOpts{servers: true})
	if err != nil {
		o.pods = []string{"reading the instance: " + err.Error()}
		return o
	}
	addr := m.observer.MasterAddr()
	if addr != "" {
		o.master = m.observer.Master()
		o.writable = m.writable(ctx, addr)
	}
	pods := s.redis
	if t.sentinel {
		pods = s.sentinels
	}
	for i := range s.redis {
		p := &s.redis[i]
		if p.Name != o.master {
			continue
		}
		v, _ := m.versions.VersionOf(containerImage(p, redisName))
		o.masterOn = v.Name
		o.loadError = m.loadError(ctx, p, redisName)
	}
	for i := range pods {
		p := &pods[i]
		le := ""
		if containerImage(p, t.container()) == t.to.Image {
			le = m.loadError(ctx, p, t.container())
		}
		o.pods = append(o.pods, describePod(m.versions, p, t.container(), s.servers[p.Name], le))
	}
	return o
}

// writable writes a key to the master's pod directly.
func (m *Mutator) writable(ctx context.Context, addr string) error {
	ctx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()
	c := m.podClientAddr(addr, m.auth.Provider())
	defer func() { _ = c.Close() }()
	return c.Set(ctx, "soak:"+m.in.Name+":version:writable", time.Now().Unix(), 0).Err()
}

// loadError returns a line of the container's log, or of its previous
// run's, saying it couldn't load the data.
func (m *Mutator) loadError(ctx context.Context, p *corev1.Pod, container string) string {
	restarted := false
	for _, cs := range p.Status.ContainerStatuses {
		restarted = restarted || cs.Name == container && cs.RestartCount > 0
	}
	for _, previous := range []bool{false, true} {
		if previous && !restarted {
			break
		}
		tail := int64(500)
		b, err := m.kube.CoreV1().Pods(m.in.Namespace).GetLogs(p.Name, &corev1.PodLogOptions{
			Container: container, Previous: previous, TailLines: &tail,
		}).DoRaw(ctx)
		if err != nil {
			continue
		}
		if line := loadErrorLine(string(b)); line != "" {
			return line
		}
	}
	return ""
}

// reset deletes the instance, its volumes and its auth Secret, waits until
// they are gone, and creates it again on the given images.
func (m *Mutator) reset(ctx context.Context, redisImage, sentinelImage string) error {
	start := time.Now()
	dctx, cancel := context.WithTimeout(ctx, m.convergeTimeout)
	err := m.instance.Delete(dctx)
	cancel()
	if err != nil {
		return expectation{fmt.Errorf("deleting the instance: %w", err)}
	}
	m.log.Info("reset phase done", "phase", "delete", "duration_seconds", time.Since(start).Seconds())
	if m.mixed != nil {
		if d, ok := m.mixed.end(time.Now()); ok {
			m.recordMixed(d)
		}
		m.mixed = nil
	}
	if m.data != nil {
		m.data.Refill()
	}
	if err := m.instance.Create(ctx, redisImage, sentinelImage); err != nil {
		return err
	}
	m.resets++
	return nil
}

// refilled holds once the recreated instance is filled again.
func (m *Mutator) refilled(context.Context) error {
	if m.data != nil && !m.data.Filled() {
		return errors.New("refilling")
	}
	return nil
}
