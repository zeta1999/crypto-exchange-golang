// Package emulator mirrors a live reference order book into the real
// matching engine, so user orders trade against live-like liquidity.
//
// The Seeder maps each reference price level to one tagged synthetic resting
// limit order in the engine. Converge(alpha) aligns that set with the
// reference, moving each level a fraction alpha of the way toward its target:
// alpha=1 is an exact reconcile (Phase 3), while alpha<1 is the progressive
// return-to-reference of Phase 4 (driven by the RTR controller in rtr.go).
//
// The seeder accounts for fills: when a user trade eats a synthetic order
// (reported via OnTrade from an engine hook), the level's tracked volume
// shrinks, so the next Converge tops it back up toward the reference. Trade
// replay (Phase 5) and toxicity (Phase 6) layer on top of this anchor.
package emulator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zeta1999/crypto-exchange-golang/internal/orderbook"
	"github.com/zeta1999/crypto-exchange-golang/internal/reference"
)

// volEps is the volume below which a level is treated as empty, and the
// threshold for "unchanged" volume — avoids float-equality churn.
const volEps = 1e-9

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
//
// The placed map is the seeder's record of the synthetic orders it owns. User
// trades that fill those orders are reported through OnTrade into a separate
// fills buffer (guarded by its own lock so the hook never contends with — or
// deadlocks against — a Converge pass), and folded into placed at the start of
// each Converge. The seeder is the sole *placer* of synthetic orders; cancels
// tolerate an order that was already filled away.
type Seeder struct {
	matcher Matcher
	ref     *reference.Book
	cfg     Config

	mu     sync.Mutex
	placed map[string]synthOrder // levelKey -> resting synthetic order

	fillMu sync.Mutex
	fills  map[string]float64 // levelKey -> volume filled since last Converge
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
		fills:   make(map[string]float64),
	}
}

// priceKey is the canonical per-level identity, matching the reference book's
// float keying so a level maps to a stable engine order ID across passes.
func priceKey(side orderbook.Side, price float64) string {
	return string(side) + ":" + strconv.FormatFloat(price, 'f', -1, 64)
}

func (s *Seeder) idPrefix() string { return "synth:" + s.cfg.Instrument + ":" }

func (s *Seeder) synthID(side orderbook.Side, price float64) string {
	return s.idPrefix() + priceKey(side, price)
}

// OnTrade records a fill against a synthetic order. Wire it to the order
// book's "trade" hook. It writes only to the fills buffer under its own lock,
// so it never blocks on (or deadlocks with) an in-flight Converge.
func (s *Seeder) OnTrade(t *orderbook.Trade) {
	if t == nil || t.Instrument != s.cfg.Instrument {
		return
	}
	prefix := s.idPrefix()
	for _, id := range [...]string{t.BuyOrderID, t.SellOrderID} {
		if !strings.HasPrefix(id, prefix) {
			continue
		}
		k := strings.TrimPrefix(id, prefix)
		s.fillMu.Lock()
		s.fills[k] += t.Volume
		s.fillMu.Unlock()
	}
}

// Reconcile aligns the engine exactly with the reference in one pass
// (equivalent to Converge with alpha=1).
func (s *Seeder) Reconcile(ctx context.Context) (Stats, error) {
	return s.Converge(ctx, 1.0)
}

// Converge moves the engine's synthetic liquidity a fraction alpha toward the
// reference book and returns what changed. For each level the new target is
//
//	target = current + alpha*(reference - current)
//
// (stale levels not in the reference decay toward zero), so alpha=1 snaps to
// the reference and 0<alpha<1 approaches it geometrically over repeated calls
// — the return-to-reference behavior. Pending fills are folded in first, so a
// user-eaten level is topped back up. Levels already at their target are left
// untouched (no churn, preserved queue priority). Cancellations precede
// placements; combined with the uncrossed-reference guard, a placement can
// never cross a surviving synthetic order, so synthetic self-trades don't
// occur (asserted in tests).
func (s *Seeder) Converge(ctx context.Context, alpha float64) (Stats, error) {
	alpha = math.Max(0, math.Min(1, alpha))

	s.mu.Lock()
	defer s.mu.Unlock()

	var st Stats
	if !s.ref.Initialized() || s.ref.Crossed() {
		st.Skipped = true
		return st, nil
	}
	s.foldFillsLocked()

	bids, asks := s.ref.Depth(s.cfg.DepthLevels)
	refVol := make(map[string]float64, len(bids)+len(asks))
	meta := make(map[string]synthOrder, len(bids)+len(asks))
	for _, l := range bids {
		k := priceKey(orderbook.SideBuy, l.Price)
		refVol[k] = l.Quantity
		meta[k] = synthOrder{id: s.synthID(orderbook.SideBuy, l.Price), side: orderbook.SideBuy, price: l.Price}
	}
	for _, l := range asks {
		k := priceKey(orderbook.SideSell, l.Price)
		refVol[k] = l.Quantity
		meta[k] = synthOrder{id: s.synthID(orderbook.SideSell, l.Price), side: orderbook.SideSell, price: l.Price}
	}

	// Compute the per-level target over the union of placed and reference
	// levels. Stale levels (placed but absent from the reference) target zero.
	type plan struct {
		o       synthOrder
		cur     float64
		target  float64
		existed bool
	}
	plans := make(map[string]plan, len(refVol)+len(s.placed))
	for k, want := range meta {
		cur, existed := s.placed[k]
		var c float64
		if existed {
			c = cur.volume
			want.id, want.side, want.price = cur.id, cur.side, cur.price
		}
		plans[k] = plan{o: want, cur: c, target: c + alpha*(refVol[k]-c), existed: existed}
	}
	for k, cur := range s.placed {
		if _, ok := refVol[k]; ok {
			continue // already planned above
		}
		plans[k] = plan{o: cur, cur: cur.volume, target: cur.volume * (1 - alpha), existed: true}
	}

	// Pass 1: cancel everything that must change (removed, drained, or
	// resized). A not-found order was already filled/cancelled away — benign.
	apply := make([]plan, 0, len(plans))
	for k, p := range plans {
		if p.existed && math.Abs(p.target-p.cur) <= volEps {
			continue // already at target; leave the resting order in place
		}
		if p.existed {
			if _, err := s.matcher.CancelOrder(ctx, s.cfg.Instrument, p.o.id); err != nil && !errors.Is(err, orderbook.ErrOrderNotFound) {
				return st, fmt.Errorf("cancel %s: %w", p.o.id, err)
			}
			delete(s.placed, k)
		}
		if p.target <= volEps {
			if p.existed {
				st.Cancelled++
			}
			continue // nothing to (re-)place
		}
		apply = append(apply, p)
	}

	// Pass 2: place new/resized levels at their target volume.
	for _, p := range apply {
		ord := &orderbook.Order{
			ID:         p.o.id,
			Instrument: s.cfg.Instrument,
			Price:      p.o.price,
			Volume:     p.target,
			Side:       p.o.side,
			Metadata:   map[string]string{MetadataKey: MetadataValue},
		}
		trades, _, err := s.matcher.PlaceLimit(ctx, ord)
		if err != nil {
			return st, fmt.Errorf("place %s: %w", p.o.id, err)
		}
		var filled float64
		for _, t := range trades {
			filled += t.Volume
		}
		st.Trades += len(trades)
		rested := p.target - filled
		if rested > volEps {
			p.o.volume = rested
			s.placed[priceKey(p.o.side, p.o.price)] = p.o
		}
		if p.existed {
			st.Resized++
		} else {
			st.Placed++
		}
	}
	return st, nil
}

// foldFillsLocked applies buffered fills to the placed volumes. Caller holds s.mu.
func (s *Seeder) foldFillsLocked() {
	s.fillMu.Lock()
	fills := s.fills
	s.fills = make(map[string]float64)
	s.fillMu.Unlock()
	for k, v := range fills {
		cur, ok := s.placed[k]
		if !ok {
			continue
		}
		cur.volume -= v
		if cur.volume <= volEps {
			delete(s.placed, k)
		} else {
			s.placed[k] = cur
		}
	}
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
		if _, err := s.matcher.CancelOrder(ctx, s.cfg.Instrument, cur.id); err != nil && !errors.Is(err, orderbook.ErrOrderNotFound) {
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
