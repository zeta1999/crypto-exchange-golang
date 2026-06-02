package toxicity

import "sync"

// Config tunes the toxicity model. Scale is the global gate (0 = off); the
// weights independently scale the VPIN-driven fill probability and the
// lambda-driven price penetration. Seed makes the injector reproducible (the
// RNG lives in the injector, not here).
type Config struct {
	Scale        float64 // global multiplier; 0 disables toxicity (pure RTR)
	KyleWeight   float64 // scales lambda → price-penetration impact
	VPINWeight   float64 // scales VPIN → adverse-fill probability
	WindowTrades int     // rolling window for the lambda regression
	BucketVolume float64 // VPIN bucket size (base-asset volume)
	Buckets      int     // number of VPIN buckets to average
	Seed         int64   // RNG seed for the adverse-selection injector
}

func (c Config) withDefaults() Config {
	if c.KyleWeight == 0 {
		c.KyleWeight = 1
	}
	if c.VPINWeight == 0 {
		c.VPINWeight = 1
	}
	if c.WindowTrades <= 0 {
		c.WindowTrades = 500
	}
	if c.BucketVolume <= 0 {
		c.BucketVolume = 1
	}
	if c.Buckets <= 0 {
		c.Buckets = 50
	}
	// Negative weights are meaningless (they'd invert the model); clamp to 0
	// to disable that component rather than silently inverting it.
	if c.KyleWeight < 0 {
		c.KyleWeight = 0
	}
	if c.VPINWeight < 0 {
		c.VPINWeight = 0
	}
	return c
}

// Model maintains the rolling Kyle/VPIN estimators from the trade tape. It is
// safe for concurrent Observe/read.
type Model struct {
	mu  sync.Mutex
	cfg Config
	k   *kyle
	v   *vpin
}

// New builds a Model from cfg (zero-valued fields take sensible defaults).
func New(cfg Config) *Model {
	cfg = cfg.withDefaults()
	return &Model{
		cfg: cfg,
		k:   newKyle(cfg.WindowTrades),
		v:   newVPIN(cfg.BucketVolume, cfg.Buckets),
	}
}

// Config returns the effective (defaulted) configuration.
func (m *Model) Config() Config { return m.cfg }

// Observe folds one tape trade into the estimators.
func (m *Model) Observe(price, qty float64, buy bool) {
	sv := qty
	if !buy {
		sv = -qty
	}
	m.mu.Lock()
	m.k.observe(price, sv)
	m.v.observe(qty, buy)
	m.mu.Unlock()
}

// Lambda is the current Kyle impact coefficient (price per unit volume, ≥0).
func (m *Model) Lambda() float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.k.lambda()
}

// VPIN is the current volume-synchronized informed-trading proxy in [0,1].
func (m *Model) VPIN() float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.v.value()
}

// Score is the adverse-fill probability driver in [0,1]: VPINWeight·VPIN
// clamped. The injector multiplies it by Scale to get a per-trade probability.
func (m *Model) Score() float64 {
	s := m.cfg.VPINWeight * m.VPIN()
	if s < 0 {
		return 0
	}
	if s > 1 {
		return 1
	}
	return s
}

// Impact is the price-penetration magnitude (in price units): KyleWeight·lambda.
// The injector multiplies it by Scale to offset the adverse sweep beyond the
// touch.
func (m *Model) Impact() float64 { return m.cfg.KyleWeight * m.Lambda() }
