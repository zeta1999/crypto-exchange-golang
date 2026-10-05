package optmarket

import (
	"fmt"
	"math"
	"time"

	"github.com/zeta1999/crypto-exchange-golang/pkg/options"
)

// IndexFunc returns the current spot index for an underlying pair (e.g.
// "BTCUSDT"), and false if that underlying is unknown/unpriced. In the running
// emulator this is wired to the matching engine's spot mid; tests inject a fixed
// value for deterministic fixtures.
type IndexFunc func(underlying string) (float64, bool)

// Smile is the vol at a strike. expiryYears is available so an equity smile
// can depend on tenor. The crypto quadratic ignores it.
type Smile interface {
	Vol(strike, forward, expiryYears float64) float64
}

// VolSurface is the crypto smile: vol as a quadratic in log-moneyness ln(K/F).
// Skew tilts it (crypto puts usually bid), Smile adds convexity. The result is
// floored so a wing never goes non-positive. It is the default Smile.
type VolSurface struct {
	ATMVol float64 // at-the-money vol (e.g. 0.65)
	Skew   float64 // d(vol)/d(ln K/F)
	Smile  float64 // curvature, >= 0
}

// Vol implements Smile. expiryYears is unused; equity smiles use it.
func (vs VolSurface) Vol(strike, forward, _ float64) float64 {
	if !(strike > 0) || !(forward > 0) {
		return math.Max(vs.ATMVol, 0.01)
	}
	m := math.Log(strike / forward)
	v := vs.ATMVol + vs.Skew*m + vs.Smile*m*m
	if v < 0.01 {
		v = 0.01
	}
	return v
}

// ForwardFunc replaces the carry forward for one underlying. Equity cash
// dividends plug in here. Nil means CarryForward.
type ForwardFunc func(spot, rate, dividendYield, years float64) float64

// CarryForward is S*exp((r-q)*T). q=0 is the crypto forward S*exp(rT).
func CarryForward(spot, rate, dividendYield, years float64) float64 {
	return spot * math.Exp((rate-dividendYield)*years)
}

// Mark is one priced option, venue-neutral. Adapters format it.
type Mark struct {
	Symbol                    string
	Price                     float64
	BidIV, AskIV, MarkIV      float64
	Delta, Gamma, Vega, Theta float64
	Rho                       float64
	HighPriceLimit            float64
	LowPriceLimit             float64
	Rate                      float64
}

// Level is one synthetic book level.
type Level struct {
	Price float64
	Size  float64
}

// Book is a synthetic order book around the mark.
type Book struct {
	TimeMs   int64
	UpdateID int64
	Bids     []Level
	Asks     []Level
}

// Contract is one listed option, venue-neutral.
type Contract struct {
	Symbol        string
	Underlying    string
	Quote         string
	Side          string // CALL / PUT
	Strike        float64
	ExpiryMs      int64
	Unit          int
	PriceScale    int
	QuantityScale int
}

// Spot is the index print for an underlying.
type Spot struct {
	Price  float64
	TimeMs int64
}

type leg struct {
	smile   Smile
	q       float64
	forward ForwardFunc
}

// Market is the options book. Construct with NewMarket, then AddInstrument /
// SetSurface. Venue adapters read marks; they do not price. All read methods
// are deterministic given the clock and the index source.
type Market struct {
	now      func() time.Time
	index    IndexFunc
	rate     float64 // continuously-compounded risk-free rate
	ivSpread float64 // bid/ask IV half-spread in vol points (e.g. 0.01)
	bookHalf float64 // synthetic-book half-spread as a fraction of mark (e.g. 0.02)
	priceCap float64 // price-limit band as a fraction of mark (e.g. 0.3)

	order    []Instrument // stable insertion order (deterministic output)
	bySymbol map[string]Instrument
	legs     map[string]leg
}

// NewMarket builds an empty market. ivSpread is in vol points; bookHalf and
// priceCap are fractions of mark. Dividend yield defaults to 0 on every
// underlying, which keeps the crypto forward and the Black price unchanged.
func NewMarket(now func() time.Time, index IndexFunc, rate, ivSpread, bookHalf, priceCap float64) *Market {
	if now == nil {
		now = time.Now
	}
	return &Market{
		now: now, index: index, rate: rate,
		ivSpread: ivSpread, bookHalf: bookHalf, priceCap: priceCap,
		bySymbol: map[string]Instrument{},
		legs:     map[string]leg{},
	}
}

// SetSurface assigns a smile to an underlying (e.g. "BTCUSDT").
// A nil smile is ignored. The dividend yield and forward hook already set
// on that underlying are kept.
func (m *Market) SetSurface(underlying string, smile Smile) {
	if smile == nil {
		return
	}
	lg := m.legs[underlying]
	lg.smile = smile
	m.legs[underlying] = lg
}

// SetDividendYield sets the continuous dividend yield q for an underlying.
// Crypto leaves this at 0.
func (m *Market) SetDividendYield(underlying string, q float64) {
	lg := m.legs[underlying]
	lg.q = q
	m.legs[underlying] = lg
}

// SetForward installs an equity forward. Nil restores CarryForward.
func (m *Market) SetForward(underlying string, fn ForwardFunc) {
	lg := m.legs[underlying]
	lg.forward = fn
	m.legs[underlying] = lg
}

// AddInstrument registers an instrument (idempotent by internal symbol).
func (m *Market) AddInstrument(in Instrument) {
	sym := in.Symbol()
	if _, ok := m.bySymbol[sym]; ok {
		return
	}
	m.bySymbol[sym] = in
	m.order = append(m.order, in)
}

// Instruments returns the registered instruments in insertion order.
func (m *Market) Instruments() []Instrument { return m.order }

// Find returns the listed instrument with this coin, strike, right, and
// settlement instant. Venue adapters parse their own symbol, then call Find.
func (m *Market) Find(coin string, strike float64, kind options.Kind, expiry time.Time) (Instrument, bool) {
	for _, in := range m.order {
		if in.Coin == coin && in.Kind == kind && in.Strike == strike && in.Expiry.Equal(expiry) {
			return in, true
		}
	}
	return Instrument{}, false
}

func (m *Market) smileFor(underlying string) Smile {
	if lg, ok := m.legs[underlying]; ok && lg.smile != nil {
		return lg.smile
	}
	return VolSurface{ATMVol: 0.5}
}

func (m *Market) yieldFor(underlying string) float64 {
	if lg, ok := m.legs[underlying]; ok {
		return lg.q
	}
	return 0
}

func (m *Market) forwardFor(underlying string, spot, years float64) float64 {
	q := m.yieldFor(underlying)
	if lg, ok := m.legs[underlying]; ok && lg.forward != nil {
		return lg.forward(spot, m.rate, q, years)
	}
	return CarryForward(spot, m.rate, q, years)
}

// computeMark prices one instrument off the current index.
func (m *Market) computeMark(in Instrument) (g options.Greeks, index, vol, t float64, err error) {
	index, ok := m.index(in.Underlying)
	if !ok || !(index > 0) {
		return options.Greeks{}, 0, 0, 0, fmt.Errorf("optmarket: no index for underlying %q", in.Underlying)
	}
	now := m.now()
	t = in.TimeToExpiry(now)
	q := m.yieldFor(in.Underlying)
	fwd := m.forwardFor(in.Underlying, index, t)
	vol = m.smileFor(in.Underlying).Vol(in.Strike, fwd, t)
	g = options.ComputeWithDividend(in.Kind, index, in.Strike, t, m.rate, q, vol)
	return g, index, vol, t, nil
}

func (m *Market) markOf(in Instrument) (Mark, error) {
	g, _, vol, _, err := m.computeMark(in)
	if err != nil {
		return Mark{}, err
	}
	bidIV := math.Max(vol-m.ivSpread, 0.001)
	askIV := vol + m.ivSpread
	hi := g.Price * (1 + m.priceCap)
	lo := math.Max(g.Price*(1-m.priceCap), 0)
	return Mark{
		Symbol:         in.Symbol(),
		Price:          g.Price,
		BidIV:          bidIV,
		AskIV:          askIV,
		MarkIV:         vol,
		Delta:          g.Delta,
		Theta:          g.Theta,
		Gamma:          g.Gamma,
		Vega:           g.Vega,
		Rho:            g.Rho,
		HighPriceLimit: hi,
		LowPriceLimit:  lo,
		Rate:           m.rate,
	}, nil
}

// Mark returns the priced option for one internal symbol.
func (m *Market) Mark(symbol string) (Mark, error) {
	in, ok := m.bySymbol[symbol]
	if !ok {
		return Mark{}, fmt.Errorf("optmarket: unknown option symbol %q", symbol)
	}
	return m.markOf(in)
}

// Marks returns a mark for every instrument that prices cleanly, in insertion
// order. Instruments whose underlying has no index are skipped.
func (m *Market) Marks() []Mark {
	out := make([]Mark, 0, len(m.order))
	for _, in := range m.order {
		if mk, err := m.markOf(in); err == nil {
			out = append(out, mk)
		}
	}
	return out
}

// Depth synthesizes an order book around the mark: `levels` price levels per
// side stepping out by the book half-spread, with linearly decaying size. A
// non-positive bid price (deep-OTM/near-expiry) drops the bid side rather than
// quoting a non-positive price.
func (m *Market) Depth(symbol string, levels int) (Book, error) {
	in, ok := m.bySymbol[symbol]
	if !ok {
		return Book{}, fmt.Errorf("optmarket: unknown option symbol %q", symbol)
	}
	g, _, _, _, err := m.computeMark(in)
	if err != nil {
		return Book{}, err
	}
	if levels < 1 {
		levels = 1
	}
	if levels > 50 {
		levels = 50
	}
	now := m.now()
	d := Book{TimeMs: now.UnixMilli(), UpdateID: now.UnixMilli()}
	mark := g.Price
	step := math.Max(mark*m.bookHalf, 0.0001)
	for i := 1; i <= levels; i++ {
		size := float64(levels-i+1) * 10.0
		bidPx := mark - float64(i)*step
		askPx := mark + float64(i)*step
		if bidPx > 0 {
			d.Bids = append(d.Bids, Level{Price: bidPx, Size: size})
		}
		d.Asks = append(d.Asks, Level{Price: askPx, Size: size})
	}
	return d, nil
}

// Contracts lists every registered option.
func (m *Market) Contracts() []Contract {
	out := make([]Contract, 0, len(m.order))
	for _, in := range m.order {
		side := "CALL"
		if in.Kind == options.Put {
			side = "PUT"
		}
		out = append(out, Contract{
			Symbol:        in.Symbol(),
			Underlying:    in.Underlying,
			Quote:         in.Quote,
			Side:          side,
			Strike:        in.Strike,
			ExpiryMs:      in.ExpiryMillis(),
			Unit:          1,
			PriceScale:    2,
			QuantityScale: 2,
		})
	}
	return out
}

// Index returns the spot index for an underlying pair.
func (m *Market) Index(underlying string) (Spot, error) {
	px, ok := m.index(underlying)
	if !ok || !(px > 0) {
		return Spot{}, fmt.Errorf("optmarket: no index for underlying %q", underlying)
	}
	return Spot{Price: px, TimeMs: m.now().UnixMilli()}, nil
}
