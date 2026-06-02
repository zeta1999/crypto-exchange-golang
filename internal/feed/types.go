// Package feed provides a normalized, venue-neutral market-data ingestion
// layer for the exchange emulator.
//
// Provenance: the Binance and Coinbase WebSocket shapes are adapted from
// github.com/notbbg/notbbg (../this-is-not-bbg, internal/feeds/ccxt). That
// code is internal/ and not importable, so the parsing logic is vendored
// here and reworked around a plain channel-based Source (the upstream
// pub/sub bus is intentionally dropped — see STATUS.md decisions log).
package feed

import (
	"encoding/json"
	"fmt"
	"time"
)

// EventKind discriminates the payload carried by an Event.
type EventKind uint8

const (
	// EventTrade carries a single executed trade in Event.Trade.
	EventTrade EventKind = iota + 1
	// EventBook carries an order-book snapshot or incremental update in
	// Event.Book. Whether it is a full snapshot or a diff is recorded on
	// LOBSnapshot.Snapshot.
	EventBook
	// EventTicker carries a best bid/ask/last in Event.Ticker.
	EventTicker
)

func (k EventKind) String() string {
	switch k {
	case EventTrade:
		return "trade"
	case EventBook:
		return "book"
	case EventTicker:
		return "ticker"
	default:
		return "unknown"
	}
}

// MarshalJSON renders the kind as a human-readable string so recorded
// fixtures are legible and diffable.
func (k EventKind) MarshalJSON() ([]byte, error) {
	return json.Marshal(k.String())
}

// UnmarshalJSON accepts the string form produced by MarshalJSON.
func (k *EventKind) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	switch s {
	case "trade":
		*k = EventTrade
	case "book":
		*k = EventBook
	case "ticker":
		*k = EventTicker
	default:
		return fmt.Errorf("feed: unknown event kind %q", s)
	}
	return nil
}

// Event is the single value type that flows over a Source channel. Exactly
// one of Trade/Book/Ticker is non-nil, selected by Kind.
type Event struct {
	Kind   EventKind    `json:"kind"`
	Trade  *Trade       `json:"trade,omitempty"`
	Book   *LOBSnapshot `json:"book,omitempty"`
	Ticker *Ticker      `json:"ticker,omitempty"`
}

// Trade is a single executed trade as reported by a venue's tape.
//
// Side is the aggressor side ("buy" = a buyer lifted the offer, "sell" = a
// seller hit the bid). The *Decimal fields carry the exchange-committed
// string representation for fidelity; "" means the venue did not expose one.
type Trade struct {
	Instrument      string    `json:"instrument"`
	Exchange        string    `json:"exchange"`
	Timestamp       time.Time `json:"timestamp"`
	Price           float64   `json:"price"`
	Quantity        float64   `json:"quantity"`
	Side            string    `json:"side"`
	TradeID         string    `json:"trade_id,omitempty"`
	PriceDecimal    string    `json:"price_decimal,omitempty"`
	QuantityDecimal string    `json:"quantity_decimal,omitempty"`
}

// LOBLevel is one price level in an order book. It must stay a pure value
// type (no slices, maps, or pointers): the reference book hands out levels
// by shallow copy and relies on that being a deep copy for its immutability
// guarantee. Adding a reference field would silently alias internal state.
type LOBLevel struct {
	Price           float64 `json:"price"`
	Quantity        float64 `json:"quantity"`
	OrderCount      uint32  `json:"order_count,omitempty"`
	PriceDecimal    string  `json:"price_decimal,omitempty"`
	QuantityDecimal string  `json:"quantity_decimal,omitempty"`
}

// LOBSnapshot is an order-book frame. When Snapshot is true it is a full
// replacement of the visible book; when false it is an incremental diff
// where a level with Quantity == 0 means "remove this price". Binance
// @depth20 always emits full snapshots; Coinbase level2 emits one initial
// snapshot followed by diffs.
type LOBSnapshot struct {
	Instrument     string     `json:"instrument"`
	Exchange       string     `json:"exchange"`
	Timestamp      time.Time  `json:"timestamp"`
	SequenceNumber uint64     `json:"sequence_number,omitempty"`
	Snapshot       bool       `json:"snapshot"`
	Bids           []LOBLevel `json:"bids"`
	Asks           []LOBLevel `json:"asks"`
}

// Ticker is a best bid/ask/last summary. Phase 1 does not yet emit these
// from the live adapters (no ticker channel is subscribed); the type and
// EventTicker kind exist so downstream layers can synthesize tickers from
// book tops without a wire-format change later.
type Ticker struct {
	Instrument string    `json:"instrument"`
	Exchange   string    `json:"exchange"`
	Timestamp  time.Time `json:"timestamp"`
	Bid        float64   `json:"bid"`
	Ask        float64   `json:"ask"`
	Last       float64   `json:"last"`
}
