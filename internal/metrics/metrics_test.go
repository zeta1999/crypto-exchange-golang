package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func scrape(t *testing.T, r *Registry) string {
	t.Helper()
	var b strings.Builder
	if err := r.WriteText(&b); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	return b.String()
}

func TestCounterAndGauge(t *testing.T) {
	r := NewRegistry()
	c := r.NewCounter("emu_orders_total", "orders placed")
	c.Inc()
	c.Add(4)
	c.Add(-1) // ignored: monotonic
	if c.Value() != 5 {
		t.Fatalf("counter = %v, want 5", c.Value())
	}

	g := r.NewGauge("emu_resting", "resting orders")
	g.Set(10)
	g.Inc()
	g.Dec()
	g.Add(-3)
	if g.Value() != 7 {
		t.Fatalf("gauge = %v, want 7", g.Value())
	}

	out := scrape(t, r)
	want := []string{
		"# HELP emu_orders_total orders placed",
		"# TYPE emu_orders_total counter",
		"emu_orders_total 5",
		"# HELP emu_resting resting orders",
		"# TYPE emu_resting gauge",
		"emu_resting 7",
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Fatalf("scrape missing %q:\n%s", w, out)
		}
	}
	// Families sorted: emu_orders_total before emu_resting.
	if strings.Index(out, "emu_orders_total") > strings.Index(out, "emu_resting") {
		t.Fatalf("families not sorted:\n%s", out)
	}
}

func TestGaugeFunc(t *testing.T) {
	r := NewRegistry()
	v := 0.0
	r.NewGaugeFunc("emu_live", "live value", func() float64 { return v })
	v = 42.5
	out := scrape(t, r)
	if !strings.Contains(out, "emu_live 42.5") {
		t.Fatalf("gaugefunc not read at scrape:\n%s", out)
	}
}

func TestCounterVec(t *testing.T) {
	r := NewRegistry()
	cv := r.NewCounterVec("emu_feed_events_total", "feed events", "venue", "kind")
	cv.WithLabelValues("binance", "book").Add(3)
	cv.WithLabelValues("binance", "trade").Inc()
	cv.WithLabelValues("binance", "book").Inc()

	out := scrape(t, r)
	if !strings.Contains(out, `emu_feed_events_total{venue="binance",kind="book"} 4`) {
		t.Fatalf("vec book wrong:\n%s", out)
	}
	if !strings.Contains(out, `emu_feed_events_total{venue="binance",kind="trade"} 1`) {
		t.Fatalf("vec trade wrong:\n%s", out)
	}
	// Series sorted by label string: book < trade.
	if strings.Index(out, `kind="book"`) > strings.Index(out, `kind="trade"`) {
		t.Fatalf("series not sorted:\n%s", out)
	}
	// Only one HELP/TYPE per family.
	if strings.Count(out, "# TYPE emu_feed_events_total") != 1 {
		t.Fatalf("TYPE emitted more than once:\n%s", out)
	}
}

func TestGaugeVec(t *testing.T) {
	r := NewRegistry()
	gv := r.NewGaugeVec("emu_synthetic_resting", "resting synths", "instrument")
	gv.WithLabelValues("BTC-USD").Set(20)
	gv.WithLabelValues("ETH-USD").Set(15)
	out := scrape(t, r)
	if !strings.Contains(out, `emu_synthetic_resting{instrument="BTC-USD"} 20`) ||
		!strings.Contains(out, `emu_synthetic_resting{instrument="ETH-USD"} 15`) {
		t.Fatalf("gaugevec wrong:\n%s", out)
	}
}

func TestLabelEscaping(t *testing.T) {
	r := NewRegistry()
	cv := r.NewCounterVec("emu_x_total", "x", "path")
	cv.WithLabelValues(`a"b\c`).Inc()
	out := scrape(t, r)
	if !strings.Contains(out, `emu_x_total{path="a\"b\\c"} 1`) {
		t.Fatalf("label not escaped:\n%s", out)
	}
}

func TestHandler(t *testing.T) {
	r := NewRegistry()
	r.NewCounter("emu_test_total", "t").Inc()
	srv := httptest.NewServer(Handler(r))
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		t.Fatalf("content-type = %q", ct)
	}
}

func TestConcurrent(t *testing.T) {
	r := NewRegistry()
	c := r.NewCounter("emu_conc_total", "c")
	g := r.NewGauge("emu_conc_gauge", "g")
	cv := r.NewCounterVec("emu_conc_vec_total", "v", "k")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				c.Inc()
				g.Add(1)
				cv.WithLabelValues("k" + string(rune('0'+i%3))).Inc()
				_ = scrape(t, r)
			}
		}(i)
	}
	wg.Wait()
	if c.Value() != 8000 {
		t.Fatalf("counter = %v, want 8000", c.Value())
	}
}
