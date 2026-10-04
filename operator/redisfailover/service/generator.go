package service

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"strconv"
	"strings"
	"text/template"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/operator/redisfailover/util"
)

const (
	redisConfigurationVolumeName = "redis-config"
	// Template used to build the Redis configuration
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

// sentinelConfigTemplate monitors a placeholder master. The operator replaces
// that monitor with SENTINEL MONITOR, which uses the Sentinel built-in
// timeouts, and then applies customConfig. Thus the timeouts here apply only to
// the placeholder. They are the customConfig defaults, so that each timeout has
// one value in the code.
var sentinelConfigTemplate = fmt.Sprintf(`sentinel monitor mymaster 127.0.0.1 {{.Spec.Redis.Port}} 2
sentinel down-after-milliseconds mymaster %d
sentinel failover-timeout mymaster %d
sentinel parallel-syncs mymaster 2`, redisfailoverv1.DefaultSentinelDownAfterMilliseconds, redisfailoverv1.DefaultSentinelFailoverTimeoutMilliseconds)

func generateSentinelService(rf *redisfailoverv1.RedisFailover, labels map[string]string, ownerRefs []metav1.OwnerReference) *corev1.Service {
	name := GetSentinelName(rf)
	namespace := rf.Namespace

	sentinelTargetPort := intstr.FromInt32(26379)
	selectorLabels := generateSelectorLabels(sentinelRoleName, rf.Name)
	labels = util.MergeLabels(labels, selectorLabels)

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       namespace,
			Labels:          labels,
			OwnerReferences: ownerRefs,
			Annotations:     rf.Spec.Sentinel.ServiceAnnotations,
		},
		Spec: corev1.ServiceSpec{
			Selector: selectorLabels,
			Ports: []corev1.ServicePort{
				{
					Name:       "sentinel",
					Port:       26379,
					TargetPort: sentinelTargetPort,
					Protocol:   "TCP",
				},
			},
		},
	}

	// The sentinel exporter sidecar listens on sentinelExporterListenPort, but
	// without a matching service port there is no way to scrape it through the
	// service.
	if rf.Spec.Sentinel.Exporter.Enabled {
		port := sentinelExporterListenPort(rf)
		svc.Spec.Ports = append(svc.Spec.Ports, corev1.ServicePort{
			Name:       "metrics",
			Port:       port,
			TargetPort: intstr.FromInt(int(port)),
			Protocol:   corev1.ProtocolTCP,
		})
	}

	return svc
}

func generateRedisService(rf *redisfailoverv1.RedisFailover, labels map[string]string, ownerRefs []metav1.OwnerReference) *corev1.Service {
	name := GetRedisName(rf)
	namespace := rf.Namespace

	selectorLabels := generateSelectorLabels(redisRoleName, rf.Name)
	labels = util.MergeLabels(labels, selectorLabels)
	defaultAnnotations := map[string]string{
		"prometheus.io/scrape": "true",
		"prometheus.io/port":   "http",
		"prometheus.io/path":   "/metrics",
	}
	annotations := util.MergeLabels(defaultAnnotations, rf.Spec.Redis.ServiceAnnotations)

	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       namespace,
			Labels:          labels,
			OwnerReferences: ownerRefs,
			Annotations:     annotations,
		},
		Spec: corev1.ServiceSpec{
			Type:      corev1.ServiceTypeClusterIP,
			ClusterIP: corev1.ClusterIPNone,
			Ports: []corev1.ServicePort{
				{
					Port:     redisExporterListenPort(rf),
					Protocol: corev1.ProtocolTCP,
					Name:     exporterPortName,
				},
			},
			Selector: selectorLabels,
		},
	}
}

func generateRedisMasterService(rf *redisfailoverv1.RedisFailover, labels map[string]string, ownerRefs []metav1.OwnerReference) *corev1.Service {
	name := GetRedisMasterName(rf)
	namespace := rf.Namespace

	selectorLabels := generateSelectorLabels(redisRoleName, rf.Name)
	selectorLabels = util.MergeLabels(selectorLabels, map[string]string{
		redisRoleLabelKey: redisRoleLabelMaster,
	})
	labels = util.MergeLabels(labels, selectorLabels)

	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       namespace,
			Labels:          labels,
			OwnerReferences: ownerRefs,
			Annotations:     rf.Spec.Redis.ServiceAnnotations,
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeClusterIP,
			Ports: []corev1.ServicePort{
				{
					Name:       "redis",
					Port:       rf.Spec.Redis.Port,
					TargetPort: intstr.FromString("redis"),
					Protocol:   corev1.ProtocolTCP,
				},
			},
			Selector: selectorLabels,
		},
	}
}

func generateRedisSlaveService(rf *redisfailoverv1.RedisFailover, labels map[string]string, ownerRefs []metav1.OwnerReference) *corev1.Service {
	name := GetRedisSlaveName(rf)
	namespace := rf.Namespace

	selectorLabels := generateSelectorLabels(redisRoleName, rf.Name)
	selectorLabels = util.MergeLabels(selectorLabels, map[string]string{
		redisRoleLabelKey: redisRoleLabelSlave,
	})
	labels = util.MergeLabels(labels, selectorLabels)

	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       namespace,
			Labels:          labels,
			OwnerReferences: ownerRefs,
			Annotations:     rf.Spec.Redis.ServiceAnnotations,
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeClusterIP,
			Ports: []corev1.ServicePort{
				{
					Name:       "redis",
					Port:       rf.Spec.Redis.Port,
					TargetPort: intstr.FromString("redis"),
					Protocol:   corev1.ProtocolTCP,
				},
			},
			Selector: selectorLabels,
		},
	}
}

func generateSentinelConfigMap(rf *redisfailoverv1.RedisFailover, labels map[string]string, ownerRefs []metav1.OwnerReference) *corev1.ConfigMap {
	name := GetSentinelName(rf)
	namespace := rf.Namespace

	labels = util.MergeLabels(labels, generateSelectorLabels(sentinelRoleName, rf.Name))

	tmpl, err := template.New("sentinel").Parse(sentinelConfigTemplate)
	if err != nil {
		panic(err)
	}

	var tplOutput bytes.Buffer
	if err := tmpl.Execute(&tplOutput, rf); err != nil {
		panic(err)
	}

	sentinelConfigFileContent := tplOutput.String()

	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       namespace,
			Labels:          labels,
			OwnerReferences: ownerRefs,
		},
		Data: map[string]string{
			sentinelConfigFileName: sentinelConfigFileContent,
		},
	}
}

func generateRedisConfigMap(rf *redisfailoverv1.RedisFailover, labels map[string]string, ownerRefs []metav1.OwnerReference) *corev1.ConfigMap {
	name := GetRedisName(rf)
	labels = util.MergeLabels(labels, generateSelectorLabels(redisRoleName, rf.Name))

	tmpl, err := template.New("redis").Parse(redisConfigTemplate)
	if err != nil {
		panic(err)
	}

	var tplOutput bytes.Buffer
	if err := tmpl.Execute(&tplOutput, rf); err != nil {
		panic(err)
	}

	// The password is not in this ConfigMap. Without a custom redis.command,
	// redis-server gets requirepass and masterauth as arguments from the
	// REDIS_PASSWORD env var (see getRedisCommand).
	redisConfigFileContent := tplOutput.String()

	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       rf.Namespace,
			Labels:          labels,
			OwnerReferences: ownerRefs,
		},
		Data: map[string]string{
			redisConfigFileName: redisConfigFileContent,
		},
	}
}

func generateRedisShutdownConfigMap(rf *redisfailoverv1.RedisFailover, labels map[string]string, ownerRefs []metav1.OwnerReference) *corev1.ConfigMap {
	name := GetRedisShutdownConfigMapName(rf)
	port := rf.Spec.Redis.Port
	namespace := rf.Namespace

	labels = util.MergeLabels(labels, generateSelectorLabels(redisRoleName, rf.Name))
	// The preStop hook runs this script with /bin/sh, which is BusyBox ash on
	// the Alpine Redis images. Thus the script uses only POSIX syntax, without
	// "let" and "[[ ]]". The script retries both Sentinel calls, because one
	// failed call must not stop the failover. The Redis pods have no service
	// links, so the script finds Sentinel through the DNS name of the Sentinel
	// Service.
	//
	// The kubelet stops the hook at the end of the grace period, and then the
	// script cannot SAVE. The timing assumes the default grace period of 30s
	// (redis.terminationGracePeriod). Each call has a 2s limit, and the
	// Sentinel part ends before 19s. The last failover request or poll starts
	// before the 12s deadline and ends before 15s. REPLICAOF and CLIENT
	// UNPAUSE add a maximum of 4s.
	//
	// Until Sentinel promotes a replica, the master still accepts writes, and
	// the promotion loses those that did not reach the replica. Thus the script
	// pauses the writes, and makes the old master a replica of the new master
	// before it ends the pause: the clients then get READONLY and not a lost
	// OK. The pause stops 6s after the deadline, also if the script stops.
	//
	// Without the pause, a failover loses writes, so the script asks for none.
	// Then Redis 7 and later pause the writes on SIGTERM and wait for the
	// replicas, and Sentinel fails over after down-after-milliseconds. The
	// pause also fails after a password change in place: the env keeps the old
	// password. Sentinel has no password, so only the local calls send
	// REDIS_PASSWORD.
	shutdownContent := ""
	if rf.SentinelsAllowed() {
		shutdownContent = fmt.Sprintf(`t=; command -v timeout >/dev/null 2>&1 && t="timeout 2"
local_cli() {
	if [ -n "${REDIS_PASSWORD}" ]; then
		REDISCLI_AUTH="${REDIS_PASSWORD}" $t redis-cli -p %[2]v "$@"
	else
		$t redis-cli -p %[2]v "$@"
	fi
}
deadline=$(($(date +%%s) + 12))
self=$(hostname -i)
master=""
retries=0
while [ -z "$master" ] && [ "$retries" -lt 3 ]; do
	retries=$((retries + 1))
	master=$($t redis-cli -h %[1]v -p 26379 --csv SENTINEL get-master-addr-by-name mymaster | tr ',' ' ' | tr -d '\"' |cut -d' ' -f1)
	if [ -z "$master" ]; then
		sleep 1
	fi
done
if [ -z "$master" ]; then
	echo "shutdown.sh: could not resolve the master from sentinel after $retries attempts" >&2
fi
if [ "$master" = "$self" ]; then
  paused=$(local_cli CLIENT PAUSE $(((deadline + 6 - $(date +%%s)) * 1000)) WRITE)
  if [ "$paused" != "OK" ]; then
  	echo "shutdown.sh: could not pause the writes, so no failover is requested (stale REDIS_PASSWORD, or Redis before 6.2): $paused" >&2
  else
  	failover=""
  	retries=0
  	while [ "$failover" != "OK" ] && [ "$retries" -lt 3 ] && [ "$(date +%%s)" -lt "$deadline" ]; do
  		retries=$((retries + 1))
  		failover=$($t redis-cli -h %[1]v -p 26379 SENTINEL failover mymaster)
  		if [ "$failover" != "OK" ]; then
  			sleep 1
  		fi
  	done
  	if [ "$failover" != "OK" ]; then
  		echo "shutdown.sh: sentinel did not accept the failover after $retries attempts: $failover" >&2
  	else
  		while { [ -z "$master" ] || [ "$master" = "$self" ]; } && [ "$(date +%%s)" -lt "$deadline" ]; do
  			sleep 1
  			master=$($t redis-cli -h %[1]v -p 26379 --csv SENTINEL get-master-addr-by-name mymaster | tr ',' ' ' | tr -d '\"' |cut -d' ' -f1)
  		done
  		if [ -z "$master" ] || [ "$master" = "$self" ]; then
  			echo "shutdown.sh: sentinel did not report a new master before the deadline" >&2
  		else
  			local_cli REPLICAOF "$master" %[2]v
  		fi
  	fi
  	local_cli CLIENT UNPAUSE
  fi
fi
`, GetSentinelName(rf), port)
	}
	shutdownContent += fmt.Sprintf(`cmd="redis-cli -p %v"
if [ ! -z "${REDIS_PASSWORD}" ]; then
	export REDISCLI_AUTH=${REDIS_PASSWORD}
fi
save_command="${cmd} save"
eval $save_command`, port)

	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       namespace,
			Labels:          labels,
			OwnerReferences: ownerRefs,
		},
		Data: map[string]string{
			"shutdown.sh": shutdownContent,
		},
	}
}

// redisReadinessMaxLinkDownSeconds is how long ready.sh keeps a replica ready
// after its link to the master dropped: a base window plus the time the
// configured failover takes to start and finish.
func redisReadinessMaxLinkDownSeconds(rf *redisfailoverv1.RedisFailover) int64 {
	window := 60 * time.Second
	if rf.OperatorManagedFailover() {
		window += rf.GetFailoverTimeoutDuration()
	} else {
		window += sentinelConfigMilliseconds(rf, "down-after-milliseconds", redisfailoverv1.DefaultSentinelDownAfterMilliseconds)
		window += sentinelConfigMilliseconds(rf, "failover-timeout", redisfailoverv1.DefaultSentinelFailoverTimeoutMilliseconds)
	}
	return int64(window.Round(time.Second) / time.Second)
}

// sentinelConfigMilliseconds returns the value sentinel customConfig gives
// param, the last one as the operator applies them in order, or def.
func sentinelConfigMilliseconds(rf *redisfailoverv1.RedisFailover, param string, def int64) time.Duration {
	ms := def
	for _, c := range rf.Spec.Sentinel.CustomConfig {
		s := strings.Split(c, " ")
		if len(s) != 2 || !strings.EqualFold(s[0], param) {
			continue
		}
		if v, err := strconv.ParseInt(s[1], 10, 64); err == nil && v >= 0 {
			ms = v
		}
	}
	return time.Duration(ms) * time.Millisecond
}

func generateRedisReadinessConfigMap(rf *redisfailoverv1.RedisFailover, labels map[string]string, ownerRefs []metav1.OwnerReference) *corev1.ConfigMap {
	name := GetRedisReadinessName(rf)
	port := rf.Spec.Redis.Port
	namespace := rf.Namespace

	labels = util.MergeLabels(labels, generateSelectorLabels(redisRoleName, rf.Name))
	readinessContent := fmt.Sprintf(`ROLE="role"
ROLE_MASTER="role:master"
ROLE_SLAVE="role:slave"
IN_SYNC="master_sync_in_progress:1"
NO_MASTER="master_host:127.0.0.1"
LINK_UP="master_link_status:up"
LINK_DOWN_SINCE="master_link_down_since_seconds:"
# A replica stays ready this long after losing its master, so it outlasts a
# failover, during which every replica's link is down.
MAX_LINK_DOWN_SECONDS=%[2]v

cmd="redis-cli -p %[1]v"
# A frozen server still accepts connections, and redis-cli would wait for its
# reply forever, holding the probe open past its timeout on some runtimes.
if command -v timeout >/dev/null 2>&1; then
	cmd="timeout 2 ${cmd}"
fi
if [ ! -z "${REDIS_PASSWORD}" ]; then
	export REDISCLI_AUTH=${REDIS_PASSWORD}
fi

cmd="${cmd} info replication"

# The operator changes the password of a running Redis in place, and the pod
# restarts onto it later. Until then a refused password says nothing about
# replication.
if echo "${cmd}" | xargs -0 sh -c 2>&1 | grep -qE "NOAUTH|WRONGPASS"; then
		exit 0
fi

check_master(){
		exit 0
}

check_slave(){
		in_sync=$(echo "${cmd} | grep ${IN_SYNC} | tr -d \"\\r\" | tr -d \"\\n\"" | xargs -0 sh -c)
		no_master=$(echo "${cmd} | grep ${NO_MASTER} | tr -d \"\\r\" | tr -d \"\\n\"" |  xargs -0 sh -c)

		if [ -n "$in_sync" ] || [ -n "$no_master" ]; then
				exit 1
		fi

		link_up=$(echo "${cmd} | grep ${LINK_UP} | tr -d \"\\r\" | tr -d \"\\n\"" | xargs -0 sh -c)
		if [ -n "$link_up" ]; then
				exit 0
		fi

		# -1 means the link has not been up since redis started: this replica
		# has never loaded the master's data, e.g. it can't read its RDB format.
		down_since=$(echo "${cmd} | grep ${LINK_DOWN_SINCE} | cut -d: -f2 | tr -d \"\\r\" | tr -d \"\\n\"" | xargs -0 sh -c)
		if [ "$down_since" -ge 0 ] 2>/dev/null && [ "$down_since" -le "$MAX_LINK_DOWN_SECONDS" ]; then
				exit 0
		fi

		exit 1
}

role=$(echo "${cmd} | grep $ROLE | tr -d \"\\r\" | tr -d \"\\n\"" | xargs -0 sh -c)
case $role in
		$ROLE_MASTER)
				check_master
				;;
		$ROLE_SLAVE)
				check_slave
				;;
		*)
				echo "unexpected"
				exit 1
esac`, port, redisReadinessMaxLinkDownSeconds(rf))

	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       namespace,
			Labels:          labels,
			OwnerReferences: ownerRefs,
		},
		Data: map[string]string{
			"ready.sh": readinessContent,
		},
	}
}

func generateRedisStatefulSet(rf *redisfailoverv1.RedisFailover, labels map[string]string, ownerRefs []metav1.OwnerReference, password string) *appsv1.StatefulSet {
	name := GetRedisName(rf)
	namespace := rf.Namespace

	redisCommand := getRedisCommand(rf)
	selectorLabels := generateSelectorLabels(redisRoleName, rf.Name)
	labels = util.MergeLabels(labels, selectorLabels)
	// The operator changes the role label of the master pod, so a pod
	// anti-affinity that matches on it does not count the master.
	affinityLabels := labels
	labels = util.MergeLabels(labels, generateRedisDefaultRoleLabel())

	mac := hmac.New(sha256.New, []byte(rf.Namespace+"/"+rf.Name))
	_, _ = mac.Write([]byte(password))
	authSecretChecksum := fmt.Sprintf("%x", mac.Sum(nil))
	podAnnotations := util.MergeAnnotations(rf.Spec.Redis.PodAnnotations, map[string]string{
		redisAuthSecretChecksumAnnotation: authSecretChecksum,
	})

	volumeMounts := getRedisVolumeMounts(rf)
	volumes := getRedisVolumes(rf)
	terminationGracePeriodSeconds := getTerminationGracePeriodSeconds(rf)

	ss := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Annotations:     rf.Annotations,
			Name:            name,
			Namespace:       namespace,
			Labels:          labels,
			OwnerReferences: ownerRefs,
		},
		Spec: appsv1.StatefulSetSpec{
			ServiceName: name,
			Replicas:    &rf.Spec.Redis.Replicas,
			UpdateStrategy: appsv1.StatefulSetUpdateStrategy{
				Type: appsv1.OnDeleteStatefulSetStrategyType,
			},
			PodManagementPolicy: appsv1.ParallelPodManagement,
			Selector: &metav1.LabelSelector{
				MatchLabels: selectorLabels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      labels,
					Annotations: podAnnotations,
				},
				Spec: corev1.PodSpec{
					Affinity:                      getAffinity(rf.Spec.Redis.Affinity, affinityLabels),
					Tolerations:                   rf.Spec.Redis.Tolerations,
					TopologySpreadConstraints:     rf.Spec.Redis.TopologySpreadConstraints,
					NodeSelector:                  rf.Spec.Redis.NodeSelector,
					SecurityContext:               getSecurityContext(rf.Spec.Redis.SecurityContext),
					HostNetwork:                   rf.Spec.Redis.HostNetwork,
					DNSPolicy:                     getDnsPolicy(rf.Spec.Redis.DNSPolicy),
					ImagePullSecrets:              rf.Spec.Redis.ImagePullSecrets,
					PriorityClassName:             rf.Spec.Redis.PriorityClassName,
					ServiceAccountName:            rf.Spec.Redis.ServiceAccountName,
					EnableServiceLinks:            ptr.To(false),
					TerminationGracePeriodSeconds: &terminationGracePeriodSeconds,
					Containers: []corev1.Container{
						{
							Name:            redisContainerName,
							Image:           rf.Spec.Redis.Image,
							ImagePullPolicy: pullPolicy(rf.Spec.Redis.ImagePullPolicy),
							SecurityContext: getContainerSecurityContext(rf.Spec.Redis.ContainerSecurityContext),
							Ports: []corev1.ContainerPort{
								{
									Name:          "redis",
									ContainerPort: rf.Spec.Redis.Port,
									Protocol:      corev1.ProtocolTCP,
								},
							},
							VolumeMounts: volumeMounts,
							Command:      redisCommand,
							Resources:    rf.Spec.Redis.Resources,
							Lifecycle: &corev1.Lifecycle{
								PreStop: &corev1.LifecycleHandler{
									Exec: &corev1.ExecAction{
										Command: []string{"/bin/sh", "/redis-shutdown/shutdown.sh"},
									},
								},
							},
						},
					},
					Volumes: volumes,
				},
			},
		},
	}

	if rf.Spec.Redis.Storage.PersistentVolumeClaim != nil {
		pvc := corev1.PersistentVolumeClaim{
			TypeMeta: metav1.TypeMeta{
				APIVersion: "v1",
				Kind:       "PersistentVolumeClaim",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:              rf.Spec.Redis.Storage.PersistentVolumeClaim.Name,
				Labels:            rf.Spec.Redis.Storage.PersistentVolumeClaim.Labels,
				Annotations:       rf.Spec.Redis.Storage.PersistentVolumeClaim.Annotations,
				CreationTimestamp: metav1.Time{},
			},
			Spec:   rf.Spec.Redis.Storage.PersistentVolumeClaim.Spec,
			Status: rf.Spec.Redis.Storage.PersistentVolumeClaim.Status,
		}
		if !rf.Spec.Redis.Storage.KeepAfterDeletion {
			// The owner reference makes Kubernetes delete the PVCs with the
			// RedisFailover.
			pvc.OwnerReferences = ownerRefs
		}
		ss.Spec.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{
			pvc,
		}
	}

	if rf.Spec.Redis.CustomLivenessProbe != nil {
		ss.Spec.Template.Spec.Containers[0].LivenessProbe = rf.Spec.Redis.CustomLivenessProbe
	} else {
		ss.Spec.Template.Spec.Containers[0].LivenessProbe = &corev1.Probe{
			InitialDelaySeconds: graceTime,
			TimeoutSeconds:      5,
			FailureThreshold:    6,
			PeriodSeconds:       15,
			ProbeHandler: corev1.ProbeHandler{
				Exec: &corev1.ExecAction{
					Command: []string{
						"sh",
						"-c",
						// Bounded like ready.sh: a frozen server never answers.
						// redis-cli exits 0 on an error reply. With a Redis password,
						// a refused login gets NOAUTH, and the grep fails the probe.
						// A server that loads its dataset answers LOADING and is
						// alive: a restart starts the load again.
						fmt.Sprintf("t=; command -v timeout >/dev/null 2>&1 && t=\"timeout 2\"; $t redis-cli -h $(hostname) -p %[1]v --user pinger --pass pingpass --no-auth-warning ping | grep -qE '^(PONG|LOADING)'", rf.Spec.Redis.Port),
					},
				},
			},
		}
	}

	if rf.Spec.Redis.CustomReadinessProbe != nil {
		ss.Spec.Template.Spec.Containers[0].ReadinessProbe = rf.Spec.Redis.CustomReadinessProbe
	} else {
		ss.Spec.Template.Spec.Containers[0].ReadinessProbe = &corev1.Probe{
			InitialDelaySeconds: graceTime,
			TimeoutSeconds:      5,
			ProbeHandler: corev1.ProbeHandler{
				Exec: &corev1.ExecAction{
					Command: []string{"/bin/sh", "/redis-readiness/ready.sh"},
				},
			},
		}
	}

	if rf.Spec.Redis.CustomStartupProbe != nil {
		ss.Spec.Template.Spec.Containers[0].StartupProbe = rf.Spec.Redis.CustomStartupProbe
	} else if rf.Spec.Redis.StartupConfigMap != "" {
		ss.Spec.Template.Spec.Containers[0].StartupProbe = &corev1.Probe{
			InitialDelaySeconds: graceTime,
			TimeoutSeconds:      5,
			FailureThreshold:    6,
			PeriodSeconds:       15,
			ProbeHandler: corev1.ProbeHandler{
				Exec: &corev1.ExecAction{
					Command: []string{"/bin/sh", "/redis-startup/startup.sh"},
				},
			},
		}
	}

	if rf.Spec.Redis.Exporter.Enabled {
		exporter := createRedisExporterContainer(rf)
		ss.Spec.Template.Spec.Containers = append(ss.Spec.Template.Spec.Containers, exporter)
	}

	// Runs before any user-supplied init container, so a restore or migration
	// step starts from a directory with no stale tempfiles in it.
	ss.Spec.Template.Spec.InitContainers = append(
		[]corev1.Container{createRDBTempfileCleanupContainer(rf)},
		ss.Spec.Template.Spec.InitContainers...,
	)

	if rf.Spec.Redis.InitContainers != nil {
		initContainers := getInitContainersWithRedisEnv(rf)
		ss.Spec.Template.Spec.InitContainers = append(ss.Spec.Template.Spec.InitContainers, initContainers...)
	}

	if rf.Spec.Redis.ExtraContainers != nil {
		extraContainers := getExtraContainersWithRedisEnv(rf)
		ss.Spec.Template.Spec.Containers = append(ss.Spec.Template.Spec.Containers, extraContainers...)
	}

	// The env of the user comes before the env of the operator. On a duplicate
	// name the last one wins, so the user cannot replace REDIS_ADDR, REDIS_PORT,
	// REDIS_USER, and REDIS_PASSWORD (only set with auth.secretPath).
	redisEnv := getRedisEnv(rf)
	mainEnv := append(ss.Spec.Template.Spec.Containers[0].Env, rf.Spec.Redis.Env...)
	ss.Spec.Template.Spec.Containers[0].Env = append(mainEnv, redisEnv...)

	return ss
}

func generateSentinelDeployment(rf *redisfailoverv1.RedisFailover, labels map[string]string, ownerRefs []metav1.OwnerReference) *appsv1.Deployment {
	name := GetSentinelName(rf)
	configMapName := GetSentinelName(rf)
	namespace := rf.Namespace

	sentinelCommand := getSentinelCommand(rf)
	selectorLabels := generateSelectorLabels(sentinelRoleName, rf.Name)
	labels = util.MergeLabels(labels, selectorLabels)

	volumeMounts := getSentinelVolumeMounts(rf)
	volumes := getSentinelVolumes(rf, configMapName)

	serviceAccountName := rf.Spec.Sentinel.ServiceAccountName
	if serviceAccountName == "" {
		serviceAccountName = GetSentinelServiceAccountName(rf)
	}

	sd := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       namespace,
			Labels:          labels,
			OwnerReferences: ownerRefs,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &rf.Spec.Sentinel.Replicas,
			Strategy: rf.Spec.Sentinel.Strategy,
			Selector: &metav1.LabelSelector{
				MatchLabels: selectorLabels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      labels,
					Annotations: rf.Spec.Sentinel.PodAnnotations,
				},
				Spec: corev1.PodSpec{
					Affinity:                  getAffinity(rf.Spec.Sentinel.Affinity, labels),
					Tolerations:               rf.Spec.Sentinel.Tolerations,
					TopologySpreadConstraints: rf.Spec.Sentinel.TopologySpreadConstraints,
					NodeSelector:              rf.Spec.Sentinel.NodeSelector,
					SecurityContext:           getSecurityContext(rf.Spec.Sentinel.SecurityContext),
					HostNetwork:               rf.Spec.Sentinel.HostNetwork,
					DNSPolicy:                 getDnsPolicy(rf.Spec.Sentinel.DNSPolicy),
					ImagePullSecrets:          rf.Spec.Sentinel.ImagePullSecrets,
					PriorityClassName:         rf.Spec.Sentinel.PriorityClassName,
					ServiceAccountName:        serviceAccountName,
					EnableServiceLinks:        ptr.To(false),
					InitContainers: []corev1.Container{
						{
							Name:            "sentinel-config-copy",
							Image:           rf.Spec.Sentinel.Image,
							ImagePullPolicy: pullPolicy(rf.Spec.Sentinel.ImagePullPolicy),
							SecurityContext: getContainerSecurityContext(rf.Spec.Sentinel.ConfigCopy.ContainerSecurityContext),
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "sentinel-config",
									MountPath: "/redis",
								},
								{
									Name:      "sentinel-config-writable",
									MountPath: "/redis-writable",
								},
							},
							Command: []string{
								"cp",
								fmt.Sprintf("/redis/%s", sentinelConfigFileName),
								fmt.Sprintf("/redis-writable/%s", sentinelConfigFileName),
							},
							Resources: corev1.ResourceRequirements{
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("10m"),
									corev1.ResourceMemory: resource.MustParse("32Mi"),
								},
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("10m"),
									corev1.ResourceMemory: resource.MustParse("32Mi"),
								},
							},
						},
					},
					Containers: []corev1.Container{
						{
							Name:            "sentinel",
							Image:           rf.Spec.Sentinel.Image,
							ImagePullPolicy: pullPolicy(rf.Spec.Sentinel.ImagePullPolicy),
							SecurityContext: getContainerSecurityContext(rf.Spec.Sentinel.ContainerSecurityContext),
							Env:             rf.Spec.Sentinel.Env,
							Ports: []corev1.ContainerPort{
								{
									Name:          "sentinel",
									ContainerPort: 26379,
									Protocol:      corev1.ProtocolTCP,
								},
							},
							VolumeMounts: volumeMounts,
							Command:      sentinelCommand,
							Resources:    rf.Spec.Sentinel.Resources,
						},
					},
					Volumes: volumes,
				},
			},
		},
	}

	if rf.Spec.Sentinel.CustomLivenessProbe != nil {
		sd.Spec.Template.Spec.Containers[0].LivenessProbe = rf.Spec.Sentinel.CustomLivenessProbe
	} else {
		sd.Spec.Template.Spec.Containers[0].LivenessProbe = &corev1.Probe{
			InitialDelaySeconds: graceTime,
			TimeoutSeconds:      5,
			ProbeHandler: corev1.ProbeHandler{
				Exec: &corev1.ExecAction{
					Command: []string{
						"sh",
						"-c",
						"t=; command -v timeout >/dev/null 2>&1 && t=\"timeout 2\"; $t redis-cli -h $(hostname) -p 26379 ping",
					},
				},
			},
		}
	}

	if rf.Spec.Sentinel.CustomReadinessProbe != nil {
		sd.Spec.Template.Spec.Containers[0].ReadinessProbe = rf.Spec.Sentinel.CustomReadinessProbe
	} else {
		sd.Spec.Template.Spec.Containers[0].ReadinessProbe = &corev1.Probe{
			InitialDelaySeconds: graceTime,
			TimeoutSeconds:      5,
			ProbeHandler: corev1.ProbeHandler{
				Exec: &corev1.ExecAction{
					// The first check keeps the Sentinel not ready until it
					// monitors a real master. SENTINEL CKQUORUM keeps it not
					// ready while it cannot reach enough Sentinels for a
					// failover, for example in a network partition. Without
					// CKQUORUM, an isolated Sentinel stays ready with an old
					// master address. See
					// https://github.com/spotahome/redis-operator/issues/663.
					Command: []string{
						"sh",
						"-c",
						"t=; command -v timeout >/dev/null 2>&1 && t=\"timeout 2\"; $t redis-cli -h $(hostname) -p 26379 sentinel get-master-addr-by-name mymaster | head -n 1 | grep -vq '127.0.0.1' && $t redis-cli -h $(hostname) -p 26379 sentinel ckquorum mymaster | grep -q '^OK'",
					},
				},
			},
		}
	}

	if rf.Spec.Sentinel.CustomStartupProbe != nil {
		sd.Spec.Template.Spec.Containers[0].StartupProbe = rf.Spec.Sentinel.CustomStartupProbe
	} else if rf.Spec.Sentinel.StartupConfigMap != "" {
		sd.Spec.Template.Spec.Containers[0].StartupProbe = &corev1.Probe{
			InitialDelaySeconds: graceTime,
			TimeoutSeconds:      5,
			FailureThreshold:    6,
			PeriodSeconds:       15,
			ProbeHandler: corev1.ProbeHandler{
				Exec: &corev1.ExecAction{
					Command: []string{"/bin/sh", "/sentinel-startup/startup.sh"},
				},
			},
		}
	}

	if rf.Spec.Sentinel.Exporter.Enabled {
		exporter := createSentinelExporterContainer(rf)
		sd.Spec.Template.Spec.Containers = append(sd.Spec.Template.Spec.Containers, exporter)
	}
	if rf.Spec.Sentinel.InitContainers != nil {
		sd.Spec.Template.Spec.InitContainers = append(sd.Spec.Template.Spec.InitContainers, rf.Spec.Sentinel.InitContainers...)
	}

	if rf.Spec.Sentinel.ExtraContainers != nil {
		sd.Spec.Template.Spec.Containers = append(sd.Spec.Template.Spec.Containers, rf.Spec.Sentinel.ExtraContainers...)
	}

	return sd
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

func generatePodDisruptionBudget(name string, namespace string, labels map[string]string, ownerRefs []metav1.OwnerReference, minAvailable intstr.IntOrString, selectorLabels map[string]string) *policyv1.PodDisruptionBudget {
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

var exporterDefaultResourceRequirements = corev1.ResourceRequirements{
	Limits: corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse(exporterDefaultLimitCPU),
		corev1.ResourceMemory: resource.MustParse(exporterDefaultLimitMemory),
	},
	Requests: corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse(exporterDefaultRequestCPU),
		corev1.ResourceMemory: resource.MustParse(exporterDefaultRequestMemory),
	},
}

// redisExporterListenPort returns the port the redis exporter listens on,
// falling back to the built-in default when the spec leaves it unset.
func redisExporterListenPort(rf *redisfailoverv1.RedisFailover) int32 {
	if p := rf.Spec.Redis.Exporter.Port; p != 0 {
		return p
	}
	return exporterPort
}

// sentinelExporterListenPort returns the port the sentinel exporter listens on,
// falling back to the built-in default when the spec leaves it unset.
func sentinelExporterListenPort(rf *redisfailoverv1.RedisFailover) int32 {
	if p := rf.Spec.Sentinel.Exporter.Port; p != 0 {
		return p
	}
	return sentinelExporterPort
}

func createRedisExporterContainer(rf *redisfailoverv1.RedisFailover) corev1.Container {
	resources := exporterDefaultResourceRequirements
	if rf.Spec.Redis.Exporter.Resources != nil {
		resources = *rf.Spec.Redis.Exporter.Resources
	}
	container := corev1.Container{
		Name:            exporterContainerName,
		Image:           rf.Spec.Redis.Exporter.Image,
		ImagePullPolicy: pullPolicy(rf.Spec.Redis.Exporter.ImagePullPolicy),
		SecurityContext: getContainerSecurityContext(rf.Spec.Redis.Exporter.ContainerSecurityContext),
		Args:            rf.Spec.Redis.Exporter.Args,
		Env: append(rf.Spec.Redis.Exporter.Env, corev1.EnvVar{
			Name: "REDIS_ALIAS",
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{
					FieldPath: "metadata.name",
				},
			},
		},
		),
		Ports: []corev1.ContainerPort{
			{
				Name:          "metrics",
				ContainerPort: redisExporterListenPort(rf),
				Protocol:      corev1.ProtocolTCP,
			},
		},
		Resources: resources,
	}

	redisEnv := getRedisExporterEnv(rf)
	container.Env = append(container.Env, redisEnv...)
	// Only for a custom port, so default pod templates stay unchanged.
	if rf.Spec.Redis.Exporter.Port != 0 {
		container.Env = append(container.Env, corev1.EnvVar{
			Name:  "REDIS_EXPORTER_WEB_LISTEN_ADDRESS",
			Value: fmt.Sprintf("0.0.0.0:%d", rf.Spec.Redis.Exporter.Port),
		})
	}

	return container
}

func createSentinelExporterContainer(rf *redisfailoverv1.RedisFailover) corev1.Container {
	resources := exporterDefaultResourceRequirements
	if rf.Spec.Sentinel.Exporter.Resources != nil {
		resources = *rf.Spec.Sentinel.Exporter.Resources
	}
	listenPort := sentinelExporterListenPort(rf)
	container := corev1.Container{
		Name:            sentinelExporterContainerName,
		Image:           rf.Spec.Sentinel.Exporter.Image,
		ImagePullPolicy: pullPolicy(rf.Spec.Sentinel.Exporter.ImagePullPolicy),
		SecurityContext: getContainerSecurityContext(rf.Spec.Sentinel.Exporter.ContainerSecurityContext),
		Args:            rf.Spec.Sentinel.Exporter.Args,
		Env: append(rf.Spec.Sentinel.Exporter.Env, corev1.EnvVar{
			Name: "REDIS_ALIAS",
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{
					FieldPath: "metadata.name",
				},
			},
		}, corev1.EnvVar{
			Name:  "REDIS_EXPORTER_WEB_LISTEN_ADDRESS",
			Value: fmt.Sprintf("0.0.0.0:%[1]v", listenPort),
		}, corev1.EnvVar{
			Name:  "REDIS_ADDR",
			Value: "redis://127.0.0.1:26379",
		},
		),
		Ports: []corev1.ContainerPort{
			{
				Name:          "metrics",
				ContainerPort: listenPort,
				Protocol:      corev1.ProtocolTCP,
			},
		},
		Resources: resources,
	}

	return container
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
	defaultUserAndGroup := ptr.To(int64(1000))

	result := &corev1.PodSecurityContext{
		RunAsUser:    defaultUserAndGroup,
		RunAsGroup:   defaultUserAndGroup,
		RunAsNonRoot: ptr.To(true),
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

// createRDBTempfileCleanupContainer removes old RDB tempfiles before Redis
// starts.
//
// During a BGSAVE, Redis writes temp-<pid>.rdb and then renames it to the
// dump file. When the process stops during the save, for example after an
// OOM kill, the tempfile stays. Redis removes only the tempfile of its own
// child process. The files collect over restarts until the volume is full,
// and then each BGSAVE fails.
//
// rdb.c sets the temp-<pid>.rdb name, and dbfilename does not change it, so
// the pattern cannot match the dump file. An init container runs before
// Redis, so no BGSAVE runs during the delete.
func createRDBTempfileCleanupContainer(rf *redisfailoverv1.RedisFailover) corev1.Container {
	return corev1.Container{
		Name:            "rdb-tempfile-cleanup",
		Image:           rf.Spec.Redis.Image,
		ImagePullPolicy: pullPolicy(rf.Spec.Redis.ImagePullPolicy),
		SecurityContext: getContainerSecurityContext(rf.Spec.Redis.ContainerSecurityContext),
		Command: []string{
			"sh", "-c",
			"find /data -maxdepth 1 -type f -name 'temp-*.rdb' -delete",
		},
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      getRedisDataVolumeName(rf),
				MountPath: "/data",
			},
		},
		Resources: corev1.ResourceRequirements{
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("10m"),
				corev1.ResourceMemory: resource.MustParse("32Mi"),
			},
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("10m"),
				corev1.ResourceMemory: resource.MustParse("32Mi"),
			},
		},
	}
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
	defaultUserAndGroup := ptr.To(int64(1000))

	result := &corev1.SecurityContext{
		Capabilities:             capabilities,
		Privileged:               ptr.To(false),
		RunAsUser:                defaultUserAndGroup,
		RunAsGroup:               defaultUserAndGroup,
		RunAsNonRoot:             ptr.To(true),
		ReadOnlyRootFilesystem:   ptr.To(true),
		AllowPrivilegeEscalation: ptr.To(false),
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

func getDnsPolicy(dnspolicy corev1.DNSPolicy) corev1.DNSPolicy {
	if dnspolicy == "" {
		return corev1.DNSClusterFirst
	}
	return dnspolicy
}

func getQuorum(rf *redisfailoverv1.RedisFailover) int32 {
	return rf.Spec.Sentinel.Replicas/2 + 1
}

func getRedisVolumeMounts(rf *redisfailoverv1.RedisFailover) []corev1.VolumeMount {
	volumeMounts := []corev1.VolumeMount{
		{
			Name:      redisConfigurationVolumeName,
			MountPath: "/redis",
		},
		{
			Name:      redisShutdownConfigurationVolumeName,
			MountPath: "/redis-shutdown",
		},
		{
			Name:      redisReadinessVolumeName,
			MountPath: "/redis-readiness",
		},
		{
			Name:      getRedisDataVolumeName(rf),
			MountPath: "/data",
		},
	}

	if rf.Spec.Redis.StartupConfigMap != "" {
		startupVolumeMount := corev1.VolumeMount{
			Name:      redisStartupConfigurationVolumeName,
			MountPath: "/redis-startup",
		}

		volumeMounts = append(volumeMounts, startupVolumeMount)
	}

	if rf.Spec.Redis.ExtraVolumeMounts != nil {
		volumeMounts = append(volumeMounts, rf.Spec.Redis.ExtraVolumeMounts...)
	}

	return volumeMounts
}

func getSentinelVolumeMounts(rf *redisfailoverv1.RedisFailover) []corev1.VolumeMount {
	volumeMounts := []corev1.VolumeMount{
		{
			Name:      "sentinel-config-writable",
			MountPath: "/redis",
		},
	}

	if rf.Spec.Sentinel.StartupConfigMap != "" {
		startupVolumeMount := corev1.VolumeMount{
			Name:      "sentinel-startup-config",
			MountPath: "/sentinel-startup",
		}
		volumeMounts = append(volumeMounts, startupVolumeMount)
	}
	if rf.Spec.Sentinel.ExtraVolumeMounts != nil {
		volumeMounts = append(volumeMounts, rf.Spec.Sentinel.ExtraVolumeMounts...)
	}

	return volumeMounts
}

func getRedisVolumes(rf *redisfailoverv1.RedisFailover) []corev1.Volume {
	configMapName := GetRedisName(rf)
	shutdownConfigMapName := GetRedisShutdownConfigMapName(rf)
	readinessConfigMapName := GetRedisReadinessName(rf)

	executeMode := int32(0744)
	volumes := []corev1.Volume{
		{
			Name: redisConfigurationVolumeName,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: configMapName,
					},
				},
			},
		},
		{
			Name: redisShutdownConfigurationVolumeName,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: shutdownConfigMapName,
					},
					DefaultMode: &executeMode,
				},
			},
		},
		{
			Name: redisReadinessVolumeName,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: readinessConfigMapName,
					},
					DefaultMode: &executeMode,
				},
			},
		},
	}

	if rf.Spec.Redis.StartupConfigMap != "" {
		startupVolumeName := rf.Spec.Redis.StartupConfigMap
		startupVolume := corev1.Volume{
			Name: redisStartupConfigurationVolumeName,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: startupVolumeName,
					},
					DefaultMode: &executeMode,
				},
			},
		}
		volumes = append(volumes, startupVolume)
	}

	if rf.Spec.Redis.ExtraVolumes != nil {
		volumes = append(volumes, rf.Spec.Redis.ExtraVolumes...)
	}

	dataVolume := getRedisDataVolume(rf)
	if dataVolume != nil {
		volumes = append(volumes, *dataVolume)
	}

	return volumes
}

func getSentinelVolumes(rf *redisfailoverv1.RedisFailover, configMapName string) []corev1.Volume {
	executeMode := int32(0744)

	volumes := []corev1.Volume{
		{
			Name: "sentinel-config",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: configMapName,
					},
				},
			},
		},
		{
			Name: "sentinel-config-writable",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		},
	}

	if rf.Spec.Sentinel.StartupConfigMap != "" {
		startupVolumeName := rf.Spec.Sentinel.StartupConfigMap
		startupVolume := corev1.Volume{
			Name: sentinelStartupConfigurationVolumeName,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: startupVolumeName,
					},
					DefaultMode: &executeMode,
				},
			},
		}
		volumes = append(volumes, startupVolume)
	}

	if rf.Spec.Sentinel.ExtraVolumes != nil {
		volumes = append(volumes, rf.Spec.Sentinel.ExtraVolumes...)
	}

	return volumes
}

func getRedisDataVolume(rf *redisfailoverv1.RedisFailover) *corev1.Volume {
	// A PVC comes from the volumeClaimTemplates, so it needs no volume here.
	// Without a storage setting, the data volume is an EmptyDir.
	switch {
	case rf.Spec.Redis.Storage.PersistentVolumeClaim != nil:
		return nil
	case rf.Spec.Redis.Storage.EmptyDir != nil:
		return &corev1.Volume{
			Name: redisStorageVolumeName,
			VolumeSource: corev1.VolumeSource{
				EmptyDir: rf.Spec.Redis.Storage.EmptyDir,
			},
		}
	default:
		return &corev1.Volume{
			Name: redisStorageVolumeName,
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		}
	}
}

func getRedisDataVolumeName(rf *redisfailoverv1.RedisFailover) string {
	switch {
	case rf.Spec.Redis.Storage.PersistentVolumeClaim != nil:
		return rf.Spec.Redis.Storage.PersistentVolumeClaim.Name
	case rf.Spec.Redis.Storage.EmptyDir != nil:
		return redisStorageVolumeName
	default:
		return redisStorageVolumeName
	}
}

func getRedisCommand(rf *redisfailoverv1.RedisFailover) []string {
	if len(rf.Spec.Redis.Command) > 0 {
		return rf.Spec.Redis.Command
	}
	// A custom redis.command gets no password arguments. With auth, the
	// command gives requirepass and masterauth to redis-server from the
	// REDIS_PASSWORD env var, a SecretKeyRef. Thus the password is not in a
	// ConfigMap or in the pod spec. The shell expands the env var, and exec
	// keeps redis-server as PID 1, so it gets SIGTERM.
	if rf.Spec.Auth.SecretPath != "" {
		return []string{
			"sh", "-c",
			fmt.Sprintf(`exec redis-server /redis/%s --requirepass "$REDIS_PASSWORD" --masterauth "$REDIS_PASSWORD"`, redisConfigFileName),
		}
	}
	return []string{
		"redis-server",
		fmt.Sprintf("/redis/%s", redisConfigFileName),
	}
}

func getSentinelCommand(rf *redisfailoverv1.RedisFailover) []string {
	if len(rf.Spec.Sentinel.Command) > 0 {
		return rf.Spec.Sentinel.Command
	}
	return []string{
		"redis-server",
		fmt.Sprintf("/redis/%s", sentinelConfigFileName),
		"--sentinel",
	}
}

func pullPolicy(specPolicy corev1.PullPolicy) corev1.PullPolicy {
	if specPolicy == "" {
		return corev1.PullAlways
	}
	return specPolicy
}

func getTerminationGracePeriodSeconds(rf *redisfailoverv1.RedisFailover) int64 {
	if rf.Spec.Redis.TerminationGracePeriodSeconds > 0 {
		return rf.Spec.Redis.TerminationGracePeriodSeconds
	}
	return 30
}

func getExtraContainersWithRedisEnv(rf *redisfailoverv1.RedisFailover) []corev1.Container {
	env := getRedisEnv(rf)
	extraContainers := getContainersWithRedisEnv(rf.Spec.Redis.ExtraContainers, env)

	return extraContainers
}

func getInitContainersWithRedisEnv(rf *redisfailoverv1.RedisFailover) []corev1.Container {
	env := getRedisEnv(rf)
	initContainers := getContainersWithRedisEnv(rf.Spec.Redis.InitContainers, env)

	return initContainers
}

func getContainersWithRedisEnv(cs []corev1.Container, e []corev1.EnvVar) []corev1.Container {
	var containers []corev1.Container
	for _, c := range cs {
		c.Env = append(c.Env, e...)
		containers = append(containers, c)
	}

	return containers
}

func getRedisEnv(rf *redisfailoverv1.RedisFailover) []corev1.EnvVar {
	var env []corev1.EnvVar

	env = append(env, corev1.EnvVar{
		Name:  "REDIS_ADDR",
		Value: fmt.Sprintf("redis://127.0.0.1:%[1]v", rf.Spec.Redis.Port),
	})

	env = append(env, corev1.EnvVar{
		Name:  "REDIS_PORT",
		Value: fmt.Sprintf("%[1]v", rf.Spec.Redis.Port),
	})

	env = append(env, corev1.EnvVar{
		Name:  "REDIS_USER",
		Value: "default",
	})

	if rf.Spec.Auth.SecretPath != "" {
		env = append(env, corev1.EnvVar{
			Name: "REDIS_PASSWORD",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: rf.Spec.Auth.SecretPath,
					},
					Key: "password",
				},
			},
		})
	}

	return env
}

func getRedisExporterEnv(rf *redisfailoverv1.RedisFailover) []corev1.EnvVar {
	var env []corev1.EnvVar

	env = append(env, corev1.EnvVar{
		Name:  "REDIS_ADDR",
		Value: fmt.Sprintf("redis://127.0.0.1:%[1]v", rf.Spec.Redis.Port),
	})

	env = append(env, corev1.EnvVar{
		Name:  "REDIS_PORT",
		Value: fmt.Sprintf("%[1]v", rf.Spec.Redis.Port),
	})

	if !envExists(rf.Spec.Redis.Exporter.Env, "REDIS_USER") {
		env = append(env, corev1.EnvVar{
			Name:  "REDIS_USER",
			Value: "default",
		})
	}

	if !envExists(rf.Spec.Redis.Exporter.Env, "REDIS_PASSWORD") {
		if rf.Spec.Auth.SecretPath != "" {
			env = append(env, corev1.EnvVar{
				Name: "REDIS_PASSWORD",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: rf.Spec.Auth.SecretPath,
						},
						Key: "password",
					},
				},
			})
		}
	}
	return env
}

func envExists(env []corev1.EnvVar, name string) bool {
	for _, e := range env {
		if e.Name == name {
			return true
		}
	}
	return false
}
