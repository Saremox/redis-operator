// Package data fills an instance with known data, keeps a ledger of
// acknowledged writes, and verifies after every failover and mutation that
// the acknowledged writes survived.
package data

import (
	"hash/fnv"
	"strconv"
)

// FillKey returns the name of fill key n of the instance rf.
func FillKey(rf string, n int64) string { return key(rf, "fill", n) }

// LedgerKey returns the name of ledger key n of the instance rf.
func LedgerKey(rf string, n int64) string { return key(rf, "ledger", n) }

// BurstKey returns the name of burst key n of the instance rf.
func BurstKey(rf string, n int64) string { return key(rf, "burst", n) }

func key(rf, kind string, n int64) string {
	return "soak:" + rf + ":" + kind + ":" + strconv.FormatInt(n, 10)
}

const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_"

// Value is the value of key: size bytes derived from the key alone, so any
// key can be verified without keeping its value.
func Value(key string, size int) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(key))
	x := h.Sum64() | 1
	b := make([]byte, size)
	for i := range b {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		b[i] = alphabet[x&63]
	}
	return string(b)
}
