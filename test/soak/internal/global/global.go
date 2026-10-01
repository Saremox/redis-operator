// Package global coordinates the mutations that affect every instance.
package global

import (
	"sync"
	"sync/atomic"
)

// Lock lets every instance's mutations run side by side, and a mutation
// that affects every instance run alone: it waits until the running
// mutations are done, and holds off new ones until it is.
type Lock struct {
	mu           sync.RWMutex
	operatorDown atomic.Bool
}

// Shared is held around every other mutation.
func (l *Lock) Shared() (unlock func()) {
	l.mu.RLock()
	return l.mu.RUnlock
}

// Exclusive is held around a mutation that affects every instance.
func (l *Lock) Exclusive() (unlock func()) {
	l.mu.Lock()
	return l.mu.Unlock
}

// SetOperatorDown marks the operator stopped by a mutation, or back.
func (l *Lock) SetOperatorDown(down bool) {
	l.operatorDown.Store(down)
}

// OperatorDown reports whether a mutation stopped the operator: every
// instance is then in a convergence window.
func (l *Lock) OperatorDown() bool {
	return l != nil && l.operatorDown.Load()
}
