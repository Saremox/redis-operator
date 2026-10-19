package redisfailover

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/log"
)

// sentinelUnknownPersist is the time from the first check that finds an
// unknown address to the check that reports it. The other Sentinels keep a
// deleted Sentinel as a peer until their reset. The reset comes after the
// hold-off of 25s and the next resync of 30s, or later in a failover.
const sentinelUnknownPersist = 90 * time.Second

// sentinelOwnIPMemory is the time that an IP of a pod of the RedisFailover
// stays known after the last check that found the pod. Thus the old IP of a
// deleted pod is not unknown while the Sentinels still know it.
const sentinelOwnIPMemory = 5 * time.Minute

// unknownAddr is an address that the Sentinels know and that no pod of the
// RedisFailover has.
type unknownAddr struct {
	since    time.Time
	reported bool
}

// sentinelUnknownAddrs holds the unknown peers and the unknown masters of the
// Sentinels of a RedisFailover by IP. own holds the last check that found
// each IP on a pod of the RedisFailover.
type sentinelUnknownAddrs struct {
	peers, masters map[string]unknownAddr
	own            map[string]time.Time
}

// unknownAddrLog holds the log messages of a kind of unknown address. Each
// message takes the list of the addresses.
type unknownAddrLog struct {
	start, last, end string
}

var (
	unknownPeerLog = unknownAddrLog{
		start: "Sentinels know the unknown peers %s. No Sentinel pod of this RedisFailover has these IPs. Each IP is a Sentinel of another RedisFailover, which can merge with this group, or an old IP of a deleted pod of this RedisFailover",
		last:  "Sentinels still know the unknown peers %s",
		end:   "The unknown peers %s are gone from the Sentinels",
	}
	unknownMasterLog = unknownAddrLog{
		start: "Sentinels monitor the unknown master addresses %s. No Redis pod of this RedisFailover has these IPs. Each IP is a master of another RedisFailover, which the Sentinels can follow, or an old IP of a deleted pod of this RedisFailover",
		last:  "Sentinels still monitor the unknown master addresses %s",
		end:   "The unknown master addresses %s are gone from the Sentinels",
	}
)

// observeSentinelAddrs reports the peers and the masters of the Sentinels that
// are no pods of the RedisFailover. A Sentinel accepts each peer that sends the
// same master name. Thus a Sentinel of another RedisFailover at an old IP of
// an own Sentinel merges the two groups, and they exchange their masters. The
// operator only reports this and changes nothing.
func (r *RedisFailoverHandler) observeSentinelAddrs(rf *redisfailoverv1.RedisFailover) {
	logger := r.logger.WithField("redisfailover", rf.Name).WithField("namespace", rf.Namespace)
	addrs, err := r.rfChecker.GetUnknownSentinelAddrs(rf)
	if err != nil {
		logger.Debugf("The operator did not list the pods, so it does not check the Sentinel peers: %v", err)
		return
	}
	// A new Sentinel gives 127.0.0.1, which is no pod.
	maps.DeleteFunc(addrs.Masters, func(ip string, _ []string) bool { return !credibleMaster(ip) })
	// A Sentinel without an answer can know an address that the others do
	// not know, so no address ends in this check.
	complete := len(addrs.Unread) == 0
	if !complete {
		logger.Debugf("Sentinels %s did not give their peers, so no unknown address ends in this check", strings.Join(addrs.Unread, ", "))
	}
	key := failoverKey(rf)
	state := r.loadSentinelUnknown(key)
	now := r.now()
	for ip := range addrs.Own {
		state.own[ip] = now
	}
	maps.DeleteFunc(state.own, func(_ string, at time.Time) bool { return now.Sub(at) >= sentinelOwnIPMemory })
	recent := func(ip string, _ []string) bool { _, ok := state.own[ip]; return ok }
	maps.DeleteFunc(addrs.Peers, recent)
	maps.DeleteFunc(addrs.Masters, recent)
	peers := trackUnknownAddrs(logger, unknownPeerLog, state.peers, addrs.Peers, complete, now)
	masters := trackUnknownAddrs(logger, unknownMasterLog, state.masters, addrs.Masters, complete, now)
	r.sentinelUnknown.Store(key, state)
	r.mClient.SetSentinelUnknownAddrs(rf.Namespace, rf.Name, peers, masters > 0)
}

// loadSentinelUnknown returns the state of key, or an empty state.
func (r *RedisFailoverHandler) loadSentinelUnknown(key string) sentinelUnknownAddrs {
	v, _ := r.sentinelUnknown.Load(key)
	if state, ok := v.(sentinelUnknownAddrs); ok {
		return state
	}
	return sentinelUnknownAddrs{peers: map[string]unknownAddr{}, masters: map[string]unknownAddr{}, own: map[string]time.Time{}}
}

// skipSentinelAddrs ends the unknown addresses in a reconcile without the
// check, for example without a Sentinel quorum or in another mode. Otherwise
// the gauges keep a value that no check confirms. The known pod IPs stay.
func (r *RedisFailoverHandler) skipSentinelAddrs(rf *redisfailoverv1.RedisFailover) {
	key := failoverKey(rf)
	v, ok := r.sentinelUnknown.Load(key)
	state, _ := v.(sentinelUnknownAddrs)
	if !ok || len(state.peers)+len(state.masters) == 0 {
		return
	}
	var ended []string
	for _, known := range []map[string]unknownAddr{state.peers, state.masters} {
		for ip, addr := range known {
			if addr.reported {
				ended = append(ended, ip)
			}
		}
		clear(known)
	}
	if len(ended) > 0 {
		slices.Sort(ended)
		r.logger.WithField("redisfailover", rf.Name).WithField("namespace", rf.Namespace).
			Infof("The operator does not check the Sentinels in this reconcile, so the report of the unknown addresses %s ends", strings.Join(ended, ", "))
		r.mClient.SetSentinelUnknownAddrs(rf.Namespace, rf.Name, 0, false)
	}
}

// trackUnknownAddrs adds the addresses of seen to known, and with complete
// removes the addresses that seen does not have. An address is reported when
// a check finds it again sentinelUnknownPersist after the first check. It
// logs the changes, and returns the number of reported addresses.
func trackUnknownAddrs(logger log.Logger, msgs unknownAddrLog, known map[string]unknownAddr, seen map[string][]string, complete bool, now time.Time) int {
	var started, lasting, ended []string
	for ip, sentinels := range seen {
		addr, ok := known[ip]
		switch {
		case !ok:
			known[ip] = unknownAddr{since: now}
		case addr.reported:
			lasting = append(lasting, describeUnknownAddr(ip, sentinels))
		case now.Sub(addr.since) >= sentinelUnknownPersist:
			known[ip] = unknownAddr{since: addr.since, reported: true}
			started = append(started, describeUnknownAddr(ip, sentinels))
		}
	}
	for ip, addr := range known {
		if _, ok := seen[ip]; ok || !complete {
			continue
		}
		delete(known, ip)
		if addr.reported {
			ended = append(ended, ip)
		}
	}
	for _, line := range []struct {
		logf  func(string, ...any)
		msg   string
		addrs []string
	}{{logger.Warningf, msgs.start, started}, {logger.Debugf, msgs.last, lasting}, {logger.Infof, msgs.end, ended}} {
		if len(line.addrs) > 0 {
			slices.Sort(line.addrs)
			line.logf(line.msg, strings.Join(line.addrs, ", "))
		}
	}
	reported := 0
	for _, addr := range known {
		if addr.reported {
			reported++
		}
	}
	return reported
}

// describeUnknownAddr gives the address and the Sentinels that know it.
func describeUnknownAddr(ip string, sentinels []string) string {
	sentinels = slices.Clone(sentinels)
	slices.Sort(sentinels)
	return fmt.Sprintf("%s (Sentinels %s)", ip, strings.Join(sentinels, " "))
}
