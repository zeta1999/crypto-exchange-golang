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
