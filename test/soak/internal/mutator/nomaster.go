package mutator

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/redis/go-redis/v9"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/saremox/redis-operator/test/soak/internal/auth"
	"github.com/saremox/redis-operator/test/soak/internal/config"
)

// planNoMaster resets every Sentinel and then deletes the master pod
// gracefully, so that no Sentinel knows a replica when the master is gone.
// Only the operator can then elect a master. Both steps are one action: a
// master kill after a failed reset would test another case.
//
// The shutdown script of the master cannot get a failover. It releases its
// write pause, so the master acknowledges writes until Redis stops and waits
// for its replicas. A write can be lost, as in a failover, and the mutation
// does not require a lossless result.
func (m *Mutator) planNoMaster(s state, master string, port int) plan {
	const kind = config.SentinelResetKillMaster
	switch {
	case !s.rf.SentinelEnabled():
		return skipped(kind, "Sentinel is off")
	case s.rf.Spec.Redis.Replicas < 2:
		return skipped(kind, "one redis pod has no replica to elect")
	case len(s.sentinels) == 0:
		return skipped(kind, "no sentinel pod")
	}
	pod, why := labelledMaster(s, master)
	if why != "" {
		return skipped(kind, "%s", why)
	}
	sentinels := slices.Clone(s.sentinels)
	return plan{
		kind:   kind,
		params: fmt.Sprintf("SENTINEL RESET * on %d Sentinels, delete pod %s (graceful)", len(sentinels), pod.Name),
		action: func(ctx context.Context) error {
			if err := m.resetSentinels(ctx, sentinels, port); err != nil {
				return err
			}
			return m.deletePod(ctx, pod.Name, pod.UID, false)
		},
		fetch:     fetchOpts{sentinelMaster: true},
		converged: noMasterConverged(pod.Name, pod.UID, s.rf.Spec.Redis.Replicas, s.rf.Spec.Sentinel.Replicas),
	}
}

// resetSentinels sends SENTINEL RESET * to every Sentinel. It fails if one
// Sentinel does not accept it, because that Sentinel can still fail over.
func (m *Mutator) resetSentinels(ctx context.Context, pods []corev1.Pod, port int) error {
	_, errs := eachPod(ctx, m, pods, port, auth.Fixed(""), func(ctx context.Context, c *redis.Client) (int64, error) {
		return c.Do(ctx, "SENTINEL", "RESET", "*").Int64()
	})
	var out []error
	for _, p := range pods {
		if err, ok := errs[p.Name]; ok {
			out = append(out, fmt.Errorf("SENTINEL RESET on %s: %w", p.Name, err))
		}
	}
	return errors.Join(out...)
}

// noMasterConverged holds once the killed master pod is back as a new, ready
// pod, one pod is labelled master, and each Sentinel knows the master and all
// its replicas again. The Sentinel check shows that the operator healed the
// Sentinels, which the next failover needs. The RedisFailover must be Healthy.
func noMasterConverged(name string, uid types.UID, redis, sentinels int32) func(state) error {
	replaced := redisPodReplaced(name, uid)
	return func(s state) error {
		if err := replaced(s); err != nil {
			return err
		}
		if err := podsReady("redis", s.redis, redis); err != nil {
			return err
		}
		if err := podsReady("sentinel", s.sentinels, sentinels); err != nil {
			return err
		}
		if n := len(withRole(s.redis, roleMaster)); n != 1 {
			return fmt.Errorf("%d pods are labelled master", n)
		}
		if err := sentinelsMonitor(s, redis); err != nil {
			return err
		}
		return healthy(s.rf)
	}
}
