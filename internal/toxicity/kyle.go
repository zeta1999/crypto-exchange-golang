// Package toxicity provides tape-induced market-toxicity estimators — Kyle's
// lambda (price impact per unit signed volume) and VPIN (volume-synchronized
// probability of informed trading) — over a rolling window of recent trades.
// The emulator's adverse-selection injector uses them to pick off resting user
// limit orders more often, and nearer unfavorable prices, when the market is
// toxic. All math is float64: these are statistical estimates, not prices.
package toxicity

// kyle estimates Kyle's lambda: the price impact per unit of signed order-flow
// volume, fit as a regression through the origin over a rolling window of
// (price change, signed volume) pairs. lambda = Σ(Δp·sv) / Σ(sv²).
type kyle struct {
	window int
	dp     []float64 // price changes
	sv     []float64 // signed volumes (buy +, sell −)
	last   float64
	have   bool
}

func newKyle(window int) *kyle {
	if window < 1 {
		window = 1
	}
	return &kyle{window: window}
}

// observe records a trade at price with signed volume sv (positive for a buy
// aggressor, negative for a sell).
func (k *kyle) observe(price, sv float64) {
	if k.have {
		k.dp = ringAppend(k.dp, price-k.last, k.window)
		k.sv = ringAppend(k.sv, sv, k.window)
	}
	k.last = price
	k.have = true
}

// lambda returns the current impact coefficient, clamped to be non-negative
// (informed buying pushes price up; a transient negative fit reads as 0).
func (k *kyle) lambda() float64 {
	var num, den float64
	for i := range k.dp {
		num += k.dp[i] * k.sv[i]
		den += k.sv[i] * k.sv[i]
	}
	if den == 0 {
		return 0
	}
	if l := num / den; l > 0 {
		return l
	}
	return 0
}

// ringAppend appends v to s, keeping at most the last n elements.
func ringAppend(s []float64, v float64, n int) []float64 {
	s = append(s, v)
	if len(s) > n {
		s = s[len(s)-n:]
	}
	return s
}
