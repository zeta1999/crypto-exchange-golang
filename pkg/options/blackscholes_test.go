package options

import (
	"math"
	"testing"
)

func approx(t *testing.T, name string, got, want, tol float64) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Errorf("%s = %.6f, want %.6f (tol %.0e)", name, got, want, tol)
	}
}

// The canonical textbook vector (same anchor as Vivaldi's vivaldi-pricing BS
// reference and Sibelius's Price-Master vectors): S=K=100, vol=20%, r=5%, T=1.
func TestBlackScholes_TextbookCall(t *testing.T) {
	g := Compute(Call, 100, 100, 1, 0.05, 0.20)
	approx(t, "pv", g.Price, 10.4506, 1e-3)
	approx(t, "delta", g.Delta, 0.6368, 1e-3)
	approx(t, "gamma", g.Gamma, 0.018762, 1e-5)
	approx(t, "vega", g.Vega, 37.524, 1e-2) // per 1.00 vol
	// theta per day = per-year(-6.414)/365
	approx(t, "theta/day", g.Theta, -6.414/365.0, 1e-4)
}

func TestBlackScholes_PutCallParity(t *testing.T) {
	s, k, tT, r, v := 100.0, 110.0, 0.5, 0.03, 0.30
	c := Price(Call, s, k, tT, r, v)
	p := Price(Put, s, k, tT, r, v)
	// C - P = S - K e^{-rT}
	approx(t, "parity", c-p, s-k*math.Exp(-r*tT), 1e-9)
}

func TestBlackScholes_DeltaBounds(t *testing.T) {
	// call delta in (0,1); put delta in (-1,0); deep ITM/OTM limits.
	if d := Compute(Call, 200, 100, 1, 0.05, 0.2).Delta; d <= 0.95 {
		t.Errorf("deep-ITM call delta %.4f should approach 1", d)
	}
	if d := Compute(Call, 50, 100, 1, 0.05, 0.2).Delta; d >= 0.05 {
		t.Errorf("deep-OTM call delta %.4f should approach 0", d)
	}
	if d := Compute(Put, 50, 100, 1, 0.05, 0.2).Delta; d >= -0.95 {
		t.Errorf("deep-ITM put delta %.4f should approach -1", d)
	}
}

func TestBlackScholes_GammaVegaSymmetry(t *testing.T) {
	// gamma and vega are identical for a call and a put at the same strike.
	c := Compute(Call, 100, 105, 0.75, 0.02, 0.4)
	p := Compute(Put, 100, 105, 0.75, 0.02, 0.4)
	approx(t, "gamma sym", c.Gamma, p.Gamma, 1e-12)
	approx(t, "vega sym", c.Vega, p.Vega, 1e-12)
}

func TestBlackScholes_DegenerateInputsAreFiniteIntrinsic(t *testing.T) {
	cases := []struct {
		name             string
		kind             Kind
		s, k, tt, r, vol float64
		wantPrice        float64
	}{
		{"expired ITM call", Call, 120, 100, 0, 0.05, 0.2, 20},
		{"expired OTM call", Call, 80, 100, 0, 0.05, 0.2, 0},
		{"expired ITM put", Put, 80, 100, 0, 0.05, 0.2, 20},
		{"zero vol ITM call", Call, 120, 100, 1, 0.0, 0.0, 20},
		{"negative T", Call, 100, 100, -1, 0.05, 0.2, 0},
	}
	for _, c := range cases {
		g := Compute(c.kind, c.s, c.k, c.tt, c.r, c.vol)
		for _, x := range []float64{g.Price, g.Delta, g.Gamma, g.Vega, g.Theta} {
			if math.IsNaN(x) || math.IsInf(x, 0) {
				t.Fatalf("%s: non-finite greek in %+v", c.name, g)
			}
		}
		approx(t, c.name+" price", g.Price, c.wantPrice, 1e-9)
	}
}

// Near-expiry prices a tiny but finite value with large finite gamma (no NaN).
func TestBlackScholes_NearExpiryFinite(t *testing.T) {
	g := Compute(Call, 100, 105, 0.001, 0.04, 0.15)
	if g.Price < 0 || math.IsNaN(g.Price) || math.IsInf(g.Gamma, 0) {
		t.Fatalf("near-expiry not finite: %+v", g)
	}
}
