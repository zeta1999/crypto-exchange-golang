package config

import (
	"strings"
	"testing"
)

// validDev returns a valid, dev-like config (emulator + both API edges on).
func validDev() *Config {
	return &Config{
		Network: Network{
			ListenGRPC: ":50051",
			ListenHTTP: ":8080",
			ListenWS:   ":8081",
		},
		Instruments: []Instrument{
			{Symbol: "BTC-USD", Base: "BTC", Quote: "USD"},
			{Symbol: "ETH-USD", Base: "ETH", Quote: "USD"},
		},
		Emulator: Emulator{
			Enabled:     true,
			Venue:       "coinbase",
			Instruments: []string{"BTC-USD", "ETH-USD"},
			Reference:   EmulatorReference{DepthLevels: 20, RefreshMs: 250},
			RTR:         EmulatorRTR{TauMs: 3000},
		},
		API: APIConfig{
			Binance: BinanceConfig{
				Enabled: true, Listen: ":8082",
				Symbols:    []SymbolPair{{Binance: "BTCUSDT", Engine: "BTC-USD"}},
				RatePerSec: 20, Burst: 40,
			},
			Coinbase: CoinbaseConfig{
				Enabled: true, Listen: ":8083",
				Products:   []string{"BTC-USD"},
				RatePerSec: 20, Burst: 40,
			},
		},
		Metrics: Metrics{Enabled: true, Listen: ":9090"},
	}
}

func TestValidateValid(t *testing.T) {
	if err := validDev().Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}

func TestValidateDevYAML(t *testing.T) {
	cfg, err := Load("../../configs/dev.yaml")
	if err != nil {
		t.Fatalf("load dev.yaml: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("dev.yaml is invalid: %v", err)
	}
}

func TestValidateOptionsTestYAML(t *testing.T) {
	cfg, err := Load("../../configs/options-test.yaml")
	if err != nil {
		t.Fatalf("load options-test.yaml: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("options-test.yaml is invalid: %v", err)
	}
	if !cfg.API.Binance.Options.Enabled || len(cfg.API.Binance.Options.Underlyings) == 0 {
		t.Fatal("options-test.yaml should enable an options surface")
	}
}

func TestValidateInvalid(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"no instruments", func(c *Config) { c.Instruments = nil }, "at least one instrument"},
		{"empty grpc", func(c *Config) { c.Network.ListenGRPC = "" }, "listen_grpc"},
		{"unknown venue", func(c *Config) { c.Emulator.Venue = "kraken" }, "venue"},
		{"replay no file", func(c *Config) { c.Emulator.Venue = "replay"; c.Emulator.Replay.File = "" }, "replay.file"},
		{"emu instrument not engine", func(c *Config) { c.Emulator.Instruments = []string{"DOGE-USD"} }, "not a configured engine instrument"},
		{"negative depth", func(c *Config) { c.Emulator.Reference.DepthLevels = -1 }, "depth_levels"},
		{"negative tau", func(c *Config) { c.Emulator.RTR.TauMs = -5 }, "tau_ms"},
		{"negative toxicity", func(c *Config) { c.Emulator.Toxicity.Scale = -1 }, "toxicity.scale"},
		{"binance no symbols", func(c *Config) { c.API.Binance.Symbols = nil }, "symbols"},
		{"binance bad engine", func(c *Config) { c.API.Binance.Symbols = []SymbolPair{{Binance: "X", Engine: "NOPE"}} }, "not a configured instrument"},
		{"binance no listen", func(c *Config) { c.API.Binance.Listen = "" }, "binance.listen"},
		{"binance neg rate", func(c *Config) { c.API.Binance.RatePerSec = -1 }, "rate_per_sec"},
		{"coinbase bad product", func(c *Config) { c.API.Coinbase.Products = []string{"NOPE"} }, "not a configured engine instrument"},
		{"coinbase no listen", func(c *Config) { c.API.Coinbase.Listen = "" }, "coinbase.listen"},
		{"metrics no listen", func(c *Config) { c.Metrics.Listen = "" }, "metrics.listen"},
		{"options no underlyings", func(c *Config) {
			c.API.Binance.Options = OptionsConfig{Enabled: true}
		}, "options.underlyings"},
		{"options missing fields", func(c *Config) {
			c.API.Binance.Options = OptionsConfig{Enabled: true, Underlyings: []OptionUnderlying{
				{StaticIndex: 50000, Expiries: []string{"2026-12-31"}, Strikes: []float64{50000}},
			}}
		}, "coin and quote are required"},
		{"options no expiry", func(c *Config) {
			c.API.Binance.Options = OptionsConfig{Enabled: true, Underlyings: []OptionUnderlying{
				{Underlying: "BTCUSDT", Coin: "BTC", Quote: "USDT", StaticIndex: 50000, Strikes: []float64{50000}},
			}}
		}, "at least one expiry"},
		{"options bad strike", func(c *Config) {
			c.API.Binance.Options = OptionsConfig{Enabled: true, Underlyings: []OptionUnderlying{
				{Underlying: "BTCUSDT", Coin: "BTC", Quote: "USDT", StaticIndex: 50000, Expiries: []string{"2026-12-31"}, Strikes: []float64{-1}},
			}}
		}, "strikes must be > 0"},
		{"options unpriceable", func(c *Config) {
			c.API.Binance.Options = OptionsConfig{Enabled: true, Underlyings: []OptionUnderlying{
				{Underlying: "BTCUSDT", Coin: "BTC", Quote: "USDT", Expiries: []string{"2026-12-31"}, Strikes: []float64{50000}},
			}}
		}, "index_engine or a positive static_index"},
		{"options bad index_engine", func(c *Config) {
			c.API.Binance.Options = OptionsConfig{Enabled: true, Underlyings: []OptionUnderlying{
				{Underlying: "BTCUSDT", Coin: "BTC", Quote: "USDT", IndexEngine: "NOPE", Expiries: []string{"2026-12-31"}, Strikes: []float64{50000}},
			}}
		}, "is not a configured instrument"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validDev()
			tc.mutate(cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}
