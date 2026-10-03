// Package global coordinates the mutations and chaos actions that affect
// every instance.
package global

import (
	"sync"
	"sync/atomic"
)

// Lock lets every instance's mutations run side by side, and a mutation
// or chaos action that affects every instance run alone: it waits until
// the running mutations are done, and holds off new ones until it is.
type Lock struct {
	mu          sync.RWMutex
	disturbance atomic.Pointer[string]
	chaos       atomic.Pointer[string]
}

// Shared is held around every other mutation.
func (l *Lock) Shared() (unlock func()) {
	l.mu.RLock()
	return l.mu.RUnlock
}

// Exclusive is held around a mutation or chaos action that affects every
// instance.
func (l *Lock) Exclusive() (unlock func()) {
	l.mu.Lock()
	return l.mu.Unlock
}

// Disturb marks all instances as disturbed for reason, for example a stopped
// operator. "" ends the disturbance.
func (l *Lock) Disturb(reason string) {
	l.disturbance.Store(&reason)
}

// Disturbance returns why all instances are disturbed, or "". All instances
// are in a convergence window while they are disturbed.
func (l *Lock) Disturbance() string {
	if l == nil {
		return ""
	}
	if r := l.disturbance.Load(); r != nil {
		return *r
	}
	return ""
}

// SetChaos marks a chaos action of kind running, or none for "".
func (l *Lock) SetChaos(kind string) {
	l.chaos.Store(&kind)
}

// Chaos returns the kind of the chaos action running, "" if none is.
func (l *Lock) Chaos() string {
	if l == nil {
		return ""
	}
	if k := l.chaos.Load(); k != nil {
		return *k
	}
	return ""
}
