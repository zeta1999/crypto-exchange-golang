package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zeta1999/crypto-exchange-golang/internal/pricer"
	"github.com/zeta1999/crypto-exchange-golang/internal/pricer/saga"
)

// TestLiveOptionsExoticDealerQuoting is the end-to-end proof of the
// optmarket dealer-quoting mode: the exchange's OPTIONS book (EAPI surface)
// lists a flow exotic next to a vanilla chain and quotes it by pricing the
// exotic's ACTUAL configured payoff through the saga Pricer plugin — never
// via vanilla legs.
//
// Live (skip-gated): needs the saga engine binary (../price-master-examples).
// Fully offline otherwise: the option underlyings use a static index (no
// engine book, no network feed).
//
// Assertions, on GET /eapi/v1/mark/exotic:
//  1. The exotic mark carries provenance (pricer=saga) with IV 0 — an exotic
//     is not a Black–Scholes instrument; no IV is invented.
//  2. The mark matches — within MC noise — the price the saga engine returns
//     for the SAME payoff, SAME spot, SAME knobs, recomputed independently in
//     this test with no server in the loop.
//  3. The exotic mark is below the plain vanilla call of the same
//     strike/expiry (a down-and-out can never dominate its vanilla) and the
//     executable bid/offer is uncrossed.
//  4. GET /eapi/v1/mark is vanilla-only (the exotic symbol is rejected there),
//     while /eapi/v1/exchangeInfo lists the exotic contract.
func TestLiveOptionsExoticDealerQuoting(t *testing.T) {
	if testing.Short() {
		t.Skip("live: needs the saga engine binary")
	}
	root := resolveSagaRoot(t)
	if root == "" {
		t.Skip("price-master-examples checkout not found (set SIBELIUS_SAMPLES_ROOT)")
	}
	t.Setenv("SIBELIUS_SAMPLES_ROOT", root)

	const (
		staticIndex = 50000.0
		strike      = 60000.0 // 1.2 x spot, 20% OTM
		barrier     = 30000.0 // 0.6 x spot, 40% below
		vol         = 0.30
		rate        = 0.05
	)
	// The SAME barrier payoff the server is configured with. The engine
	// collapses when(dates) to maturity; T comes from exotic_expiry_years.
	barrierScript := fmt.Sprintf(
		`contract DownAndOutCall { let S = spot("X") var alive = 1.0 `+
			`alive := alive * cond(S > %.1f, 1.0, 0.0) `+
			`one(KRW) |> scale(alive * max(S - %.1f, 0.0)) |> when(2027-06-30) }`,
		barrier, strike)
	vanillaScript := fmt.Sprintf(
		`contract Call { let S = spot("X") one(KRW) |> scale(max(S - %.1f, 0.0)) |> when(2027-06-30) }`, strike)

	binPath := buildExchangeBinary(t)
	dir := t.TempDir()
	grpcPort, wsPort, httpPort, binancePort := freeTCPPort(t), freeTCPPort(t), freeTCPPort(t), freeTCPPort(t)

	cfg := fmt.Sprintf(liveExoticOptionsCfg,
		grpcPort, httpPort, wsPort,
		filepath.Join(dir, "e.wal"),
		binancePort,
		indentPayoff(barrierScript))
	cfgPath := filepath.Join(dir, "ex.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	logFile, err := os.Create(filepath.Join(dir, "server.log"))
	if err != nil {
		t.Fatal(err)
	}
	srv := exec.Command(binPath)
	// EXCHANGE_CONFIG selects the preset; the saga backend resolves
	// payoffs relative to the examples root, and default binary discovery
	// walks up from the module root — run from the repo root so both work,
	// and pin the root via env to be safe.
	srv.Dir = repoRoot()
	srv.Env = append(os.Environ(), "EXCHANGE_CONFIG="+cfgPath, "SIBELIUS_SAMPLES_ROOT="+root)
	srv.Stdout, srv.Stderr = logFile, logFile
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		srv.Process.Kill()
		_ = srv.Wait()
		_ = logFile.Close()
	})

	// The /eapi/* routes live on the Binance edge, so wait on its port.
	base := fmt.Sprintf("http://127.0.0.1:%d", binancePort)
	if err := waitHTTP(base+"/eapi/v1/exchangeInfo", 90*time.Second); err != nil {
		dumpLog(t, dir)
		t.Fatalf("server never came up on %s: %v", base, err)
	}

	// 1) The exotic mark, from the live server.
	var marks []struct {
		Symbol string `json:"symbol"`
		Price  string `json:"price"`
		Bid    string `json:"bid"`
		Ask    string `json:"ask"`
		MarkIV string `json:"markIV"`
		Pricer string `json:"pricer"`
		Model  string `json:"model"`
	}
	if err := getJSON(base+"/eapi/v1/mark/exotic?symbol=BTC-260630-60000-BAR", &marks); err != nil {
		dumpLog(t, dir)
		t.Fatal(err)
	}
	if len(marks) != 1 {
		t.Fatalf("want 1 exotic mark, got %d: %+v", len(marks), marks)
	}
	m := marks[0]
	if m.Pricer != "saga" {
		t.Fatalf("exotic mark must carry pricer provenance, got %q: %+v", m.Pricer, m)
	}
	if m.MarkIV != "0.00000000" {
		t.Errorf("exotic markIV must be 0 (no invented IV), got %q", m.MarkIV)
	}
	price, _ := strconv64(m.Price)
	bid, _ := strconv64(m.Bid)
	ask, _ := strconv64(m.Ask)
	if price <= 0 {
		t.Fatalf("non-positive exotic mark %q", m.Price)
	}
	if bid <= 0 || bid > price || ask < price {
		t.Fatalf("executable quote must bracket the fair value (bid=%v price=%v ask=%v)", bid, price, ask)
	}
	if bid >= ask {
		t.Fatalf("exotic book crossed: bid=%v ask=%v", bid, ask)
	}
	t.Logf("live exotic mark: symbol=%s price=%s bid=%s ask=%s pricer=%s model=%s",
		m.Symbol, m.Price, m.Bid, m.Ask, m.Pricer, m.Model)

	// 2) Independent saga reference for the SAME payoff (no server in the
	// loop). Same portable seed ⇒ reproducible to the engine's own ~1e-3
	// golden tolerance; allow 5% for any build drift.
	expiryYears := 1.0
	ref := func(script string) (float64, error) {
		res, err := (&saga.Saga{Timeout: 5 * time.Minute}).Price(context.Background(), pricer.Params{
			Symbol: "BAR", Spot: staticIndex, Vol: vol, Rate: rate,
			ExpiryYears: expiryYears, NumPaths: 65536, NumSteps: 252, Seed: 42,
			Script: script,
		})
		if err != nil {
			return 0, err
		}
		return res.Price, nil
	}
	fair, err := ref(barrierScript)
	if err != nil {
		t.Fatalf("independent saga barrier reference: %v", err)
	}
	vanilla, err := ref(vanillaScript)
	if err != nil {
		t.Fatalf("independent saga vanilla reference: %v", err)
	}
	t.Logf("saga reference: barrier=%s vanilla=%s | server mark=%s", lean(fair), lean(vanilla), lean(price))
	if rel := math.Abs(price-fair) / fair; rel > 0.05 {
		t.Errorf("server exotic mark %s deviates from the ACTUAL barrier fair value %s by %.4f (>5%%) — the configured payoff is not being priced",
			lean(price), lean(fair), rel)
	}
	if price >= vanilla {
		t.Errorf("exotic mark %s >= plain vanilla call %s: a down-and-out never exceeds its vanilla (possible vanilla-leg fabrication)", lean(price), lean(vanilla))
	}

	// 3) The vanilla endpoint stays vanilla-only; exchangeInfo lists the exotic.
	var vMarks []map[string]any
	if err := getJSON(base+"/eapi/v1/mark?symbol=BTC-260630-60000-BAR", &vMarks); err == nil {
		t.Errorf("vanilla /eapi/v1/mark must reject the exotic symbol (got: %+v)", vMarks)
	}
	var info struct {
		OptionSymbols []struct {
			Symbol string `json:"symbol"`
		} `json:"optionSymbols"`
	}
	if err := getJSON(base+"/eapi/v1/exchangeInfo", &info); err != nil {
		t.Fatalf("exchangeInfo: %v", err)
	}
	found := false
	for _, s := range info.OptionSymbols {
		if s.Symbol == "BTC-260630-60000-BAR" {
			found = true
		}
	}
	if !found {
		t.Errorf("exotic contract missing from /eapi/v1/exchangeInfo: %+v", info.OptionSymbols)
	}
	t.Log("live options dealer quoting: exotic mark tracks the ACTUAL saga payoff, uncrossed, below vanilla, provenance present: OK")
}

// --- helpers (self-contained; the FIX smoke test has its own copies) ---

func resolveSagaRoot(t *testing.T) string {
	t.Helper()
	if env := os.Getenv("SIBELIUS_SAMPLES_ROOT"); env != "" {
		return env
	}
	// The checkout is a sibling of this module. Probe relative to the module
	// root (CWD-independent) first, then the CWD-relative depths as fallbacks.
	cands := []string{
		filepath.Join(repoRoot(), "..", "price-master-examples"),
		"../../../price-master-examples", // CWD = internal/integration
		"../../price-master-examples",
		"../price-master-examples",
	}
	for _, cand := range cands {
		if st, err := os.Stat(filepath.Join(cand, "bin")); err == nil && st.IsDir() {
			abs, err := filepath.Abs(cand)
			if err != nil {
				t.Fatal(err)
			}
			return abs
		}
	}
	return ""
}

func repoRoot() string {
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	// internal/integration -> 2 up = repo root.
	return filepath.Join(wd, "..", "..")
}

func buildExchangeBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "exchange")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/exchange")
	cmd.Dir = repoRoot()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("cannot build the exchange binary: %v\n%s", err, out)
	}
	return bin
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func waitHTTP(url string, deadline time.Duration) error {
	end := time.Now().Add(deadline)
	var last error
	for time.Now().Before(end) {
		resp, err := http.Get(url) //nolint:gosec // loopback test URL
		if err == nil {
			resp.Body.Close()
			return nil
		}
		last = err
		time.Sleep(300 * time.Millisecond)
	}
	return last
}

func getJSON(url string, v any) error {
	resp, err := http.Get(url) //nolint:gosec // loopback test URL
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

func strconv64(s string) (float64, error) {
	var v float64
	_, err := fmt.Sscanf(s, "%g", &v)
	return v, err
}

func lean(v float64) string {
	s := fmt.Sprintf("%.4f", v)
	s = strings.TrimRight(s, "0")
	if strings.HasSuffix(s, ".") {
		s = strings.TrimSuffix(s, ".")
	}
	return s
}

func indentPayoff(script string) string {
	// Embed one line under the `payoff:` block-scalar key (content must be
	// indented strictly deeper than the key, which sits at 6 spaces).
	return "        " + script
}

func dumpLog(t *testing.T, dir string) {
	t.Helper()
	if b, err := os.ReadFile(filepath.Join(dir, "server.log")); err == nil {
		if len(b) > 3000 {
			b = b[len(b)-3000:]
		}
		t.Logf("server log (tail):\n%s", b)
	}
}

// liveExoticOptionsCfg is the offline options preset: a static-index
// underlying, a 1-strike vanilla chain, and one flow exotic (down-and-out
// barrier call) priced by the saga plugin. Placeholders (in order):
// %d grpc, %d native-http, %d native-ws, %q wal, %d binance(edge w/ EAPI),
// %s the indented inline Aria payoff. The native-http port is present only
// because the config schema requires the three network listeners.
const liveExoticOptionsCfg = `
network:
  listen_grpc: ":%d"
  listen_http: ":%d"
  listen_ws: ":%d"
  token_secret: "smoke"
  tls: { cert_file: "", key_file: "" }
database:
  dsn: "memory"
limits:
  max_open_orders: 500
  min_tick_size: 0.01
instruments:
  - symbol: "BTC-USD"
    base: "BTC"
    quote: "USD"
storage:
  wal_path: %q
api:
  binance:
    enabled: true
    listen: ":%d"
    api_key: "k"
    secret: "s"
    rate_per_sec: 0
    burst: 0
    symbols:
      - binance: "BTCUSDT"
        engine: "BTC-USD"
    balances: { USD: "1000000", BTC: "10" }
options:
  enabled: true
  rate: 0.03
  iv_spread: 0.02
  book_half_spread: 0.02
  price_band: 0.5
  venues:
    - name: binance
  underlyings:
    - underlying: "BTCUSDT"
      coin: "BTC"
      quote: "USDT"
      static_index: 50000
      atm_vol: 0.30
      expiries: ["2027-06-30"]
      strikes: [50000]
  pricer: "saga"
  exotic_vol: 0.30
  exotic_rate: 0.05
  exotic_num_paths: 65536
  exotic_num_steps: 252
  exotic_seed: 42
  exotic_expiry_years: 1.0
  exotic_spread_bps: 10
  exotic_ttl: "5s"
  exotics:
    - coin: "BTC"
      underlying: "BTCUSDT"
      expiry: "2027-06-30"
      strike: 60000
      kind: "C"
      venue_symbol: "BTC-260630-60000-BAR"
      payoff: |
%s
metrics:
  enabled: false
emulator:
  enabled: false
transfer:
  enabled: false
`
