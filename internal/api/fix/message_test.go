package fix

import (
	"bytes"
	"strings"
	"testing"
)

// soh replaces '|' with the real separator for readable test literals.
func soh(s string) string { return strings.ReplaceAll(s, "|", string(SOH)) }

func TestEncode_BodyLengthAndChecksum(t *testing.T) {
	// A known Logon: the body length and checksum are computed by the encoder.
	m := NewMessage("A")
	m.Set(TagSenderCompID, "MIRAGE")
	m.Set(TagTargetCompID, "VIVALDI")
	m.SetInt(TagMsgSeqNum, 1)
	m.Set(TagSendingTime, "20260604-08:00:00.000")
	m.SetInt(98, 0)   // EncryptMethod
	m.SetInt(108, 30) // HeartBtInt
	enc := m.Encode()

	if !bytes.HasPrefix(enc, []byte("8=FIX.4.4"+string(SOH))) {
		t.Fatalf("missing BeginString prefix: %q", enc)
	}
	if !bytes.Contains(enc, []byte(soh("|10="))) || !bytes.HasSuffix(enc, []byte{SOH}) {
		t.Fatalf("missing/!terminated CheckSum: %q", enc)
	}
	// Round-trips through Decode (which independently re-validates 9 and 10).
	dec, err := Decode(enc)
	if err != nil {
		t.Fatalf("decode own encode: %v", err)
	}
	if dec.MsgType() != "A" {
		t.Errorf("msgType=%q", dec.MsgType())
	}
	if v, _ := dec.GetInt(108); v != 30 {
		t.Errorf("HeartBtInt=%d", v)
	}
}

func TestDecode_RejectsTamperedChecksum(t *testing.T) {
	enc := buildValid(t)
	// Flip a body byte; the stored checksum no longer matches.
	tampered := bytes.Replace(enc, []byte("VIVALDI"), []byte("VIVALDX"), 1)
	if _, err := Decode(tampered); err == nil {
		t.Fatal("expected checksum/bodylength error on tampered message")
	}
}

func TestDecode_RejectsWrongBodyLength(t *testing.T) {
	// Hand-craft a message with a deliberately wrong BodyLength.
	raw := soh("8=FIX.4.4|9=5|35=0|10=000|")
	if _, err := Decode([]byte(raw)); err == nil {
		t.Fatal("expected BodyLength mismatch")
	}
}

func TestDecode_RejectsMalformed(t *testing.T) {
	for _, raw := range []string{
		"",
		"garbage",
		soh("35=D|49=X|"),          // no BeginString first
		soh("8=FIX.4.2|9=2|35=0|"), // wrong version
	} {
		if _, err := Decode([]byte(raw)); err == nil {
			t.Errorf("expected error for %q", raw)
		}
	}
}

func TestDecode_PreservesRepeatingGroupOrder(t *testing.T) {
	// Market-data snapshot style repeats tag 269/270; order must survive.
	m := NewMessage("W")
	m.Set(55, "BTCUSDT")
	m.SetInt(268, 2)
	m.Set(269, "0").Set(270, "100").Set(271, "5") // bid
	m.Set(269, "1").Set(270, "101").Set(271, "7") // ask
	dec, err := Decode(m.Encode())
	if err != nil {
		t.Fatal(err)
	}
	var entryTypes []string
	for _, f := range dec.Fields {
		if f.Tag == 269 {
			entryTypes = append(entryTypes, f.Value)
		}
	}
	if len(entryTypes) != 2 || entryTypes[0] != "0" || entryTypes[1] != "1" {
		t.Errorf("group order lost: %v", entryTypes)
	}
}

func buildValid(t *testing.T) []byte {
	t.Helper()
	m := NewMessage("A")
	m.Set(TagSenderCompID, "MIRAGE")
	m.Set(TagTargetCompID, "VIVALDI")
	m.SetInt(TagMsgSeqNum, 1)
	m.Set(TagSendingTime, "20260604-08:00:00.000")
	m.SetInt(98, 0)
	m.SetInt(108, 30)
	return m.Encode()
}
