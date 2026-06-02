package coinbase

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

func doReq(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = "10.0.0.1:9999"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestRateLimitReturns429 asserts the Coinbase edge throttles with a 429 and a
// rate_limit_exceeded body, and records the metric.
func TestRateLimitReturns429(t *testing.T) {
	clock := func() time.Time { return time.UnixMilli(1_700_000_000_000).UTC() }
	book := orderbook.New([]string{"BTC-USD"})
	eng := engine.New(book, noopMargin{}, nil)
	authn := NewAuthenticator("k", "c2VjcmV0", "", clock)

	reg := metrics.NewRegistry()
	m := NewMetrics(reg)
	limiter := ratelimit.NewKeyedLimiter(0.0001, 1, time.Minute)
	csrv := New(eng, NewProducts([]string{"BTC-USD"}), authn, NewRegistry(clock),
		WithClock(clock), WithRateLimiter(limiter), WithMetrics(m))
	h := csrv.Handler()

	if rec := doReq(t, h, "/api/v3/brokerage/time"); rec.Code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", rec.Code)
	}
	rec := doReq(t, h, "/api/v3/brokerage/time")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second request status = %d, want 429", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "rate_limit_exceeded") {
		t.Fatalf("429 body missing rate_limit_exceeded: %s", rec.Body.String())
	}

	var b strings.Builder
	_ = reg.WriteText(&b)
	if !strings.Contains(b.String(), "exchange_coinbase_rate_limited_total 1") {
		t.Fatalf("rate_limited metric not incremented:\n%s", b.String())
	}
}
