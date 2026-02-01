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
