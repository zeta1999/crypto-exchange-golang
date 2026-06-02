package emulator

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"sync"

	"github.com/zeta1999/crypto-exchange-golang/internal/feed"
	"github.com/zeta1999/crypto-exchange-golang/internal/orderbook"
	"github.com/zeta1999/crypto-exchange-golang/pkg/decimal"
)

// TapeMetadataKey tags engine orders that originate from the real trade tape
// (Phase 5), distinct from synthetic seeded liquidity (MetadataKey).
const TapeMetadataKey = "tape"

// IOCMatcher executes a marketable order immediate-or-cancel (fills up to its
// price, never rests). *engine.Engine satisfies it via ExecuteIOC.
type IOCMatcher interface {
	ExecuteIOC(ctx context.Context, ord *orderbook.Order) ([]*orderbook.Trade, error)
}

// TapeReplay injects a venue's real executed trades into the engine so that
// resting orders — especially user limit orders — fill in sync with the live
// tape, as they would on the real venue. Each tape trade becomes an IOC order,
// side = the trade's aggressor, capped at the trade's price; the engine matches
// it against resting liquidity (user orders fill at their own price by
// price-time priority; synthetic liquidity absorbs the rest and is restored by
// the RTR controller) and discards any unfilled remainder atomically, so the
// tape order is never visible as resting liquidity.
type TapeReplay struct {
	matcher    IOCMatcher
	instrument string

	mu  sync.Mutex
	seq uint64
}

// NewTapeReplay constructs a TapeReplay driving matcher for instrument.
func NewTapeReplay(matcher IOCMatcher, instrument string) *TapeReplay {
	return &TapeReplay{matcher: matcher, instrument: instrument}
}

func (r *TapeReplay) nextID() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	return "tape:" + r.instrument + ":" + strconv.FormatUint(r.seq, 10)
}

// tradePrice/tradeQty prefer the venue's exact decimal strings, falling back to
// the float field only when finite (strconv.ParseFloat yields NaN/±Inf without
// error, and the venue string can be "NaN"; FromFloat would panic on those).
func tradePrice(t *feed.Trade) decimal.Decimal {
	if d, err := decimal.Parse(t.PriceDecimal); err == nil {
		return d
	}
	if math.IsNaN(t.Price) || math.IsInf(t.Price, 0) {
		return decimal.Zero
	}
	return decimal.FromFloat(t.Price)
}

func tradeQty(t *feed.Trade) decimal.Decimal {
	if d, err := decimal.Parse(t.QuantityDecimal); err == nil {
		return d
	}
	if math.IsNaN(t.Quantity) || math.IsInf(t.Quantity, 0) {
		return decimal.Zero
	}
	return decimal.FromFloat(t.Quantity)
}

// Inject applies one tape trade to the engine and returns the resulting fills.
// Trades for other instruments, with non-positive size/price, or with an
// unrecognized aggressor side are ignored.
func (r *TapeReplay) Inject(ctx context.Context, t *feed.Trade) ([]*orderbook.Trade, error) {
	if t == nil || t.Instrument != r.instrument {
		return nil, nil
	}
	var side orderbook.Side
	switch t.Side {
	case "buy":
		side = orderbook.SideBuy // aggressor lifted asks
	case "sell":
		side = orderbook.SideSell // aggressor hit bids
	default:
		return nil, nil // unknown side — drop rather than guess
	}

	price, qty := tradePrice(t), tradeQty(t)
	if qty.Sign() <= 0 || price.Sign() <= 0 {
		return nil, nil
	}

	ord := &orderbook.Order{
		ID:         r.nextID(),
		Instrument: r.instrument,
		Price:      price,
		Volume:     qty,
		Side:       side,
		Metadata:   map[string]string{TapeMetadataKey: "true"},
	}
	trades, err := r.matcher.ExecuteIOC(ctx, ord)
	if err != nil {
		return nil, fmt.Errorf("tape inject %s: %w", ord.ID, err)
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
