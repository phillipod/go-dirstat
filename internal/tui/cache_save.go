package tui

import (
	"sync"
	"sync/atomic"
)

// cacheSaveCoordinator orders cache writes without blocking the Bubble Tea
// update loop. A scan is marked successful as soon as its completion is
// accepted; background save commands then serialize here and discard any
// generation superseded by a newer successful scan.
//
// Merely starting, canceling, or failing a newer scan does not advance the
// successful generation, so a pending save of the last good result remains
// eligible.
type cacheSaveCoordinator struct {
	latestSuccessful atomic.Uint64
	saveMu           sync.Mutex
}

func (c *cacheSaveCoordinator) markSuccessful(generation uint64) {
	for {
		latest := c.latestSuccessful.Load()
		if generation <= latest {
			return
		}
		if c.latestSuccessful.CompareAndSwap(latest, generation) {
			return
		}
	}
}

// save runs save only when generation is still the newest successfully
// completed scan. Calls are serialized so an older write can never finish
// after a newer write and replace it.
func (c *cacheSaveCoordinator) save(generation uint64, save func() error) (bool, error) {
	c.saveMu.Lock()
	defer c.saveMu.Unlock()

	if generation < c.latestSuccessful.Load() {
		return false, nil
	}
	return true, save()
}
