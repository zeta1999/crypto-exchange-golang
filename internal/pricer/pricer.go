// Package pricer is the plugin seam for flow-exotics valuation (PLAN §
// "options: exotics as plugins"). Everything below the valuation layer keys off
// this interface — the valuation layer never names a concrete engine, so a
// new back-end (in-process Go MC, CUDA, another vendor binary, a remote
// pricing service) is one adapter plus registration.
//
// It mirrors the feed.Source pattern: small interface, deterministic inputs,
// recorded golden outputs, so plugin behavior is pinned by fixtures exactly the
// way the emulator's scenario golden test is.
package pricer

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
)

// Params is the venue- and product-agnostic market state for one pricing
// request. Backends use what they need; the rest is ignored (a vanilla
// BS pricer only reads Underlying/Side/Strike/Expiry; a Monte-Carlo exotic
// reads Model, NumPaths, Seed, and the payoff Script/ScriptFile).
type Params struct {
	// Instrument identity.
	Symbol string

	// Market state.
	Spot         float64 // underlying spot ("S" in .aria scripts is set from this)
	Forward      float64 // optional; 0 = derive from Spot
	Vol          float64 // volatility (annualized, e.g. 0.25)
	Rate         float64 // risk-free (annualized, e.g. 0.05)
	ExpiryYears  float64 // T in years
	ExpiryMillis int64   // settlement instant, epoch ms (EAPI convention)

	// Vanilla contract terms. For exotics these are usually informational.
	Strike float64
	Side   string // "C" or "P"

	// Monte-Carlo / engine knobs.
	NumPaths int
	NumSteps int
	Seed     int64
	RNG      string // "portable" = bit-reproducible across OS/compilers
	Model    string // "bs" (default) | "dupire" | "hybrid_lv" (AriaMCPriceLV)

	// Payoff. Exactly one of Script / ScriptFile should be set for exotic
	// payoffs. ScriptFile is resolved relative to the backend's workdir.
	Script     string
	ScriptFile string

	// Raw carries engine-specific extra arguments (e.g. a Dupire
	// "surface" map for local-vol models) merged into the request.
	Raw map[string]any

	// Endpoint selects the engine command (for multi-endpoint backends).
	// Empty = backend default.
	Endpoint string
}

// Result is one pricing output.
type Result struct {
	Price     float64
	NumPaths  int
	NumSteps  int
	RNG       string
	ElapsedMS float64
	// Raw is the backend's full response object (for debugging, logging, and
	// fixture capture).
	Raw map[string]any
}

// ErrNoPrice is returned when a backend succeeded but its output carried no
// finite price (bad endpoint, malformed input the engine accepted silently).
var ErrNoPrice = errors.New("pricer: no finite price in backend output")

// Pricer is the plugin interface for flow-exotics valuation.
//
// Implementations must be safe for concurrent use. Deterministic backends
// (same Params ⇒ same Price bit-for-bit) are preferred: they are unit- and
// golden-testable. Non-deterministic backends must document their variance.
type Pricer interface {
	// Name identifies the backend ("saga", "mc-go", ...).
	Name() string
	// Price values one instrument under Params.
	Price(ctx context.Context, p Params) (Result, error)
}

// Registry hands out priced backends by name so callers (API edges, the
// emulator's dealer-quoting mode, FIX quote responders) depend only on
// (name, Params) and never on a concrete engine. Safe for concurrent use:
// it is fine to Register while responders are calling Get/Names (e.g. a
// config reload), though the common pattern is register-once, then frozen.
type Registry struct {
	mu sync.RWMutex

	pricers map[string]Pricer
}

func NewRegistry() *Registry { return &Registry{pricers: map[string]Pricer{}} }

// Register adds or replaces a backend. Names are compared case-insensitively.
func (r *Registry) Register(p Pricer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pricers[strings.ToLower(p.Name())] = p
}

// Get returns the named backend.
func (r *Registry) Get(name string) (Pricer, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.pricers[strings.ToLower(name)]
	return p, ok
}

// Names lists registered backends (sorted).
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.pricers))
	for n := range r.pricers {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
