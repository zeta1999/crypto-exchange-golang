package fix

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"time"
)

// fixTimeLayout is the FIX UTCTimestamp format (millis precision).
const fixTimeLayout = "20060102-15:04:05.000"

// Application receives the validated application-layer messages. The session
// owns all administrative traffic (logon/heartbeat/resend/logout); the app only
// sees business messages and lifecycle callbacks. Handlers run on the session's
// read goroutine and may call Session.Send.
type Application interface {
	OnLogon(s *Session)
	OnLogout(s *Session)
	OnNewOrderSingle(s *Session, m *Message)
	OnOrderCancelRequest(s *Session, m *Message)
	OnOrderCancelReplace(s *Session, m *Message)
	OnMarketDataRequest(s *Session, m *Message)
}

// Session is one FIX 4.4 connection (acceptor side). It frames messages off the
// stream, validates sequence numbers, handles the admin layer, and dispatches
// business messages to the Application. Send is safe for concurrent use.
type Session struct {
	rw           io.ReadWriter
	r            *bufio.Reader
	senderCompID string // this acceptor's CompID
	targetCompID string // the initiator's CompID (learned at Logon)
	app          Application
	dict         *Dictionary
	now          func() time.Time

	mu        sync.Mutex // guards outSeq + the ordered enqueue
	outSeq    int        // next outbound MsgSeqNum
	inSeq     int        // next expected inbound MsgSeqNum
	loggedOn  bool
	heartBtMs int

	// out decouples network writes from the caller: a Send from inside the
	// order-book "trade" hook (which holds the book lock) enqueues here instead of
	// doing blocking network I/O, and writeLoop drains it. This BOUNDS — it does
	// not eliminate — engine coupling: a client must back up more than outBuffer
	// messages before the hook's enqueue blocks and back-pressures matching. For
	// an offline emulator that's acceptable; a production edge would drop or
	// disconnect a wedged consumer instead.
	out  chan []byte
	done chan struct{}
}

// outBuffer is the per-session outbound queue depth — generous so a normal burst
// (a NEW + a fill + an MD update) never blocks, and a wedged client must overrun
// 1024 queued messages before it can back-pressure the engine.
const outBuffer = 1024

var errSessionClosed = errors.New("fix: session closed")

// NewSession builds an acceptor session over rw. senderCompID is this side's
// CompID; the peer's is learned at Logon.
func NewSession(rw io.ReadWriter, senderCompID string, app Application, dict *Dictionary, now func() time.Time) *Session {
	if now == nil {
		now = time.Now
	}
	return &Session{
		rw: rw, r: bufio.NewReader(rw),
		senderCompID: senderCompID, app: app, dict: dict, now: now,
		outSeq: 1, inSeq: 1,
		out:  make(chan []byte, outBuffer),
		done: make(chan struct{}),
	}
}

// TargetCompID is the initiator's CompID (valid after Logon).
func (s *Session) TargetCompID() string { return s.targetCompID }

// Send stamps the standard header (49/56/34/52), assigns the next outbound
// sequence, and enqueues the encoded message for the writer goroutine. The
// sequence assignment and enqueue happen under one lock so the wire order always
// matches the sequence order even under concurrent callers. Safe for concurrent
// use; never performs network I/O on the caller's goroutine.
func (s *Session) Send(m *Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.enqueueLocked(m)
}

func (s *Session) enqueueLocked(m *Message) error {
	// Prepend the session header right after MsgType (35 is field[0]).
	hdr := []Field{
		{Tag: TagSenderCompID, Value: s.senderCompID},
		{Tag: TagTargetCompID, Value: s.targetCompID},
		{Tag: TagMsgSeqNum, Value: strconv.Itoa(s.outSeq)},
		{Tag: TagSendingTime, Value: s.now().UTC().Format(fixTimeLayout)},
	}
	m.Fields = append(m.Fields[:1:1], append(hdr, m.Fields[1:]...)...)
	for _, f := range hdr {
		m.byTag[f.Tag] = append(m.byTag[f.Tag], f.Value)
	}
	s.outSeq++
	b := m.Encode()
	select {
	case s.out <- b:
		return nil
	case <-s.done:
		return errSessionClosed
	}
}

// writeLoop drains the outbound queue to the connection. It is the ONLY writer,
// so writes are serialized and ordered without holding the engine's locks.
func (s *Session) writeLoop() {
	for {
		select {
		case b := <-s.out:
			if _, err := s.rw.Write(b); err != nil {
				return
			}
		case <-s.done:
			return
		}
	}
}

// Run processes the session until the connection closes or a fatal protocol
// error occurs. The first message must be a Logon.
func (s *Session) Run() error {
	go s.writeLoop()
	defer close(s.done)
	for {
		raw, err := ReadFrame(s.r)
		if err != nil {
			if s.loggedOn {
				s.app.OnLogout(s)
			}
			if err == io.EOF {
				return nil
			}
			return err
		}
		m, err := Decode(raw)
		if err != nil {
			// Structural failure: we cannot trust seq/CompID — drop the session.
			return err
		}
		if done, err := s.handle(m); err != nil || done {
			return err
		}
	}
}

// handle processes one decoded message. Returns done=true to end the session.
func (s *Session) handle(m *Message) (done bool, err error) {
	mt := m.MsgType()

	// Logon must come first and establishes the peer CompID.
	if !s.loggedOn {
		if mt != MsgLogon {
			return true, fmt.Errorf("fix: first message must be Logon, got %q", mt)
		}
		return s.onLogon(m)
	}

	// Sequence handling for an already-established session.
	seq, _ := m.GetInt(TagMsgSeqNum)
	switch {
	case mt == MsgSequenceReset:
		// Advance our expected inbound seq (GapFill or reset). Never REWIND below
		// the current expected seq — a reset that lowers the sequence is ignored.
		if nsn, ok := m.GetInt(TagNewSeqNo); ok && nsn >= s.inSeq {
			s.inSeq = nsn
		}
		return false, nil
	case seq > s.inSeq:
		// Gap: ask the peer to resend from our expected seq.
		s.sendResendRequest(s.inSeq)
		return false, nil
	case seq < s.inSeq:
		// Already seen (or a stale dup) — ignore administratively.
		return false, nil
	}
	s.inSeq++ // in-order; consume it

	switch mt {
	case MsgHeartbeat:
		return false, nil
	case MsgTestRequest:
		s.sendHeartbeat(m) // echo TestReqID
		return false, nil
	case MsgResendRequest:
		// We don't replay app messages; gap-fill administratively to current seq.
		s.sendSequenceReset()
		return false, nil
	case MsgLogout:
		s.sendLogout("responding to logout")
		s.app.OnLogout(s)
		return true, nil
	case MsgNewOrderSingle, MsgOrderCancelReq, MsgOrderCancelRepl, MsgMarketDataReq:
		if ve := s.dict.Validate(m); ve != nil {
			s.sendReject(seq, ve)
			return false, nil
		}
		s.dispatch(m)
		return false, nil
	default:
		// Known-but-unhandled or unknown business type.
		s.sendReject(seq, &ValidateError{Tag: TagMsgType, Msg: "unsupported MsgType " + mt})
		return false, nil
	}
}

func (s *Session) onLogon(m *Message) (bool, error) {
	if ve := s.dict.Validate(m); ve != nil {
		return true, fmt.Errorf("fix: bad logon: %s", ve.Msg)
	}
	// The initiator's SenderCompID is our TargetCompID.
	s.targetCompID, _ = m.Get(TagSenderCompID)
	if hb, ok := m.GetInt(TagHeartBtInt); ok {
		s.heartBtMs = hb * 1000
	}
	seq, _ := m.GetInt(TagMsgSeqNum)
	s.inSeq = seq + 1
	s.loggedOn = true

	// Acknowledge with a Logon echoing the heartbeat interval.
	ack := NewMessage(MsgLogon)
	ack.SetInt(TagEncryptMethod, 0)
	ack.SetInt(TagHeartBtInt, s.heartBtMs/1000)
	if err := s.Send(ack); err != nil {
		return true, err
	}
	s.app.OnLogon(s)
	return false, nil
}

func (s *Session) sendHeartbeat(testReq *Message) {
	hb := NewMessage(MsgHeartbeat)
	if testReq != nil {
		if id, ok := testReq.Get(TagTestReqID); ok {
			hb.Set(TagTestReqID, id)
		}
	}
	_ = s.Send(hb)
}

func (s *Session) sendResendRequest(from int) {
	rr := NewMessage(MsgResendRequest)
	rr.SetInt(TagBeginSeqNo, from)
	rr.SetInt(TagEndSeqNo, 0) // 0 = infinity (through the latest)
	_ = s.Send(rr)
}

func (s *Session) sendSequenceReset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	sr := NewMessage(MsgSequenceReset)
	sr.Set(TagGapFillFlag, "Y")
	sr.SetInt(TagNewSeqNo, s.outSeq+1) // skip to the next outbound seq
	_ = s.enqueueLocked(sr)
}

// sendLogout sends a Logout with an optional text reason.
func (s *Session) sendLogout(reason string) {
	lo := NewMessage(MsgLogout)
	if reason != "" {
		lo.Set(TagText, reason)
	}
	_ = s.Send(lo)
}

// sendReject sends a session-level Reject (3) referencing the offending field.
func (s *Session) sendReject(refSeq int, ve *ValidateError) {
	rj := NewMessage(MsgReject)
	rj.SetInt(TagRefSeqNum, refSeq)
	if ve.Tag != 0 {
		rj.SetInt(TagRefTagID, ve.Tag)
	}
	rj.Set(TagText, ve.Msg)
	_ = s.Send(rj)
}

func (s *Session) dispatch(m *Message) {
	switch m.MsgType() {
	case MsgNewOrderSingle:
		s.app.OnNewOrderSingle(s, m)
	case MsgOrderCancelReq:
		s.app.OnOrderCancelRequest(s, m)
	case MsgOrderCancelRepl:
		s.app.OnOrderCancelReplace(s, m)
	case MsgMarketDataReq:
		s.app.OnMarketDataRequest(s, m)
	}
}
