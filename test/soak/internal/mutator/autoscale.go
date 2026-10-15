package mutator

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/test/soak/internal/autoscale"
	"github.com/saremox/redis-operator/test/soak/internal/config"
	"github.com/saremox/redis-operator/test/soak/internal/maxmem"
)

const (
	// loadPercent is above the 95 percent that gives a step at the next
	// sample, and below maxmemory, so that no write fails.
	loadPercent = 97
	// loadHold bounds the wait for a step: the operator samples one time in
	// each minute, and starts a step at a reconcile after the sample.
	loadHold = 3 * time.Minute
)

// loader is the part of Data that loads an instance up to a step.
type loader interface {
	Load(ctx context.Context, percent int64, hold time.Duration, until func(context.Context) (bool, error)) (string, error)
}

// notApplied is a mutation that found nothing to do when it ran, for example
// no step to disturb. Its result is skipped.
type notApplied struct{ error }

// The user paths of the autoscale kinds. Mode Off, a lower memoryMax and a
// change of the spec limit may recreate and shrink a pod. A load, mode
// Recommend or Auto and a lost state may not.
var (
	allowNone     = &autoscale.Allow{}
	allowResize   = &autoscale.Allow{Recreate: true, Shrink: true}
	allowKill     = &autoscale.Allow{Recreate: true}
	allowFailover = &autoscale.Allow{Recreate: true, Handover: true}
)

// nextMode is the cycle of autoscale_mode.
var nextMode = map[string]string{
	redisfailoverv1.AutoscalingModeAuto:      redisfailoverv1.AutoscalingModeRecommend,
	redisfailoverv1.AutoscalingModeRecommend: redisfailoverv1.AutoscalingModeOff,
	redisfailoverv1.AutoscalingModeOff:       redisfailoverv1.AutoscalingModeAuto,
}

func (m *Mutator) planAutoscale(r *rand.Rand, kind config.Kind, s state) plan {
	a := s.rf.Spec.Redis.Autoscaling
	if a == nil {
		return skipped(kind, "the RedisFailover has no redis.autoscaling")
	}
	replicas := s.rf.Spec.Redis.Replicas
	converged := autoscaleConverged(replicas)
	if kind == config.AutoscaleMode {
		want := nextMode[a.Mode]
		p := plan{
			kind:      kind,
			params:    fmt.Sprintf("redis.autoscaling.mode %s -> %s", a.Mode, want),
			patch:     mergePatch(map[string]any{"redis": map[string]any{"autoscaling": map[string]any{"mode": want}}}),
			allow:     allowNone,
			converged: modeConverged(want, replicas),
		}
		if want == redisfailoverv1.AutoscalingModeOff {
			p.allow = allowResize
		}
		return p
	}
	if a.Mode != redisfailoverv1.AutoscalingModeAuto {
		return skipped(kind, "mode %s", a.Mode)
	}
	switch kind {
	case config.AutoscaleLimit:
		return planLimit(r, m.in.Autoscale.Limit, s)
	case config.AutoscaleMemoryMax:
		cur := a.MemoryMax.Value()
		want := pickOther(r, m.in.Autoscale.MemoryMax, cur/mi) * mi
		if spec := s.rf.Spec.Redis.Resources.Limits.Memory().Value(); want < spec {
			return skipped(kind, "memoryMax %s is below the limit %s", maxmem.FormatBytes(want), maxmem.FormatBytes(spec))
		}
		q := resource.NewQuantity(want, resource.BinarySI)
		return plan{
			kind:   kind,
			params: fmt.Sprintf("redis.autoscaling.memoryMax %s -> %s", a.MemoryMax.String(), q.String()),
			patch:  mergePatch(map[string]any{"redis": map[string]any{"autoscaling": map[string]any{"memoryMax": q.String()}}}),
			allow:  allowResize,
			converged: func(s state) error {
				if mm := s.rf.Spec.Redis.Autoscaling; mm == nil || mm.MemoryMax == nil || mm.MemoryMax.Value() != want {
					return errors.New("the RedisFailover does not have the change yet")
				}
				return converged(s)
			},
		}
	case config.AutoscaleStateLost:
		name := "rfa-" + m.in.Name
		return plan{
			kind:   kind,
			params: "delete the ConfigMap " + name,
			allow:  allowNone,
			action: func(ctx context.Context) error {
				err := m.kube.CoreV1().ConfigMaps(m.in.Namespace).Delete(ctx, name, metav1.DeleteOptions{})
				if apierrors.IsNotFound(err) {
					return notApplied{fmt.Errorf("no ConfigMap %s", name)}
				}
				return err
			},
			converged: converged,
		}
	case config.AutoscaleLoad:
		return m.planLoad(kind, s, "", "", converged)
	case config.AutoscaleKillMaster:
		pod, why := labelledMaster(s, m.observer.Master())
		if why != "" {
			return skipped(kind, "%s", why)
		}
		return m.planLoad(kind, s, pod.Name, pod.UID, converged)
	case config.AutoscaleKillReplica:
		replicas := withRole(s.redis, roleReplica)
		if len(replicas) == 0 {
			return skipped(kind, "no replica")
		}
		pod := replicas[r.IntN(len(replicas))]
		return m.planLoad(kind, s, pod.Name, pod.UID, converged)
	}
	return skipped(kind, "unknown kind")
}

// planLimit changes the spec limit, and a memory request with it, because
// mode Auto needs a request equal to the limit.
func planLimit(r *rand.Rand, rg config.Range, s state) plan {
	const kind = config.AutoscaleLimit
	res := s.rf.Spec.Redis.Resources
	lim := res.Limits.Memory()
	want := resource.NewQuantity(pickOther(r, rg, lim.Value()/mi)*mi, resource.BinarySI)
	if mm := s.rf.Spec.Redis.Autoscaling.MemoryMax; mm != nil && want.Cmp(*mm) > 0 {
		return skipped(kind, "the limit %s is above memoryMax %s", want.String(), mm.String())
	}
	patch := map[string]any{"limits": map[string]string{"memory": want.String()}}
	if _, ok := res.Requests[corev1.ResourceMemory]; ok {
		patch["requests"] = map[string]string{"memory": want.String()}
	}
	converged := autoscaleConverged(s.rf.Spec.Redis.Replicas)
	return plan{
		kind:   kind,
		params: fmt.Sprintf("memory %s -> %s", lim.String(), want.String()),
		patch:  mergePatch(map[string]any{"redis": map[string]any{"resources": patch}}),
		allow:  allowResize,
		converged: func(s state) error {
			if !s.rf.Spec.Redis.Resources.Limits.Memory().Equal(*want) {
				return errors.New("the RedisFailover does not have the change yet")
			}
			return converged(s)
		},
	}
}

// planLoad loads the instance up to a step. With a pod, it deletes the pod
// gracefully while the step is Applying, and the result is skipped when no
// step is Applying within loadHold.
func (m *Mutator) planLoad(kind config.Kind, s state, pod string, uid types.UID, converged func(state) error) plan {
	l, ok := m.data.(loader)
	if !ok {
		return skipped(kind, "no data")
	}
	before := s.rf.Status.Autoscaling.LastStep
	p := plan{kind: kind, params: fmt.Sprintf("load to %d percent of maxmemory for at most %s", loadPercent, loadHold), allow: allowNone, converged: converged}
	until := func(ctx context.Context) (bool, error) {
		rf, err := m.rfs.DatabasesV1().RedisFailovers(m.in.Namespace).Get(ctx, m.in.Name, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		st := rf.Status.Autoscaling
		if st.LastStep == before {
			return false, nil
		}
		if pod == "" {
			return true, nil
		}
		if st.Phase != autoscale.PhaseApplying {
			// The step ended before the tester saw it Applying.
			return false, notApplied{fmt.Errorf("the step of %s is %s", st.LastStep, st.Phase)}
		}
		return true, m.deletePod(ctx, pod, uid, false)
	}
	if pod != "" {
		p.params += fmt.Sprintf(", delete pod %s (graceful) while the step is Applying", pod)
		p.allow = allowKill
		if kind == config.AutoscaleKillMaster {
			p.allow = allowFailover
		}
		p.converged = func(s state) error {
			if err := redisPodReplaced(pod, uid)(s); err != nil {
				return err
			}
			return converged(s)
		}
	}
	p.action = func(ctx context.Context) error {
		deleted := false
		skip, err := l.Load(ctx, loadPercent, loadHold, func(ctx context.Context) (bool, error) {
			ok, err := until(ctx)
			deleted = ok
			return ok, err
		})
		switch {
		case skip != "":
			return notApplied{fmt.Errorf("load not started: %s", skip)}
		case err != nil:
			return err
		case pod != "" && !deleted:
			return notApplied{fmt.Errorf("no step was Applying within %s", loadHold)}
		}
		return nil
	}
	return p
}

// autoscaleConverged holds when no step is Applying, the rollout is complete,
// no resize is pending, each redis pod runs with at least the limit of the
// StatefulSet, maxmemory follows, and the RedisFailover is Healthy. A pod can
// keep a larger limit, for example on Kubernetes 1.33 and 1.34, which cannot
// lower a limit in place.
func autoscaleConverged(replicas int32) func(state) error {
	return func(s state) error {
		if st := s.rf.Status.Autoscaling; st.Phase == autoscale.PhaseApplying {
			return fmt.Errorf("the step of %s is Applying", st.LastStep)
		}
		if err := statefulSetConverged(s.sts, replicas); err != nil {
			return err
		}
		if err := podsReady("redis", s.redis, replicas); err != nil {
			return err
		}
		want := templateLimit(s.sts)
		for i := range s.redis {
			p := &s.redis[i]
			if p.Annotations[resizeRequest] != "" {
				return fmt.Errorf("%s: resize requested at %s", p.Name, p.Annotations[resizeRequest])
			}
			l := maxmem.RedisLimit(p)
			if l < want {
				return fmt.Errorf("%s runs with the limit %s, the StatefulSet has %s", p.Name, maxmem.FormatBytes(l), maxmem.FormatBytes(want))
			}
			// A lower limit that the kubelet did not apply yet would shrink the
			// pod after the window, without a user path.
			if spec := podSpecLimit(p); spec != 0 && spec != l {
				return fmt.Errorf("%s runs with the limit %s, its spec has %s", p.Name, maxmem.FormatBytes(l), maxmem.FormatBytes(spec))
			}
		}
		if err := maxmem.Check(s.rf, s.rf.Status.Message, s.config); err != nil {
			return err
		}
		return healthy(s.rf)
	}
}

// modeConverged holds when the spec has the mode and the instance converged.
// Mode Off gives each pod the spec limit and removes the status.
func modeConverged(mode string, replicas int32) func(state) error {
	converged := autoscaleConverged(replicas)
	return func(s state) error {
		if a := s.rf.Spec.Redis.Autoscaling; a == nil || a.Mode != mode {
			return errors.New("the RedisFailover does not have the change yet")
		}
		if err := converged(s); err != nil || mode != redisfailoverv1.AutoscalingModeOff {
			return err
		}
		if s.rf.Status.Autoscaling != (redisfailoverv1.AutoscalingStatus{}) {
			return fmt.Errorf("status.autoscaling is not empty: %+v", s.rf.Status.Autoscaling)
		}
		spec := s.rf.Spec.Redis.Resources.Limits.Memory().Value()
		for i := range s.redis {
			if l := maxmem.RedisLimit(&s.redis[i]); l != spec {
				return fmt.Errorf("%s runs with the limit %s, the spec has %s", s.redis[i].Name, maxmem.FormatBytes(l), maxmem.FormatBytes(spec))
			}
		}
		return nil
	}
}

// templateLimit returns the memory limit of the redis container of the
// StatefulSet template.
func templateLimit(sts *appsv1.StatefulSet) int64 {
	return redisLimitOf(sts.Spec.Template.Spec.Containers)
}

// podSpecLimit returns the memory limit of the redis container in the pod
// spec, which a resize changes before the kubelet applies it.
func podSpecLimit(p *corev1.Pod) int64 {
	return redisLimitOf(p.Spec.Containers)
}

func redisLimitOf(containers []corev1.Container) int64 {
	for _, c := range containers {
		if c.Name == redisName {
			return c.Resources.Limits.Memory().Value()
		}
	}
	return 0
}
