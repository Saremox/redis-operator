package observer

import (
	"strings"
	"time"
)

// The invariants of maxmemory and the used memory. Both are judged apart from
// the others: each event is a finding, also inside a window.
const (
	// invNoevictionFull is a master that refuses writes under noeviction,
	// because its data reached maxmemory, for longer than
	// observer.noevictionFullLimit. The operator keeps maxmemory under a
	// policy that is not allkeys-*, so nothing frees memory, and the fill and
	// the ledger stop.
	invNoevictionFull = "noeviction_full"
	// invMaxmemoryBelowUsed is a maxmemory that decreased below the memory
	// that the pod uses, under a policy that is not allkeys-*. The operator
	// keeps maxmemory in that case.
	invMaxmemoryBelowUsed = "maxmemory_below_used"
	// minFullMargin and the margin of 1 percent of maxmemory cover the change
	// of the used memory between the refused write and the INFO sample.
	minFullMargin = 1 << 20
)

// counted returns the memory that Redis compares with maxmemory: used_memory
// without mem_not_counted_for_evict, the replication and AOF buffers.
func (i info) counted() int64 {
	return max(0, i.int("used_memory")-i.int("mem_not_counted_for_evict"))
}

func fullMargin(maxmemory int64) int64 {
	return max(minFullMargin, maxmemory/100)
}

// full reports whether a server refuses writes by design: its policy is
// noeviction, and its counted memory is at maxmemory, less the margin.
func (i info) full() bool {
	mm := i.int("maxmemory")
	return i["maxmemory_policy"] == "noeviction" && mm > 0 && i.counted() >= mm-fullMargin(mm)
}

// NoevictionFull reports whether, at the last round, the master refused
// writes by design, for at most observer.noevictionFullLimit. A failed write
// with OOM is then expected.
func (o *Observer) NoevictionFull() bool {
	v := o.view.Load()
	return v != nil && v.noevictionFull
}

// judgeNoeviction follows the time that the master is full under noeviction,
// and returns whether this is still an expected state. Only a master that is
// not full ends the state. A round without a single master, for example after
// an INFO timeout, tells nothing about the memory: it keeps the time, so that
// short gaps cannot start the limit again.
func (o *Observer) judgeNoeviction(now time.Time, s snapshot) bool {
	master, err := s.master()
	switch {
	case err != nil && o.fullSince.IsZero():
		return false
	case err != nil:
	case !master.info.full():
		if !o.fullSince.IsZero() {
			o.log.Info("noeviction full ended", "duration_seconds", now.Sub(o.fullSince).Seconds())
		}
		o.fullSince, o.fullFound = time.Time{}, false
		return false
	case o.fullSince.IsZero():
		o.fullSince = now
		o.log.Info("noeviction full: the master refuses writes by design", "pod", master.Name,
			"used_memory", master.info.counted(), "maxmemory", master.info.int("maxmemory"))
	}
	if now.Sub(o.fullSince) < o.fullLimit {
		return true
	}
	if !o.fullFound {
		o.fullFound = true
		o.findings.WithLabelValues(invNoevictionFull).Inc()
		o.log.Warn("noeviction full for longer than the limit", "invariant", invNoevictionFull, "finding", true,
			"since", o.fullSince.UTC().Format(time.RFC3339), "limit_seconds", o.fullLimit.Seconds())
	}
	return false
}

// judgeMaxmemory finds a maxmemory that decreased below the used memory of
// the pod under a policy that is not allkeys-*. A pod without INFO in this
// round keeps its last maxmemory.
func (o *Observer) judgeMaxmemory(s snapshot) {
	last := o.maxmemory
	o.maxmemory = map[string]int64{}
	for _, p := range s.redis {
		prev, seen := last[p.UID]
		if p.info == nil {
			if seen {
				o.maxmemory[p.UID] = prev
			}
			continue
		}
		mm := p.info.int("maxmemory")
		o.maxmemory[p.UID] = mm
		lowered := seen && mm > 0 && (prev == 0 || mm < prev)
		if lowered && !strings.HasPrefix(p.info["maxmemory_policy"], "allkeys-") && p.info.counted() > mm+fullMargin(mm) {
			o.findings.WithLabelValues(invMaxmemoryBelowUsed).Inc()
			o.log.Warn("maxmemory lowered below the used memory", "invariant", invMaxmemoryBelowUsed, "finding", true,
				"pod", p.Name, "maxmemory", mm, "previous_maxmemory", prev, "used_memory", p.info.counted(),
				"policy", p.info["maxmemory_policy"])
		}
	}
}
