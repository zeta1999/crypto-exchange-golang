package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/zeta1999/crypto-exchange-golang/internal/emulator"
	"github.com/zeta1999/crypto-exchange-golang/internal/engine"
	"github.com/zeta1999/crypto-exchange-golang/internal/feed"
	"github.com/zeta1999/crypto-exchange-golang/internal/feed/binance"
	"github.com/zeta1999/crypto-exchange-golang/internal/feed/coinbase"
	"github.com/zeta1999/crypto-exchange-golang/internal/orderbook"
	"github.com/zeta1999/crypto-exchange-golang/internal/reference"
	"github.com/zeta1999/crypto-exchange-golang/pkg/config"
	"golang.org/x/sync/errgroup"
)

// syntheticExemptMargin delegates to an inner validator for user orders but
// waves through the emulator's own synthetic liquidity, which mirrors the
// reference book and must not be rejected by user credit limits.
type syntheticExemptMargin struct{ inner engine.MarginValidator }

func (m syntheticExemptMargin) Validate(ctx context.Context, ord *orderbook.Order) error {
	// Emulator-internal orders (mirrored synthetic liquidity, replayed tape
	// trades) are not subject to user credit limits.
	if ord.Metadata[emulator.MetadataKey] == emulator.MetadataValue ||
		ord.Metadata[emulator.TapeMetadataKey] == "true" {
		return nil
	}
	return m.inner.Validate(ctx, ord)
}

// newFeedSource builds the venue feed for the configured instruments,
// subscribing to order-book updates (the reference book's input) and trades
// (the tape replayed against resting orders).
func newFeedSource(venue string, instruments []string) (feed.Source, error) {
	switch venue {
	case "coinbase":
		return coinbase.New(coinbase.Config{Symbols: instruments, FeedTypes: []string{"orderbook", "trades"}}), nil
	case "binance":
		return binance.New(binance.Config{Symbols: instruments, FeedTypes: []string{"orderbook", "trades"}}), nil
	default:
		return nil, fmt.Errorf("unknown emulator venue %q (want coinbase|binance)", venue)
	}
}

// startEmulator wires the live-venue mirror: feed → reference books → per-
// instrument seeders converged toward the reference (return-to-reference). It
// registers a trade hook so user fills against synthetic liquidity are
// accounted, and launches the feed-consume and convergence loops on group.
//
// The emulator instruments must be registered engine symbols (1:1 venue↔engine
// symbol mapping). Returns the seeders keyed by symbol for the trade hook.
func startEmulator(ctx context.Context, group *errgroup.Group, cfg config.Emulator, eng *engine.Engine, book *orderbook.OrderBook) (map[string]*emulator.Seeder, error) {
	src, err := newFeedSource(cfg.Venue, cfg.Instruments)
	if err != nil {
		return nil, err
	}
	events, err := src.Start(ctx)
	if err != nil {
		return nil, fmt.Errorf("start %s feed: %w", cfg.Venue, err)
	}

	refs := reference.NewSet()
	tapes := make(map[string]*emulator.TapeReplay, len(cfg.Instruments))
	for _, sym := range cfg.Instruments {
		tapes[sym] = emulator.NewTapeReplay(eng, sym)
	}
	// One dispatcher routes the single feed channel: book frames rebuild the
	// reference book; trade prints are replayed against the engine so resting
	// (user + synthetic) orders fill in sync with the live tape.
	group.Go(func() error {
		for {
			select {
			case <-ctx.Done():
				return nil
			case ev, ok := <-events:
				if !ok {
					return nil
				}
				switch ev.Kind {
				case feed.EventBook:
					refs.Apply(ev)
				case feed.EventTrade:
					if ev.Trade != nil {
						if tr := tapes[ev.Trade.Instrument]; tr != nil {
							if _, err := tr.Inject(ctx, ev.Trade); err != nil {
								log.Printf("tape inject error: %v", err)
							}
						}
					}
				}
			}
		}
	})

	refresh := time.Duration(cfg.Reference.RefreshMs) * time.Millisecond
	if refresh <= 0 {
		refresh = 250 * time.Millisecond
	}
	tau := time.Duration(cfg.RTR.TauMs) * time.Millisecond

	seeders := make(map[string]*emulator.Seeder, len(cfg.Instruments))
	for _, sym := range cfg.Instruments {
		refBook := refs.Ensure(sym, cfg.Venue)
		seeder := emulator.NewSeeder(eng, refBook, emulator.Config{
			Instrument:  sym,
			DepthLevels: cfg.Reference.DepthLevels,
		})
		seeders[sym] = seeder

		if tau > 0 {
			rtr := emulator.NewRTR(seeder, tau)
			group.Go(func() error { return runUntilCanceled(rtr.Run(ctx, refresh)) })
		} else {
			group.Go(func() error { return runUntilCanceled(seeder.Run(ctx, refresh)) })
		}
	}

	log.Printf("emulator mirroring %s %v (depth=%d refresh=%s tau=%s)",
		cfg.Venue, cfg.Instruments, cfg.Reference.DepthLevels, refresh, tau)
	return seeders, nil
}

// runUntilCanceled treats context cancellation as a clean shutdown.
func runUntilCanceled(err error) error {
	if err == nil || err == context.Canceled {
		return nil
	}
	return err
}
