package optmarket

import (
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/zeta1999/crypto-exchange-golang/pkg/options"
)

// IndexFunc returns the current spot index for an underlying pair (e.g.
// "BTCUSDT"), and false if that underlying is unknown/unpriced. In the running
// emulator this is wired to the matching engine's spot mid; tests inject a fixed
// value for deterministic fixtures.
type IndexFunc func(underlying string) (float64, bool)

// VolSurface is a deterministic per-underlying smile: vol as a quadratic in
// log-moneyness ln(K/F). Skew tilts it (crypto puts usually bid), Smile adds
// convexity. The result is floored so a wing never goes non-positive.
type VolSurface struct {
	ATMVol float64 // at-the-money vol (e.g. 0.65)
	Skew   float64 // d(vol)/d(ln K/F)
	Smile  float64 // curvature, >= 0
}

// Vol returns the implied vol at `strike` given the forward `fwd`.
func (vs VolSurface) Vol(strike, fwd float64) float64 {
	if !(strike > 0) || !(fwd > 0) {
		return math.Max(vs.ATMVol, 0.01)
	}
	m := math.Log(strike / fwd)
	v := vs.ATMVol + vs.Skew*m + vs.Smile*m*m
	if v < 0.01 {
		v = 0.01
	}
	return v
}

// Market is the options market-data surface. Construct with NewMarket, then
// AddInstrument / SetSurface. All read methods are deterministic given the clock
// + index source.
type Market struct {
	now      func() time.Time
	index    IndexFunc
	rate     float64 // continuously-compounded risk-free rate
	ivSpread float64 // bid/ask IV half-spread in vol points (e.g. 0.01)
	bookHalf float64 // synthetic-book half-spread as a fraction of mark (e.g. 0.02)
	priceCap float64 // price-limit band as a fraction of mark (e.g. 0.3)

	order    []Instrument // stable insertion order (deterministic output)
	bySymbol map[string]Instrument
	surfaces map[string]VolSurface // per underlying
}

// NewMarket builds an empty market. ivSpread is in vol points; bookHalf and
// priceCap are fractions of mark.
func NewMarket(now func() time.Time, index IndexFunc, rate, ivSpread, bookHalf, priceCap float64) *Market {
	if now == nil {
		now = time.Now
	}
	return &Market{
		now: now, index: index, rate: rate,
		ivSpread: ivSpread, bookHalf: bookHalf, priceCap: priceCap,
		bySymbol: map[string]Instrument{},
		surfaces: map[string]VolSurface{},
	}
}

// SetSurface assigns a vol surface to an underlying (e.g. "BTCUSDT").
func (m *Market) SetSurface(underlying string, vs VolSurface) { m.surfaces[underlying] = vs }

// AddInstrument registers an instrument (idempotent by symbol).
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

func (m *Market) surfaceFor(underlying string) VolSurface {
	if vs, ok := m.surfaces[underlying]; ok {
		return vs
	}
	return VolSurface{ATMVol: 0.5} // sane flat default
}

// ---- EAPI-shaped wire types (string-encoded numbers, like Binance) ----

// MarkData is one element of GET /eapi/v1/mark. No `rho` — Binance EAPI omits it.
type MarkData struct {
	Symbol           string `json:"symbol"`
	MarkPrice        string `json:"markPrice"`
	BidIV            string `json:"bidIV"`
	AskIV            string `json:"askIV"`
	MarkIV           string `json:"markIV"`
	Delta            string `json:"delta"`
	Theta            string `json:"theta"`
	Gamma            string `json:"gamma"`
	Vega             string `json:"vega"`
	HighPriceLimit   string `json:"highPriceLimit"`
	LowPriceLimit    string `json:"lowPriceLimit"`
	RiskFreeInterest string `json:"riskFreeInterest"`
}

// Depth is GET /eapi/v1/depth.
type Depth struct {
	T    int64       `json:"T"` // message time (epoch ms)
	U    int64       `json:"u"` // update id
	Bids [][2]string `json:"bids"`
	Asks [][2]string `json:"asks"`
}

// OptionSymbolInfo is one optionSymbols entry of GET /eapi/v1/exchangeInfo.
type OptionSymbolInfo struct {
	Symbol        string `json:"symbol"`
	Underlying    string `json:"underlying"`
	QuoteAsset    string `json:"quoteAsset"`
	StrikePrice   string `json:"strikePrice"`
	ExpiryDate    int64  `json:"expiryDate"`
	Side          string `json:"side"` // CALL / PUT
	Unit          int    `json:"unit"`
	PriceScale    int    `json:"priceScale"`
	QuantityScale int    `json:"quantityScale"`
}

// ExchangeInfo is GET /eapi/v1/exchangeInfo (options subset).
type ExchangeInfo struct {
	Timezone      string             `json:"timezone"`
	ServerTime    int64              `json:"serverTime"`
	OptionSymbols []OptionSymbolInfo `json:"optionSymbols"`
}

// IndexData is GET /eapi/v1/index.
type IndexData struct {
	IndexPrice string `json:"indexPrice"`
	Time       int64  `json:"time"`
}

// fstr formats a float as a fixed-precision decimal string (deterministic across
// platforms — important for recorded fixtures). Binance uses 8dp on EAPI.
func fstr(f float64) string { return strconv.FormatFloat(f, 'f', 8, 64) }

// computeMark prices one instrument off the current index. Returns the greeks,
// the index, and the implied vol used.
func (m *Market) computeMark(in Instrument) (g options.Greeks, index, vol, t float64, err error) {
	index, ok := m.index(in.Underlying)
	if !ok || !(index > 0) {
		return options.Greeks{}, 0, 0, 0, fmt.Errorf("optmarket: no index for underlying %q", in.Underlying)
	}
	now := m.now()
	t = in.TimeToExpiry(now)
	fwd := index * math.Exp(m.rate*t)
	vol = m.surfaceFor(in.Underlying).Vol(in.Strike, fwd)
	g = options.Compute(in.Kind, index, in.Strike, t, m.rate, vol)
	return g, index, vol, t, nil
}

// Mark returns the EAPI mark record for one instrument symbol.
func (m *Market) Mark(symbol string) (MarkData, error) {
	in, ok := m.bySymbol[symbol]
	if !ok {
		return MarkData{}, fmt.Errorf("optmarket: unknown option symbol %q", symbol)
	}
	return m.markOf(in)
}

func (m *Market) markOf(in Instrument) (MarkData, error) {
	g, _, vol, _, err := m.computeMark(in)
	if err != nil {
		return MarkData{}, err
	}
	bidIV := math.Max(vol-m.ivSpread, 0.001)
	askIV := vol + m.ivSpread
	hi := g.Price * (1 + m.priceCap)
	lo := math.Max(g.Price*(1-m.priceCap), 0)
	return MarkData{
		Symbol:           in.Symbol(),
		MarkPrice:        fstr(g.Price),
		BidIV:            fstr(bidIV),
		AskIV:            fstr(askIV),
		MarkIV:           fstr(vol),
		Delta:            fstr(g.Delta),
		Theta:            fstr(g.Theta),
		Gamma:            fstr(g.Gamma),
		Vega:             fstr(g.Vega),
		HighPriceLimit:   fstr(hi),
		LowPriceLimit:    fstr(lo),
		RiskFreeInterest: fstr(m.rate),
	}, nil
}

// MarkAll returns mark records for every instrument that prices cleanly, in
// insertion order. Instruments whose underlying has no index are skipped.
func (m *Market) MarkAll() []MarkData {
	out := make([]MarkData, 0, len(m.order))
	for _, in := range m.order {
		if md, err := m.markOf(in); err == nil {
			out = append(out, md)
		}
	}
	return out
}

// Depth synthesizes an order book around the mark: `levels` price levels per
// side stepping out by the book half-spread, with linearly decaying size. A
// non-positive bid price (deep-OTM/near-expiry) drops the bid side rather than
// quoting a non-positive price.
func (m *Market) Depth(symbol string, levels int) (Depth, error) {
	in, ok := m.bySymbol[symbol]
	if !ok {
		return Depth{}, fmt.Errorf("optmarket: unknown option symbol %q", symbol)
	}
	g, _, _, _, err := m.computeMark(in)
	if err != nil {
		return Depth{}, err
	}
	if levels < 1 {
		levels = 1
	}
	if levels > 50 {
		levels = 50
	}
	now := m.now()
	d := Depth{T: now.UnixMilli(), U: now.UnixMilli(), Bids: [][2]string{}, Asks: [][2]string{}}
	mark := g.Price
	step := math.Max(mark*m.bookHalf, 0.0001) // absolute price step per level
	for i := 1; i <= levels; i++ {
		qty := fstr(float64(levels-i+1) * 10.0) // 10*levels at top, decaying
		bidPx := mark - float64(i)*step
		askPx := mark + float64(i)*step
		if bidPx > 0 {
			d.Bids = append(d.Bids, [2]string{fstr(bidPx), qty})
		}
		d.Asks = append(d.Asks, [2]string{fstr(askPx), qty})
	}
	return d, nil
}

// ExchangeInfo lists every registered option symbol with its contract spec.
func (m *Market) ExchangeInfo() ExchangeInfo {
	now := m.now()
	syms := make([]OptionSymbolInfo, 0, len(m.order))
	for _, in := range m.order {
		side := "CALL"
		if in.Kind == options.Put {
			side = "PUT"
		}
		syms = append(syms, OptionSymbolInfo{
			Symbol:        in.Symbol(),
			Underlying:    in.Underlying,
			QuoteAsset:    in.Quote,
			StrikePrice:   strconv.FormatFloat(in.Strike, 'f', -1, 64),
			ExpiryDate:    in.ExpiryMillis(),
			Side:          side,
			Unit:          1,
			PriceScale:    2,
			QuantityScale: 2,
		})
	}
	return ExchangeInfo{Timezone: "UTC", ServerTime: now.UnixMilli(), OptionSymbols: syms}
}

// Index returns the spot index for an underlying pair.
func (m *Market) Index(underlying string) (IndexData, error) {
	px, ok := m.index(underlying)
	if !ok || !(px > 0) {
		return IndexData{}, fmt.Errorf("optmarket: no index for underlying %q", underlying)
	}
	return IndexData{IndexPrice: fstr(px), Time: m.now().UnixMilli()}, nil
}
