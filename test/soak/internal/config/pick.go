package config

import (
	"cmp"
	"hash/fnv"
	"maps"
	"math/rand/v2"
	"slices"
)

// StepRand returns the random source of one step of a lane: an instance's
// mutator, by the instance name, or the chaos lane. Each lane and step gets
// its own source, so that a logged step can be replayed alone.
func StepRand(seed int64, lane string, step int) *rand.Rand {
	h := fnv.New64a()
	_, _ = h.Write([]byte(lane))
	return rand.New(rand.NewPCG(uint64(seed)^h.Sum64(), uint64(step)))
}

// Pick picks a key by its weight.
func Pick[K cmp.Ordered](r *rand.Rand, weights map[K]int) K {
	keys := slices.Sorted(maps.Keys(weights))
	total := 0
	for _, k := range keys {
		total += weights[k]
	}
	n := r.IntN(total)
	for _, k := range keys {
		if n < weights[k] {
			return k
		}
		n -= weights[k]
	}
	panic("unreachable")
}
