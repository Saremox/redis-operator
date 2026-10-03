package observer

import (
	"errors"
	"fmt"
	"net"
	"strconv"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/test/soak/internal/customconfig"
	"github.com/saremox/redis-operator/test/soak/internal/maxmem"
)

// Invariant names, the values of the invariant label.
const (
	invPods              = "pods"
	invOneMaster         = "one_master"
	invMasterService     = "master_service"
	invReplication       = "replication"
	invSentinelAgreement = "sentinel_agreement"
	invHealthy           = "healthy"
	invConfig            = "config"
	// invReplicaReadyWithoutData is a Ready replica whose link is down and was
	// never up since it started: Kubernetes sends reads to a pod without the
	// data of the master.
	invReplicaReadyWithoutData = "replica_ready_without_data"
	// invOOMKilled is judged apart from the others: every OOM kill is a
	// finding, in a convergence window too.
	invOOMKilled = "oom_killed"
)

type pod struct {
	Name  string
	UID   string
	IP    string
	Ready bool
	// OOMKills identify the pod's containers' OOM kills.
	OOMKills []string
}

type redisPod struct {
	pod
	info info
	err  error
	// limit is the redis container's memory limit.
	limit int64
	// config holds the customConfig keys, and maxmemory and
	// maxmemory-policy with maxMemory.
	config    map[string]string
	configErr error
}

type sentinelPod struct {
	pod
	// master is the address the Sentinel reports for mymaster, fields
	// SENTINEL MASTER mymaster.
	master string
	fields map[string]string
	err    error
}

// snapshot is everything one round of checks looks at.
type snapshot struct {
	// rf has the operator's defaults applied.
	rf  *redisfailoverv1.RedisFailover
	uid string
	// bootstrap is the node every pod replicates from, if any; then
	// sourceReplID and sourceOffset are where the source's master's
	// replication stream is.
	bootstrap        *redisfailoverv1.BootstrapSettings
	sourceReplID     string
	sourceOffset     int64
	sourceErr        error
	pvc              bool
	sentinel         bool
	redisReplicas    int32
	sentinelReplicas int32
	port             int
	state            string
	message          string
	redis            []redisPod
	sentinels        []sentinelPod
	// endpoints are the ready addresses of the rfrm EndpointSlices.
	endpoints []string
}

type check struct {
	invariant string
	err       error
}

func evaluate(s snapshot) []check {
	var checks []check
	if s.bootstrap != nil {
		// No pod is the master: each replicates from the bootstrap node,
		// and the operator labels none master.
		checks = []check{
			{invPods, s.checkPods()},
			{invOneMaster, s.checkBootstrap()},
			{invMasterService, s.checkNoMasterService()},
		}
	} else {
		master, err := s.master()
		checks = []check{
			{invPods, s.checkPods()},
			{invOneMaster, err},
			{invMasterService, s.checkMasterService(master)},
			{invReplication, s.checkReplication(master)},
		}
		if s.sentinel {
			checks = append(checks, check{invSentinelAgreement, s.checkSentinels(master)})
		}
	}
	checks = append(checks, check{invReplicaReadyWithoutData, s.checkReadyReplicas()})
	if s.rf != nil {
		checks = append(checks, check{invConfig, s.checkConfig()})
	}
	return append(checks, check{invHealthy, s.checkHealthy()})
}

var errNoMaster = errors.New("no single master")

func (s snapshot) masters() []*redisPod {
	var masters []*redisPod
	for i := range s.redis {
		if s.redis[i].info.role() == roleMaster {
			masters = append(masters, &s.redis[i])
		}
	}
	return masters
}

func (s snapshot) master() (*redisPod, error) {
	masters := s.masters()
	if len(masters) != 1 {
		names := make([]string, len(masters))
		for i, m := range masters {
			names[i] = m.Name
		}
		return nil, fmt.Errorf("%d masters %v", len(masters), names)
	}
	return masters[0], nil
}

func (s snapshot) checkPods() error {
	var redis []pod
	for _, p := range s.redis {
		redis = append(redis, p.pod)
	}
	err := podsReady("redis", redis, s.redisReplicas)
	if s.sentinel {
		err = errors.Join(err, podsReady("sentinel", sentinelPods(s), s.sentinelReplicas))
	}
	return err
}

func sentinelPods(s snapshot) []pod {
	var pods []pod
	for _, p := range s.sentinels {
		pods = append(pods, p.pod)
	}
	return pods
}

func podsReady(kind string, pods []pod, want int32) error {
	var errs []error
	if len(pods) != int(want) {
		errs = append(errs, fmt.Errorf("%d %s pods, want %d", len(pods), kind, want))
	}
	for _, p := range pods {
		if !p.Ready {
			errs = append(errs, fmt.Errorf("%s not ready", p.Name))
		}
	}
	return errors.Join(errs...)
}

func (s snapshot) checkMasterService(master *redisPod) error {
	if master == nil {
		return errNoMaster
	}
	if len(s.endpoints) != 1 {
		return fmt.Errorf("%d ready endpoints %v, want only %s (%s)", len(s.endpoints), s.endpoints, master.IP, master.Name)
	}
	if s.endpoints[0] != master.IP {
		return fmt.Errorf("endpoint %s is not the master %s (%s)", s.endpoints[0], master.IP, master.Name)
	}
	return nil
}

func (s snapshot) checkReplication(master *redisPod) error {
	if master == nil {
		return errNoMaster
	}
	port := strconv.Itoa(s.port)
	var errs []error
	for _, r := range s.redis {
		switch {
		case r.Name == master.Name:
		case r.info == nil:
			errs = append(errs, fmt.Errorf("%s: %w", r.Name, r.err))
		case r.info.role() != roleReplica:
			errs = append(errs, fmt.Errorf("%s: role %q", r.Name, r.info["role"]))
		case r.info["master_host"] != master.IP || r.info["master_port"] != port:
			errs = append(errs, fmt.Errorf("%s replicates from %s:%s, the master is %s:%s (%s)",
				r.Name, r.info["master_host"], r.info["master_port"], master.IP, port, master.Name))
		case r.info["master_link_status"] != "up":
			errs = append(errs, fmt.Errorf("%s: master_link_status %s", r.Name, r.info["master_link_status"]))
		}
	}
	return errors.Join(errs...)
}

// checkBootstrap checks that every pod replicates from the bootstrap node
// with its link up, and follows the stream of the source's current master.
func (s snapshot) checkBootstrap() error {
	var errs []error
	for _, r := range s.redis {
		switch {
		case r.info == nil:
			errs = append(errs, fmt.Errorf("%s: %w", r.Name, r.err))
		case r.info.role() != roleReplica:
			errs = append(errs, fmt.Errorf("%s: role %q", r.Name, r.info["role"]))
		case r.info["master_host"] != s.bootstrap.Host || r.info["master_port"] != s.bootstrap.Port:
			errs = append(errs, fmt.Errorf("%s replicates from %s:%s, the bootstrap node is %s:%s",
				r.Name, r.info["master_host"], r.info["master_port"], s.bootstrap.Host, s.bootstrap.Port))
		case r.info["master_link_status"] != "up":
			errs = append(errs, fmt.Errorf("%s: master_link_status %s", r.Name, r.info["master_link_status"]))
		case s.sourceErr == nil && r.info["master_replid"] != s.sourceReplID:
			errs = append(errs, fmt.Errorf("%s replicates stream %s, the source's master is on %s",
				r.Name, r.info["master_replid"], s.sourceReplID))
		}
	}
	return errors.Join(errs...)
}

// checkReadyReplicas checks that no Ready redis pod is a replica that never
// completed a sync: its link is down and was never up since the server
// started. Redis and Valkey then report master_link_down_since_seconds -1.
func (s snapshot) checkReadyReplicas() error {
	var errs []error
	for _, r := range s.redis {
		if r.Ready && r.info.role() == roleReplica && r.info["master_link_status"] != "up" &&
			r.info["master_link_down_since_seconds"] == "-1" {
			errs = append(errs, fmt.Errorf("%s is Ready without a completed sync: its link to %s:%s is %s and hasn't been up since it started",
				r.Name, r.info["master_host"], r.info["master_port"], r.info["master_link_status"]))
		}
	}
	return errors.Join(errs...)
}

func (s snapshot) checkNoMasterService() error {
	if len(s.endpoints) != 0 {
		return fmt.Errorf("%d ready endpoints %v, want none: no pod is the master", len(s.endpoints), s.endpoints)
	}
	return nil
}

// bootstrapLags returns each pod's replication offset behind the bootstrap
// node's, for the pods on its stream.
func (s snapshot) bootstrapLags() map[string]int64 {
	lags := map[string]int64{}
	for _, r := range s.redis {
		if r.info.role() == roleReplica && r.info["master_host"] == s.bootstrap.Host && r.info["master_replid"] == s.sourceReplID {
			lags[r.Name] = max(0, s.sourceOffset-r.info.int("slave_repl_offset"))
		}
	}
	return lags
}

// lags returns each replica's replication offset behind the master, for
// the replicas that replicate from it.
func (s snapshot) lags(master *redisPod) map[string]int64 {
	lags := map[string]int64{}
	offset := master.info.int("master_repl_offset")
	for _, r := range s.redis {
		if r.Name == master.Name || r.info.role() != roleReplica || r.info["master_host"] != master.IP {
			continue
		}
		// The two INFO replies are not from the same instant.
		lags[r.Name] = max(0, offset-r.info.int("slave_repl_offset"))
	}
	return lags
}

func (s snapshot) checkSentinels(master *redisPod) error {
	if master == nil {
		return errNoMaster
	}
	want := net.JoinHostPort(master.IP, strconv.Itoa(s.port))
	var errs []error
	for _, p := range s.sentinels {
		switch {
		case p.err != nil:
			errs = append(errs, fmt.Errorf("%s: %w", p.Name, p.err))
		case p.master != want:
			errs = append(errs, fmt.Errorf("%s reports %s, the master is %s (%s)", p.Name, p.master, want, master.Name))
		}
	}
	return errors.Join(errs...)
}

// checkConfig checks every redis pod with an IP, the pods the operator
// configures, against customConfig and the managed maxmemory settings, and
// with Sentinel every Sentinel against Sentinel's customConfig.
func (s snapshot) checkConfig() error {
	var errs []error
	var pods []maxmem.Pod
	for _, p := range s.redis {
		if p.IP == "" {
			continue
		}
		if p.configErr != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p.Name, p.configErr))
			continue
		}
		if err := customconfig.Redis(s.rf, p.config); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p.Name, err))
		}
		mp := maxmem.Pod{Name: p.Name, Limit: p.limit}
		mp.MaxMemory, mp.Err = strconv.ParseInt(p.config["maxmemory"], 10, 64)
		mp.Policy = p.config["maxmemory-policy"]
		pods = append(pods, mp)
	}
	if s.rf.Spec.Redis.MaxMemory != nil {
		errs = append(errs, maxmem.Check(s.rf, s.message, pods))
	}
	if s.sentinel {
		for _, p := range s.sentinels {
			if p.err != nil {
				continue
			}
			if err := customconfig.Sentinel(s.rf, p.fields); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", p.Name, err))
			}
		}
	}
	return errors.Join(errs...)
}

func (s snapshot) checkHealthy() error {
	if s.state != redisfailoverv1.HealthyState {
		return fmt.Errorf("state %q: %s", s.state, s.message)
	}
	return nil
}
