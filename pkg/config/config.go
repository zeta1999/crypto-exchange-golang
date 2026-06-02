package config

import (
	"fmt"
	"os"

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
	Enabled bool         `yaml:"enabled"`
	Listen  string       `yaml:"listen"`
	APIKey  string       `yaml:"api_key"`
	Secret  string       `yaml:"secret"`
	Symbols []SymbolPair `yaml:"symbols"`
}

// SymbolPair maps a Binance symbol ("BTCUSDT") to an engine instrument
// ("BTC-USD").
type SymbolPair struct {
	Binance string `yaml:"binance"`
	Engine  string `yaml:"engine"`
}

// CoinbaseConfig configures the Coinbase-Advanced-Trade-compatible REST edge
// (PLAN Phase 9, a documented SUBSET). Disabled by default. APIKey/Secret/
// Passphrase are the credentials a client must present (legacy Coinbase
// Exchange HMAC-SHA256 signing; JWT/ES256 is deferred). Products is the
// allow-list of product IDs to serve; Coinbase product IDs ("BTC-USD") are
// identical to engine instruments.
type CoinbaseConfig struct {
	Enabled    bool     `yaml:"enabled"`
	Listen     string   `yaml:"listen"`
	APIKey     string   `yaml:"api_key"`
	Secret     string   `yaml:"secret"`
	Passphrase string   `yaml:"passphrase"`
	Products   []string `yaml:"products"`
}

// Emulator configures live-venue mirroring (feed → reference book → seeded
// synthetic liquidity in the engine, with return-to-reference). When
// disabled, the exchange runs as a plain matching engine. See PLAN.md.
type Emulator struct {
	Enabled     bool               `yaml:"enabled"`
	Venue       string             `yaml:"venue"` // "coinbase" | "binance"
	Instruments []string           `yaml:"instruments"`
	Reference   EmulatorReference  `yaml:"reference"`
	RTR         EmulatorRTR        `yaml:"rtr"`
	Toxicity    EmulatorToxicity   `yaml:"toxicity"`
	PriceShift  EmulatorPriceShift `yaml:"price_shift"`
	Latency     EmulatorLatency    `yaml:"latency"`
	Scenario    EmulatorScenario   `yaml:"scenario"`
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
