package custody

import (
	"encoding/base32"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
)

// --- Stellar StrKey (base32 + CRC16-XModem) ---
//
// A StrKey is base32(versionByte || payload || crc16LE). The version byte's
// high 5 bits select the human-readable prefix: account ('G') and seed ('S').

const (
	strkeyVersionAccount = 6 << 3  // 48 → 'G' (ed25519 public key)
	strkeyVersionSeed    = 18 << 3 // 144 → 'S' (ed25519 seed)
)

// base32Std is RFC 4648 base32 (uppercase, no padding), as Stellar uses.
var base32Std = base32.StdEncoding.WithPadding(base32.NoPadding)

// crc16XModem computes the CRC16-XModem checksum (poly 0x1021, init 0x0000)
// that Stellar appends to a StrKey, little-endian.
func crc16XModem(data []byte) uint16 {
	var crc uint16
	for _, b := range data {
		crc ^= uint16(b) << 8
		for i := 0; i < 8; i++ {
			if crc&0x8000 != 0 {
				crc = (crc << 1) ^ 0x1021
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

// strkeyEncode encodes payload under version into a StrKey string.
func strkeyEncode(version byte, payload []byte) string {
	buf := make([]byte, 0, 1+len(payload)+2)
	buf = append(buf, version)
	buf = append(buf, payload...)
	cs := crc16XModem(buf)
	buf = append(buf, byte(cs), byte(cs>>8)) // little-endian
	return base32Std.EncodeToString(buf)
}

// strkeyDecode reverses strkeyEncode, validating the version byte and checksum.
func strkeyDecode(version byte, s string) ([]byte, error) {
	raw, err := base32Std.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("strkey: base32: %w", err)
	}
	if len(raw) < 3 {
		return nil, errors.New("strkey: too short")
	}
	if raw[0] != version {
		return nil, errors.New("strkey: version mismatch")
	}
	want := uint16(raw[len(raw)-2]) | uint16(raw[len(raw)-1])<<8
	if got := crc16XModem(raw[:len(raw)-2]); got != want {
		return nil, errors.New("strkey: checksum mismatch")
	}
	return raw[1 : len(raw)-2], nil
}

// --- Base58 (Bitcoin / Solana alphabet) ---

const base58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// base58Encode encodes input using the Bitcoin/Solana base58 alphabet,
// preserving leading-zero bytes as leading '1's.
func base58Encode(input []byte) string {
	zeros := 0
	for zeros < len(input) && input[zeros] == 0 {
		zeros++
	}
	num := new(big.Int).SetBytes(input)
	radix := big.NewInt(58)
	mod := new(big.Int)
	var out []byte
	for num.Sign() > 0 {
		num.DivMod(num, radix, mod)
		out = append(out, base58Alphabet[mod.Int64()])
	}
	for i := 0; i < zeros; i++ {
		out = append(out, base58Alphabet[0])
	}
	// reverse
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return string(out)
}

// base58Decode reverses base58Encode.
func base58Decode(s string) ([]byte, error) {
	num := big.NewInt(0)
	radix := big.NewInt(58)
	for i := 0; i < len(s); i++ {
		idx := strings.IndexByte(base58Alphabet, s[i])
		if idx < 0 {
			return nil, fmt.Errorf("base58: invalid character %q", s[i])
		}
		num.Mul(num, radix)
		num.Add(num, big.NewInt(int64(idx)))
	}
	decoded := num.Bytes()
	zeros := 0
	for zeros < len(s) && s[zeros] == base58Alphabet[0] {
		zeros++
	}
	out := make([]byte, zeros+len(decoded))
	copy(out[zeros:], decoded)
	return out, nil
}

// --- amount formatting ---

// formatUnits renders an integer amount of base units (e.g. lamports) as a
// human-readable decimal string with dec fractional digits, trailing zeros
// trimmed (e.g. formatUnits(1500000000, 9) == "1.5").
func formatUnits(v uint64, dec int) string {
	s := strconv.FormatUint(v, 10)
	if dec == 0 {
		return s
	}
	for len(s) <= dec {
		s = "0" + s
	}
	intPart, fracPart := s[:len(s)-dec], s[len(s)-dec:]
	fracPart = strings.TrimRight(fracPart, "0")
	if fracPart == "" {
		return intPart
	}
	return intPart + "." + fracPart
}
