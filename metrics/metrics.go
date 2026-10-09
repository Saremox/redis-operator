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

// variables for setting various indicator labels
const (
	SUCCESS                                = "SUCCESS"
	FAIL                                   = "FAIL"
	STATUS_HEALTHY                         = "HEALTHY"
	STATUS_UNHEALTHY                       = "UNHEALTHY"
	NOT_APPLICABLE                         = "NA"
	UNHEALTHY                              = 1.0
	HEALTHY                                = 0.0
	REDIS_REPLICA_MISMATCH                 = "REDIS_STATEFULSET_REPLICAS_MISMATCH"
	SENTINEL_REPLICA_MISMATCH              = "SENTINEL_DEPLOYMENT_REPLICAS_MISMATCH"
	NO_MASTER                              = "NO_MASTER_AVAILABLE"
	NUMBER_OF_MASTERS                      = "MASTER_COUNT_IS_NOT_ONE"
	SENTINEL_WRONG_MASTER                  = "SENTINEL_IS_CONFIGURED_WITH_WRONG_MASTER_IP"
	SLAVE_WRONG_MASTER                     = "SLAVE_IS_CONFIGURED_WITH_WRONG_MASTER_IP"
	SENTINEL_NOT_READY                     = "SENTINEL_NOT_READY"
	REGEX_NOT_FOUND                        = "SENTINEL_REGEX_NOT_FOUND"
	SENTINEL_TOO_MANY                      = "SENTINEL_TOO_MANY"
	MISC                                   = "MISC_ERROR"
	SENTINEL_NUMBER_IN_MEMORY_MISMATCH     = "SENTINEL_NUMBER_IN_MEMORY_MISMATCH"
	REDIS_SLAVES_NUMBER_IN_MEMORY_MISMATCH = "REDIS_SLAVES_NUMBER_IN_MEMORY_MISMATCH"
	// MASTER_HANDOVER_ABORTED counts a FAILOVER of the rollout that Redis
	// aborted, because the replica did not catch up in time.
	MASTER_HANDOVER_ABORTED = "MASTER_HANDOVER_ABORTED"
	// redis connection related errors
	WRONG_PASSWORD_USED = "WRONG_PASSWORD_USED"
	NOAUTH              = "AUTH_CREDENTIALS_NOT_PROVIDED"
	NOPERM              = "REDIS_USER_DOES_NOT_HAVE_PERMISSIONS"
	IO_TIMEOUT          = "CONNECTION_TIMEDOUT"
	CONNECTION_REFUSED  = "CONNECTION_REFUSED"

	K8S_FORBIDDEN_ERR = "USER_FORBIDDEN_TO_PERFORM_ACTION"
	K8S_UNAUTH        = "CLIENT_NOT_AUTHORISED"
	K8S_MISC          = "MISC_ERROR_CHECK_LOGS"
	K8S_NOT_FOUND     = "RESOURCE_NOT_FOUND"

	KIND_REDIS                  = "REDIS"
	KIND_SENTINEL               = "SENTINEL"
	APPLY_REDIS_CONFIG          = "APPLY_REDIS_CONFIG"
	APPLY_EXTERNAL_MASTER       = "APPLY_EXT_MASTER_ALL"
	APPLY_SENTINEL_CONFIG       = "APPLY_SENTINEL_CONFIG"
	MONITOR_REDIS_WITH_PORT     = "SET_SENTINEL_TO_MONITOR_REDIS_WITH_GIVEN_PORT"
	RESET_SENTINEL              = "RESET_ALL_SENTINEL_CONFIG"
	GET_NUM_SENTINELS_IN_MEM    = "GET_NUMBER_OF_SENTINELS_IN_MEMORY"    // `info sentinel` command on a sentinel machine > grep sentinel
	GET_NUM_REDIS_SLAVES_IN_MEM = "GET_NUMBER_OF_REDIS_SLAVES_IN_MEMORY" // `info sentinel` command on a sentinel machine > grep slaves
	GET_SLAVE_OF                = "GET_MASTER_OF_GIVEN_SLAVE_INSTANCE"
	IS_MASTER                   = "CHECK_IF_INSTANCE_IS_MASTER"
	MAKE_MASTER                 = "MAKE_INSTANCE_AS_MASTER"
	MAKE_SLAVE_OF               = "MAKE_SLAVE_OF_GIVEN_MASTER_INSTANCE"
	GET_SENTINEL_MONITOR        = "SENTINEL_GET_MASTER_INSTANCE"
	GET_SENTINEL_REPLICAS       = "SENTINEL_GET_REPLICAS"
	GET_SENTINEL_MASTER_DOWN    = "SENTINEL_GET_MASTER_DOWN"
	CHECK_SENTINEL_QUORUM       = "SENTINEL_CKQUORUM"
	SLAVE_IS_READY              = "CHECK_IF_SLAVE_IS_READY"
	GET_REPLICATION_INFO        = "GET_REPLICATION_INFO"
	GET_MEMORY_INFO             = "GET_MEMORY_INFO"
	DISCONNECT_CLIENTS          = "DISCONNECT_CLIENTS_ON_DEMOTED_INSTANCE"
	SET_PASSWORD                = "SET_PASSWORD"
	FAILOVER_TO                 = "FAILOVER_TO_REPLICA"
	FAILOVER_ABORT              = "FAILOVER_ABORT"
)

var ( // used for garbage collection of metrics
	mutex                     sync.Mutex
	recorders                 = []recorder{}
	instanceMetricLastUpdated = map[string]time.Time{}
	resourceMetricLastUpdated = map[string]time.Time{}
	checkMetricLastUpdated    = map[string]checkMetricInfo{}
)

// checkMetricInfo identifies one instance series of redisCheck or
// sentinelCheck. The garbage collection deletes the series of an old pod IP
// also while the operator reconciles its RedisFailover.
type checkMetricInfo struct {
	kind      string // "redis" or "sentinel"
	namespace string
	resource  string
	indicator string
	instance  string
	lastSeen  time.Time
}

// Recorder collects the operator metrics. NewRecorder exposes them to
// Prometheus, and Dummy discards them for tests.
type Recorder interface {
	ControllerRecorder

	// ClusterOK metrics
	SetClusterOK(namespace string, name string)
	SetClusterError(namespace string, name string)
	DeleteCluster(namespace string, name string)

	RecordEnsureOperation(objectNamespace string, objectName string, objectKind string, resourceName string, status string)

	RecordRedisCheck(namespace string, resource string, indicator /* aspect of redis that is unhealthy */ string, instance string, status string)
	RecordSentinelCheck(namespace string, resource string, indicator /* aspect of sentinel that is unhealthy */ string, instance string, status string)

	RecordK8sOperation(namespace string, kind string, name string, operation string, status string, err string)
	RecordRedisOperation(kind string, IP string, operation string, status string, err string)
}

// recorder implements Recorder so the metrics can be managed by Prometheus.
type recorder struct {
	// Metrics fields.
	clusterOK            *prometheus.GaugeVec
	ensureResource       *prometheus.CounterVec
	redisCheck           *prometheus.CounterVec
	sentinelCheck        *prometheus.CounterVec
	k8sServiceOperations *prometheus.CounterVec
	redisOperations      *prometheus.CounterVec
	ControllerRecorder
}

// NewRecorder returns a new Recorder that registers its metrics on reg.
func NewRecorder(namespace string, reg prometheus.Registerer) Recorder {
	// Create metrics.
	clusterOK := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Subsystem: promControllerSubsystem,
		Name:      "cluster_ok",
		Help:      "1 when the last reconcile of the RedisFailover had no error, 0 when it failed.",
	}, []string{"namespace", "name"})

	ensureResource := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: promControllerSubsystem,
		Name:      "ensure_resource_total",
		Help:      "Number of ensure attempts for a resource of a RedisFailover, by status. An attempt also counts when the resource needs no change.",
	}, []string{"namespace", "name", "kind", "resource_name", "status"})

	redisCheck := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: promControllerSubsystem,
		Name:      "redis_checks_total",
		Help:      "Number of checks of the Redis pods, by indicator and status (HEALTHY or UNHEALTHY).",
	}, []string{"namespace", "resource", "indicator", "instance", "status"})

	sentinelCheck := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: promControllerSubsystem,
		Name:      "sentinel_checks_total",
		Help:      "Number of checks of the Sentinel pods, by indicator and status (HEALTHY or UNHEALTHY).",
	}, []string{"namespace", "resource", "indicator", "instance", "status"})

	redisOperations := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: promControllerSubsystem,
			Name:      "redis_operations_total",
			Help:      "Number of commands that the operator sent to Redis and Sentinel, by status.",
		}, []string{"kind" /* redis or sentinel */, "IP", "operation", "status", "err"})

	k8sServiceOperations := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: promControllerSubsystem,
			Name:      "k8s_operations_total",
			Help:      "Number of Kubernetes API calls of the operator, by status.",
		}, []string{"namespace", "kind", "name", "operation", "status", "err"})

	// Create the instance.
	r := recorder{
		clusterOK:            clusterOK,
		ensureResource:       ensureResource,
		redisCheck:           redisCheck,
		sentinelCheck:        sentinelCheck,
		k8sServiceOperations: k8sServiceOperations,
		redisOperations:      redisOperations,
		ControllerRecorder:   newControllerRecorder(reg),
	}

	// Register metrics.
	reg.MustRegister(
		r.clusterOK,
		r.ensureResource,
		r.redisCheck,
		r.sentinelCheck,
		r.k8sServiceOperations,
		r.redisOperations,
	)
	mutex.Lock()
	recorders = append(recorders, r)
	mutex.Unlock()
	return r
}

// SetClusterOK sets cluster_ok to 1.
func (r recorder) SetClusterOK(namespace string, name string) {
	r.clusterOK.WithLabelValues(namespace, name).Set(1)
}

// SetClusterError sets cluster_ok to 0.
func (r recorder) SetClusterError(namespace string, name string) {
	r.clusterOK.WithLabelValues(namespace, name).Set(0)
}

// DeleteCluster deletes the cluster_ok series of a deleted RedisFailover, so
// the metric does not report a cluster that no longer exists.
func (r recorder) DeleteCluster(namespace string, name string) {
	r.clusterOK.DeleteLabelValues(namespace, name)
}

func (r recorder) RecordEnsureOperation(objectNamespace string, objectName string, objectKind string, resourceName string, status string) {
	r.ensureResource.WithLabelValues(objectNamespace, objectName, objectKind, resourceName, status).Add(1)
	updateResourceMetricLastUpdatedTracker(objectNamespace, objectKind, objectName)
}

func (r recorder) RecordRedisCheck(namespace string, resource string, indicator /* aspect of redis that is unhealthy */ string, instance string, status string) {
	r.redisCheck.WithLabelValues(namespace, resource, indicator, instance, status).Add(1)
	updateResourceMetricLastUpdatedTracker(namespace, "redisfailover", resource)
	updateCheckMetricLastUpdatedTracker("redis", namespace, resource, indicator, instance)
}

func (r recorder) RecordSentinelCheck(namespace string, resource string, indicator /* aspect of sentinel that is unhealthy */ string, instance string, status string) {
	r.sentinelCheck.WithLabelValues(namespace, resource, indicator, instance, status).Add(1)
	updateResourceMetricLastUpdatedTracker(namespace, "redisfailover", resource)
	updateCheckMetricLastUpdatedTracker("sentinel", namespace, resource, indicator, instance)
}

func (r recorder) RecordK8sOperation(namespace string, kind string, name string, operation string, status string, err string) {
	r.k8sServiceOperations.WithLabelValues(namespace, kind, name, operation, status, err).Add(1)
	updateResourceMetricLastUpdatedTracker(namespace, kind, name)
}

func (r recorder) RecordRedisOperation(kind /* redis or sentinel */ string, IP string, operation string, status string, err string) {
	r.redisOperations.WithLabelValues(kind, IP, operation, status, err).Add(1)
	updateInstanceMetricLastUpdatedTracker(IP)
}

func updateResourceMetricLastUpdatedTracker(namespace string, kind string, name string) {
	mutex.Lock()
	resourceMetricLastUpdated[fmt.Sprintf("%v/%v/%v", namespace, kind, name)] = time.Now()
	mutex.Unlock()
}

func updateInstanceMetricLastUpdatedTracker(IP string) {
	mutex.Lock()
	instanceMetricLastUpdated[IP] = time.Now()
	mutex.Unlock()
}

func updateCheckMetricLastUpdatedTracker(kind string, namespace string, resource string, indicator string, instance string) {
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
		metricsDeletedCount := 0
		kubernetesResourceBasedLabels, customResourceBasedLabels, ipBasedLabels := getLabelsOfStaleMetrics()
		staleCheckMetrics := getStaleCheckMetrics()
		mutex.Lock()
		currentRecorders := make([]recorder, len(recorders))
		copy(currentRecorders, recorders)
		mutex.Unlock()
		for _, recorder := range currentRecorders {
			for _, label := range kubernetesResourceBasedLabels {
				metricsDeletedCount += recorder.ensureResource.DeletePartialMatch(label)
				metricsDeletedCount += recorder.k8sServiceOperations.DeletePartialMatch(label)
			}
			for _, label := range customResourceBasedLabels {
				metricsDeletedCount += recorder.redisCheck.DeletePartialMatch(label)
				metricsDeletedCount += recorder.sentinelCheck.DeletePartialMatch(label)
				// A new map, because label is used again for the next recorder.
				labelWithName := prometheus.Labels{"namespace": label["namespace"], "name": label["resource"]}
				metricsDeletedCount += recorder.clusterOK.DeletePartialMatch(labelWithName)
			}
			for _, label := range ipBasedLabels {
				metricsDeletedCount += recorder.redisOperations.DeletePartialMatch(label)
			}
			// customResourceBasedLabels matches only a RedisFailover without
			// reconciles. Pod IPs change at each restart, so without this loop
			// the series of each old IP stay, and the memory use grows.
			for _, entry := range staleCheckMetrics {
				check := recorder.redisCheck
				if entry.kind == "sentinel" {
					check = recorder.sentinelCheck
				}
				// The tracker forgets the entry, so delete both status series now.
				for _, status := range []string{STATUS_HEALTHY, STATUS_UNHEALTHY} {
					if check.DeleteLabelValues(entry.namespace, entry.resource, entry.indicator, entry.instance, status) {
						metricsDeletedCount++
					}
				}
			}
		}
		log.Debugf("delete %v stale metrics", metricsDeletedCount)
		time.Sleep(metricsGCIntervalMinutes * time.Minute)
	}
}

func getLabelsOfStaleMetrics() (kubernetesResourceBasedLabels []prometheus.Labels, customResourceBasedLabels []prometheus.Labels, ipBasedLabels []prometheus.Labels) {

	kubernetesResourceBasedLabels = []prometheus.Labels{}
	customResourceBasedLabels = []prometheus.Labels{}
	ipBasedLabels = []prometheus.Labels{}

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
	for IP, value := range instanceMetricLastUpdated {
		if value.Before(time.Now().Add(-metricsGCIntervalMinutes * time.Minute)) {
			ipBasedLabels = append(ipBasedLabels,
				prometheus.Labels{
					"IP": IP,
				},
			)
			// The labels hold the key, so the tracker can forget it.
			delete(instanceMetricLastUpdated, IP)
		}

	}
	mutex.Unlock()
	return kubernetesResourceBasedLabels, customResourceBasedLabels, ipBasedLabels
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
