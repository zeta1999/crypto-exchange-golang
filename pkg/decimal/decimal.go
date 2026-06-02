// Package decimal provides an exact base-10 fixed-point number for market
// prices and quantities. A Decimal has 18 fractional digits and is stored as
// a signed 128-bit integer of scaled units (value = raw / 10^18), so decimal
// venue prices round-trip losslessly and matching is bit-deterministic —
// unlike float64. See PLAN.md §9.
//
// The 128-bit value is held as a two's-complement {hi int64, lo uint64} pair,
// which keeps Decimal a comparable value type (usable directly as a map key).
// Add/Sub use math/bits with signed-overflow detection; Mul/Div use big.Int
// intermediates (a correct first cut — the hot paths can later move to
// allocation-free limb math without changing this API).
package decimal

import (
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"math/bits"
	"strings"
)

// ScaleDigits is the number of fractional decimal digits.
const ScaleDigits = 18

// scaleU is 10^18, the scale factor. It fits in a uint64 (10^18 ≈ 1.15e18 < 2^63).
const scaleU uint64 = 1_000_000_000_000_000_000

var (
	scaleBig = new(big.Int).SetUint64(scaleU)
	two128   = new(big.Int).Lsh(big.NewInt(1), 128)                                  // 2^128
	maxPos   = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 127), big.NewInt(1)) // 2^127 - 1
	minNeg   = new(big.Int).Neg(new(big.Int).Lsh(big.NewInt(1), 127))                // -2^127
)

// Decimal is an exact base-10 fixed-point number (18 fractional digits).
// The zero value is 0.
type Decimal struct {
	hi int64
	lo uint64
}

// Zero is the additive identity.
var Zero = Decimal{}

// --- construction ---

// FromRaw builds a Decimal from its scaled 128-bit representation.
func FromRaw(hi int64, lo uint64) Decimal { return Decimal{hi: hi, lo: lo} }

// Raw returns the scaled 128-bit representation (high/low words).
func (d Decimal) Raw() (hi int64, lo uint64) { return d.hi, d.lo }

// FromInt returns the Decimal value of an integer. It never overflows: every
// int64 (|n| ≤ ~9.2e18) scaled by 10^18 stays within the 128-bit range
// (integer part reaches ~±1.7e20).
func FromInt(n int64) Decimal {
	return fromBig(new(big.Int).Mul(big.NewInt(n), scaleBig))
}

// FromFloat converts a float64, rounding to the nearest 1e-18. It is lossy
// (floats can't represent most decimals exactly) and intended for ingestion
// convenience and edge scaling, not for authoritative price/quantity values —
// prefer Parse on the venue's decimal string. Panics on NaN/±Inf and on
// 128-bit overflow.
func FromFloat(f float64) Decimal {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		panic("decimal: FromFloat of non-finite value")
	}
	bf := new(big.Float).SetPrec(200).SetFloat64(f)
	bf.Mul(bf, new(big.Float).SetInt(scaleBig))
	// Round half away from zero.
	if f < 0 {
		bf.Sub(bf, big.NewFloat(0.5))
	} else {
		bf.Add(bf, big.NewFloat(0.5))
	}
	raw, _ := bf.Int(nil)
	return fromBig(raw)
}

// MustParse is Parse that panics on error; for constants and tests.
func MustParse(s string) Decimal {
	d, err := Parse(s)
	if err != nil {
		panic(fmt.Sprintf("decimal: MustParse(%q): %v", s, err))
	}
	return d
}

// Parse converts a decimal string ("-123.456", "0.1", "42") exactly. Up to 18
// fractional digits are kept; extra digits are truncated and missing digits
// are zero-padded. It errors on malformed input or on overflow of the 128-bit
// range.
func Parse(s string) (Decimal, error) {
	if s == "" {
		return Decimal{}, fmt.Errorf("decimal: empty string")
	}
	neg := false
	i := 0
	if s[0] == '+' || s[0] == '-' {
		neg = s[0] == '-'
		i++
	}
	if i >= len(s) || !isDigit(s[i]) {
		return Decimal{}, fmt.Errorf("decimal: invalid format %q", s)
	}

	integral := new(big.Int)
	for i < len(s) && isDigit(s[i]) {
		integral.Mul(integral, big.NewInt(10))
		integral.Add(integral, big.NewInt(int64(s[i]-'0')))
		i++
	}

	fractional := new(big.Int)
	fracDigits := 0
	if i < len(s) {
		if s[i] != '.' {
			return Decimal{}, fmt.Errorf("decimal: invalid format %q", s)
		}
		i++
		for i < len(s) && isDigit(s[i]) {
			if fracDigits < ScaleDigits {
				fractional.Mul(fractional, big.NewInt(10))
				fractional.Add(fractional, big.NewInt(int64(s[i]-'0')))
				fracDigits++
			}
			i++
		}
		for fracDigits < ScaleDigits {
			fractional.Mul(fractional, big.NewInt(10))
			fracDigits++
		}
	}
	if i != len(s) {
		return Decimal{}, fmt.Errorf("decimal: invalid format %q", s)
	}

	raw := new(big.Int).Mul(integral, scaleBig)
	raw.Add(raw, fractional)
	if neg {
		raw.Neg(raw)
	}
	d, ok := fromBigChecked(raw)
	if !ok {
		return Decimal{}, fmt.Errorf("decimal: %q overflows 128-bit range", s)
	}
	return d, nil
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// --- conversion / formatting ---

// Float64 returns the value as a float64 (lossy).
func (d Decimal) Float64() float64 {
	f, _ := new(big.Rat).SetFrac(d.toBig(), scaleBig).Float64()
	return f
}

// String formats with 6 fractional digits (trailing digits truncated).
func (d Decimal) String() string { return d.StringPrec(6) }

// StringPrec formats with prec fractional digits (clamped to [0,18]), trimming
// (not rounding) excess precision.
func (d Decimal) StringPrec(prec int) string {
	if prec < 0 {
		prec = 0
	}
	if prec > ScaleDigits {
		prec = ScaleDigits
	}
	neg := d.Sign() < 0
	abs := new(big.Int).Abs(d.toBig())
	integral := new(big.Int)
	frac := new(big.Int)
	integral.DivMod(abs, scaleBig, frac)

	var b strings.Builder
	if neg {
		b.WriteByte('-')
	}
	b.WriteString(integral.String())
	if prec > 0 {
		// Render the fractional part as exactly 18 zero-padded digits, then trim.
		fs := frac.String()
		fs = strings.Repeat("0", ScaleDigits-len(fs)) + fs
		b.WriteByte('.')
		b.WriteString(fs[:prec])
	}
	return b.String()
}

// MarshalText renders the full-precision decimal string (18 digits).
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

// MarshalJSON encodes as a JSON string to preserve exactness on the wire.
func (d Decimal) MarshalJSON() ([]byte, error) {
	return []byte(`"` + d.StringPrec(ScaleDigits) + `"`), nil
}

// UnmarshalJSON accepts a JSON string ("123.45") or a bare JSON number
// (123.45); JSON null leaves the value unchanged. A quoted value is decoded as
// a proper JSON string (so escapes and one-sided quotes are handled
// correctly). Exponent notation (1.5e2) is not supported — venues quote exact
// decimal strings, which is the intended wire form.
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

// Add returns d + e. Panics on 128-bit overflow.
func (d Decimal) Add(e Decimal) Decimal {
	lo, carry := bits.Add64(d.lo, e.lo, 0)
	hi := d.hi + e.hi + int64(carry)
	// Signed overflow: operands share a sign but the result's differs.
	if (d.hi < 0) == (e.hi < 0) && (hi < 0) != (d.hi < 0) {
		panic("decimal: Add overflow")
	}
	return Decimal{hi: hi, lo: lo}
}

// Sub returns d - e. Panics on 128-bit overflow.
func (d Decimal) Sub(e Decimal) Decimal {
	lo, borrow := bits.Sub64(d.lo, e.lo, 0)
	hi := d.hi - e.hi - int64(borrow)
	// Signed overflow: operands differ in sign and the result's sign differs from d's.
	if (d.hi < 0) != (e.hi < 0) && (hi < 0) != (d.hi < 0) {
		panic("decimal: Sub overflow")
	}
	return Decimal{hi: hi, lo: lo}
}

// Neg returns -d.
func (d Decimal) Neg() Decimal { return Zero.Sub(d) }

// Mul returns d * e (= d.raw * e.raw / scale, truncated toward zero). Panics
// on overflow. Allocation-free: it does the 128×128→256 product and the ÷scale
// in 64-bit limbs via math/bits (validated against a big.Rat oracle in tests).
func (d Decimal) Mul(e Decimal) Decimal {
	amhi, amlo, aneg := absMag(d)
	bmhi, bmlo, bneg := absMag(e)
	// 128×128 → 256-bit product of the magnitudes (p0 least significant).
	p0, p1, p2, p3 := mul128to256(amhi, amlo, bmhi, bmlo)
	// Divide the 256-bit magnitude by scale (a uint64); truncates toward zero.
	q0, q1, q2, q3 := div256by64(p0, p1, p2, p3, scaleU)
	if q2 != 0 || q3 != 0 { // quotient exceeds 128 bits
		panic("decimal: value overflows 128-bit range")
	}
	return fromMag(q1, q0, aneg != bneg)
}

// Div returns d / e (= d.raw * scale / e.raw, truncated toward zero). Panics on
// division by zero or overflow. Allocation-free: the numerator |d|·scale is a
// 128×64→≤192-bit product (in 256-bit limbs) and the ÷|e| is a 256÷128 binary
// long division (div256by128). Validated against a big.Rat oracle in tests.
//
// (Binary long division, not Knuth Algorithm D: it's O(256) iterations vs
// Knuth's O(limbs²), but Div is a rare path — mid/average prices, not the
// matching hot loop — and it's far easier to prove correct. Revisit with Knuth
// D only if Div ever shows up hot.)
func (d Decimal) Div(e Decimal) Decimal {
	if e.IsZero() {
		panic("decimal: division by zero")
	}
	amhi, amlo, aneg := absMag(d)
	bmhi, bmlo, bneg := absMag(e)
	// Numerator = |d| × scale (scale fits in 64 bits, so bhi=0).
	n0, n1, n2, n3 := mul128to256(amhi, amlo, 0, scaleU)
	qhi, qlo, ok := div256by128(n0, n1, n2, n3, bmhi, bmlo)
	if !ok {
		panic("decimal: value overflows 128-bit range")
	}
	return fromMag(qhi, qlo, aneg != bneg)
}

// MulFloat scales d by a float64 factor (doubly lossy: d→float64 then the
// product→Decimal). For non-exact edge scaling such as the RTR convergence
// fraction; not for authoritative values. Panics if d is large enough that
// d.Float64()*f overflows the 128-bit range, so keep operands modest.
func (d Decimal) MulFloat(f float64) Decimal { return FromFloat(d.Float64() * f) }

// Abs returns |d|.
func (d Decimal) Abs() Decimal {
	if d.Sign() < 0 {
		return d.Neg()
	}
	return d
}

// --- comparison ---

// Sign returns -1, 0, or +1.
func (d Decimal) Sign() int {
	if d.hi < 0 {
		return -1
	}
	if d.hi == 0 && d.lo == 0 {
		return 0
	}
	return 1
}

// IsZero reports whether d == 0.
func (d Decimal) IsZero() bool { return d.hi == 0 && d.lo == 0 }

// Cmp returns -1, 0, or +1 as d is less than, equal to, or greater than e.
func (d Decimal) Cmp(e Decimal) int {
	if d.hi != e.hi {
		if d.hi < e.hi {
			return -1
		}
		return 1
	}
	if d.lo != e.lo {
		if d.lo < e.lo { // low word is unsigned
			return -1
		}
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

// --- allocation-free 128/256-bit limb math (used by Mul) ---

const signBit = uint64(1) << 63

// absMag returns the unsigned 128-bit magnitude of d and whether it was negative.
func absMag(d Decimal) (mhi, mlo uint64, neg bool) {
	if d.hi < 0 {
		// Negate the two's-complement 128-bit value to get the magnitude.
		lo, borrow := bits.Sub64(0, d.lo, 0)
		hi, _ := bits.Sub64(0, uint64(d.hi), borrow)
		return hi, lo, true
	}
	return uint64(d.hi), d.lo, false
}

// mul128to256 returns the 256-bit product of two 128-bit magnitudes as four
// 64-bit limbs, p0 least significant.
func mul128to256(ahi, alo, bhi, blo uint64) (p0, p1, p2, p3 uint64) {
	llHi, llLo := bits.Mul64(alo, blo)
	lhHi, lhLo := bits.Mul64(alo, bhi)
	hlHi, hlLo := bits.Mul64(ahi, blo)
	hhHi, hhLo := bits.Mul64(ahi, bhi)

	p0 = llLo
	// p1 = llHi + lhLo + hlLo
	var c1, c2 uint64
	p1, c1 = bits.Add64(llHi, lhLo, 0)
	p1, c2 = bits.Add64(p1, hlLo, 0)
	carry1 := c1 + c2
	// p2 = lhHi + hlHi + hhLo + carry1
	var c3, c4, c5 uint64
	p2, c3 = bits.Add64(lhHi, hlHi, 0)
	p2, c4 = bits.Add64(p2, hhLo, 0)
	p2, c5 = bits.Add64(p2, carry1, 0)
	carry2 := c3 + c4 + c5
	// p3 = hhHi + carry2 (cannot overflow: max product fits 256 bits)
	p3 = hhHi + carry2
	return
}

// ge128 reports whether the 128-bit unsigned (ahi:alo) >= (bhi:blo).
func ge128(ahi, alo, bhi, blo uint64) bool {
	if ahi != bhi {
		return ahi > bhi
	}
	return alo >= blo
}

// div256by128 divides a 256-bit value (limbs n0..n3, n0 least significant) by a
// 128-bit divisor (dhi:dlo, must be non-zero), via binary long division. It
// returns the low 128 bits of the quotient (truncated toward zero) and ok=false
// if the quotient exceeds 128 bits. The running remainder R is kept < divisor,
// so it fits in 128 bits; the bit shifted out of R on each step is tracked
// separately (carry) — when set, the true remainder is ≥ 2^128 > divisor, and
// the 128-bit wrapping subtract of the divisor is exact because the borrow it
// produces cancels that 2^128.
func div256by128(n0, n1, n2, n3, dhi, dlo uint64) (qhi, qlo uint64, ok bool) {
	n := [4]uint64{n0, n1, n2, n3}
	var q [4]uint64
	var rhi, rlo uint64
	for i := 255; i >= 0; i-- {
		carry := rhi >> 63 // bit shifting out of the 128-bit remainder
		rhi = (rhi << 1) | (rlo >> 63)
		rlo <<= 1
		rlo |= (n[i>>6] >> uint(i&63)) & 1 // bring down bit i of the numerator
		if carry == 1 || ge128(rhi, rlo, dhi, dlo) {
			lo, borrow := bits.Sub64(rlo, dlo, 0)
			rhi, _ = bits.Sub64(rhi, dhi, borrow)
			rlo = lo
			q[i>>6] |= uint64(1) << uint(i&63)
		}
	}
	if q[2] != 0 || q[3] != 0 { // quotient exceeds 128 bits
		return 0, 0, false
	}
	return q[1], q[0], true
}

// div256by64 divides a 256-bit value (limbs n0..n3, n0 least significant) by a
// uint64 divisor, returning the 256-bit quotient (q0 least significant),
// truncated toward zero. d must be > 0. Each step's remainder stays < d, so the
// bits.Div64 precondition (hi < d) always holds.
func div256by64(n0, n1, n2, n3, d uint64) (q0, q1, q2, q3 uint64) {
	var rem uint64
	q3, rem = bits.Div64(0, n3, d)
	q2, rem = bits.Div64(rem, n2, d)
	q1, rem = bits.Div64(rem, n1, d)
	q0, _ = bits.Div64(rem, n0, d)
	return
}

// fromMag builds a Decimal from an unsigned 128-bit magnitude and sign,
// panicking if it exceeds the signed 128-bit range.
func fromMag(mhi, mlo uint64, neg bool) Decimal {
	if neg {
		// Valid iff magnitude ≤ 2^127 (so the value ≥ -2^127).
		if mhi > signBit || (mhi == signBit && mlo > 0) {
			panic("decimal: value overflows 128-bit range")
		}
		lo, borrow := bits.Sub64(0, mlo, 0)
		hi, _ := bits.Sub64(0, mhi, borrow)
		return Decimal{hi: int64(hi), lo: lo}
	}
	// Positive: valid iff the top bit is clear (magnitude < 2^127).
	if mhi >= signBit {
		panic("decimal: value overflows 128-bit range")
	}
	return Decimal{hi: int64(mhi), lo: mlo}
}

// --- 128-bit <-> big.Int bridging ---

// toBig reconstructs the signed 128-bit value: hi*2^64 + lo.
func (d Decimal) toBig() *big.Int {
	b := new(big.Int).SetInt64(d.hi)
	b.Lsh(b, 64)
	return b.Add(b, new(big.Int).SetUint64(d.lo))
}

// fromBig converts a big.Int to a Decimal, panicking if it exceeds the signed
// 128-bit range.
func fromBig(b *big.Int) Decimal {
	d, ok := fromBigChecked(b)
	if !ok {
		panic("decimal: value overflows 128-bit range")
	}
	return d
}

// fromBigChecked converts a big.Int, reporting ok=false on 128-bit overflow.
func fromBigChecked(b *big.Int) (Decimal, bool) {
	if b.Cmp(maxPos) > 0 || b.Cmp(minNeg) < 0 {
		return Decimal{}, false
	}
	t := b
	if b.Sign() < 0 {
		t = new(big.Int).Add(b, two128) // two's-complement representation
	}
	// Split into 64-bit words.
	loMask := new(big.Int).SetUint64(^uint64(0))
	loB := new(big.Int).And(t, loMask)
	hiB := new(big.Int).Rsh(t, 64)
	return Decimal{hi: int64(hiB.Uint64()), lo: loB.Uint64()}, true
}
