package service

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"text/template"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/operator/redisfailover/util"
)

// sentinelConfigTemplate monitors a placeholder master. The operator replaces
// that monitor with SENTINEL MONITOR, which uses the Sentinel built-in
// timeouts, and then applies customConfig. Thus the timeouts here apply only to
// the placeholder. They are the customConfig defaults, so that each timeout has
// one value in the code.
var sentinelConfigTemplate = fmt.Sprintf(`sentinel monitor {{masterName .}} 127.0.0.1 {{.Spec.Redis.Port}} 2
sentinel down-after-milliseconds {{masterName .}} %d
sentinel failover-timeout {{masterName .}} %d
sentinel parallel-syncs {{masterName .}} 2`, redisfailoverv1.DefaultSentinelDownAfterMilliseconds, redisfailoverv1.DefaultSentinelFailoverTimeoutMilliseconds)

func generateSentinelConfigMap(rf *redisfailoverv1.RedisFailover, labels map[string]string, ownerRefs []metav1.OwnerReference) *corev1.ConfigMap {
	name := GetSentinelName(rf)
	namespace := rf.Namespace

	labels = util.MergeLabels(labels, generateSelectorLabels(sentinelRoleName, rf.Name))

	tmpl, err := template.New("sentinel").Funcs(template.FuncMap{"masterName": SentinelMasterName}).Parse(sentinelConfigTemplate)
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

// redisShutdownFailoverScript is the part of shutdown.sh that asks Sentinel
// for a failover. Its format arguments are the Sentinel Service name and the
// Redis port.
//
// The preStop hook runs this script with /bin/sh, which is BusyBox ash on
// the Alpine Redis images. Thus the script uses only POSIX syntax, without
// "let" and "[[ ]]". The sleep of BusyBox and of GNU coreutils accepts
// 0.1 seconds. The script retries both Sentinel calls, because one
// failed call must not stop the failover. The Redis pods have no service
// links, so the script finds Sentinel through the DNS name of the Sentinel
// Service.
//
// The kubelet stops the hook at the end of the grace period, and then the
// script cannot SAVE. The timing assumes the default grace period of 30s
// (redis.terminationGracePeriod). Each call has a 2s limit, and the
// Sentinel part ends before 19s. The last poll starts before the 12s
// deadline and ends before 15s. REPLICAOF and CLIENT UNPAUSE add a maximum
// of 4s. The last failover request starts at the latest 3s before the
// deadline. Thus an accepted request always leaves time for one poll and
// the REPLICAOF.
//
// Until Sentinel promotes a replica, the master still accepts writes, and
// the promotion loses those that did not reach the replica. Thus the script
// pauses the writes, and makes the old master a replica of the new master
// before it ends the pause: the clients then get READONLY and not a lost
// OK. The pause stops 6s after the deadline, also if the script stops.
//
// Sentinel promotes the replica with the highest offset that it knows, but
// this offset can be 10s old. Thus the script waits until each online
// replica confirms the offset of the master. The replicas confirm once per
// second. WAIT does not help: it waits only for the commands of its own
// connection, so on a new connection it returns at once. The wait stops 8s
// before the deadline. Then one failover request fits, and two fit when
// INFO answers. When the master lookups used this time, the script does
// not wait: a lookup that finds the master ends before 8s, so one failover
// request still fits.
//
// Without the pause, a failover loses writes, so the script asks for none.
// Then Redis 7 and later pause the writes on SIGTERM and wait for the
// replicas, and Sentinel fails over after down-after-milliseconds. The
// pause also fails after a password change in place: the env keeps the old
// password. Sentinel has no password, so only the local calls send
// REDIS_PASSWORD.
const redisShutdownFailoverScript = `t=; command -v timeout >/dev/null 2>&1 && t="timeout 2"
local_cli() {
	if [ -n "${REDIS_PASSWORD}" ]; then
		REDISCLI_AUTH="${REDIS_PASSWORD}" $t redis-cli -p %[2]v "$@"
	else
		$t redis-cli -p %[2]v "$@"
	fi
}
replicas_behind() {
	info=$(local_cli INFO replication | tr -d '\r')
	top=""
	for line in $info; do
		case "$line" in
		master_repl_offset:*) top=${line#*:} ;;
		esac
	done
	[ -n "$top" ] || return
	n=0
	for line in $info; do
		case "$line" in
		slave*state=online,offset=*)
			offset=${line#*,offset=}
			if [ "${offset%%%%,*}" -lt "$top" ]; then n=$((n + 1)); fi ;;
		esac
	done
	echo "$n"
}
deadline=$(($(date +%%s) + 12))
self=$(hostname -i)
master=""
retries=0
while [ -z "$master" ] && [ "$retries" -lt 3 ]; do
	retries=$((retries + 1))
	master=$($t redis-cli -h %[1]v -p 26379 --csv SENTINEL get-master-addr-by-name %[3]v | tr ',' ' ' | tr -d '\"' |cut -d' ' -f1)
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
  	behind=""
  	while [ "$behind" != 0 ] && [ "$(date +%%s)" -lt "$((deadline - 8))" ]; do
  		behind=$(replicas_behind)
  		if [ "$behind" != 0 ]; then
  			sleep 0.1
  		fi
  	done
  	if [ "$behind" != 0 ]; then
  		echo "shutdown.sh: not all online replicas confirmed the writes, the failover can lose writes (replicas behind: ${behind:-unknown})" >&2
  	fi
  	failover=""
  	retries=0
  	while [ "$failover" != "OK" ] && [ "$retries" -lt 3 ] && [ "$(date +%%s)" -lt "$((deadline - 3))" ]; do
  		retries=$((retries + 1))
  		failover=$($t redis-cli -h %[1]v -p 26379 SENTINEL failover %[3]v)
  		if [ "$failover" != "OK" ]; then
  			sleep 1
  		fi
  	done
  	if [ "$failover" != "OK" ]; then
  		echo "shutdown.sh: sentinel did not accept the failover after $retries attempts: $failover" >&2
  	else
  		while { [ -z "$master" ] || [ "$master" = "$self" ]; } && [ "$(date +%%s)" -lt "$deadline" ]; do
  			sleep 1
  			master=$($t redis-cli -h %[1]v -p 26379 --csv SENTINEL get-master-addr-by-name %[3]v | tr ',' ' ' | tr -d '\"' |cut -d' ' -f1)
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
`

func generateRedisShutdownConfigMap(rf *redisfailoverv1.RedisFailover, labels map[string]string, ownerRefs []metav1.OwnerReference) *corev1.ConfigMap {
	name := GetRedisShutdownConfigMapName(rf)
	port := rf.Spec.Redis.Port
	namespace := rf.Namespace

	labels = util.MergeLabels(labels, generateSelectorLabels(redisRoleName, rf.Name))
	// With sentinel.enabled false nothing runs this script: redisLifecycle
	// gives the Redis container no preStop hook, because Redis saves on
	// SIGTERM by itself. The ConfigMap and its mount stay in both modes, so
	// that nothing else has to change and a user who runs the script by hand
	// still finds it.
	shutdownContent := ""
	if rf.SentinelsAllowed() {
		shutdownContent = fmt.Sprintf(redisShutdownFailoverScript, GetSentinelName(rf), port, SentinelMasterName(rf))
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
		window += SentinelFailoverWindow(rf)
	}
	return int64(window.Round(time.Second) / time.Second)
}

// SentinelFailoverWindow is the time that Sentinel takes to start a failover
// after the loss of the master and to end it: the down-after-milliseconds plus
// the failover-timeout of the Sentinel customConfig.
func SentinelFailoverWindow(rf *redisfailoverv1.RedisFailover) time.Duration {
	downAfter, failoverTimeout := SentinelTimeouts(rf)
	return downAfter + failoverTimeout
}

// SentinelTimeouts returns the down-after-milliseconds and the
// failover-timeout of the Sentinel customConfig.
func SentinelTimeouts(rf *redisfailoverv1.RedisFailover) (time.Duration, time.Duration) {
	return sentinelConfigMilliseconds(rf, "down-after-milliseconds", redisfailoverv1.DefaultSentinelDownAfterMilliseconds),
		sentinelConfigMilliseconds(rf, "failover-timeout", redisfailoverv1.DefaultSentinelFailoverTimeoutMilliseconds)
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

// redisReadinessScript is ready.sh. Its format arguments are the Redis port
// and the result of redisReadinessMaxLinkDownSeconds.
const redisReadinessScript = `ROLE="role"
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
esac`

func generateRedisReadinessConfigMap(rf *redisfailoverv1.RedisFailover, labels map[string]string, ownerRefs []metav1.OwnerReference) *corev1.ConfigMap {
	name := GetRedisReadinessName(rf)
	port := rf.Spec.Redis.Port
	namespace := rf.Namespace

	labels = util.MergeLabels(labels, generateSelectorLabels(redisRoleName, rf.Name))
	readinessContent := fmt.Sprintf(redisReadinessScript, port, redisReadinessMaxLinkDownSeconds(rf))

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
