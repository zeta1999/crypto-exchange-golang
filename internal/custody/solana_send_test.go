package custody

import (
	"bytes"
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
