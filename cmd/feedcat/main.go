// Command feedcat connects to a feed.Source (binance, coinbase, or a
// recorded replay file) and prints normalized trades and book tops to
// stdout. With -record it also persists every event to a JSONL file for
// later deterministic replay.
//
// Examples:
//
//	feedcat -venue coinbase -symbols BTC-USD,ETH-USD
//	feedcat -venue binance -symbols BTCUSDT -record cap.jsonl
//	feedcat -venue replay -file cap.jsonl
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/zeta1999/crypto-exchange-golang/internal/feed"
	"github.com/zeta1999/crypto-exchange-golang/internal/feed/binance"
	"github.com/zeta1999/crypto-exchange-golang/internal/feed/coinbase"
	"github.com/zeta1999/crypto-exchange-golang/internal/feed/replay"
)

func main() {
	venue := flag.String("venue", "coinbase", "feed source: binance | coinbase | replay")
	symbolsCSV := flag.String("symbols", "BTC-USD", "comma-separated symbols (Binance: BTCUSDT; Coinbase: BTC-USD)")
	file := flag.String("file", "", "replay input file (required for -venue replay)")
	record := flag.String("record", "", "write all events as JSONL to this file")
	duration := flag.Duration("duration", 0, "stop after this long (0 = run until interrupted / EOF)")
	flag.Parse()

	if *record != "" && *record == *file {
		log.Fatalf("feedcat: -record would overwrite the -file replay input %q", *file)
	}

	src, err := buildSource(*venue, splitCSV(*symbolsCSV), *file)
	if err != nil {
		log.Fatalf("feedcat: %v", err)
	}

	rec, closeRec, err := buildRecorder(*record)
	if err != nil {
		log.Fatalf("feedcat: %v", err)
	}
	if closeRec != nil {
		defer closeRec()
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if *duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *duration)
		defer cancel()
	}

	events, err := src.Start(ctx)
	if err != nil {
		log.Fatalf("feedcat: start %s: %v", src.Name(), err)
	}
	log.Printf("feedcat: streaming from %s", src.Name())

	var trades, books uint64
	for {
		select {
		case <-ctx.Done():
			log.Printf("feedcat: done — %d trades, %d book updates; status=%+v", trades, books, src.Status())
			return
		case ev, ok := <-events:
			if !ok {
				log.Printf("feedcat: source closed — %d trades, %d book updates; status=%+v", trades, books, src.Status())
				return
			}
			if rec != nil {
				if err := rec.Record(ev); err != nil {
					log.Printf("feedcat: record error: %v", err)
				}
			}
			switch ev.Kind {
			case feed.EventTrade:
				trades++
				printTrade(ev.Trade)
			case feed.EventBook:
				books++
				printBook(ev.Book)
			case feed.EventTicker:
				printTicker(ev.Ticker)
			}
		}
	}
}

func buildSource(venue string, symbols []string, file string) (feed.Source, error) {
	switch strings.ToLower(venue) {
	case "binance":
		return binance.New(binance.Config{Symbols: symbols}), nil
	case "coinbase":
		return coinbase.New(coinbase.Config{Symbols: symbols}), nil
	case "replay":
		if file == "" {
			return nil, fmt.Errorf("-file is required for -venue replay")
		}
		return replay.New(file), nil
	default:
		return nil, fmt.Errorf("unknown venue %q (want binance|coinbase|replay)", venue)
	}
}

func buildRecorder(path string) (*replay.Recorder, func(), error) {
	if path == "" {
		return nil, nil, nil
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, nil, fmt.Errorf("create record file: %w", err)
	}
	return replay.NewRecorder(f), func() { _ = f.Close() }, nil
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func printTrade(t *feed.Trade) {
	if t == nil {
		return
	}
	// Prefer the venue's exact decimal strings; fall back to the floats for
	// events that don't carry them (e.g. synthetic or replayed captures).
	price := orFloat(t.PriceDecimal, t.Price)
	qty := orFloat(t.QuantityDecimal, t.Quantity)
	fmt.Printf("%s TRADE %-10s %-8s %s @ %s  %4s\n",
		t.Timestamp.UTC().Format(time.RFC3339Nano), t.Exchange, t.Instrument,
		qty, price, strings.ToUpper(t.Side))
}

// orFloat returns dec if non-empty, else a compact rendering of f.
func orFloat(dec string, f float64) string {
	if dec != "" {
		return dec
	}
	return fmt.Sprintf("%g", f)
}

func printBook(b *feed.LOBSnapshot) {
	if b == nil {
		return
	}
	kind := "DIFF"
	if b.Snapshot {
		kind = "SNAP"
	}
	bid, ask := "-", "-"
	if len(b.Bids) > 0 {
		bid = fmt.Sprintf("%g x %g", b.Bids[0].Price, b.Bids[0].Quantity)
	}
	if len(b.Asks) > 0 {
		ask = fmt.Sprintf("%g x %g", b.Asks[0].Price, b.Asks[0].Quantity)
	}
	fmt.Printf("%s BOOK  %-10s %-8s %s seq=%d  bid[%s] ask[%s]  (%dxb/%dxa)\n",
		b.Timestamp.UTC().Format(time.RFC3339Nano), b.Exchange, b.Instrument, kind,
		b.SequenceNumber, bid, ask, len(b.Bids), len(b.Asks))
}

func printTicker(t *feed.Ticker) {
	if t == nil {
		return
	}
	fmt.Printf("%s TICK  %-10s %-8s bid=%g ask=%g last=%g\n",
		t.Timestamp.UTC().Format(time.RFC3339Nano), t.Exchange, t.Instrument, t.Bid, t.Ask, t.Last)
}
