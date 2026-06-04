// Package options provides Black–Scholes pricing and greeks for European
// options, used by the emulator's options market-data surface (CR-9). It is a
// small, pure, allocation-light numerical core — no exchange types leak in.
//
// Conventions (documented because callers — notably Vivaldi's vivaldi-optdata —
// normalize these into their own greeks):
//   - vol is an annualized volatility (e.g. 0.65 = 65%).
//   - T is time to expiry in years (ACT/365).
//   - r is the continuously-compounded risk-free rate.
//   - delta/gamma are per unit of spot; vega is per 1.00 of vol (NOT per 1%);
//     theta is per CALENDAR DAY (so a long option's theta is negative and small).
//   - rho is intentionally NOT provided: Binance's options API (EAPI) does not
//     publish it, and this surface mimics EAPI. Deribit publishes rho; a Deribit
//     adapter can add it.
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

// Greeks bundles the sensitivities EAPI publishes (no rho — see package doc).
type Greeks struct {
	Price float64 // present value in quote currency
	Delta float64 // dPrice/dSpot
	Gamma float64 // d2Price/dSpot2
	Vega  float64 // dPrice/dVol, per 1.00 of vol
	Theta float64 // dPrice/dt, per calendar day (negative for long options)
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
	return greeks(kind, s, k, t, r, vol).Price
}

// Compute returns price + greeks for a European option. Always finite: degenerate
// inputs yield intrinsic value with zero greeks (never NaN/Inf), so a risk limit
// comparing `greek > cap` is never silently defeated by a NaN.
func Compute(kind Kind, s, k, t, r, vol float64) Greeks {
	return greeks(kind, s, k, t, r, vol)
}

func greeks(kind Kind, s, k, t, r, vol float64) Greeks {
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
	d1 := (math.Log(s/k) + (r+0.5*vol*vol)*t) / (vol * sqrtT)
	d2 := d1 - vol*sqrtT
	disc := math.Exp(-r * t)
	pdf := normPDF(d1)

	g := Greeks{
		Gamma: pdf / (s * vol * sqrtT),
		Vega:  s * pdf * sqrtT, // per 1.00 of vol
	}

	if kind == Call {
		nd1, nd2 := normCDF(d1), normCDF(d2)
		g.Price = s*nd1 - k*disc*nd2
		g.Delta = nd1
		// per-year theta, converted to per-day below
		g.Theta = -(s*pdf*vol)/(2*sqrtT) - r*k*disc*nd2
	} else {
		nmd1, nmd2 := normCDF(-d1), normCDF(-d2)
		g.Price = k*disc*nmd2 - s*nmd1
		g.Delta = -nmd1
		g.Theta = -(s*pdf*vol)/(2*sqrtT) + r*k*disc*nmd2
	}
	g.Theta /= 365.0 // per calendar day
	return g
}
