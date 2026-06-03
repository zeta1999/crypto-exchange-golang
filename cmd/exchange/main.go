package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/zeta1999/crypto-exchange-golang/internal/account"
	"github.com/zeta1999/crypto-exchange-golang/internal/api/binance"
	"github.com/zeta1999/crypto-exchange-golang/internal/api/coinbase"
	"github.com/zeta1999/crypto-exchange-golang/internal/api/grpcserver"
	"github.com/zeta1999/crypto-exchange-golang/internal/api/httpserver"
	wsadapter "github.com/zeta1999/crypto-exchange-golang/internal/api/ws"
	"github.com/zeta1999/crypto-exchange-golang/internal/emulator"
	"github.com/zeta1999/crypto-exchange-golang/internal/engine"
	"github.com/zeta1999/crypto-exchange-golang/internal/margin"
	"github.com/zeta1999/crypto-exchange-golang/internal/metrics"
	"github.com/zeta1999/crypto-exchange-golang/internal/orderbook"
	"github.com/zeta1999/crypto-exchange-golang/internal/ratelimit"
	"github.com/zeta1999/crypto-exchange-golang/pkg/auth"
	"github.com/zeta1999/crypto-exchange-golang/pkg/config"
	"github.com/zeta1999/crypto-exchange-golang/pkg/decimal"
	"github.com/zeta1999/crypto-exchange-golang/pkg/wal"
	"golang.org/x/sync/errgroup"
)

func main() {
	cfgPath := os.Getenv("EXCHANGE_CONFIG")
	if cfgPath == "" {
		cfgPath = "configs/dev.yaml"
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		log.Fatalf("config validation failed: %v", err)
	}

	// Metrics registry. Always created (cheap); only exposed if cfg.Metrics
	// is enabled. The emulator + API edges register their instruments on it.
	reg := metrics.Default()
	ordersPlaced := reg.NewCounterVec("exchange_orders_placed_total", "Orders placed into the engine by API edge", "edge")
	tradesTotal := reg.NewCounter("exchange_trades_total", "Trades produced by the matching engine")
	cancelsTotal := reg.NewCounter("exchange_cancels_total", "Order cancels observed by the matching engine")

	var instruments []string
	for _, inst := range cfg.Instruments {
		instruments = append(instruments, inst.Symbol)
	}

	walWriter, err := wal.New(cfg.Storage.WALPath)
	if err != nil {
		log.Fatalf("init wal: %v", err)
	}
	defer walWriter.Close()

	book := orderbook.New(instruments)
	events := make(chan grpcserver.Event, 1024)
	book.RegisterHook(func(evt string, data interface{}) {
		if err := walWriter.Append("orderbook."+evt, data); err != nil {
			log.Printf("wal hook error: %v", err)
		}
		// Engine-level metrics: trades produced, cancels observed. Atomic, never
		// blocks the book.
		switch evt {
		case "trade":
			tradesTotal.Inc()
		case "cancel":
			cancelsTotal.Inc()
		}
		select {
		case events <- grpcserver.Event{Name: evt, Data: data}:
		default:
			log.Printf("stream event dropped: %s", evt)
		}
	})
	// Synthetic emulator liquidity bypasses user margin checks.
	marginValidator := syntheticExemptMargin{inner: margin.NewValidator(book, margin.WithNotionalLimit(1_000_000))}
	eng := engine.New(book, marginValidator, walWriter)
	tokenValidator := auth.NewTokenValidator(cfg.Network.TokenSecret)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	group, ctx := errgroup.WithContext(ctx)

	// Optionally mirror a live venue into the engine (feed → reference →
	// seeded synthetic liquidity with return-to-reference).
	if cfg.Emulator.Enabled {
		seeders, err := startEmulator(ctx, group, cfg.Emulator, eng, book, reg)
		if err != nil {
			log.Fatalf("start emulator: %v", err)
		}
		book.RegisterHook(func(evt string, data interface{}) {
			if evt != "trade" {
				return
			}
			t, ok := data.(*orderbook.Trade)
			if !ok {
				return
			}
			if s := seeders[t.Instrument]; s != nil {
				s.OnTrade(t) // account user fills against synthetic liquidity
			}
		})
	}

	grpcSrv := grpcserver.New(eng, tokenValidator, events)
	wsHandler := wsadapter.NewHandler(eng, tokenValidator)
	uiFS := http.FS(os.DirFS("http/ui"))
	httpSrv := httpserver.New(newMeteredEngine(eng, ordersPlaced, "native"), tokenValidator, wsHandler, uiFS)

	group.Go(func() error {
		log.Printf("gRPC listening on %s", cfg.Network.ListenGRPC)
		if err := grpcserver.ListenAndServe(ctx, cfg.Network.ListenGRPC, grpcSrv); err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		}
		return nil
	})
	group.Go(func() error {
		log.Printf("HTTP listening on %s", cfg.Network.ListenHTTP)
		if err := httpserver.ListenAndServe(ctx, cfg.Network.ListenHTTP, httpSrv, cfg.Network.TLS.CertFile, cfg.Network.TLS.KeyFile); err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
		return nil
	})
	group.Go(func() error {
		log.Printf("WebSocket listening on %s", cfg.Network.ListenWS)
		if err := wsadapter.Listen(ctx, cfg.Network.ListenWS, wsHandler, cfg.Network.TLS.CertFile, cfg.Network.TLS.KeyFile); err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
		return nil
	})

	// Artificial latency for the API edges (Phase 7): the static
	// emulator.latency knobs (+ jitter). order_ack_ms is applied synchronously
	// on the edge handler goroutine; fill_report_ms holds back delivery of the
	// fill (TRADE) user-data update asynchronously (the book hook can't sleep).
	// Scenario-driven mutation of edge latency is a future item.
	var apiAckDelay, apiFillDelay func() time.Duration
	if cfg.Emulator.Latency.OrderAckMs > 0 || cfg.Emulator.Latency.FillReportMs > 0 {
		edgeLat := emulator.NewLatency(emulator.LatencyConfig{
			OrderAck:   time.Duration(cfg.Emulator.Latency.OrderAckMs) * time.Millisecond,
			FillReport: time.Duration(cfg.Emulator.Latency.FillReportMs) * time.Millisecond,
			Jitter:     time.Duration(cfg.Emulator.Latency.JitterMs) * time.Millisecond,
			Dist:       emulator.ParseLatencyDist(cfg.Emulator.Latency.Distribution),
		}, cfg.Emulator.Toxicity.Seed)
		if cfg.Emulator.Latency.OrderAckMs > 0 {
			apiAckDelay = edgeLat.OrderAckDelay
		}
		if cfg.Emulator.Latency.FillReportMs > 0 {
			apiFillDelay = edgeLat.FillReportDelay
		}
	}

	// Optional Binance-spot-compatible REST edge (Phase 8, a documented
	// SUBSET). Additive and gated behind cfg.API.Binance.Enabled.
	if cfg.API.Binance.Enabled {
		bcfg := cfg.API.Binance
		pairs := make([]binance.SymbolPair, 0, len(bcfg.Symbols))
		for _, sp := range bcfg.Symbols {
			pairs = append(pairs, binance.SymbolPair{Binance: sp.Binance, Engine: sp.Engine})
		}
		symbolMap := binance.NewSymbolMap(pairs)
		authn := binance.NewAuthenticator(bcfg.APIKey, bcfg.Secret, nil)
		registry := binance.NewRegistry(nil)
		opts := []binance.Option{binance.WithMetrics(binance.NewMetrics(reg))}
		if bcfg.RatePerSec > 0 {
			opts = append(opts, binance.WithRateLimiter(ratelimit.NewKeyedLimiter(bcfg.RatePerSec, bcfg.Burst, 0)))
		}
		if led := buildLedger(bcfg.Balances); led != nil {
			opts = append(opts, binance.WithLedger(led))
		}
		if apiAckDelay != nil {
			opts = append(opts, binance.WithAckDelay(apiAckDelay))
		}
		if apiFillDelay != nil {
			opts = append(opts, binance.WithFillDelay(apiFillDelay))
		}
		binanceSrv := binance.New(newMeteredEngine(eng, ordersPlaced, "binance"), symbolMap, authn, registry, opts...)
		binanceSrv.AttachHooks(book) // wire trade/cancel hooks for fill tracking
		group.Go(func() error {
			log.Printf("Binance REST edge listening on %s", bcfg.Listen)
			if err := binance.ListenAndServe(ctx, bcfg.Listen, binanceSrv, cfg.Network.TLS.CertFile, cfg.Network.TLS.KeyFile); err != nil && !errors.Is(err, context.Canceled) {
				return err
			}
			return nil
		})
	}

	// Optional Coinbase-Advanced-Trade-compatible REST edge (Phase 9, a
	// documented SUBSET). Additive and gated behind cfg.API.Coinbase.Enabled.
	if cfg.API.Coinbase.Enabled {
		ccfg := cfg.API.Coinbase
		products := coinbase.NewProducts(ccfg.Products)
		authn := coinbase.NewAuthenticator(ccfg.APIKey, ccfg.Secret, ccfg.Passphrase, nil)
		if ccfg.JWTPublicKey != "" {
			v, err := coinbase.NewJWTVerifier(ccfg.JWTPublicKey, ccfg.JWTKeyName, nil)
			if err != nil {
				log.Fatalf("coinbase JWT verifier: %v", err)
			}
			authn.WithJWT(v)
			log.Printf("Coinbase edge: ES256 JWT auth enabled")
		}
		registry := coinbase.NewRegistry(nil)
		opts := []coinbase.Option{coinbase.WithMetrics(coinbase.NewMetrics(reg))}
		if ccfg.RatePerSec > 0 {
			opts = append(opts, coinbase.WithRateLimiter(ratelimit.NewKeyedLimiter(ccfg.RatePerSec, ccfg.Burst, 0)))
		}
		if led := buildLedger(ccfg.Balances); led != nil {
			opts = append(opts, coinbase.WithLedger(led))
		}
		if apiAckDelay != nil {
			opts = append(opts, coinbase.WithAckDelay(apiAckDelay))
		}
		if apiFillDelay != nil {
			opts = append(opts, coinbase.WithFillDelay(apiFillDelay))
		}
		coinbaseSrv := coinbase.New(newMeteredEngine(eng, ordersPlaced, "coinbase"), products, authn, registry, opts...)
		coinbaseSrv.AttachHooks(book) // wire trade/cancel hooks for fill tracking
		group.Go(func() error {
			log.Printf("Coinbase REST edge listening on %s", ccfg.Listen)
			if err := coinbase.ListenAndServe(ctx, ccfg.Listen, coinbaseSrv, cfg.Network.TLS.CertFile, cfg.Network.TLS.KeyFile); err != nil && !errors.Is(err, context.Canceled) {
				return err
			}
			return nil
		})
	}

	// Dedicated, unauthenticated Prometheus metrics listener (conventional for a
	// scrape endpoint). Disabled unless cfg.Metrics.Enabled.
	if cfg.Metrics.Enabled {
		mcfg := cfg.Metrics
		mux := http.NewServeMux()
		mux.Handle("/metrics", metrics.Handler(reg))
		server := &http.Server{Addr: mcfg.Listen, Handler: mux}
		group.Go(func() error {
			log.Printf("metrics listening on %s/metrics", mcfg.Listen)
			go func() {
				<-ctx.Done()
				_ = server.Shutdown(context.Background())
			}()
			if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		})
	}

	if err := group.Wait(); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

// buildLedger constructs an account.Ledger from a config balances map (asset ->
// decimal-string free amount). Returns nil when no balances are configured, so
// the edge keeps its legacy stub behaviour.
func buildLedger(balances map[string]string) *account.Ledger {
	if len(balances) == 0 {
		return nil
	}
	initial := make(map[string]decimal.Decimal, len(balances))
	for asset, amt := range balances {
		v, err := decimal.Parse(amt)
		if err != nil {
			log.Fatalf("invalid balance for %s: %v", asset, err)
		}
		initial[asset] = v
	}
	return account.NewLedger(initial)
}
