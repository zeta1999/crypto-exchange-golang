// Package options provides Black–Scholes pricing and greeks for European
// options, used by the emulator's options market-data surface (CR-9). It is a
// small, pure, allocation-light numerical core — no exchange types leak in.
//
// Conventions (documented because callers — notably Vivaldi's vivaldi-optdata —
// normalize these into their own greeks):
//   - vol is an annualized volatility (e.g. 0.65 = 65%).
//   - T is time to expiry in years (ACT/365).
//   - r is the continuously-compounded risk-free rate.
//   - q is a continuous dividend yield. Crypto passes 0. A non-zero q is the
//     equity hook: the forward is S*exp((r-q)*T) and the Black-Scholes terms
//     pick up the discount on the spot. q=0 is the historical formula, bit for bit.
//   - delta/gamma are per unit of spot; vega is per 1.00 of vol (NOT per 1%);
//     theta is per CALENDAR DAY (so a long option's theta is negative and small).
//   - rho is dPrice/dr per 1.00 of rate. Binance EAPI does not publish it; the
//     Binance adapter omits the field. Deribit does publish it.
package options

import "math"

// Kind is the option right.
type Kind int

const (
	Call Kind = iota
	Put
)

func (k Kind) String() string {
	if k == Put {
		return "PUT"
	}
	return "CALL"
}

// Greeks bundles price and sensitivities. Rho is dPrice/dr. Venue adapters
// decide whether to publish it.
type Greeks struct {
	Price float64 // present value in quote currency
	Delta float64 // dPrice/dSpot
	Gamma float64 // d2Price/dSpot2
	Vega  float64 // dPrice/dVol, per 1.00 of vol
	Theta float64 // dPrice/dt, per calendar day (negative for long options)
	Rho   float64 // dPrice/dr, per 1.00 of rate
}

// normCDF is the standard-normal CDF via the complementary error function
// (accurate and branch-free, no table).
func normCDF(x float64) float64 {
	return 0.5 * math.Erfc(-x/math.Sqrt2)
}

// normPDF is the standard-normal density.
func normPDF(x float64) float64 {
	return math.Exp(-0.5*x*x) / math.Sqrt(2*math.Pi)
}

// Price returns the Black–Scholes present value of a European option.
// Degenerate inputs (T<=0, vol<=0, S<=0, K<=0) collapse to (undiscounted)
// intrinsic value rather than NaN, so a near/at-expiry instrument still quotes
// sanely (at T=0 there is nothing to discount).
func Price(kind Kind, s, k, t, r, vol float64) float64 {
	return greeks(kind, s, k, t, r, 0, vol).Price
}

// Compute returns price + greeks for a European option with no dividend yield.
// Always finite: degenerate inputs yield intrinsic value with zero greeks
// (never NaN/Inf), so a risk limit comparing `greek > cap` is never silently
// defeated by a NaN.
func Compute(kind Kind, s, k, t, r, vol float64) Greeks {
	return greeks(kind, s, k, t, r, 0, vol)
}

// ComputeWithDividend is Compute with a continuous dividend yield q.
// q=0 matches Compute.
func ComputeWithDividend(kind Kind, s, k, t, r, q, vol float64) Greeks {
	return greeks(kind, s, k, t, r, q, vol)
}

func greeks(kind Kind, s, k, t, r, q, vol float64) Greeks {
	// At/after expiry or with no vol/spot/strike: discounted intrinsic, flat greeks.
	if !(t > 0) || !(vol > 0) || !(s > 0) || !(k > 0) {
		intrinsic := 0.0
		if kind == Call {
			intrinsic = math.Max(s-k, 0)
		} else {
			intrinsic = math.Max(k-s, 0)
		}
		g := Greeks{Price: intrinsic}
		// Delta is the only non-zero greek at expiry (a step at the strike).
		if kind == Call && s > k {
			g.Delta = 1
		} else if kind == Put && s < k {
			g.Delta = -1
		}
		return g
	}

	sqrtT := math.Sqrt(t)
	// b = r-q. q=0 leaves d1 on the historical risk-neutral drift.
	d1 := (math.Log(s/k) + (r-q+0.5*vol*vol)*t) / (vol * sqrtT)
	d2 := d1 - vol*sqrtT
	discR := math.Exp(-r * t)
	discQ := math.Exp(-q * t)
	pdf := normPDF(d1)
	spotPdf := s * discQ * pdf

	g := Greeks{
		Gamma: discQ * pdf / (s * vol * sqrtT),
		Vega:  spotPdf * sqrtT, // per 1.00 of vol
	}

	if kind == Call {
		nd1, nd2 := normCDF(d1), normCDF(d2)
		g.Price = s*discQ*nd1 - k*discR*nd2
		g.Delta = discQ * nd1
		// per-year theta, converted to per-day below
		g.Theta = -(spotPdf*vol)/(2*sqrtT) - r*k*discR*nd2 + q*s*discQ*nd1
		g.Rho = k * t * discR * nd2
	} else {
		nmd1, nmd2 := normCDF(-d1), normCDF(-d2)
		g.Price = k*discR*nmd2 - s*discQ*nmd1
		g.Delta = -discQ * nmd1
		g.Theta = -(spotPdf*vol)/(2*sqrtT) + r*k*discR*nmd2 - q*s*discQ*nmd1
		g.Rho = -k * t * discR * nmd2
	}
	g.Theta /= 365.0 // per calendar day
	return g
}
