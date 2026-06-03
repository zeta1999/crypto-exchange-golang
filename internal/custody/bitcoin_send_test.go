package custody

import (
	"bytes"
	"encoding/hex"
	"testing"
)

func hx(s string) []byte { b, _ := hex.DecodeString(s); return b }

// TestBIP143SighashVector reproduces the canonical BIP-143 native-P2WPKH example
// (https://github.com/bitcoin/bips/blob/master/bip-0143.mediawiki) sighash for
// the segwit input (#1). A definitive check of the segwit signing preimage.
func TestBIP143SighashVector(t *testing.T) {
	// The BIP-143 example gives outpoint txids already in serialized (LE) form,
	// so they are used as-is here. (Send() reverses Esplora's display txids.)
	ins := []btcIn{
		{txidLE: hx("fff7f7881a8099afa6940d42d1e7f6362bec38171ea3edf433541db4e4ad969f"), vout: 0, sequence: 0xffffffee},
		{txidLE: hx("ef51e1b804cc89d182d279655c3aa89e815b1b309fe287d9b2b55d57b90ec68a"), vout: 1, sequence: 0xffffffff},
	}
	outs := []btcOut{
		{value: 0x0000000006b22c20, script: hx("76a9148280b37df378db99f66f85c95a783a76ac7a6d5988ac")},
		{value: 0x000000000d519390, script: hx("76a9143bde42dbee7e4dbe6a21b2d50ce2f0167faa815988ac")},
	}
	scriptCode := hx("76a9141d0f172a0ecb48aee1be1f2687d2963ae33f71a188ac")
	got := bip143Sighash(1, ins, outs, 1, scriptCode, 0x0000000023c34600, 0x11)

	const want = "c37af31116d1b27caf68aae9e3ac82f1477929014d5b917657d0eb49478cb670"
	if hex.EncodeToString(got) != want {
		t.Fatalf("BIP-143 sighash = %s, want %s", hex.EncodeToString(got), want)
	}
}

// TestBech32DecodeRoundTrip checks segwit decode against encode (BIP-173 program).
func TestBech32DecodeRoundTrip(t *testing.T) {
	prog := hx("751e76e8199196d454941c45d1b3a323f1433bd6")
	addr, err := segwitAddress("tb", 0, prog)
	if err != nil {
		t.Fatal(err)
	}
	got, err := segwitProgram("tb", addr)
	if err != nil {
		t.Fatalf("decode %s: %v", addr, err)
	}
	if !bytes.Equal(got, prog) {
		t.Fatalf("decoded program %x, want %x", got, prog)
	}
}
