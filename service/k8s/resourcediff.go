package k8s

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// The *UpToDate functions below all follow the same pattern: compare a live
// stored object against a desired one built by this codebase's generator.go,
// so CreateOrUpdate can skip a no-op write. Each normalizes away a specific
// set of fields that the API server fills in with its own default but that
// the relevant generator.go builder never sets - left unnormalized, those
// fields would make stored differ from desired on every reconcile even when
// nothing meaningful changed. Only ever normalize a copy of stored, never
// desired: desired already leaves these fields unset, and clearing them
// there too would silently accept a real change to one of them.

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
// Normalized here: RevisionHistoryLimit and the PodSpec/container fields
// normalizePodSpecForComparison clears - none of them are ever set by
// generateRedisStatefulSet. Only the stored copy is touched - desired is
// compared as built, so a real difference desired does specify still
// surfaces normally.
func statefulSetUpToDate(stored, desired *appsv1.StatefulSet) bool {
	if !equality.Semantic.DeepEqual(stored.Labels, desired.Labels) {
		return false
	}
	// Annotations are compared as-is (not normalized) because
	// CreateOrUpdateStatefulSet merges stored's annotations into desired
	// before this check runs, so by this point desired.Annotations already
	// equals stored.Annotations unless the merge actually changed something.
	if !equality.Semantic.DeepEqual(stored.Annotations, desired.Annotations) {
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
	if !equality.Semantic.DeepEqual(stored.Labels, desired.Labels) {
		return false
	}

	normalized := stored.Spec.DeepCopy()
	normalized.RevisionHistoryLimit = nil
	normalized.ProgressDeadlineSeconds = nil
	if equality.Semantic.DeepEqual(defaultedStrategy(stored.Spec.Strategy), defaultedStrategy(desired.Spec.Strategy)) {
		normalized.Strategy = desired.Spec.Strategy
	}
	normalizePodSpecForComparison(&normalized.Template.Spec)

	return equality.Semantic.DeepEqual(normalized, &desired.Spec)
}

// defaultedStrategy fills in what the API server defaults in a Deployment
// strategy, so a stored strategy compares equal to the desired one it came from.
func defaultedStrategy(s appsv1.DeploymentStrategy) appsv1.DeploymentStrategy {
	s = *s.DeepCopy()
	if s.Type == "" {
		s.Type = appsv1.RollingUpdateDeploymentStrategyType
	}
	if s.Type != appsv1.RollingUpdateDeploymentStrategyType {
		return s
	}
	if s.RollingUpdate == nil {
		s.RollingUpdate = &appsv1.RollingUpdateDeployment{}
	}
	quarter := intstr.FromString("25%")
	if s.RollingUpdate.MaxUnavailable == nil {
		s.RollingUpdate.MaxUnavailable = &quarter
	}
	if s.RollingUpdate.MaxSurge == nil {
		s.RollingUpdate.MaxSurge = &quarter
	}
	return s
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
// Normalized here: SessionAffinity (the API server defaults it to "None")
// and InternalTrafficPolicy (defaulted to a non-nil "Cluster" pointer).
func serviceUpToDate(stored, desired *corev1.Service) bool {
	if !equality.Semantic.DeepEqual(stored.Labels, desired.Labels) {
		return false
	}
	if !equality.Semantic.DeepEqual(stored.Annotations, desired.Annotations) {
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
// Spec and no server-side defaulting on its Data/BinaryData, so no
// normalization is needed here at all.
func configMapUpToDate(stored, desired *corev1.ConfigMap) bool {
	if !equality.Semantic.DeepEqual(stored.Labels, desired.Labels) {
		return false
	}
	if !equality.Semantic.DeepEqual(stored.Annotations, desired.Annotations) {
		return false
	}
	return equality.Semantic.DeepEqual(stored.Data, desired.Data) &&
		equality.Semantic.DeepEqual(stored.BinaryData, desired.BinaryData)
}

// podDisruptionBudgetUpToDate is statefulSetUpToDate's counterpart for
// PodDisruptionBudget. See its doc comment for the general comparison
// strategy. Unlike StatefulSet/Deployment/Service, no normalization is
// needed here: MaxAvailable and UnhealthyPodEvictionPolicy are the only
// fields the API server is known to default, and generatePodDisruptionBudget
// never sets either.
func podDisruptionBudgetUpToDate(stored, desired *policyv1.PodDisruptionBudget) bool {
	if !equality.Semantic.DeepEqual(stored.Labels, desired.Labels) {
		return false
	}
	return equality.Semantic.DeepEqual(&stored.Spec, &desired.Spec)
}

// serviceAccountUpToDate is statefulSetUpToDate's counterpart for
// ServiceAccount. See its doc comment for the general comparison strategy.
// generateSentinelServiceAccount sets nothing beyond ObjectMeta, so this only
// needs to compare Labels.
func serviceAccountUpToDate(stored, desired *corev1.ServiceAccount) bool {
	return equality.Semantic.DeepEqual(stored.Labels, desired.Labels)
}

// normalizePodSpecForComparison clears, in place, the PodSpec and container
// fields that neither generateRedisStatefulSet nor generateSentinelDeployment
// ever set and that the API server fills in with its own default:
// PodSpec.RestartPolicy, PodSpec.SchedulerName, PodSpec.
// DeprecatedServiceAccount (mirroring ServiceAccountName), and each
// container's TerminationMessagePath/TerminationMessagePolicy.
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
