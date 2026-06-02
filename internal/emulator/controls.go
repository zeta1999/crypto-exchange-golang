package emulator

import (
	"sync/atomic"
)

// Controls is the runtime-mutable holder for the Phase 7 fault injectors. The
// dispatcher (and the per-instrument workers) read it on EVERY feed event,
// while a scenario runner mutates it on a timeline. Reads must therefore be
// cheap and never block the hot path; writes are rare (one per scenario event).
//
// Sync mechanism: both fields are atomic.Pointer, so a reader does a single
// lock-free atomic load and a writer a single atomic store. No mutex is taken
// on the dispatcher hot path, and the scenario writer can never block a reader
// (or vice-versa). PriceShift is a small value type, so we box it behind a
// pointer to swap it atomically; Latency is already a pointer (it owns a
// mutex-guarded RNG) so it is swapped directly.
//
// The zero value is NOT ready to use — construct via NewControls so the
// pointers are non-nil and the behavior matches a no-scenario startup.
type Controls struct {
	shift atomic.Pointer[PriceShift]
	lat   atomic.Pointer[Latency]
}

// NewControls seeds a Controls from the static config values captured at
// startup. With no scenario configured these never change, so behavior is
// identical to the pre-scenario emulator. lat may be nil-config but must be a
// real *Latency (its Sleep/Delay methods tolerate a zero config as no-ops).
func NewControls(shift PriceShift, lat *Latency) *Controls {
	c := &Controls{}
	c.shift.Store(&shift)
	c.lat.Store(lat)
	return c
}

// PriceShift returns the current price shift by a single lock-free atomic load.
// Safe to call concurrently with SetPriceShift from the dispatcher hot path.
func (c *Controls) PriceShift() PriceShift {
	if p := c.shift.Load(); p != nil {
		return *p
	}
	return PriceShift{}
}

// SetPriceShift atomically swaps in a new price shift. Called by the scenario
// runner; never blocks a concurrent reader.
func (c *Controls) SetPriceShift(p PriceShift) {
	c.shift.Store(&p)
}

// Latency returns the current latency injector by a single lock-free atomic
// load. The returned *Latency is itself concurrency-safe (its jitter RNG is
// mutex-guarded).
func (c *Controls) Latency() *Latency {
	return c.lat.Load()
}

// SetLatency atomically swaps in a new latency injector. The old injector is
// dropped; any in-flight Sleep on it completes normally (it captured its own
// duration), so a swap mid-sleep is benign.
func (c *Controls) SetLatency(l *Latency) {
	c.lat.Store(l)
}
