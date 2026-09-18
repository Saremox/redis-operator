package k8s

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/equality"
)

// statefulSetUpToDate reports whether desired would change anything about
// stored if applied, so the caller can skip a no-op Update call.
//
// It compares stored directly against desired rather than against a record
// of what the operator last wrote (e.g. a stamped hash): that is what lets it
// also catch drift, since a live object edited by hand no longer matches
// desired and is reported as needing an update, same as an intentional spec
// change would be. Comparing the live object's fields directly is what
// controller-runtime's CreateOrUpdate does too.
//
// The comparison is normalized first: fields the API server fills in with a
// default (RevisionHistoryLimit, PodSpec.RestartPolicy/SchedulerName,
// container TerminationMessagePath/Policy, ...) are cleared on a copy of
// stored before comparing, because generateRedisStatefulSet never sets them
// and so desired never carries the server's default value for them. Left
// unnormalized, those fields would differ from desired on every reconcile
// even when nothing meaningful changed. Only the stored copy is touched -
// desired is compared as built, so a real difference desired does specify
// still surfaces normally.
func statefulSetUpToDate(stored, desired *appsv1.StatefulSet) bool {
	if !mapsEqual(stored.Labels, desired.Labels) {
		return false
	}
	// Annotations are compared as-is (not normalized) because
	// CreateOrUpdateStatefulSet merges stored's annotations into desired
	// before this check runs, so by this point desired.Annotations already
	// equals stored.Annotations unless the merge actually changed something.
	// mapsEqual (not equality.Semantic.DeepEqual) matters here specifically:
	// util.MergeAnnotations always returns a non-nil map, even merging two
	// nils, while a real object with no annotations set comes back from the
	// API server with a nil map - a plain DeepEqual would see nil vs. {} as
	// a permanent difference on every reconcile of any RedisFailover with no
	// custom annotations.
	if !mapsEqual(stored.Annotations, desired.Annotations) {
		return false
	}

	normalized := stored.Spec.DeepCopy()
	normalized.RevisionHistoryLimit = nil
	normalizePodSpecForComparison(&normalized.Template.Spec)

	return equality.Semantic.DeepEqual(normalized, &desired.Spec)
}

// deploymentUpToDate is statefulSetUpToDate's counterpart for Deployment. See
// its doc comment for the comparison strategy.
//
// Deployment's own ObjectMeta.Annotations are deliberately excluded from the
// comparison: generateSentinelDeployment never sets them, but the deployment
// controller stamps deployment.kubernetes.io/revision on every rollout, so
// comparing them as-is would always report a difference. Unlike StatefulSet,
// CreateOrUpdateDeployment has no pre-existing merge step that folds stored's
// annotations into desired first, so this reports "no meaningful change" for
// annotation-only drift. If a caller ever starts setting Deployment-level
// annotations from the RedisFailover spec, this needs revisiting.
func deploymentUpToDate(stored, desired *appsv1.Deployment) bool {
	if !mapsEqual(stored.Labels, desired.Labels) {
		return false
	}

	normalized := stored.Spec.DeepCopy()
	normalized.RevisionHistoryLimit = nil
	normalized.ProgressDeadlineSeconds = nil
	normalized.Strategy = appsv1.DeploymentStrategy{}
	normalizePodSpecForComparison(&normalized.Template.Spec)

	return equality.Semantic.DeepEqual(normalized, &desired.Spec)
}

// serviceUpToDate is statefulSetUpToDate's counterpart for Service. See its
// doc comment for the general comparison strategy.
//
// It must be called after mergeImmutableServiceFields, which CreateOrUpdate
// Service already runs before writing: that folds stored's ClusterIP(s),
// IPFamilies, IPFamilyPolicy, HealthCheckNodePort and per-port NodePort into
// desired whenever desired left them unset, the same way the
// VolumeClaimTemplates copy in CreateOrUpdateStatefulSet does - so by the
// time this runs, those fields already agree unless something meaningful
// changed, and this function does not need to normalize them again.
//
// What it does normalize: SessionAffinity (the API server defaults it to
// "None") and InternalTrafficPolicy (defaulted to a non-nil "Cluster"
// pointer), neither of which any of the Service builders in generator.go
// ever set.
func serviceUpToDate(stored, desired *corev1.Service) bool {
	if !mapsEqual(stored.Labels, desired.Labels) {
		return false
	}
	if !mapsEqual(stored.Annotations, desired.Annotations) {
		return false
	}

	normalized := stored.Spec.DeepCopy()
	normalized.SessionAffinity = ""
	normalized.SessionAffinityConfig = nil
	normalized.InternalTrafficPolicy = nil

	return equality.Semantic.DeepEqual(normalized, &desired.Spec)
}

// configMapUpToDate is statefulSetUpToDate's counterpart for ConfigMap. See
// its doc comment for the general comparison strategy. ConfigMap has no
// Spec, no server-side defaulting on its Data/BinaryData, and none of the
// ConfigMap builders in generator.go set annotations, so no normalization is
// needed beyond the nil-vs-empty-map handling mapsEqual already gives Labels
// and Annotations.
func configMapUpToDate(stored, desired *corev1.ConfigMap) bool {
	if !mapsEqual(stored.Labels, desired.Labels) {
		return false
	}
	if !mapsEqual(stored.Annotations, desired.Annotations) {
		return false
	}
	return equality.Semantic.DeepEqual(stored.Data, desired.Data) &&
		equality.Semantic.DeepEqual(stored.BinaryData, desired.BinaryData)
}

// podDisruptionBudgetUpToDate is statefulSetUpToDate's counterpart for
// PodDisruptionBudget. See its doc comment for the general comparison
// strategy. generatePodDisruptionBudget never sets MaxAvailable or
// UnhealthyPodEvictionPolicy, and neither is known to be defaulted to a
// non-zero value by the API server, so - unlike StatefulSet/Deployment/
// Service - no normalization is needed here beyond mapsEqual for Labels.
func podDisruptionBudgetUpToDate(stored, desired *policyv1.PodDisruptionBudget) bool {
	if !mapsEqual(stored.Labels, desired.Labels) {
		return false
	}
	return equality.Semantic.DeepEqual(&stored.Spec, &desired.Spec)
}

// serviceAccountUpToDate is statefulSetUpToDate's counterpart for
// ServiceAccount. See its doc comment for the general comparison strategy.
// generateSentinelServiceAccount sets nothing beyond ObjectMeta, so this only
// needs to compare Labels.
func serviceAccountUpToDate(stored, desired *corev1.ServiceAccount) bool {
	return mapsEqual(stored.Labels, desired.Labels)
}

// normalizePodSpecForComparison clears, in place, the PodSpec and container
// fields that neither generateRedisStatefulSet nor generateSentinelDeployment
// ever set and that the API server fills in with its own default
// (PodSpec.RestartPolicy, PodSpec.SchedulerName, PodSpec.
// DeprecatedServiceAccount mirroring ServiceAccountName, and each
// container's TerminationMessagePath/TerminationMessagePolicy). It must only
// ever be called on a copy of a live (stored) object, never on a desired
// object being built for a write: desired already leaves these fields unset,
// and clearing them there too would silently accept a real change to one of
// them.
func normalizePodSpecForComparison(spec *corev1.PodSpec) {
	spec.RestartPolicy = ""
	spec.SchedulerName = ""
	spec.DeprecatedServiceAccount = ""

	for i := range spec.Containers {
		spec.Containers[i].TerminationMessagePath = ""
		spec.Containers[i].TerminationMessagePolicy = ""
	}
	for i := range spec.InitContainers {
		spec.InitContainers[i].TerminationMessagePath = ""
		spec.InitContainers[i].TerminationMessagePolicy = ""
	}
}

// mapsEqual reports whether a and b hold the same key/value pairs, treating
// a nil map and an empty map as equal. A plain DeepEqual does not: desired
// objects here are built through util.MergeLabels/MergeAnnotations, which
// always allocate a map even when merging zero or nil inputs, while an
// object read back from the API server carries a nil map when nothing was
// ever set on it. Without this, comparing the two would report a permanent
// difference on every reconcile of any RedisFailover with no custom
// annotations.
func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}
