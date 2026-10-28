package redisfailover

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/log"
	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
	"github.com/saremox/redis-operator/service/redis"
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

// sentinelPeerHelloFresh is the age of the last hello of an alive peer. A
// Sentinel sends a hello every 2 seconds.
const sentinelPeerHelloFresh = 10 * time.Second

// sentinelPeerResetMemory is the time after the reset of a peer in which the
// return of that peer shows a Sentinel of another group.
const sentinelPeerResetMemory = 5 * time.Minute

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
	// runIDs holds the run IDs of the alive peers at each IP of a Sentinel
	// pod, for as long as own keeps the IP.
	runIDs map[string]map[string]bool
	// resets holds, by peer IP and then by Sentinel IP, the time of the reset
	// that removed the peer from that Sentinel.
	resets map[string]map[string]time.Time
	// foreign holds, by peer IP and then by Sentinel IP, the peers that a
	// check found as a Sentinel of another group. An entry stays until a check
	// of that Sentinel finds the peer dead or gone, so the memory of the reset
	// and of the run IDs does not end it.
	foreign map[string]map[string]bool
	// stale holds, by Sentinel IP, the peers of the last check that are no
	// Sentinel pod. A reset removes them.
	stale map[string][]string
	// unset holds the Sentinels of the last check that monitor no Redis and
	// know no alive peer that is no Sentinel pod.
	unset map[string]bool
	// merged holds the description of the addresses of another group, while
	// the operator sends no SENTINEL MONITOR or RESET. It is empty without
	// such addresses. mergedIPs holds the IPs of these addresses.
	merged    string
	mergedIPs []string
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
// check also finds the peers that a reset removes, and the merge that stops
// the SENTINEL MONITOR and RESET of the operator.
func (r *RedisFailoverHandler) observeSentinelAddrs(rf *redisfailoverv1.RedisFailover) {
	logger := r.logger.WithField("redisfailover", rf.Name).WithField("namespace", rf.Namespace)
	key := failoverKey(rf)
	state := r.loadSentinelUnknown(key)
	// The heal does not reset a peer of an old check.
	state.stale, state.unset = nil, nil
	addrs, err := r.rfChecker.GetUnknownSentinelAddrs(rf)
	if err != nil {
		logger.Debugf("The operator did not list the pods, so it does not check the Sentinel peers: %v", err)
		r.sentinelUnknown.Store(key, state)
		setMergedStatus(rf, state.mergedIPs)
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
	now := r.now()
	for ip := range addrs.Own {
		state.own[ip] = now
	}
	maps.DeleteFunc(state.own, func(_ string, at time.Time) bool { return now.Sub(at) >= sentinelOwnIPMemory })
	state.learnPeers(addrs, now)
	r.checkSentinelMerge(logger, rf, &state, addrs, complete)

	// A peer at the IP of a deleted pod that sends hellos with a run ID that
	// the pod did not have is a new Sentinel, so the memory of the IP does
	// not hide it.
	newPeer := state.newPeers(addrs)
	recent := func(ip string, _ []string) bool { _, ok := state.own[ip]; return ok && !newPeer[ip] }
	maps.DeleteFunc(addrs.Peers, recent)
	maps.DeleteFunc(addrs.Masters, recent)
	peers := trackUnknownAddrs(logger, unknownPeerLog, state.peers, addrs.Peers, complete, now)
	masters := trackUnknownAddrs(logger, unknownMasterLog, state.masters, addrs.Masters, complete, now)
	r.sentinelUnknown.Store(key, state)
	r.mClient.SetSentinelUnknownAddrs(rf.Namespace, rf.Name, peers, masters > 0)
}

// alivePeer reports whether the peer sent a hello in the last
// sentinelPeerHelloFresh. Only a hello merges two groups. A Sentinel with
// another master name answers PING but sends no hello for this name. Without
// the age of the hello, a peer without the flags s_down and disconnected is
// alive.
func alivePeer(peer redis.SentinelPeer) bool {
	if peer.LastHello < 0 {
		return !peer.Down
	}
	return peer.LastHello < sentinelPeerHelloFresh
}

// learnPeers keeps the run IDs at the IPs of the Sentinel pods, and the
// recent resets of the peers that are no pod.
func (s *sentinelUnknownAddrs) learnPeers(addrs rfservice.SentinelAddrs, now time.Time) {
	for _, group := range addrs.Groups {
		for _, peer := range group.Peers {
			if _, unknown := addrs.Peers[peer.IP]; unknown || !alivePeer(peer) || peer.RunID == "" {
				continue
			}
			if s.runIDs[peer.IP] == nil {
				s.runIDs[peer.IP] = map[string]bool{}
			}
			s.runIDs[peer.IP][peer.RunID] = true
		}
	}
	maps.DeleteFunc(s.runIDs, func(ip string, _ map[string]bool) bool { _, ok := s.own[ip]; return !ok })
	// A reset peer that a check does not find stays, because a Sentinel of
	// another group can come back after this check.
	maps.DeleteFunc(s.resets, func(ip string, bySentinel map[string]time.Time) bool {
		maps.DeleteFunc(bySentinel, func(_ string, at time.Time) bool { return now.Sub(at) >= sentinelPeerResetMemory })
		return addrs.Own[ip] || len(bySentinel) == 0
	})
}

// newPeers returns the IPs of the unknown peers that are alive and have a
// run ID that no Sentinel pod had at that IP. Without a known run ID, each
// alive peer counts.
func (s *sentinelUnknownAddrs) newPeers(addrs rfservice.SentinelAddrs) map[string]bool {
	ips := map[string]bool{}
	for _, group := range addrs.Groups {
		for _, peer := range group.Peers {
			if _, unknown := addrs.Peers[peer.IP]; unknown && alivePeer(peer) && !s.runIDs[peer.IP][peer.RunID] {
				ips[peer.IP] = true
			}
		}
	}
	return ips
}

// foreignPeers returns the alive unknown peers that are a Sentinel of another
// group, with the Sentinels that know them, and keeps them in s.foreign. Such
// a peer came back after a reset, or it has a run ID that the Sentinel pod at
// its IP did not have. After a reset, only a Sentinel that sends hellos with
// this master name comes back. A peer with a run ID of the pod at its IP is
// an own Sentinel that still runs after a forced deletion.
func (s *sentinelUnknownAddrs) foreignPeers(addrs rfservice.SentinelAddrs) map[string][]string {
	foreign := map[string][]string{}
	// A new Sentinel pod can get the IP of a deleted one and join the
	// Sentinels before its pod status has the IP.
	if addrs.PendingIP {
		return foreign
	}
	for sip, group := range addrs.Groups {
		for _, peer := range group.Peers {
			if _, unknown := addrs.Peers[peer.IP]; !unknown || !alivePeer(peer) || slices.Contains(foreign[peer.IP], sip) {
				continue
			}
			_, reset := s.resets[peer.IP][sip]
			runIDs := s.runIDs[peer.IP]
			if s.foreign[peer.IP][sip] || (reset && !runIDs[peer.RunID]) || (len(runIDs) > 0 && !runIDs[peer.RunID]) {
				foreign[peer.IP] = append(foreign[peer.IP], sip)
			}
		}
	}
	s.keepForeign(addrs, foreign)
	return foreign
}

// keepForeign stores foreign in s.foreign. It removes an entry when a pod has
// the peer IP, when the Sentinel has no pod, or when a check of that Sentinel
// does not find the peer alive. A Sentinel without an answer keeps its
// entries.
func (s *sentinelUnknownAddrs) keepForeign(addrs rfservice.SentinelAddrs, foreign map[string][]string) {
	for ip, bySentinel := range s.foreign {
		maps.DeleteFunc(bySentinel, func(sip string, _ bool) bool {
			if _, read := addrs.Groups[sip]; read {
				return !slices.Contains(foreign[ip], sip)
			}
			return !addrs.Own[sip]
		})
		if addrs.Own[ip] || len(bySentinel) == 0 {
			delete(s.foreign, ip)
		}
	}
	for ip, sentinels := range foreign {
		if s.foreign[ip] == nil {
			s.foreign[ip] = map[string]bool{}
		}
		for _, sip := range sentinels {
			s.foreign[ip][sip] = true
		}
	}
}

// checkSentinelMerge finds the addresses of another group in the Sentinels,
// and the peers that a reset removes. The operator sends no SENTINEL MONITOR
// or RESET to a merged group: the other group returns each Sentinel to its
// master in about 2 seconds, so each change only moves the master of the
// Sentinels again. A master of another group counts only together with an
// alive unknown peer. Without such a peer, a new monitor ends the state.
func (r *RedisFailoverHandler) checkSentinelMerge(logger log.Logger, rf *redisfailoverv1.RedisFailover, state *sentinelUnknownAddrs, addrs rfservice.SentinelAddrs, complete bool) {
	foreign := state.foreignPeers(addrs)
	masters := map[string][]string{}
	alive := false
	state.stale, state.unset = map[string][]string{}, map[string]bool{}
	for sip, group := range addrs.Groups {
		aliveHere, back := false, false
		for _, peer := range group.Peers {
			if _, unknown := addrs.Peers[peer.IP]; !unknown {
				continue
			}
			aliveHere = aliveHere || alivePeer(peer)
			_, reset := state.resets[peer.IP][sip]
			back = back || (reset && alivePeer(peer))
			if !slices.Contains(state.stale[sip], peer.IP) {
				state.stale[sip] = append(state.stale[sip], peer.IP)
			}
		}
		// A peer that came back after a reset comes back after each reset.
		// The Sentinel gets the next reset when that peer stops.
		if back {
			delete(state.stale, sip)
		}
		alive = alive || aliveHere
		if _, unknown := addrs.Masters[group.Master]; unknown && !group.MasterDown {
			masters[group.Master] = append(masters[group.Master], sip)
		}
		if !credibleMaster(group.Master) && !aliveHere {
			state.unset[sip] = true
		}
	}
	var found, ips []string
	for ip, sentinels := range foreign {
		found = append(found, "peer "+describeUnknownAddr(ip, sentinels))
		ips = append(ips, ip)
	}
	if alive {
		for ip, sentinels := range masters {
			found = append(found, "master "+describeUnknownAddr(ip, sentinels))
			ips = append(ips, ip)
		}
	}
	slices.Sort(found)
	slices.Sort(ips)
	ips = slices.Compact(ips)
	merged := strings.Join(found, ", ")
	switch {
	case merged != "" && state.merged == "":
		logger.Warningf(mergeLog.start, merged)
	case merged != "":
		logger.Debugf(mergeLog.last, merged)
	case state.merged != "" && !complete:
		// A Sentinel without an answer can still know the addresses.
		merged, ips = state.merged, state.mergedIPs
	case state.merged != "":
		logger.Infof(mergeLog.end, state.merged)
	}
	state.merged, state.mergedIPs = merged, ips
	setMergedStatus(rf, ips)
}

// mergeLog holds the log messages of a merge with another group.
var mergeLog = unknownAddrLog{
	start: "Sentinels of this RedisFailover know addresses of another group: %s. The operator sends no SENTINEL MONITOR, REMOVE or RESET to the Sentinels until these addresses are gone, because the other group undoes each change in seconds",
	last:  "Sentinels still know addresses of another group: %s",
	end:   "The addresses of another group are gone from the Sentinels: %s. The operator heals the Sentinels again",
}

// setMergedStatus sets the status while the Sentinels know the addresses ips
// of another group.
func setMergedStatus(rf *redisfailoverv1.RedisFailover, ips []string) {
	if len(ips) == 0 {
		return
	}
	rf.Status = redisfailoverv1.RedisFailoverStatus{
		State:   redisfailoverv1.NotHealthyState,
		Message: fmt.Sprintf("Sentinels know the addresses %s of another group, so the operator sends them no SENTINEL MONITOR or RESET", strings.Join(ips, ", ")),
	}
}

// loadSentinelUnknown returns the state of key, or an empty state.
func (r *RedisFailoverHandler) loadSentinelUnknown(key string) sentinelUnknownAddrs {
	v, _ := r.sentinelUnknown.Load(key)
	if state, ok := v.(sentinelUnknownAddrs); ok {
		return state
	}
	return sentinelUnknownAddrs{
		peers: map[string]unknownAddr{}, masters: map[string]unknownAddr{}, own: map[string]time.Time{},
		runIDs: map[string]map[string]bool{}, resets: map[string]map[string]time.Time{}, foreign: map[string]map[string]bool{},
	}
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
