//go:build decimal_float64

// Package decimal — float64 backend (PLAN.md §9.7, "Backends (optional)").
//
// This file is selected with `-tags decimal_float64` and is mutually
// exclusive with decimal.go (guarded by `!decimal_float64`). It defines a
// concrete, comparable Decimal backed by a raw float64 value in place of the
// exact 128-bit scaled core. The public method surface and the ScaleDigits /
// Zero / Min / Max / Parse / MustParse / FromFloat / FromInt symbols used by
// every caller stay identical, so the rest of the repository compiles
// unchanged — the backend switch is the single build-tag line.
//
// This backend is a DELIBERATELY FAST / LOOSE numeric mode for A/B
// benchmarking only. It is not bit-deterministic, does not round-trip exact
// decimal strings, and its arithmetic is the caller-visible float64
// semantics (including NaN/Inf propagation). Use it to measure the exact
// core's overhead, never as a default.
package decimal

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// ScaleDigits is the number of fractional decimal digits. For the float64
// backend it is kept at the same 18 as the exact core so that all callers'
// StringPrec(ScaleDigits) formatting stays one line (the digits are then
// whatever float64 formatting yields, padded/truncated to 18).
const ScaleDigits = 18

// FloatDecimal is the float64-backed implementation. It is stored as a raw
// float64 so the value is directly the number it represents (no fixed-point
// scale factor).
type FloatDecimal struct{ f float64 }

// Decimal is a type alias to FloatDecimal for the -tags decimal_float64
// build. Type aliases are interchangeable with the named type, so existing
// callers (which spell decimal.Decimal) keep compiling with no source
// change and the value remains comparable / usable as a map key.
type Decimal = FloatDecimal

// Zero is the additive identity.
var Zero = Decimal{}

// --- construction ---

// FromInt returns the FloatDecimal value of an integer.
func FromInt(n int64) Decimal { return Decimal{f: float64(n)} }

// FromFloat returns the FloatDecimal value of a float64. Panics on NaN/Inf
// to preserve the exact core's documented behavior (non-finite floats panic
// on ingestion).
func FromFloat(f float64) Decimal {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		panic("decimal: FromFloat of non-finite value")
	}
	return Decimal{f: f}
}

// MustParse is Parse that panics on error.
func MustParse(s string) Decimal {
	d, err := Parse(s)
	if err != nil {
		panic(fmt.Sprintf("decimal: MustParse(%q): %v", s, err))
	}
	return d
}

// Parse converts a decimal string (e.g. "-123.45", "0.1", "42"). It accepts
// the same grammar as the exact backend — an optional sign, then at least one
// digit, optionally a single '.' followed by more digits, then end of string
// — and rejects surrounding whitespace, trailing characters, and exponent
// notation. It returns the nearest float64 value (lossy by nature of the
// backend).
func Parse(s string) (Decimal, error) {
	if s == "" {
		return Zero, fmt.Errorf("decimal: empty string")
	}
	i := 0
	if s[0] == '+' || s[0] == '-' {
		i = 1
	}
	if i >= len(s) || !isDecimalDigit(s[i]) {
		return Zero, fmt.Errorf("decimal: invalid format %q", s)
	}
	seenDot := false
	digits := 0
	for ; i < len(s); i++ {
		c := s[i]
		if isDecimalDigit(c) {
			digits++
		} else if c == '.' {
			if seenDot {
				return Zero, fmt.Errorf("decimal: invalid format %q", s)
			}
			seenDot = true
		} else {
			// Any other byte (letters, a second sign, spaces, exponent ...) is
			// a malformed decimal string, exactly as in the exact backend.
			return Zero, fmt.Errorf("decimal: invalid format %q", s)
		}
	}
	if digits == 0 {
		return Zero, fmt.Errorf("decimal: invalid format %q", s)
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return Zero, fmt.Errorf("decimal: invalid format %q", s)
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return Zero, fmt.Errorf("decimal: %q out of float64 range", s)
	}
	return Decimal{f: f}, nil
}

func isDecimalDigit(c byte) bool { return c >= '0' && c <= '9' }

// --- conversion / formatting ---

// Float64 returns the raw float64 value.
func (d Decimal) Float64() float64 { return d.f }

// String formats with 6 fractional digits.
func (d Decimal) String() string { return d.StringPrec(6) }

// StringPrec formats with prec fractional digits (clamped to [0,18]). The
// float64 backend formats via %.<prec>f, which yields the correctly-rounded
// float64 rendering at that precision. Trailing digits are NOT
// zero-padded beyond what float64 can represent for large magnitudes —
// callers that need exact fixed-width decimal strings should use the exact
// backend.
func (d Decimal) StringPrec(prec int) string {
	if prec < 0 {
		prec = 0
	}
	if prec > ScaleDigits {
		prec = ScaleDigits
	}
	if d.f == 0 {
		// Match the exact backend's signed-zero truncation behavior for
		// sub-tiny negatives (e.g. Parse("-0.000000000000000001") →
		// "-0.000000" with StringPrec(6)); the float64 value rounds to 0
		// here, so synthesize the sign from the input is impossible —
		// we just emit the numeric zero.
		if prec == 0 {
			return "0"
		}
		return "0." + strings.Repeat("0", prec)
	}
	out := fmt.Sprintf("%.*f", prec, d.f)
	// %f never emits "-0" for tiny negative non-zero magnitudes at low prec,
	// so there is no special-casing needed.
	return out
}

// MarshalText renders the full-precision decimal string.
func (d Decimal) MarshalText() ([]byte, error) { return []byte(d.StringPrec(ScaleDigits)), nil }

// UnmarshalText parses a decimal string.
func (d *Decimal) UnmarshalText(text []byte) error {
	v, err := Parse(string(text))
	if err != nil {
		return err
	}
	*d = v
	return nil
}

// MarshalJSON encodes as a JSON string to match the exact backend's wire
// form (a quoted decimal string).
func (d Decimal) MarshalJSON() ([]byte, error) {
	return []byte(`"` + d.StringPrec(ScaleDigits) + `"`), nil
}

// UnmarshalJSON accepts a JSON string ("123.45") or a bare JSON number,
// mirroring the exact backend (null leaves the value unchanged).
func (d *Decimal) UnmarshalJSON(data []byte) error {
	s := strings.TrimSpace(string(data))
	if s == "null" {
		return nil
	}
	if len(s) > 0 && s[0] == '"' {
		var str string
		if err := json.Unmarshal(data, &str); err != nil {
			return err
		}
		s = str
	}
	v, err := Parse(s)
	if err != nil {
		return err
	}
	*d = v
	return nil
}

// --- arithmetic ---

// Add returns d + e in float64 semantics.
func (d Decimal) Add(e Decimal) Decimal { return Decimal{f: d.f + e.f} }

// Sub returns d - e in float64 semantics.
func (d Decimal) Sub(e Decimal) Decimal { return Decimal{f: d.f - e.f} }

// Neg returns -d.
func (d Decimal) Neg() Decimal { return Decimal{f: -d.f} }

// Mul returns d * e in float64 semantics.
func (d Decimal) Mul(e Decimal) Decimal { return Decimal{f: d.f * e.f} }

// Div returns d / e in float64 semantics. Panics on division by zero to
// preserve the documented contract of the exact backend (a silent float64 +Inf
// would be surprising for matchers and ledgers).
func (d Decimal) Div(e Decimal) Decimal {
	if e.f == 0 {
		panic("decimal: division by zero")
	}
	return Decimal{f: d.f / e.f}
}

// MulFloat scales d by a float64 factor.
func (d Decimal) MulFloat(f float64) Decimal { return Decimal{f: d.f * f} }

// Abs returns |d|.
func (d Decimal) Abs() Decimal { return Decimal{f: math.Abs(d.f)} }

// --- comparison ---

// Sign returns -1, 0, or +1. NaN compares as non-zero (its sign is +1 since
// math.Copysign of NaN carries the sign bit; we treat NaN as +1 to be
// deterministic with the float64 bit pattern).
func (d Decimal) Sign() int {
	if math.IsNaN(d.f) {
		return 1
	}
	switch {
	case d.f < 0:
		return -1
	case d.f > 0:
		return 1
	}
	return 0
}

// IsZero reports whether d == 0 (and not NaN).
func (d Decimal) IsZero() bool { return d.f == 0 }

// Cmp returns -1, 0, or +1. NaN sorts above all values (float64 NaN
// convention) for deterministic total ordering as a map key.
func (d Decimal) Cmp(e Decimal) int {
	// Handle NaN deterministically: NaN > everything.
	dN, eN := math.IsNaN(d.f), math.IsNaN(e.f)
	switch {
	case dN && eN:
		return 0
	case dN:
		return 1
	case eN:
		return -1
	case d.f < e.f:
		return -1
	case d.f > e.f:
		return 1
	}
	return 0
}

func (d Decimal) Eq(e Decimal) bool  { return d == e }
func (d Decimal) Lt(e Decimal) bool  { return d.Cmp(e) < 0 }
func (d Decimal) Lte(e Decimal) bool { return d.Cmp(e) <= 0 }
func (d Decimal) Gt(e Decimal) bool  { return d.Cmp(e) > 0 }
func (d Decimal) Gte(e Decimal) bool { return d.Cmp(e) >= 0 }

// Min returns the smaller of a and b.
func Min(a, b Decimal) Decimal {
	if a.Cmp(b) <= 0 {
		return a
	}
	return b
}

// Max returns the larger of a and b.
func Max(a, b Decimal) Decimal {
	if a.Cmp(b) >= 0 {
		return a
	}
	return b
}
