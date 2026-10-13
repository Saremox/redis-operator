package redisfailover

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/operator/redisfailover/autoscale"
	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
)

// errForeignState is the error for a state ConfigMap that another object
// controls. The autoscaler does not change or delete that ConfigMap.
var errForeignState = errors.New("the state ConfigMap is not controlled by the RedisFailover")

// autoscaleSample is the last sample of a RedisFailover in memory. The
// persisted LastSample is written only together with other changes.
type autoscaleSample struct {
	at    time.Time
	ratio float64
}

// autoscale runs the memory autoscaler after the checks and sets
// rf.Status.Autoscaling. The Recommend mode only reports, so an error is
// logged and never fails the reconcile or makes the RedisFailover NotHealthy.
func (r *RedisFailoverHandler) autoscale(rf *redisfailoverv1.RedisFailover, observed redisfailoverv1.AutoscalingStatus) {
	if !rf.AutoscalingEnabled() {
		// Without an earlier status, there is no state to remove. This keeps
		// the API calls of an instance without autoscaling unchanged.
		if observed != (redisfailoverv1.AutoscalingStatus{}) {
			r.stopAutoscaler(rf, observed)
		}
		return
	}

	key := failoverKey(rf)
	master, _ := r.autoscaleMasters.LoadAndDelete(key)
	limit := rf.Spec.Redis.Resources.Limits.Memory().Value()
	stored, cm, err := r.readAutoscaleState(rf)
	// The autoscaler cannot use a state of another owner or of a newer
	// operator, and it does not overwrite it. No metric value is known.
	blocked := ""
	switch {
	case errors.Is(err, errForeignState):
		blocked = autoscale.ReasonForeignState
	case err == nil && stored.Version > autoscale.StateVersion:
		blocked = autoscale.ReasonNewerState
		err = fmt.Errorf("the state ConfigMap has version %d, this operator reads version %d", stored.Version, autoscale.StateVersion)
	}
	if blocked != "" {
		rf.Status.Autoscaling = redisfailoverv1.AutoscalingStatus{Reason: blocked}
		r.mClient.DeleteAutoscaler(rf.Namespace, rf.Name)
		r.logAutoscale(rf, err)
		return
	}
	if err != nil {
		rf.Status.Autoscaling = observed
		r.logAutoscale(rf, err)
		return
	}
	maxLimit := rf.Spec.Redis.Autoscaling.MemoryMax.Value()
	now := r.now()
	var last autoscaleSample
	if v, ok := r.autoscaleSamples.Load(key); ok {
		last = v.(autoscaleSample)
	} else {
		last.ratio = stored.Window.PeakRatio
	}
	// A leftover state, for example of a mode change to Off that the status
	// did not show, must not give a recommendation of an earlier time. A
	// failed write keeps an earlier LastSample, but this operator sampled the
	// state, so it is not a leftover.
	seen := stored
	seen.LastSample = max(seen.LastSample, last.at.Unix())
	state := autoscale.Expire(seen, now)
	var errs []error
	// The persisted time limits the samples after an operator restart. It is
	// written only with other changes, so the time in memory is necessary too.
	if sampleDue(now, time.Unix(stored.LastSample, 0)) && sampleDue(now, last.at) {
		in := autoscale.Input{
			Now:      now,
			Healthy:  rf.Status.State == redisfailoverv1.HealthyState,
			Limit:    limit,
			Max:      maxLimit,
			Workload: rf.Spec.Redis.Autoscaling.Workload,
			Policy:   rf.MaxMemoryPolicy(),
		}
		if ip, _ := master.(string); in.Healthy && ip != "" {
			info, err := r.rfChecker.GetAutoscalerInfo(rf, ip)
			if err != nil {
				errs = append(errs, fmt.Errorf("no autoscaler sample of %s: %w", ip, err))
			}
			in.Info = info
		}
		state = autoscale.Observe(state, in)
		last.at = now
		if in.Info != nil {
			last.ratio = autoscale.UsedRatio(in.Info, limit)
		}
		r.autoscaleSamples.Store(key, last)
		if cm == nil || state.NeedsWrite(stored) {
			errs = append(errs, r.writeAutoscaleState(rf, cm, state))
		}
	}

	rf.Status.Autoscaling = autoscale.Report(state, limit, maxLimit)
	var recommended int64
	if rf.Status.Autoscaling.RecommendedMemory != "" {
		recommended = min(state.Recommended, maxLimit)
	}
	r.mClient.SetAutoscaler(rf.Namespace, rf.Name, recommended, last.ratio, state.HighWindows)
	r.logAutoscale(rf, errors.Join(errs...))
}

// sampleDue treats a sample time in the future as elapsed. After the clock
// went back, the autoscaler must not stop until the clock reaches that time.
func sampleDue(now, last time.Time) bool {
	elapsed := now.Sub(last)
	return elapsed < 0 || elapsed >= autoscale.SampleInterval
}

// readAutoscaleState returns the state and its ConfigMap, or a nil ConfigMap
// when it does not exist.
func (r *RedisFailoverHandler) readAutoscaleState(rf *redisfailoverv1.RedisFailover) (autoscale.State, *corev1.ConfigMap, error) {
	cm, err := r.getAutoscaleConfigMap(rf)
	if err != nil || cm == nil {
		return autoscale.State{}, nil, err
	}
	var state autoscale.State
	if err := json.Unmarshal([]byte(cm.Data[rfservice.AutoscalerStateKey]), &state); err != nil {
		// The next write replaces the content.
		r.logger.WithField("namespace", rf.Namespace).WithField("name", rf.Name).Warningf("Autoscaler state in configmap %s is not valid, the autoscaler starts again: %s", cm.Name, err.Error())
		return autoscale.State{}, cm, nil
	}
	return state, cm, nil
}

// getAutoscaleConfigMap returns nil when the ConfigMap does not exist.
func (r *RedisFailoverHandler) getAutoscaleConfigMap(rf *redisfailoverv1.RedisFailover) (*corev1.ConfigMap, error) {
	name := rfservice.GetAutoscalerStateName(rf)
	cm, err := r.k8sservice.GetConfigMap(rf.Namespace, name)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !metav1.IsControlledBy(cm, rf) {
		return nil, errForeignState
	}
	return cm, nil
}

func (r *RedisFailoverHandler) writeAutoscaleState(rf *redisfailoverv1.RedisFailover, stored *corev1.ConfigMap, state autoscale.State) error {
	// State has only strings and finite numbers, so Marshal cannot fail.
	data, _ := json.Marshal(state)
	cm := rfservice.GenerateAutoscalerStateConfigMap(rf, r.getLabels(rf), r.createOwnerReferences(rf), string(data))
	if stored == nil {
		return r.k8sservice.CreateConfigMap(rf.Namespace, cm)
	}
	// With the read resource version, the update fails when another writer
	// changed the state after the read.
	cm.ResourceVersion = stored.ResourceVersion
	return r.k8sservice.UpdateConfigMap(rf.Namespace, cm)
}

// stopAutoscaler removes the state, the metrics and the status after a change
// of the mode to Off. On an error, the status stays, so the next reconcile
// tries again.
func (r *RedisFailoverHandler) stopAutoscaler(rf *redisfailoverv1.RedisFailover, observed redisfailoverv1.AutoscalingStatus) {
	cm, err := r.getAutoscaleConfigMap(rf)
	if errors.Is(err, errForeignState) {
		// Not a state of this RedisFailover, so there is nothing to delete.
		err = nil
	}
	if err == nil && cm != nil {
		err = r.k8sservice.DeleteConfigMap(rf.Namespace, cm.Name)
		if apierrors.IsNotFound(err) {
			err = nil
		}
	}
	if err != nil {
		rf.Status.Autoscaling = observed
		r.logAutoscale(rf, err)
		return
	}
	r.mClient.DeleteAutoscaler(rf.Namespace, rf.Name)
	r.forgetAutoscaler(failoverKey(rf))
}

func (r *RedisFailoverHandler) forgetAutoscaler(key string) {
	r.autoscaleMasters.Delete(key)
	r.autoscaleSamples.Delete(key)
	r.autoscaleLogged.Delete(key)
}

// logAutoscale logs an error at Warning when it differs from the last one,
// and at Debug otherwise.
func (r *RedisFailoverHandler) logAutoscale(rf *redisfailoverv1.RedisFailover, err error) {
	key := failoverKey(rf)
	if err == nil {
		r.autoscaleLogged.Delete(key)
		return
	}
	logger := r.logger.WithField("namespace", rf.Namespace).WithField("name", rf.Name)
	if last, _ := r.autoscaleLogged.Swap(key, err.Error()); last == err.Error() {
		logger.Debugf("Autoscaler: %s", err.Error())
		return
	}
	logger.Warningf("Autoscaler: %s", err.Error())
}
