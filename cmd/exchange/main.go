package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/zeta1999/crypto-exchange-golang/internal/api/binance"
	"github.com/zeta1999/crypto-exchange-golang/internal/api/grpcserver"
	"github.com/zeta1999/crypto-exchange-golang/internal/api/httpserver"
	wsadapter "github.com/zeta1999/crypto-exchange-golang/internal/api/ws"
	"github.com/zeta1999/crypto-exchange-golang/internal/engine"
	"github.com/zeta1999/crypto-exchange-golang/internal/margin"
	"github.com/zeta1999/crypto-exchange-golang/internal/orderbook"
	"github.com/zeta1999/crypto-exchange-golang/pkg/auth"
	"github.com/zeta1999/crypto-exchange-golang/pkg/config"
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
		seeders, err := startEmulator(ctx, group, cfg.Emulator, eng, book)
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
	httpSrv := httpserver.New(eng, tokenValidator, wsHandler, uiFS)

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
		binanceSrv := binance.New(eng, symbolMap, authn, registry)
		binanceSrv.AttachHooks(book) // wire trade/cancel hooks for fill tracking
		group.Go(func() error {
			log.Printf("Binance REST edge listening on %s", bcfg.Listen)
			if err := binance.ListenAndServe(ctx, bcfg.Listen, binanceSrv, cfg.Network.TLS.CertFile, cfg.Network.TLS.KeyFile); err != nil && !errors.Is(err, context.Canceled) {
				return err
			}
			return nil
		})
	}

	if err := group.Wait(); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
