package redisfailover

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/saremox/redis-operator/service/k8s"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
	"github.com/saremox/redis-operator/service/redis"
)

// CheckAndHeal checks the state of the RedisFailover and heals each check
// that fails. It writes the status one time, at the end.
func (r *RedisFailoverHandler) CheckAndHeal(rf *redisfailoverv1.RedisFailover) error {
	defer r.startSentinelMasterRound(failoverKey(rf))()
	return r.checkAndHeal(context.Background(), rf)
}

// checkAndHeal is CheckAndHeal with the context for the status write.
func (r *RedisFailoverHandler) checkAndHeal(ctx context.Context, rf *redisfailoverv1.RedisFailover) error {

	observed := rf.Status
	r.autoscaleRollout.Delete(failoverKey(rf))

	rf.Status = redisfailoverv1.RedisFailoverStatus{
		State: redisfailoverv1.HealthyState,
	}

	// A panic ends the reconcile with an error, so the status and cluster_ok must not stay healthy.
	defer func() {
		if p := recover(); p != nil {
			rf.Status = redisfailoverv1.RedisFailoverStatus{
				State:       redisfailoverv1.NotHealthyState,
				Message:     "internal error, see the operator log",
				Autoscaling: observed.Autoscaling,
			}
			updateStatus(ctx, r.k8sservice, rf, observed)
			r.mClient.SetClusterError(rf.Namespace, rf.Name)
			panic(p)
		}
		// The checks replace rf.Status as a whole, so the autoscaler sets
		// its part last.
		r.autoscale(rf, observed.Autoscaling)
		updateStatus(ctx, r.k8sservice, rf, observed)
	}()

	// Every check below authenticates, so a changed password goes first.
	if err := r.applyPassword(rf); err != nil {
		rf.Status = redisfailoverv1.RedisFailoverStatus{
			State:   redisfailoverv1.NotHealthyState,
			Message: "unable to apply the configured password",
		}
		return err
	}

	// The password skips the pods that do not run, so a stale Pending pod
	// does not hold it.
	allRunning := r.redisPodsRunning(rf)

	if rf.Bootstrapping() || rf.OperatorManagedFailover() {
		r.skipSentinelAddrs(rf)
	}
	if rf.Bootstrapping() {
		return r.checkAndHealBootstrapMode(rf, allRunning)
	}

	if rf.OperatorManagedFailover() {
		return r.checkAndHealOperatorManagedMode(rf, allRunning)
	}

	return r.checkAndHealSentinelMode(rf, allRunning)
}

// redisQuorumRunning reports whether a quorum (majority) of the Redis pods
// runs. A quorum of Running pods is sufficient to heal. One Pending pod, for
// example from an unschedulable affinity or a lost zone, must not block the
// election and the Sentinel configuration. The heal steps use only the Running
// pods. With all pods running, a quorum runs.
func (r *RedisFailoverHandler) redisQuorumRunning(rf *redisfailoverv1.RedisFailover, allRunning bool) bool {
	if allRunning || r.rfChecker.IsRedisRunningQuorum(rf) {
		return true
	}
	errorMsg := "redis quorum not running"
	rf.Status = redisfailoverv1.RedisFailoverStatus{
		State:   redisfailoverv1.NotHealthyState,
		Message: errorMsg,
	}
	setRedisCheckerMetrics(r.mClient, "redis", rf.Namespace, rf.Name, metrics.REDIS_REPLICA_MISMATCH, metrics.NOT_APPLICABLE, errors.New(errorMsg))
	r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Debugf("Redis quorum not running, waiting for redis statefulset reconcile")
	return false
}

// applyConfigAndRollout applies the custom Redis config and maxmemory, and
// then the Redis pod rollout. rolloutMsg is the status message after an
// error of the rollout.
func (r *RedisFailoverHandler) applyConfigAndRollout(rf *redisfailoverv1.RedisFailover, master, rolloutMsg string) error {
	if rf.AutoscalingEnabled() {
		r.autoscaleMasters.Store(failoverKey(rf), master)
	}
	err := r.applyRedisCustomConfig(rf)
	var holdRollout bool
	if err == nil {
		holdRollout, err = r.ensureRedisMaxMemory(rf, master)
	}
	setRedisCheckerMetrics(r.mClient, "redis", rf.Namespace, rf.Name, metrics.APPLY_REDIS_CONFIG, metrics.NOT_APPLICABLE, err)
	if err != nil {
		rf.Status = redisfailoverv1.RedisFailoverStatus{
			State:   redisfailoverv1.NotHealthyState,
			Message: "unable to apply custom config",
		}
		return err
	}

	if !holdRollout {
		err = r.UpdateRedisesPods(rf)
		if err != nil {
			rf.Status = redisfailoverv1.RedisFailoverStatus{
				State:   redisfailoverv1.NotHealthyState,
				Message: rolloutMsg,
			}
			return err
		}
	}
	return nil
}

func (r *RedisFailoverHandler) checkAndHealBootstrapMode(rf *redisfailoverv1.RedisFailover, allRunning bool) error {

	if !allRunning {
		errorMsg := "not all replicas running"
		rf.Status = redisfailoverv1.RedisFailoverStatus{
			State:   redisfailoverv1.NotHealthyState,
			Message: errorMsg,
		}
		setRedisCheckerMetrics(r.mClient, "redis", rf.Namespace, rf.Name, metrics.REDIS_REPLICA_MISMATCH, metrics.NOT_APPLICABLE, errors.New(errorMsg))
		r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Debugf("Number of redis mismatch, waiting for redis statefulset reconcile")
		return nil
	}

	// Before UpdateRedisesPods, so a lowered memory limit can hold the rollout.
	holdRollout, err := r.ensureRedisMaxMemory(rf, "")
	if err != nil {
		setRedisCheckerMetrics(r.mClient, "redis", rf.Namespace, rf.Name, metrics.APPLY_REDIS_CONFIG, metrics.NOT_APPLICABLE, err)
		rf.Status = redisfailoverv1.RedisFailoverStatus{
			State:   redisfailoverv1.NotHealthyState,
			Message: "unable to set Redis maxmemory",
		}
		return err
	}
	if !holdRollout {
		err = r.UpdateRedisesPods(rf)
		if err != nil {
			rf.Status = redisfailoverv1.RedisFailoverStatus{
				State:   redisfailoverv1.NotHealthyState,
				Message: "unable to update Redis PODs",
			}
			return err
		}
	}
	err = r.applyRedisCustomConfig(rf)
	setRedisCheckerMetrics(r.mClient, "redis", rf.Namespace, rf.Name, metrics.APPLY_REDIS_CONFIG, metrics.NOT_APPLICABLE, err)
	if err != nil {
		rf.Status = redisfailoverv1.RedisFailoverStatus{
			State:   redisfailoverv1.NotHealthyState,
			Message: "unable to set Redis custom config",
		}
		return err
	}

	bootstrapSettings := rf.Spec.BootstrapNode
	err = r.rfHealer.SetExternalMasterOnAll(bootstrapSettings.Host, bootstrapSettings.Port, rf)
	setRedisCheckerMetrics(r.mClient, "redis", rf.Namespace, rf.Name, metrics.APPLY_EXTERNAL_MASTER, metrics.NOT_APPLICABLE, err)
	if err != nil {
		rf.Status = redisfailoverv1.RedisFailoverStatus{
			State:   redisfailoverv1.NotHealthyState,
			Message: "unable to set external master to all",
		}
		return err
	}

	if rf.SentinelsAllowed() {
		return r.healBootstrapSentinels(rf, bootstrapSettings)
	}
	return nil
}

// healBootstrapSentinels makes each Sentinel monitor the bootstrap node, and
// then heals the Sentinels.
func (r *RedisFailoverHandler) healBootstrapSentinels(rf *redisfailoverv1.RedisFailover, bootstrapSettings *redisfailoverv1.BootstrapSettings) error {
	if !r.rfChecker.IsSentinelRunning(rf) {
		errorMsg := "not all replicas running"
		rf.Status = redisfailoverv1.RedisFailoverStatus{
			State:   redisfailoverv1.NotHealthyState,
			Message: errorMsg,
		}
		setRedisCheckerMetrics(r.mClient, "sentinel", rf.Namespace, rf.Name, metrics.SENTINEL_REPLICA_MISMATCH, metrics.NOT_APPLICABLE, errors.New(errorMsg))
		r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Debugf("Number of sentinel mismatch, waiting for sentinel deployment reconcile")
		return nil
	}

	sentinels, err := r.rfChecker.GetSentinelsIPs(rf)
	if err != nil {
		rf.Status = redisfailoverv1.RedisFailoverStatus{
			State:   redisfailoverv1.NotHealthyState,
			Message: "unable to get sentinels IPs",
		}
		return err
	}
	heal := r.startSentinelHeal(rf)
	defer r.saveSentinelResets(heal, sentinels)
	for _, sip := range sentinels {
		err = r.rfChecker.CheckSentinelMonitor(sip, bootstrapSettings.Host, bootstrapSettings.Port)
		setRedisCheckerMetrics(r.mClient, "sentinel", rf.Namespace, rf.Name, metrics.SENTINEL_WRONG_MASTER, sip, err)
		if heal.skip(sip, err) {
			continue
		}
		if err != nil {
			r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Warningf("Fixing sentinel not monitoring expected master: %s", err.Error())
			if !r.sentinelChangeAllowed(rf, sentinels, heal, sip) {
				continue
			}
			if err := r.rfHealer.NewSentinelMonitorWithPort(sip, bootstrapSettings.Host, bootstrapSettings.Port, rf); err != nil {
				if heal.skip(sip, err) {
					continue
				}
				rf.Status = redisfailoverv1.RedisFailoverStatus{
					State:   redisfailoverv1.NotHealthyState,
					Message: "unable to check sentinel monitor",
				}
				return err
			}
			heal.resets[sip] = r.now()
		}
	}
	return r.checkAndHealSentinels(rf, sentinels, heal)
}

func (r *RedisFailoverHandler) applyRedisCustomConfig(rf *redisfailoverv1.RedisFailover) error {
	redises, err := r.rfChecker.GetRedisesIPs(rf)
	if err != nil {
		return err
	}
	for _, rip := range redises {
		if err := r.rfHealer.SetRedisCustomConfig(rip, rf); err != nil {
			// Skip a pod on a lost node, so that the reachable pods and the
			// other heal steps still run. Other errors, for example a bad config
			// value or a wrong password, stop the reconcile.
			if redis.IsUnreachableError(err) {
				r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Warningf("Skipping custom config on unreachable redis %s: %s", rip, err.Error())
				continue
			}
			return err
		}
	}
	return nil
}

// ensureRedisMaxMemory applies the managed maxmemory. master is "" when
// bootstrapping. It reports whether the pod rollout must be held.
func (r *RedisFailoverHandler) ensureRedisMaxMemory(rf *redisfailoverv1.RedisFailover, master string) (bool, error) {
	if rf.Spec.Redis.MaxMemory == nil {
		// The message and the hold end with the setting.
		r.logMaxMemoryOutcome(r.logger.WithField("redisfailover", rf.Name).WithField("namespace", rf.Namespace), failoverKey(rf), rfservice.MaxMemoryResult{})
		return false, nil
	}
	redises, err := r.rfChecker.GetRedisesIPs(rf)
	if err != nil {
		return false, err
	}
	logger := r.logger.WithField("redisfailover", rf.Name).WithField("namespace", rf.Namespace)
	if master != "" {
		// A failover earlier in this reconcile would make the check run against
		// a replica. Without a master nothing is checked, so hold the rollout.
		if master, err = r.rfChecker.GetMasterIP(rf); err != nil {
			logger.Warningf("Skipping maxmemory and the pod rollout, unable to resolve the master: %s", err.Error())
			return true, nil
		}
	}
	result, err := r.rfHealer.EnsureRedisMaxMemory(rf, master, redises)
	if err != nil {
		return false, err
	}
	if result.Message != "" {
		rf.Status.Message = result.Message
	}
	r.logMaxMemoryOutcome(logger, failoverKey(rf), result)
	return result.HoldRollout, nil
}

// maxMemoryOutcome is the part of a MaxMemoryResult that the handler logs.
type maxMemoryOutcome struct {
	message string
	hold    bool
}

// logMaxMemoryOutcome logs an outcome at Warning when it differs from the last
// one that was logged for the RedisFailover, and at Debug otherwise. The status
// message shows an unchanged outcome to the user. The end of a message or of
// a hold logs at Info, so that the log shows how long it lasted.
func (r *RedisFailoverHandler) logMaxMemoryOutcome(logger log.Logger, key string, result rfservice.MaxMemoryResult) {
	outcome := maxMemoryOutcome{message: result.Message, hold: result.HoldRollout}
	if outcome == (maxMemoryOutcome{}) {
		if last, loaded := r.maxMemoryLogged.LoadAndDelete(key); loaded {
			logMaxMemoryOutcomeEnd(logger, last, outcome)
		}
		return
	}
	last, loaded := r.maxMemoryLogged.Swap(key, outcome)
	if loaded {
		logMaxMemoryOutcomeEnd(logger, last, outcome)
	}
	logf := logger.Warningf
	if loaded && last == outcome {
		logf = logger.Debugf
	}
	if result.Message != "" {
		logf("%s", result.Message)
	}
	if result.HoldRollout {
		logf("Holding the pod rollout until maxmemory fits the lowered memory limit")
	}
}

// logMaxMemoryOutcomeEnd logs each part of the last outcome that current
// does not have. A changed message is a Warning of the new outcome.
func logMaxMemoryOutcomeEnd(logger log.Logger, last any, current maxMemoryOutcome) {
	previous, ok := last.(maxMemoryOutcome)
	if !ok {
		return
	}
	if previous.message != "" && current.message == "" {
		logger.Infof("The maxmemory message ended: %s", previous.message)
	}
	if previous.hold && !current.hold {
		logger.Infof("The maxmemory hold of the pod rollout ended")
	}
}

func getRedisPort(p int32) string {
	return strconv.Itoa(int(p))
}

func setRedisCheckerMetrics(metricsClient metrics.Recorder, mode /* redis or sentinel? */, rfNamespace, rfName, property, IP string, err error) {
	switch mode {
	case "sentinel":
		if err != nil {
			metricsClient.RecordSentinelCheck(rfNamespace, rfName, property, IP, metrics.STATUS_UNHEALTHY)
		} else {
			metricsClient.RecordSentinelCheck(rfNamespace, rfName, property, IP, metrics.STATUS_HEALTHY)
		}
	case "redis":
		if err != nil {
			metricsClient.RecordRedisCheck(rfNamespace, rfName, property, IP, metrics.STATUS_UNHEALTHY)
		} else {
			metricsClient.RecordRedisCheck(rfNamespace, rfName, property, IP, metrics.STATUS_HEALTHY)
		}
	}
}

// updateStatus is the only status write of a reconcile. The checks set only
// State and Message, so LastChanged comes from the observed status unless the
// state changed. An unchanged status is not written, which saves one API call
// on each resync and pod event.
func updateStatus(ctx context.Context, k8sservice k8s.Services, rf *redisfailoverv1.RedisFailover, observed redisfailoverv1.RedisFailoverStatus) {
	if observed.State != rf.Status.State {
		rf.Status.LastChanged = time.Now().Format(time.RFC3339)
	} else {
		rf.Status.LastChanged = observed.LastChanged
	}
	if rf.Status == observed {
		return
	}
	k8sservice.UpdateRedisFailoverStatus(ctx, rf.Namespace, rf, metav1.PatchOptions{})
}
