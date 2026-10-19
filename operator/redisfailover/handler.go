package redisfailover

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
	"github.com/saremox/redis-operator/operator/redisfailover/util"
	"github.com/saremox/redis-operator/service/k8s"
)

const (
	rfLabelManagedByKey     = "app.kubernetes.io/managed-by"
	rfLabelNameKey          = "redisfailovers.databases.spotahome.com/name"
	skipReconcileAnnotation = "redisfailovers.databases.spotahome.com/skip-reconcile"
	// redisFailoverFinalizer makes the deletion of a RedisFailover visible to
	// Handle. Without it, the object can leave the informer cache before
	// Handle sees it, and rfController.process then calls Forget with the
	// key only. The finalizer keeps the object, with DeletionTimestamp set,
	// until Handle removes the finalizer. Handle then removes the state
	// outside the object: the cluster_ok metric series and the maps of the
	// handler.
	redisFailoverFinalizer = "redisfailovers.databases.spotahome.com/finalizer"
	// masterUnreachableAnnotation holds, on the master pod, the RFC3339 time
	// of the first check that the master missed. failoverTimeout counts from it.
	masterUnreachableAnnotation = "redisfailovers.databases.spotahome.com/unreachable-since"
)

var (
	defaultLabels = map[string]string{
		rfLabelManagedByKey: operatorName,
	}
)

// RedisFailoverHandler is the Redis Failover handler. This handler will create the required
// resources that a RF needs.
type RedisFailoverHandler struct {
	config     Config
	k8sservice k8s.Services
	rfService  rfservice.RedisFailoverClient
	rfChecker  rfservice.RedisFailoverCheck
	rfHealer   rfservice.RedisFailoverHeal
	mClient    metrics.Recorder
	logger     log.Logger
	// passwords holds a passwordState per namespace/name, so a changed secret
	// can be applied with the old password.
	passwords sync.Map
	// appliedSecretLogged holds, per namespace/name, the appliedSecretLog of
	// the last error with the applied passwords Secret.
	appliedSecretLogged sync.Map
	// rolloutWaits holds, per namespace/name, the rolloutWait the redis pod
	// rollout waits on and since when.
	rolloutWaits sync.Map
	// unreachableCleared records, per namespace/name, that no pod has the
	// unreachable-since annotation, so a healthy reconcile does not list pods.
	unreachableCleared sync.Map
	// maxMemoryLogged holds, per namespace/name, the last maxmemory outcome
	// that was logged at Warning, so that an unchanged outcome does not repeat
	// at each reconcile.
	maxMemoryLogged sync.Map
	// handoverRetries holds, per namespace/name, the handoverRetry after a
	// handover of the master role that did not move the role.
	handoverRetries sync.Map
	// handoverFallbackLogged holds, per namespace/name, the last logged reason
	// for a master pod delete instead of a handover, so it does not repeat.
	handoverFallbackLogged sync.Map
	// sentinelResets holds, per namespace/name, the time of the last SENTINEL
	// RESET or SENTINEL MONITOR of each Sentinel IP.
	sentinelResets sync.Map
	// sentinelFailovers holds, per namespace/name, the sentinelFailover that
	// the Sentinel heal waits for.
	sentinelFailovers sync.Map
	// sentinelReadErrors holds, per namespace/name, the time of the first read
	// error of the failover state of each Sentinel IP.
	sentinelReadErrors sync.Map
	// sentinelMasterWaits holds, per namespace/name, the sentinelMasterWait of
	// the master and replica heal.
	sentinelMasterWaits sync.Map
	// sentinelUnknown holds, per namespace/name, the sentinelUnknownAddrs of
	// the Sentinels.
	sentinelUnknown sync.Map
	// autoscaleReads holds, per namespace/name, the autoscaleRead of the
	// current reconcile.
	autoscaleReads sync.Map
	// autoscaleMasters holds, per namespace/name, the master that the checks
	// of the current reconcile found, so the autoscaler needs no extra call.
	autoscaleMasters sync.Map
	// autoscaleRollout holds, per namespace/name, the autoscaleRollout that
	// UpdateRedisesPods found in the current reconcile. CheckAndHeal deletes
	// it at its start, so a reconcile without UpdateRedisesPods has none.
	autoscaleRollout sync.Map
	// autoscaleSamples holds, per namespace/name, the last autoscaleSample.
	autoscaleSamples sync.Map
	// autoscaleLogged holds, per namespace/name, the last autoscaler error
	// that was logged at Warning.
	autoscaleLogged sync.Map
	// autoscaleRefusalLogged holds, per namespace/name, the last refusal of
	// a resize to an autoscaler revision that was logged at Warning.
	autoscaleRefusalLogged sync.Map
	// autoscaleOff holds, per namespace/name, true after a reconcile without
	// autoscaling found no state ConfigMap.
	autoscaleOff sync.Map
	// autoscaleClearLogged holds, per namespace/name, the last failed removal
	// of a resize request that was logged at Warning.
	autoscaleClearLogged sync.Map
	now                  func() time.Time
	// requeue is nil until New connects the controller.
	requeue func(key string, after time.Duration)
}

// NewRedisFailoverHandler returns a new RF handler.
func NewRedisFailoverHandler(config Config, rfService rfservice.RedisFailoverClient, rfChecker rfservice.RedisFailoverCheck, rfHealer rfservice.RedisFailoverHeal, k8sservice k8s.Services, mClient metrics.Recorder, logger log.Logger) *RedisFailoverHandler {
	return &RedisFailoverHandler{
		config:     config,
		rfService:  rfService,
		rfChecker:  rfChecker,
		rfHealer:   rfHealer,
		mClient:    mClient,
		k8sservice: k8sservice,
		logger:     logger,
		now:        time.Now,
	}
}

// Forget drops the cluster_ok series and the in-memory state of a RedisFailover.
// The controller also calls it for a RedisFailover that is gone before a
// reconcile sees its deletion timestamp, for example after a manual removal of
// the finalizer.
func (r *RedisFailoverHandler) Forget(key string) {
	namespace, name, _ := strings.Cut(key, "/")
	r.mClient.DeleteCluster(namespace, name)
	r.passwords.Delete(key)
	r.appliedSecretLogged.Delete(key)
	r.rolloutWaits.Delete(key)
	r.unreachableCleared.Delete(key)
	r.maxMemoryLogged.Delete(key)
	r.handoverRetries.Delete(key)
	r.handoverFallbackLogged.Delete(key)
	r.sentinelResets.Delete(key)
	r.sentinelFailovers.Delete(key)
	r.sentinelReadErrors.Delete(key)
	r.sentinelMasterWaits.Delete(key)
	r.sentinelUnknown.Delete(key)
	r.forgetAutoscaler(key)
}

// Handle will ensure the redis failover is in the expected state.
func (r *RedisFailoverHandler) Handle(ctx context.Context, obj runtime.Object) error {
	rf, ok := obj.(*redisfailoverv1.RedisFailover)
	if !ok {
		return fmt.Errorf("can't handle the received object: not a redisfailover")
	}
	defer r.startSentinelMasterRound(failoverKey(rf))()
	// The controller waits for a running reconcile on shutdown, and the heal
	// steps ignore a cancel. Thus the writes keep only the values of ctx.
	ctx = context.WithoutCancel(ctx)

	// Deletion cleanup and finalizer registration run before anything else,
	// including Validate(): an object that never passes validation must
	// still get a finalizer (so its eventual deletion is observable here)
	// and must still have its metrics cleaned up on the way out.
	if rf.DeletionTimestamp != nil {
		if !slices.Contains(rf.Finalizers, redisFailoverFinalizer) {
			// Finalizer already removed (or never added) - nothing left to do.
			return nil
		}
		r.Forget(failoverKey(rf))
		remaining := slices.DeleteFunc(slices.Clone(rf.Finalizers), func(f string) bool {
			return f == redisFailoverFinalizer
		})
		return r.k8sservice.PatchRedisFailoverFinalizers(ctx, rf.Namespace, rf.Name, remaining, metav1.PatchOptions{})
	}

	if !slices.Contains(rf.Finalizers, redisFailoverFinalizer) {
		finalizers := append(slices.Clone(rf.Finalizers), redisFailoverFinalizer)
		if err := r.k8sservice.PatchRedisFailoverFinalizers(ctx, rf.Namespace, rf.Name, finalizers, metav1.PatchOptions{}); err != nil {
			return err
		}
	}

	if rf.Annotations[skipReconcileAnnotation] == "true" {
		r.logger.WithField("redisfailover", rf.Name).WithField("namespace", rf.Namespace).
			Infof("skip-reconcile annotation set to true, skipping reconciliation")
		return nil
	}

	if err := rf.Validate(); err != nil {
		r.mClient.SetClusterError(rf.Namespace, rf.Name)
		r.reportInvalid(ctx, rf, err)
		return err
	}

	rf, err := r.withEffectiveLimit(rf)
	if err != nil {
		r.mClient.SetClusterError(rf.Namespace, rf.Name)
		return fmt.Errorf("autoscaler state: %w", err)
	}

	// The owner references make the RedisFailover the owner of the objects
	// managed by this handler.
	oRefs := r.createOwnerReferences(rf)

	labels := r.getLabels(rf)

	if err := r.Ensure(rf, labels, oRefs, r.mClient); err != nil {
		r.mClient.SetClusterError(rf.Namespace, rf.Name)
		var other *k8s.ControlledByOtherError
		if errors.As(err, &other) {
			r.reportInvalid(ctx, rf, err)
		}
		return err
	}

	if err := r.checkAndHeal(ctx, rf); err != nil {
		r.mClient.SetClusterError(rf.Namespace, rf.Name)
		return err
	}

	r.mClient.SetClusterOK(rf.Namespace, rf.Name)
	return nil
}

// withEffectiveLimit reads the autoscaler state for the autoscaler of this
// reconcile and returns effectiveRF. Ensure must not write the pods with the
// spec limit when the autoscaler applied a larger one. On an error, it
// returns rf.
func (r *RedisFailoverHandler) withEffectiveLimit(rf *redisfailoverv1.RedisFailover) (*redisfailoverv1.RedisFailover, error) {
	if !rf.AutoscalingEnabled() {
		// A state with an Effective limit can stay without a status, for
		// example after a failed status write. A later mode Recommend or Auto
		// would use it. One look after the operator start finds it. With
		// a status, the autoscaler removes the state after the checks.
		if _, clean := r.autoscaleOff.Load(failoverKey(rf)); !clean && rf.Status.Autoscaling == (redisfailoverv1.AutoscalingStatus{}) {
			r.stopAutoscaler(rf, rf.Status.Autoscaling)
		}
		return rf, nil
	}
	read, err := r.readAutoscale(rf)
	if err != nil {
		return rf, err
	}
	r.autoscaleReads.Store(failoverKey(rf), read)
	return effectiveRF(rf, read.effective), nil
}

// effectiveRF returns rf with the effective memory limit of the redis
// container, for Ensure and the checks. Validate reads the spec before. The
// redis StatefulSet gets the annotations of the RedisFailover, and with them
// the spec limit, so the next reconcile sees a change of the spec limit.
func effectiveRF(rf *redisfailoverv1.RedisFailover, effective int64) *redisfailoverv1.RedisFailover {
	if !rf.AutoscalingEnabled() {
		return rf
	}
	limit := rf.Spec.Redis.Resources.Limits.Memory()
	if effective == limit.Value() && rf.Annotations[rfservice.SpecMemoryLimitAnnotation] == limit.String() {
		return rf
	}
	rf = rf.DeepCopy()
	rf.Annotations = util.MergeAnnotations(rf.Annotations, map[string]string{rfservice.SpecMemoryLimitAnnotation: limit.String()})
	if effective == limit.Value() {
		return rf
	}
	resources := &rf.Spec.Redis.Resources
	value := *resource.NewQuantity(effective, resource.BinarySI)
	// Only a request equal to the limit follows it, so the QoS class stays.
	if request, ok := resources.Requests[corev1.ResourceMemory]; ok && request.Cmp(*limit) == 0 {
		resources.Requests[corev1.ResourceMemory] = value
	}
	resources.Limits[corev1.ResourceMemory] = value
	return rf
}

// getLabels merges the static operator labels, the name label and the labels
// of the RedisFailover that labelWhitelist allows. A later map wins on a
// duplicate key, so the RedisFailover labels can replace the other labels.
func (r *RedisFailoverHandler) getLabels(rf *redisfailoverv1.RedisFailover) map[string]string {
	dynLabels := map[string]string{
		rfLabelNameKey: rf.Name,
	}

	filteredCustomLabels := make(map[string]string)
	if len(rf.Spec.LabelWhitelist) != 0 {
		for _, regex := range rf.Spec.LabelWhitelist {
			compiledRegexp, err := regexp.Compile(regex)
			if err != nil {
				r.logger.WithField("redisfailover", rf.Name).WithField("namespace", rf.Namespace).Errorf("Unable to compile label whitelist regex '%s', ignoring it.", regex)
				continue
			}
			for labelKey, labelValue := range rf.Labels {
				if match := compiledRegexp.MatchString(labelKey); match {
					filteredCustomLabels[labelKey] = labelValue
				}
			}
		}
	} else {
		filteredCustomLabels = rf.Labels
	}
	return util.MergeLabels(defaultLabels, dynLabels, filteredCustomLabels)
}

// reportInvalid shows a validation error or an object name conflict in the
// status. The operator does not reconcile such a RedisFailover, so the last
// status can be wrong.
func (r *RedisFailoverHandler) reportInvalid(ctx context.Context, rf *redisfailoverv1.RedisFailover, err error) {
	status := redisfailoverv1.RedisFailoverStatus{
		State:       redisfailoverv1.NotHealthyState,
		Message:     err.Error(),
		LastChanged: rf.Status.LastChanged,
		Autoscaling: rf.Status.Autoscaling,
	}
	if rf.Status == status {
		return
	}
	if rf.Status.State != status.State {
		status.LastChanged = time.Now().Format(time.RFC3339)
	}
	rf.Status = status
	r.k8sservice.UpdateRedisFailoverStatus(ctx, rf.Namespace, rf, metav1.PatchOptions{})
}

func (r *RedisFailoverHandler) createOwnerReferences(rf *redisfailoverv1.RedisFailover) []metav1.OwnerReference {
	rfvk := redisfailoverv1.VersionKind(redisfailoverv1.RFKind)
	return []metav1.OwnerReference{
		*metav1.NewControllerRef(rf, rfvk),
	}
}
