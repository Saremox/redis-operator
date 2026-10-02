package data

import (
	"math/rand/v2"
	"sync"
)

// ledger records the sequence numbers of acknowledged writes. Every write
// acknowledged since the previous verification is checked at the next one,
// and a sample of those the previous verification checked. Older ones are
// aged out: their keys are deleted and they are forgotten.
type ledger struct {
	mu sync.Mutex
	// next is the next sequence number to write, inflight the one being
	// written or -1.
	next, inflight int64
	acked          ranges
	// verified: every acknowledged write below was checked; aged: every
	// number below is forgotten and its key deleted.
	verified, aged int64
}

func newLedger() *ledger {
	return &ledger{inflight: -1}
}

// begin returns the next sequence number to write.
func (l *ledger) begin() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.inflight = l.next
	l.next++
	return l.inflight
}

func (l *ledger) end(seq int64, acked bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if acked {
		l.acked.add(seq)
	}
	l.inflight = -1
}

// round is one verification's share of the ledger.
type round struct {
	// recent are every write acknowledged since the previous verification,
	// below to.
	recent []span
	// older is a sample of olderAll, those the previous verification
	// checked.
	older    []int64
	olderAll []span
	to       int64
}

func (l *ledger) plan(rnd *rand.Rand, k int) round {
	l.mu.Lock()
	defer l.mu.Unlock()
	to := l.next
	if l.inflight >= 0 {
		to = l.inflight
	}
	older := l.acked.within(l.aged, l.verified)
	return round{
		recent:   l.acked.within(l.verified, to),
		older:    sample(rnd, older, k),
		olderAll: older,
		to:       to,
	}
}

// commit forgets lost writes, so each is counted once, and marks the round
// verified. It returns the numbers to age out, whose keys the caller
// deletes before calling aged.
func (l *ledger) commit(r round, lost []int64) span {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, n := range lost {
		l.acked.remove(n)
	}
	old := span{l.aged, l.verified}
	l.verified = max(l.verified, r.to)
	return old
}

func (l *ledger) agedOut(s span) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.aged = max(l.aged, s.hi)
	l.acked.dropBelow(l.aged)
}

// forget forgets every write so far, as if verified and aged out.
func (l *ledger) forget() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.verified, l.aged = l.next, l.next
	l.acked.dropBelow(l.next)
}

// recentSample returns up to k of the writes acknowledged since the last
// verification. Their keys are deleted only after two more verifications.
func (l *ledger) recentSample(rnd *rand.Rand, k int) []int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	to := l.next
	if l.inflight >= 0 {
		to = l.inflight
	}
	return sample(rnd, l.acked.within(l.verified, to), k)
}
