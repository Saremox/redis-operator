package chaos

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// mirrorAnnotation marks the API server's copy of a static pod.
const mirrorAnnotation = "kubernetes.io/config.mirror"

// errBlocked is an eviction a PodDisruptionBudget still blocked at the
// drain timeout.
var errBlocked = errors.New("blocked by a PodDisruptionBudget")

// stays reports whether a drain leaves a pod on its node, as kubectl drain
// does with --ignore-daemonsets: DaemonSet and static pods, and pods that
// are done.
func stays(p *corev1.Pod) bool {
	return p.Annotations[mirrorAnnotation] != "" ||
		p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed ||
		slices.ContainsFunc(p.OwnerReferences, func(o metav1.OwnerReference) bool { return o.Kind == "DaemonSet" })
}

// evictable returns the pods a drain evicts: those it doesn't leave, and
// that aren't going already.
func evictable(pods []corev1.Pod) []corev1.Pod {
	var out []corev1.Pod
	for i := range pods {
		if p := &pods[i]; !stays(p) && p.DeletionTimestamp == nil {
			out = append(out, *p)
		}
	}
	return out
}

// drained holds once no pod but those a drain leaves runs on the node.
func drained(node string, pods []corev1.Pod) error {
	var names []string
	for i := range pods {
		if p := &pods[i]; p.Spec.NodeName == node && !stays(p) {
			names = append(names, p.Namespace+"/"+p.Name)
		}
	}
	if len(names) > 0 {
		return fmt.Errorf("%d pods left on %s: %s", len(names), node, strings.Join(names, ", "))
	}
	return nil
}

// drainable returns the nodes that may be drained: those the selector
// picks that are Ready and schedulable, except the tester's own.
func drainable(nodes []corev1.Node, self string) []string {
	var out []string
	for _, n := range nodes {
		if n.Name == self || n.Spec.Unschedulable {
			continue
		}
		for _, c := range n.Status.Conditions {
			if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
				out = append(out, n.Name)
			}
		}
	}
	slices.Sort(out)
	return out
}

// drain cordons a node and evicts its pods, respecting PodDisruptionBudgets,
// waits until every instance converged on the other nodes, holds the node
// cordoned, uncordons it, and waits until every instance is quiet again.
func (l *Lane) drain(ctx context.Context, a *action) outcome {
	nodes, err := l.kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: l.cfg.Drain.NodeSelector})
	if err != nil {
		return outcome{err: err}
	}
	names := drainable(nodes.Items, l.node)
	if len(names) == 0 {
		return outcome{skip: "no node to drain"}
	}
	node := names[a.r.IntN(len(names))]
	log := a.log.With("node", node)
	a.begin()
	l.lock.Disturb("node drain of " + node)
	defer l.lock.Disturb("")
	start := time.Now()
	if err := l.cordon(ctx, node, true); err != nil {
		return outcome{err: err}
	}
	var o outcome
	blocked, err := l.evictAll(ctx, node, log)
	evicted := time.Since(start)
	switch {
	case errors.Is(err, errBlocked):
		o.timeout = fmt.Errorf("evicting: %w", err)
	case err != nil:
		o.err = err
	default:
		since := time.Now()
		err = l.await(ctx, func(ctx context.Context) error {
			pods, err := l.kube.CoreV1().Pods("").List(ctx, metav1.ListOptions{FieldSelector: "spec.nodeName=" + node})
			if err != nil {
				return err
			}
			if err := drained(node, pods.Items); err != nil {
				return err
			}
			return settled(l.reports(), since)
		})
		if err != nil {
			o.timeout = fmt.Errorf("drained: %w", err)
		} else {
			d := time.Since(start)
			o.converged = append(o.converged, d)
			log.Info("node drained", "evict_seconds", evicted.Seconds(), "converge_seconds", d.Seconds(), "pdb_blocked", blocked,
				"hold_seconds", l.cfg.Drain.Hold.Seconds())
			select {
			case <-ctx.Done():
			case <-time.After(l.cfg.Drain.Hold.Duration):
			}
		}
	}
	if err := l.cordon(context.WithoutCancel(ctx), node, false); err != nil {
		o.err = errors.Join(o.err, err)
		return o
	}
	since := l.release()
	if err := l.await(ctx, func(context.Context) error { return quiet(l.reports(), since) }); err != nil {
		o.timeout = errors.Join(o.timeout, fmt.Errorf("uncordoned: %w", err))
		return o
	}
	log.Info("node uncordoned", "converge_seconds", time.Since(since).Seconds(), "duration_seconds", time.Since(start).Seconds())
	return o
}

func (l *Lane) cordon(ctx context.Context, node string, on bool) error {
	patch := fmt.Appendf(nil, `{"spec":{"unschedulable":%t}}`, on)
	_, err := l.kube.CoreV1().Nodes().Patch(ctx, node, types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}

// evictAll evicts every evictable pod on the node at once, each retried
// while a PodDisruptionBudget blocks it, until the drain timeout. It
// returns how often a budget blocked an eviction.
func (l *Lane) evictAll(ctx context.Context, node string, log *slog.Logger) (int, error) {
	list, err := l.kube.CoreV1().Pods("").List(ctx, metav1.ListOptions{FieldSelector: "spec.nodeName=" + node})
	if err != nil {
		return 0, err
	}
	deadline := time.Now().Add(l.cfg.Drain.Timeout.Duration)
	var mu sync.Mutex
	var errs []error
	blocked := 0
	var wg sync.WaitGroup
	for _, p := range evictable(list.Items) {
		if p.Spec.NodeName != node {
			continue
		}
		wg.Go(func() {
			b, err := l.evict(ctx, p, deadline, log)
			mu.Lock()
			defer mu.Unlock()
			if b {
				blocked++
			}
			if err != nil {
				errs = append(errs, fmt.Errorf("%s/%s: %w", p.Namespace, p.Name, err))
			}
		})
	}
	wg.Wait()
	return blocked, errors.Join(errs...)
}

// evict evicts a pod, retrying while a PodDisruptionBudget blocks it, and
// reports whether one did.
func (l *Lane) evict(ctx context.Context, p corev1.Pod, deadline time.Time, log *slog.Logger) (bool, error) {
	uid := p.UID
	ev := &policyv1.Eviction{
		ObjectMeta:    metav1.ObjectMeta{Name: p.Name, Namespace: p.Namespace},
		DeleteOptions: &metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}},
	}
	var since time.Time
	for {
		err := l.kube.PolicyV1().Evictions(p.Namespace).Evict(ctx, ev)
		switch {
		case err == nil || apierrors.IsNotFound(err) || apierrors.IsConflict(err):
			if !since.IsZero() {
				d := time.Since(since)
				l.evictionBlocked.Observe(d.Seconds())
				log.Info("eviction unblocked", "pod", p.Name, "pod_namespace", p.Namespace, "blocked_seconds", d.Seconds())
			}
			return !since.IsZero(), nil
		case apierrors.IsTooManyRequests(err):
			if since.IsZero() {
				since = time.Now()
				log.Info("eviction blocked", "pod", p.Name, "pod_namespace", p.Namespace, "reason", err.Error())
			}
			if time.Now().After(deadline) {
				d := time.Since(since)
				l.evictionBlocked.Observe(d.Seconds())
				log.Warn("eviction still blocked at the drain timeout", "pod", p.Name, "pod_namespace", p.Namespace, "blocked_seconds", d.Seconds())
				return true, fmt.Errorf("%w for %s: %w", errBlocked, d.Round(time.Second), err)
			}
		default:
			return !since.IsZero(), err
		}
		select {
		case <-ctx.Done():
			return !since.IsZero(), ctx.Err()
		case <-time.After(2 * l.poll):
		}
	}
}
