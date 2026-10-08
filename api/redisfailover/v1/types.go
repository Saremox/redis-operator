package v1

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
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
	Spec              RedisFailoverSpec   `json:"spec"`
	Status            RedisFailoverStatus `json:"status,omitempty"`
}

// RedisFailoverSpec is the desired state of a RedisFailover.
type RedisFailoverSpec struct {
	Redis          RedisSettings      `json:"redis,omitempty"`
	Sentinel       SentinelSettings   `json:"sentinel,omitempty"`
	Auth           AuthSettings       `json:"auth,omitempty"`
	LabelWhitelist []string           `json:"labelWhitelist,omitempty"`
	BootstrapNode  *BootstrapSettings `json:"bootstrapNode,omitempty"`
	// TLS adds a TLS port to Redis. Validation rejects this field in this
	// release, because the operator does not yet change a running
	// RedisFailover to TLS or renew its certificates.
	TLS *TLSSettings `json:"tls,omitempty"`
}

// TLSSettings configures TLS for the Redis pods and for the connections of
// the operator. Without it, Redis uses no TLS.
type TLSSettings struct {
	// SecretName is the Secret with tls.crt, tls.key and ca.crt, for example
	// from cert-manager. The Redis pods and the operator use the same
	// certificate. Thus with clientAuth Required or Optional, the certificate
	// needs the client auth usage next to server auth.
	SecretName string `json:"secretName"`
	// CA replaces ca.crt of secretName as the trusted CA bundle.
	CA *TLSCASource `json:"ca,omitempty"`
	// Port is the TLS port of Redis. It is separate from redis.port, so it
	// does not change when the plaintext port closes. Defaults to 6380.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port,omitempty"`
	// PlaintextPort keeps redis.port open next to the TLS port for the
	// applications. Disabled closes it. Defaults to Enabled.
	// +kubebuilder:validation:Enum=Enabled;Disabled
	PlaintextPort string `json:"plaintextPort,omitempty"`
	// ClientAuth sets tls-auth-clients: Required (yes), Optional or None
	// (no). Defaults to Required.
	// +kubebuilder:validation:Enum=Required;Optional;None
	ClientAuth string `json:"clientAuth,omitempty"`
	// ServerName is the name that the operator checks in the certificate of
	// each Redis pod, because pod IPs change. Defaults to the DNS name of the
	// master Service, rfrm-<name>.<namespace>.svc.
	ServerName string `json:"serverName,omitempty"`
}

// TLSCASource names one Secret or one ConfigMap that holds the CA bundle.
type TLSCASource struct {
	SecretName    string `json:"secretName,omitempty"`
	ConfigMapName string `json:"configMapName,omitempty"`
	// Key is the key of the CA bundle. Defaults to ca.crt.
	Key string `json:"key,omitempty"`
}

// RedisCommandRename is a "rename-command" entry of redis.conf.
// It must not rename a command that the operator, the pod scripts or the replicas send:
// AUTH, CLIENT, CONFIG, INFO, PING, PSYNC, REPLCONF, REPLICAOF or SLAVEOF. With Sentinels,
// it must not rename EXEC, MULTI, PUBLISH or SUBSCRIBE. With an aclfile, it must not rename ACL.
// The shutdown script also sends SAVE. Validation allows a rename of SAVE, but then that SAVE fails.
type RedisCommandRename struct {
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
}

// RedisSettings configures the Redis pods.
type RedisSettings struct {
	Image                string                      `json:"image,omitempty"`
	ImagePullPolicy      corev1.PullPolicy           `json:"imagePullPolicy,omitempty"`
	Replicas             int32                       `json:"replicas,omitempty"`
	Port                 int32                       `json:"port,omitempty"`
	Resources            corev1.ResourceRequirements `json:"resources,omitempty"`
	Env                  []corev1.EnvVar             `json:"env,omitempty"`
	CustomConfig         []string                    `json:"customConfig,omitempty"`
	CustomCommandRenames []RedisCommandRename        `json:"customCommandRenames,omitempty"`
	// Command replaces the default redis command. With auth.secretPath, the
	// default passes the password to redis-server, so a custom command must
	// pass --requirepass and --masterauth from $REDIS_PASSWORD itself.
	// Otherwise Redis runs with no password.
	Command                   []string                          `json:"command,omitempty"`
	ShutdownConfigMap         string                            `json:"shutdownConfigMap,omitempty"`
	StartupConfigMap          string                            `json:"startupConfigMap,omitempty"`
	Storage                   RedisStorage                      `json:"storage,omitempty"`
	InitContainers            []corev1.Container                `json:"initContainers,omitempty"`
	Exporter                  Exporter                          `json:"exporter,omitempty"`
	ExtraContainers           []corev1.Container                `json:"extraContainers,omitempty"`
	Affinity                  *corev1.Affinity                  `json:"affinity,omitempty"`
	SecurityContext           *corev1.PodSecurityContext        `json:"securityContext,omitempty"`
	ContainerSecurityContext  *corev1.SecurityContext           `json:"containerSecurityContext,omitempty"`
	ImagePullSecrets          []corev1.LocalObjectReference     `json:"imagePullSecrets,omitempty"`
	Tolerations               []corev1.Toleration               `json:"tolerations,omitempty"`
	TopologySpreadConstraints []corev1.TopologySpreadConstraint `json:"topologySpreadConstraints,omitempty"`
	NodeSelector              map[string]string                 `json:"nodeSelector,omitempty"`
	PodAnnotations            map[string]string                 `json:"podAnnotations,omitempty"`
	ServiceAnnotations        map[string]string                 `json:"serviceAnnotations,omitempty"`
	HostNetwork               bool                              `json:"hostNetwork,omitempty"`
	DNSPolicy                 corev1.DNSPolicy                  `json:"dnsPolicy,omitempty"`
	PriorityClassName         string                            `json:"priorityClassName,omitempty"`
	ServiceAccountName        string                            `json:"serviceAccountName,omitempty"`
	// TerminationGracePeriodSeconds is the grace period of the Redis pods in
	// seconds. Defaults to 30. The shutdown script must end in this time. It
	// needs the time of its SAVE, plus about 19s with Sentinel.
	TerminationGracePeriodSeconds int64                `json:"terminationGracePeriod,omitempty"`
	ExtraVolumes                  []corev1.Volume      `json:"extraVolumes,omitempty"`
	ExtraVolumeMounts             []corev1.VolumeMount `json:"extraVolumeMounts,omitempty"`
	CustomLivenessProbe           *corev1.Probe        `json:"customLivenessProbe,omitempty"`
	CustomReadinessProbe          *corev1.Probe        `json:"customReadinessProbe,omitempty"`
	CustomStartupProbe            *corev1.Probe        `json:"customStartupProbe,omitempty"`
	DisablePodDisruptionBudget    bool                 `json:"disablePodDisruptionBudget,omitempty"`
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
	FailoverTimeout            *metav1.Duration                  `json:"failoverTimeout,omitempty"`
	Image                      string                            `json:"image,omitempty"`
	ImagePullPolicy            corev1.PullPolicy                 `json:"imagePullPolicy,omitempty"`
	Replicas                   int32                             `json:"replicas,omitempty"`
	Resources                  corev1.ResourceRequirements       `json:"resources,omitempty"`
	Env                        []corev1.EnvVar                   `json:"env,omitempty"`
	CustomConfig               []string                          `json:"customConfig,omitempty"`
	Command                    []string                          `json:"command,omitempty"`
	StartupConfigMap           string                            `json:"startupConfigMap,omitempty"`
	Affinity                   *corev1.Affinity                  `json:"affinity,omitempty"`
	SecurityContext            *corev1.PodSecurityContext        `json:"securityContext,omitempty"`
	ContainerSecurityContext   *corev1.SecurityContext           `json:"containerSecurityContext,omitempty"`
	ImagePullSecrets           []corev1.LocalObjectReference     `json:"imagePullSecrets,omitempty"`
	Tolerations                []corev1.Toleration               `json:"tolerations,omitempty"`
	TopologySpreadConstraints  []corev1.TopologySpreadConstraint `json:"topologySpreadConstraints,omitempty"`
	NodeSelector               map[string]string                 `json:"nodeSelector,omitempty"`
	PodAnnotations             map[string]string                 `json:"podAnnotations,omitempty"`
	ServiceAnnotations         map[string]string                 `json:"serviceAnnotations,omitempty"`
	InitContainers             []corev1.Container                `json:"initContainers,omitempty"`
	Exporter                   Exporter                          `json:"exporter,omitempty"`
	ExtraContainers            []corev1.Container                `json:"extraContainers,omitempty"`
	ConfigCopy                 SentinelConfigCopy                `json:"configCopy,omitempty"`
	HostNetwork                bool                              `json:"hostNetwork,omitempty"`
	DNSPolicy                  corev1.DNSPolicy                  `json:"dnsPolicy,omitempty"`
	PriorityClassName          string                            `json:"priorityClassName,omitempty"`
	ServiceAccountName         string                            `json:"serviceAccountName,omitempty"`
	ExtraVolumes               []corev1.Volume                   `json:"extraVolumes,omitempty"`
	ExtraVolumeMounts          []corev1.VolumeMount              `json:"extraVolumeMounts,omitempty"`
	CustomLivenessProbe        *corev1.Probe                     `json:"customLivenessProbe,omitempty"`
	CustomReadinessProbe       *corev1.Probe                     `json:"customReadinessProbe,omitempty"`
	CustomStartupProbe         *corev1.Probe                     `json:"customStartupProbe,omitempty"`
	DisablePodDisruptionBudget bool                              `json:"disablePodDisruptionBudget,omitempty"`
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
	SecretPath string `json:"secretPath,omitempty"`
}

// BootstrapSettings names an external Redis that the Redis pods replicate from.
type BootstrapSettings struct {
	Host           string `json:"host,omitempty"`
	Port           string `json:"port,omitempty"`
	AllowSentinels bool   `json:"allowSentinels,omitempty"`
}

// Exporter configures the metrics exporter sidecar of the Redis or Sentinel pods.
type Exporter struct {
	Enabled                  bool                         `json:"enabled,omitempty"`
	Image                    string                       `json:"image,omitempty"`
	ImagePullPolicy          corev1.PullPolicy            `json:"imagePullPolicy,omitempty"`
	ContainerSecurityContext *corev1.SecurityContext      `json:"containerSecurityContext,omitempty"`
	Args                     []string                     `json:"args,omitempty"`
	Env                      []corev1.EnvVar              `json:"env,omitempty"`
	Resources                *corev1.ResourceRequirements `json:"resources,omitempty"`
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
	ContainerSecurityContext *corev1.SecurityContext `json:"containerSecurityContext,omitempty"`
}

// RedisStorage configures the data volume of the Redis pods.
type RedisStorage struct {
	KeepAfterDeletion     bool                           `json:"keepAfterDeletion,omitempty"`
	EmptyDir              *corev1.EmptyDirVolumeSource   `json:"emptyDir,omitempty"`
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

	Items []RedisFailover `json:"items"`
}

// RedisFailoverStatus is the result of the last reconcile. State is Healthy or
// NotHealthy, and Message gives the reason.
type RedisFailoverStatus struct {
	State       string     `json:"state,omitempty"`
	LastChanged string     `json:"lastChanged,omitempty"`
	Message     string     `json:"message,omitempty"`
	TLS         *TLSStatus `json:"tls,omitempty"`
}

// TLSStatus is the observed TLS state of the Redis pods. The operator writes
// the Redis config from it, so that a restarted pod starts in this state.
type TLSStatus struct {
	Phase string `json:"phase,omitempty"`
	// Target is the state that the spec asks for: Dual, TLSOnly or None.
	Target string `json:"target,omitempty"`
	// InternalLinks tells how the replicas connect to the master:
	// Plaintext, Mixed or TLS.
	InternalLinks string `json:"internalLinks,omitempty"`
	// PlaintextPort is Open, Mixed or Closed.
	PlaintextPort       string       `json:"plaintextPort,omitempty"`
	Port                int32        `json:"port,omitempty"`
	CertificateNotAfter *metav1.Time `json:"certificateNotAfter,omitempty"`
	Message             string       `json:"message,omitempty"`
	LastTransition      string       `json:"lastTransition,omitempty"`
}
