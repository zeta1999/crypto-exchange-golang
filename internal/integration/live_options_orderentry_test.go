package integration

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// TestLiveOptionsOrderEntry is the end-to-end proof of options ORDER ENTRY on
// the EAPI surface: the exchange boots with an options book (static index),
// registers the option symbols on the engine book, and serves SIGNED
// POST /eapi/v1/order (place), GET /eapi/v1/order (query) and DELETE
// /eapi/v1/order (cancel) against a real matching book.
//
// Offline and always runnable (no saga binary, no network feed): a vanilla
// chain with a far-future expiry, so the expiry-settle sweep has nothing to
// settle — the order LIFECYCLE is what's under test. Assertions:
//  1. A resting BUY at the mark price returns NEW.
//  2. GET /eapi/v1/order returns it by orderId AND by origClientOrderId.
//  3. GET /eapi/v1/orders lists it; a duplicate newClientOrderId is -2010
//     and still only one order appears (CR-5 place-time idempotency).
//  4. DELETE /eapi/v1/order cancels it; open-orders is empty afterwards.
func TestLiveOptionsOrderEntry(t *testing.T) {
	binPath := buildExchangeBinary(t)
	dir := t.TempDir()
	grpcPort, wsPort, httpPort, binancePort := freeTCPPort(t), freeTCPPort(t), freeTCPPort(t), freeTCPPort(t)

	cfg := fmt.Sprintf(liveOrderEntryCfg,
		grpcPort, httpPort, wsPort,
		filepath.Join(dir, "e.wal"),
		binancePort)
	cfgPath := filepath.Join(dir, "oe.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	logFile, err := os.Create(filepath.Join(dir, "server.log"))
	if err != nil {
		t.Fatal(err)
	}
	srv := exec.Command(binPath)
	srv.Dir = repoRoot()
	srv.Env = append(os.Environ(), "EXCHANGE_CONFIG="+cfgPath)
	srv.Stdout, srv.Stderr = logFile, logFile
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		srv.Process.Kill()
		_ = srv.Wait()
		_ = logFile.Close()
	})

	base := fmt.Sprintf("http://127.0.0.1:%d", binancePort)
	if err := waitHTTP(base+"/eapi/v1/exchangeInfo", 90*time.Second); err != nil {
		dumpLog(t, dir)
		t.Fatalf("server never came up on %s: %v", base, err)
	}
	const apiKey, apiSecret = "k", "s"
	sym := "BTC-270630-50000-C" // the one listed contract (static index, 1 strike, 2027-06-30)

	// Resting price: the live EAPI mark of the contract (inside its band).
	var marks []struct {
		MarkPrice string `json:"markPrice"`
	}
	if err := getJSON(base+"/eapi/v1/mark?symbol="+sym, &marks); err != nil || len(marks) != 1 {
		t.Fatalf("mark for %s: %v / %d entries", sym, err, len(marks))
	}
	price := marks[0].MarkPrice

	// 1) Resting BUY at the mark price → NEW.
	resp := signedEAPI(t, base, "POST", "/eapi/v1/order",
		eq("symbol", sym, "side", "BUY", "type", "LIMIT", "quantity", "1",
			"price", price, "newClientOrderId", "oe-1"), apiKey, apiSecret)
	var place struct {
		OrderID int64  `json:"orderId"`
		Status  string `json:"status"`
	}
	decodeEAPI(t, resp, &place)
	if place.Status != "NEW" || place.OrderID <= 0 {
		t.Fatalf("place: %+v (status %d)", place, resp.StatusCode)
	}

	// 2) Query by orderId and by origClientOrderId.
	var q struct {
		OrderID       int64  `json:"orderId"`
		ClientOrderID string `json:"clientOrderId"`
		Status        string `json:"status"`
	}
	decodeEAPI(t, signedEAPI(t, base, "GET", "/eapi/v1/order",
		eq("symbol", sym, "orderId", strconv.FormatInt(place.OrderID, 10)), apiKey, apiSecret), &q)
	if q.OrderID != place.OrderID || q.Status != "NEW" || q.ClientOrderID != "oe-1" {
		t.Fatalf("query by orderId: %+v", q)
	}
	decodeEAPI(t, signedEAPI(t, base, "GET", "/eapi/v1/order",
		eq("symbol", sym, "origClientOrderId", "oe-1"), apiKey, apiSecret), &q)
	if q.OrderID != place.OrderID {
		t.Fatalf("query by origClientOrderId: %+v", q)
	}

	// 3) Open orders lists it; a duplicate id is -2010 with no 2nd order.
	resp = signedEAPI(t, base, "GET", "/eapi/v1/orders", eq("symbol", sym), apiKey, apiSecret)
	var open []struct {
		OrderID int64 `json:"orderId"`
	}
	decodeEAPI(t, resp, &open)
	if len(open) != 1 || open[0].OrderID != place.OrderID {
		t.Fatalf("openOrders: %+v", open)
	}
	resp = signedEAPI(t, base, "POST", "/eapi/v1/order",
		eq("symbol", sym, "side", "BUY", "type", "LIMIT", "quantity", "1",
			"price", price, "newClientOrderId", "oe-1"), apiKey, apiSecret)
	var dup struct {
		Code int `json:"code"`
		Msg  string
	}
	decodeEAPI(t, resp, &dup)
	if dup.Code != -2010 {
		t.Fatalf("duplicate newClientOrderId: got code %d (want -2010)", dup.Code)
	}
	resp = signedEAPI(t, base, "GET", "/eapi/v1/orders", eq("symbol", sym), apiKey, apiSecret)
	decodeEAPI(t, resp, &open)
	if len(open) != 1 {
		t.Fatalf("duplicate must not create a 2nd resting order: %d open", len(open))
	}

	// 4) Cancel by orderId → CANCELED; open orders empty.
	resp = signedEAPI(t, base, "DELETE", "/eapi/v1/order",
		eq("symbol", sym, "orderId", strconv.FormatInt(place.OrderID, 10)), apiKey, apiSecret)
	var done struct {
		Status string `json:"status"`
	}
	decodeEAPI(t, resp, &done)
	if done.Status != "CANCELED" {
		t.Fatalf("cancel: %+v (status %d)", done, resp.StatusCode)
	}
	resp = signedEAPI(t, base, "GET", "/eapi/v1/orders", eq("symbol", sym), apiKey, apiSecret)
	decodeEAPI(t, resp, &open)
	if len(open) != 0 {
		t.Fatalf("openOrders after cancel: %d (want 0)", len(open))
	}
}

// eq builds url.Values from alternating key/value strings.
func eq(pairs ...string) url.Values {
	v := url.Values{}
	for i := 0; i+1 < len(pairs); i += 2 {
		v.Set(pairs[i], pairs[i+1])
	}
	return v
}

// nowUnixMs is the wall clock in epoch-ms — the signed requests need a
// timestamp inside the server's recvWindow, and the live server runs on
// time.Now (no injected clock).
func nowUnixMs() int64 { return time.Now().UnixMilli() }

// signedEAPI issues a SIGNED EAPI request: the Binance scheme the edge
// verifies — timestamp + HMAC-SHA256 over the query string, signature appended
// last, X-MBX-APIKEY header.
func signedEAPI(t *testing.T, base, method, path string, params url.Values, key, secret string) *http.Response {
	t.Helper()
	params.Set("timestamp", strconv.FormatInt(nowUnixMs(), 10))
	raw := params.Encode()
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(raw))
	sig := hex.EncodeToString(mac.Sum(nil))
	req, err := http.NewRequest(method, base+path+"?"+raw+"&signature="+url.QueryEscape(sig), nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("X-MBX-APIKEY", key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

// decodeEAPI reads and JSON-decodes a response body, reporting errors with
// the HTTP status. Bodies are small and closed even on error.
func decodeEAPI(t *testing.T, resp *http.Response, v interface{}) {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("decode %s %s (status %d): %v", resp.Request.Method, resp.Request.URL.Path, resp.StatusCode, err)
	}
}

// liveOrderEntryCfg boots the exchange with ONE far-future vanilla contract
// (static index 50000) and a seeded USDT ledger, so signed option order
// entry can be exercised end to end with no saga binary and no network feed.
const liveOrderEntryCfg = `
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
    balances: { USDT: "1000000" }
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
`
