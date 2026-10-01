package mutator

import (
	"errors"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
)

// state is what planning a mutation and judging its convergence look at.
type state struct {
	// rf has the operator's defaults applied.
	rf        *redisfailoverv1.RedisFailover
	sts       *appsv1.StatefulSet
	sentinel  *appsv1.Deployment
	redis     []corev1.Pod
	sentinels []corev1.Pod
}

// redisReplicasConverged holds once the StatefulSet runs want ready pods on
// its update revision. Its currentRevision isn't compared: the operator's
// StatefulSets use OnDelete, for which the controller never advances it.
func redisReplicasConverged(want int32) func(state) error {
	return func(s state) error {
		if err := statefulSetConverged(s.sts, want); err != nil {
			return err
		}
		return podsReady("redis", s.redis, want)
	}
}

func statefulSetConverged(sts *appsv1.StatefulSet, want int32) error {
	switch {
	case sts == nil:
		return errors.New("no redis StatefulSet")
	case sts.Spec.Replicas == nil || *sts.Spec.Replicas != want:
		return fmt.Errorf("redis StatefulSet spec.replicas is not %d yet", want)
	case sts.Status.ObservedGeneration < sts.Generation:
		return errors.New("redis StatefulSet change not observed yet")
	case sts.Status.Replicas != want || sts.Status.ReadyReplicas != want || sts.Status.UpdatedReplicas != want:
		return fmt.Errorf("redis StatefulSet has %d replicas, %d ready, %d updated, want %d",
			sts.Status.Replicas, sts.Status.ReadyReplicas, sts.Status.UpdatedReplicas, want)
	}
	return nil
}

// sentinelReplicasConverged holds once the Sentinel Deployment runs want
// ready, updated and available pods and nothing else.
func sentinelReplicasConverged(want int32) func(state) error {
	return func(s state) error {
		d := s.sentinel
		switch {
		case d == nil:
			return errors.New("no Sentinel Deployment")
		case d.Spec.Replicas == nil || *d.Spec.Replicas != want:
			return fmt.Errorf("sentinel Deployment spec.replicas is not %d yet", want)
		case d.Status.ObservedGeneration < d.Generation:
			return errors.New("sentinel Deployment change not observed yet")
		case d.Status.Replicas != want || d.Status.ReadyReplicas != want ||
			d.Status.UpdatedReplicas != want || d.Status.AvailableReplicas != want:
			return fmt.Errorf("sentinel Deployment has %d replicas, %d ready, %d updated, %d available, want %d",
				d.Status.Replicas, d.Status.ReadyReplicas, d.Status.UpdatedReplicas, d.Status.AvailableReplicas, want)
		}
		return podsReady("sentinel", s.sentinels, want)
	}
}

// resourcesConverged holds once every redis pod runs with the wanted cpu
// and memory, and no in-place resize is pending.
func resourcesConverged(want corev1.ResourceRequirements, replicas int32) func(state) error {
	return func(s state) error {
		if err := statefulSetConverged(s.sts, replicas); err != nil {
			return err
		}
		if err := podsReady("redis", s.redis, replicas); err != nil {
			return err
		}
		var errs []error
		for i := range s.redis {
			p := &s.redis[i]
			if p.Annotations[resizeRequest] != "" {
				errs = append(errs, fmt.Errorf("%s: resize requested at %s", p.Name, p.Annotations[resizeRequest]))
				continue
			}
			applied, err := appliedResources(p)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", p.Name, err))
				continue
			}
			if d := resourceDiff(applied, want); d != "" {
				errs = append(errs, fmt.Errorf("%s: %s", p.Name, d))
			}
		}
		return errors.Join(errs...)
	}
}

// appliedResources returns the redis container's resources as the kubelet
// reports them, or the pod spec's when it doesn't report them and no resize
// is in flight.
func appliedResources(p *corev1.Pod) (corev1.ResourceRequirements, error) {
	for _, cs := range p.Status.ContainerStatuses {
		if cs.Name == redisName && cs.Resources != nil {
			return *cs.Resources, nil
		}
	}
	for _, c := range p.Status.Conditions {
		if (c.Type == corev1.PodResizePending || c.Type == corev1.PodResizeInProgress) && c.Status == corev1.ConditionTrue {
			return corev1.ResourceRequirements{}, fmt.Errorf("%s", c.Type)
		}
	}
	for _, c := range p.Spec.Containers {
		if c.Name == redisName {
			return c.Resources, nil
		}
	}
	return corev1.ResourceRequirements{}, errors.New("no redis container")
}

// resourceDiff describes how the cpu and memory requests and limits that
// want sets differ from got.
func resourceDiff(got, want corev1.ResourceRequirements) string {
	for _, l := range []struct {
		name      string
		got, want corev1.ResourceList
	}{{"requests", got.Requests, want.Requests}, {"limits", got.Limits, want.Limits}} {
		for _, n := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
			w, ok := l.want[n]
			if !ok {
				continue
			}
			if g, ok := l.got[n]; !ok || !g.Equal(w) {
				return fmt.Sprintf("%s.%s is %s, want %s", l.name, n, g.String(), w.String())
			}
		}
	}
	return ""
}

// redisPodReplaced holds once the killed redis pod is back under its name
// as a new, ready pod.
func redisPodReplaced(name string, uid types.UID) func(state) error {
	return func(s state) error {
		for i := range s.redis {
			p := &s.redis[i]
			if p.Name != name {
				continue
			}
			switch {
			case p.UID == uid:
				return fmt.Errorf("%s not deleted yet", name)
			case !ready(p):
				return fmt.Errorf("%s not ready", name)
			}
			return nil
		}
		return fmt.Errorf("%s not recreated yet", name)
	}
}

// sentinelPodReplaced holds once the killed Sentinel pod is gone and the
// Deployment is back to want ready pods.
func sentinelPodReplaced(uid types.UID, want int32) func(state) error {
	converged := sentinelReplicasConverged(want)
	return func(s state) error {
		for _, p := range s.sentinels {
			if p.UID == uid {
				return fmt.Errorf("%s not deleted yet", p.Name)
			}
		}
		return converged(s)
	}
}

func podsReady(kind string, pods []corev1.Pod, want int32) error {
	if len(pods) != int(want) {
		return fmt.Errorf("%d %s pods, want %d", len(pods), kind, want)
	}
	for i := range pods {
		if !ready(&pods[i]) {
			return fmt.Errorf("%s not ready", pods[i].Name)
		}
	}
	return nil
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
