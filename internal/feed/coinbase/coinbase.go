// Package coinbase implements a feed.Source for the Coinbase Advanced Trade
// WebSocket API.
//
// market_trades parsing is adapted from github.com/notbbg/notbbg
// internal/feeds/ccxt/coinbase.go. The level2 book channel was subscribed
// but never published upstream; its parse+emit (the l2_data snapshot/update
// protocol) is implemented here from scratch — see STATUS.md.
package coinbase

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/zeta1999/crypto-exchange-golang/internal/feed"
)

// DefaultWSURL is the Coinbase Advanced Trade market-data endpoint.
const DefaultWSURL = "wss://advanced-trade-ws.coinbase.com"

// Source streams Coinbase trades and the level2 book as normalized events.
type Source struct {
	*feed.StatusTracker
	symbols   []string
	feedTypes []string
	wsURL     string
}

// Config configures a Coinbase Source.
type Config struct {
	// Symbols are Coinbase product IDs, e.g. "BTC-USD".
	Symbols []string
	// FeedTypes selects channels: "trades" (market_trades) and/or
	// "orderbook" (level2). Empty defaults to both.
	FeedTypes []string
	// WSURL overrides the default endpoint (used by tests/staging).
	WSURL string
}

// New constructs a Coinbase Source from cfg.
func New(cfg Config) *Source {
	feedTypes := cfg.FeedTypes
	if len(feedTypes) == 0 {
		feedTypes = []string{"trades", "orderbook"}
	}
	wsURL := cfg.WSURL
	if wsURL == "" {
		wsURL = DefaultWSURL
	}
	return &Source{
		StatusTracker: feed.NewStatusTracker("coinbase"),
		symbols:       cfg.Symbols,
		feedTypes:     feedTypes,
		wsURL:         wsURL,
	}
}

func (s *Source) Name() string { return "coinbase" }

// channels maps the configured feed types to Advanced Trade channel names.
func (s *Source) channels() []string {
	var out []string
	for _, ft := range s.feedTypes {
		switch ft {
		case "trades":
			out = append(out, "market_trades")
		case "orderbook":
			out = append(out, "level2")
		}
	}
	return out
}

// readTimeout bounds a single read so a silent (half-open) socket trips the
// deadline and the reconnect loop recovers instead of blocking forever.
const readTimeout = 60 * time.Second

// Start opens the websocket and returns a channel of normalized events.
func (s *Source) Start(ctx context.Context) (<-chan feed.Event, error) {
	out := make(chan feed.Event, 1024)
	go func() {
		defer close(out)
		feed.RunReconnect(ctx, s.StatusTracker, func(ctx context.Context) error {
			return s.connectAndStream(ctx, out)
		})
	}()
	return out, nil
}

func (s *Source) connectAndStream(ctx context.Context, out chan<- feed.Event) error {
	channels := s.channels()
	if len(channels) == 0 {
		return fmt.Errorf("coinbase: no channels configured")
	}
	if len(s.symbols) == 0 {
		return fmt.Errorf("coinbase: no symbols configured")
	}

	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	conn, _, err := dialer.DialContext(ctx, s.wsURL, nil)
	if err != nil {
		return fmt.Errorf("dial coinbase: %w", err)
	}
	defer conn.Close()

	// A blocking ReadMessage doesn't observe ctx; close the conn on
	// cancellation so the read unwinds promptly. The done channel stops the
	// watcher when the stream returns for any other reason.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-done:
		}
	}()

	// Advanced Trade subscribes one channel per message (unlike the legacy
	// Exchange feed's "channels" array).
	for _, ch := range channels {
		sub := map[string]any{
			"type":        "subscribe",
			"channel":     ch,
			"product_ids": s.symbols,
		}
		if err := conn.WriteJSON(sub); err != nil {
			return fmt.Errorf("subscribe %s: %w", ch, err)
		}
	}

	s.SetState("connected")
	slog.Info("coinbase connected", "symbols", s.symbols, "channels", channels)

	// sequence_num is the connection-global counter (contiguous across all
	// products and frame types on this socket), so gap detection belongs
	// here, not in the per-instrument reference book. Reset per connection.
	var lastSeq uint64
	var seqInit bool

	for {
		if ctx.Err() != nil {
			return nil
		}
		_ = conn.SetReadDeadline(time.Now().Add(readTimeout))
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}
		s.RecordUpdate(len(msg), 0)
		if seq, ok := peekSequence(msg); ok {
			if seqInit && seq != lastSeq+1 {
				s.RecordError()
				slog.Warn("coinbase sequence gap", "expected", lastSeq+1, "got", seq)
			}
			lastSeq, seqInit = seq, true
		}
		events, perr := ParseMessage(msg)
		if perr != nil {
			s.RecordError()
			slog.Debug("coinbase parse error", "error", perr)
		}
		for _, ev := range events {
			select {
			case out <- ev:
			case <-ctx.Done():
				return nil
			}
		}
	}
}

// peekSequence extracts the connection-global sequence_num from a raw frame
// for gap detection, without committing to a full parse. ok is false when
// the field is absent (e.g. a non-data control frame).
func peekSequence(raw []byte) (uint64, bool) {
	var probe struct {
		SequenceNum *uint64 `json:"sequence_num"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil || probe.SequenceNum == nil {
		return 0, false
	}
	return *probe.SequenceNum, true
}

// --- wire types ---

type envelope struct {
	Channel     string          `json:"channel"`
	Timestamp   string          `json:"timestamp"`
	SequenceNum uint64          `json:"sequence_num"`
	Events      json.RawMessage `json:"events"`
}

type tradeEvent struct {
	Type   string `json:"type"`
	Trades []struct {
		ProductID string `json:"product_id"`
		Price     string `json:"price"`
		Size      string `json:"size"`
		Side      string `json:"side"` // "BUY" | "SELL" (aggressor)
		Time      string `json:"time"`
		TradeID   string `json:"trade_id"`
	} `json:"trades"`
}

type l2Event struct {
	Type      string `json:"type"` // "snapshot" | "update"
	ProductID string `json:"product_id"`
	Updates   []struct {
		Side        string `json:"side"` // "bid" | "offer"
		EventTime   string `json:"event_time"`
		PriceLevel  string `json:"price_level"`
		NewQuantity string `json:"new_quantity"` // "0" => remove level
	} `json:"updates"`
}

// ParseMessage decodes one Advanced Trade frame into zero or more
// normalized events. Stateless and pure for fixture-based testing. The
// Coinbase response channel for level2 is "l2_data" (the subscribe name is
// "level2"); subscription acks and heartbeats are ignored.
func ParseMessage(raw []byte) ([]feed.Event, error) {
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("coinbase: %w", err)
	}
	// Server send time, used as the book timestamp fallback when an update
	// carries no per-level event_time.
	envTime, _ := time.Parse(time.RFC3339Nano, env.Timestamp)

	switch env.Channel {
	case "market_trades":
		return parseTrades(env.Events)
	case "l2_data":
		return parseL2(env.Events, env.SequenceNum, envTime)
	default:
		return nil, nil
	}
}

func parseTrades(rawEvents json.RawMessage) ([]feed.Event, error) {
	var events []tradeEvent
	if err := json.Unmarshal(rawEvents, &events); err != nil {
		return nil, fmt.Errorf("coinbase trades: %w", err)
	}
	var out []feed.Event
	for _, ev := range events {
		for _, t := range ev.Trades {
			price, perr := strconv.ParseFloat(t.Price, 64)
			qty, qerr := strconv.ParseFloat(t.Size, 64)
			if perr != nil || qerr != nil || !feed.Finite(price) || !feed.Finite(qty) {
				continue // drop rather than emit a zero-priced trade
			}
			ts, _ := time.Parse(time.RFC3339Nano, t.Time)
			out = append(out, feed.Event{
				Kind: feed.EventTrade,
				Trade: &feed.Trade{
					Instrument:      t.ProductID,
					Exchange:        "coinbase",
					Timestamp:       ts,
					Price:           price,
					Quantity:        qty,
					Side:            strings.ToLower(t.Side),
					TradeID:         t.TradeID,
					PriceDecimal:    t.Price,
					QuantityDecimal: t.Size,
				},
			})
		}
	}
	return out, nil
}

func parseL2(rawEvents json.RawMessage, seq uint64, envTime time.Time) ([]feed.Event, error) {
	var events []l2Event
	if err := json.Unmarshal(rawEvents, &events); err != nil {
		return nil, fmt.Errorf("coinbase l2: %w", err)
	}
	var out []feed.Event
	for _, ev := range events {
		book := &feed.LOBSnapshot{
			Instrument:     ev.ProductID,
			Exchange:       "coinbase",
			SequenceNumber: seq,
			Snapshot:       ev.Type == "snapshot",
		}
		var latest time.Time
		for _, u := range ev.Updates {
			price, perr := strconv.ParseFloat(u.PriceLevel, 64)
			qty, qerr := strconv.ParseFloat(u.NewQuantity, 64)
			if perr != nil || qerr != nil || !feed.Finite(price) || !feed.Finite(qty) {
				// Dropping a level is safer than emitting price/qty 0:
				// qty 0 is the "remove this price" signal (see
				// feed.LOBSnapshot) and would corrupt the book.
				continue
			}
			level := feed.LOBLevel{
				Price:           price,
				Quantity:        qty,
				PriceDecimal:    u.PriceLevel,
				QuantityDecimal: u.NewQuantity,
			}
			switch u.Side {
			case "bid":
				book.Bids = append(book.Bids, level)
			case "offer", "ask":
				book.Asks = append(book.Asks, level)
			}
			if t, err := time.Parse(time.RFC3339Nano, u.EventTime); err == nil && t.After(latest) {
				latest = t
			}
		}
		// Prefer the latest per-level event_time; fall back to the frame's
		// server timestamp so a book event is never left zero-timed.
		if latest.IsZero() {
			latest = envTime
		}
		book.Timestamp = latest
		out = append(out, feed.Event{Kind: feed.EventBook, Book: book})
	}
	return out, nil
}
