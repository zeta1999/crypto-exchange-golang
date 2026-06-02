package decimal

import (
	"encoding/json"
	"math"
	"math/big"
	"math/rand"
	"testing"
)

func TestParseStringRoundTrip(t *testing.T) {
	cases := []struct {
		in   string
		out  string // StringPrec(18)
		frac int
		out6 string // StringPrec(6)
	}{
		{"0", "0.000000000000000000", 18, "0.000000"},
		{"42", "42.000000000000000000", 18, "42.000000"},
		{"-42", "-42.000000000000000000", 18, "-42.000000"},
		{"0.1", "0.100000000000000000", 18, "0.100000"},
		{"123.456", "123.456000000000000000", 18, "123.456000"},
		{"-0.000000000000000001", "-0.000000000000000001", 18, "-0.000000"}, // negative truncates to -0.000000 (sign preserved)
		{"70000.12345678", "70000.123456780000000000", 18, "70000.123456"},
	}
	for _, c := range cases {
		d, err := Parse(c.in)
		if err != nil {
			t.Fatalf("Parse(%q): %v", c.in, err)
		}
		if got := d.StringPrec(18); got != c.out {
			t.Errorf("Parse(%q).StringPrec(18) = %q, want %q", c.in, got, c.out)
		}
		if got := d.StringPrec(6); got != c.out6 {
			t.Errorf("Parse(%q).StringPrec(6) = %q, want %q", c.in, got, c.out6)
		}
	}
}

func TestParseTruncatesAndPads(t *testing.T) {
	// More than 18 fractional digits: extra are truncated.
	d := MustParse("1.1234567890123456789999")
	if got := d.StringPrec(18); got != "1.123456789012345678" {
		t.Errorf("truncation: got %q", got)
	}
}

func TestParseErrors(t *testing.T) {
	// Note: "1." is accepted as 1.0 (trailing dot, no fractional digits), matching
	// the reference parser; ".5" errors (must start with a digit after any sign).
	for _, bad := range []string{"", "abc", "1.2.3", "+", "-", ".5", "1e3", " 1", "1 ", "+-1", "12x"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) should error", bad)
		}
	}
}

// ratOf returns the exact rational value of d.
func ratOf(d Decimal) *big.Rat { return new(big.Rat).SetFrac(d.toBig(), scaleBig) }

// truncToRaw truncates a rational value (in decimal units) to a scaled integer
// (toward zero) — the reference semantics for Mul/Div.
func truncToRaw(r *big.Rat) *big.Int {
	scaled := new(big.Rat).Mul(r, new(big.Rat).SetInt(scaleBig))
	return new(big.Int).Quo(scaled.Num(), scaled.Denom())
}

func TestArithmeticAgainstRatOracle(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	randDec := func() Decimal {
		// Values up to ~±1e8 with full fractional precision.
		raw := big.NewInt(rng.Int63n(2e17) - 1e17)
		raw.Mul(raw, big.NewInt(rng.Int63n(1_000_000)+1))
		return fromBig(raw)
	}
	for i := 0; i < 5000; i++ {
		a, b := randDec(), randDec()

		// Add/Sub are exact.
		if got, want := a.Add(b).toBig(), new(big.Int).Add(a.toBig(), b.toBig()); got.Cmp(want) != 0 {
			t.Fatalf("Add: got %v want %v", got, want)
		}
		if got, want := a.Sub(b).toBig(), new(big.Int).Sub(a.toBig(), b.toBig()); got.Cmp(want) != 0 {
			t.Fatalf("Sub: got %v want %v", got, want)
		}
		// Mul/Div match truncated rational arithmetic.
		if got, want := a.Mul(b).toBig(), truncToRaw(new(big.Rat).Mul(ratOf(a), ratOf(b))); got.Cmp(want) != 0 {
			t.Fatalf("Mul(%v,%v): got %v want %v", a, b, got, want)
		}
		if !b.IsZero() {
			if got, want := a.Div(b).toBig(), truncToRaw(new(big.Rat).Quo(ratOf(a), ratOf(b))); got.Cmp(want) != 0 {
				t.Fatalf("Div(%v,%v): got %v want %v", a, b, got, want)
			}
		}
	}
}

func TestMulDivIdentities(t *testing.T) {
	a := MustParse("123.456")
	b := MustParse("0.001")
	if got := a.Mul(b); got.StringPrec(18) != "0.123456000000000000" {
		t.Errorf("123.456 * 0.001 = %q", got.StringPrec(18))
	}
	// (a / b) * b ≈ a for exact divisors.
	ten := FromInt(10)
	if got := FromInt(100).Div(ten); !got.Eq(ten) {
		t.Errorf("100/10 = %v, want 10", got)
	}
}

func TestSignZeroCmp(t *testing.T) {
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
	if Min(a, b) != a || Max(a, b) != b || a.Abs() != MustParse("1.5") {
		t.Error("min/max/abs")
	}
	if a.Neg() != MustParse("1.5") {
		t.Error("neg")
	}
}

func TestCmpAcrossWordBoundary(t *testing.T) {
	// Values whose low words compare differently than signed would.
	big1 := MustParse("18446744073.709551616") // ~2^64 / 1e9, exercises hi/lo split
	big2 := MustParse("18446744073.709551617")
	if big1.Cmp(big2) != -1 {
		t.Errorf("cmp across boundary: %v vs %v", big1, big2)
	}
}

func TestOverflowAndDivZeroPanic(t *testing.T) {
	mustPanic := func(name string, f func()) {
		defer func() {
			if recover() == nil {
				t.Errorf("%s: expected panic", name)
			}
		}()
		f()
	}
	// Near max representable (~1.7e38; scaled ~1.7e20 integer part).
	huge := MustParse("100000000000000000000") // 1e20
	mustPanic("Mul overflow", func() { huge.Mul(huge) })
	mustPanic("Div by zero", func() { FromInt(1).Div(Zero) })

	// Add overflow at the very top of the range.
	maxD := fromBig(maxPos)
	mustPanic("Add overflow", func() { maxD.Add(FromRaw(0, 1)) })
}

func TestParseOverflowErrors(t *testing.T) {
	if _, err := Parse("1000000000000000000000000000000000000000"); err == nil {
		t.Error("expected overflow error on huge integral")
	}
}

func TestJSONRoundTrip(t *testing.T) {
	d := MustParse("70123.45")
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `"70123.450000000000000000"` {
		t.Errorf("MarshalJSON = %s", b)
	}
	var got Decimal
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got != d {
		t.Errorf("round-trip: got %v want %v", got, d)
	}
	// Bare JSON number is also accepted.
	if err := json.Unmarshal([]byte(`0.5`), &got); err != nil || got != MustParse("0.5") {
		t.Errorf("bare number unmarshal: %v err=%v", got, err)
	}
}

func TestUnmarshalJSONRobustness(t *testing.T) {
	var d Decimal
	// One-sided / malformed quotes must error, not silently parse.
	for _, bad := range []string{`"5`, `5"`, `"1.2.3"`, `"abc"`, `1e3`} {
		if err := d.UnmarshalJSON([]byte(bad)); err == nil {
			t.Errorf("UnmarshalJSON(%s) should error", bad)
		}
	}
	// null leaves the value unchanged.
	d = MustParse("7")
	if err := d.UnmarshalJSON([]byte("null")); err != nil || d != MustParse("7") {
		t.Errorf("null handling: d=%v err=%v", d, err)
	}
	// Quoted and bare both parse.
	if err := d.UnmarshalJSON([]byte(`"1.25"`)); err != nil || d != MustParse("1.25") {
		t.Errorf("quoted: d=%v err=%v", d, err)
	}
	if err := d.UnmarshalJSON([]byte(`2.5`)); err != nil || d != MustParse("2.5") {
		t.Errorf("bare: d=%v err=%v", d, err)
	}
}

func TestFromFloatNonFinitePanics(t *testing.T) {
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

func TestUsableAsMapKey(t *testing.T) {
	m := map[Decimal]int{}
	m[MustParse("1.5")]++
	m[MustParse("1.5")]++
	m[MustParse("1.50")]++ // same value, same key
	if m[MustParse("1.5")] != 3 {
		t.Errorf("map key: got %d, want 3", m[MustParse("1.5")])
	}
}

func TestFromIntFloat(t *testing.T) {
	if FromInt(5) != MustParse("5") {
		t.Error("FromInt")
	}
	d := FromFloat(0.25)
	if d != MustParse("0.25") {
		t.Errorf("FromFloat(0.25) = %v", d)
	}
	if got := MustParse("3.5").Float64(); got != 3.5 {
		t.Errorf("Float64 = %v", got)
	}
}

func TestMulIsAllocationFree(t *testing.T) {
	a, b := MustParse("12345.678901234567"), MustParse("9.87654321")
	if n := testing.AllocsPerRun(1000, func() { _ = a.Mul(b) }); n != 0 {
		t.Errorf("Mul allocates %v times/op, want 0", n)
	}
}

func BenchmarkMul(b *testing.B) {
	x, y := MustParse("70123.45"), MustParse("0.00318")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = x.Mul(y)
	}
}

func TestDivTruncatesTowardZero(t *testing.T) {
	// 1/3 and -1/3 both truncate toward zero (not floor), to 18 digits.
	if got := FromInt(1).Div(FromInt(3)).StringPrec(18); got != "0.333333333333333333" {
		t.Errorf("1/3 = %q", got)
	}
	if got := FromInt(-1).Div(FromInt(3)).StringPrec(18); got != "-0.333333333333333333" {
		t.Errorf("-1/3 = %q (must truncate toward zero, not floor)", got)
	}
	// Exact and sign combinations.
	if got := MustParse("-7").Div(FromInt(2)); got != MustParse("-3.5") {
		t.Errorf("-7/2 = %v", got)
	}
	if got := MustParse("100").Div(MustParse("-4")); got != MustParse("-25") {
		t.Errorf("100/-4 = %v", got)
	}
}

func TestDivOverflowPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected overflow panic")
		}
	}()
	// 1e20 / 1e-18 = 1e38 — exceeds the 128-bit range.
	MustParse("100000000000000000000").Div(MustParse("0.000000000000000001"))
}

func TestDivIsAllocationFree(t *testing.T) {
	a, b := MustParse("70123.456789"), MustParse("3.14159")
	if n := testing.AllocsPerRun(1000, func() { _ = a.Div(b) }); n != 0 {
		t.Errorf("Div allocates %v times/op, want 0", n)
	}
}

func BenchmarkDiv(b *testing.B) {
	x, y := MustParse("70123.45"), MustParse("3.14159")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = x.Div(y)
	}
}
