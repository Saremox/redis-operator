package observer

import (
	"errors"
	"fmt"
	"net"
	"strconv"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
)

// Invariant names, the values of the invariant label.
const (
	invPods              = "pods"
	invOneMaster         = "one_master"
	invMasterService     = "master_service"
	invReplication       = "replication"
	invSentinelAgreement = "sentinel_agreement"
	invHealthy           = "healthy"
)

type pod struct {
	Name  string
	UID   string
	IP    string
	Ready bool
}

type redisPod struct {
	pod
	info info
	err  error
}

type sentinelPod struct {
	pod
	// master is the address the Sentinel reports for mymaster.
	master string
	err    error
}

// snapshot is everything one round of checks looks at.
type snapshot struct {
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
	master, err := s.master()
	checks := []check{
		{invPods, s.checkPods()},
		{invOneMaster, err},
		{invMasterService, s.checkMasterService(master)},
		{invReplication, s.checkReplication(master)},
	}
	if s.sentinel {
		checks = append(checks, check{invSentinelAgreement, s.checkSentinels(master)})
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
	var redis, sentinels []pod
	for _, p := range s.redis {
		redis = append(redis, p.pod)
	}
	for _, p := range s.sentinels {
		sentinels = append(sentinels, p.pod)
	}
	err := podsReady("redis", redis, s.redisReplicas)
	if s.sentinel {
		err = errors.Join(err, podsReady("sentinel", sentinels, s.sentinelReplicas))
	}
	return err
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

// lags returns each replica's replication offset behind the master, for
// the replicas that replicate from it.
func (s snapshot) lags(master *redisPod) map[string]int64 {
	lags := map[string]int64{}
	offset := master.info.int("master_repl_offset")
	for _, r := range s.redis {
		if r.Name == master.Name || r.info.role() != roleReplica || r.info["master_host"] != master.IP {
			continue
		}
		// The two INFO replies aren't taken at the same instant.
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

func (s snapshot) checkHealthy() error {
	if s.state != redisfailoverv1.HealthyState {
		return fmt.Errorf("state %q: %s", s.state, s.message)
	}
	return nil
}
