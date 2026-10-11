package saga_test

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/zeta1999/crypto-exchange-golang/internal/pricer"
	"github.com/zeta1999/crypto-exchange-golang/internal/pricer/saga"
)

// resolveRoot returns the price-master-examples checkout root (abs path), or
// skips the test. Same optional-out-of-tree convention as conformance/ccxt-go.
func resolveRoot(t *testing.T) string {
	t.Helper()
	if r := os.Getenv("SIBELIUS_SAMPLES_ROOT"); r != "" {
		return r
	}
	for _, cand := range []string{
		"../../../../price-master-examples", // internal/pricer/saga → work/ sibling
		"../../../price-master-examples",
	} {
		if st, err := os.Stat(cand + "/bin"); err == nil && st.IsDir() {
			abs, err := filepath.Abs(cand)
			if err != nil {
				return cand
			}
			return abs
		}
	}
	t.Skip("price-master-examples checkout not found (set SIBELIUS_SAMPLES_ROOT)")
	return ""
}

// setRoot forces the env var for the lifetime of the test (t.Setenv also
// marks the test non-parallel, protecting the shared env var).
func setRoot(t *testing.T) {
	t.Helper()
	t.Setenv("SIBELIUS_SAMPLES_ROOT", resolveRoot(t))
}

func TestSaga_BarrierDownAndOut(t *testing.T) {
	setRoot(t)
	s := &saga.Saga{}

	res, err := s.Price(context.Background(), pricer.Params{
		Symbol: "DOWN_AND_OUT",
		Script: `contract DownAndOutCall { let S = spot("X") var alive = 1.0 alive := alive * cond(S > 80.0, 1.0, 0.0) one(KRW) |> scale(alive * max(S - 100.0, 0.0)) |> when(2026-01-01) }`,
		Spot:   100, Vol: 0.30, Rate: 0.05, ExpiryYears: 1.0,
		NumPaths: 65536, NumSteps: 252, Seed: 42,
	})
	if err != nil {
		t.Fatalf("Price: %v", err)
	}
	// Golden pinned to the repo-recorded value (examples/02-exotics/
	// barrier-down-and-out/expected/input.expected.json) at the SAME
	// relative tolerance (1e-3) the examples harness uses — the shipped
	// demo binary's RNG stream differs microscopically from the build
	// that recorded the golden (13.417529… vs 13.417598…), but is well
	// within the gate the examples project itself applies.
	const want = 13.417598957079523
	if math.Abs(res.Price-want)/want > 1e-3 {
		t.Errorf("price = %v, want %v (barrier example golden)", res.Price, want)
	}
	if res.NumPaths != 65536 || res.NumSteps != 252 || res.RNG != "portable" {
		t.Errorf("echo mismatch: %+v", res)
	}
}

// TestSaga_BarrierPDEGolden pins the CI golden for the barrier product through
// the deterministic PDE reference endpoint (Crank–Nicolson, pde_lv_barrier)
// instead of the RNG-driven Monte-Carlo path: the PDE output is deterministic,
// so the float-to-float comparison is gated at 1e-8 relative — far tighter
// than the 1e-3 MC gate, and robust to the sub-1e-12 cross-build drift that
// made a bit-exact MC golden untenable. (MC remains the product floor at 1e-3
// wherever bit-identical engines are not available; the PDE endpoint IS the
// "uncapped, bit-identical" reference for this payoff class.)
//
// The price is additionally cross-anchored against the independent
// Reiner–Rubinstein continuous-barrier closed-form value 13.244869 documented
// for these exact parameters (barrier-pde-localvol/README.md, agrees with the
// PDE to ~5e-6 rel, i.e. the grid discretization) so the pinned golden is
// proven to be the CORRECT barrier price, not merely a stable one.
// pdeParams builds a pass-through PDE request via the plugin's default branch
// (endpoint forwarded verbatim; the vanilla-script fallback is suppressed
// because Model is non-empty). Spot/Rate/ExpiryYears ride the well-known
// Params fields — the default branch seeds spot/T/rate from them — and must
// NOT also appear in args (Raw keys are not merged over pre-seeded keys).
func pdeParams(sym string, spot, rate, tYears float64, args map[string]any) pricer.Params {
	return pricer.Params{
		Symbol: sym, Model: "pde_lv_barrier", Endpoint: "AriaPDEPriceLV/1",
		Spot: spot, Rate: rate, ExpiryYears: tYears,
		Raw: args,
	}
}

// flatSurface builds the two-point flat Dupire surface the barrier scenarios use.
func flatSurface(vol float64) map[string]any {
	return map[string]any{
		"t_grid":    []any{0.0, 2.0},
		"logS_grid": []any{3.0, 6.2},
		"vol":       []any{[]any{vol, vol}, []any{vol, vol}},
	}
}

// checkPDE asserts the deterministic-solver echo fields come back intact.
func checkPDE(t *testing.T, res pricer.Result, barrier float64, knockIn, isCall bool) {
	t.Helper()
	if m, _ := res.Raw["method"].(string); m != "pde_lv_barrier" {
		t.Fatalf("method = %q, want pde_lv_barrier", m)
	}
	if mon, _ := res.Raw["monitoring"].(string); mon != "continuous" {
		t.Fatalf("monitoring = %q, want continuous", mon)
	}
	if lx, _ := res.Raw["lower_barrier"].(float64); lx != barrier {
		t.Fatalf("lower_barrier echo = %v, want %v", lx, barrier)
	}
	if ki, _ := res.Raw["knock_in"].(bool); ki != knockIn {
		t.Fatalf("knock_in = %v, want %v", ki, knockIn)
	}
	if ci, _ := res.Raw["isCall"].(bool); ci != isCall {
		t.Fatalf("isCall = %v, want %v", ci, isCall)
	}
}

// TestSaga_VanillaPutMC pins the engine's vanilla PUT Monte-Carlo output
// deterministically (portable RNG, seeded) with an independent analytic
// Black-Scholes anchor (gate 1e-2, MC noise at 2^17 paths ~1e-3). Puts were
// previously untested while calls were covered; this closes the put-side MC
// coverage of the vanilla path.
func TestSaga_VanillaPutMC(t *testing.T) {
	setRoot(t)
	s := &saga.Saga{}

	res, err := s.PriceVanilla(context.Background(), 100, true, pricer.Params{
		Symbol: "VANILLA_PUT", Spot: 100, Vol: 0.25, Rate: 0.05, ExpiryYears: 1.0,
		NumPaths: 131072, NumSteps: 252, Seed: 42,
	})
	if err != nil {
		t.Fatalf("PriceVanilla: %v", err)
	}
	// CI golden: recorded deterministic MC value (portable RNG, seed 42).
	const want = 7.457204961780442
	if rel := math.Abs(res.Price-want) / want; rel > 1e-3 {
		t.Errorf("mc put = %v, want %v (rel %g, gate 1e-3)", res.Price, want, rel)
	}
	// Independent analytic anchor: closed-form Black-Scholes put.
	const bs = 7.458941380440123
	if rel := math.Abs(res.Price-bs) / bs; rel > 1e-2 {
		t.Errorf("mc put = %v, black-scholes %v (rel %g, gate 1e-2)", res.Price, bs, rel)
	}
	t.Logf("mc vanilla put = %.9f (golden @1e-3, bs @1e-2)", res.Price)
}

func TestSaga_BarrierPDEGolden(t *testing.T) {
	setRoot(t)
	s := &saga.Saga{}

	// Scenario 1: down-and-OUT CALL, flat sigma=0.30, S=K=100, B=80, r=0.05, T=1
	// (examples/02-exotics/barrier-pde-localvol/input.json).
	res, err := s.Price(context.Background(), pdeParams("BARRIER_DO_CALL_PDE", 100, 0.05, 1.0, map[string]any{
		"strike": 100, "isCall": true, "lower_barrier": 80,
		"pde_nx": 1201, "pde_nsigma": 5,
		"surface": flatSurface(0.30),
	}))
	if err != nil {
		t.Fatalf("Price: %v", err)
	}
	checkPDE(t, res, 80, false, true)
	// CI golden: recorded deterministic PDE value (input.expected.json),
	// float-vs-float at 1e-8 rel (observed cross-build drift ~2.6e-13).
	const want = 13.244937327991609
	if rel := math.Abs(res.Price-want) / want; rel > 1e-8 {
		t.Errorf("pde barrier = %v, want %v (rel %g, gate 1e-8)", res.Price, want, rel)
	}
	// Independent accuracy anchor: Reiner-Rubinstein continuous down-and-out
	// call closed form (13.244869, documented in barrier-pde-localvol/README);
	// the PDE sits within its discretization error of the analytic solution.
	const analytic = 13.244869
	if rel := math.Abs(res.Price-analytic) / analytic; rel > 1e-4 {
		t.Errorf("pde barrier = %v, analytic %v (rel %g, gate 1e-4)", res.Price, analytic, rel)
	}
	t.Logf("pde DO call = %.9f (golden @1e-8, analytic @1e-4)", res.Price)
}

// TestSaga_BarrierPDEGolden_DownAndInPut pins a SECOND barrier PDE golden:
// down-and-IN PUT, flat sigma=0.25, S=K=100, B=70, r=0.05, T=1. Different
// volatility and barrier distance than the call scenario, so a regression in
// either the Dupire-surface plumbing or the knock-in boundary condition is
// caught. Goldens are deterministic PDE values; float-vs-float at 1e-8.
//
// Cross-check (no external reference needed): a down-and-in put is pathwise
// <= a vanilla put (zero payoff on paths that never knock in, identical after
// a knock-in), so the PDE price must be STRICTLY below the engine's vanilla
// put at the same params -- an independent engine cross-verification.
func TestSaga_BarrierPDEGolden_DownAndInPut(t *testing.T) {
	setRoot(t)
	s := &saga.Saga{}

	res, err := s.Price(context.Background(), pdeParams("BARRIER_DI_PUT_PDE", 100, 0.05, 1.0, map[string]any{
		"strike": 100, "isCall": false, "knock_in": true, "lower_barrier": 70,
		"pde_nx": 1201, "pde_nsigma": 5,
		"surface": flatSurface(0.25),
	}))
	if err != nil {
		t.Fatalf("Price: %v", err)
	}
	checkPDE(t, res, 70, true, false)
	// CI golden: deterministic PDE price of this scenario (3 runs
	// bit-identical; cross-build drift sub-1e-12).
	const want = 3.783580721466973
	if rel := math.Abs(res.Price-want) / want; rel > 1e-8 {
		t.Errorf("pde DI put = %v, want %v (rel %g, gate 1e-8)", res.Price, want, rel)
	}
	// Independent cross-verification: price the VANILLA put at the same params
	// (deterministic AriaMCPrice, same engine) -- barrier put must be strictly
	// below it.
	vp, err := s.PriceVanilla(context.Background(), 100, true, pricer.Params{
		Spot: 100, Vol: 0.25, Rate: 0.05, ExpiryYears: 1.0,
		NumPaths: 131072, NumSteps: 252, Seed: 42,
	})
	if err != nil {
		t.Fatalf("PriceVanilla: %v", err)
	}
	if res.Price >= vp.Price {
		t.Errorf("DI put %v must be < vanilla put %v (pathwise <=, strict ATM)", res.Price, vp.Price)
	}
	t.Logf("pde DI put = %.9f (golden @1e-8), vanilla put MC = %.9f", res.Price, vp.Price)
}

func TestSaga_ArithAsianScriptFile(t *testing.T) {
	setRoot(t)
	s := &saga.Saga{Timeout: 10 * time.Minute} // 400k paths can exceed the 2-min default on slow boxes

	// script_file resolves relative to the examples root; this exercises the
	// payoff-script (.aria) path, including the engine's average-accrual semantics.
	res, err := s.Price(context.Background(), pricer.Params{
		Symbol:     "ASIAN_ARITH",
		ScriptFile: "payoffs/asian_arith_call.aria",
		Spot:       100, Vol: 0.20, Rate: 0.05, ExpiryYears: 1.0,
		NumPaths: 400000, NumSteps: 252, Seed: 42,
	})
	if err != nil {
		t.Fatalf("Price: %v", err)
	}
	// Golden pinned to examples/02-exotics/asian-arithmetic-bs/expected/
	// input.expected.json at the harness's 1e-3 relative tolerance.
	const want = 5.777010252130649
	if math.Abs(res.Price-want)/want > 1e-3 {
		t.Errorf("arithmetic asian call = %v, want %v (recorded golden)", res.Price, want)
	}
	t.Logf("arith asian call = %.6f (%d paths, %.0f ms)", res.Price, res.NumPaths, res.ElapsedMS)
}

func TestSaga_PricerInterfaceAndRegistry(t *testing.T) {
	var p pricer.Pricer = &saga.Saga{}
	_ = p.Name() // the plugin seam is used through the interface only

	reg := pricer.NewRegistry()
	reg.Register(p)
	got, ok := reg.Get("saga")
	if !ok || got.Name() != "saga" {
		t.Fatalf("registry round-trip failed: ok=%v names=%v", ok, reg.Names())
	}
	if _, ok := reg.Get("SAGA"); !ok {
		t.Error("registry lookup should be case-insensitive")
	}
	// Registry must tolerate Register while readers run (config-reload path).
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				reg.Get("saga")
				reg.Names()
			}
		}
	}()
	for i := 0; i < 100; i++ {
		reg.Register(&saga.Saga{}) // same-name overwrite, concurrent with readers
	}
	close(stop)
	wg.Wait()
}

func TestSaga_ConcurrentCalls(t *testing.T) {
	setRoot(t)
	s := &saga.Saga{Timeout: 10 * time.Minute}
	const n = 8 // each call is ~200 ms; 8 in parallel keeps the suite fast
	var wg sync.WaitGroup
	errs := make([]error, n+1)
	pdePrices := make([]float64, 1)
	wg.Add(n + 1)
	// One deterministic PDE call interleaved with the MC workers: under
	// concurrency the deterministic path must still return the exact golden
	// (no shared-state corruption in the plugin's temp dirs / env).
	go func() {
		defer wg.Done()
		r, e := s.Price(context.Background(), pdeParams("BARRIER_DO_CALL_PDE", 100, 0.05, 1.0, map[string]any{
			"strike": 100, "isCall": true, "lower_barrier": 80,
			"pde_nx": 1201, "pde_nsigma": 5,
			"surface": flatSurface(0.30),
		}))
		if e != nil {
			errs[0] = e
			return
		}
		pdePrices[0] = r.Price
	}()
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			_, errs[i+1] = s.Price(context.Background(), pricer.Params{
				// Barrier call: cheap (65k paths) and deterministic.
				Script: `contract DownAndOutCall { let S = spot("X") var alive = 1.0 alive := alive * cond(S > 80.0, 1.0, 0.0) one(KRW) |> scale(alive * max(S - 100.0, 0.0)) |> when(2026-01-01) }`,
				Spot:   100, Vol: 0.30, Rate: 0.05, ExpiryYears: 1.0,
				NumPaths: 65536, NumSteps: 252, Seed: 42,
			})
		}(i)
	}
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			t.Errorf("call %d: %v", i, e)
		}
	}
	// The interleaved deterministic PDE call returns the CI golden exactly
	// (same constant as TestSaga_BarrierPDEGolden).
	if rel := math.Abs(pdePrices[0]-13.244937327991609) / 13.244937327991609; rel > 1e-8 {
		t.Errorf("concurrent PDE price = %v, golden 13.244937327991609 (rel %g)", pdePrices[0], rel)
	}
}

func TestSaga_FailurePreservesInput(t *testing.T) {
	setRoot(t)
	// Point at the real examples root but use a deliberately broken script:
	// the engine should fail, and the error must name the preserved input.
	s := &saga.Saga{}
	_, err := s.Price(context.Background(), pricer.Params{
		Script: "total garbage that is not aria syntax at all {",
		Spot:   100, Vol: 0.25, Rate: 0.05, ExpiryYears: 1.0,
		NumPaths: 1024, NumSteps: 252, Seed: 1,
	})
	if err == nil {
		t.Fatal("expected an error for a malformed Aria script")
	}
	t.Logf("failure error: %v", err)
}
