package emulator

import (
	"context"
	"log/slog"
	"math"
	"time"
)

// RTR is the return-to-reference controller. It drives a Seeder's progressive
// convergence: on each tick it computes a convergence fraction from an
// exponential decay over the horizon tau and calls Seeder.Converge. After a
// user trade perturbs the engine book, the deviation from the reference decays
// with time-constant tau (≈63% closed per tau, ≈95% per 3·tau), so liquidity
// is restored gradually rather than snapping back instantly.
//
// Convergence is a pure function of the elapsed time and tau, so a fixed tick
// schedule (or a deterministic replay clock) yields identical, reproducible
// book trajectories.
type RTR struct {
	seeder *Seeder
	tau    time.Duration
}

// NewRTR returns a controller converging seeder toward its reference with
// horizon tau. A tau <= 0 means snap immediately (alpha=1 every tick).
func NewRTR(seeder *Seeder, tau time.Duration) *RTR {
	return &RTR{seeder: seeder, tau: tau}
}

// Alpha returns the fraction of the remaining deviation to close for an
// elapsed interval dt, given the horizon tau: 1 - exp(-dt/tau). It is exported
// so tests (and replay) can step convergence deterministically without a
// wall clock.
func (r *RTR) Alpha(dt time.Duration) float64 {
	if r.tau <= 0 || dt <= 0 {
		return 1
	}
	a := 1 - math.Exp(-dt.Seconds()/r.tau.Seconds())
	if a > 1 {
		return 1
	}
	return a
}

// Step performs one convergence step for an elapsed interval dt.
func (r *RTR) Step(ctx context.Context, dt time.Duration) (Stats, error) {
	return r.seeder.Converge(ctx, r.Alpha(dt))
}

// Run converges on every tick until ctx is cancelled. Convergence errors are
// logged, not fatal, so a transient engine error doesn't tear down the loop.
func (r *RTR) Run(ctx context.Context, tick time.Duration) error {
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			if _, err := r.Step(ctx, tick); err != nil {
				slog.Warn("rtr step error", "instrument", r.seeder.cfg.Instrument, "error", err)
			}
		}
	}
}
