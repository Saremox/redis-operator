package chaos

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/saremox/redis-operator/test/soak/internal/config"
	"github.com/saremox/redis-operator/test/soak/internal/observer"
)

// leaseName is the Lease of the operator leader.
const leaseName = "redis-failover-lease"

// operatorState is the operator Deployment, its pods and its leader's
// Lease.
type operatorState struct {
	deployment *appsv1.Deployment
	pods       []corev1.Pod
	lease      *coordinationv1.Lease
}

func (l *Lane) operatorState(ctx context.Context) (operatorState, error) {
	var s operatorState
	var err error
	ns := l.operator.Namespace
	if s.deployment, err = l.kube.AppsV1().Deployments(ns).Get(ctx, l.operator.Deployment, metav1.GetOptions{}); err != nil {
		return s, err
	}
	selector, err := metav1.LabelSelectorAsSelector(s.deployment.Spec.Selector)
	if err != nil {
		return s, err
	}
	pods, err := l.kube.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
	if err != nil {
		return s, err
	}
	s.pods = pods.Items
	s.lease, err = l.kube.CoordinationV1().Leases(ns).Get(ctx, leaseName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		s.lease, err = nil, nil
	}
	return s, err
}

// leader returns the operator pod that holds the Lease, its holder
// identity being the pod's hostname, an underscore and a UUID.
func leader(s operatorState) (*corev1.Pod, error) {
	if s.lease == nil || s.lease.Spec.HolderIdentity == nil || *s.lease.Spec.HolderIdentity == "" {
		return nil, errors.New("no operator holds the lease")
	}
	name, _, _ := strings.Cut(*s.lease.Spec.HolderIdentity, "_")
	for i := range s.pods {
		if s.pods[i].Name == name {
			return &s.pods[i], nil
		}
	}
	return nil, fmt.Errorf("the lease holder %s is no operator pod", name)
}

// leading holds once a Ready operator pod that satisfies ok holds the
// Lease, and returns it.
func leading(s operatorState, ok func(*corev1.Pod) error) (*corev1.Pod, error) {
	p, err := leader(s)
	if err != nil {
		return nil, err
	}
	if !observer.Ready(p) {
		return nil, fmt.Errorf("the leader %s is not ready", p.Name)
	}
	if err := ok(p); err != nil {
		return nil, fmt.Errorf("the leader %s: %w", p.Name, err)
	}
	return p, nil
}

// restarted holds once the leader is a new pod, none of old.
func restarted(s operatorState, old []types.UID) (*corev1.Pod, error) {
	return leading(s, func(p *corev1.Pod) error {
		if slices.Contains(old, p.UID) {
			return errors.New("was there before the restart")
		}
		return nil
	})
}

// upgraded holds once the Deployment rolled out image, nothing else runs,
// and a pod on it leads.
func upgraded(s operatorState, image string) (*corev1.Pod, error) {
	d := s.deployment
	want := int32(1)
	if d.Spec.Replicas != nil {
		want = *d.Spec.Replicas
	}
	switch {
	case d.Spec.Template.Spec.Containers[0].Image != image:
		return nil, fmt.Errorf("the Deployment runs %s, not %s", d.Spec.Template.Spec.Containers[0].Image, image)
	case d.Status.ObservedGeneration < d.Generation:
		return nil, errors.New("the Deployment's change isn't observed yet")
	case d.Status.Replicas != want || d.Status.UpdatedReplicas != want || d.Status.ReadyReplicas != want || d.Status.AvailableReplicas != want:
		return nil, fmt.Errorf("the Deployment has %d replicas, %d updated, %d ready, %d available, want %d",
			d.Status.Replicas, d.Status.UpdatedReplicas, d.Status.ReadyReplicas, d.Status.AvailableReplicas, want)
	}
	for i := range s.pods {
		if p := &s.pods[i]; p.Spec.Containers[0].Image != image {
			return nil, fmt.Errorf("%s still runs %s", p.Name, p.Spec.Containers[0].Image)
		}
	}
	return leading(s, func(p *corev1.Pod) error {
		if p.Spec.Containers[0].Image != image {
			return fmt.Errorf("runs %s", p.Spec.Containers[0].Image)
		}
		return nil
	})
}

// downSince returns how long the instances ran without a leading operator:
// from stopped until the new leader acquired the Lease.
func downSince(s operatorState, stopped time.Time) time.Duration {
	if s.lease == nil || s.lease.Spec.AcquireTime == nil {
		return 0
	}
	return max(0, s.lease.Spec.AcquireTime.Sub(stopped))
}

// release ends the disturbance of every instance, and returns when.
func (l *Lane) release() time.Time {
	l.lock.Disturb("")
	return time.Now()
}

// restart deletes the operator pods, waits until a new one leads, and
// then until every instance is quiet.
func (l *Lane) restart(ctx context.Context, a *action) outcome {
	s, err := l.operatorState(ctx)
	if err != nil {
		return outcome{err: err}
	}
	if len(s.pods) == 0 {
		return outcome{skip: "no operator pod"}
	}
	before := "none"
	if p, err := leader(s); err == nil {
		before = p.Name
	}
	var old []types.UID
	for _, p := range s.pods {
		old = append(old, p.UID)
	}
	a.begin()
	l.lock.Disturb("operator restart")
	defer l.lock.Disturb("")
	start := time.Now()
	for _, p := range s.pods {
		uid := p.UID
		err := l.kube.CoreV1().Pods(l.operator.Namespace).Delete(ctx, p.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
		if err != nil && !apierrors.IsNotFound(err) {
			return outcome{err: err}
		}
	}
	var after *corev1.Pod
	err = l.await(ctx, func(ctx context.Context) error {
		if s, err = l.operatorState(ctx); err != nil {
			return err
		}
		after, err = restarted(s, old)
		return err
	})
	if err != nil {
		return outcome{timeout: fmt.Errorf("the operator: %w", err)}
	}
	down := downSince(s, start)
	l.operatorDown.WithLabelValues(string(a.kind)).Observe(down.Seconds())
	since := l.release()
	if err := l.await(ctx, func(context.Context) error { return quiet(l.reports(), since) }); err != nil {
		return outcome{timeout: err}
	}
	d := time.Since(start)
	a.log.Info("operator restarted", "from", before, "to", after.Name, "operator_down_seconds", down.Seconds(),
		"converge_seconds", d.Seconds())
	return outcome{converged: []time.Duration{d}}
}

// upgrade upgrades the operator release to every other configured version
// in turn, and back to the one it runs.
func (l *Lane) upgrade(ctx context.Context, a *action) outcome {
	s, err := l.operatorState(ctx)
	if err != nil {
		return outcome{err: err}
	}
	image := s.deployment.Spec.Template.Spec.Containers[0].Image
	versions := l.cfg.Upgrade.Versions
	i := slices.IndexFunc(versions, func(v config.OperatorVersion) bool { return v.Image == image })
	if i < 0 {
		return outcome{skip: fmt.Sprintf("the operator runs %s, no configured version", image)}
	}
	hops := append(slices.Clone(versions[i+1:]), versions[:i+1]...)
	a.begin()
	var o outcome
	from := versions[i]
	for _, to := range hops {
		d, err, timeout := l.hop(ctx, a, from, to)
		if ctx.Err() != nil {
			return o
		}
		switch {
		case err != nil:
			o.err = errors.Join(o.err, fmt.Errorf("%s -> %s: %w", from.Name, to.Name, err))
		case timeout != nil:
			o.timeout = errors.Join(o.timeout, fmt.Errorf("%s -> %s: %w", from.Name, to.Name, timeout))
		default:
			o.converged = append(o.converged, d)
		}
		from = to
	}
	return o
}

// helmArgs returns the arguments of the upgrade to a version: the
// release's own values, reused, then the version's image and the
// configured --set values. The chart names the Deployment after the
// release.
func helmArgs(ch config.Chaos, op config.Operator, to config.OperatorVersion) []string {
	u := ch.Upgrade
	args := []string{"upgrade", op.Deployment, to.Chart, "--namespace", op.Namespace, "--reset-then-reuse-values",
		"--set", "image.repository=" + to.Repository(), "--set", "image.tag=" + to.Tag(),
		"--wait", "--timeout", ch.Timeout.Duration.String()}
	if to.Version != "" {
		args = append(args, "--version", to.Version)
	}
	for _, s := range u.Set {
		args = append(args, "--set", s)
	}
	return args
}

// hop upgrades to one version. helm runs the CRD hook of the chart first, and
// fails if the hook fails. hop then waits until a pod on the new image leads,
// and until all instances are quiet. It returns the time from the start of the
// upgrade to convergence, or why it failed or did not converge in time.
func (l *Lane) hop(ctx context.Context, a *action, from, to config.OperatorVersion) (time.Duration, error, error) {
	log := a.log.With("from", from.Name, "to", to.Name)
	l.lock.Disturb("operator upgrade to " + to.Name)
	defer l.lock.Disturb("")
	start := time.Now()
	out, err := l.helm(ctx, helmArgs(l.cfg, l.operator, to)...)
	log = log.With("helm_seconds", time.Since(start).Seconds())
	if err != nil {
		log.Warn("helm upgrade failed", "error", err.Error(), "output", tail(string(out), 20))
		return 0, fmt.Errorf("helm upgrade: %w", err), nil
	}
	var s operatorState
	var after *corev1.Pod
	err = l.await(ctx, func(ctx context.Context) error {
		if s, err = l.operatorState(ctx); err != nil {
			return err
		}
		after, err = upgraded(s, to.Image)
		return err
	})
	if err != nil {
		log.Warn("operator not upgraded", "error", err.Error())
		return 0, nil, fmt.Errorf("the operator: %w", err)
	}
	down := downSince(s, start)
	l.operatorDown.WithLabelValues(string(a.kind)).Observe(down.Seconds())
	since := l.release()
	if err := l.await(ctx, func(context.Context) error { return quiet(l.reports(), since) }); err != nil {
		log.Warn("instances not converged after the upgrade", "error", err.Error())
		return 0, nil, err
	}
	d := time.Since(start)
	log.Info("operator upgraded", "leader", after.Name, "operator_down_seconds", down.Seconds(), "converge_seconds", d.Seconds())
	return d, nil, nil
}

// tail returns the last n lines of s.
func tail(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.Join(lines[max(0, len(lines)-n):], "\n")
}
