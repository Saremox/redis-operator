package metrics

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/saremox/redis-operator/log"
)

const (
	promControllerSubsystem  = "controller"
	metricsGCIntervalMinutes = 5
)

func init() {
	go removeStaleMetrics()
}

// The label values of the metrics: the status, the check indicator, the
// error and the operation.
const (
	// SUCCESS is the status of an operation without an error.
	SUCCESS = "SUCCESS"
	// FAIL is the status of an operation with an error.
	FAIL = "FAIL"
	// STATUS_HEALTHY is the status of a check that passed.
	STATUS_HEALTHY = "HEALTHY"
	// STATUS_UNHEALTHY is the status of a check that failed.
	STATUS_UNHEALTHY = "UNHEALTHY"
	// NOT_APPLICABLE is the value of a label that does not apply.
	NOT_APPLICABLE = "NA"
	// UNHEALTHY is the gauge value of an unhealthy state.
	UNHEALTHY = 1.0
	// HEALTHY is the gauge value of a healthy state.
	HEALTHY = 0.0
	// REDIS_REPLICA_MISMATCH checks the replicas of the Redis StatefulSet.
	REDIS_REPLICA_MISMATCH = "REDIS_STATEFULSET_REPLICAS_MISMATCH"
	// SENTINEL_REPLICA_MISMATCH checks the replicas of the Sentinel
	// Deployment.
	SENTINEL_REPLICA_MISMATCH = "SENTINEL_DEPLOYMENT_REPLICAS_MISMATCH"
	// NO_MASTER checks that a master is available.
	NO_MASTER = "NO_MASTER_AVAILABLE"
	// NUMBER_OF_MASTERS checks that there is exactly one master.
	NUMBER_OF_MASTERS = "MASTER_COUNT_IS_NOT_ONE"
	// SENTINEL_WRONG_MASTER checks the master IP of a Sentinel.
	SENTINEL_WRONG_MASTER = "SENTINEL_IS_CONFIGURED_WITH_WRONG_MASTER_IP"
	// SLAVE_WRONG_MASTER checks the master IP of a replica.
	SLAVE_WRONG_MASTER = "SLAVE_IS_CONFIGURED_WITH_WRONG_MASTER_IP"
	// SENTINEL_NOT_READY is the error of a Sentinel that is not ready.
	SENTINEL_NOT_READY = "SENTINEL_NOT_READY"
	// REGEX_NOT_FOUND is the error of a Sentinel reply without the expected
	// field.
	REGEX_NOT_FOUND = "SENTINEL_REGEX_NOT_FOUND"
	// SENTINEL_TOO_MANY is the error of a Sentinel reply with too many
	// matches.
	SENTINEL_TOO_MANY = "SENTINEL_TOO_MANY"
	// MISC is the error of a Sentinel reply that the operator cannot parse.
	MISC = "MISC_ERROR"
	// SENTINEL_NUMBER_IN_MEMORY_MISMATCH checks the number of Sentinels
	// that a Sentinel knows.
	SENTINEL_NUMBER_IN_MEMORY_MISMATCH = "SENTINEL_NUMBER_IN_MEMORY_MISMATCH"
	// REDIS_SLAVES_NUMBER_IN_MEMORY_MISMATCH checks the number of replicas
	// that a Sentinel knows.
	REDIS_SLAVES_NUMBER_IN_MEMORY_MISMATCH = "REDIS_SLAVES_NUMBER_IN_MEMORY_MISMATCH"
	// MASTER_HANDOVER_ABORTED counts a FAILOVER of the rollout that Redis or
	// the operator aborted.
	MASTER_HANDOVER_ABORTED = "MASTER_HANDOVER_ABORTED"
	// WRONG_PASSWORD_USED is the error of a refused password (WRONGPASS).
	WRONG_PASSWORD_USED = "WRONG_PASSWORD_USED"
	// NOAUTH is the error of a command without a password (NOAUTH).
	NOAUTH = "AUTH_CREDENTIALS_NOT_PROVIDED"
	// NOPERM is the error of a command without a permission (NOPERM).
	NOPERM = "REDIS_USER_DOES_NOT_HAVE_PERMISSIONS"
	// IO_TIMEOUT is the error of a connection that timed out.
	IO_TIMEOUT = "CONNECTION_TIMEDOUT"
	// CONNECTION_REFUSED is the error of a refused connection.
	CONNECTION_REFUSED = "CONNECTION_REFUSED"

	// AutoscalerStepDone is the result of CountAutoscalerStep for a step
	// that ended.
	AutoscalerStepDone = "done"
	// AutoscalerStepReverted is the result of CountAutoscalerStep for a
	// reverted step.
	AutoscalerStepReverted = "reverted"
	// AutoscalerStepHeld is the result of CountAutoscalerStep for a held
	// step.
	AutoscalerStepHeld = "held"

	// K8S_FORBIDDEN_ERR is the error of a forbidden Kubernetes request.
	K8S_FORBIDDEN_ERR = "USER_FORBIDDEN_TO_PERFORM_ACTION"
	// K8S_UNAUTH is the error of an unauthorized Kubernetes request.
	K8S_UNAUTH = "CLIENT_NOT_AUTHORISED"
	// K8S_MISC is the error of a Kubernetes request with another error.
	K8S_MISC = "MISC_ERROR_CHECK_LOGS"
	// K8S_NOT_FOUND is the error of a Kubernetes object that is not found.
	K8S_NOT_FOUND = "RESOURCE_NOT_FOUND"

	// KIND_REDIS is the kind of a Redis pod.
	KIND_REDIS = "REDIS"
	// KIND_SENTINEL is the kind of a Sentinel pod.
	KIND_SENTINEL = "SENTINEL"
	// APPLY_REDIS_CONFIG applies the Redis configuration.
	APPLY_REDIS_CONFIG = "APPLY_REDIS_CONFIG"
	// APPLY_EXTERNAL_MASTER makes all the Redis pods replicas of the
	// bootstrap master.
	APPLY_EXTERNAL_MASTER = "APPLY_EXT_MASTER_ALL"
	// APPLY_SENTINEL_CONFIG applies the Sentinel configuration.
	APPLY_SENTINEL_CONFIG = "APPLY_SENTINEL_CONFIG"
	// MONITOR_REDIS_WITH_PORT sets the master that a Sentinel monitors.
	MONITOR_REDIS_WITH_PORT = "SET_SENTINEL_TO_MONITOR_REDIS_WITH_GIVEN_PORT"
	// RESET_SENTINEL resets the state of a Sentinel.
	RESET_SENTINEL = "RESET_ALL_SENTINEL_CONFIG"
	// GET_NUM_SENTINELS_IN_MEM reads the number of Sentinels from INFO
	// sentinel of a Sentinel.
	GET_NUM_SENTINELS_IN_MEM = "GET_NUMBER_OF_SENTINELS_IN_MEMORY"
	// GET_NUM_REDIS_SLAVES_IN_MEM reads the number of replicas from INFO
	// sentinel of a Sentinel.
	GET_NUM_REDIS_SLAVES_IN_MEM = "GET_NUMBER_OF_REDIS_SLAVES_IN_MEMORY"
	// GET_SLAVE_OF reads the master of a replica.
	GET_SLAVE_OF = "GET_MASTER_OF_GIVEN_SLAVE_INSTANCE"
	// IS_MASTER checks if a Redis pod is the master.
	IS_MASTER = "CHECK_IF_INSTANCE_IS_MASTER"
	// MAKE_MASTER makes a Redis pod the master.
	MAKE_MASTER = "MAKE_INSTANCE_AS_MASTER"
	// MAKE_SLAVE_OF makes a Redis pod a replica of a master.
	MAKE_SLAVE_OF = "MAKE_SLAVE_OF_GIVEN_MASTER_INSTANCE"
	// GET_SENTINEL_MONITOR reads the master that a Sentinel monitors.
	GET_SENTINEL_MONITOR = "SENTINEL_GET_MASTER_INSTANCE"
	// GET_SENTINEL_REPLICAS reads the replicas that a Sentinel knows.
	GET_SENTINEL_REPLICAS = "SENTINEL_GET_REPLICAS"
	// GET_SENTINEL_MASTER_DOWN reads if a Sentinel sees the master as down.
	GET_SENTINEL_MASTER_DOWN = "SENTINEL_GET_MASTER_DOWN"
	// GET_SENTINEL_FAILOVER reads if a Sentinel runs a failover.
	GET_SENTINEL_FAILOVER = "SENTINEL_GET_FAILOVER_IN_PROGRESS"
	// GET_SENTINEL_MASTER_ADDR reads the master that a Sentinel gives to the
	// clients.
	GET_SENTINEL_MASTER_ADDR = "SENTINEL_GET_MASTER_ADDR_BY_NAME"
	// GET_SENTINEL_PEERS reads the master and the other Sentinels that a
	// Sentinel knows.
	GET_SENTINEL_PEERS = "SENTINEL_GET_PEERS"
	// CHECK_SENTINEL_QUORUM runs SENTINEL CKQUORUM.
	CHECK_SENTINEL_QUORUM = "SENTINEL_CKQUORUM"
	// SLAVE_IS_READY checks if a replica is in sync with its master.
	SLAVE_IS_READY = "CHECK_IF_SLAVE_IS_READY"
	// GET_REPLICATION_INFO reads INFO replication.
	GET_REPLICATION_INFO = "GET_REPLICATION_INFO"
	// GET_MEMORY_INFO reads INFO memory.
	GET_MEMORY_INFO = "GET_MEMORY_INFO"
	// DISCONNECT_CLIENTS disconnects the clients of a demoted master.
	DISCONNECT_CLIENTS = "DISCONNECT_CLIENTS_ON_DEMOTED_INSTANCE"
	// SET_PASSWORD sets the password of a Redis or Sentinel pod.
	SET_PASSWORD = "SET_PASSWORD"
	// FAILOVER_TO runs FAILOVER TO a replica.
	FAILOVER_TO = "FAILOVER_TO_REPLICA"
	// FAILOVER_ABORT runs FAILOVER ABORT.
	FAILOVER_ABORT = "FAILOVER_ABORT"
	// GET_RUN_ID reads the run ID of a Redis pod.
	GET_RUN_ID = "GET_RUN_ID"
	// GET_FAILOVER_STATE reads the failover state of a Redis pod.
	GET_FAILOVER_STATE = "GET_FAILOVER_STATE"
	// GET_AUTOSCALER_INFO reads the INFO fields of the autoscaler.
	GET_AUTOSCALER_INFO = "GET_AUTOSCALER_INFO"
)

var ( // used for garbage collection of metrics
	mutex                     sync.Mutex
	recorders                 = []recorder{}
	instanceMetricLastUpdated = map[podKey]time.Time{}
	podsByIP                  = map[podIPKey]map[podKey]time.Time{}
	resourceMetricLastUpdated = map[string]time.Time{}
	checkMetricLastUpdated    = map[string]checkMetricInfo{}
)

// checkMetricInfo identifies one instance series of redisCheck or
// sentinelCheck. The garbage collection deletes the series of a removed pod
// also while the operator reconciles its RedisFailover.
type checkMetricInfo struct {
	kind      string // "redis" or "sentinel"
	namespace string
	resource  string
	indicator string
	instance  string
	lastSeen  time.Time
}

// podKey identifies a pod in the series of redisOperations.
type podKey struct {
	namespace string
	name      string
}

// podIPKey is a pod IP of one kind, KIND_REDIS or KIND_SENTINEL. With the
// host network, a pod has the IP of its node, so a redis pod and a sentinel
// pod can have the same IP.
type podIPKey struct {
	kind string
	ip   string
}

// Recorder collects the operator metrics. NewRecorder exposes them to
// Prometheus, and Dummy discards them for tests.
type Recorder interface {
	ControllerRecorder

	// ClusterOK metrics
	SetClusterOK(namespace string, name string)
	SetClusterError(namespace string, name string)
	DeleteCluster(namespace string, name string)

	// SetAutoscaler sets the gauges of the memory autoscaler.
	SetAutoscaler(namespace string, name string, recommendedBytes int64, usedRatio float64, highWindows int32, effectiveBytes int64)
	// SetAutoscalerPhase sets the phase of the last autoscaler step: 0 Idle,
	// 1 Applying, 2 Done, 3 Reverted, 4 Held.
	SetAutoscalerPhase(namespace string, name string, phase int)
	// CountAutoscalerStep counts the end of an autoscaler step with the result
	// AutoscalerStepDone, AutoscalerStepReverted or AutoscalerStepHeld.
	CountAutoscalerStep(namespace string, name string, result string)
	// DeleteAutoscaler deletes the series of SetAutoscaler, SetAutoscalerPhase
	// and CountAutoscalerStep, for example when the autoscaler mode changes to
	// Off.
	DeleteAutoscaler(namespace string, name string)

	// SetSentinelUnknownAddrs sets the count of the unknown peer IPs and the
	// unknown master flag of the Sentinels of a RedisFailover.
	SetSentinelUnknownAddrs(namespace string, name string, peers int, master bool)

	RecordEnsureOperation(objectNamespace string, objectName string, objectKind string, resourceName string, status string)

	// SetPodIP tells which pod of the kind KIND_REDIS or KIND_SENTINEL has
	// the IP. The series of RecordRedisCheck, RecordSentinelCheck and
	// RecordRedisOperation then carry the pod name, because a pod IP changes
	// each time Kubernetes creates the pod again.
	SetPodIP(kind string, namespace string, name string, ip string)

	// RecordRedisCheck and RecordSentinelCheck take a pod IP or NOT_APPLICABLE
	// as instance.
	RecordRedisCheck(namespace string, resource string, indicator /* aspect of redis that is unhealthy */ string, instance string, status string)
	RecordSentinelCheck(namespace string, resource string, indicator /* aspect of sentinel that is unhealthy */ string, instance string, status string)

	RecordK8sOperation(namespace string, kind string, name string, operation string, status string, err string)
	RecordRedisOperation(kind string, IP string, operation string, status string, err string)
}

// recorder implements Recorder so the metrics can be managed by Prometheus.
type recorder struct {
	// Metrics fields.
	clusterOK            *prometheus.GaugeVec
	recommendedMemory    *prometheus.GaugeVec
	memoryUsedRatio      *prometheus.GaugeVec
	highWindows          *prometheus.GaugeVec
	effectiveMemory      *prometheus.GaugeVec
	stepPhase            *prometheus.GaugeVec
	steps                *prometheus.CounterVec
	ensureResource       *prometheus.CounterVec
	redisCheck           *prometheus.CounterVec
	sentinelCheck        *prometheus.CounterVec
	k8sServiceOperations *prometheus.CounterVec
	redisOperations      *prometheus.CounterVec
	unknownPeers         *prometheus.GaugeVec
	unknownMaster        *prometheus.GaugeVec
	ControllerRecorder
}

// NewRecorder returns a new Recorder that registers its metrics on reg.
func NewRecorder(namespace string, reg prometheus.Registerer) Recorder {
	r := recorder{
		clusterOK: newGaugeVec(namespace, "cluster_ok",
			"1 when the last reconcile of the RedisFailover had no error, 0 when it failed.",
			"namespace", "name"),
		recommendedMemory: newGaugeVec(namespace, "autoscaler_recommended_memory_bytes",
			"Memory limit that the autoscaler recommends for the redis container, or 0 for no change.",
			"namespace", "name"),
		memoryUsedRatio: newGaugeVec(namespace, "autoscaler_memory_used_ratio",
			"used_memory of the master divided by maxmemory, or by the memory limit without maxmemory, at the last sample.",
			"namespace", "name"),
		highWindows: newGaugeVec(namespace, "autoscaler_high_windows",
			"Number of consecutive 5-minute windows with high memory use.",
			"namespace", "name"),
		effectiveMemory: newGaugeVec(namespace, "autoscaler_effective_memory_bytes",
			"Memory limit of the redis container that the operator applies: the limit of redis.resources, or a larger limit of the autoscaler.",
			"namespace", "name"),
		stepPhase: newGaugeVec(namespace, "autoscaler_step_phase",
			"Phase of the last autoscaler step: 0 Idle, 1 Applying, 2 Done, 3 Reverted, 4 Held.",
			"namespace", "name"),
		steps: newCounterVec(namespace, "autoscaler_steps_total",
			"Number of autoscaler steps that ended, by result: done, reverted or held.",
			"namespace", "name", "result"),
		ensureResource: newCounterVec(namespace, "ensure_resource_total",
			"Number of ensure attempts for a resource of a RedisFailover, by status. An attempt also counts when the resource needs no change.",
			"namespace", "name", "kind", "resource_name", "status"),
		redisCheck: newCounterVec(namespace, "redis_checks_total",
			"Number of checks of the Redis pods, by indicator and status (HEALTHY or UNHEALTHY). The pod_name is the pod name, or NA for a check of the RedisFailover.",
			"namespace", "resource", "indicator", "pod_name", "status"),
		sentinelCheck: newCounterVec(namespace, "sentinel_checks_total",
			"Number of checks of the Sentinel pods, by indicator and status (HEALTHY or UNHEALTHY). The pod_name is the pod name, or NA for a check of the RedisFailover.",
			"namespace", "resource", "indicator", "pod_name", "status"),
		k8sServiceOperations: newCounterVec(namespace, "k8s_operations_total",
			"Number of Kubernetes API calls of the operator, by status.",
			"namespace", "kind", "name", "operation", "status", "err"),
		redisOperations: newCounterVec(namespace, "redis_operations_total",
			"Number of commands that the operator sent to Redis and Sentinel, by pod and status.",
			"kind" /* redis or sentinel */, "namespace", "pod_name", "operation", "status", "err"),
		unknownPeers: newGaugeVec(namespace, "unknown_sentinel_peers",
			"Number of peer IPs that the Sentinels of the RedisFailover know, that no Sentinel pod of the RedisFailover has, and that no pod of it had in the last 5 minutes, in two checks at least 90s apart.",
			"namespace", "name"),
		unknownMaster: newGaugeVec(namespace, "sentinel_unknown_master",
			"1 when a Sentinel of the RedisFailover monitors a master IP that no Redis pod of the RedisFailover has and that no pod of it had in the last 5 minutes, in two checks at least 90s apart, else 0.",
			"namespace", "name"),
		ControllerRecorder: newControllerRecorder(reg),
	}

	// Register metrics.
	reg.MustRegister(
		r.clusterOK,
		r.recommendedMemory,
		r.memoryUsedRatio,
		r.highWindows,
		r.effectiveMemory,
		r.stepPhase,
		r.steps,
		r.ensureResource,
		r.redisCheck,
		r.sentinelCheck,
		r.k8sServiceOperations,
		r.redisOperations,
		r.unknownPeers,
		r.unknownMaster,
	)
	mutex.Lock()
	recorders = append(recorders, r)
	mutex.Unlock()
	return r
}

// newGaugeVec returns a gauge of the controller subsystem.
func newGaugeVec(namespace, name, help string, labels ...string) *prometheus.GaugeVec {
	return prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Subsystem: promControllerSubsystem,
		Name:      name,
		Help:      help,
	}, labels)
}

// newCounterVec returns a counter of the controller subsystem.
func newCounterVec(namespace, name, help string, labels ...string) *prometheus.CounterVec {
	return prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: promControllerSubsystem,
		Name:      name,
		Help:      help,
	}, labels)
}

// SetClusterOK sets cluster_ok to 1.
func (r recorder) SetClusterOK(namespace, name string) {
	r.clusterOK.WithLabelValues(namespace, name).Set(1)
}

// SetClusterError sets cluster_ok to 0.
func (r recorder) SetClusterError(namespace, name string) {
	r.clusterOK.WithLabelValues(namespace, name).Set(0)
}

// DeleteCluster deletes the cluster_ok series of a deleted RedisFailover, so
// the metric does not report a cluster that no longer exists.
func (r recorder) DeleteCluster(namespace, name string) {
	r.clusterOK.DeleteLabelValues(namespace, name)
	r.DeleteAutoscaler(namespace, name)
	r.unknownPeers.DeleteLabelValues(namespace, name)
	r.unknownMaster.DeleteLabelValues(namespace, name)
}

func (r recorder) SetAutoscaler(namespace, name string, recommendedBytes int64, usedRatio float64, highWindows int32, effectiveBytes int64) {
	r.recommendedMemory.WithLabelValues(namespace, name).Set(float64(recommendedBytes))
	r.memoryUsedRatio.WithLabelValues(namespace, name).Set(usedRatio)
	r.highWindows.WithLabelValues(namespace, name).Set(float64(highWindows))
	r.effectiveMemory.WithLabelValues(namespace, name).Set(float64(effectiveBytes))
	updateResourceMetricLastUpdatedTracker(namespace, "redisfailover", name)
}

func (r recorder) SetAutoscalerPhase(namespace, name string, phase int) {
	r.stepPhase.WithLabelValues(namespace, name).Set(float64(phase))
	updateResourceMetricLastUpdatedTracker(namespace, "redisfailover", name)
}

func (r recorder) CountAutoscalerStep(namespace, name, result string) {
	r.steps.WithLabelValues(namespace, name, result).Inc()
	updateResourceMetricLastUpdatedTracker(namespace, "redisfailover", name)
}

func (r recorder) DeleteAutoscaler(namespace, name string) {
	r.recommendedMemory.DeleteLabelValues(namespace, name)
	r.memoryUsedRatio.DeleteLabelValues(namespace, name)
	r.highWindows.DeleteLabelValues(namespace, name)
	r.effectiveMemory.DeleteLabelValues(namespace, name)
	r.stepPhase.DeleteLabelValues(namespace, name)
	for _, result := range []string{AutoscalerStepDone, AutoscalerStepReverted, AutoscalerStepHeld} {
		r.steps.DeleteLabelValues(namespace, name, result)
	}
}

func (r recorder) SetSentinelUnknownAddrs(namespace, name string, peers int, master bool) {
	r.unknownPeers.WithLabelValues(namespace, name).Set(float64(peers))
	unknownMaster := HEALTHY
	if master {
		unknownMaster = UNHEALTHY
	}
	r.unknownMaster.WithLabelValues(namespace, name).Set(unknownMaster)
	updateResourceMetricLastUpdatedTracker(namespace, "redisfailover", name)
}

func (r recorder) RecordEnsureOperation(objectNamespace, objectName, objectKind, resourceName, status string) {
	r.ensureResource.WithLabelValues(objectNamespace, objectName, objectKind, resourceName, status).Add(1)
	updateResourceMetricLastUpdatedTracker(objectNamespace, objectKind, objectName)
}

func (r recorder) SetPodIP(kind, namespace, name, ip string) {
	key := podIPKey{kind: kind, ip: ip}
	mutex.Lock()
	if podsByIP[key] == nil {
		podsByIP[key] = map[podKey]time.Time{}
	}
	podsByIP[key][podKey{namespace: namespace, name: name}] = time.Now()
	mutex.Unlock()
}

// podOf returns the pod of SetPodIP. For an unknown IP or NOT_APPLICABLE, the
// value is the name, so the series stays apart from the other pods. An IP of
// more than one pod gives the same result, because the operator cannot tell
// which pod got the command.
func podOf(kind, ip string) podKey {
	mutex.Lock()
	defer mutex.Unlock()
	if pods := podsByIP[podIPKey{kind: kind, ip: ip}]; len(pods) == 1 {
		for pod := range pods {
			return pod
		}
	}
	return podKey{namespace: NOT_APPLICABLE, name: ip}
}

func (r recorder) RecordRedisCheck(namespace, resource, indicator /* aspect of redis that is unhealthy */, instance, status string) {
	instance = podOf(KIND_REDIS, instance).name
	r.redisCheck.WithLabelValues(namespace, resource, indicator, instance, status).Add(1)
	updateResourceMetricLastUpdatedTracker(namespace, "redisfailover", resource)
	updateCheckMetricLastUpdatedTracker("redis", namespace, resource, indicator, instance)
}

func (r recorder) RecordSentinelCheck(namespace, resource, indicator /* aspect of sentinel that is unhealthy */, instance, status string) {
	instance = podOf(KIND_SENTINEL, instance).name
	r.sentinelCheck.WithLabelValues(namespace, resource, indicator, instance, status).Add(1)
	updateResourceMetricLastUpdatedTracker(namespace, "redisfailover", resource)
	updateCheckMetricLastUpdatedTracker("sentinel", namespace, resource, indicator, instance)
}

func (r recorder) RecordK8sOperation(namespace, kind, name, operation, status, err string) {
	r.k8sServiceOperations.WithLabelValues(namespace, kind, name, operation, status, err).Add(1)
	updateResourceMetricLastUpdatedTracker(namespace, kind, name)
}

func (r recorder) RecordRedisOperation(kind /* redis or sentinel */, IP, operation, status, err string) {
	pod := podOf(kind, IP)
	r.redisOperations.WithLabelValues(kind, pod.namespace, pod.name, operation, status, err).Add(1)
	updateInstanceMetricLastUpdatedTracker(pod)
}

func updateResourceMetricLastUpdatedTracker(namespace, kind, name string) {
	mutex.Lock()
	resourceMetricLastUpdated[fmt.Sprintf("%v/%v/%v", namespace, kind, name)] = time.Now()
	mutex.Unlock()
}

func updateInstanceMetricLastUpdatedTracker(pod podKey) {
	mutex.Lock()
	instanceMetricLastUpdated[pod] = time.Now()
	mutex.Unlock()
}

func updateCheckMetricLastUpdatedTracker(kind, namespace, resource, indicator, instance string) {
	key := strings.Join([]string{kind, namespace, resource, indicator, instance}, "/")
	mutex.Lock()
	checkMetricLastUpdated[key] = checkMetricInfo{
		kind:      kind,
		namespace: namespace,
		resource:  resource,
		indicator: indicator,
		instance:  instance,
		lastSeen:  time.Now(),
	}
	mutex.Unlock()
}

// removeStaleMetrics runs every metricsGCIntervalMinutes. It deletes each
// series that got no update in that time.
func removeStaleMetrics() {
	for {
		metricsDeletedCount := deleteStaleMetrics()
		log.Debugf("delete %v stale metrics", metricsDeletedCount)
		time.Sleep(metricsGCIntervalMinutes * time.Minute)
	}
}

// deleteStaleMetrics deletes the stale series of each recorder and returns
// their count.
func deleteStaleMetrics() int {
	metricsDeletedCount := 0
	kubernetesResourceBasedLabels, customResourceBasedLabels, podBasedLabels := getLabelsOfStaleMetrics()
	staleCheckMetrics := getStaleCheckMetrics()
	mutex.Lock()
	currentRecorders := make([]recorder, len(recorders))
	copy(currentRecorders, recorders)
	mutex.Unlock()
	for _, recorder := range currentRecorders {
		metricsDeletedCount += recorder.deleteStale(kubernetesResourceBasedLabels, customResourceBasedLabels, podBasedLabels)
		metricsDeletedCount += recorder.deleteStaleChecks(staleCheckMetrics)
	}
	return metricsDeletedCount
}

// deleteStale deletes the series that match the labels of getLabelsOfStaleMetrics.
func (r recorder) deleteStale(kubernetesResourceBasedLabels, customResourceBasedLabels, podBasedLabels []prometheus.Labels) int {
	metricsDeletedCount := 0
	for _, label := range kubernetesResourceBasedLabels {
		metricsDeletedCount += r.ensureResource.DeletePartialMatch(label)
		metricsDeletedCount += r.k8sServiceOperations.DeletePartialMatch(label)
	}
	for _, label := range customResourceBasedLabels {
		metricsDeletedCount += r.redisCheck.DeletePartialMatch(label)
		metricsDeletedCount += r.sentinelCheck.DeletePartialMatch(label)
		// A new map, because label is used again for the next recorder.
		labelWithName := prometheus.Labels{"namespace": label["namespace"], "name": label["resource"]}
		metricsDeletedCount += r.clusterOK.DeletePartialMatch(labelWithName)
		metricsDeletedCount += r.recommendedMemory.DeletePartialMatch(labelWithName)
		metricsDeletedCount += r.memoryUsedRatio.DeletePartialMatch(labelWithName)
		metricsDeletedCount += r.highWindows.DeletePartialMatch(labelWithName)
		metricsDeletedCount += r.effectiveMemory.DeletePartialMatch(labelWithName)
		metricsDeletedCount += r.stepPhase.DeletePartialMatch(labelWithName)
		metricsDeletedCount += r.steps.DeletePartialMatch(labelWithName)
		metricsDeletedCount += r.unknownPeers.DeletePartialMatch(labelWithName)
		metricsDeletedCount += r.unknownMaster.DeletePartialMatch(labelWithName)
	}
	for _, label := range podBasedLabels {
		metricsDeletedCount += r.redisOperations.DeletePartialMatch(label)
	}
	return metricsDeletedCount
}

// deleteStaleChecks deletes the check series of the entries of
// getStaleCheckMetrics.
func (r recorder) deleteStaleChecks(staleCheckMetrics []checkMetricInfo) int {
	metricsDeletedCount := 0
	// customResourceBasedLabels matches only a RedisFailover without
	// reconciles. Without this loop, the series of each removed pod
	// stay, and the memory use grows.
	for _, entry := range staleCheckMetrics {
		check := r.redisCheck
		if entry.kind == "sentinel" {
			check = r.sentinelCheck
		}
		// The tracker forgets the entry, so delete both status series now.
		for _, status := range []string{STATUS_HEALTHY, STATUS_UNHEALTHY} {
			if check.DeleteLabelValues(entry.namespace, entry.resource, entry.indicator, entry.instance, status) {
				metricsDeletedCount++
			}
		}
	}
	return metricsDeletedCount
}

func getLabelsOfStaleMetrics() (kubernetesResourceBasedLabels, customResourceBasedLabels, podBasedLabels []prometheus.Labels) {

	kubernetesResourceBasedLabels = []prometheus.Labels{}
	customResourceBasedLabels = []prometheus.Labels{}
	podBasedLabels = []prometheus.Labels{}

	mutex.Lock()
	for key, value := range resourceMetricLastUpdated {
		// if the key is stale
		if value.Before(time.Now().Add(-metricsGCIntervalMinutes * time.Minute)) {
			// extract keys and create labels
			ids := strings.Split(key, "/")
			namespace := ids[0]
			kind := ids[1]
			resource := ids[2]
			kubernetesResourceBasedLabels = append(kubernetesResourceBasedLabels,
				prometheus.Labels{
					"namespace": namespace,
					"name":      resource,
					"kind":      kind,
				},
			)
			customResourceBasedLabels = append(customResourceBasedLabels,
				prometheus.Labels{
					"namespace": namespace,
					"resource":  resource,
				},
			)
			// The labels hold the key, so the tracker can forget it.
			delete(resourceMetricLastUpdated, key)
		}
	}
	mutex.Unlock()

	mutex.Lock()
	for pod, value := range instanceMetricLastUpdated {
		if value.Before(time.Now().Add(-metricsGCIntervalMinutes * time.Minute)) {
			podBasedLabels = append(podBasedLabels,
				prometheus.Labels{
					"namespace": pod.namespace,
					"pod_name":  pod.name,
				},
			)
			// The labels hold the key, so the tracker can forget it.
			delete(instanceMetricLastUpdated, pod)
		}

	}
	// An IP of two pods gets a pod name again when only one pod stays listed.
	for key, pods := range podsByIP {
		for pod, lastSeen := range pods {
			if lastSeen.Before(time.Now().Add(-metricsGCIntervalMinutes * time.Minute)) {
				delete(pods, pod)
			}
		}
		if len(pods) == 0 {
			delete(podsByIP, key)
		}
	}
	mutex.Unlock()
	return kubernetesResourceBasedLabels, customResourceBasedLabels, podBasedLabels
}

// getStaleCheckMetrics returns the instance series of redisCheck and
// sentinelCheck without an update for metricsGCIntervalMinutes, and removes
// them from the tracker.
func getStaleCheckMetrics() []checkMetricInfo {
	stale := []checkMetricInfo{}
	cutoff := time.Now().Add(-metricsGCIntervalMinutes * time.Minute)

	mutex.Lock()
	defer mutex.Unlock()
	for key, entry := range checkMetricLastUpdated {
		if entry.lastSeen.Before(cutoff) {
			stale = append(stale, entry)
			delete(checkMetricLastUpdated, key)
		}
	}
	return stale
}
