// Package saga is the "saga" flow-exotics pricing plugin: it drives the
// Sibelius/Aria `SibeliusSaga` demo binary (from ../price-master-examples)
// as an external valuation engine, speaking its JSON CLI.
//
// The binary contract (see ../price-master-examples/README.md):
//
//	SibeliusSaga <input.json> <out.json>
//
// where input.json is `{"name":"<Endpoint>/1","arguments":[{...}]}` and out.json
// is a JSON object whose `price` is the value. Monte-Carlo examples must set
// `"rng":"portable"` to be bit-reproducible across platforms, and examples
// that reference payoff scripts (`script_file`) must be run FROM the examples
// repo root — the adapter sets cmd.Dir to that root for the call.
//
// The repo root is never hardcoded: resolution order is
//
//  1. Saga.Root (set by the caller)
//  2. the SIBELIUS_SAMPLES_ROOT env var
//  3. price-master-examples relative to the module root
//     (works for `go run` and installed binaries under this checkout)
//
// and the binary path likewise (Saga.Bin, then SAGA_BIN, then
// <root>/bin/SibeliusSaga-* platform probe, then ../Sibelius/build_release).
package saga

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/zeta1999/crypto-exchange-golang/internal/pricer"
)

const name = "saga"

// maxCaptureBytes caps engine stdout/stderr retention: the binary is known to
// spew a large symbol table on some failure modes, and only a tail is ever
// surfaced — keep the last cap bytes and drop the head so a broken engine
// cannot fill memory.
const maxCaptureBytes = 64 << 10

// maxOutBytes caps the engine's out.json read (engine-controlled size).
const maxOutBytes = 1 << 20

// Saga prices with the SibeliusSaga external binary. Safe for concurrent use —
// each Price call gets a private temp dir and the struct is immutable after
// construction (register-once, then frozen).
type Saga struct {
	// Root is the price-master-examples checkout root. Empty = auto-resolve.
	Root string
	// Bin is the SibeliusSaga binary. Empty = auto-resolve under Root.
	Bin string
	// Timeout caps one engine call (default 2 min). Large path counts can
	// exceed this — raise it per-instance, e.g. &Saga{Timeout: 10 * time.Minute}.
	Timeout time.Duration
	// Surface is the optional default Dupire local-vol surface used when Model
	// selects local-vol and the caller supplies none (Params.Raw["surface"]).
	// Without it, local-vol pricing falls back to a flat square at p.Vol
	// (which must be > 0 in either case — a fabricated vol is never priced).
	Surface map[string]any
}

// cappedWriter keeps the tail of a stream, bounded to maxCaptureBytes.
type cappedWriter struct {
	buf bytes.Buffer
}

func (w *cappedWriter) Write(b []byte) (int, error) {
	w.buf.Write(b)                             // bytes.Buffer is single-goroutine per stream;
	if n := w.buf.Len(); n > maxCaptureBytes { // os/exec copies each *non-file*
		w.buf.Next(n - maxCaptureBytes) // writer in its OWN goroutine — never share one.
	}
	return len(b), nil
}

func (w *cappedWriter) String() string {
	b := w.buf.Bytes()
	if len(b) > 2000 {
		return "…" + string(b[len(b)-2000:])
	}
	return string(b)
}

// Name implements pricer.Pricer.
func (s *Saga) Name() string { return name }

// Probe reports whether this instance can actually start the engine binary
// (root + binary resolve), without launching it. It returns the resolved
// (root, bin) and a nil error when ready; a non-nil error names the first
// thing missing. Callers use it to degrade gracefully (or fail fast) BEFORE
// a pricing request would discover the problem — e.g. the exchange's
// dealer-quoting mode. Resolution is identical to Price.
func (s *Saga) Probe() (root, bin string, err error) {
	root = s.Root
	if root == "" {
		root = os.Getenv("SIBELIUS_SAMPLES_ROOT")
	}
	if root == "" {
		if root, err = defaultRoot(); err != nil {
			return "", "", err
		}
	}
	bin = s.Bin
	if bin == "" {
		bin = os.Getenv("SAGA_BIN")
	}
	if bin == "" {
		if bin, err = defaultBin(root); err != nil {
			return "", "", err
		}
	}
	return root, bin, nil
}

// Price implements pricer.Pricer. On engine failure the temp dir holding the
// exact request payload is preserved and its path reported, so a failed
// deterministic request can be replayed; it is removed only on success.
func (s *Saga) Price(ctx context.Context, p pricer.Params) (pricer.Result, error) {
	root := s.Root
	if root == "" {
		root = os.Getenv("SIBELIUS_SAMPLES_ROOT")
	}
	if root == "" {
		var err error
		if root, err = defaultRoot(); err != nil {
			return pricer.Result{}, fmt.Errorf("saga: %w", err)
		}
	}
	// Absolutize: cmd.Dir changes the CWD, which would re-resolve a relative
	// root (and hence the binary path) against the examples checkout. A
	// failure to absolutize means a relative path would be resolved against
	// the PROCESS cwd after the chdir — exactly the hazard we are avoiding,
	// so it is fatal.
	if !filepath.IsAbs(root) {
		a, err := filepath.Abs(root)
		if err != nil {
			return pricer.Result{}, fmt.Errorf("saga: cannot absolutize Root %q: %w", root, err)
		}
		root = a
	}
	bin := s.Bin
	if bin != "" && !filepath.IsAbs(bin) {
		a, err := filepath.Abs(bin)
		if err != nil {
			return pricer.Result{}, fmt.Errorf("saga: cannot absolutize Bin %q: %w", bin, err)
		}
		bin = a
	}
	if bin == "" {
		bin = os.Getenv("SAGA_BIN")
	}
	if bin == "" {
		var err error
		if bin, err = defaultBin(root); err != nil {
			return pricer.Result{}, fmt.Errorf("saga: %w", err)
		}
	}
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	endpoint, args, err := buildRequest(withDefaultSurface(s, p))
	if err != nil {
		return pricer.Result{}, err
	}

	tmp, err := os.MkdirTemp("", "saga-*")
	if err != nil {
		return pricer.Result{}, err
	}
	keepTmp := true
	defer func() {
		if keepTmp {
			return // preserved; path is in the error
		}
		os.RemoveAll(tmp)
	}()
	inPath := filepath.Join(tmp, "input.json")
	outPath := filepath.Join(tmp, "out.json")

	payload, err := json.Marshal(map[string]any{"name": endpoint, "arguments": []any{args}})
	if err != nil {
		return pricer.Result{}, err
	}
	if err := os.WriteFile(inPath, payload, 0o644); err != nil {
		return pricer.Result{}, err
	}

	cmd := exec.CommandContext(ctx, bin, inPath, outPath)
	// Payoff scripts are referenced by relative path; the examples must run
	// from the checkout root.
	cmd.Dir = root
	// Separate bounded writers: os/exec starts one copy goroutine per
	// non-file writer, so sharing one buffer across stdout+stderr would race.
	var outCap, errCap cappedWriter
	cmd.Stdout = &outCap
	cmd.Stderr = &errCap
	runErr := cmd.Run()
	if runErr != nil {
		if ctx.Err() != nil {
			kind := "cancelled"
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				kind = fmt.Sprintf("timed out after %v", timeout)
			}
			return pricer.Result{}, fmt.Errorf("saga: %s: %w (input preserved at %s)", kind, ctx.Err(), inPath)
		}
		return pricer.Result{}, fmt.Errorf("saga: %s failed: %v\nengine output: %s\n(input preserved at %s)",
			filepath.Base(bin), runErr, errCap.String()+outCap.String(), inPath)
	}

	raw, err := readCapped(outPath, maxOutBytes)
	if err != nil {
		return pricer.Result{}, fmt.Errorf("saga: reading engine output: %w (input preserved at %s)", err, inPath)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return pricer.Result{}, fmt.Errorf("saga: bad engine output: %w\noutput: %s (input preserved at %s)",
			err, tail(string(raw), 500), inPath)
	}
	keepTmp = false

	res := pricer.Result{Raw: decoded}
	if v, ok := decoded["price"].(float64); ok && !math.IsNaN(v) && !math.IsInf(v, 0) {
		res.Price = v
	} else {
		return pricer.Result{}, fmt.Errorf("saga: %w for %s (input preserved at %s)", pricer.ErrNoPrice, endpoint, inPath)
	}
	res.NumPaths = intAsInt(decoded["num_paths"])
	res.NumSteps = intAsInt(decoded["num_steps"])
	res.RNG, _ = decoded["rng"].(string)
	if v, ok := decoded["elapsed_ms"].(float64); ok && !math.IsNaN(v) && !math.IsInf(v, 0) {
		res.ElapsedMS = v
	}
	return res, nil
}

// readCapped reads at most n bytes from p and errors if it exceeds n.
func readCapped(path string, n int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, n+1)
	total, _ := f.Read(buf)
	for total <= n {
		k, e := f.Read(buf[total:])
		total += k
		if e != nil {
			break
		}
	}
	if total > n {
		return nil, fmt.Errorf("engine output exceeds %d bytes", n)
	}
	return buf[:total], nil
}

// withDefaultSurface merges the Saga-level default surface into a copy of
// p so buildRequest never mutates the caller's Params, and the caller can
// reuse a Params value across backends. Precedence in buildRequest:
// p.Raw["surface"] > s.Surface > flat square at p.Vol.
func withDefaultSurface(s *Saga, p pricer.Params) pricer.Params {
	if s.Surface == nil {
		return p
	}
	raw := make(map[string]any, len(p.Raw)+1)
	for k, v := range p.Raw {
		raw[k] = v
	}
	if _, has := raw["surface"]; !has {
		raw["surface"] = s.Surface
	}
	p.Raw = raw
	return p
}

// buildRequest maps a pricer.Params onto the engine endpoint + argument object.
func buildRequest(p pricer.Params) (string, map[string]any, error) {
	if p.Script != "" && p.ScriptFile != "" {
		return "", nil, fmt.Errorf("saga: set Script OR ScriptFile, not both")
	}
	if p.Script == "" && p.ScriptFile == "" && p.Model == "" {
		// Documented vanilla shape: infer the payoff from strike/side. Only
		// when the caller has NOT selected an endpoint explicitly (p.Model
		// empty): explicit-model callers — including pass-through endpoints
		// like AriaPDEPriceLV whose arguments carry the payoff as
		// strike/isCall/barrier and which reject an inline `script` — are
		// fully responsible for the request shape.
		// (p is a copy of the caller's Params — buildRequest never mutates
		// the incoming struct.)
		if p.Strike <= 0 {
			return "", nil, fmt.Errorf("saga: no payoff: set Script, ScriptFile, or Strike (+Side)")
		}
		p.Script = vanillaScript(p.Strike, p.Side == "P")
	}

	args := map[string]any{
		"spot": p.Spot, "T": p.ExpiryYears,
		"num_paths": p.NumPaths, "num_steps": p.NumSteps,
		"seed": p.Seed, "rng": p.RNG,
	}
	if args["rng"] == "" {
		args["rng"] = "portable"
	}
	if p.Script != "" {
		args["script"] = p.Script
	} else {
		args["script_file"] = p.ScriptFile
	}

	switch strings.ToLower(p.Model) {
	case "", "bs":
		if p.Rate != 0 {
			args["rate"] = p.Rate
		}
		if p.Vol <= 0 {
			return "", nil, fmt.Errorf("saga: bs model requires Vol > 0")
		}
		args["vol"] = p.Vol
		endpoint := p.Endpoint
		if endpoint == "" {
			endpoint = "AriaMCPrice/1"
		}
		return endpoint, args, nil
	case "dupire", "local-vol", "localvol":
		// Surface precedence: Params.Raw["surface"] (caller) → Saga.Surface
		// (instance default, injected in Price) → flat square at p.Vol.
		surface, _ := p.Raw["surface"].(map[string]any)
		if surface == nil {
			if p.Vol <= 0 {
				return "", nil, fmt.Errorf("saga: local-vol model requires Vol > 0 or an explicit surface (Raw[\"surface\"])")
			}
			surface = flatSurface(p.Vol, p.ExpiryYears)
		}
		args["dynamics"] = "dupire"
		args["surface"] = surface
		endpoint := p.Endpoint
		if endpoint == "" {
			endpoint = "AriaMCPriceLV/1"
		}
		if p.Rate != 0 {
			args["rate"] = p.Rate
		}
		return endpoint, args, nil
	case "hybrid_lv":
		args["dynamics"] = "hybrid_lv"
		if surface, ok := p.Raw["surface"].(map[string]any); ok {
			args["surface"] = surface
		}
		if p.Rate != 0 {
			args["rate"] = p.Rate
		}
		endpoint := p.Endpoint
		if endpoint == "" {
			endpoint = "AriaMCPriceLV/1"
		}
		return endpoint, args, nil
	default:
		// Forward the endpoint verbatim; pass market state through wholesale
		// so exotic/multi-asset inputs keep working without new knobs.
		if p.Vol > 0 {
			args["vol"] = p.Vol
		}
		if p.Rate != 0 {
			args["rate"] = p.Rate
		}
		if p.Endpoint == "" {
			return "", nil, fmt.Errorf("saga: unknown model %q and no Endpoint set", p.Model)
		}
		for k, v := range p.Raw {
			if _, dup := args[k]; !dup {
				args[k] = v
			}
		}
		return p.Endpoint, args, nil
	}
}

// flatSurface builds a two-point Dupire surface that is flat at `vol` and
// stretched to cover an expiry of `tYears` years (grid endpoints are
// extrapolation-free by construction). `vol` must be > 0.
func flatSurface(vol, tYears float64) map[string]any {
	tEnd := 2.0
	if tYears*1.25 > tEnd {
		tEnd = tYears * 1.25
	}
	return map[string]any{
		"t_grid":    []any{0.0, tEnd},
		"logS_grid": []any{3.0, 6.2},
		"vol":       []any{[]any{vol, vol}, []any{vol, vol}},
	}
}

// vanillaScript is the minimal Aria contract for a European plain option —
// the exotics in the examples repo all follow this shape.
//
//	contract Call { let S = spot("X") one(KRW) |> scale(max(S - K, 0.0)) |> when(<date>) }
func vanillaScript(strike float64, put bool) string {
	body := fmt.Sprintf("max(S - %s, 0.0)", f(strike))
	if put {
		body = fmt.Sprintf("max(%s - S, 0.0)", f(strike))
	}
	side := "Call"
	if put {
		side = "Put"
	}
	// The when-date only pins payment timing to T; T itself comes from Params.
	return fmt.Sprintf(`contract %s { let S = spot("X") one(KRW) |> scale(%s) |> when(2026-01-01) }`, side, body)
}

// f formats a strike without trailing zeros or exponential noise.
func f(v float64) string {
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.10f", v), "0"), ".")
}

func intAsInt(v any) int {
	n, _ := v.(float64)
	return int(n)
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

// defaultRoot finds the price-master-examples checkout.
func defaultRoot() (string, error) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("cannot resolve caller path for root discovery")
	}
	// Up from internal/pricer/saga: repo root is 3 levels up; the sibling
	// checkout is one level above that.
	modRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..", ".."))
	for _, cand := range []string{
		filepath.Join(modRoot, "..", "price-master-examples"),
		filepath.Join(modRoot, "price-master-examples"),
	} {
		if info, err := os.Stat(filepath.Join(cand, "bin")); err == nil && info.IsDir() {
			return cand, nil
		}
	}
	return "", fmt.Errorf("saga: price-master-examples not found (set Root or SIBELIUS_SAMPLES_ROOT); tried %s", filepath.Join(modRoot, "..", "price-master-examples"))
}

func executable(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir() && st.Mode()&0o111 != 0
}

// defaultBin locates the SibeliusSaga binary under root/bin (reassembled),
// falling back to a sibling dev build.
func defaultBin(root string) (string, error) {
	var variants []string
	{
		base := "SibeliusSaga-" + runtime.GOOS + "-" + runtime.GOARCH
		variants = append(variants, base+"-cuda", base)
		if runtime.GOOS == "darwin" {
			variants = append(variants, base+"-metal")
		}
	}
	for _, v := range variants {
		p := filepath.Join(root, "bin", v)
		if executable(p) {
			return p, nil
		}
	}
	if p := filepath.Join(filepath.Dir(root), "Sibelius", "build_release", "SibeliusSaga"); executable(p) {
		return p, nil
	}
	return "", fmt.Errorf("saga: no SibeliusSaga binary found under %s (run bin/reassemble.sh; set SAGA_BIN or s.Bin to override)", filepath.Join(root, "bin"))
}

// PriceVanilla prices a plain European vanilla (call/put) without an .aria
// file: the payoff is generated inline from strike/side. Use it for
// BS-vs-MC cross-checks, or as the entry for option instruments with no
// bespoke script. `strike` may legitimately be 0 (degenerate zero-strike
// structures); the Params.Strike/Side fields are untouched.
func (s *Saga) PriceVanilla(ctx context.Context, strike float64, put bool, p pricer.Params) (pricer.Result, error) {
	p.Script = vanillaScript(strike, put)
	p.ScriptFile = ""
	return s.Price(ctx, p)
}
