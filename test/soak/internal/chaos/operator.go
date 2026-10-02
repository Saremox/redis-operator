package chaos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/saremox/redis-operator/test/soak/internal/config"
)

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
	s.lease, err = l.kube.CoordinationV1().Leases(ns).Get(ctx, l.operator.Lease, metav1.GetOptions{})
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
	if !ready(p) {
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

func ready(p *corev1.Pod) bool {
	if p.DeletionTimestamp != nil {
		return false
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
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
// configured --set values.
func helmArgs(u config.Upgrade, namespace string, to config.OperatorVersion) []string {
	args := []string{"upgrade", u.Release, to.Chart, "--namespace", namespace, "--reset-then-reuse-values",
		"--set", "image.repository=" + to.Repository(), "--set", "image.tag=" + to.Tag(),
		"--wait", "--timeout", u.Timeout.Duration.String()}
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
	s, err := l.operatorState(ctx)
	if err != nil {
		return 0, err, nil
	}
	old, _ := leader(s)
	crd := l.crdVersion(ctx)
	l.lock.Disturb("operator upgrade to " + to.Name)
	defer l.lock.Disturb("")
	start := time.Now()
	wctx, stop := context.WithCancel(ctx)
	w := &watch{}
	var wg sync.WaitGroup
	wg.Go(func() { l.watchUpgrade(wctx, w, old) })
	out, helmErr := l.helm(ctx, helmArgs(l.cfg.Upgrade, l.operator.Namespace, to)...)
	helmDone := time.Now()
	stop()
	wg.Wait()
	hook := w.hook(helmErr == nil)
	log = log.With("helm_seconds", helmDone.Sub(start).Seconds(), "hook", hook.result, "hook_seconds", hook.duration.Seconds(),
		"crd_before", crd, "crd_after", l.crdVersion(ctx))
	if helmErr != nil {
		log.Warn("helm upgrade failed", "error", helmErr.Error(), "output", tail(string(out), 20))
		return 0, fmt.Errorf("helm upgrade: %w", helmErr), nil
	}
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
	stopped := start
	if !w.leaderStopped.IsZero() {
		stopped = w.leaderStopped
	}
	down := downSince(s, stopped)
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

// crdVersion describes the CRD's generation and resourceVersion, to tell
// whether the hook changed it.
func (l *Lane) crdVersion(ctx context.Context) string {
	rc := l.kube.Discovery().RESTClient()
	if rc == nil {
		return "unknown"
	}
	raw, err := rc.Get().AbsPath("/apis/apiextensions.k8s.io/v1/customresourcedefinitions", l.cfg.Upgrade.CRD).
		DoRaw(ctx)
	var crd metav1.PartialObjectMetadata
	if err == nil {
		err = json.Unmarshal(raw, &crd)
	}
	if err != nil {
		return "unknown: " + err.Error()
	}
	return fmt.Sprintf("generation %d, resourceVersion %s", crd.Generation, crd.ResourceVersion)
}

// watch is what watchUpgrade saw of the CRD hook Job and the old leader.
type watch struct {
	mu            sync.Mutex
	created       time.Time
	completed     time.Time
	failed        bool
	gone          time.Time
	leaderStopped time.Time
}

type hookResult struct {
	result   string
	duration time.Duration
}

// hook judges the CRD hook from what watchUpgrade saw: helm deletes the hook
// Job when it succeeds, and fails the upgrade when it fails.
func (w *watch) hook(helmOK bool) hookResult {
	w.mu.Lock()
	defer w.mu.Unlock()
	end := w.completed
	if end.IsZero() {
		end = w.gone
	}
	switch {
	case w.created.IsZero():
		return hookResult{result: "not seen"}
	case w.failed:
		return hookResult{result: "failed", duration: end.Sub(w.created)}
	case !w.completed.IsZero() || helmOK && !w.gone.IsZero():
		return hookResult{result: "succeeded", duration: end.Sub(w.created)}
	}
	return hookResult{result: "unknown"}
}

// watchUpgrade follows the CRD hook Job and the old leader's deletion
// while helm runs.
func (l *Lane) watchUpgrade(ctx context.Context, w *watch, old *corev1.Pod) {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	ns := l.operator.Namespace
	for {
		job, err := l.kube.BatchV1().Jobs(ns).Get(ctx, l.cfg.Upgrade.HookJob, metav1.GetOptions{})
		w.mu.Lock()
		switch {
		case err == nil:
			if w.created.IsZero() {
				w.created = job.CreationTimestamp.Time
			}
			for _, c := range job.Status.Conditions {
				if c.Status != corev1.ConditionTrue {
					continue
				}
				switch c.Type {
				case batchv1.JobComplete:
					w.completed = c.LastTransitionTime.Time
				case batchv1.JobFailed:
					w.failed = true
				}
			}
		case apierrors.IsNotFound(err) && !w.created.IsZero() && w.gone.IsZero():
			w.gone = time.Now()
		}
		w.mu.Unlock()
		if old != nil && w.leaderStopped.IsZero() {
			p, err := l.kube.CoreV1().Pods(ns).Get(ctx, old.Name, metav1.GetOptions{})
			switch {
			case err == nil && p.UID == old.UID && p.DeletionTimestamp != nil:
				w.leaderStopped = deletedAt(p)
			case apierrors.IsNotFound(err) || err == nil && p.UID != old.UID:
				w.leaderStopped = time.Now()
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// deletedAt returns when a pod was deleted: its deletionTimestamp is when
// its grace period ends.
func deletedAt(p *corev1.Pod) time.Time {
	t := p.DeletionTimestamp.Time
	if p.DeletionGracePeriodSeconds != nil {
		t = t.Add(-time.Duration(*p.DeletionGracePeriodSeconds) * time.Second)
	}
	return t
}

// tail returns the last n lines of s.
func tail(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.Join(lines[max(0, len(lines)-n):], "\n")
}
