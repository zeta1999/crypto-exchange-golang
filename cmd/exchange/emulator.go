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
	"github.com/zeta1999/crypto-exchange-golang/internal/feed/replay"
	"github.com/zeta1999/crypto-exchange-golang/internal/metrics"
	"github.com/zeta1999/crypto-exchange-golang/internal/orderbook"
	"github.com/zeta1999/crypto-exchange-golang/internal/reference"
	"github.com/zeta1999/crypto-exchange-golang/internal/toxicity"
	"github.com/zeta1999/crypto-exchange-golang/pkg/config"
	"golang.org/x/sync/errgroup"
)

// emuMetrics groups the emulator-pipeline instruments. Counters are atomic and
// never block the goroutine that increments them; gauges that reflect live
// state are registered as GaugeFuncs read at scrape time.
type emuMetrics struct {
	feedEvents     *metrics.CounterVec // labels: venue, kind
	feedConnected  *metrics.Gauge
	feedErrors     *metrics.Gauge
	convergePasses *metrics.Counter
	rtrPasses      *metrics.Counter
	tapeInjects    *metrics.Counter
	toxicFires     *metrics.Counter
}

func newEmuMetrics(reg *metrics.Registry) *emuMetrics {
	if reg == nil {
		return nil
	}
	return &emuMetrics{
		feedEvents:     reg.NewCounterVec("emu_feed_events_total", "Venue feed events received by venue and kind", "venue", "kind"),
		feedConnected:  reg.NewGauge("emu_feed_connected", "1 when the venue feed is connected, else 0"),
		feedErrors:     reg.NewGauge("emu_feed_errors", "Cumulative venue feed errors/reconnects reported by Source.Status"),
		convergePasses: reg.NewCounter("emu_converge_passes_total", "Seeder reconcile/converge passes executed"),
		rtrPasses:      reg.NewCounter("emu_rtr_passes_total", "Return-to-reference convergence steps executed"),
		tapeInjects:    reg.NewCounter("emu_tape_injects_total", "Tape trades injected into the engine"),
		toxicFires:     reg.NewCounter("emu_toxicity_fires_total", "Adverse-selection sweeps fired by the toxicity injector"),
	}
}

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
func newFeedSource(cfg config.Emulator) (feed.Source, error) {
	switch cfg.Venue {
	case "coinbase":
		return coinbase.New(coinbase.Config{Symbols: cfg.Instruments, FeedTypes: []string{"orderbook", "trades"}}), nil
	case "binance":
		return binance.New(binance.Config{Symbols: cfg.Instruments, FeedTypes: []string{"orderbook", "trades"}}), nil
	case "replay":
		// Run the whole emulator offline from a recorded trace (Phase 1's
		// replay.Source), deterministically — ideal for reproducible OMS tests.
		if cfg.Replay.File == "" {
			return nil, fmt.Errorf("emulator venue=replay requires replay.file")
		}
		// speed<=0 = as-fast-as-possible (deterministic); >0 paces by the
		// recorded inter-event timestamps (1.0 = real time).
		return replay.New(cfg.Replay.File, replay.WithSpeed(cfg.Replay.Speed)), nil
	default:
		return nil, fmt.Errorf("unknown emulator venue %q (want coinbase|binance|replay)", cfg.Venue)
	}
}

// startEmulator wires the live-venue mirror: feed → reference books → per-
// instrument seeders converged toward the reference (return-to-reference). It
// registers a trade hook so user fills against synthetic liquidity are
// accounted, and launches the feed-consume and convergence loops on group.
//
// The emulator instruments must be registered engine symbols (1:1 venue↔engine
// symbol mapping). Returns the seeders keyed by symbol for the trade hook.
func startEmulator(ctx context.Context, group *errgroup.Group, cfg config.Emulator, eng *engine.Engine, book *orderbook.OrderBook, reg *metrics.Registry) (map[string]*emulator.Seeder, error) {
	src, err := newFeedSource(cfg)
	if err != nil {
		return nil, err
	}
	events, err := src.Start(ctx)
	if err != nil {
		return nil, fmt.Errorf("start %s feed: %w", cfg.Venue, err)
	}

	m := newEmuMetrics(reg)

	// Periodic sampler for feed health gauges (Source.Status isn't event-driven).
	if m != nil {
		group.Go(func() error {
			t := time.NewTicker(time.Second)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return nil
				case <-t.C:
					st := src.Status()
					if st.State == "connected" {
						m.feedConnected.Set(1)
					} else {
						m.feedConnected.Set(0)
					}
					m.feedErrors.Set(float64(st.ErrorCount))
				}
			}
		})
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
	// injectorModel exposes each instrument's toxicity model (when enabled) to the
	// metrics samplers for VPIN/lambda gauges. nil entry ⇒ toxicity disabled.
	injectorModel := make(map[string]*toxicity.Model, len(cfg.Instruments))
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
			injectorModel[sym] = model
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
						} else if m != nil {
							m.tapeInjects.Inc()
						}
						if injector != nil {
							fills, err := injector.Observe(ctx, ev.Trade)
							if err != nil {
								log.Printf("toxicity inject error: %v", err)
							} else if m != nil && len(fills) > 0 {
								m.toxicFires.Inc()
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
					if m != nil {
						m.feedEvents.WithLabelValues(cfg.Venue, "book").Inc()
					}
				case feed.EventTrade:
					if ev.Trade != nil {
						sym = ev.Trade.Instrument
					}
					if m != nil {
						m.feedEvents.WithLabelValues(cfg.Venue, "trade").Inc()
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

	// Per-instrument live-state gauges (read at scrape time).
	var (
		syntheticResting *metrics.GaugeVec
		bookAnomalies    *metrics.GaugeVec
		bookCrossings    *metrics.GaugeVec
		bookStale        *metrics.GaugeVec
		toxVPIN          *metrics.GaugeVec
		toxLambda        *metrics.GaugeVec
	)
	if m != nil {
		syntheticResting = reg.NewGaugeVec("emu_synthetic_resting", "Synthetic orders the seeder believes are resting", "instrument")
		bookAnomalies = reg.NewGaugeVec("emu_reference_anomalies", "Reference-book diffs dropped (no base snapshot)", "instrument")
		bookCrossings = reg.NewGaugeVec("emu_reference_crossings", "Times the reference book was left crossed", "instrument")
		bookStale = reg.NewGaugeVec("emu_reference_stale", "1 when the reference book is stale/uninitialized, else 0", "instrument")
		toxVPIN = reg.NewGaugeVec("emu_toxicity_vpin", "Current VPIN estimate from the toxicity model", "instrument")
		toxLambda = reg.NewGaugeVec("emu_toxicity_lambda", "Current Kyle lambda estimate from the toxicity model", "instrument")
	}

	staleAge := time.Duration(cfg.Reference.RefreshMs) * 4 * time.Millisecond

	seeders := make(map[string]*emulator.Seeder, len(cfg.Instruments))
	for _, sym := range cfg.Instruments {
		refBook := refs.Ensure(sym, cfg.Venue)
		seeder := emulator.NewSeeder(eng, refBook, emulator.Config{
			Instrument:  sym,
			DepthLevels: cfg.Reference.DepthLevels,
		})
		seeders[sym] = seeder

		if m != nil {
			sym := sym
			seeder := seeder
			refBook := refBook
			// A 1s sampler drives the per-instrument gauges from cheap live reads.
			rg := syntheticResting.WithLabelValues(sym)
			ab := bookAnomalies.WithLabelValues(sym)
			cb := bookCrossings.WithLabelValues(sym)
			sb := bookStale.WithLabelValues(sym)
			var vg, lg *metrics.Gauge
			if injectorModel[sym] != nil {
				vg = toxVPIN.WithLabelValues(sym)
				lg = toxLambda.WithLabelValues(sym)
			}
			group.Go(func() error {
				t := time.NewTicker(time.Second)
				defer t.Stop()
				for {
					select {
					case <-ctx.Done():
						return nil
					case <-t.C:
						rg.Set(float64(seeder.SyntheticCount()))
						ab.Set(float64(refBook.Anomalies()))
						cb.Set(float64(refBook.Crossings()))
						if refBook.Stale(time.Now(), staleAge) {
							sb.Set(1)
						} else {
							sb.Set(0)
						}
						if mdl := injectorModel[sym]; mdl != nil && vg != nil {
							vg.Set(mdl.VPIN())
							lg.Set(mdl.Lambda())
						}
					}
				}
			})
		}

		if tau > 0 {
			rtr := emulator.NewRTR(seeder, tau)
			group.Go(func() error { return runUntilCanceled(runRTR(ctx, rtr, refresh, m)) })
		} else {
			group.Go(func() error { return runUntilCanceled(runSeeder(ctx, seeder, refresh, m)) })
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

// runRTR drives a return-to-reference controller on a fixed tick, counting each
// convergence step. It mirrors RTR.Run but folds in the metrics increment (and
// uses the same actual-elapsed-dt semantics). m may be nil.
func runRTR(ctx context.Context, rtr *emulator.RTR, tick time.Duration, m *emuMetrics) error {
	t := time.NewTicker(tick)
	defer t.Stop()
	last := time.Now()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case now := <-t.C:
			dt := now.Sub(last)
			last = now
			if _, err := rtr.Step(ctx, dt); err != nil {
				log.Printf("rtr step error: %v", err)
			} else if m != nil {
				m.rtrPasses.Inc()
				m.convergePasses.Inc()
			}
		}
	}
}

// runSeeder drives a seeder's exact reconcile on a fixed tick (instant-mirror
// mode), counting each pass. Mirrors Seeder.Run. m may be nil.
func runSeeder(ctx context.Context, seeder *emulator.Seeder, tick time.Duration, m *emuMetrics) error {
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			if _, err := seeder.Reconcile(ctx); err != nil {
				log.Printf("seeder reconcile error: %v", err)
			} else if m != nil {
				m.convergePasses.Inc()
			}
		}
	}
}
