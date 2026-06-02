package emulator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"

	"github.com/zeta1999/crypto-exchange-golang/internal/feed"
	"github.com/zeta1999/crypto-exchange-golang/internal/orderbook"
	"github.com/zeta1999/crypto-exchange-golang/pkg/decimal"
)

// TapeMetadataKey tags engine orders that originate from the real trade tape
// (Phase 5), distinct from synthetic seeded liquidity (MetadataKey).
const TapeMetadataKey = "tape"

// TapeReplay injects a venue's real executed trades into the engine so that
// resting orders — especially user limit orders — fill in sync with the live
// tape, as they would on the real venue. Each tape trade becomes a marketable
// order, side = the trade's aggressor, capped at the trade's price; the engine
// matches it against resting liquidity (user orders fill at their own price by
// price-time priority; synthetic liquidity absorbs the rest and is restored by
// the RTR controller). Any unfilled remainder is cancelled immediately so the
// tape order never lingers in the book.
type TapeReplay struct {
	matcher    Matcher
	instrument string

	mu  sync.Mutex
	seq uint64
}

// NewTapeReplay constructs a TapeReplay driving matcher for instrument.
func NewTapeReplay(matcher Matcher, instrument string) *TapeReplay {
	return &TapeReplay{matcher: matcher, instrument: instrument}
}

func (r *TapeReplay) nextID() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	return "tape:" + r.instrument + ":" + strconv.FormatUint(r.seq, 10)
}

// tradePrice/tradeQty prefer the venue's exact decimal strings, falling back to
// the float fields when a string isn't present.
func tradePrice(t *feed.Trade) decimal.Decimal {
	if d, err := decimal.Parse(t.PriceDecimal); err == nil {
		return d
	}
	return decimal.FromFloat(t.Price)
}

func tradeQty(t *feed.Trade) decimal.Decimal {
	if d, err := decimal.Parse(t.QuantityDecimal); err == nil {
		return d
	}
	return decimal.FromFloat(t.Quantity)
}

// Inject applies one tape trade to the engine and returns the resulting fills.
// Trades for other instruments, or with non-positive size, are ignored.
func (r *TapeReplay) Inject(ctx context.Context, t *feed.Trade) ([]*orderbook.Trade, error) {
	if t == nil || t.Instrument != r.instrument {
		return nil, nil
	}
	price, qty := tradePrice(t), tradeQty(t)
	if qty.Sign() <= 0 || price.Sign() <= 0 {
		return nil, nil
	}

	// The tape side is the aggressor: a "buy" print lifted asks, so we inject a
	// buy taker (matches asks up to price); a "sell" print hit bids.
	side := orderbook.SideBuy
	if t.Side == "sell" {
		side = orderbook.SideSell
	}

	id := r.nextID()
	ord := &orderbook.Order{
		ID:         id,
		Instrument: r.instrument,
		Price:      price,
		Volume:     qty,
		Side:       side,
		Metadata:   map[string]string{TapeMetadataKey: "true"},
	}
	trades, _, err := r.matcher.PlaceLimit(ctx, ord)
	if err != nil {
		return nil, fmt.Errorf("tape inject %s: %w", id, err)
	}
	// The tape order is a taker, not a standing order: cancel any unfilled
	// remainder so it doesn't rest in the book. Not-found means it filled
	// completely (or was already removed) — benign.
	if _, err := r.matcher.CancelOrder(ctx, r.instrument, id); err != nil && !errors.Is(err, orderbook.ErrOrderNotFound) {
		return trades, fmt.Errorf("tape cancel remainder %s: %w", id, err)
	}
	return trades, nil
}

// Run consumes feed events until ch is closed or ctx is cancelled, injecting
// each trade for this instrument in arrival order (real-time pacing — the live
// feed delivers trades at their natural rate). Accelerated/recorded pacing is
// handled by the Phase 7 trace-replay clock. Inject errors are logged, not
// fatal.
func (r *TapeReplay) Run(ctx context.Context, ch <-chan feed.Event) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if ev.Kind != feed.EventTrade || ev.Trade == nil {
				continue
			}
			if _, err := r.Inject(ctx, ev.Trade); err != nil {
				slog.Warn("tape inject error", "instrument", r.instrument, "error", err)
			}
		}
	}
}
