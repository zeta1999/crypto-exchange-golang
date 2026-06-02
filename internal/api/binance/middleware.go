package binance

import (
	"net"
	"net/http"

	"github.com/zeta1999/crypto-exchange-golang/internal/metrics"
	"github.com/zeta1999/crypto-exchange-golang/internal/ratelimit"
)

// Metrics holds the counters the Binance edge increments. Any nil field is a
// no-op (so metrics are entirely optional). Created via NewMetrics.
type Metrics struct {
	requests    *metrics.CounterVec // labels: endpoint, status
	rateLimited *metrics.Counter
}

// NewMetrics registers the Binance-edge metric families on reg.
func NewMetrics(reg *metrics.Registry) *Metrics {
	if reg == nil {
		return nil
	}
	return &Metrics{
		requests:    reg.NewCounterVec("exchange_binance_requests_total", "Binance REST edge requests by endpoint and HTTP status", "endpoint", "status"),
		rateLimited: reg.NewCounter("exchange_binance_rate_limited_total", "Binance REST requests rejected by the rate limiter (HTTP 429 / code -1003)"),
	}
}

// WithRateLimiter attaches a per-key token-bucket limiter to the edge. A nil
// limiter (or one with rate<=0) disables limiting.
func WithRateLimiter(l *ratelimit.KeyedLimiter) Option {
	return func(s *Server) { s.limiter = l }
}

// WithMetrics attaches request/rate-limit counters.
func WithMetrics(m *Metrics) Option {
	return func(s *Server) { s.metrics = m }
}

// statusRecorder captures the response status for metrics.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// middleware wraps the mux with rate limiting (on a configured limiter) and
// request metrics. WebSocket upgrade paths bypass the status recorder's
// buffering concerns by simply not wrapping hijack — net/http's hijacker is
// preserved because we embed ResponseWriter, but to be safe WS paths skip the
// recorder.
func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Rate limit before any work. Key by API key when present, else remote IP.
		if s.limiter != nil {
			if !s.limiter.Allow(rateKey(r)) {
				if s.metrics != nil && s.metrics.rateLimited != nil {
					s.metrics.rateLimited.Inc()
				}
				s.recordRequest(r, http.StatusTooManyRequests)
				writeError(w, errTooManyRequests())
				return
			}
		}
		// WebSocket endpoints need the raw ResponseWriter for hijacking; don't wrap.
		if isWSPath(r.URL.Path) {
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

func isWSPath(path string) bool {
	return path == "/stream" || (len(path) >= 4 && path[:4] == "/ws/")
}

// rateKey returns the limiter key: the API key header if present, else the
// client IP (so unauthenticated public traffic is still bounded per source).
func rateKey(r *http.Request) string {
	if k := r.Header.Get("X-MBX-APIKEY"); k != "" {
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

// endpointLabel collapses a request path to a low-cardinality endpoint label.
func endpointLabel(path string) string {
	switch {
	case path == "/api/v3/order":
		return "/api/v3/order"
	case path == "/api/v3/openOrders":
		return "/api/v3/openOrders"
	case path == "/api/v3/account":
		return "/api/v3/account"
	case path == "/api/v3/depth":
		return "/api/v3/depth"
	case path == "/api/v3/ticker/price":
		return "/api/v3/ticker/price"
	case path == "/api/v3/ping":
		return "/api/v3/ping"
	case path == "/api/v3/time":
		return "/api/v3/time"
	case path == "/api/v3/exchangeInfo":
		return "/api/v3/exchangeInfo"
	case path == "/api/v3/userDataStream":
		return "/api/v3/userDataStream"
	case path == "/stream":
		return "/stream"
	case len(path) >= 4 && path[:4] == "/ws/":
		return "/ws/"
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
