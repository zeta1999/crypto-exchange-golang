// Package fix is a minimal FIX 4.4 ACCEPTOR for the exchange emulator (CR-8): a
// localhost stand-in so an OMS (Vivaldi) can connect as a FIX initiator and drive
// order entry + a FIX market-data / liquidity search against the same matching
// engine the REST edge uses — end-to-end, offline, no real venue.
//
// Scope (a documented SUBSET, like the REST edges): session layer (Logon/Logout/
// Heartbeat/TestRequest/ResendRequest/SequenceReset), order entry (NewOrderSingle
// D, OrderCancelRequest F, OrderCancelReplaceRequest G -> ExecutionReport 8,
// OrderCancelReject 9), and market data (MarketDataRequest V -> snapshot W +
// incremental X). Not a certified FIX engine; messages are validated against a
// small embedded data dictionary (see dictionary.go).
package fix

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// SOH is the FIX field separator (ASCII 0x01).
const SOH = byte(0x01)

// BeginString is the FIX version this acceptor speaks.
const BeginString = "FIX.4.4"

// Standard tags used across the engine.
const (
	TagBeginString  = 8
	TagBodyLength   = 9
	TagCheckSum     = 10
	TagMsgType      = 35
	TagSenderCompID = 49
	TagTargetCompID = 56
	TagMsgSeqNum    = 34
	TagSendingTime  = 52
)

// Field is one tag=value pair.
type Field struct {
	Tag   int
	Value string
}

// Message is an ordered list of fields (repeating groups are preserved by order).
// A Message built for sending must NOT contain tags 8/9/10 — Encode derives them.
type Message struct {
	Fields []Field
	byTag  map[int][]string
}

// NewMessage starts a message of the given MsgType (tag 35). The session fills in
// the rest of the standard header (49/56/34/52) before app fields.
func NewMessage(msgType string) *Message {
	m := &Message{byTag: map[int][]string{}}
	m.Set(TagMsgType, msgType)
	return m
}

// Set appends a field (FIX allows repeats; order is preserved).
func (m *Message) Set(tag int, value string) *Message {
	if m.byTag == nil {
		m.byTag = map[int][]string{}
	}
	m.Fields = append(m.Fields, Field{Tag: tag, Value: value})
	m.byTag[tag] = append(m.byTag[tag], value)
	return m
}

// SetInt is Set for an integer value.
func (m *Message) SetInt(tag int, v int) *Message { return m.Set(tag, strconv.Itoa(v)) }

// Get returns the first value for a tag.
func (m *Message) Get(tag int) (string, bool) {
	vs, ok := m.byTag[tag]
	if !ok || len(vs) == 0 {
		return "", false
	}
	return vs[0], true
}

// GetInt returns the first value for a tag parsed as an int.
func (m *Message) GetInt(tag int) (int, bool) {
	v, ok := m.Get(tag)
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, false
	}
	return n, true
}

// Has reports whether the tag is present.
func (m *Message) Has(tag int) bool { _, ok := m.byTag[tag]; return ok }

// MsgType returns the message type (tag 35).
func (m *Message) MsgType() string { v, _ := m.Get(TagMsgType); return v }

// body serializes every field as "tag=value<SOH>" in order.
func body(fields []Field) []byte {
	var b strings.Builder
	for _, f := range fields {
		b.WriteString(strconv.Itoa(f.Tag))
		b.WriteByte('=')
		b.WriteString(f.Value)
		b.WriteByte(SOH)
	}
	return []byte(b.String())
}

// Encode serializes the message with a computed BodyLength (9) and CheckSum (10).
// The field list must begin at tag 35 (no 8/9/10).
func (m *Message) Encode() []byte {
	bodyBytes := body(m.Fields)
	header := fmt.Sprintf("8=%s%c9=%d%c", BeginString, SOH, len(bodyBytes), SOH)
	full := append([]byte(header), bodyBytes...)
	cs := checksum(full)
	full = append(full, []byte(fmt.Sprintf("10=%03d%c", cs, SOH))...)
	return full
}

// ReadFrame reads exactly one complete FIX message off r, using the declared
// BodyLength (9) to delimit it (FIX is not newline-framed). Returns the raw
// bytes (incl. BeginString and CheckSum).
func ReadFrame(r *bufio.Reader) ([]byte, error) {
	begin, err := r.ReadBytes(SOH) // "8=FIX.4.4<SOH>"
	if err != nil {
		return nil, err
	}
	lenField, err := r.ReadBytes(SOH) // "9=<n><SOH>"
	if err != nil {
		return nil, err
	}
	ls := string(lenField)
	if len(ls) < 3 || ls[:2] != "9=" {
		return nil, errMalformed
	}
	n, err := strconv.Atoi(ls[2 : len(ls)-1])
	if err != nil || n < 0 || n > 1<<20 {
		return nil, errMalformed
	}
	bodyBuf := make([]byte, n)
	if _, err := io.ReadFull(r, bodyBuf); err != nil {
		return nil, err
	}
	csField, err := r.ReadBytes(SOH) // "10=<ccc><SOH>"
	if err != nil {
		return nil, err
	}
	full := append(append(append([]byte{}, begin...), lenField...), bodyBuf...)
	return append(full, csField...), nil
}

// ReadMessage frames and decodes one FIX message from r.
func ReadMessage(r *bufio.Reader) (*Message, error) {
	raw, err := ReadFrame(r)
	if err != nil {
		return nil, err
	}
	return Decode(raw)
}

// checksum is the sum of all bytes mod 256.
func checksum(b []byte) int {
	var sum int
	for _, c := range b {
		sum += int(c)
	}
	return sum % 256
}

var (
	errMalformed   = errors.New("fix: malformed message")
	errBodyLength  = errors.New("fix: BodyLength (9) mismatch")
	errCheckSum    = errors.New("fix: CheckSum (10) mismatch")
	errBeginString = errors.New("fix: missing/invalid BeginString (8)")
)

// Decode parses a complete raw FIX message and validates BeginString, BodyLength
// and CheckSum. The returned Message keeps all fields (incl. 8/9/10) in order.
func Decode(raw []byte) (*Message, error) {
	if len(raw) == 0 {
		return nil, errMalformed
	}
	parts := strings.Split(string(raw), string(SOH))
	// A well-formed message ends with "10=xxx" then SOH, so the split yields a
	// trailing empty element; drop it.
	if len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	if len(parts) < 3 {
		return nil, errMalformed
	}

	m := &Message{byTag: map[int][]string{}}
	for _, p := range parts {
		eq := strings.IndexByte(p, '=')
		if eq <= 0 {
			return nil, fmt.Errorf("%w: bad field %q", errMalformed, p)
		}
		tag, err := strconv.Atoi(p[:eq])
		if err != nil {
			return nil, fmt.Errorf("%w: non-numeric tag %q", errMalformed, p[:eq])
		}
		m.Set(tag, p[eq+1:])
	}

	// BeginString must be the first field and exactly FIX.4.4.
	if len(m.Fields) == 0 || m.Fields[0].Tag != TagBeginString || m.Fields[0].Value != BeginString {
		return nil, errBeginString
	}
	// BodyLength must be the second field and match the byte count from the field
	// after BodyLength's SOH up to and including the SOH before CheckSum.
	declaredLen, ok := m.GetInt(TagBodyLength)
	if !ok || len(m.Fields) < 2 || m.Fields[1].Tag != TagBodyLength {
		return nil, fmt.Errorf("%w: missing BodyLength", errMalformed)
	}
	// Recompute body bytes = everything after "8=..|9=..|" up to (not incl) "10=".
	bodyFields := m.Fields[2:]
	if n := len(bodyFields); n == 0 || bodyFields[n-1].Tag != TagCheckSum {
		return nil, fmt.Errorf("%w: missing CheckSum", errMalformed)
	}
	bodyFields = bodyFields[:len(bodyFields)-1] // drop CheckSum field
	if got := len(body(bodyFields)); got != declaredLen {
		return nil, fmt.Errorf("%w: declared %d got %d", errBodyLength, declaredLen, got)
	}
	// CheckSum over all bytes up to (not including) the "10=" field.
	declaredCS, _ := m.GetInt(TagCheckSum)
	header := fmt.Sprintf("8=%s%c9=%d%c", BeginString, SOH, declaredLen, SOH)
	full := append([]byte(header), body(bodyFields)...)
	if got := checksum(full); got != declaredCS {
		return nil, fmt.Errorf("%w: declared %03d got %03d", errCheckSum, declaredCS, got)
	}
	return m, nil
}
