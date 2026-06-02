package emulator

import (
	"context"
	"fmt"
	"math/rand"
	"sync"

	"github.com/zeta1999/crypto-exchange-golang/internal/feed"
	"github.com/zeta1999/crypto-exchange-golang/internal/orderbook"
	"github.com/zeta1999/crypto-exchange-golang/internal/reference"
	"github.com/zeta1999/crypto-exchange-golang/internal/toxicity"
	"github.com/zeta1999/crypto-exchange-golang/pkg/decimal"
)

// toxicSweepSize is intentionally large: the adverse sweep is an
// immediate-or-cancel order bounded by its price cap, not its size, so it
// clears all resting liquidity (synthetic + user) within the cap and discards
// the rest. Picking off whatever rests at/near the touch is the point.
var toxicSweepSize = decimal.FromInt(1_000_000_000)

// ToxicInjector models configurable market toxicity (PLAN [b]). It folds each
// tape trade into a toxicity.Model (Kyle's lambda + VPIN) and, with probability
// scale·Score, fires an adverse-selection sweep: a marketable IOC order in the
// informed-flow direction that penetrates the touch by scale·Impact, picking
// off resting user limit orders more often (Score) and nearer unfavorable
// prices (Impact) when the market is toxic. With Scale = 0 it is a pure no-op,
// so the emulator reduces to return-to-reference. Reproducible via the seed.
type ToxicInjector struct {
	matcher    IOCMatcher
	ref        *reference.Book
	model      *toxicity.Model
	instrument string

	mu  sync.Mutex
	rng *rand.Rand
	seq uint64
}

// NewToxicInjector builds an injector for instrument. Scale and the weights
// come from the model's config; seed makes the firing decisions reproducible.
func NewToxicInjector(matcher IOCMatcher, ref *reference.Book, model *toxicity.Model, instrument string, seed int64) *ToxicInjector {
	return &ToxicInjector{
		matcher:    matcher,
		ref:        ref,
		model:      model,
		instrument: instrument,
		rng:        rand.New(rand.NewSource(seed)),
	}
}

// Observe folds a tape trade into the model and, scaled by toxicity, may inject
// an adverse sweep. Returns any resulting fills (nil when nothing fired).
func (ti *ToxicInjector) Observe(ctx context.Context, t *feed.Trade) ([]*orderbook.Trade, error) {
	if t == nil || t.Instrument != ti.instrument {
		return nil, nil
	}
	var buy bool
	switch t.Side {
	case "buy":
		buy = true
	case "sell":
		buy = false
	default:
		return nil, nil
	}
	ti.model.Observe(t.Price, t.Quantity, buy)

	scale := ti.model.Config().Scale
	if scale <= 0 {
		return nil, nil // toxicity off → pure RTR
	}
	p := scale * ti.model.Score()
	ti.mu.Lock()
	fire := ti.rng.Float64() < p
	ti.mu.Unlock()
	if !fire {
		return nil, nil
	}
	return ti.sweep(ctx, buy, scale)
}

// sweep injects the adverse IOC order in the informed-flow direction, capped
// scale·Impact beyond the current touch.
func (ti *ToxicInjector) sweep(ctx context.Context, buy bool, scale float64) ([]*orderbook.Trade, error) {
	bid, ask, ok := ti.ref.BestBidAsk()
	if !ok {
		return nil, nil
	}
	offset := decimal.FromFloat(scale * ti.model.Impact()) // ≥ 0

	side := orderbook.SideBuy
	capPrice := ask.Price.Add(offset) // buy sweep lifts asks up to ask+offset
	if !buy {
		side = orderbook.SideSell
		capPrice = bid.Price.Sub(offset) // sell sweep hits bids down to bid−offset
		if capPrice.Sign() <= 0 {
			return nil, nil
		}
	}

	ti.mu.Lock()
	ti.seq++
	id := fmt.Sprintf("toxic:%s:%d", ti.instrument, ti.seq)
	ti.mu.Unlock()

	ord := &orderbook.Order{
		ID:         id,
		Instrument: ti.instrument,
		Price:      capPrice,
		Volume:     toxicSweepSize,
		Side:       side,
		// Tagged tape: emulator-injected aggressive flow, exempt from user margin.
		Metadata: map[string]string{TapeMetadataKey: "true"},
	}
	return ti.matcher.ExecuteIOC(ctx, ord)
}
