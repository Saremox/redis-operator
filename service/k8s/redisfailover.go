package k8s

import (
	"context"
	"encoding/json"

	"k8s.io/apimachinery/pkg/types"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	redisfailoverclientset "github.com/saremox/redis-operator/client/k8s/clientset/versioned"
	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
)

// RedisFailover the RF service that knows how to interact with k8s to get them.
type RedisFailover interface {
	// ListRedisFailovers lists the redisfailovers on a cluster.
	ListRedisFailovers(ctx context.Context, namespace string, opts metav1.ListOptions) (*redisfailoverv1.RedisFailoverList, error)
	// WatchRedisFailovers watches the redisfailovers on a cluster.
	WatchRedisFailovers(ctx context.Context, namespace string, opts metav1.ListOptions) (watch.Interface, error)
	UpdateRedisFailoverStatus(ctx context.Context, namespace string, redisFailover *redisfailoverv1.RedisFailover, opts metav1.PatchOptions)
	// PatchRedisFailoverFinalizers replaces a RedisFailover's finalizers list
	// with the given one. Finalizers are in the metadata, not in the status
	// subresource, so UpdateRedisFailoverStatus cannot change them.
	PatchRedisFailoverFinalizers(ctx context.Context, namespace string, name string, finalizers []string, opts metav1.PatchOptions) error
}

// RedisFailoverService is the RedisFailover service implementation using API calls to kubernetes.
type RedisFailoverService struct {
	k8sCli          redisfailoverclientset.Interface
	logger          log.Logger
	metricsRecorder metrics.Recorder
}

// NewRedisFailoverService returns a new RedisFailover KubeService.
func NewRedisFailoverService(k8scli redisfailoverclientset.Interface, logger log.Logger, metricsRecorder metrics.Recorder) *RedisFailoverService {
	logger = logger.With("service", "k8s.redisfailover")
	return &RedisFailoverService{
		k8sCli:          k8scli,
		logger:          logger,
		metricsRecorder: metricsRecorder,
	}
}

// ListRedisFailovers implements the RedisFailover interface.
func (r *RedisFailoverService) ListRedisFailovers(ctx context.Context, namespace string, opts metav1.ListOptions) (*redisfailoverv1.RedisFailoverList, error) {
	redisFailoverList, err := r.k8sCli.DatabasesV1().RedisFailovers(namespace).List(ctx, opts)
	recordMetrics(namespace, "RedisFailover", metrics.NOT_APPLICABLE, "LIST", err, r.metricsRecorder)
	return redisFailoverList, err
}

// WatchRedisFailovers implements the RedisFailover interface.
func (r *RedisFailoverService) WatchRedisFailovers(ctx context.Context, namespace string, opts metav1.ListOptions) (watch.Interface, error) {
	watcher, err := r.k8sCli.DatabasesV1().RedisFailovers(namespace).Watch(ctx, opts)
	recordMetrics(namespace, "RedisFailover", metrics.NOT_APPLICABLE, "WATCH", err, r.metricsRecorder)
	return watcher, err
}

// UpdateRedisFailoverStatus patches the status of the RedisFailover. It logs
// a failed patch and does not return the error.
func (r *RedisFailoverService) UpdateRedisFailoverStatus(ctx context.Context, namespace string, rf *redisfailoverv1.RedisFailover, opts metav1.PatchOptions) {
	// Fields are nested under an explicit map (rather than marshaling
	// redisfailoverv1.RedisFailoverStatus directly) so every field is always
	// present in the patch, even when empty: RedisFailoverStatus's `omitempty`
	// json tags exist for the full RedisFailover object, but a JSON merge
	// patch treats an omitted field as "leave unchanged", not "clear it" -
	// omitting an empty Message here would leave a stale one from a previous
	// status on the server.
	//
	// The marshal error is ignored (matching pod.go's UpdatePodLabels and
	// UpdatePodAnnotations, which do the same for their merge patches): every
	// value here is a string, an integer, nil or a map of them, and
	// json.Marshal cannot fail on such a map.
	patch := map[string]any{
		"status": map[string]any{
			"state":       rf.Status.State,
			"lastChanged": rf.Status.LastChanged,
			"message":     rf.Status.Message,
			"autoscaling": autoscalingPatch(rf.Status.Autoscaling),
		},
	}
	patchBytes, _ := json.Marshal(patch)

	// Through the status subresource, so a status change does not bump
	// metadata.generation.
	_, err := r.k8sCli.DatabasesV1().RedisFailovers(namespace).Patch(ctx, rf.Name, types.MergePatchType, patchBytes, opts, "status")
	if err != nil {
		recordMetrics(namespace, "RedisFailover", metrics.NOT_APPLICABLE, "PATCH", err, r.metricsRecorder)
		r.logger.WithField("redisfailover", rf.Name).WithField("namespace", rf.Namespace).Errorf("Error while patching RedisFailover status: %s", err.Error())
	}
}

// autoscalingPatch returns null for an empty status, which removes the
// field. A merge patch keeps each nested field that it omits, so an empty
// field is null and not left out.
func autoscalingPatch(a redisfailoverv1.AutoscalingStatus) any {
	if a == (redisfailoverv1.AutoscalingStatus{}) {
		return nil
	}
	orNil := func(v string) any {
		if v == "" {
			return nil
		}
		return v
	}
	var highWindows any
	if a.HighWindows != 0 {
		highWindows = a.HighWindows
	}
	return map[string]any{
		"workload":          orNil(a.Workload),
		"recommendedMemory": orNil(a.RecommendedMemory),
		"reason":            orNil(a.Reason),
		"evaluatedAt":       orNil(a.EvaluatedAt),
		"highWindows":       highWindows,
		"effectiveMemory":   orNil(a.EffectiveMemory),
		"phase":             orNil(a.Phase),
		"lastStep":          orNil(a.LastStep),
		"retryAfter":        orNil(a.RetryAfter),
	}
}

// PatchRedisFailoverFinalizers implements the RedisFailover interface. A JSON
// merge patch replaces the full finalizers array, so the caller gives the
// full list, not only the changed finalizer.
func (r *RedisFailoverService) PatchRedisFailoverFinalizers(ctx context.Context, namespace, name string, finalizers []string, opts metav1.PatchOptions) error {
	if finalizers == nil {
		finalizers = []string{}
	}
	patch := map[string]any{
		"metadata": map[string]any{
			"finalizers": finalizers,
		},
	}
	patchBytes, _ := json.Marshal(patch)

	_, err := r.k8sCli.DatabasesV1().RedisFailovers(namespace).Patch(ctx, name, types.MergePatchType, patchBytes, opts)
	recordMetrics(namespace, "RedisFailover", metrics.NOT_APPLICABLE, "PATCH", err, r.metricsRecorder)
	if err != nil {
		r.logger.WithField("redisfailover", name).WithField("namespace", namespace).Errorf("Error while patching RedisFailover finalizers: %s", err.Error())
	}
	return err
}
