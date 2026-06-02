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
	"github.com/zeta1999/crypto-exchange-golang/pkg/decimal"
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
	nonce  uint64                // generation counter for unique order IDs

	fillMu sync.Mutex
	fills  map[string]decimal.Decimal // full order ID -> volume filled since last Converge
}

type synthOrder struct {
	id     string
	side   orderbook.Side
	price  decimal.Decimal
	volume decimal.Decimal
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
		fills:   make(map[string]decimal.Decimal),
	}
}

// priceKey is the canonical per-level identity. It uses the exact 18-digit
// decimal rendering so a level maps to a stable engine order ID across passes,
// and so two equal prices always collapse to one key.
func priceKey(side orderbook.Side, price decimal.Decimal) string {
	return string(side) + ":" + price.StringPrec(18)
}

func (s *Seeder) idPrefix() string { return "synth:" + s.cfg.Instrument + ":" }

// nextIDLocked mints a fresh, unique engine order ID for a level. The trailing
// generation counter is what lets fill accounting distinguish a placement from
// the order it replaced: a fill buffered against an old generation is ignored
// once the level is re-placed. Caller holds s.mu.
func (s *Seeder) nextIDLocked(side orderbook.Side, price decimal.Decimal) string {
	s.nonce++
	return s.idPrefix() + priceKey(side, price) + "#" + strconv.FormatUint(s.nonce, 10)
}

// OrderID returns the current engine order ID for a level, if one is resting.
// Useful for ops/inspection and tests that need to address a synthetic order.
func (s *Seeder) OrderID(side orderbook.Side, price decimal.Decimal) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.placed[priceKey(side, price)]
	if !ok {
		return "", false
	}
	return o.id, true
}

// OnTrade records a fill against a synthetic order. Wire it to the order
// book's "trade" hook. It writes only to the fills buffer (keyed by the full,
// generation-stamped order ID) under its own lock, so it never blocks on — or
// deadlocks with — an in-flight Converge, and a fill can never be applied to a
// different generation of the same price level.
func (s *Seeder) OnTrade(t *orderbook.Trade) {
	if t == nil || t.Instrument != s.cfg.Instrument {
		return
	}
	prefix := s.idPrefix()
	for _, id := range [...]string{t.BuyOrderID, t.SellOrderID} {
		if !strings.HasPrefix(id, prefix) {
			continue
		}
		s.fillMu.Lock()
		s.fills[id] = s.fills[id].Add(t.Volume)
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
// for levels still in the reference; levels that have left the reference are
// drained promptly (removed this pass, not decayed) to avoid a dust tail as
// the book moves. So alpha=1 snaps to the reference and 0<alpha<1 approaches
// it geometrically over repeated calls — the return-to-reference behavior.
// Pending fills are folded in first, so a
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
	refVol := make(map[string]decimal.Decimal, len(bids)+len(asks))
	side := make(map[string]orderbook.Side, len(bids)+len(asks))
	price := make(map[string]decimal.Decimal, len(bids)+len(asks))
	for _, l := range bids {
		k := priceKey(orderbook.SideBuy, l.Price)
		refVol[k], side[k], price[k] = l.Quantity, orderbook.SideBuy, l.Price
	}
	for _, l := range asks {
		k := priceKey(orderbook.SideSell, l.Price)
		refVol[k], side[k], price[k] = l.Quantity, orderbook.SideSell, l.Price
	}

	// Compute the per-level target over the union of placed and reference
	// levels. Stale levels (placed but absent from the reference) target zero.
	type plan struct {
		key     string
		side    orderbook.Side
		price   decimal.Decimal
		target  decimal.Decimal
		existed bool
		curID   string // engine ID of the order to cancel, if existed
	}
	plans := make(map[string]plan, len(refVol)+len(s.placed))
	for k := range refVol {
		cur, existed := s.placed[k]
		c := decimal.Zero
		curID := ""
		if existed {
			c, curID = cur.volume, cur.id
		}
		// target = c + alpha*(ref - c). alpha is the float convergence
		// fraction from the RTR controller; the step itself is lossy edge
		// scaling (MulFloat), which is exactly the intended use.
		target := c.Add(refVol[k].Sub(c).MulFloat(alpha))
		plans[k] = plan{key: k, side: side[k], price: price[k], target: target, existed: existed, curID: curID}
	}
	for k, cur := range s.placed {
		if _, ok := refVol[k]; ok {
			continue // already planned above
		}
		// Stale level (no longer in the reference, e.g. price moved past it):
		// drain it promptly — target 0 — rather than decaying geometrically,
		// which would leave a growing tail of dust orders as the book moves
		// ("drain stale synthetics first", PLAN §4). Only the volumes of
		// levels still in the reference converge gradually.
		plans[k] = plan{key: k, side: cur.side, price: cur.price, target: decimal.Zero, existed: true, curID: cur.id}
	}

	// Pass 1: cancel everything that must change (removed, drained, or
	// resized). A not-found order was already filled/cancelled away — benign.
	apply := make([]plan, 0, len(plans))
	for k, p := range plans {
		cur := s.placed[k]
		if p.existed && p.target.Eq(cur.volume) {
			continue // already at target; leave the resting order in place
		}
		if p.existed {
			if _, err := s.matcher.CancelOrder(ctx, s.cfg.Instrument, p.curID); err != nil && !errors.Is(err, orderbook.ErrOrderNotFound) {
				return st, fmt.Errorf("cancel %s: %w", p.curID, err)
			}
			delete(s.placed, k) // its generation is retired; buffered fills for curID now go stale
		}
		if p.target.Sign() <= 0 {
			if p.existed {
				st.Cancelled++
			}
			continue // nothing to (re-)place
		}
		apply = append(apply, p)
	}

	// Pass 2: place new/resized levels at their target volume with a fresh
	// generation ID. The tracked volume is the intended target; any fill
	// (including one during this very placement, e.g. crossing a resting user
	// order) is captured by OnTrade against the new ID and folded next pass —
	// the single source of fill truth, so no double counting.
	for _, p := range apply {
		id := s.nextIDLocked(p.side, p.price)
		ord := &orderbook.Order{
			ID:         id,
			Instrument: s.cfg.Instrument,
			Price:      p.price,
			Volume:     p.target,
			Side:       p.side,
			Metadata:   map[string]string{MetadataKey: MetadataValue},
		}
		trades, _, err := s.matcher.PlaceLimit(ctx, ord)
		if err != nil {
			return st, fmt.Errorf("place %s: %w", id, err)
		}
		st.Trades += len(trades)
		s.placed[p.key] = synthOrder{id: id, side: p.side, price: p.price, volume: p.target}
		if p.existed {
			st.Resized++
		} else {
			st.Placed++
		}
	}
	return st, nil
}

// foldFillsLocked applies buffered fills to the placed volumes. Fills are keyed
// by full (generation-stamped) order ID; a fill whose ID is no longer the
// current generation for its level is discarded, so a fill against a replaced
// order never decrements its successor. Caller holds s.mu.
func (s *Seeder) foldFillsLocked() {
	s.fillMu.Lock()
	fills := s.fills
	s.fills = make(map[string]decimal.Decimal)
	s.fillMu.Unlock()
	if len(fills) == 0 {
		return
	}
	byID := make(map[string]string, len(s.placed)) // current order ID -> levelKey
	for k, o := range s.placed {
		byID[o.id] = k
	}
	for id, v := range fills {
		k, ok := byID[id]
		if !ok {
			continue // stale generation (order was replaced/removed) — discard
		}
		cur := s.placed[k]
		cur.volume = cur.volume.Sub(v)
		if cur.volume.Sign() <= 0 {
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

// Run reconciles exactly (alpha=1) on every tick until ctx is cancelled — the
// "instant mirror" mode, with no return-to-reference easing. It is mutually
// exclusive with RTR.Run on the same seeder; use one driver or the other, not
// both (they would fight over the synthetic orders). Errors are logged, not
// fatal, so a transient engine error doesn't tear down seeding.
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
