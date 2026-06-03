package custody

import (
	"encoding/hex"
	"strings"
	"testing"
)

// TestSegwitAddressBIP173Vector validates the whole bech32 + segwit + convertBits
// stack against the canonical BIP-173 mainnet P2WPKH example: witness program
// 751e76e8199196d454941c45d1b3a323f1433bd6 (v0) →
// bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4.
func TestSegwitAddressBIP173Vector(t *testing.T) {
	prog, _ := hex.DecodeString("751e76e8199196d454941c45d1b3a323f1433bd6")
	got, err := segwitAddress("bc", 0, prog)
	if err != nil {
		t.Fatalf("segwitAddress: %v", err)
	}
	const want = "bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4"
	if got != want {
		t.Fatalf("segwit address = %s, want %s", got, want)
	}
	// Same program on testnet HRP yields a tb1 address.
	tb, _ := segwitAddress("tb", 0, prog)
	if !strings.HasPrefix(tb, "tb1q") {
		t.Fatalf("testnet address = %s, want tb1q…", tb)
	}
}

func TestBitcoinKeygenAddress(t *testing.T) {
	b := NewBitcoin()
	k, err := b.NewKey()
	if err != nil || len(k) != 32 {
		t.Fatalf("NewKey len=%d err=%v", len(k), err)
	}
	a1, err := b.Address(k)
	if err != nil {
		t.Fatal(err)
	}
	a2, _ := b.Address(k)
	if a1 != a2 || !strings.HasPrefix(a1, "tb1q") {
		t.Fatalf("address not deterministic/valid: %q", a1)
	}
	if b.Network() != "testnet" {
		t.Errorf("network = %q, want testnet", b.Network())
	}
}

func TestHash160Length(t *testing.T) {
	if got := len(hash160([]byte("hello"))); got != 20 {
		t.Fatalf("hash160 len = %d, want 20", got)
	}
}
