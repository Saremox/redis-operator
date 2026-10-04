package redisfailover

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sync"
	"time"

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
	// Handle. Without it, the object is not in the informer cache when the
	// delete event comes, and rfController.process drops a key without an
	// object. The finalizer keeps the object, with DeletionTimestamp set,
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
	// rolloutWaits holds, per namespace/name, the rolloutWait the redis pod
	// rollout waits on and since when.
	rolloutWaits sync.Map
	// unreachableCleared records, per namespace/name, that no pod has the
	// unreachable-since annotation, so a healthy reconcile does not list pods.
	unreachableCleared sync.Map
	now                func() time.Time
	// requeue is nil until New connects the controller.
	requeue func(key string, after time.Duration)
}

// NewRedisFailoverHandler returns a new RF handler
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

// Handle will ensure the redis failover is in the expected state.
func (r *RedisFailoverHandler) Handle(_ context.Context, obj runtime.Object) error {
	rf, ok := obj.(*redisfailoverv1.RedisFailover)
	if !ok {
		return fmt.Errorf("can't handle the received object: not a redisfailover")
	}

	// Deletion cleanup and finalizer registration run before anything else,
	// including Validate(): an object that never passes validation must
	// still get a finalizer (so its eventual deletion is observable here)
	// and must still have its metrics cleaned up on the way out.
	if rf.DeletionTimestamp != nil {
		if !slices.Contains(rf.Finalizers, redisFailoverFinalizer) {
			// Finalizer already removed (or never added) - nothing left to do.
			return nil
		}
		r.mClient.DeleteCluster(rf.Namespace, rf.Name)
		r.passwords.Delete(passwordKey(rf))
		r.rolloutWaits.Delete(passwordKey(rf))
		r.unreachableCleared.Delete(failoverKey(rf))
		remaining := slices.DeleteFunc(slices.Clone(rf.Finalizers), func(f string) bool {
			return f == redisFailoverFinalizer
		})
		return r.k8sservice.PatchRedisFailoverFinalizers(context.Background(), rf.Namespace, rf.Name, remaining, metav1.PatchOptions{})
	}

	if !slices.Contains(rf.Finalizers, redisFailoverFinalizer) {
		finalizers := append(slices.Clone(rf.Finalizers), redisFailoverFinalizer)
		if err := r.k8sservice.PatchRedisFailoverFinalizers(context.Background(), rf.Namespace, rf.Name, finalizers, metav1.PatchOptions{}); err != nil {
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
		r.reportInvalid(rf, err)
		return err
	}

	// The owner references make the RedisFailover the owner of the objects
	// managed by this handler.
	oRefs := r.createOwnerReferences(rf)

	labels := r.getLabels(rf)

	if err := r.Ensure(rf, labels, oRefs, r.mClient); err != nil {
		r.mClient.SetClusterError(rf.Namespace, rf.Name)
		var other *k8s.ControlledByOtherError
		if errors.As(err, &other) {
			r.reportInvalid(rf, err)
		}
		return err
	}

	if err := r.CheckAndHeal(rf); err != nil {
		r.mClient.SetClusterError(rf.Namespace, rf.Name)
		return err
	}

	r.mClient.SetClusterOK(rf.Namespace, rf.Name)
	return nil
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
				r.logger.Errorf("Unable to compile label whitelist regex '%s', ignoring it.", regex)
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
func (r *RedisFailoverHandler) reportInvalid(rf *redisfailoverv1.RedisFailover, err error) {
	status := redisfailoverv1.RedisFailoverStatus{
		State:       redisfailoverv1.NotHealthyState,
		Message:     err.Error(),
		LastChanged: rf.Status.LastChanged,
	}
	if rf.Status == status {
		return
	}
	if rf.Status.State != status.State {
		status.LastChanged = time.Now().Format(time.RFC3339)
	}
	rf.Status = status
	r.k8sservice.UpdateRedisFailoverStatus(context.Background(), rf.Namespace, rf, metav1.PatchOptions{})
}

func (r *RedisFailoverHandler) createOwnerReferences(rf *redisfailoverv1.RedisFailover) []metav1.OwnerReference {
	rfvk := redisfailoverv1.VersionKind(redisfailoverv1.RFKind)
	return []metav1.OwnerReference{
		*metav1.NewControllerRef(rf, rfvk),
	}
}
