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

// Start opens the websocket and returns a channel of normalized events.
// It reconnects with a fixed backoff until ctx is cancelled, then closes
// the channel.
func (s *Source) Start(ctx context.Context) (<-chan feed.Event, error) {
	out := make(chan feed.Event, 1024)
	go func() {
		defer close(out)
		for {
			if ctx.Err() != nil {
				s.SetState("closed")
				return
			}
			err := s.connectAndStream(ctx, out)
			if ctx.Err() != nil {
				s.SetState("closed")
				return
			}
			if err != nil {
				s.RecordError()
				s.SetState("reconnecting")
				slog.Warn("binance connection lost, reconnecting", "error", err)
				select {
				case <-ctx.Done():
				case <-time.After(5 * time.Second):
				}
			}
		}
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

	s.SetState("connected")
	slog.Info("binance connected", "streams", len(streams))

	for {
		if ctx.Err() != nil {
			return nil
		}
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}
		start := time.Now()
		events, perr := ParseMessage(msg)
		if perr != nil {
			slog.Debug("binance parse error", "error", perr)
		}
		for _, ev := range events {
			select {
			case out <- ev:
			case <-ctx.Done():
				return nil
			}
		}
		s.RecordUpdate(len(msg), time.Since(start))
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
// normalized events. It is stateless and pure so it can be unit-tested
// against recorded fixtures without a live socket.
func ParseMessage(raw []byte) ([]feed.Event, error) {
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
		ev, err := parseDepth(wrapper.Stream, wrapper.Data)
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
	price, _ := strconv.ParseFloat(msg.Price, 64)
	qty, _ := strconv.ParseFloat(msg.Quantity, 64)
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

func parseDepth(stream string, data json.RawMessage) (feed.Event, error) {
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
			Timestamp:  time.Now(),
			Snapshot:   true, // @depth20 is a full top-of-book replacement
			Bids:       parseLevels(msg.Bids),
			Asks:       parseLevels(msg.Asks),
		},
	}, nil
}

func parseLevels(raw [][]string) []feed.LOBLevel {
	levels := make([]feed.LOBLevel, 0, len(raw))
	for _, l := range raw {
		if len(l) < 2 {
			continue
		}
		price, _ := strconv.ParseFloat(l[0], 64)
		qty, _ := strconv.ParseFloat(l[1], 64)
		levels = append(levels, feed.LOBLevel{
			Price:           price,
			Quantity:        qty,
			PriceDecimal:    l[0],
			QuantityDecimal: l[1],
		})
	}
	return levels
}
