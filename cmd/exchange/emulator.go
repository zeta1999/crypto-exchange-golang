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
	"github.com/zeta1999/crypto-exchange-golang/internal/toxicity"
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

	// Phase 7 fault injectors. The price shift transforms every venue price so
	// the reference book, seeder, AND tape all see a coherent dislocation
	// (skipped entirely when it is the identity, for zero overhead). The
	// latency injector wires the FEED→BOOK delay here; OrderAck/FillReport
	// belong at the API edges (Phase 8/9). Seed reuses the toxicity seed so a
	// scenario is reproducible from a single knob.
	shift := emulator.NewPriceShift(cfg.PriceShift.OffsetBps, cfg.PriceShift.Scale)
	shiftActive := !shift.IsIdentity()
	lat := emulator.NewLatency(emulator.LatencyConfig{
		FeedToBook: time.Duration(cfg.Latency.FeedToBookMs) * time.Millisecond,
		OrderAck:   time.Duration(cfg.Latency.OrderAckMs) * time.Millisecond,
		FillReport: time.Duration(cfg.Latency.FillReportMs) * time.Millisecond,
		Jitter:     time.Duration(cfg.Latency.JitterMs) * time.Millisecond,
	}, cfg.Toxicity.Seed)
	// TODO(phase7): apply OrderAck/FillReport at API edges (Phase 8/9).

	// Each instrument's tape runs on its own goroutine, fed by a buffered
	// channel, so a burst of trade prints can't head-of-line block reference
	// book updates (which the dispatcher applies inline) or the seeder/RTR.
	// Each trade is replayed (fills resting orders in sync) and then fed to the
	// optional toxicity injector (adverse selection scaled by Kyle λ / VPIN).
	tx := cfg.Toxicity
	tapeCh := make(map[string]chan feed.Event, len(cfg.Instruments))
	for _, sym := range cfg.Instruments {
		ch := make(chan feed.Event, 1024)
		tapeCh[sym] = ch
		tape := emulator.NewTapeReplay(eng, sym)
		var injector *emulator.ToxicInjector
		if tx.Scale > 0 {
			model := toxicity.New(toxicity.Config{
				Scale: tx.Scale, KyleWeight: tx.KyleWeight, VPINWeight: tx.VPINWeight,
				WindowTrades: tx.WindowTrades, BucketVolume: tx.BucketVolume,
				Buckets: tx.Buckets, Seed: tx.Seed,
			})
			injector = emulator.NewToxicInjector(eng, refs.Ensure(sym, cfg.Venue), model, sym, tx.Seed)
		}
		group.Go(func() error {
			for {
				select {
				case <-ctx.Done():
					return nil
				case ev, ok := <-ch:
					if !ok {
						return nil
					}
					if ev.Kind != feed.EventTrade || ev.Trade == nil {
						continue
					}
					if _, err := tape.Inject(ctx, ev.Trade); err != nil {
						log.Printf("tape inject error: %v", err)
					}
					if injector != nil {
						if _, err := injector.Observe(ctx, ev.Trade); err != nil {
							log.Printf("toxicity inject error: %v", err)
						}
					}
				}
			}
		})
	}

	group.Go(func() error {
		for {
			select {
			case <-ctx.Done():
				return nil
			case ev, ok := <-events:
				if !ok {
					return nil
				}
				// Apply the price shift before routing so the reference book,
				// seeder, and tape all observe the same dislocated venue.
				if shiftActive {
					ev = shift.ApplyEvent(ev)
				}
				switch ev.Kind {
				case feed.EventBook:
					// Feed→book latency delays the reference goroutine inline
					// (intended: a slow feed). No-op for the dev default of 0.
					lat.Sleep(ctx, lat.FeedToBookDelay())
					refs.Apply(ev) // fast; rebuilds the reference book inline
				case feed.EventTrade:
					if ev.Trade == nil {
						continue
					}
					if ch := tapeCh[ev.Trade.Instrument]; ch != nil {
						select {
						case ch <- ev: // hand off to the instrument's tape goroutine
						default:
							log.Printf("tape channel full for %s; dropping print", ev.Trade.Instrument)
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
