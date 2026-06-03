package metrics

import (
	"strings"
	"testing"
)

func TestHistogramObserveAndRender(t *testing.T) {
	r := NewRegistry()
	h := r.NewHistogram("req_latency_seconds", "request latency", []float64{0.001, 0.01, 0.1})
	// 3 observations: 0.0005 (≤ all), 0.05 (≤ 0.1), 5 (> all → only +Inf)
	h.Observe(0.0005)
	h.Observe(0.05)
	h.Observe(5)

	var sb strings.Builder
	if err := r.WriteText(&sb); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := sb.String()

	// Cumulative buckets: le=0.001 → 1, le=0.01 → 1, le=0.1 → 2, +Inf → 3.
	for _, want := range []string{
		`req_latency_seconds_bucket{le="0.001"} 1`,
		`req_latency_seconds_bucket{le="0.01"} 1`,
		`req_latency_seconds_bucket{le="0.1"} 2`,
		`req_latency_seconds_bucket{le="+Inf"} 3`,
		`req_latency_seconds_count 3`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("scrape missing %q\n---\n%s", want, out)
		}
	}
	// sum = 0.0005 + 0.05 + 5 = 5.0505
	if !strings.Contains(out, "req_latency_seconds_sum 5.0505") {
		t.Errorf("scrape missing correct sum\n---\n%s", out)
	}
}
