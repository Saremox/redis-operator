package data

import (
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
)

// ranges is a set of numbers held as sorted, disjoint spans, which stays
// small while the numbers are mostly consecutive.
type ranges struct {
	spans []span
}

// span is the numbers from lo up to, not including, hi.
type span struct{ lo, hi int64 }

func (r *ranges) add(n int64) {
	if k := len(r.spans); k > 0 {
		last := &r.spans[k-1]
		switch {
		case n == last.hi:
			last.hi++
			return
		case n > last.hi:
			r.spans = append(r.spans, span{n, n + 1})
			return
		}
	} else {
		r.spans = append(r.spans, span{n, n + 1})
		return
	}
	i, _ := slices.BinarySearchFunc(r.spans, n, func(s span, n int64) int {
		switch {
		case s.hi <= n:
			return -1
		case s.lo > n:
			return 1
		}
		return 0
	})
	if i < len(r.spans) && r.spans[i].lo <= n {
		return
	}
	r.spans = slices.Insert(r.spans, i, span{n, n + 1})
	r.merge()
}

func (r *ranges) remove(n int64) {
	for i, s := range r.spans {
		if n < s.lo || n >= s.hi {
			continue
		}
		switch {
		case s.hi-s.lo == 1:
			r.spans = slices.Delete(r.spans, i, i+1)
		case n == s.lo:
			r.spans[i].lo++
		case n == s.hi-1:
			r.spans[i].hi--
		default:
			r.spans = slices.Insert(r.spans, i+1, span{n + 1, s.hi})
			r.spans[i].hi = n
		}
		return
	}
}

func (r *ranges) merge() {
	out := r.spans[:0]
	for _, s := range r.spans {
		if k := len(out); k > 0 && out[k-1].hi >= s.lo {
			out[k-1].hi = max(out[k-1].hi, s.hi)
			continue
		}
		out = append(out, s)
	}
	r.spans = out
}

// within returns the numbers from lo up to hi as spans.
func (r *ranges) within(lo, hi int64) []span {
	var out []span
	for _, s := range r.spans {
		s.lo, s.hi = max(s.lo, lo), min(s.hi, hi)
		if s.lo < s.hi {
			out = append(out, s)
		}
	}
	return out
}

// dropBelow removes every number below n.
func (r *ranges) dropBelow(n int64) {
	r.spans = (&ranges{spans: r.spans}).within(n, 1<<62)
}

func count(spans []span) int64 {
	var c int64
	for _, s := range spans {
		c += s.hi - s.lo
	}
	return c
}

func members(spans []span) []int64 {
	var out []int64
	for _, s := range spans {
		for n := s.lo; n < s.hi; n++ {
			out = append(out, n)
		}
	}
	return out
}

// sample returns up to k distinct numbers of spans at random.
func sample(rnd *rand.Rand, spans []span, k int) []int64 {
	total := count(spans)
	if int64(k) >= total {
		return members(spans)
	}
	picked := map[int64]bool{}
	for len(picked) < k {
		i := rnd.Int64N(total)
		for _, s := range spans {
			if i < s.hi-s.lo {
				picked[s.lo+i] = true
				break
			}
			i -= s.hi - s.lo
		}
	}
	out := make([]int64, 0, k)
	for n := range picked {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// format writes spans like 1-4,7, with the last number of each span.
func format(spans []span) string {
	var b strings.Builder
	for i, s := range spans {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatInt(s.lo, 10))
		if s.hi-s.lo > 1 {
			b.WriteByte('-')
			b.WriteString(strconv.FormatInt(s.hi-1, 10))
		}
	}
	return b.String()
}

// spansOf collects sorted numbers into spans.
func spansOf(ns []int64) []span {
	var r ranges
	for _, n := range ns {
		r.add(n)
	}
	return r.spans
}
