package service

import (
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/operator/redisfailover/util"
)

const (
	redisConfigurationVolumeName = "redis-config"
	// Template used to build the Redis configuration.
	redisConfigTemplate = `slaveof 127.0.0.1 {{.Spec.Redis.Port}}
port {{.Spec.Redis.Port}}
tcp-keepalive 60
save 900 1
save 300 10
user pinger -@all +ping on >pingpass
{{- range .Spec.Redis.CustomCommandRenames}}
rename-command "{{.From}}" "{{.To}}"
{{- end}}
`

	redisShutdownConfigurationVolumeName   = "redis-shutdown-config"
	redisStartupConfigurationVolumeName    = "redis-startup-config"
	redisReadinessVolumeName               = "redis-readiness-config"
	redisStorageVolumeName                 = "redis-data"
	sentinelStartupConfigurationVolumeName = "sentinel-startup-config"

	graceTime = 30
)

// startupScriptProbe returns the startup probe that runs the startup.sh of a
// startupConfigMap.
func startupScriptProbe(script string) *corev1.Probe {
	return &corev1.Probe{
		InitialDelaySeconds: graceTime,
		TimeoutSeconds:      5,
		FailureThreshold:    6,
		PeriodSeconds:       15,
		ProbeHandler: corev1.ProbeHandler{
			Exec: &corev1.ExecAction{
				Command: []string{"/bin/sh", script},
			},
		},
	}
}

// generateSentinelServiceAccount returns the ServiceAccount that the operator
// creates for Sentinel when sentinel.serviceAccountName is empty.
func generateSentinelServiceAccount(rf *redisfailoverv1.RedisFailover, labels map[string]string, ownerRefs []metav1.OwnerReference) *corev1.ServiceAccount {
	name := GetSentinelServiceAccountName(rf)
	selectorLabels := generateSelectorLabels(sentinelRoleName, rf.Name)
	labels = util.MergeLabels(labels, selectorLabels)

	return &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       rf.Namespace,
			Labels:          labels,
			OwnerReferences: ownerRefs,
		},
	}
}

func generatePodDisruptionBudget(name, namespace string, labels map[string]string, ownerRefs []metav1.OwnerReference, minAvailable intstr.IntOrString, selectorLabels map[string]string) *policyv1.PodDisruptionBudget {
	return &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       namespace,
			Labels:          labels,
			OwnerReferences: ownerRefs,
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MinAvailable: &minAvailable,
			Selector: &metav1.LabelSelector{
				MatchLabels: selectorLabels,
			},
		},
	}
}

func getAffinity(affinity *corev1.Affinity, labels map[string]string) *corev1.Affinity {
	if affinity != nil {
		return affinity
	}

	// Return a SOFT anti-affinity
	return &corev1.Affinity{
		PodAntiAffinity: &corev1.PodAntiAffinity{
			PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{
				{
					Weight: 100,
					PodAffinityTerm: corev1.PodAffinityTerm{
						TopologyKey: hostnameTopologyKey,
						LabelSelector: &metav1.LabelSelector{
							MatchLabels: labels,
						},
					},
				},
			},
		},
	}
}

// getSecurityContext returns the operator's default pod security context, with
// any field the user set on secctx taking precedence. A partial user context
// only overrides the fields it specifies instead of dropping all the defaults.
func getSecurityContext(secctx *corev1.PodSecurityContext) *corev1.PodSecurityContext {
	defaultUserAndGroup := new(int64(1000))

	result := &corev1.PodSecurityContext{
		RunAsUser:    defaultUserAndGroup,
		RunAsGroup:   defaultUserAndGroup,
		RunAsNonRoot: new(true),
		FSGroup:      defaultUserAndGroup,
		SeccompProfile: &corev1.SeccompProfile{
			Type: corev1.SeccompProfileTypeRuntimeDefault,
		},
	}
	if secctx == nil {
		return result
	}

	// Start from the user's context (so fields the operator has no default for
	// are preserved) and only fall back to a default where the user left it unset.
	merged := secctx.DeepCopy()
	if merged.RunAsUser == nil {
		merged.RunAsUser = result.RunAsUser
	}
	if merged.RunAsGroup == nil {
		merged.RunAsGroup = result.RunAsGroup
	}
	if merged.RunAsNonRoot == nil {
		merged.RunAsNonRoot = result.RunAsNonRoot
	}
	if merged.FSGroup == nil {
		merged.FSGroup = result.FSGroup
	}
	if merged.SeccompProfile == nil {
		merged.SeccompProfile = result.SeccompProfile
	}
	return merged
}

// getContainerSecurityContext returns the operator's default container security
// context, with any field the user set on secctx taking precedence. A partial
// user context only overrides the fields it specifies instead of dropping all
// the defaults.
func getContainerSecurityContext(secctx *corev1.SecurityContext) *corev1.SecurityContext {
	capabilities := &corev1.Capabilities{
		Add: []corev1.Capability{},
		Drop: []corev1.Capability{
			"ALL",
		},
	}
	defaultUserAndGroup := new(int64(1000))

	result := &corev1.SecurityContext{
		Capabilities:             capabilities,
		Privileged:               new(false),
		RunAsUser:                defaultUserAndGroup,
		RunAsGroup:               defaultUserAndGroup,
		RunAsNonRoot:             new(true),
		ReadOnlyRootFilesystem:   new(true),
		AllowPrivilegeEscalation: new(false),
	}
	if secctx == nil {
		return result
	}

	merged := secctx.DeepCopy()
	if merged.Capabilities == nil {
		merged.Capabilities = result.Capabilities
	}
	if merged.Privileged == nil {
		merged.Privileged = result.Privileged
	}
	if merged.RunAsUser == nil {
		merged.RunAsUser = result.RunAsUser
	}
	if merged.RunAsGroup == nil {
		merged.RunAsGroup = result.RunAsGroup
	}
	if merged.RunAsNonRoot == nil {
		merged.RunAsNonRoot = result.RunAsNonRoot
	}
	if merged.ReadOnlyRootFilesystem == nil {
		merged.ReadOnlyRootFilesystem = result.ReadOnlyRootFilesystem
	}
	if merged.AllowPrivilegeEscalation == nil {
		merged.AllowPrivilegeEscalation = result.AllowPrivilegeEscalation
	}
	return merged
}

// automountServiceAccountToken disables the token mount when the RedisFailover
// sets no service account. The generated containers do not use the Kubernetes
// API. A user who sets a service account may need its token in a sidecar.
func automountServiceAccountToken(serviceAccountName string) *bool {
	if serviceAccountName == "" {
		return new(false)
	}
	return nil
}

func getDNSPolicy(dnspolicy corev1.DNSPolicy) corev1.DNSPolicy {
	if dnspolicy == "" {
		return corev1.DNSClusterFirst
	}
	return dnspolicy
}

func getQuorum(rf *redisfailoverv1.RedisFailover) int32 {
	return rf.Spec.Sentinel.Replicas/2 + 1
}

func pullPolicy(specPolicy corev1.PullPolicy) corev1.PullPolicy {
	if specPolicy == "" {
		return corev1.PullAlways
	}
	return specPolicy
}
