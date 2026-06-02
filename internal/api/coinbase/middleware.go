package coinbase

import (
	"net"
	"net/http"
	"strings"

	"github.com/zeta1999/crypto-exchange-golang/internal/metrics"
	"github.com/zeta1999/crypto-exchange-golang/internal/ratelimit"
)

// Metrics holds the counters the Coinbase edge increments. Any nil field is a
// no-op. Created via NewMetrics.
type Metrics struct {
	requests    *metrics.CounterVec // labels: endpoint, status
	rateLimited *metrics.Counter
}

// NewMetrics registers the Coinbase-edge metric families on reg.
func NewMetrics(reg *metrics.Registry) *Metrics {
	if reg == nil {
		return nil
	}
	return &Metrics{
		requests:    reg.NewCounterVec("exchange_coinbase_requests_total", "Coinbase REST edge requests by endpoint and HTTP status", "endpoint", "status"),
		rateLimited: reg.NewCounter("exchange_coinbase_rate_limited_total", "Coinbase REST requests rejected by the rate limiter (HTTP 429)"),
	}
}

// WithRateLimiter attaches a per-key token-bucket limiter. A nil limiter (or
// rate<=0) disables limiting.
func WithRateLimiter(l *ratelimit.KeyedLimiter) Option {
	return func(s *Server) { s.limiter = l }
}

// WithMetrics attaches request/rate-limit counters.
func WithMetrics(m *Metrics) Option {
	return func(s *Server) { s.metrics = m }
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.limiter != nil {
			if !s.limiter.Allow(rateKey(r)) {
				if s.metrics != nil && s.metrics.rateLimited != nil {
					s.metrics.rateLimited.Inc()
				}
				s.recordRequest(r, http.StatusTooManyRequests)
				writeError(w, errRateLimitedf())
				return
			}
		}
		if r.URL.Path == "/ws" { // WebSocket upgrade needs the raw writer.
			next.ServeHTTP(w, r)
			return
		}
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.recordRequest(r, rec.status)
	})
}

func (s *Server) recordRequest(r *http.Request, status int) {
	if s.metrics == nil || s.metrics.requests == nil {
		return
	}
	s.metrics.requests.WithLabelValues(endpointLabel(r.URL.Path), httpStatusLabel(status)).Inc()
}

func rateKey(r *http.Request) string {
	if k := r.Header.Get("CB-ACCESS-KEY"); k != "" {
		return "key:" + k
	}
	return "ip:" + clientIP(r)
}

func clientIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// endpointLabel collapses a request path to a low-cardinality endpoint label,
// folding the variable order_id / product_id path segments.
func endpointLabel(path string) string {
	const base = "/api/v3/brokerage/"
	switch path {
	case base + "time":
		return base + "time"
	case base + "product_book":
		return base + "product_book"
	case base + "orders":
		return base + "orders"
	case base + "orders/batch_cancel":
		return base + "orders/batch_cancel"
	case base + "orders/historical/batch":
		return base + "orders/historical/batch"
	case base + "accounts":
		return base + "accounts"
	case "/ws":
		return "/ws"
	}
	switch {
	case strings.HasPrefix(path, base+"products/"):
		return base + "products/{id}"
	case strings.HasPrefix(path, base+"orders/historical/"):
		return base + "orders/historical/{id}"
	default:
		return "other"
	}
}

func httpStatusLabel(status int) string {
	switch {
	case status >= 500:
		return "5xx"
	case status == http.StatusTooManyRequests:
		return "429"
	case status >= 400:
		return "4xx"
	case status >= 200 && status < 300:
		return "2xx"
	default:
		return "other"
	}
}
