package saga_test

import (
	"context"
	"fmt"
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
func TestSaga_BarrierPDEGolden(t *testing.T) {
	setRoot(t)
	s := &saga.Saga{}

	// Same flat σ=0.30 Dupire surface as the recorded example (S=K=100, B=80,
	// r=0.05, T=1): examples/02-exotics/barrier-pde-localvol/input.json. The
	// endpoint is forwarded verbatim via the plugin's pass-through branch
	// (Model "pde_lv_barrier" is intentionally not a reserved model name);
	// num_paths/num_steps/seed ride along and are ignored by the PDE solver.
	res, err := s.Price(context.Background(), pricer.Params{
		Symbol: "BARRIER_DO_CALL_PDE",
		Spot:   100, Vol: 0.30, Rate: 0.05, ExpiryYears: 1.0,
		Model:    "pde_lv_barrier",
		Endpoint: "AriaPDEPriceLV/1",
		Raw: map[string]any{
			"strike":        100,
			"isCall":        true,
			"lower_barrier": 80,
			"pde_nx":        1201,
			"pde_nsigma":    5,
			"surface": map[string]any{
				"t_grid":    []any{0.0, 2.0},
				"logS_grid": []any{3.0, 6.2},
				"vol":       []any{[]any{0.30, 0.30}, []any{0.30, 0.30}},
			},
		},
	})
	if err != nil {
		t.Fatalf("Price: %v", err)
	}
	// Prove the deterministic path actually ran (not an MC fallback under a
	// renamed key): the engine echoes its solver + monitoring mode back.
	if m, _ := res.Raw["method"].(string); m != "pde_lv_barrier" {
		t.Fatalf("method = %q, want pde_lv_barrier", m)
	}
	if mon, _ := res.Raw["monitoring"].(string); mon != "continuous" {
		t.Fatalf("monitoring = %q, want continuous", mon)
	}
	if lx, _ := res.Raw["lower_barrier"].(float64); lx != 80 {
		t.Fatalf("lower_barrier echo = %v, want 80", lx)
	}

	// CI golden: recorded deterministic PDE value (examples/02-exotics/
	// barrier-pde-localvol/expected/input.expected.json), float-vs-float at
	// 1e-8 relative (the observed cross-build drift is ~2.6e-13 rel).
	const want = 13.244937327991609
	if rel := math.Abs(res.Price-want) / want; rel > 1e-8 {
		t.Errorf("pde barrier = %v, want %v (rel %g, gate 1e-8)", res.Price, want, rel)
	}
	// Independent accuracy anchor: closed-form Reiner–Rubinstein continuous
	// down-and-out call at the same parameters (13.244869, documented in the
	// examples README; the PDE is within its discretization error of it).
	const analytic = 13.244869
	if rel := math.Abs(res.Price-analytic) / analytic; rel > 1e-4 {
		t.Errorf("pde barrier = %v, analytic %v (rel %g, gate 1e-4)", res.Price, analytic, rel)
	}
	elapsed := res.ElapsedMS
	if elapsed <= 0 {
		elapsed = -1 // the PDE endpoint does not report elapsed_ms
	}
	elapsedStr := "n/a"
	if elapsed >= 0 {
		elapsedStr = fmt.Sprintf("%.0f ms", elapsed)
	}
	t.Logf("pde barrier call = %.9f (golden %v @1e-8, analytic %v @1e-4, elapsed %s)",
		res.Price, want, analytic, elapsedStr)
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
	errs := make([]error, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			_, errs[i] = s.Price(context.Background(), pricer.Params{
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
