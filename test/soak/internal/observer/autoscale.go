package observer

import (
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/saremox/redis-operator/test/soak/internal/autoscale"
)

// SetMutation gives the observer what the running mutation of the instance
// may do to the pods. ok is false while no mutation runs. Call it before Run.
func (o *Observer) SetMutation(allowed func() (a autoscale.Allow, ok bool)) {
	o.mutation = allowed
}

// judgeAutoscale gives the round to the judge of an instance with autoscale,
// and counts its events. A step opens a window, because the operator sets
// maxmemory one reconcile after the last resize. changed is a change of the
// generation in this round.
func (o *Observer) judgeAutoscale(now time.Time, s snapshot, generation int64, changed bool) {
	if o.judge == nil || s.rf == nil {
		return
	}
	events, settling := o.judge.Observe(autoscaleRound(now, s, generation, o.allowed(changed)))
	for _, e := range events {
		log := o.log.With("step", e.Step)
		if e.Judge == "" {
			o.steps.WithLabelValues(e.Result).Inc()
			log.Info("autoscale step", "result", e.Result, "reason", e.Message)
			continue
		}
		o.findings.WithLabelValues(e.Judge).Inc()
		log.Warn("autoscale judge violated", "invariant", e.Judge, "finding", true, "reason", e.Message)
	}
	if settling {
		if !o.tracker.windowOpen() {
			o.log.Info("convergence window opened", "by", "autoscale step")
		}
		o.tracker.extend(now)
	}
}

// allowed returns what the user paths of this round may do to the pods. A
// chaos action, a stopped operator or a change by hand may do all of it.
func (o *Observer) allowed(changed bool) autoscale.Allow {
	if o.lock.Disturbance() != "" || o.lock.Chaos() != "" {
		return autoscale.AllowAll
	}
	if o.mutation != nil {
		if a, ok := o.mutation(); ok {
			return a
		}
	}
	if changed {
		return autoscale.AllowAll
	}
	return autoscale.Allow{}
}

func autoscaleRound(now time.Time, s snapshot, generation int64, allow autoscale.Allow) autoscale.Round {
	r := autoscale.Round{
		Now:        now,
		Generation: generation,
		Spec:       s.rf.Spec.Redis.Resources.Limits.Memory().Value(),
		Status:     s.rf.Status.Autoscaling,
		Allow:      allow,
	}
	if a := s.rf.Spec.Redis.Autoscaling; a != nil && a.MemoryMax != nil {
		r.Max = a.MemoryMax.Value()
	}
	if m, err := s.master(); err == nil {
		r.Master = m.Name
	}
	for _, p := range s.redis {
		mm, _ := strconv.ParseInt(p.config["maxmemory"], 10, 64)
		r.Pods = append(r.Pods, autoscale.Pod{Name: p.Name, UID: p.UID, Restarts: p.restarts, Limit: p.limit, MaxMemory: mm})
	}
	return r
}

// restarts returns the restart count of a container of the pod.
func restarts(p *corev1.Pod, container string) int32 {
	for _, cs := range p.Status.ContainerStatuses {
		if cs.Name == container {
			return cs.RestartCount
		}
	}
	return 0
}
