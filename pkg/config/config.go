package config

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Instrument describes a tradable symbol supported by the exchange.
type Instrument struct {
	Symbol string `yaml:"symbol"`
	Base   string `yaml:"base"`
	Quote  string `yaml:"quote"`
}

// Network defines bind addresses and secrets for public APIs.
type Network struct {
	ListenGRPC  string `yaml:"listen_grpc"`
	ListenHTTP  string `yaml:"listen_http"`
	ListenWS    string `yaml:"listen_ws"`
	TokenSecret string `yaml:"token_secret"`
	TLS         TLS    `yaml:"tls"`
}

// TLS describes certificate locations for HTTPS/WSS endpoints.
type TLS struct {
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

// Limits contains global sanity checks for the exchange.
type Limits struct {
	MaxOpenOrders int     `yaml:"max_open_orders"`
	MinTickSize   float64 `yaml:"min_tick_size"`
}

// Config captures the full runtime configuration from YAML.
type Database struct {
	DSN string `yaml:"dsn"`
}

type Config struct {
	Network     Network      `yaml:"network"`
	Database    Database     `yaml:"database"`
	Limits      Limits       `yaml:"limits"`
	Instruments []Instrument `yaml:"instruments"`
	Storage     Storage      `yaml:"storage"`
	Emulator    Emulator     `yaml:"emulator"`
	API         APIConfig    `yaml:"api"`
	Metrics     Metrics      `yaml:"metrics"`
}

// Metrics configures the Prometheus-text metrics endpoint. When Enabled, a
// dedicated unauthenticated HTTP listener serves /metrics on Listen (public, as
// is conventional for a scrape endpoint). Disabled by default (non-breaking).
type Metrics struct {
	Enabled bool   `yaml:"enabled"`
	Listen  string `yaml:"listen"`
}

// APIConfig groups optional third-party-compatible API edges.
type APIConfig struct {
	Binance  BinanceConfig  `yaml:"binance"`
	Coinbase CoinbaseConfig `yaml:"coinbase"`
}

// BinanceConfig configures the Binance-spot-compatible REST edge (PLAN Phase
// 8, a documented SUBSET). Disabled by default. APIKey/Secret are the
// credentials a stock Binance client must present (HMAC-SHA256 signing).
// Symbols maps Binance concatenated symbols to engine instruments.
type BinanceConfig struct {
	Enabled    bool         `yaml:"enabled"`
	Listen     string       `yaml:"listen"`
	APIKey     string       `yaml:"api_key"`
	Secret     string       `yaml:"secret"`
	Symbols    []SymbolPair `yaml:"symbols"`
	RatePerSec float64      `yaml:"rate_per_sec"` // token-bucket refill; <=0 disables
	Burst      int          `yaml:"burst"`        // bucket capacity
}

// SymbolPair maps a Binance symbol ("BTCUSDT") to an engine instrument
// ("BTC-USD").
type SymbolPair struct {
	Binance string `yaml:"binance"`
	Engine  string `yaml:"engine"`
}

// CoinbaseConfig configures the Coinbase-Advanced-Trade-compatible REST edge
// (PLAN Phase 9, a documented SUBSET). Disabled by default. APIKey/Secret/
// Passphrase are the legacy Coinbase Exchange HMAC-SHA256 credentials.
// Additionally, when JWTPublicKey (a PEM-encoded EC P-256 public key) is set,
// the edge accepts the production Advanced Trade ES256 JWT auth: a Bearer token
// (REST) or the subscribe `jwt` field (WS) is verified against that key.
// JWTKeyName, if set, is the expected JWT sub/kid. Products is the allow-list
// of product IDs; Coinbase product IDs ("BTC-USD") equal engine instruments.
type CoinbaseConfig struct {
	Enabled      bool     `yaml:"enabled"`
	Listen       string   `yaml:"listen"`
	APIKey       string   `yaml:"api_key"`
	Secret       string   `yaml:"secret"`
	Passphrase   string   `yaml:"passphrase"`
	JWTPublicKey string   `yaml:"jwt_public_key"` // PEM EC P-256 public key; enables ES256 JWT auth
	JWTKeyName   string   `yaml:"jwt_key_name"`   // expected JWT sub/kid ("" = don't check)
	Products     []string `yaml:"products"`
	RatePerSec   float64  `yaml:"rate_per_sec"` // token-bucket refill; <=0 disables
	Burst        int      `yaml:"burst"`        // bucket capacity
}

// Emulator configures live-venue mirroring (feed → reference book → seeded
// synthetic liquidity in the engine, with return-to-reference). When
// disabled, the exchange runs as a plain matching engine. See PLAN.md.
type Emulator struct {
	Enabled     bool               `yaml:"enabled"`
	Venue       string             `yaml:"venue"` // "coinbase" | "binance" | "replay"
	Instruments []string           `yaml:"instruments"`
	Reference   EmulatorReference  `yaml:"reference"`
	RTR         EmulatorRTR        `yaml:"rtr"`
	Toxicity    EmulatorToxicity   `yaml:"toxicity"`
	PriceShift  EmulatorPriceShift `yaml:"price_shift"`
	Latency     EmulatorLatency    `yaml:"latency"`
	Scenario    EmulatorScenario   `yaml:"scenario"`
	Replay      EmulatorReplay     `yaml:"replay"`
}

// EmulatorReplay configures the replay venue: feed the whole emulator from a
// recorded JSONL trace (cmd/feedcat -record output) instead of a live venue,
// deterministically and offline. Used when Venue == "replay".
type EmulatorReplay struct {
	File  string  `yaml:"file"`  // recorded trace path (required for venue=replay)
	Speed float64 `yaml:"speed"` // playback multiplier: <=0 = as-fast-as-possible (deterministic); 1.0 = real time; 10.0 = 10x
}

// EmulatorScenario configures the scripted test-bed timeline (PLAN §5 Phase 7).
// When File is empty (the dev default) no runner starts and the controls stay
// at the static price_shift/latency values above. File is a JSONL scenario
// (one timed action per line) that mutates the fault injectors on cue. Speed
// scales the schedule: 1.0 is real time, >1 accelerates (e.g. 10 runs a 10s
// timeline in 1s); <= 0 is treated as 1.
type EmulatorScenario struct {
	File  string  `yaml:"file"`
	Speed float64 `yaml:"speed"`
}

// EmulatorPriceShift configures the artificial price shift (PLAN §5 Phase 7):
// shifted = price * scale * (1 + offset_bps/10000). The zero value (offset_bps
// 0, scale 0 → treated as 1) is the identity (no shift), so the dev default is
// a no-op. Used to drive two emulated venues apart and manufacture a
// closeable cross-venue arbitrage.
type EmulatorPriceShift struct {
	OffsetBps float64 `yaml:"offset_bps"` // additive shift in basis points
	Scale     float64 `yaml:"scale"`      // multiplicative shift (0 or 1 = none)
}

// EmulatorLatency configures artificial latency injection (PLAN §5 Phase 7).
// All zero (the dev default) means no added delay anywhere. feed_to_book_ms
// delays the reference goroutine (a slow feed); order_ack_ms / fill_report_ms
// apply at the API edges (Phase 8/9). jitter_ms adds a uniform random
// [0, jitter) on top of each base delay.
type EmulatorLatency struct {
	FeedToBookMs int `yaml:"feed_to_book_ms"`
	OrderAckMs   int `yaml:"order_ack_ms"`
	FillReportMs int `yaml:"fill_report_ms"`
	JitterMs     int `yaml:"jitter_ms"`
}

// EmulatorToxicity configures the adverse-selection model (PLAN [b]). Scale=0
// disables it (pure return-to-reference).
type EmulatorToxicity struct {
	Scale        float64 `yaml:"scale"`
	KyleWeight   float64 `yaml:"kyle_weight"`
	VPINWeight   float64 `yaml:"vpin_weight"`
	WindowTrades int     `yaml:"window_trades"`
	BucketVolume float64 `yaml:"bucket_volume"`
	Buckets      int     `yaml:"buckets"`
	Seed         int64   `yaml:"seed"`
}

// EmulatorReference controls how the reference book is mirrored into the engine.
type EmulatorReference struct {
	DepthLevels int `yaml:"depth_levels"` // levels per side to mirror; 0 = all
	RefreshMs   int `yaml:"refresh_ms"`   // reconcile/convergence cadence
}

// EmulatorRTR controls return-to-reference convergence.
type EmulatorRTR struct {
	TauMs int `yaml:"tau_ms"` // convergence horizon; 0 = instant mirror (snap)
}

type Storage struct {
	WALPath string `yaml:"wal_path"`
}

// Load reads the YAML configuration from disk.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	return &cfg, nil
}

// Validate checks the configuration for internal consistency and reports an
// aggregated error listing every problem found (or nil when valid). It is
// intended to be called right after Load so the process fails fast with a
// helpful message instead of misbehaving at runtime.
func (c *Config) Validate() error {
	var errs []string
	add := func(format string, args ...interface{}) {
		errs = append(errs, fmt.Sprintf(format, args...))
	}

	// Instruments: at least one, each with a non-empty symbol. Build the engine
	// symbol set used by the edge/emulator subset checks below.
	engine := make(map[string]bool, len(c.Instruments))
	if len(c.Instruments) == 0 {
		add("instruments: at least one instrument is required")
	}
	for i, inst := range c.Instruments {
		if strings.TrimSpace(inst.Symbol) == "" {
			add("instruments[%d]: symbol must not be empty", i)
			continue
		}
		engine[inst.Symbol] = true
	}

	// Network listen addresses.
	if strings.TrimSpace(c.Network.ListenGRPC) == "" {
		add("network.listen_grpc must not be empty")
	}
	if strings.TrimSpace(c.Network.ListenHTTP) == "" {
		add("network.listen_http must not be empty")
	}
	if strings.TrimSpace(c.Network.ListenWS) == "" {
		add("network.listen_ws must not be empty")
	}

	c.validateEmulator(engine, add)
	c.validateAPI(engine, add)
	c.validateMetrics(add)

	if len(errs) == 0 {
		return nil
	}
	return errors.New("invalid config:\n  - " + strings.Join(errs, "\n  - "))
}

func (c *Config) validateEmulator(engine map[string]bool, add func(string, ...interface{})) {
	em := c.Emulator
	if !em.Enabled {
		return
	}
	switch em.Venue {
	case "coinbase", "binance":
	case "replay":
		if em.Replay.File == "" {
			add("emulator.replay.file is required when venue=replay")
		}
	default:
		add("emulator.venue %q is unknown (want coinbase|binance|replay)", em.Venue)
	}
	if len(em.Instruments) == 0 {
		add("emulator.instruments must not be empty when the emulator is enabled")
	}
	for _, sym := range em.Instruments {
		if !engine[sym] {
			add("emulator.instruments: %q is not a configured engine instrument", sym)
		}
	}
	if em.Reference.DepthLevels < 0 {
		add("emulator.reference.depth_levels must be >= 0")
	}
	if em.Reference.RefreshMs < 0 {
		add("emulator.reference.refresh_ms must be >= 0")
	}
	if em.RTR.TauMs < 0 {
		add("emulator.rtr.tau_ms must be >= 0")
	}
	if em.Latency.FeedToBookMs < 0 || em.Latency.OrderAckMs < 0 ||
		em.Latency.FillReportMs < 0 || em.Latency.JitterMs < 0 {
		add("emulator.latency.* must be >= 0")
	}
	tx := em.Toxicity
	if tx.Scale < 0 {
		add("emulator.toxicity.scale must be >= 0")
	}
	if tx.KyleWeight < 0 || tx.VPINWeight < 0 {
		add("emulator.toxicity.{kyle_weight,vpin_weight} must be >= 0")
	}
}

func (c *Config) validateAPI(engine map[string]bool, add func(string, ...interface{})) {
	if b := c.API.Binance; b.Enabled {
		if strings.TrimSpace(b.Listen) == "" {
			add("api.binance.listen must not be empty when enabled")
		}
		if len(b.Symbols) == 0 {
			add("api.binance.symbols must map at least one symbol when enabled")
		}
		mapped := 0
		for _, sp := range b.Symbols {
			if sp.Binance == "" || sp.Engine == "" {
				add("api.binance.symbols: each entry needs both binance and engine")
				continue
			}
			if !engine[sp.Engine] {
				add("api.binance.symbols: engine %q is not a configured instrument", sp.Engine)
				continue
			}
			mapped++
		}
		if len(b.Symbols) > 0 && mapped == 0 {
			add("api.binance.symbols: no entry maps to a configured engine instrument")
		}
		if b.RatePerSec < 0 {
			add("api.binance.rate_per_sec must be >= 0")
		}
		if b.Burst < 0 {
			add("api.binance.burst must be >= 0")
		}
	}
	if cb := c.API.Coinbase; cb.Enabled {
		if strings.TrimSpace(cb.Listen) == "" {
			add("api.coinbase.listen must not be empty when enabled")
		}
		if len(cb.Products) == 0 {
			add("api.coinbase.products must list at least one product when enabled")
		}
		mapped := 0
		for _, p := range cb.Products {
			if p == "" {
				add("api.coinbase.products: product id must not be empty")
				continue
			}
			if !engine[p] {
				add("api.coinbase.products: %q is not a configured engine instrument", p)
				continue
			}
			mapped++
		}
		if len(cb.Products) > 0 && mapped == 0 {
			add("api.coinbase.products: no product maps to a configured engine instrument")
		}
		if cb.RatePerSec < 0 {
			add("api.coinbase.rate_per_sec must be >= 0")
		}
		if cb.Burst < 0 {
			add("api.coinbase.burst must be >= 0")
		}
	}
}

func (c *Config) validateMetrics(add func(string, ...interface{})) {
	if c.Metrics.Enabled && strings.TrimSpace(c.Metrics.Listen) == "" {
		add("metrics.listen must not be empty when metrics are enabled")
	}
}
