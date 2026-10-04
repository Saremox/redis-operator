package k8s

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// The *UpToDate functions compare the stored object with the desired object
// from generator.go, so that CreateOrUpdate does not write an unchanged
// object. The API server sets defaults for some fields that generator.go does
// not set. Each function clears these fields in a copy of stored. It does not
// clear them in desired, because that hides a real change.
//
// Each function compares OwnerReferences. A child that lost its owner, for
// example after `kubectl delete --cascade=orphan`, must get the owner of the
// new RedisFailover, or the garbage collector does not delete it later.
// Known limit: the volume claim templates of a StatefulSet cannot change, so
// a PVC that the StatefulSet creates later still gets the owner reference of
// the old RedisFailover from the template.

// statefulSetUpToDate reports whether desired changes nothing in stored.
//
// It compares with the live object, not with a hash of the last write, so a
// manual change of the object also gives an update.
//
// It clears RevisionHistoryLimit and the fields that
// normalizePodSpecForComparison clears.
func statefulSetUpToDate(stored, desired *appsv1.StatefulSet) bool {
	if !equality.Semantic.DeepEqual(stored.Labels, desired.Labels) {
		return false
	}
	if !equality.Semantic.DeepEqual(stored.OwnerReferences, desired.OwnerReferences) {
		return false
	}
	// CreateOrUpdateStatefulSet merges the stored annotations into desired
	// before this check, so the annotations differ only after a real change.
	if !equality.Semantic.DeepEqual(stored.Annotations, desired.Annotations) {
		return false
	}

	normalized := stored.Spec.DeepCopy()
	normalized.RevisionHistoryLimit = nil
	normalizePodSpecForComparison(&normalized.Template.Spec)

	return equality.Semantic.DeepEqual(normalized, &desired.Spec)
}

// deploymentUpToDate is statefulSetUpToDate for a Deployment.
//
// It does not compare the annotations of the Deployment. The Deployment
// controller sets deployment.kubernetes.io/revision, and
// generateSentinelDeployment sets no annotations. When generator.go sets
// Deployment annotations, this function must compare them.
func deploymentUpToDate(stored, desired *appsv1.Deployment) bool {
	if !equality.Semantic.DeepEqual(stored.Labels, desired.Labels) {
		return false
	}
	if !equality.Semantic.DeepEqual(stored.OwnerReferences, desired.OwnerReferences) {
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

// serviceUpToDate is statefulSetUpToDate for a Service.
//
// Call it after mergeImmutableServiceFields, which copies the fields that the
// API server assigns from stored into desired. It clears SessionAffinity
// (default "None") and InternalTrafficPolicy (default "Cluster").
func serviceUpToDate(stored, desired *corev1.Service) bool {
	if !equality.Semantic.DeepEqual(stored.Labels, desired.Labels) {
		return false
	}
	if !equality.Semantic.DeepEqual(stored.OwnerReferences, desired.OwnerReferences) {
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

// configMapUpToDate is statefulSetUpToDate for a ConfigMap. The API server
// sets no defaults in a ConfigMap, so it clears no fields.
func configMapUpToDate(stored, desired *corev1.ConfigMap) bool {
	if !equality.Semantic.DeepEqual(stored.Labels, desired.Labels) {
		return false
	}
	if !equality.Semantic.DeepEqual(stored.OwnerReferences, desired.OwnerReferences) {
		return false
	}
	if !equality.Semantic.DeepEqual(stored.Annotations, desired.Annotations) {
		return false
	}
	return equality.Semantic.DeepEqual(stored.Data, desired.Data) &&
		equality.Semantic.DeepEqual(stored.BinaryData, desired.BinaryData)
}

// podDisruptionBudgetUpToDate is statefulSetUpToDate for a
// PodDisruptionBudget. The API server sets no defaults in a policy/v1
// PodDisruptionBudget spec, so it clears no fields.
func podDisruptionBudgetUpToDate(stored, desired *policyv1.PodDisruptionBudget) bool {
	if !equality.Semantic.DeepEqual(stored.Labels, desired.Labels) {
		return false
	}
	if !equality.Semantic.DeepEqual(stored.OwnerReferences, desired.OwnerReferences) {
		return false
	}
	return equality.Semantic.DeepEqual(&stored.Spec, &desired.Spec)
}

// serviceAccountUpToDate is statefulSetUpToDate for a ServiceAccount.
// generateSentinelServiceAccount sets only labels and owner references.
func serviceAccountUpToDate(stored, desired *corev1.ServiceAccount) bool {
	return equality.Semantic.DeepEqual(stored.Labels, desired.Labels) &&
		equality.Semantic.DeepEqual(stored.OwnerReferences, desired.OwnerReferences)
}

// normalizePodSpecForComparison clears the pod and container fields that the
// API server sets and that generator.go does not set.
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
