// Package binance implements a feed.Source for Binance spot market data.
//
// It subscribes to the combined stream endpoint for @trade and
// @depth20@100ms and normalizes both into feed.Event values. Adapted from
// github.com/notbbg/notbbg internal/feeds/ccxt/binance.go (kline/backfill
// paths dropped; pub/sub bus replaced with a channel — see STATUS.md).
package binance

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

// DefaultWSURL is the Binance spot combined-stream base.
const DefaultWSURL = "wss://stream.binance.com:9443/ws"

// Source streams Binance trades and depth as normalized feed.Events.
type Source struct {
	*feed.StatusTracker
	symbols   []string
	feedTypes []string
	wsURL     string
}

// Config configures a Binance Source.
type Config struct {
	// Symbols are Binance symbols, e.g. "BTCUSDT". Case-insensitive.
	Symbols []string
	// FeedTypes selects channels: "trades" (@trade) and/or "orderbook"
	// (@depth20@100ms). Empty defaults to both.
	FeedTypes []string
	// WSURL overrides the default endpoint (used by tests/staging).
	WSURL string
}

// New constructs a Binance Source from cfg.
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
		StatusTracker: feed.NewStatusTracker("binance"),
		symbols:       cfg.Symbols,
		feedTypes:     feedTypes,
		wsURL:         wsURL,
	}
}

func (s *Source) Name() string { return "binance" }

// streams builds the list of stream names for the configured symbols and
// feed types, e.g. ["btcusdt@trade", "btcusdt@depth20@100ms"].
func (s *Source) streams() []string {
	var out []string
	for _, sym := range s.symbols {
		l := strings.ToLower(sym)
		for _, ft := range s.feedTypes {
			switch ft {
			case "trades":
				out = append(out, l+"@trade")
			case "orderbook":
				out = append(out, l+"@depth20@100ms")
			}
		}
	}
	return out
}

// readTimeout bounds how long a single read may block. Binance pushes
// continuously, so a silent (half-open) socket trips this deadline and the
// reconnect loop recovers instead of blocking forever.
const readTimeout = 60 * time.Second

// Start opens the websocket and returns a channel of normalized events.
// It reconnects with capped exponential backoff until ctx is cancelled,
// then closes the channel.
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
	streams := s.streams()
	if len(streams) == 0 {
		return fmt.Errorf("binance: no streams configured")
	}
	url := strings.TrimSuffix(s.wsURL, "/ws") + "/stream?streams=" + strings.Join(streams, "/")

	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	conn, _, err := dialer.DialContext(ctx, url, nil)
	if err != nil {
		return fmt.Errorf("dial binance: %w", err)
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

	s.SetState("connected")
	slog.Info("binance connected", "streams", len(streams))

	for {
		if ctx.Err() != nil {
			return nil
		}
		_ = conn.SetReadDeadline(time.Now().Add(readTimeout))
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}
		recv := time.Now()
		s.RecordUpdate(len(msg), 0)
		events, perr := ParseMessage(msg, recv)
		if perr != nil {
			s.RecordError()
			slog.Debug("binance parse error", "error", perr)
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

// --- wire types ---

type streamMsg struct {
	Stream string          `json:"stream"`
	Data   json.RawMessage `json:"data"`
}

type tradeMsg struct {
	Symbol   string `json:"s"`
	Price    string `json:"p"`
	Quantity string `json:"q"`
	Time     int64  `json:"T"`
	IsMaker  bool   `json:"m"` // true => buyer is the maker => sell aggressor
	TradeID  int64  `json:"t"`
}

type depthMsg struct {
	Symbol string     `json:"s,omitempty"`
	Bids   [][]string `json:"bids"`
	Asks   [][]string `json:"asks"`
}

// ParseMessage decodes one combined-stream frame into zero or more
// normalized events. It is pure (deterministic in its inputs) so it can be
// unit-tested against recorded fixtures without a live socket. recv is the
// time the frame was read; it stamps the book snapshot, which — unlike a
// trade — carries no event time on the @depth20 wire.
func ParseMessage(raw []byte, recv time.Time) ([]feed.Event, error) {
	var wrapper streamMsg
	if err := json.Unmarshal(raw, &wrapper); err != nil || wrapper.Stream == "" {
		return nil, fmt.Errorf("binance: not a combined-stream message")
	}
	switch {
	case strings.Contains(wrapper.Stream, "@trade"):
		ev, err := parseTrade(wrapper.Data)
		if err != nil {
			return nil, err
		}
		return []feed.Event{ev}, nil
	case strings.Contains(wrapper.Stream, "@depth"):
		ev, err := parseDepth(wrapper.Stream, wrapper.Data, recv)
		if err != nil {
			return nil, err
		}
		return []feed.Event{ev}, nil
	default:
		return nil, nil
	}
}

func parseTrade(data json.RawMessage) (feed.Event, error) {
	var msg tradeMsg
	if err := json.Unmarshal(data, &msg); err != nil {
		return feed.Event{}, fmt.Errorf("binance trade: %w", err)
	}
	price, perr := strconv.ParseFloat(msg.Price, 64)
	qty, qerr := strconv.ParseFloat(msg.Quantity, 64)
	if perr != nil || qerr != nil || !feed.Finite(price) || !feed.Finite(qty) {
		return feed.Event{}, fmt.Errorf("binance trade: bad price/qty %q/%q", msg.Price, msg.Quantity)
	}
	side := "buy"
	if msg.IsMaker {
		side = "sell"
	}
	return feed.Event{
		Kind: feed.EventTrade,
		Trade: &feed.Trade{
			Instrument:      msg.Symbol,
			Exchange:        "binance",
			Timestamp:       time.UnixMilli(msg.Time),
			Price:           price,
			Quantity:        qty,
			Side:            side,
			TradeID:         strconv.FormatInt(msg.TradeID, 10),
			PriceDecimal:    msg.Price,
			QuantityDecimal: msg.Quantity,
		},
	}, nil
}

func parseDepth(stream string, data json.RawMessage, recv time.Time) (feed.Event, error) {
	var msg depthMsg
	if err := json.Unmarshal(data, &msg); err != nil {
		return feed.Event{}, fmt.Errorf("binance depth: %w", err)
	}
	symbol := msg.Symbol
	if symbol == "" {
		// "btcusdt@depth20@100ms" -> "BTCUSDT"
		if i := strings.IndexByte(stream, '@'); i > 0 {
			symbol = strings.ToUpper(stream[:i])
		}
	}
	return feed.Event{
		Kind: feed.EventBook,
		Book: &feed.LOBSnapshot{
			Instrument: symbol,
			Exchange:   "binance",
			Timestamp:  recv,
			Snapshot:   true, // @depth20 is a full top-of-book replacement
			Bids:       parseLevels(msg.Bids),
			Asks:       parseLevels(msg.Asks),
		},
	}, nil
}

// parseLevels converts [price, qty] string pairs to levels, skipping any
// that fail to parse. A skipped level is dropped rather than emitted as a
// zero — a zero quantity is the book's "remove this price" signal, so a
// silent parse-to-zero would corrupt the book downstream.
func parseLevels(raw [][]string) []feed.LOBLevel {
	levels := make([]feed.LOBLevel, 0, len(raw))
	for _, l := range raw {
		if len(l) < 2 {
			continue
		}
		price, perr := strconv.ParseFloat(l[0], 64)
		qty, qerr := strconv.ParseFloat(l[1], 64)
		if perr != nil || qerr != nil || !feed.Finite(price) || !feed.Finite(qty) {
			continue
		}
		levels = append(levels, feed.LOBLevel{
			Price:           price,
			Quantity:        qty,
			PriceDecimal:    l[0],
			QuantityDecimal: l[1],
		})
	}
	return levels
}
