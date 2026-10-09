package mutator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"

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

// operatorElected is the start of the status message of an operator that
// promotes a replica because no Sentinel can fail over. The operator sets no
// message for its other elections.
const operatorElected = "Sentinel knew no replica to fail over to, the operator promoted "

// The changes of the config-epoch on the Sentinels, as epochChange says.
const (
	epochRose      = "rose"
	epochFell      = "fell"
	epochUnchanged = "unchanged"
	epochMixed     = "mixed"
	epochUnread    = "unread"
)

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
// The action watches the status messages and reads the config-epoch of each
// Sentinel before the reset. The recovery reads the epochs again after the
// convergence.
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
	var messages *statusWatch
	timeout := m.cfg.Timeout(kind, m.observerCfg, s.rf.Spec.Redis.Replicas)
	version := s.rf.ResourceVersion
	return plan{
		kind:   kind,
		params: fmt.Sprintf("SENTINEL RESET * on %d Sentinels, delete pod %s (graceful)", len(sentinels), pod.Name),
		action: func(ctx context.Context) error {
			messages = m.watchStatus(ctx, timeout, version)
			before = m.configEpochs(ctx, sentinels, port)
			if err := m.resetSentinels(ctx, sentinels, port); err != nil {
				return err
			}
			return m.deletePod(ctx, pod.Name, pod.UID, false)
		},
		fetch:     fetchOpts{sentinelMaster: true},
		converged: noMasterConverged(pod.Name, pod.UID, s.rf.Spec.Redis.Replicas, s.rf.Spec.Sentinel.Replicas),
		recovery: func(ctx context.Context) (string, []any) {
			elected := messages.sawPrefix(operatorElected)
			change := epochChange(before, m.configEpochs(ctx, sentinels, port))
			return recoveryPath(elected, change), []any{"config_epoch", change, "operator_message", elected}
		},
	}
}

// statusWatch collects the status messages of the RedisFailover. A watch sees
// each status update. A poll would miss a message that the operator replaces
// within one period.
type statusWatch struct {
	w      watch.Interface
	cancel context.CancelFunc
	done   chan struct{}
	name   string
	// seen belongs to the goroutine until done is closed.
	seen map[string]bool
}

// watchStatus starts to collect the status messages of the instance for at
// most limit. The watch starts at the resource version of the plan, so that a
// message from before the mutation is not an event. It returns nil if the watch
// does not start: the recovery then has no message to look at.
func (m *Mutator) watchStatus(ctx context.Context, limit time.Duration, version string) *statusWatch {
	ctx, cancel := context.WithTimeout(ctx, limit)
	opts := metav1.ListOptions{FieldSelector: "metadata.name=" + m.in.Name, ResourceVersion: version}
	w, err := m.rfs.DatabasesV1().RedisFailovers(m.in.Namespace).Watch(ctx, opts)
	if err != nil {
		cancel()
		return nil
	}
	sw := &statusWatch{w: w, cancel: cancel, done: make(chan struct{}), name: m.in.Name, seen: map[string]bool{}}
	go func() {
		defer close(sw.done)
		// A blocked receive goroutine of the watch ends only with Stop.
		defer w.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case e, ok := <-w.ResultChan():
				if !ok {
					return
				}
				sw.add(e)
			}
		}
	}()
	return sw
}

func (sw *statusWatch) add(e watch.Event) {
	if rf, ok := e.Object.(*redisfailoverv1.RedisFailover); ok && rf.Name == sw.name {
		sw.seen[rf.Status.Message] = true
	}
}

// sawPrefix stops the watch and reports whether a message started with
// prefix. It also reads the events that the watch already holds. A nil watch
// saw nothing.
func (sw *statusWatch) sawPrefix(prefix string) bool {
	if sw == nil {
		return false
	}
	sw.cancel()
	<-sw.done
	for e := range sw.w.ResultChan() {
		sw.add(e)
	}
	for msg := range sw.seen {
		if strings.HasPrefix(msg, prefix) {
			return true
		}
	}
	return false
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

// epochChange tells how the config-epoch changed on the Sentinels between the
// reads before the reset and after the convergence. It is epochUnread if a
// read failed or the Sentinels differ. It is epochMixed if the epoch rose on
// one Sentinel and fell on another.
func epochChange(before, after map[string]int64) string {
	if before == nil || len(before) != len(after) {
		return epochUnread
	}
	var rose, fell bool
	for name, b := range before {
		a, ok := after[name]
		if !ok {
			return epochUnread
		}
		rose = rose || a > b
		fell = fell || a < b
	}
	switch {
	case rose && fell:
		return epochMixed
	case rose:
		return epochRose
	case fell:
		return epochFell
	}
	return epochUnchanged
}

// recoveryPath names the party that made the new master. The operator message
// shows that the operator promoted a replica. Without it, the epoch decides.
// A Sentinel failover raises the config-epoch. The operator monitors the new
// master again on each Sentinel, which sets the epoch to 0. SENTINEL RESET
// keeps the epoch.
//
// The path is unknown if the epoch did not change, if it rose on one Sentinel
// and fell on another, or if a read failed. The operator sets the message
// only if no Sentinel can fail over. Its other elections leave a trace only
// when the epoch was above 0 before.
func recoveryPath(elected bool, change string) string {
	switch {
	case elected, change == epochFell:
		return pathOperator
	case change == epochRose:
		return pathSentinel
	}
	return pathUnknown
}

// recordRecovery counts and logs the recovery path of a converged mutation
// whose plan classifies one. The path never changes the result.
func (m *Mutator) recordRecovery(ctx context.Context, p plan, result string, log *slog.Logger) *slog.Logger {
	if p.recovery == nil || result != resultConverged {
		return log
	}
	path, attrs := p.recovery(ctx)
	m.recoveries.WithLabelValues(path).Inc()
	return log.With(append([]any{"recovery_path", path}, attrs...)...)
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
