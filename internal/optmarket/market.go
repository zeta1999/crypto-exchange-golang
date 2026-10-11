package optmarket

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/zeta1999/crypto-exchange-golang/internal/pricer"
	"github.com/zeta1999/crypto-exchange-golang/pkg/options"
)

// defaultExoticTTL is the per-exotic pricing cache interval when the caller
// does not set one. A saga call is a subprocess, so marks for an exotic are
// re-priced at most once per interval no matter how often the market data is
// requested.
const defaultExoticTTL = 5 * time.Second

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
//
// For VANILLA contracts the price is Black–Scholes and BidIV/AskIV/MarkIV +
// the greeks are populated; Bid/Ask are 0 (the synthetic book is what the
// venue quotes two-sided). For FLOW-EXOTIC contracts the price is the
// pricer's single fair value, Bid/Ask bracket it (the executable quote), and
// the IV/greeks fields are 0 — an exotic is not a Black–Scholes instrument
// and no implied volatility or greek is invented for it.
type Mark struct {
	Symbol string
	Price  float64
	// Two-sided executable quote. Vanilla: 0 (use Depth). Exotic: fair±spread.
	Bid                       float64
	Ask                       float64
	BidIV, AskIV, MarkIV      float64
	Delta, Gamma, Vega, Theta float64
	Rho                       float64
	HighPriceLimit            float64
	LowPriceLimit             float64
	Rate                      float64
	// Exotic provenance (empty for vanillas) so a consumer can tell how the
	// number was produced. Pricer is the backend name, Model the engine model.
	Pricer string
	Model  string
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

// Market is the options book. Construct with NewMarket, then AddInstrument /
// SetSurface. Venue adapters read marks; they do not price. All read methods
// are deterministic given the clock and the index source — EXCEPT exotic
// pricing, which shells out to the attached Pricer (the saga backend is a
// subprocess). Exotic results are cached per contract for the configured
// TTL so a mark flood makes O(1) engine calls per interval, and the book
// never stalls serving vanillas while an engine is slow.
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

	// Flow-exotics (dealer-quoting mode): attached via AttachPricer. Vanilla
	// contracts price off the Black–Scholes core regardless; exotics price
	// off the registered backend against their ACTUAL payoff. Both sets may
	// be empty — a nil sharedModel means "no exotics configured" and
	// AddExotic/Mark return an error rather than panicking or fabricating.
	reg         *pricer.Registry
	pricerName  string
	sharedModel pricer.Params // base Params (Vol/Rate/NumPaths/NumSteps/Seed/Model/RNG); per-exotic specs override Vol/Model
	exotics     map[string]Exotic
	exoticOrder []string // stable exotic insertion order (deterministic marks/contracts)
	exoticTTL   time.Duration
	exoticMax   float64 // risk limit: a pricer fair value above this is NOT quoted (0 = no limit)
	exoticHalf  float64 // exotic bid/ask half-spread as a FRACTION of fair value; 0 = fall back to bookHalf

	exoticMu    sync.RWMutex // guards exoticBySym (the Market itself is immutable after build)
	exoticBySym map[string]exoticCacheEntry
}

type leg struct {
	smile   Smile
	q       float64
	forward ForwardFunc
}

// exoticCacheEntry holds one priced exotic for its TTL. A nil entry means
// "priced, but not quotable right now" (no index, pricer error, or the risk
// limit rejected it) — negative caching so a broken exotic cannot spawn an
// engine call per mark request.
type exoticCacheEntry struct {
	mk      Mark
	at      time.Time
	failErr error // non-nil => negative cache (Mark returns it; Marks skips it)
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
		bySymbol: map[string]Instrument{}, legs: map[string]leg{},
		exotics: map[string]Exotic{}, exoticBySym: map[string]exoticCacheEntry{},
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

// ---------------------------------------------------------------------------
// Flow exotics (dealer-quoting mode)
//
// Vanilla contracts stay on the Black–Scholes path, untouched. Exotics get
// one Mark per underlying index per ttl: each exotic's ACTUAL configured
// payoff (.aria script or file) is priced ONCE per interval by the registered
// Pricer backend — there is no vanilla-leg decomposition for a barrier /
// Asian / autocall, so there is none here. The executable quote brackets the
// single fair value by the market's book half-spread:
//
//	bid   = fair · (1 − bookHalf)
//	offer = fair · (1 + bookHalf)
//
// with the same price-limit band (priceCap) as vanillas. Because the MC
// output is the input, exotic marks carry NO implied vol or greeks — those
// are Black–Scholes artifacts, and inventing them would be exactly the
// "price the exotic from vanilla legs" mistake; the fields stay 0 and the
// EAPI exotic endpoint surfaces `pricer` + `model` instead.
//
// Risk limit: with exoticMaxFairValue > 0 a fair value above the limit is a
// REJECTED quote (negatively cached: the broken/outrageous payoff cannot
// spawn an engine call per request). Non-finite and non-positive fair values
// are always rejected — no free instruments.

// AttachPricer enables exotic dealer quoting on this book. reg must contain
// a pricer named pricerName (case-insensitive); shared is the per-exotic base
// model (Vol, Rate, NumPaths, NumSteps, Seed, RNG, Model — see pricer.Params;
// per-exotic specs override Vol/Model); ttl is the per-exotic cache interval
// (non-positive ttl means defaultExoticTTL); maxFairValue is the risk limit
// (0 = none); halfSpreadFrac is the bid/ask half-spread as a fraction of fair
// value (0 = use the market's bookHalf).
func (m *Market) AttachPricer(reg *pricer.Registry, pricerName string, shared pricer.Params, ttl time.Duration, maxFairValue, halfSpreadFrac float64) error {
	if reg == nil {
		return fmt.Errorf("optmarket: AttachPricer: nil registry")
	}
	if _, ok := reg.Get(pricerName); !ok {
		return fmt.Errorf("optmarket: pricer %q not registered (have %v)", pricerName, reg.Names())
	}
	if ttl <= 0 {
		ttl = defaultExoticTTL
	}
	m.reg = reg
	m.pricerName = pricerName
	m.sharedModel = shared
	m.exoticTTL = ttl
	m.exoticMax = maxFairValue
	if halfSpreadFrac > 0 {
		m.exoticHalf = halfSpreadFrac
	}
	return nil
}

// exoticHalfFrac is the half-spread (fraction of fair) used to bracket an
// exotic's bid/ask: the configured fraction, else the market's bookHalf.
func (m *Market) exoticHalfFrac() float64 {
	if m.exoticHalf > 0 {
		return m.exoticHalf
	}
	return m.bookHalf
}

// HasPricer reports whether exotic dealer quoting is attached.
func (m *Market) HasPricer() bool { return m.reg != nil }

// AddExotic registers one flow exotic (idempotent by venue symbol). The
// venue symbol's last token must not be a bare C/P (that is the side token
// on every vanilla symbol — collision would break the venue parsers).
func (m *Market) AddExotic(ex Exotic) error {
	sym := strings.TrimSpace(ex.VenueSymbol)
	if sym == "" {
		return fmt.Errorf("optmarket: exotic with empty venue symbol")
	}
	if last := sym[strings.LastIndex(sym, "-")+1:]; last == "C" || last == "P" {
		return fmt.Errorf("optmarket: exotic venue symbol %q ends in %q, which parses as a vanilla side", sym, last)
	}
	if strings.HasSuffix(sym, "-") {
		return fmt.Errorf("optmarket: exotic venue symbol %q ends in a dash", sym)
	}
	if ex.Payoff == "" && ex.PayoffFile == "" {
		return fmt.Errorf("optmarket: exotic %q has neither payoff nor payoff_file", sym)
	}
	if ex.Payoff != "" && ex.PayoffFile != "" {
		return fmt.Errorf("optmarket: exotic %q sets both payoff and payoff_file (pick one)", sym)
	}
	if ex.Strike <= 0 {
		return fmt.Errorf("optmarket: exotic %q has a non-positive strike", sym)
	}
	if ex.Underlying == "" {
		return fmt.Errorf("optmarket: exotic %q has no underlying index", sym)
	}
	if _, dup := m.exotics[sym]; dup {
		return nil // idempotent
	}
	m.exotics[sym] = ex
	m.exoticOrder = append(m.exoticOrder, sym)
	return nil
}

// ExoticSymbols lists registered exotic venue symbols in insertion order.
func (m *Market) ExoticSymbols() []string { return append([]string(nil), m.exoticOrder...) }

// exotic is the cached-or-stale lookup used by Mark/Depth: it refreshes the
// entry when it is absent or older than exoticTTL, under the write lock.
// Negative results (pricer error / limit rejected / no index) are cached too,
// for the full TTL.
func (m *Market) exotic(sym string) (Mark, error) {
	now := m.now()
	m.exoticMu.RLock()
	e, ok := m.exoticBySym[sym]
	m.exoticMu.RUnlock()
	if ok && now.Sub(e.at) < m.exoticTTL {
		if e.failErr != nil {
			return Mark{}, e.failErr
		}
		return e.mk, nil
	}
	mk, err := m.priceExoticNow(sym)
	entry := exoticCacheEntry{mk: mk, at: now}
	if err != nil {
		entry.failErr = err
	}
	m.exoticMu.Lock()
	m.exoticBySym[sym] = entry
	m.exoticMu.Unlock()
	if err != nil {
		return Mark{}, err
	}
	return mk, nil
}

// priceExoticNow is one engine call for one exotic. It builds Params from the
// shared model, applies the per-exotic vol/model overrides, derives a
// positive vol (override → shared vol → underlying surface at the strike),
// and reads the underlying index as spot. It clears Strike/Side so no
// backend can fabricate a vanilla payoff from them — the script IS the
// instrument.
func (m *Market) priceExoticNow(sym string) (Mark, error) {
	ex, ok := m.exotics[sym]
	if !ok {
		return Mark{}, fmt.Errorf("optmarket: unknown exotic %q", sym)
	}
	index, ok := m.index(ex.Underlying)
	if !ok || !(index > 0) {
		return Mark{}, fmt.Errorf("optmarket: no index for exotic %q's underlying %q", sym, ex.Underlying)
	}
	t := ex.TimeToExpiry(m.now())
	if t <= 0 {
		return Mark{}, fmt.Errorf("optmarket: exotic %q is expired", sym)
	}

	p := m.sharedModel
	p.Symbol = sym
	p.Spot = index
	p.Script = ex.Payoff
	p.ScriptFile = ex.PayoffFile
	if ex.Model != "" {
		p.Model = ex.Model
	}
	if ex.Vol > 0 {
		p.Vol = ex.Vol
	} else if p.Vol <= 0 {
		// Inherit from the underlying surface (same vol the vanilla chain
		// at this strike would see) — a positive, defensible vol.
		fwd := m.forwardFor(ex.Underlying, index, t)
		p.Vol = m.smileFor(ex.Underlying).Vol(ex.Strike, fwd, t)
	}
	p.Strike = 0 // the payoff script defines the instrument
	p.Side = ""

	pc, ok := m.reg.Get(m.pricerName)
	if !ok {
		return Mark{}, fmt.Errorf("optmarket: pricer %q not registered", m.pricerName)
	}
	res, err := pc.Price(context.Background(), p)
	if err != nil {
		return Mark{}, fmt.Errorf("optmarket: pricer %q: %w", m.pricerName, err)
	}
	fair := res.Price
	if math.IsNaN(fair) || math.IsInf(fair, 0) {
		return Mark{}, fmt.Errorf("optmarket: pricer %q returned non-finite price for %q", m.pricerName, sym)
	}
	if !(fair > 0) {
		return Mark{}, fmt.Errorf("optmarket: pricer %q returned non-positive fair value for %q (payoff may be identically zero at this spot)", m.pricerName, sym)
	}
	if m.exoticMax > 0 && fair > m.exoticMax {
		return Mark{}, fmt.Errorf("optmarket: exotic %q fair value %.8g exceeds risk limit %.8g — not quoted", sym, fair, m.exoticMax)
	}

	// Bracket the single fair value with the exotic half-spread.
	half := m.exoticHalfFrac()
	bid := fair * (1 - half)
	if bid < 1e-9 {
		bid = 1e-9
	}
	model := strings.ToLower(p.Model)
	if model == "" {
		// The saga backend (the only one this surface uses) treats an empty
		// model as its Monte-Carlo BS endpoint; say so in the provenance.
		model = "bs"
	}
	return Mark{
		Symbol: sym,
		Price:  fair,
		Delta:  0, Gamma: 0, Vega: 0, Theta: 0, Rho: 0, // not a BS instrument: no IV, no greeks
		HighPriceLimit: fair * (1 + m.priceCap),
		LowPriceLimit:  math.Max(fair*(1-m.priceCap), 0),
		Rate:           m.rate,
		Bid:            bid,
		Ask:            fair * (1 + half),
		Pricer:         m.pricerName,
		Model:          model,
	}, nil
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

// Mark returns the priced option (or exotic) for one venue symbol.
// Returns a negative-cache error while an exotic's price is stale-failed.
func (m *Market) Mark(symbol string) (Mark, error) {
	if _, isExotic := m.exotics[symbol]; isExotic {
		return m.exotic(symbol)
	}
	in, ok := m.bySymbol[symbol]
	if !ok {
		return Mark{}, fmt.Errorf("optmarket: unknown option symbol %q", symbol)
	}
	return m.markOf(in)
}

// Marks returns a mark for every instrument that prices cleanly, in insertion
// order. Vanillas come first (registration order), then exotics. Instruments
// whose underlying has no index are skipped, as are exotics currently
// rejected by the pricer or the risk limit.
func (m *Market) Marks() []Mark {
	out := make([]Mark, 0, len(m.order)+len(m.exoticOrder))
	for _, in := range m.order {
		if mk, err := m.markOf(in); err == nil {
			out = append(out, mk)
		}
	}
	for _, sym := range m.exoticOrder {
		if mk, err := m.exotic(sym); err == nil {
			out = append(out, mk)
		}
	}
	return out
}

// Depth synthesizes an order book. Vanillas: `levels` price levels per side
// stepping out by the book half-spread, with linearly decaying size; a
// non-positive bid price (deep-OTM/near-expiry) drops the bid side rather
// than quoting a non-positive price. Exotics: the executable two-sided quote
// itself — one level per side at the cached mark's Bid/Ask (size 10) — since
// an MC price has no ladder to step out of.
func (m *Market) Depth(symbol string, levels int) (Book, error) {
	if _, isExotic := m.exotics[symbol]; isExotic {
		mk, err := m.exotic(symbol)
		if err != nil {
			return Book{}, err
		}
		now := m.now()
		d := Book{TimeMs: now.UnixMilli(), UpdateID: now.UnixMilli()}
		if mk.Bid > 0 {
			d.Bids = []Level{{Price: mk.Bid, Size: 10}}
		}
		if mk.Ask > 0 {
			d.Asks = []Level{{Price: mk.Ask, Size: 10}}
		}
		return d, nil
	}
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
	return m.synthesizeBook(g.Price, levels), nil
}

// synthesizeBook builds the vanilla ladder around a mark price: `levels`
// price levels per side stepping out by the book half-spread, with linearly
// decaying size.
func (m *Market) synthesizeBook(mark float64, levels int) Book {
	now := m.now()
	d := Book{TimeMs: now.UnixMilli(), UpdateID: now.UnixMilli()}
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
	return d
}

// Contracts lists every registered option and exotic.
func (m *Market) Contracts() []Contract {
	out := make([]Contract, 0, len(m.order)+len(m.exoticOrder))
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
	for _, sym := range m.exoticOrder {
		ex := m.exotics[sym]
		side := "CALL"
		if ex.Kind == options.Put {
			side = "PUT"
		}
		out = append(out, Contract{
			Symbol:        sym,
			Underlying:    ex.Underlying,
			Quote:         ex.Quote,
			Side:          side,
			Strike:        ex.Strike,
			ExpiryMs:      ex.ExpiryMillis(),
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
