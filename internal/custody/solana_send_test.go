package custody

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestCompactU16(t *testing.T) {
	if got := compactU16(3); !bytes.Equal(got, []byte{3}) {
		t.Errorf("compactU16(3) = %v", got)
	}
	if got := compactU16(127); !bytes.Equal(got, []byte{127}) {
		t.Errorf("compactU16(127) = %v", got)
	}
	if got := compactU16(128); !bytes.Equal(got, []byte{0x80, 0x01}) {
		t.Errorf("compactU16(128) = %v, want [0x80 0x01]", got)
	}
}

// TestSolTransferMessage checks the legacy message serialization layout for a
// 1-SOL System transfer with known from/to/blockhash.
func TestSolTransferMessage(t *testing.T) {
	from := bytes.Repeat([]byte{0x01}, 32)
	to := bytes.Repeat([]byte{0x02}, 32)
	bh := bytes.Repeat([]byte{0x03}, 32)
	m := solTransferMessage(from, to, bh, 1_000_000_000) // 1 SOL = 1e9 lamports

	// 3 (header) +1 +96 (3 keys) +32 (blockhash) +1 +1 +1 +2 +1 +12 (data) = 150.
	if len(m) != 150 {
		t.Fatalf("message length = %d, want 150", len(m))
	}
	if !bytes.Equal(m[:3], []byte{1, 0, 1}) {
		t.Errorf("header = %v, want [1 0 1]", m[:3])
	}
	// SystemProgram (3rd account key) must be all zeros.
	sp := m[4+64 : 4+96]
	if !bytes.Equal(sp, make([]byte, 32)) {
		t.Errorf("system program key not all-zero")
	}
	// Instruction data (last 12 bytes): u32 LE 2 (Transfer) + u64 LE 1e9.
	wantData := []byte{2, 0, 0, 0, 0x00, 0xca, 0x9a, 0x3b, 0, 0, 0, 0}
	if !bytes.Equal(m[len(m)-12:], wantData) {
		t.Errorf("instruction data = %v, want %v", m[len(m)-12:], wantData)
	}
}

// TestSPLTransferMessage checks the TransferChecked message layout: 5 accounts,
// header [1 0 2], program-id index 4, account indices [source mint dest owner],
// and the 10-byte data [12 || u64 amount || u8 decimals].
func TestSPLTransferMessage(t *testing.T) {
	owner := bytes.Repeat([]byte{0x01}, 32)
	source := bytes.Repeat([]byte{0x02}, 32)
	dest := bytes.Repeat([]byte{0x03}, 32)
	mint := bytes.Repeat([]byte{0x04}, 32)
	prog := bytes.Repeat([]byte{0x05}, 32)
	bh := bytes.Repeat([]byte{0x06}, 32)
	const amount = uint64(1_500_000) // 1.5 USDC at 6 decimals
	m := splTransferMessage(owner, source, dest, mint, prog, bh, amount, 6)

	// 3 (header) +1 +160 (5 keys) +32 (blockhash) +1 (instr count) +1 (prog idx)
	// +1 (#accts) +4 (indices) +1 (data len) +10 (data) = 214.
	if len(m) != 214 {
		t.Fatalf("message length = %d, want 214", len(m))
	}
	if !bytes.Equal(m[:3], []byte{1, 0, 2}) {
		t.Errorf("header = %v, want [1 0 2]", m[:3])
	}
	if m[3] != 5 {
		t.Errorf("account count = %d, want 5", m[3])
	}
	// Account keys are ordered owner, source, dest, mint, program.
	keys := m[4 : 4+160]
	for i, want := range [][]byte{owner, source, dest, mint, prog} {
		if !bytes.Equal(keys[i*32:(i+1)*32], want) {
			t.Errorf("account key %d mismatch", i)
		}
	}
	// After blockhash: instr count (1), program-id index (4), #accounts (4),
	// indices [1 3 2 0] (source, mint, dest, owner).
	off := 4 + 160 + 32
	if m[off] != 1 {
		t.Errorf("instruction count = %d, want 1", m[off])
	}
	if m[off+1] != 4 {
		t.Errorf("program-id index = %d, want 4 (token program)", m[off+1])
	}
	if m[off+2] != 4 {
		t.Errorf("account-index count = %d, want 4", m[off+2])
	}
	if got := m[off+3 : off+7]; !bytes.Equal(got, []byte{1, 3, 2, 0}) {
		t.Errorf("account indices = %v, want [1 3 2 0]", got)
	}
	// Data: len byte 10, then [12 || u64 LE amount || u8 6].
	data := m[off+7:]
	if data[0] != 10 {
		t.Fatalf("data len prefix = %d, want 10", data[0])
	}
	data = data[1:]
	if data[0] != 12 {
		t.Errorf("instruction tag = %d, want 12 (TransferChecked)", data[0])
	}
	if got := binary.LittleEndian.Uint64(data[1:9]); got != amount {
		t.Errorf("amount = %d, want %d", got, amount)
	}
	if data[9] != 6 {
		t.Errorf("decimals = %d, want 6", data[9])
	}
}
