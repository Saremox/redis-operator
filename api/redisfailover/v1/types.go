package v1

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// RedisFailover is a Redis master with replicas that the operator deploys and repairs.
// +kubebuilder:printcolumn:name="NAME",type="string",JSONPath=".metadata.name"
// +kubebuilder:printcolumn:name="REDIS",type="integer",JSONPath=".spec.redis.replicas"
// +kubebuilder:printcolumn:name="SENTINELS",type="integer",JSONPath=".spec.sentinel.replicas"
// +kubebuilder:printcolumn:name="AGE",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:resource:singular=redisfailover,path=redisfailovers,shortName=rf,scope=Namespaced
// +kubebuilder:subresource:status
type RedisFailover struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	// Spec is the desired state. The operator does not reconcile a spec that
	// fails validation, and the status then shows the error.
	Spec RedisFailoverSpec `json:"spec"`
	// Status is the state that the operator found in its last reconcile.
	Status RedisFailoverStatus `json:"status,omitempty"`
}

// RedisFailoverSpec is the desired state of a RedisFailover.
type RedisFailoverSpec struct {
	// Redis configures the Redis StatefulSet rfr-<NAME> and its pods.
	Redis RedisSettings `json:"redis,omitempty"`
	// Sentinel configures the Sentinel Deployment rfs-<NAME>, and selects
	// whether Sentinel or the operator does the failover.
	Sentinel SentinelSettings `json:"sentinel,omitempty"`
	// Auth configures the Redis password. Without it, Redis has no password.
	Auth AuthSettings `json:"auth,omitempty"`
	// LabelWhitelist is a list of RE2 regular expressions. The operator copies
	// a label of the RedisFailover to the objects that it creates only when
	// the label key matches one expression. Without a list, the operator
	// copies all labels. The operator ignores an expression that does not
	// compile.
	LabelWhitelist []string `json:"labelWhitelist,omitempty"`
	// BootstrapNode is an external Redis. All Redis pods replicate from it, so
	// that the clients can move from that Redis to this RedisFailover.
	BootstrapNode *BootstrapSettings `json:"bootstrapNode,omitempty"`
}

// RedisCommandRename is a "rename-command" entry of redis.conf.
// It must not rename a command that the operator, the pod scripts or the replicas send:
// AUTH, CLIENT, CONFIG, INFO, PING, PSYNC, REPLCONF, REPLICAOF or SLAVEOF. With Sentinels,
// it must not rename EXEC, MULTI, PUBLISH or SUBSCRIBE. With an aclfile, it must not rename ACL.
// With Sentinel, the shutdown script also sends SAVE. Validation allows a rename of SAVE, but then that SAVE fails.
type RedisCommandRename struct {
	// From is the command to rename. It can contain only letters and
	// underscores.
	From string `json:"from,omitempty"`
	// To is the name that replaces From. It can contain only letters and
	// underscores. An empty value disables the command.
	To string `json:"to,omitempty"`
}

// RedisSettings configures the Redis pods.
type RedisSettings struct {
	// Image is the image of the Redis container and of the init container that
	// removes RDB tempfiles. Without a value, the operator uses its default
	// Redis image.
	Image string `json:"image,omitempty"`
	// ImagePullPolicy applies to the Redis container and to the init
	// container that removes RDB tempfiles. Defaults to Always.
	ImagePullPolicy corev1.PullPolicy `json:"imagePullPolicy,omitempty"`
	// Replicas is the number of Redis pods. Defaults to 3.
	Replicas int32 `json:"replicas,omitempty"`
	// Port is the Redis port in each pod and in the Services rfrm-<NAME> and
	// rfrs-<NAME>. Defaults to 6379.
	Port int32 `json:"port,omitempty"`
	// Resources are the compute resources of the Redis container. maxMemory
	// and autoscaling use its memory limit.
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
	// Env adds environment variables to the Redis container. The operator
	// variables REDIS_ADDR, REDIS_PORT, REDIS_USER and, with auth.secretPath,
	// REDIS_PASSWORD replace a variable with the same name. Do not set REDIS_PASSWORD here:
	// without auth.secretPath, the pod scripts use it as the password.
	Env []corev1.EnvVar `json:"env,omitempty"`
	// CustomConfig is a list of "parameter value" entries. The operator
	// applies each entry with CONFIG SET on each Redis, and sends ACL LOAD for
	// aclfile, so a change restarts no pod. Do not set port, bind or dir,
	// because the operator controls them.
	CustomConfig []string `json:"customConfig,omitempty"`
	// CustomCommandRenames writes rename-command entries to redis.conf.
	// Validation rejects a rename of a command that the RedisFailover needs.
	CustomCommandRenames []RedisCommandRename `json:"customCommandRenames,omitempty"`
	// Command replaces the default redis command. With auth.secretPath, the
	// default passes the password to redis-server, so a custom command must
	// pass --requirepass and --masterauth from $REDIS_PASSWORD itself.
	// Otherwise Redis runs with no password.
	Command []string `json:"command,omitempty"`
	// ShutdownConfigMap is the name of a ConfigMap with a shutdown.sh key, in
	// the namespace of the RedisFailover. Its script replaces the default
	// preStop script, also the write pause before a Sentinel failover. The
	// ConfigMap must exist, or the reconcile fails.
	ShutdownConfigMap string `json:"shutdownConfigMap,omitempty"`
	// StartupConfigMap is the name of a ConfigMap with a startup.sh key. The
	// operator mounts it at /redis-startup and runs the script as the startup
	// probe, unless customStartupProbe is set.
	StartupConfigMap string `json:"startupConfigMap,omitempty"`
	// Storage configures the data volume at /data. Without a value, it is an
	// emptyDir, so Kubernetes deletes the data with the pod.
	Storage RedisStorage `json:"storage,omitempty"`
	// InitContainers run after the init container that removes RDB tempfiles.
	// The operator adds REDIS_ADDR, REDIS_PORT, REDIS_USER and, with
	// auth.secretPath, REDIS_PASSWORD to their environment.
	InitContainers []corev1.Container `json:"initContainers,omitempty"`
	// Exporter configures a metrics exporter sidecar in each Redis pod. When it
	// is enabled, the operator also creates the Service rfr-<NAME> for its port.
	Exporter Exporter `json:"exporter,omitempty"`
	// ExtraContainers are more containers in each Redis pod. They get the same
	// environment variables of the operator as initContainers.
	ExtraContainers []corev1.Container `json:"extraContainers,omitempty"`
	// Affinity replaces the default affinity of the Redis pods. The default is
	// a preferred pod anti-affinity, so the scheduler tries to put each Redis
	// pod on a different node.
	Affinity *corev1.Affinity `json:"affinity,omitempty"`
	// SecurityContext is the pod security context of the Redis pods. A field
	// that is not set keeps the default of the operator: user, group and
	// fsGroup 1000, runAsNonRoot and the RuntimeDefault seccomp profile.
	SecurityContext *corev1.PodSecurityContext `json:"securityContext,omitempty"`
	// ContainerSecurityContext applies to the Redis container and to the init
	// container that removes RDB tempfiles. A field that is not set keeps the
	// default of the operator. A capabilities value replaces the default drop
	// of ALL.
	ContainerSecurityContext *corev1.SecurityContext `json:"containerSecurityContext,omitempty"`
	// ImagePullSecrets are the Secrets that the Redis pods use to pull their
	// images.
	ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`
	// Tolerations are the tolerations of the Redis pods.
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`
	// TopologySpreadConstraints are the topology spread constraints of the
	// Redis pods.
	TopologySpreadConstraints []corev1.TopologySpreadConstraint `json:"topologySpreadConstraints,omitempty"`
	// NodeSelector limits the Redis pods to the nodes with these labels.
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	// PodAnnotations are annotations that the operator adds to the Redis pods.
	// The operator also sets the annotation redisfailovers.databases.spotahome.com/secret-checksum, so
	// that a password change restarts the pods.
	PodAnnotations map[string]string `json:"podAnnotations,omitempty"`
	// ServiceAnnotations are annotations that the operator adds to the
	// Services rfr-<NAME>, rfrm-<NAME> and rfrs-<NAME>. On rfr-<NAME>, they replace a default prometheus.io
	// annotation with the same key.
	ServiceAnnotations map[string]string `json:"serviceAnnotations,omitempty"`
	// HostNetwork runs the Redis pods in the network namespace of the node.
	HostNetwork bool `json:"hostNetwork,omitempty"`
	// DNSPolicy is the DNS policy of the Redis pods. Defaults to ClusterFirst.
	DNSPolicy corev1.DNSPolicy `json:"dnsPolicy,omitempty"`
	// PriorityClassName is the PriorityClass of the Redis pods. The operator
	// does not create the PriorityClass.
	PriorityClassName string `json:"priorityClassName,omitempty"`
	// ServiceAccountName is the ServiceAccount of the Redis pods, which the
	// user creates. Without a value, the pods use the default ServiceAccount
	// and do not mount its token.
	ServiceAccountName string `json:"serviceAccountName,omitempty"`
	// TerminationGracePeriodSeconds is the grace period of the Redis pods in
	// seconds. Defaults to 30. The shutdown script must end in this time. It
	// needs the time of its SAVE, plus about 19s with Sentinel.
	TerminationGracePeriodSeconds int64 `json:"terminationGracePeriod,omitempty"`
	// ExtraVolumes are more volumes in the Redis pods.
	ExtraVolumes []corev1.Volume `json:"extraVolumes,omitempty"`
	// ExtraVolumeMounts are more volume mounts in the Redis container.
	ExtraVolumeMounts []corev1.VolumeMount `json:"extraVolumeMounts,omitempty"`
	// CustomLivenessProbe replaces the default liveness probe of the Redis
	// container, which sends PING.
	CustomLivenessProbe *corev1.Probe `json:"customLivenessProbe,omitempty"`
	// CustomReadinessProbe replaces the default readiness probe of the Redis
	// container. The default probe keeps a replica not ready while it syncs
	// with the master, or has no master.
	CustomReadinessProbe *corev1.Probe `json:"customReadinessProbe,omitempty"`
	// CustomStartupProbe is the startup probe of the Redis container. Without
	// it, the container has a startup probe only with startupConfigMap.
	CustomStartupProbe *corev1.Probe `json:"customStartupProbe,omitempty"`
	// DisablePodDisruptionBudget removes the PodDisruptionBudget of the Redis
	// pods. Defaults to false.
	DisablePodDisruptionBudget bool `json:"disablePodDisruptionBudget,omitempty"`
	// PreventMasterEviction, when true, annotates the current master pod with
	// cluster-autoscaler.kubernetes.io/safe-to-evict=false so the cluster
	// autoscaler does not drain the node of the master. The operator marks the
	// replicas as evictable. Defaults to false.
	PreventMasterEviction bool `json:"preventMasterEviction,omitempty"`
	// PodDisruptionBudgetMinAvailable overrides the PodDisruptionBudget
	// minAvailable for the redis pods. Defaults to 2 (or 1 when replicas <= 2).
	PodDisruptionBudgetMinAvailable *intstr.IntOrString `json:"podDisruptionBudgetMinAvailable,omitempty"`
	// MaxMemory lets the operator set maxmemory and maxmemory-policy from the
	// redis container's memory limit. Values set in customConfig take precedence.
	// It needs a memory limit of at least 64Mi. It uses the smallest limit of
	// all redis pods, because a failover can promote any replica. It sets
	// replica-ignore-maxmemory yes, so customConfig cannot set it to no.
	// Only an allkeys-* policy lowers maxmemory below the memory in use. With
	// another policy, the operator keeps the old value and holds the pod rollout.
	MaxMemory *MaxMemorySettings `json:"maxMemory,omitempty"`
	// InPlaceResize controls whether the operator resizes a redis pod in place
	// when an update changes only the container resources. Otherwise the
	// operator recreates the pod. A resize needs Kubernetes 1.33 or later, and
	// 1.35 to lower a memory limit. Defaults to Enabled.
	// +kubebuilder:validation:Enum=Enabled;Disabled
	InPlaceResize string `json:"inPlaceResize,omitempty"`
	// Autoscaling lets the operator compute the memory limit of the redis
	// container from the memory use. Unset means Off.
	Autoscaling *AutoscalingSettings `json:"autoscaling,omitempty"`
}

// AutoscalingSettings configures the memory autoscaler. The memory limit in
// redis.resources is the lower bound, and memoryMax is the upper bound.
type AutoscalingSettings struct {
	// Mode is Off, Recommend or Auto. Recommend shows a memory limit in
	// status.autoscaling and changes no pod. Auto also raises the memory
	// limit of the pods in place. It needs redis.maxMemory, a memory request
	// equal to the limit or unset, and Kubernetes 1.33 or later. With
	// bootstrapNode, only Off is valid. Defaults to Off.
	// +kubebuilder:validation:Enum=Off;Recommend;Auto
	Mode string `json:"mode,omitempty"`
	// MemoryMax is the largest memory limit that the autoscaler recommends.
	// It must not be less than redis.resources.limits.memory. When it is
	// equal, the autoscaler recommends no step. The modes Recommend and Auto
	// need it.
	MemoryMax *resource.Quantity `json:"memoryMax,omitempty"`
	// RecreateAfter is reserved for the time after which Auto recreates a pod
	// that it cannot resize in place. The operator does not use it, and
	// validation rejects it with Auto. Other modes accept 0s, or 5m and more.
	// The value has at most 16 characters and each number at most 6 digits,
	// because a value that overflows stops the decode of all RedisFailovers.
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MaxLength=16
	// +kubebuilder:validation:Pattern=`^[-+]?(0|(([0-9]{1,6}(\.[0-9]{0,6})?|\.[0-9]{1,6})(ns|us|µs|μs|ms|s|m|h))+)$`
	RecreateAfter *metav1.Duration `json:"recreateAfter,omitempty"`
	// Workload is Auto, Store or Cache. Auto uses the maxmemory-policy:
	// allkeys-* is a cache, noeviction a store, and volatile-* a store until
	// Redis evicts a key. Defaults to Auto.
	// +kubebuilder:validation:Enum=Auto;Store;Cache
	Workload string `json:"workload,omitempty"`
}

// MaxMemorySettings configures the operator-managed maxmemory.
type MaxMemorySettings struct {
	// Percent of the memory limit used as maxmemory. At least 32Mi of the
	// limit is always left free. Defaults to 75.
	// +kubebuilder:validation:Minimum=10
	// +kubebuilder:validation:Maximum=95
	Percent int32 `json:"percent,omitempty"`
	// Policy is the maxmemory-policy. Defaults to noeviction.
	// +kubebuilder:validation:Enum=noeviction;allkeys-lru;allkeys-lfu;allkeys-random;volatile-lru;volatile-lfu;volatile-random;volatile-ttl
	Policy string `json:"policy,omitempty"`
}

// SentinelSettings configures the Sentinel pods and the failover.
type SentinelSettings struct {
	// Enabled deploys Sentinel, and Sentinel does the failover. When false,
	// the operator does the failover. Defaults to false.
	Enabled *bool `json:"enabled,omitempty"`
	// FailoverTimeout applies only when enabled is false. It is the time that the
	// operator waits for a master that does not answer while its pod runs.
	// Thus a short stall causes no failover. A master that is gone or not found
	// gets no wait. Defaults to 10s; 0s disables the wait.
	FailoverTimeout *metav1.Duration `json:"failoverTimeout,omitempty"`
	// Image is the image of the Sentinel container and of the init container
	// that copies sentinel.conf. Without a value, the operator uses its
	// default Redis image.
	Image string `json:"image,omitempty"`
	// ImagePullPolicy applies to the Sentinel container and to the init
	// container that copies sentinel.conf. Defaults to Always.
	ImagePullPolicy corev1.PullPolicy `json:"imagePullPolicy,omitempty"`
	// Replicas is the number of Sentinel pods. Defaults to 3. The quorum of
	// the Sentinels is replicas / 2 + 1.
	Replicas int32 `json:"replicas,omitempty"`
	// Resources are the compute resources of the Sentinel container.
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
	// Env adds environment variables to the Sentinel container. The operator
	// adds no variables of its own to this container.
	Env []corev1.EnvVar `json:"env,omitempty"`
	// CustomConfig is a list of "parameter value" entries. The operator
	// applies each entry with SENTINEL SET mymaster on each Sentinel. It adds
	// down-after-milliseconds 5000 and failover-timeout 10000, unless the
	// list sets them.
	CustomConfig []string `json:"customConfig,omitempty"`
	// Command replaces the default Sentinel command, redis-server
	// /redis/sentinel.conf --sentinel.
	Command []string `json:"command,omitempty"`
	// StartupConfigMap is the name of a ConfigMap with a startup.sh key. The
	// operator mounts it at /sentinel-startup and runs the script as the
	// startup probe, unless customStartupProbe is set.
	StartupConfigMap string `json:"startupConfigMap,omitempty"`
	// Affinity replaces the default affinity of the Sentinel pods. The default
	// is a preferred pod anti-affinity, so the scheduler tries to put each
	// Sentinel pod on a different node.
	Affinity *corev1.Affinity `json:"affinity,omitempty"`
	// SecurityContext is the pod security context of the Sentinel pods. A
	// field that is not set keeps the default of the operator, as in
	// redis.securityContext.
	SecurityContext *corev1.PodSecurityContext `json:"securityContext,omitempty"`
	// ContainerSecurityContext applies to the Sentinel container. A field that
	// is not set keeps the default of the operator. A capabilities value
	// replaces the default drop of ALL.
	ContainerSecurityContext *corev1.SecurityContext `json:"containerSecurityContext,omitempty"`
	// ImagePullSecrets are the Secrets that the Sentinel pods use to pull
	// their images.
	ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`
	// Tolerations are the tolerations of the Sentinel pods.
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`
	// TopologySpreadConstraints are the topology spread constraints of the
	// Sentinel pods.
	TopologySpreadConstraints []corev1.TopologySpreadConstraint `json:"topologySpreadConstraints,omitempty"`
	// NodeSelector limits the Sentinel pods to the nodes with these labels.
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	// PodAnnotations are annotations that the operator adds to the Sentinel
	// pods.
	PodAnnotations map[string]string `json:"podAnnotations,omitempty"`
	// ServiceAnnotations are annotations that the operator adds to the
	// Sentinel Service rfs-<NAME>.
	ServiceAnnotations map[string]string `json:"serviceAnnotations,omitempty"`
	// InitContainers run after the init container that copies sentinel.conf.
	InitContainers []corev1.Container `json:"initContainers,omitempty"`
	// Exporter configures a metrics exporter sidecar in each Sentinel pod. When
	// it is enabled, the Service rfs-<NAME> also exposes its port.
	Exporter Exporter `json:"exporter,omitempty"`
	// ExtraContainers are more containers in each Sentinel pod.
	ExtraContainers []corev1.Container `json:"extraContainers,omitempty"`
	// ConfigCopy configures the init container sentinel-config-copy.
	ConfigCopy SentinelConfigCopy `json:"configCopy,omitempty"`
	// HostNetwork runs the Sentinel pods in the network namespace of the node.
	HostNetwork bool `json:"hostNetwork,omitempty"`
	// DNSPolicy is the DNS policy of the Sentinel pods. Defaults to
	// ClusterFirst.
	DNSPolicy corev1.DNSPolicy `json:"dnsPolicy,omitempty"`
	// PriorityClassName is the PriorityClass of the Sentinel pods. The
	// operator does not create the PriorityClass.
	PriorityClassName string `json:"priorityClassName,omitempty"`
	// ServiceAccountName is the ServiceAccount of the Sentinel pods, which the
	// user creates. Without a value, the operator creates the ServiceAccount
	// rfs-sa-<NAME>, and the pods do not mount its token.
	ServiceAccountName string `json:"serviceAccountName,omitempty"`
	// ExtraVolumes are more volumes in the Sentinel pods.
	ExtraVolumes []corev1.Volume `json:"extraVolumes,omitempty"`
	// ExtraVolumeMounts are more volume mounts in the Sentinel container.
	ExtraVolumeMounts []corev1.VolumeMount `json:"extraVolumeMounts,omitempty"`
	// CustomLivenessProbe replaces the default liveness probe of the Sentinel
	// container, which sends PING.
	CustomLivenessProbe *corev1.Probe `json:"customLivenessProbe,omitempty"`
	// CustomReadinessProbe replaces the default readiness probe of the
	// Sentinel container. The default probe keeps a Sentinel not ready until
	// it monitors a master and SENTINEL CKQUORUM succeeds.
	CustomReadinessProbe *corev1.Probe `json:"customReadinessProbe,omitempty"`
	// CustomStartupProbe is the startup probe of the Sentinel container.
	// Without it, the container has a startup probe only with
	// startupConfigMap.
	CustomStartupProbe *corev1.Probe `json:"customStartupProbe,omitempty"`
	// DisablePodDisruptionBudget removes the PodDisruptionBudget of the
	// Sentinel pods. Defaults to false.
	DisablePodDisruptionBudget bool `json:"disablePodDisruptionBudget,omitempty"`
	// Strategy overrides the update strategy of the sentinel Deployment, for
	// example to set rollingUpdate maxSurge and maxUnavailable. Defaults to the
	// Kubernetes default RollingUpdate strategy.
	Strategy appsv1.DeploymentStrategy `json:"strategy,omitempty"`
	// PodDisruptionBudgetMinAvailable overrides the PodDisruptionBudget
	// minAvailable for the sentinel pods. Defaults to 2 (or 1 when sentinel
	// replicas <= 2).
	PodDisruptionBudgetMinAvailable *intstr.IntOrString `json:"podDisruptionBudgetMinAvailable,omitempty"`
}

// AuthSettings configures the Redis password.
type AuthSettings struct {
	// SecretPath is the name of a Secret in the namespace of the RedisFailover.
	// Its key password is the Redis password. Sentinel has no password.
	SecretPath string `json:"secretPath,omitempty"`
}

// BootstrapSettings names an external Redis that the Redis pods replicate from.
type BootstrapSettings struct {
	// Host is the IP address of the external Redis, or the ClusterIP of a
	// Service. Validation requires it.
	Host string `json:"host,omitempty"`
	// Port is the port of the external Redis. Defaults to 6379.
	Port string `json:"port,omitempty"`
	// AllowSentinels lets the operator deploy the Sentinels, which then
	// monitor the external Redis. It applies only when sentinel.enabled is
	// true. Defaults to false.
	AllowSentinels bool `json:"allowSentinels,omitempty"`
}

// Exporter configures the metrics exporter sidecar of the Redis or Sentinel pods.
type Exporter struct {
	// Enabled adds the exporter sidecar. Defaults to false.
	Enabled bool `json:"enabled,omitempty"`
	// Image is the image of the exporter container. Without a value, the
	// operator uses its default redis_exporter image.
	Image string `json:"image,omitempty"`
	// ImagePullPolicy is the image pull policy of the exporter container.
	// Defaults to Always.
	ImagePullPolicy corev1.PullPolicy `json:"imagePullPolicy,omitempty"`
	// ContainerSecurityContext applies to the exporter container. A field
	// that is not set keeps the default of the operator.
	ContainerSecurityContext *corev1.SecurityContext `json:"containerSecurityContext,omitempty"`
	// Args are the arguments of the exporter container.
	Args []string `json:"args,omitempty"`
	// Env adds environment variables to the exporter container. In the Redis
	// exporter, a REDIS_USER or REDIS_PASSWORD entry replaces the value of
	// the operator. The variables of the operator replace other entries with
	// the same name.
	Env []corev1.EnvVar `json:"env,omitempty"`
	// Resources replace the default resources of the exporter container:
	// requests of 10m CPU and 50Mi memory, limits of 1000m CPU and 100Mi
	// memory.
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`
	// Port the exporter sidecar listens on and the metrics service exposes.
	// Defaults to 9121 for the redis exporter and 9355 for the sentinel exporter
	// when left as 0.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port,omitempty"`
}

// SentinelConfigCopy configures the init container that copies sentinel.conf
// to a writable volume. Sentinel writes its state to that file.
type SentinelConfigCopy struct {
	// ContainerSecurityContext applies to the init container. A field that is
	// not set keeps the default of the operator.
	ContainerSecurityContext *corev1.SecurityContext `json:"containerSecurityContext,omitempty"`
}

// RedisStorage configures the data volume of the Redis pods.
type RedisStorage struct {
	// KeepAfterDeletion keeps the PVCs when Kubernetes deletes the
	// RedisFailover. The operator removes the owner reference of the
	// RedisFailover from the PVCs. When the field is false again, the operator
	// does not add the owner reference.
	KeepAfterDeletion bool `json:"keepAfterDeletion,omitempty"`
	// EmptyDir configures the emptyDir data volume, for example its medium or
	// sizeLimit. The operator ignores it when persistentVolumeClaim is set.
	EmptyDir *corev1.EmptyDirVolumeSource `json:"emptyDir,omitempty"`
	// PersistentVolumeClaim is the volume claim template of the Redis
	// StatefulSet. The operator increases the storage request of the PVCs to
	// a larger request here. It does not make a PVC smaller. The operator
	// ignores other changes to the claim.
	PersistentVolumeClaim *EmbeddedPersistentVolumeClaim `json:"persistentVolumeClaim,omitempty"`
}

// EmbeddedPersistentVolumeClaim is an embedded version of k8s.io/api/core/v1.PersistentVolumeClaim.
// It contains TypeMeta and a reduced ObjectMeta.
type EmbeddedPersistentVolumeClaim struct {
	metav1.TypeMeta `json:",inline"`

	// EmbeddedObjectMetadata holds the name, the labels and the annotations of the PVC.
	EmbeddedObjectMetadata `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	// Spec defines the desired characteristics of a volume requested by a pod author.
	// More info: https://kubernetes.io/docs/concepts/storage/persistent-volumes#persistentvolumeclaims
	// +optional
	Spec corev1.PersistentVolumeClaimSpec `json:"spec,omitempty" protobuf:"bytes,2,opt,name=spec"`

	// Status represents the current information/status of a persistent volume claim.
	// Read-only.
	// More info: https://kubernetes.io/docs/concepts/storage/persistent-volumes#persistentvolumeclaims
	// +optional
	Status corev1.PersistentVolumeClaimStatus `json:"status,omitempty" protobuf:"bytes,3,opt,name=status"`
}

// EmbeddedObjectMetadata contains a subset of the fields included in k8s.io/apimachinery/pkg/apis/meta/v1.ObjectMeta
// Only fields which are relevant to embedded resources are included.
type EmbeddedObjectMetadata struct {
	// Name must be unique within a namespace. Is required when creating resources, although
	// some resources may allow a client to request the generation of an appropriate name
	// automatically. Name is primarily intended for creation idempotence and configuration
	// definition.
	// Cannot be updated.
	// More info: http://kubernetes.io/docs/user-guide/identifiers#names
	// +optional
	Name string `json:"name,omitempty" protobuf:"bytes,1,opt,name=name"`

	// Map of string keys and values that can be used to organize and categorize
	// (scope and select) objects. May match selectors of replication controllers
	// and services.
	// More info: http://kubernetes.io/docs/user-guide/labels
	// +optional
	Labels map[string]string `json:"labels,omitempty" protobuf:"bytes,11,rep,name=labels"`

	// Annotations is an unstructured key value map stored with a resource that may be
	// set by external tools to store and retrieve arbitrary metadata. They are not
	// queryable and should be preserved when modifying objects.
	// More info: http://kubernetes.io/docs/user-guide/annotations
	// +optional
	Annotations map[string]string `json:"annotations,omitempty" protobuf:"bytes,12,rep,name=annotations"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// RedisFailoverList is the result of a list request for RedisFailovers.
type RedisFailoverList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`

	// Items holds the RedisFailovers that the list request returned.
	Items []RedisFailover `json:"items"`
}

// RedisFailoverStatus is the result of the last reconcile. State is Healthy or
// NotHealthy, and Message gives the reason.
type RedisFailoverStatus struct {
	// State is Healthy or NotHealthy.
	State string `json:"state,omitempty"`
	// LastChanged is the RFC3339 time of the last change of state.
	LastChanged string `json:"lastChanged,omitempty"`
	// Message gives the reason for a NotHealthy state. With a Healthy state,
	// it can show a wait of the operator, for example in a pod rollout.
	Message string `json:"message,omitempty"`
	// Autoscaling is the last result of redis.autoscaling. It is empty when
	// the mode is Off.
	// +optional
	Autoscaling AutoscalingStatus `json:"autoscaling,omitzero"`
}

// AutoscalingStatus holds only scalar fields, because the operator compares
// the status with == to skip a write that changes nothing.
type AutoscalingStatus struct {
	// Workload is Store or Cache.
	Workload string `json:"workload,omitempty"`
	// RecommendedMemory is the memory limit that the autoscaler recommends.
	// It is empty when the autoscaler recommends no change.
	RecommendedMemory string `json:"recommendedMemory,omitempty"`
	// Reason tells why the autoscaler recommends a change or no change.
	Reason string `json:"reason,omitempty"`
	// EvaluatedAt is the RFC3339 time, in full minutes, of the last change of
	// the recommendation or of highWindows.
	EvaluatedAt string `json:"evaluatedAt,omitempty"`
	// HighWindows is the number of consecutive 5-minute windows with high
	// memory use, 3 at most.
	HighWindows int32 `json:"highWindows,omitempty"`
	// EffectiveMemory is the memory limit of the redis container when it
	// differs from redis.resources.limits.memory.
	EffectiveMemory string `json:"effectiveMemory,omitempty"`
	// Phase is the phase of the last autoscaler step: Idle, Applying, Done,
	// Reverted or Held.
	Phase string `json:"phase,omitempty"`
	// LastStep is the RFC3339 time, in full minutes, of the start of the
	// last step.
	LastStep string `json:"lastStep,omitempty"`
	// RetryAfter is the RFC3339 time, in full minutes, after which a step
	// can start again after a revert.
	RetryAfter string `json:"retryAfter,omitempty"`
}
