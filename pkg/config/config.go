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
}

// Emulator configures live-venue mirroring (feed → reference book → seeded
// synthetic liquidity in the engine, with return-to-reference). When
// disabled, the exchange runs as a plain matching engine. See PLAN.md.
type Emulator struct {
	Enabled     bool              `yaml:"enabled"`
	Venue       string            `yaml:"venue"` // "coinbase" | "binance"
	Instruments []string          `yaml:"instruments"`
	Reference   EmulatorReference `yaml:"reference"`
	RTR         EmulatorRTR       `yaml:"rtr"`
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
