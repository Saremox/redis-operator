package mutator

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/test/soak/internal/maxmem"
	"github.com/saremox/redis-operator/test/soak/internal/observer"
)

// state is what planning a mutation and judging its convergence look at.
type state struct {
	// rf has the operator's defaults applied.
	rf        *redisfailoverv1.RedisFailover
	sts       *appsv1.StatefulSet
	sentinel  *appsv1.Deployment
	redis     []corev1.Pod
	sentinels []corev1.Pod
	// config is every redis pod's maxmemory settings, for instances with
	// maxMemory.
	config []maxmem.Pod
	// password is the Secret's current password, and authSecret the Secret
	// auth_add creates.
	password   string
	authSecret string
	// auth is whether each redis pod accepts the password fetchOpts
	// names.
	auth map[string]error
	// sentinelMasters is every Sentinel's SENTINEL MASTER mymaster.
	sentinelMasters map[string]sentinelMaster
	// sentinelService and sentinelConfigMap tell whether they exist.
	sentinelService   bool
	sentinelConfigMap bool
	// servers is what every redis and Sentinel pod reports in INFO, by pod
	// name.
	servers map[string]server
}

type sentinelMaster struct {
	fields map[string]string
	err    error
}

// redisReplicasConverged holds when the StatefulSet runs want ready pods on
// its update revision. It does not compare currentRevision, because the
// operator StatefulSets use OnDelete, and for OnDelete the controller never
// advances currentRevision.
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

// memoryConverged holds when each redis pod runs the new memory and the
// operator applied maxmemory. Without an allkeys-* policy, the operator can
// instead keep maxmemory when the target is below the data: then the status
// says so, and no pod shrinks.
func memoryConverged(want corev1.ResourceRequirements, replicas int32, before map[string]int64, target int64, evicts bool) func(state) error {
	applied := resourcesConverged(want, replicas)
	return func(s state) error {
		if err := maxmem.Check(s.rf, s.rf.Status.Message, s.config); err != nil {
			return err
		}
		err := applied(s)
		if err == nil || evicts {
			return err
		}
		if !maxmem.KeptBelowData(s.rf.Status.Message, target) {
			return fmt.Errorf("%w, and the status doesn't keep maxmemory above %s: %q", err, maxmem.FormatBytes(target), s.rf.Status.Message)
		}
		if err := podsReady("redis", s.redis, replicas); err != nil {
			return err
		}
		for i := range s.redis {
			p := &s.redis[i]
			if l := maxmem.RedisLimit(p); l < before[p.Name] {
				return fmt.Errorf("%s shrunk to %s while maxmemory is kept", p.Name, maxmem.FormatBytes(l))
			}
		}
		return nil
	}
}

// maxMemoryConverged holds once the spec has the change and every redis pod
// runs with the maxmemory settings the operator sets for it.
func maxMemoryConverged(replicas int32, changed func(*redisfailoverv1.MaxMemorySettings) bool) func(state) error {
	return func(s state) error {
		if mm := s.rf.Spec.Redis.MaxMemory; mm == nil || !changed(mm) {
			return errors.New("the RedisFailover doesn't have the change")
		}
		if err := podsReady("redis", s.redis, replicas); err != nil {
			return err
		}
		return maxmem.Check(s.rf, s.rf.Status.Message, s.config)
	}
}

// appliedResources returns the redis container resources that the kubelet
// reports. If the kubelet does not report them and no resize is in progress,
// it returns the resources of the pod spec.
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
			case !observer.Ready(p):
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
		if !observer.Ready(&pods[i]) {
			return fmt.Errorf("%s not ready", pods[i].Name)
		}
	}
	return nil
}

// passwordAccepted holds once every redis pod accepts the password fetched
// for.
func passwordAccepted(s state) error {
	if s.auth == nil {
		return errors.New("the password wasn't checked")
	}
	var errs []error
	for _, p := range s.redis {
		if err, ok := s.auth[p.Name]; !ok || err != nil {
			errs = append(errs, fmt.Errorf("%s refuses the password: %v", p.Name, err))
		}
	}
	return errors.Join(errs...)
}

// authConverged holds when each redis pod accepts the new password and rolled
// onto the changed Secret, each Sentinel monitors the master without problems,
// and the RedisFailover is Healthy. before is the secret checksum of the pod
// template before the change.
func authConverged(before string, replicas int32) func(state) error {
	return func(s state) error {
		if err := passwordAccepted(s); err != nil {
			return err
		}
		sum := templateChecksum(s)
		if sum == before {
			return errors.New("the redis pod template's secret checksum hasn't changed yet")
		}
		if err := statefulSetConverged(s.sts, replicas); err != nil {
			return err
		}
		if err := podsReady("redis", s.redis, replicas); err != nil {
			return err
		}
		for _, p := range s.redis {
			if p.Annotations[secretChecksum] != sum {
				return fmt.Errorf("%s hasn't rolled onto the secret yet", p.Name)
			}
		}
		if s.rf.SentinelEnabled() {
			if err := sentinelsMonitor(s, replicas); err != nil {
				return err
			}
		}
		return healthy(s.rf)
	}
}

// sentinelsMonitor holds when each Sentinel sees the master up and all its
// replicas. A Sentinel cannot do this without the password.
func sentinelsMonitor(s state, replicas int32) error {
	var errs []error
	for _, p := range s.sentinels {
		m, ok := s.sentinelMasters[p.Name]
		switch {
		case !ok:
			errs = append(errs, fmt.Errorf("%s: SENTINEL MASTER not read", p.Name))
		case m.err != nil:
			errs = append(errs, fmt.Errorf("%s: %w", p.Name, m.err))
		default:
			flags := strings.Split(m.fields["flags"], ",")
			for _, f := range []string{"s_down", "o_down", "disconnected"} {
				if slices.Contains(flags, f) {
					errs = append(errs, fmt.Errorf("%s flags the master %s", p.Name, m.fields["flags"]))
					break
				}
			}
			if n := m.fields["num-slaves"]; n != strconv.Itoa(int(replicas-1)) {
				errs = append(errs, fmt.Errorf("%s knows %s replicas, want %d", p.Name, n, replicas-1))
			}
		}
	}
	return errors.Join(errs...)
}

// toggleConverged holds once the Sentinel Deployment, Service and ConfigMap
// exist and run, or are gone, as the mode asks, and the RedisFailover is
// Healthy.
func toggleConverged(sentinel bool, sentinels, replicas int32) func(state) error {
	running := sentinelReplicasConverged(sentinels)
	return func(s state) error {
		if s.rf.SentinelEnabled() != sentinel {
			return errors.New("the RedisFailover doesn't have the change")
		}
		if err := podsReady("redis", s.redis, replicas); err != nil {
			return err
		}
		if sentinel {
			if err := running(s); err != nil {
				return err
			}
			if !s.sentinelService || !s.sentinelConfigMap {
				return fmt.Errorf("the Sentinel Service exists: %t, its ConfigMap exists: %t", s.sentinelService, s.sentinelConfigMap)
			}
		} else if s.sentinel != nil || s.sentinelService || s.sentinelConfigMap || len(s.sentinels) > 0 {
			return fmt.Errorf("the Sentinel Deployment exists: %t, its Service exists: %t, its ConfigMap exists: %t, %d Sentinel pods",
				s.sentinel != nil, s.sentinelService, s.sentinelConfigMap, len(s.sentinels))
		}
		return healthy(s.rf)
	}
}
