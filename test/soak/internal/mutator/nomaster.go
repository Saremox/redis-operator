package mutator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"

	"github.com/redis/go-redis/v9"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/saremox/redis-operator/test/soak/internal/auth"
	"github.com/saremox/redis-operator/test/soak/internal/config"
	"github.com/saremox/redis-operator/test/soak/internal/observer"
)

// The recovery paths of sentinel_reset_kill_master, the values of the path
// label.
const (
	pathSentinel = "sentinel"
	pathOperator = "operator"
	pathUnknown  = "unknown"
)

var recoveryPaths = []string{pathSentinel, pathOperator, pathUnknown}

// planNoMaster resets every Sentinel and then deletes the master pod
// gracefully. Each Sentinel learns its replicas again at its next INFO of the
// master. The recovery is a Sentinel failover or an election by the operator.
// Both steps are one action: a master kill after a failed reset would test
// another case.
//
// If the shutdown script gets no failover, it releases its write pause, and
// the master accepts writes until Redis gets SIGTERM. Redis 7 and later then
// pause writes and wait for the replicas, up to shutdown-timeout. A write is
// lost only if a replica lags, so the mutation does not require a lossless
// result.
//
// The action reads the config-epoch of each Sentinel before the reset. The
// recovery reads it again after the convergence.
func (m *Mutator) planNoMaster(s state, master string, port int) plan {
	const kind = config.SentinelResetKillMaster
	switch {
	case !s.rf.SentinelEnabled():
		return skipped(kind, "Sentinel is off")
	case s.rf.Spec.Redis.Replicas < 2:
		return skipped(kind, "one redis pod has no replica to promote")
	case len(s.sentinels) == 0:
		return skipped(kind, "no sentinel pod")
	}
	pod, why := labelledMaster(s, master)
	if why != "" {
		return skipped(kind, "%s", why)
	}
	sentinels := slices.Clone(s.sentinels)
	var before map[string]int64
	return plan{
		kind:   kind,
		params: fmt.Sprintf("SENTINEL RESET * on %d Sentinels, delete pod %s (graceful)", len(sentinels), pod.Name),
		action: func(ctx context.Context) error {
			before = m.configEpochs(ctx, sentinels, port)
			if err := m.resetSentinels(ctx, sentinels, port); err != nil {
				return err
			}
			return m.deletePod(ctx, pod.Name, pod.UID, false)
		},
		fetch:     fetchOpts{sentinelMaster: true},
		converged: noMasterConverged(pod.Name, pod.UID, s.rf.Spec.Redis.Replicas, s.rf.Spec.Sentinel.Replicas),
		recovery: func(ctx context.Context) string {
			return recoveryPath(before, m.configEpochs(ctx, sentinels, port))
		},
	}
}

// configEpochs returns the config-epoch of the master on each Sentinel, by pod
// name. It returns nil if one Sentinel does not answer.
func (m *Mutator) configEpochs(ctx context.Context, pods []corev1.Pod, port int) map[string]int64 {
	epochs, errs := eachPod(ctx, m, pods, port, auth.Fixed(""), func(ctx context.Context, c *redis.Client) (int64, error) {
		fields, err := observer.SentinelMaster(ctx, c)
		if err != nil {
			return 0, err
		}
		return strconv.ParseInt(fields["config-epoch"], 10, 64)
	})
	if len(errs) > 0 {
		return nil
	}
	return epochs
}

// recoveryPath names the party that made the new master, from the
// config-epoch of each Sentinel before the reset and after the convergence.
// A Sentinel failover raises the epoch. The operator makes each Sentinel
// monitor the new master again, which sets the epoch to 0. SENTINEL RESET
// keeps the epoch.
//
// The path is unknown if the epoch did not change, if it rose on one Sentinel
// and fell on another, or if a read failed. An election by the operator
// leaves no trace when the epoch was 0 before.
func recoveryPath(before, after map[string]int64) string {
	if before == nil || len(before) != len(after) {
		return pathUnknown
	}
	var rose, fell bool
	for name, b := range before {
		a, ok := after[name]
		if !ok {
			return pathUnknown
		}
		rose = rose || a > b
		fell = fell || a < b
	}
	switch {
	case rose && !fell:
		return pathSentinel
	case fell && !rose:
		return pathOperator
	}
	return pathUnknown
}

// recordRecovery counts and logs the recovery path of a converged mutation
// whose plan classifies one. The path never changes the result.
func (m *Mutator) recordRecovery(ctx context.Context, p plan, result string, log *slog.Logger) *slog.Logger {
	if p.recovery == nil || result != resultConverged {
		return log
	}
	path := p.recovery(ctx)
	m.recoveries.WithLabelValues(path).Inc()
	return log.With("recovery_path", path)
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
// its replicas again, which the next failover needs. The RedisFailover must be
// Healthy.
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
