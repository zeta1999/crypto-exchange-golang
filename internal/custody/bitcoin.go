package custody

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"golang.org/x/crypto/ripemd160" //nolint:staticcheck // BIP-143 HASH160 needs RIPEMD-160
)

// Bitcoin testnet endpoints. Esplora serves address stats; the faucet is
// web/captcha-only, so funding is manual.
const (
	btcEsplora   = "https://blockstream.info/testnet/api"
	btcFaucetURL = "https://coinfaucet.eu/en/btc-testnet/"
	btcHRP       = "tb" // human-readable prefix for testnet/signet bech32 addresses
)

// Bitcoin is the BTC testnet chain: secp256k1 keys, native-segwit (P2WPKH)
// bech32 addresses, and Esplora balance lookups. Funding is a manual faucet.
type Bitcoin struct {
	esplora string
	faucet  string
	hc      *http.Client
}

// NewBitcoin returns a Bitcoin chain pinned to testnet.
func NewBitcoin() *Bitcoin {
	return &Bitcoin{esplora: btcEsplora, faucet: btcFaucetURL, hc: defaultHTTPClient()}
}

func (b *Bitcoin) ID() string      { return "btc" }
func (b *Bitcoin) Network() string { return "testnet" }

// NewKey generates a fresh secp256k1 private key (32 bytes).
func (b *Bitcoin) NewKey() ([]byte, error) {
	priv, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		return nil, err
	}
	return priv.Serialize(), nil
}

// Address derives a native-segwit v0 (P2WPKH) bech32 testnet address ("tb1…")
// from a 32-byte secp256k1 key: bech32(tb, 0, HASH160(compressedPubKey)).
func (b *Bitcoin) Address(secret []byte) (string, error) {
	if len(secret) != 32 {
		return "", fmt.Errorf("btc: bad key length %d (want 32)", len(secret))
	}
	priv := secp256k1.PrivKeyFromBytes(secret)
	pub := priv.PubKey().SerializeCompressed() // 33 bytes
	return segwitAddress(btcHRP, 0, hash160(pub))
}

// hash160 = RIPEMD160(SHA256(b)), the standard Bitcoin public-key hash.
func hash160(b []byte) []byte {
	s := sha256.Sum256(b)
	r := ripemd160.New()
	r.Write(s[:])
	return r.Sum(nil)
}

// esploraAddress is the subset of an Esplora /address/{addr} response we read.
type esploraAddress struct {
	ChainStats struct {
		FundedSum int64 `json:"funded_txo_sum"`
		SpentSum  int64 `json:"spent_txo_sum"`
	} `json:"chain_stats"`
}

// Balances returns the confirmed BTC balance (funded − spent) via Esplora.
func (b *Bitcoin) Balances(ctx context.Context, address string) ([]Balance, error) {
	body, err := httpGet(ctx, b.hc, b.esplora+"/address/"+address)
	if err != nil {
		return nil, fmt.Errorf("btc: esplora: %w", err)
	}
	var a esploraAddress
	if err := json.Unmarshal(body, &a); err != nil {
		return nil, fmt.Errorf("btc: decode address: %w", err)
	}
	sats := a.ChainStats.FundedSum - a.ChainStats.SpentSum
	if sats < 0 {
		sats = 0
	}
	return []Balance{{Asset: "BTC", Amount: formatUnits(uint64(sats), 8)}}, nil
}

// Tap: testnet BTC faucets are web/captcha, so funding is manual.
func (b *Bitcoin) Tap(ctx context.Context, address, asset string, amount float64) (string, error) {
	return "", ErrManualFaucet
}

// ManualURL returns the testnet faucet to visit.
func (b *Bitcoin) ManualURL(asset string) (string, bool) {
	return b.faucet, true
}

// --- bech32 / segwit address encoding (BIP-173) ---

const bech32Charset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"

func bech32Polymod(values []int) int {
	gen := []int{0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3}
	chk := 1
	for _, v := range values {
		top := chk >> 25
		chk = (chk&0x1ffffff)<<5 ^ v
		for i := 0; i < 5; i++ {
			if (top>>uint(i))&1 == 1 {
				chk ^= gen[i]
			}
		}
	}
	return chk
}

func bech32HrpExpand(hrp string) []int {
	out := make([]int, 0, len(hrp)*2+1)
	for i := 0; i < len(hrp); i++ {
		out = append(out, int(hrp[i]>>5))
	}
	out = append(out, 0)
	for i := 0; i < len(hrp); i++ {
		out = append(out, int(hrp[i]&31))
	}
	return out
}

func bech32Checksum(hrp string, data []int) []int {
	values := append(bech32HrpExpand(hrp), data...)
	values = append(values, 0, 0, 0, 0, 0, 0)
	polymod := bech32Polymod(values) ^ 1 // bech32 (v0); bech32m would use 0x2bc830a3
	out := make([]int, 6)
	for i := 0; i < 6; i++ {
		out[i] = (polymod >> uint(5*(5-i))) & 31
	}
	return out
}

func bech32Encode(hrp string, data []int) string {
	combined := append(data, bech32Checksum(hrp, data)...)
	var sb strings.Builder
	sb.WriteString(hrp)
	sb.WriteByte('1')
	for _, d := range combined {
		sb.WriteByte(bech32Charset[d])
	}
	return sb.String()
}

// convertBits regroups a byte slice from 8-bit to 5-bit groups (pad=true).
func convertBits(data []byte, fromBits, toBits uint, pad bool) ([]int, error) {
	acc, bits := 0, uint(0)
	maxv := (1 << toBits) - 1
	var out []int
	for _, b := range data {
		acc = (acc << fromBits) | int(b)
		bits += fromBits
		for bits >= toBits {
			bits -= toBits
			out = append(out, (acc>>bits)&maxv)
		}
	}
	if pad {
		if bits > 0 {
			out = append(out, (acc<<(toBits-bits))&maxv)
		}
	} else if bits >= fromBits || ((acc<<(toBits-bits))&maxv) != 0 {
		return nil, fmt.Errorf("bech32: invalid padding")
	}
	return out, nil
}

// segwitAddress encodes a witness program as a bech32 segwit address.
func segwitAddress(hrp string, version int, program []byte) (string, error) {
	conv, err := convertBits(program, 8, 5, true)
	if err != nil {
		return "", err
	}
	data := append([]int{version}, conv...)
	return bech32Encode(hrp, data), nil
}
