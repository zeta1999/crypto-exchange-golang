// Package emulator mirrors a live reference order book into the real
// matching engine, so user orders trade against live-like liquidity.
//
// The Seeder (Phase 3) maps each reference price level to one tagged
// synthetic resting limit order in the engine and reconciles that set on a
// cadence — adding new levels, resizing changed ones, and cancelling levels
// that left the reference. Later phases layer return-to-reference (Phase 4),
// trade replay (Phase 5), and toxicity (Phase 6) on top of this anchor.
package emulator

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/zeta1999/crypto-exchange-golang/internal/orderbook"
	"github.com/zeta1999/crypto-exchange-golang/internal/reference"
)

// MetadataKey/MetadataValue tag engine orders the seeder owns, so other
// layers (RTR, toxicity, API) can distinguish synthetic liquidity from real
// user orders.
const (
	MetadataKey   = "synthetic"
	MetadataValue = "true"
)

// Matcher is the slice of the matching engine the seeder drives. *engine.Engine
// satisfies it; tests can substitute a fake.
type Matcher interface {
	PlaceLimit(ctx context.Context, ord *orderbook.Order) ([]*orderbook.Trade, *orderbook.Snapshot, error)
	CancelOrder(ctx context.Context, instrument, orderID string) (*orderbook.Order, error)
}

// Config parameterizes a Seeder.
type Config struct {
	// Instrument is the engine symbol to seed (must be registered with the
	// engine's order book).
	Instrument string
	// DepthLevels caps how many price levels per side are mirrored; <= 0
	// mirrors the full reference depth.
	DepthLevels int
}

// Seeder reconciles one instrument's engine liquidity against a reference book.
type Seeder struct {
	matcher Matcher
	ref     *reference.Book
	cfg     Config

	mu     sync.Mutex
	placed map[string]synthOrder // levelKey -> resting synthetic order
}

type synthOrder struct {
	id     string
	side   orderbook.Side
	price  float64
	volume float64
}

// Stats summarizes one reconcile pass.
type Stats struct {
	Placed    int  // new levels added
	Resized   int  // levels whose volume changed (cancel + re-place)
	Cancelled int  // levels removed (no longer in reference)
	Trades    int  // unexpected fills during placement (should be 0 with no user flow)
	Skipped   bool // reference not initialized or crossed; no action taken
}

// NewSeeder constructs a Seeder mirroring ref into matcher for cfg.Instrument.
func NewSeeder(matcher Matcher, ref *reference.Book, cfg Config) *Seeder {
	return &Seeder{
		matcher: matcher,
		ref:     ref,
		cfg:     cfg,
		placed:  make(map[string]synthOrder),
	}
}

// priceKey is the canonical per-level identity, matching the reference book's
// float keying so a level maps to a stable engine order ID across passes.
func priceKey(side orderbook.Side, price float64) string {
	return string(side) + ":" + strconv.FormatFloat(price, 'f', -1, 64)
}

func (s *Seeder) synthID(side orderbook.Side, price float64) string {
	return "synth:" + s.cfg.Instrument + ":" + priceKey(side, price)
}

// Reconcile makes one pass to align the engine's synthetic liquidity with the
// current reference book. Cancellations run before placements so the book is
// never transiently crossed by a stale order. It is safe to call repeatedly;
// an unchanged reference produces a no-op pass.
func (s *Seeder) Reconcile(ctx context.Context) (Stats, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var st Stats
	// Never mirror a book we can't trust: an uninitialized book has no
	// liquidity yet, and a crossed book would seed self-crossing orders.
	if !s.ref.Initialized() || s.ref.Crossed() {
		st.Skipped = true
		return st, nil
	}

	bids, asks := s.ref.Depth(s.cfg.DepthLevels)
	desired := make(map[string]synthOrder, len(bids)+len(asks))
	for _, l := range bids {
		k := priceKey(orderbook.SideBuy, l.Price)
		desired[k] = synthOrder{id: s.synthID(orderbook.SideBuy, l.Price), side: orderbook.SideBuy, price: l.Price, volume: l.Quantity}
	}
	for _, l := range asks {
		k := priceKey(orderbook.SideSell, l.Price)
		desired[k] = synthOrder{id: s.synthID(orderbook.SideSell, l.Price), side: orderbook.SideSell, price: l.Price, volume: l.Quantity}
	}

	// Pass 1: cancel levels that are gone or whose volume changed. A changed
	// level is re-placed in pass 2; we remember it so it counts as a resize,
	// not a fresh placement.
	resized := make(map[string]bool)
	for k, cur := range s.placed {
		want, ok := desired[k]
		if ok && want.volume == cur.volume {
			continue // unchanged; leave the resting order in place
		}
		if _, err := s.matcher.CancelOrder(ctx, s.cfg.Instrument, cur.id); err != nil {
			return st, fmt.Errorf("cancel %s: %w", cur.id, err)
		}
		delete(s.placed, k)
		if ok {
			resized[k] = true
			st.Resized++
		} else {
			st.Cancelled++
		}
	}

	// Pass 2: place new and resized levels.
	for k, want := range desired {
		if _, ok := s.placed[k]; ok {
			continue // already resting at the right volume
		}
		ord := &orderbook.Order{
			ID:         want.id,
			Instrument: s.cfg.Instrument,
			Price:      want.price,
			Volume:     want.volume,
			Side:       want.side,
			Metadata:   map[string]string{MetadataKey: MetadataValue},
		}
		trades, _, err := s.matcher.PlaceLimit(ctx, ord)
		if err != nil {
			return st, fmt.Errorf("place %s: %w", want.id, err)
		}
		var filled float64
		for _, t := range trades {
			filled += t.Volume
		}
		st.Trades += len(trades)
		if rested := want.volume - filled; rested > 0 {
			want.volume = rested
			s.placed[k] = want
			if !resized[k] { // resizes already counted in pass 1
				st.Placed++
			}
		}
	}
	return st, nil
}

// SyntheticCount returns how many synthetic orders the seeder currently
// believes are resting.
func (s *Seeder) SyntheticCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.placed)
}

// Clear cancels every synthetic order the seeder placed.
func (s *Seeder) Clear(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, cur := range s.placed {
		if _, err := s.matcher.CancelOrder(ctx, s.cfg.Instrument, cur.id); err != nil {
			return fmt.Errorf("cancel %s: %w", cur.id, err)
		}
		delete(s.placed, k)
	}
	return nil
}

// Run reconciles on every tick of interval until ctx is cancelled. Reconcile
// errors are logged, not fatal, so a transient engine error doesn't tear down
// seeding.
func (s *Seeder) Run(ctx context.Context, interval time.Duration) error {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			if _, err := s.Reconcile(ctx); err != nil {
				slog.Warn("seeder reconcile error", "instrument", s.cfg.Instrument, "error", err)
			}
		}
	}
}
