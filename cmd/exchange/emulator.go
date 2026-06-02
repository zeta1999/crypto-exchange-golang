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
	lat := emulator.NewLatency(emulator.LatencyConfig{
		FeedToBook: time.Duration(cfg.Latency.FeedToBookMs) * time.Millisecond,
		OrderAck:   time.Duration(cfg.Latency.OrderAckMs) * time.Millisecond,
		FillReport: time.Duration(cfg.Latency.FillReportMs) * time.Millisecond,
		Jitter:     time.Duration(cfg.Latency.JitterMs) * time.Millisecond,
	}, cfg.Toxicity.Seed)
	// TODO(phase7): apply OrderAck/FillReport at API edges (Phase 8/9).

	// Controls is the runtime-mutable holder the dispatcher + workers read on
	// every event. Seeded from the static config above, it is identical to the
	// pre-scenario behavior until a scenario runner mutates it (below). Reads
	// are lock-free atomic loads, so the scenario writer never blocks the
	// dispatcher hot path.
	controls := emulator.NewControls(shift, lat)

	// Optional scripted timeline (PLAN §5 Phase 7, the test-bed core): a JSONL
	// scenario drives the fault injectors through regime changes on cue. Empty
	// file ⇒ no runner; controls stay at the static config values above. Loaded
	// before any worker starts so a malformed scenario fails startup loudly.
	if cfg.Scenario.File != "" {
		sc, err := emulator.LoadScenario(cfg.Scenario.File, cfg.Toxicity.Seed)
		if err != nil {
			return nil, fmt.Errorf("load scenario: %w", err)
		}
		log.Printf("scenario loaded: %d events (file=%s speed=%v)", sc.Len(), cfg.Scenario.File, cfg.Scenario.Speed)
		start := time.Now()
		speed := cfg.Scenario.Speed
		group.Go(func() error { return runUntilCanceled(sc.Run(ctx, controls, start, speed)) })
	}

	// Each instrument gets its own buffered channel + worker goroutine handling
	// BOTH book and trade events, so the shared dispatcher only routes (never
	// sleeps or does engine work) — one slow instrument or a feed→book latency
	// can't head-of-line block the others. Per worker: book events rebuild the
	// reference (after the feed→book latency), trade events are replayed (fill
	// resting orders in sync) and fed to the optional toxicity injector.
	tx := cfg.Toxicity
	inCh := make(map[string]chan feed.Event, len(cfg.Instruments))
	for _, sym := range cfg.Instruments {
		ch := make(chan feed.Event, 1024)
		inCh[sym] = ch
		refBook := refs.Ensure(sym, cfg.Venue)
		tape := emulator.NewTapeReplay(eng, sym)
		var injector *emulator.ToxicInjector
		if tx.Scale > 0 {
			model := toxicity.New(toxicity.Config{
				Scale: tx.Scale, KyleWeight: tx.KyleWeight, VPINWeight: tx.VPINWeight,
				WindowTrades: tx.WindowTrades, BucketVolume: tx.BucketVolume,
				Buckets: tx.Buckets, Seed: tx.Seed,
			})
			injector = emulator.NewToxicInjector(eng, refBook, model, sym, tx.Seed)
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
					switch ev.Kind {
					case feed.EventBook:
						// Read the current latency live (a scenario may have swapped
						// it). Same Latency until/unless a scenario mutates it.
						lat := controls.Latency()
						lat.Sleep(ctx, lat.FeedToBookDelay()) // per-instrument slow feed
						refBook.Apply(ev.Book)
					case feed.EventTrade:
						if ev.Trade == nil {
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
			}
		})
	}

	// The dispatcher only price-shifts and routes by instrument — no blocking.
	group.Go(func() error {
		for {
			select {
			case <-ctx.Done():
				return nil
			case ev, ok := <-events:
				if !ok {
					return nil
				}
				// Read the current shift live (a scenario may have changed it).
				// ApplyEvent is a fast no-op when the shift is the identity, so
				// the common static-no-shift case stays zero-overhead.
				ev = controls.PriceShift().ApplyEvent(ev) // coherent dislocation across book + tape
				var sym string
				switch ev.Kind {
				case feed.EventBook:
					if ev.Book != nil {
						sym = ev.Book.Instrument
					}
				case feed.EventTrade:
					if ev.Trade != nil {
						sym = ev.Trade.Instrument
					}
				}
				if ch := inCh[sym]; ch != nil {
					select {
					case ch <- ev:
					default:
						log.Printf("emulator channel full for %s; dropping event", sym)
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
