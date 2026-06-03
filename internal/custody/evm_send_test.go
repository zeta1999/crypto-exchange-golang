package custody

import (
	"bytes"
	"math/big"
	"testing"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
)

// TestSignLegacyTxEIP155Vector reproduces the canonical EIP-155 example
// (https://eips.ethereum.org/EIPS/eip-155): private key 0x4646…46, nonce 9,
// gasPrice 20 gwei, gasLimit 21000, to 0x3535…35, value 1 ETH, chainID 1 →
// the exact signed-tx RLP. A definitive check of RLP + EIP-155 signing.
func TestSignLegacyTxEIP155Vector(t *testing.T) {
	priv := secp256k1.PrivKeyFromBytes(bytes.Repeat([]byte{0x46}, 32))
	to := bytes.Repeat([]byte{0x35}, 20)
	value, _ := new(big.Int).SetString("1000000000000000000", 10) // 1 ETH
	got, err := signLegacyTx(priv,
		big.NewInt(1),           // chainID
		big.NewInt(9),           // nonce
		big.NewInt(20000000000), // gasPrice (20 gwei)
		big.NewInt(21000),       // gasLimit
		value, to, nil)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	const want = "0xf86c098504a817c800825208943535353535353535353535353535353535353535880de0b6b3a76400008025a028ef61340bd939bc2195fe537567866003e1a15d3c71ff63e1590620aa636276a067cbe9d8997f761aecb703304b3800ccf555c9f3dc64214b297fb1966a3b6d83"
	if got != want {
		t.Fatalf("signed tx mismatch:\n got %s\nwant %s", got, want)
	}
}

func TestBaseUnits(t *testing.T) {
	cases := []struct {
		amount string
		dec    int
		want   string
	}{
		{"1.5", 18, "1500000000000000000"},
		{"1", 18, "1000000000000000000"},
		{"0.000001", 6, "1"},
		{"1.5", 6, "1500000"},
		{"250.123456789", 6, "250123456"}, // extra fractional digits truncated
		{"0", 18, "0"},
	}
	for _, c := range cases {
		got, err := baseUnits(c.amount, c.dec)
		if err != nil {
			t.Errorf("baseUnits(%q,%d): %v", c.amount, c.dec, err)
			continue
		}
		if got.String() != c.want {
			t.Errorf("baseUnits(%q,%d) = %s, want %s", c.amount, c.dec, got, c.want)
		}
	}
}
