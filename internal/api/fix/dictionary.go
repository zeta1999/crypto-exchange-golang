package fix

import "fmt"

// Application-layer tags used by this acceptor (FIX 4.4 standard tag numbers).
const (
	TagClOrdID      = 11
	TagOrigClOrdID  = 41
	TagOrderID      = 37
	TagExecID       = 17
	TagExecType     = 150
	TagOrdStatus    = 39
	TagSymbol       = 55
	TagSide         = 54
	TagOrderQty     = 38
	TagOrdType      = 40
	TagPrice        = 44
	TagTimeInForce  = 59
	TagLastQty      = 32
	TagLastPx       = 31
	TagLeavesQty    = 151
	TagCumQty       = 14
	TagAvgPx        = 6
	TagText         = 58
	TagCxlRejReason = 102
	TagCxlRejRespTo = 434
	TagTransactTime = 60

	TagEncryptMethod = 98
	TagHeartBtInt    = 108
	TagTestReqID     = 112
	TagBeginSeqNo    = 7
	TagEndSeqNo      = 16
	TagNewSeqNo      = 36
	TagGapFillFlag   = 123
	TagRefSeqNum     = 45
	TagRefTagID      = 371
	TagRefMsgType    = 372
	TagSessionRej    = 373
	TagBusinessRej   = 380

	// Market data
	TagMDReqID        = 262
	TagSubReqType     = 263
	TagMarketDepth    = 264
	TagMDUpdateType   = 265
	TagNoMDEntryTypes = 267
	TagNoRelatedSym   = 146
	TagNoMDEntries    = 268
	TagMDEntryType    = 269
	TagMDEntryPx      = 270
	TagMDEntrySize    = 271
	TagMDUpdateAction = 279
	TagMDReqRejReason = 281
)

// Message types (tag 35 values).
const (
	MsgLogon             = "A"
	MsgHeartbeat         = "0"
	MsgTestRequest       = "1"
	MsgResendRequest     = "2"
	MsgReject            = "3"
	MsgSequenceReset     = "4"
	MsgLogout            = "5"
	MsgExecutionReport   = "8"
	MsgOrderCancelReject = "9"
	MsgNewOrderSingle    = "D"
	MsgOrderCancelReq    = "F"
	MsgOrderCancelRepl   = "G"
	MsgMarketDataReq     = "V"
	MsgMarketDataSnap    = "W"
	MsgMarketDataInc     = "X"
	MsgMarketDataReject  = "Y"
	MsgBusinessReject    = "j"
)

// Dictionary is a small FIX 4.4 validation dictionary: which message types are
// known, and which body tags each inbound type requires. It is the offline
// conformance layer — a message missing a required tag is rejected with the
// offending tag, exactly as a real engine would (Reject/3 or BusinessReject/j).
type Dictionary struct {
	// required maps an inbound MsgType to its required body tags.
	required map[string][]int
}

// headerRequired are the standard header tags every inbound message must carry
// (BeginString/BodyLength/CheckSum are validated structurally by Decode).
var headerRequired = []int{TagMsgType, TagSenderCompID, TagTargetCompID, TagMsgSeqNum, TagSendingTime}

// NewDictionary returns the FIX 4.4 subset this acceptor accepts.
func NewDictionary() *Dictionary {
	return &Dictionary{required: map[string][]int{
		MsgLogon:           {TagEncryptMethod, TagHeartBtInt},
		MsgHeartbeat:       {},
		MsgTestRequest:     {TagTestReqID},
		MsgResendRequest:   {TagBeginSeqNo, TagEndSeqNo},
		MsgSequenceReset:   {TagNewSeqNo},
		MsgLogout:          {},
		MsgNewOrderSingle:  {TagClOrdID, TagSymbol, TagSide, TagOrderQty, TagOrdType},
		MsgOrderCancelReq:  {TagClOrdID, TagOrigClOrdID, TagSymbol, TagSide},
		MsgOrderCancelRepl: {TagClOrdID, TagOrigClOrdID, TagSymbol, TagSide, TagOrderQty, TagOrdType},
		MsgMarketDataReq:   {TagMDReqID, TagSubReqType, TagMarketDepth, TagNoMDEntryTypes, TagNoRelatedSym},
	}}
}

// Known reports whether the MsgType is one this acceptor handles.
func (d *Dictionary) Known(msgType string) bool {
	_, ok := d.required[msgType]
	return ok
}

// ValidateError carries the offending tag so the caller can build a Reject (3)
// referencing RefTagID. Tag 0 means a message-level (not tag-level) problem.
type ValidateError struct {
	Tag int
	Msg string
}

func (e *ValidateError) Error() string { return e.Msg }

// Validate checks header completeness, that the type is known, and that all
// type-required body tags are present. A conditionally-required tag (Price on a
// LIMIT order) is enforced too.
func (d *Dictionary) Validate(m *Message) *ValidateError {
	for _, tag := range headerRequired {
		if !m.Has(tag) {
			return &ValidateError{Tag: tag, Msg: fmt.Sprintf("missing required header tag %d", tag)}
		}
	}
	mt := m.MsgType()
	req, ok := d.required[mt]
	if !ok {
		return &ValidateError{Tag: TagMsgType, Msg: fmt.Sprintf("unsupported MsgType %q", mt)}
	}
	for _, tag := range req {
		if !m.Has(tag) {
			return &ValidateError{Tag: tag, Msg: fmt.Sprintf("%s missing required tag %d", mt, tag)}
		}
	}
	// Conditional: a LIMIT order (40=2) must carry a Price (44).
	if mt == MsgNewOrderSingle || mt == MsgOrderCancelRepl {
		if ot, _ := m.Get(TagOrdType); ot == "2" && !m.Has(TagPrice) {
			return &ValidateError{Tag: TagPrice, Msg: "LIMIT order missing Price (44)"}
		}
	}
	return nil
}
