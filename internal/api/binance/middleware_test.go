package binance

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zeta1999/crypto-exchange-golang/internal/engine"
	"github.com/zeta1999/crypto-exchange-golang/internal/metrics"
	"github.com/zeta1999/crypto-exchange-golang/internal/orderbook"
	"github.com/zeta1999/crypto-exchange-golang/internal/ratelimit"
)

// doReq runs a GET against h with a fixed client IP so the keyed limiter sees a
// single source.
func doReq(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = "10.0.0.1:12345"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestRateLimitReturns429 wires a burst-1 limiter and asserts the second public
// request from the same source is rejected with HTTP 429 and Binance code -1003,
// and that the rate-limited metric increments.
func TestRateLimitReturns429(t *testing.T) {
	clock := func() time.Time { return time.UnixMilli(1_700_000_000_000).UTC() }
	book := orderbook.New([]string{"BTC-USD"})
	eng := engine.New(book, noopMargin{}, nil)
	authn := NewAuthenticator(testAPIKey, testSecret, clock)

	reg := metrics.NewRegistry()
	m := NewMetrics(reg)
	limiter := ratelimit.NewKeyedLimiter(0.0001, 1, time.Minute) // burst 1, ~no refill
	bsrv := New(eng, newTestSymbolMap(), authn, NewRegistry(clock),
		WithClock(clock), WithRateLimiter(limiter), WithMetrics(m))

	h := bsrv.Handler()

	// First request: allowed.
	rec1 := doReq(t, h, "/api/v3/ping")
	if rec1.Code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", rec1.Code)
	}
	// Second request from the same IP: throttled.
	rec2 := doReq(t, h, "/api/v3/ping")
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("second request status = %d, want 429", rec2.Code)
	}
	if !strings.Contains(rec2.Body.String(), "-1003") {
		t.Fatalf("429 body missing -1003: %s", rec2.Body.String())
	}

	var b strings.Builder
	_ = reg.WriteText(&b)
	out := b.String()
	if !strings.Contains(out, "exchange_binance_rate_limited_total 1") {
		t.Fatalf("rate_limited metric not incremented:\n%s", out)
	}
	if !strings.Contains(out, `exchange_binance_requests_total{endpoint="/api/v3/ping",status="2xx"} 1`) {
		t.Fatalf("request metric missing 2xx:\n%s", out)
	}
}

func TestRateLimitDisabledByDefault(t *testing.T) {
	clock := func() time.Time { return time.UnixMilli(1_700_000_000_000).UTC() }
	book := orderbook.New([]string{"BTC-USD"})
	eng := engine.New(book, noopMargin{}, nil)
	authn := NewAuthenticator(testAPIKey, testSecret, clock)
	bsrv := New(eng, newTestSymbolMap(), authn, NewRegistry(clock), WithClock(clock))
	h := bsrv.Handler()
	for i := 0; i < 50; i++ {
		if rec := doReq(t, h, "/api/v3/ping"); rec.Code != http.StatusOK {
			t.Fatalf("request %d throttled with no limiter: %d", i, rec.Code)
		}
	}
}
