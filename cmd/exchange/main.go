package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

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
	book.RegisterHook(func(evt string, data interface{}) {
		if err := walWriter.Append("orderbook."+evt, data); err != nil {
			log.Printf("wal hook error: %v", err)
		}
	})
	marginValidator := margin.NewValidator(book, margin.WithNotionalLimit(1_000_000))
	eng := engine.New(book, marginValidator, walWriter)
	tokenValidator := auth.NewTokenValidator(cfg.Network.TokenSecret)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	grpcSrv := grpcserver.New(eng, tokenValidator)
	wsHandler := wsadapter.NewHandler(eng, tokenValidator)
	uiFS := http.FS(os.DirFS("http/ui"))
	httpSrv := httpserver.New(eng, tokenValidator, wsHandler, uiFS)

	group, ctx := errgroup.WithContext(ctx)
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

	if err := group.Wait(); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
