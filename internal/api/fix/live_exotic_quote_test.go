package fix

import (
	"bufio"
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
	"github.com/zeta1999/crypto-exchange-golang/pkg/decimal"
)

// TestLiveExoticPayoffQuotes is the end-to-end proof that the FIX quote flow
// prices each exotic's ACTUAL configured payoff — not a fabricated ATM
// vanilla put-call-parity pair.
//
// Live (skip-gated): needs outbound internet (Coinbase feed for the funded
// book) and the saga engine binary (../price-master-examples). It boots the
// real exchange binary on a free port, feeds BTC-USD from the live Coinbase
// emulator, and exposes a quote-only exotic "EXO" whose payoff is an
// ATM-ish down-and-out call priced through the saga Pricer plugin.
//
// Assertions:
//  1. z→S returns a positive, uncrossed bid/offer pair.
//  2. The quote midpoint matches — within MC noise — the price the saga
//     engine returns for the SAME payoff at the SAME spot, recomputed
//     independently in this test (no server in the loop). A fabricated
//     parity-pair quote does not satisfy this.
//  3. The quote midpoint is below the plain vanilla call of the same
//     strike/expiry: a down-and-out can never dominate its vanilla, which
//     a parity-pair quote (independent C + P legs) does not guarantee.
func TestLiveExoticPayoffQuotes(t *testing.T) {
	if testing.Short() {
		t.Skip("live: needs network + saga binary")
	}

	spot, err := coinbaseSpot()
	if err != nil || spot <= 0 {
		t.Skipf("no live Coinbase spot: %v", err)
	}

	binPath, err := buildExchange(t)
	if err != nil {
		t.Skipf("cannot build the exchange binary: %v", err)
	}

	// ATM-adjacent barrier call: K = 1.2·S (20% OTM), B = 0.6·S (40% below
	// spot). At 30% vol over 1y the barrier is unlikely to be hit, so the
	// payoff value is in (0, vanilla_call). If the FIX edge were instead
	// fabricating an ATM vanilla C+P pair off the live mid S, the quote
	// midpoint would be ≈ S − K + put ≈ S − 1.2S + C(1.2S) — a value that is
	// usually negative and never equals the barrier's MC price.
	K := spot * 1.2
	B := spot * 0.6
	barrierScript := fmt.Sprintf(
		`contract DownAndOutCall { let S = spot("X") var alive = 1.0 `+
			`alive := alive * cond(S > %s, 1.0, 0.0) `+
			`one(KRW) |> scale(alive * max(S - %s, 0.0)) |> when(2026-01-01) }`,
		leanNum(B), leanNum(K))
	vanillaScript := fmt.Sprintf(
		`contract Call { let S = spot("X") one(KRW) |> scale(max(S - %s, 0.0)) |> when(2026-01-01) }`,
		leanNum(K))

	dir := t.TempDir()
	fixPort := freePort(t)

	cfg := fmt.Sprintf(cfgTemplate,
		freePort(t), freePort(t), freePort(t),
		filepath.Join(dir, "e.wal"),
		freePort(t),
		indentBlock(barrierScript))
	cfg = strings.Replace(cfg, "FIXPORT", fmt.Sprint(fixPort), 1)
	cfgPath := filepath.Join(dir, "cfg.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	// Boot the real server with EXCHANGE_CONFIG pointing at our file.
	logF, err := os.Create(filepath.Join(dir, "server.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer logF.Close()
	cmd := exec.Command(binPath)
	cmd.Env = append(os.Environ(), "EXCHANGE_CONFIG="+cfgPath)
	cmd.Stdout = logF
	cmd.Stderr = logF
	if err := cmd.Start(); err != nil {
		t.Fatalf("server start: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	fixAddr := fmt.Sprintf("127.0.0.1:%d", fixPort)
	deadline := time.Now().Add(90 * time.Second)
	up := false
	for time.Now().Before(deadline) {
		c, err := net.Dial("tcp", fixAddr)
		if err == nil {
			c.Close()
			up = true
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if !up {
		b, _ := os.ReadFile(filepath.Join(dir, "server.log"))
		t.Fatalf("server did not come up on %s:\n%s", fixAddr, b)
	}
	// Let the coinbase feed seed the funded book.
	time.Sleep(3 * time.Second)

	// Drive the quote flow over a real socket.
	conn, err := net.Dial("tcp", fixAddr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	cli := &fixClient{conn: conn, r: bufio.NewReader(conn), seq: 1, in: make(chan *Message, 16), t: t}
	go cli.readLoop()
	cli.logon()

	qreq := NewMessage(MsgQuoteRequest)
	qreq.Set(TagQuoteReqID, "Q1")
	qreq.Set(TagSymbol, "EXO")
	qreq.Set(TagSide, "1")
	qreq.Set(TagOrderQty, "1")
	cli.send(qreq)
	// The saga engine call can exceed the 2s default expect window (cold
	// binary, live spot); poll for the Quote with a generous deadline.
	var qm *Message
	quoteDeadline := time.After(90 * time.Second)
	for qm == nil {
		select {
		case m, ok := <-cli.in:
			if !ok {
				t.Fatal("connection closed waiting for Quote")
			}
			if m.MsgType() == MsgQuote {
				qm = m
			}
		case <-quoteDeadline:
			b, _ := os.ReadFile(filepath.Join(dir, "server.log"))
			t.Fatalf("no Quote within 90s; server log:\n%s", b)
		}
	}
	qid, _ := qm.Get(TagQuoteID)
	bidS, _ := qm.Get(TagBidPx)
	offerS, _ := qm.Get(TagOfferPx)
	if bidS == "" || offerS == "" {
		t.Fatalf("quote missing prices: %+v", qm.Fields)
	}
	bid, _ := decimal.Parse(bidS)
	offer, _ := decimal.Parse(offerS)
	if bid.Sign() <= 0 || offer.Sign() <= 0 || bid.Gt(offer) {
		t.Fatalf("bad quote: bid=%s offer=%s", bidS, offerS)
	}
	mid := bid.Add(offer).Div(decimal.FromInt(2)).Float64()
	t.Logf("Z→S Quote: QuoteID=%s BidPx=%s OfferPx=%s mid=%s | spot=%.2f",
		qid, bidS, offerS, leanNum(mid), spot)

	// Independent reference prices via the saga Pricer (no server).
	px := &saga.Saga{Timeout: 5 * time.Minute}
	ref := func(script string) (float64, error) {
		res, err := px.Price(context.Background(), pricer.Params{
			Symbol: "EXO", Spot: spot, Vol: 0.30, Rate: 0.05,
			ExpiryYears: 1.0, NumPaths: 65536, NumSteps: 252, Seed: 42,
			Script: script,
		})
		if err != nil {
			return 0, err
		}
		return res.Price, nil
	}
	barrier, err := ref(barrierScript)
	if err != nil {
		t.Fatalf("saga barrier reference: %v", err)
	}
	vanilla, err := ref(vanillaScript)
	if err != nil {
		t.Fatalf("saga vanilla reference: %v", err)
	}
	t.Logf("saga reference: barrier=%s vanilla=%s | quote mid=%s",
		leanNum(barrier), leanNum(vanilla), leanNum(mid))

	relDiff := math.Abs(mid-barrier) / barrier
	if relDiff > 0.05 {
		t.Errorf("quote mid %s deviates from ACTUAL barrier fair value %s by %.4f (>5%%): the configured payoff is not being priced",
			leanNum(mid), leanNum(barrier), relDiff)
	}
	if mid >= vanilla {
		t.Errorf("quote mid %s >= plain vanilla call %s: a down-and-out never exceeds its vanilla (possible fabricated parity pair)",
			leanNum(mid), leanNum(vanilla))
	}
	t.Log("live exotic quote: quote tracks the ACTUAL barrier payoff (saga), uncrossed, in (0, vanilla): OK")
}

// cfgTemplate is the smoke server config. Placeholders:
//
//	%d (x4)  → grpc/http/ws/binance free ports
//	%q        → wal path
//	FIXPORT   → FIX listen port (substituted after)
//	%s        → indented Aria script (the exotic payload)
const cfgTemplate = `
network:
  listen_grpc: ":%d"
  listen_http: ":%d"
  listen_ws: ":%d"
  token_secret: "smoke"
  tls: { cert_file: "", key_file: "" }
database:
  dsn: "memory"
limits:
  max_open_orders: 5000
  min_tick_size: 0.01
instruments:
  - symbol: "BTC-USD"
    base: "BTC"
    quote: "USD"
  - symbol: "BTC-EXO"        # quote-only exotic: its own empty book holds the confirmed orders
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
    balances:
      USD: "1000000"
      BTC: "10"
  fix:
    enabled: true
    listen: "127.0.0.1:FIXPORT"
    sender_comp_id: "MIRAGE"
    symbols:
      - binance: "EXO"
        engine: "BTC-EXO"
    quotes:
      enabled: true
      pricer: "saga"
      exotics:
        - engine: "BTC-EXO"
          payoff: |
%s
      spot_from:
        "BTC-EXO": "BTC-USD"
      vol: 0.30
      rate: 0.05
      spread_bps: 10
      num_paths: 65536
      num_steps: 252
      seed: 42
      quote_ttl: "5m"
metrics:
  enabled: false
emulator:
  enabled: true
  venue: "coinbase"
  instruments: ["BTC-USD"]
  reference:
    depth_levels: 20
    refresh_ms: 200
  rtr:
    tau_ms: 0
transfer:
  enabled: false
`

// coinbaseSpot fetches the live BTC-USD spot price (public, unsigned).
func coinbaseSpot() (float64, error) {
	r, err := http.Get("https://api.coinbase.com/v2/prices/BTC-USD/spot")
	if err != nil {
		return 0, err
	}
	defer r.Body.Close()
	var body struct {
		Data struct {
			Amount string `json:"amount"`
		} `json:"data"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return 0, err
	}
	var v float64
	if _, err := fmt.Sscanf(body.Data.Amount, "%g", &v); err != nil {
		return 0, err
	}
	return v, nil
}

// buildExchange compiles the exchange binary into the test's temp dir.
func buildExchange(t *testing.T) (string, error) {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	repo := filepath.Join(wd, "..", "..", "..") // 3 up from internal/api/fix
	bin := filepath.Join(t.TempDir(), "exchange")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/exchange")
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("go build: %v\n%s", err, out)
	}
	return bin, nil
}

// freePort returns a likely-free TCP port.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	p := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return p
}

// indentBlock prepends 12 spaces to each line for embedding in the
// `payoff: |` YAML block scalar: the key sits at 10 spaces, so the
// content must be indented STRICTLY deeper (12) or the parser rejects it.
func indentBlock(s string) string {
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = "            " + lines[i]
	}
	return strings.Join(lines, "\n")
}

// leanNum formats without trailing zeros (for scripts and log lines).
func leanNum(v float64) string {
	s := fmt.Sprintf("%.4f", v)
	s = strings.TrimRight(s, "0")
	if strings.HasSuffix(s, ".") {
		s = strings.TrimSuffix(s, ".")
	}
	return s
}
