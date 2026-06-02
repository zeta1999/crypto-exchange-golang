// Package metrics is a tiny, dependency-free metrics registry that emits the
// Prometheus text exposition format (version 0.0.4). It implements only what
// this project needs: monotonic Counters, settable Gauges, callback-backed
// GaugeFuncs (read at scrape time), and label-keyed CounterVec/GaugeVec.
//
// There is no client_golang dependency: the exposition format is small and
// stable, and hand-rolling it keeps the sandbox build free of new modules.
//
// All instruments are concurrency-safe (atomic where possible). Reads never
// block writers and writers never block the hot path that increments them.
package metrics

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// metricType is the Prometheus TYPE for a family.
type metricType string

const (
	typeCounter metricType = "counter"
	typeGauge   metricType = "gauge"
)

// Counter is a monotonically increasing value. Safe for concurrent use.
type Counter struct {
	v uint64 // float64 bits via math.Float64bits, CAS-updated
}

// Inc adds 1.
func (c *Counter) Inc() { c.Add(1) }

// Add adds delta (delta < 0 is ignored — counters are monotonic).
func (c *Counter) Add(delta float64) {
	if delta < 0 {
		return
	}
	for {
		old := atomic.LoadUint64(&c.v)
		nv := math.Float64bits(math.Float64frombits(old) + delta)
		if atomic.CompareAndSwapUint64(&c.v, old, nv) {
			return
		}
	}
}

// Value returns the current value.
func (c *Counter) Value() float64 { return math.Float64frombits(atomic.LoadUint64(&c.v)) }

// Gauge is a value that can go up or down. Safe for concurrent use.
type Gauge struct {
	v uint64 // float64 bits
}

// Set sets the gauge to v.
func (g *Gauge) Set(v float64) { atomic.StoreUint64(&g.v, math.Float64bits(v)) }

// Add adds delta (may be negative).
func (g *Gauge) Add(delta float64) {
	for {
		old := atomic.LoadUint64(&g.v)
		nv := math.Float64bits(math.Float64frombits(old) + delta)
		if atomic.CompareAndSwapUint64(&g.v, old, nv) {
			return
		}
	}
}

// Inc/Dec adjust by 1.
func (g *Gauge) Inc() { g.Add(1) }
func (g *Gauge) Dec() { g.Add(-1) }

// Value returns the current value.
func (g *Gauge) Value() float64 { return math.Float64frombits(atomic.LoadUint64(&g.v)) }

// family is one metric name with its help/type and its child series keyed by a
// canonical label string (empty for an unlabeled metric).
type family struct {
	name   string
	help   string
	typ    metricType
	labels []string // declared label names (order is the wire order)

	mu       sync.Mutex
	counters map[string]*Counter
	gauges   map[string]*Gauge
	funcs    map[string]func() float64
	// order preserves first-seen series ordering only as a tiebreak; output is
	// sorted by label string for determinism.
	keys []string
}

// Registry holds metric families. Safe for concurrent registration and scrape.
type Registry struct {
	mu         sync.Mutex
	families   map[string]*family
	familyKeys []string
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{families: make(map[string]*family)}
}

func (r *Registry) family(name, help string, typ metricType, labels []string) *family {
	r.mu.Lock()
	defer r.mu.Unlock()
	f, ok := r.families[name]
	if ok {
		return f
	}
	f = &family{
		name:     name,
		help:     help,
		typ:      typ,
		labels:   append([]string(nil), labels...),
		counters: make(map[string]*Counter),
		gauges:   make(map[string]*Gauge),
		funcs:    make(map[string]func() float64),
	}
	r.families[name] = f
	r.familyKeys = append(r.familyKeys, name)
	return f
}

// NewCounter registers (or returns the existing) unlabeled counter.
func (r *Registry) NewCounter(name, help string) *Counter {
	f := r.family(name, help, typeCounter, nil)
	return f.counter("")
}

// NewGauge registers (or returns the existing) unlabeled gauge.
func (r *Registry) NewGauge(name, help string) *Gauge {
	f := r.family(name, help, typeGauge, nil)
	return f.gauge("")
}

// NewGaugeFunc registers a gauge whose value is read from fn at scrape time.
func (r *Registry) NewGaugeFunc(name, help string, fn func() float64) {
	f := r.family(name, help, typeGauge, nil)
	f.gaugeFunc("", fn)
}

// CounterVec is a counter family parameterized by label values.
type CounterVec struct {
	f      *family
	labels []string
}

// NewCounterVec registers (or returns) a labeled counter family.
func (r *Registry) NewCounterVec(name, help string, labels ...string) *CounterVec {
	f := r.family(name, help, typeCounter, labels)
	return &CounterVec{f: f, labels: labels}
}

// WithLabelValues returns the counter for the given label values (lazily
// created). The number of values must match the declared labels.
func (cv *CounterVec) WithLabelValues(values ...string) *Counter {
	return cv.f.counter(seriesKey(cv.labels, values))
}

// GaugeVec is a gauge family parameterized by label values.
type GaugeVec struct {
	f      *family
	labels []string
}

// NewGaugeVec registers (or returns) a labeled gauge family.
func (r *Registry) NewGaugeVec(name, help string, labels ...string) *GaugeVec {
	f := r.family(name, help, typeGauge, labels)
	return &GaugeVec{f: f, labels: labels}
}

// WithLabelValues returns the gauge for the given label values (lazily created).
func (gv *GaugeVec) WithLabelValues(values ...string) *Gauge {
	return gv.f.gauge(seriesKey(gv.labels, values))
}

func (f *family) counter(key string) *Counter {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.counters[key]
	if !ok {
		c = &Counter{}
		f.counters[key] = c
		f.keys = append(f.keys, key)
	}
	return c
}

func (f *family) gauge(key string) *Gauge {
	f.mu.Lock()
	defer f.mu.Unlock()
	g, ok := f.gauges[key]
	if !ok {
		g = &Gauge{}
		f.gauges[key] = g
		f.keys = append(f.keys, key)
	}
	return g
}

func (f *family) gaugeFunc(key string, fn func() float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.funcs[key]; !ok {
		f.keys = append(f.keys, key)
	}
	f.funcs[key] = fn
}

// seriesKey renders the canonical label string `name="value",...` from the
// declared label names and the provided values. Excess/short values are handled
// gracefully (missing values render empty) so a mismatch never panics a scrape.
func seriesKey(names, values []string) string {
	if len(names) == 0 {
		return ""
	}
	var b strings.Builder
	for i, n := range names {
		if i > 0 {
			b.WriteByte(',')
		}
		v := ""
		if i < len(values) {
			v = values[i]
		}
		b.WriteString(n)
		b.WriteString(`="`)
		b.WriteString(escapeLabelValue(v))
		b.WriteString(`"`)
	}
	return b.String()
}

// escapeLabelValue escapes backslash, double-quote, and newline per the
// exposition format.
func escapeLabelValue(v string) string {
	if !strings.ContainsAny(v, `\"`+"\n") {
		return v
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(v)
}

// WriteText writes all families in the Prometheus text exposition format,
// sorted by family name and then by label series, with one HELP/TYPE per
// family.
func (r *Registry) WriteText(w io.Writer) error {
	r.mu.Lock()
	names := append([]string(nil), r.familyKeys...)
	fams := make([]*family, 0, len(names))
	for _, n := range names {
		fams = append(fams, r.families[n])
	}
	r.mu.Unlock()
	sort.Slice(fams, func(i, j int) bool { return fams[i].name < fams[j].name })

	for _, f := range fams {
		if err := f.writeText(w); err != nil {
			return err
		}
	}
	return nil
}

func (f *family) writeText(w io.Writer) error {
	f.mu.Lock()
	// Snapshot series values under the lock; call GaugeFuncs here (they must not
	// re-enter the registry, by contract).
	type sample struct {
		key string
		val float64
	}
	samples := make([]sample, 0, len(f.keys))
	seen := make(map[string]bool, len(f.keys))
	for _, k := range f.keys {
		if seen[k] {
			continue
		}
		seen[k] = true
		switch {
		case f.counters[k] != nil:
			samples = append(samples, sample{k, f.counters[k].Value()})
		case f.gauges[k] != nil:
			samples = append(samples, sample{k, f.gauges[k].Value()})
		case f.funcs[k] != nil:
			samples = append(samples, sample{k, f.funcs[k]()})
		}
	}
	help, typ, name := f.help, f.typ, f.name
	f.mu.Unlock()

	sort.Slice(samples, func(i, j int) bool { return samples[i].key < samples[j].key })

	var b strings.Builder
	if help != "" {
		fmt.Fprintf(&b, "# HELP %s %s\n", name, escapeHelp(help))
	}
	fmt.Fprintf(&b, "# TYPE %s %s\n", name, typ)
	for _, s := range samples {
		if s.key == "" {
			fmt.Fprintf(&b, "%s %s\n", name, formatFloat(s.val))
		} else {
			fmt.Fprintf(&b, "%s{%s} %s\n", name, s.key, formatFloat(s.val))
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// escapeHelp escapes backslash and newline in HELP text per the format.
func escapeHelp(s string) string {
	if !strings.ContainsAny(s, "\\\n") {
		return s
	}
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`).Replace(s)
}

// formatFloat renders a value the way Prometheus expects: integers without a
// decimal point, otherwise the shortest round-trippable form.
func formatFloat(v float64) string {
	if math.IsInf(v, 1) {
		return "+Inf"
	}
	if math.IsInf(v, -1) {
		return "-Inf"
	}
	if math.IsNaN(v) {
		return "NaN"
	}
	if v == math.Trunc(v) && math.Abs(v) < 1e15 {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}
