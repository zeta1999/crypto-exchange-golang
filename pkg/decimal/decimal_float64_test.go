//go:build decimal_float64

// Tests for the float64 backend (selected with -tags decimal_float64). The
// contract here is "a float64 behind the same API": every arithmetic operation
// must equal plain float64 arithmetic with no extra rounding, the parse/format/
// JSON surface must round-trip, the value must stay comparable and usable as a
// map key, and the documented panic contracts (Div by zero; FromFloat of a
// non-finite value) must hold. This validates the backend against the
// ground-truth float64 ops themselves — the independent A/B reference it
// exists to be measured against.
package decimal

import (
	"encoding/json"
	"math"
	"testing"
)

// eq reports IEEE-754 bit-identical equality (so we can assert the float
// backend adds no rounding beyond the underlying float64 op).
func eq(a, b float64) bool { return math.Float64bits(a) == math.Float64bits(b) }

func TestFloatParseRoundTrip(t *testing.T) {
	cases := map[string]float64{
		"0": 0, "42": 42, "-42": -42, "0.1": 0.1, "123.456": 123.456,
		"-0.000000000000000001": -0.000000000000000001, "70000.12345678": 70000.12345678,
	}
	for in, want := range cases {
		d, err := Parse(in)
		if err != nil {
			t.Fatalf("Parse(%q): %v", in, err)
		}
		if !eq(d.Float64(), want) {
			t.Errorf("Parse(%q).Float64() = %v, want %v", in, d.Float64(), want)
		}
	}
}

func TestFloatParseErrors(t *testing.T) {
	// Same malformed grammar as the exact backend must be rejected.
	for _, bad := range []string{"", "abc", "1.2.3", "+", "-", ".5", "1e3", " 1", "1 ", "+-1", "12x"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) should error", bad)
		}
	}
}

func TestFloatConstruction(t *testing.T) {
	if !eq(FromInt(5).Float64(), 5) {
		t.Error("FromInt(5)")
	}
	if !eq(FromFloat(0.25).Float64(), 0.25) {
		t.Error("FromFloat(0.25)")
	}
	if !eq(MustParse("3.5").Float64(), 3.5) {
		t.Error("MustParse(3.5)")
	}
}

func TestFloatArithmeticIsPlainFloat64(t *testing.T) {
	// For a spread of values (including non-dyadic ones that expose any hidden
	// rounding), each op must bit-match the plain float64 counterpart.
	vals := []float64{0, 1, -1, 0.5, 0.25, 0.1, 3.5, 123.456, -123.456, 1e6, 1e-3, 70000.12345678, -0.000000000000000001}
	for _, a := range vals {
		af := FromFloat(a)
		for _, b := range vals {
			bf := FromFloat(b)
			if got := af.Add(bf).Float64(); !eq(got, a+b) {
				t.Fatalf("Add(%v,%v) = %v, want %v", a, b, got, a+b)
			}
			if got := af.Sub(bf).Float64(); !eq(got, a-b) {
				t.Fatalf("Sub(%v,%v) = %v, want %v", a, b, got, a-b)
			}
			if got := af.Mul(bf).Float64(); !eq(got, a*b) {
				t.Fatalf("Mul(%v,%v) = %v, want %v", a, b, got, a*b)
			}
			f := af.Neg().Float64()
			if !eq(f, -a) {
				t.Fatalf("Neg(%v) = %v, want %v", a, f, -a)
			}
			ab := af.Abs().Float64()
			if !eq(ab, math.Abs(a)) {
				t.Fatalf("Abs(%v) = %v, want %v", a, ab, math.Abs(a))
			}
			if math.IsNaN(b) || b == 0 {
				continue // Div by zero handled separately; NaN not in our value set
			}
			if got := af.Div(bf).Float64(); !eq(got, a/b) {
				t.Fatalf("Div(%v,%v) = %v, want %v", a, b, got, a/b)
			}
		}
		if got := af.MulFloat(1.5).Float64(); !eq(got, a*1.5) {
			t.Fatalf("MulFloat(%v,1.5) = %v, want %v", a, got, a*1.5)
		}
	}
}

func TestFloatDivZeroPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("Div by zero should panic")
		}
	}()
	FromInt(1).Div(Zero)
}

func TestFloatFromFloatNonFinitePanics(t *testing.T) {
	mustPanic := func(name string, f func()) {
		defer func() {
			if recover() == nil {
				t.Errorf("%s: expected panic", name)
			}
		}()
		f()
	}
	mustPanic("NaN", func() { FromFloat(math.NaN()) })
	mustPanic("+Inf", func() { FromFloat(math.Inf(1)) })
	mustPanic("-Inf", func() { FromFloat(math.Inf(-1)) })
}

func TestFloatSignZeroCmp(t *testing.T) {
	if !Zero.IsZero() || Zero.Sign() != 0 {
		t.Error("Zero")
	}
	a, b := MustParse("-1.5"), MustParse("2.25")
	if a.Sign() != -1 || b.Sign() != 1 {
		t.Error("sign")
	}
	if a.Cmp(b) != -1 || b.Cmp(a) != 1 || a.Cmp(a) != 0 {
		t.Error("cmp")
	}
	if !a.Lt(b) || !b.Gt(a) || !a.Lte(a) || !b.Gte(b) {
		t.Error("ordering helpers")
	}
	if Min(a, b) != a || Max(a, b) != b {
		t.Error("min/max")
	}
	// NaN sorts deterministically above all values in Cmp, and signs as +1.
	nan := Decimal{f: math.NaN()}
	if nan.Cmp(a) != 1 || a.Cmp(nan) != -1 || nan.Cmp(Decimal{f: math.NaN()}) != 0 {
		t.Error("NaN total-order cmp")
	}
	if nan.Sign() != 1 {
		t.Error("NaN sign")
	}
	// Limitation pinned explicitly: maps and == use the float64 bits (NaN !=
	// NaN), so a NaN value CANNOT round-trip as a map key even though Cmp(n,n)
	// is 0. A NaN is unreachable through the constructors, but reachable via
	// float64 overflow arithmetic (Inf - Inf), which is why this backend is
	// bench-only, never for order books.
	m := map[Decimal]int{}
	m[nan] = 1
	if _, ok := m[nan]; ok {
		t.Error("expected NaN map-key lookup to MISS (documented limitation)")
	}
	if nan.Eq(Decimal{f: math.NaN()}) {
		t.Error("Eq on NaN must be false (Go == semantics)")
	}
}

func TestFloatUsableAsMapKeyAndEq(t *testing.T) {
	m := map[Decimal]int{}
	m[MustParse("1.5")]++
	m[MustParse("1.5")]++
	m[MustParse("1.50")]++ // same float64 value, same key
	if m[MustParse("1.5")] != 3 {
		t.Errorf("map key: got %d, want 3", m[MustParse("1.5")])
	}
	if !MustParse("3.5").Eq(MustParse("3.50")) {
		t.Error("Eq should hold for equal values")
	}
}

func TestFloatStringFormat(t *testing.T) {
	if got := Zero.String(); got != "0.000000" {
		t.Errorf("Zero.String() = %q, want 0.000000", got)
	}
	if got := MustParse("3.5").String(); got != "3.500000" {
		t.Errorf("3.5.String() = %q, want 3.500000", got)
	}
	// String() == StringPrec(6); larger precision is accepted and clamped.
	if got := MustParse("3.5").StringPrec(6); got != "3.500000" {
		t.Errorf("StringPrec(6) = %q", got)
	}
	if got := MustParse("3.5").StringPrec(20); got != MustParse("3.5").StringPrec(18) {
		t.Errorf("StringPrec(20) should clamp to 18 digits")
	}
}

func TestFloatJSONRoundTrip(t *testing.T) {
	d := MustParse("70123.45")
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	// The float backend emits the same quoted wire form as the exact backend
	// (a string with 18 fractional digits), but reflects the float64 value:
	// 70123.45 is not exactly representable, so the digits are its float64
	// rendering. What must hold: the wire form is a 18-fraction quoted string
	// whose parsed value equals the float64 input.
	const want = `"70123.449999999997089617"`
	if string(b) != want {
		t.Errorf("MarshalJSON = %s, want %s", b, want)
	}
	var got Decimal
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if !eq(got.Float64(), d.Float64()) {
		t.Errorf("round-trip: got %v want %v", got.Float64(), d.Float64())
	}
	// Bare JSON number is also accepted.
	if err := json.Unmarshal([]byte(`0.5`), &got); err != nil || !eq(got.Float64(), 0.5) {
		t.Errorf("bare number unmarshal: %v err=%v", got.Float64(), err)
	}
}

func TestFloatBenchmarkOps(t *testing.T) {
	// The whole point of the loose backend is speed; prove it is allocation-free
	// (plain float64 ops) so A/B benchmarks are meaningful.
	a, b := MustParse("12345.678901234567"), MustParse("9.87654321")
	if n := testing.AllocsPerRun(1000, func() {
		_ = a.Add(b)
	}); n != 0 {
		t.Errorf("Add allocates %v times/op, want 0", n)
	}
	if n := testing.AllocsPerRun(1000, func() {
		_ = a.Mul(b)
	}); n != 0 {
		t.Errorf("Mul allocates %v times/op, want 0", n)
	}
}

// BenchmarkFloatMul is the A/B half of the loose-backend benchmark: it reports
// the float64 backend's Mul throughput next to the exact core's (run
// `go test -tags decimal_float64 ./pkg/decimal/ -run ^ -bench BenchmarkMul`
// against the untagged default build). The float backend's whole purpose is to
// measure the exact core's overhead, so both benchmark the same shape of op.
func BenchmarkFloatMul(b *testing.B) {
	x, y := MustParse("70123.45"), MustParse("0.00318")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = x.Mul(y)
	}
}

func BenchmarkFloatAddDiv(b *testing.B) {
	x, y := MustParse("70123.45"), MustParse("0.00318")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = x.Add(y)
		_ = x.Div(y)
	}
}
