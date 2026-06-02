package replay

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zeta1999/crypto-exchange-golang/internal/feed"
)

func sampleEvents() []feed.Event {
	ts := time.Unix(1700000000, 0).UTC()
	return []feed.Event{
		{Kind: feed.EventBook, Book: &feed.LOBSnapshot{
			Instrument: "BTC-USD", Exchange: "coinbase", Timestamp: ts, Snapshot: true,
			Bids: []feed.LOBLevel{{Price: 41999, Quantity: 1.2}},
			Asks: []feed.LOBLevel{{Price: 42001, Quantity: 0.5}},
		}},
		{Kind: feed.EventTrade, Trade: &feed.Trade{
			Instrument: "BTC-USD", Exchange: "coinbase", Timestamp: ts.Add(time.Second),
			Price: 42000.5, Quantity: 0.01, Side: "buy", TradeID: "99",
		}},
		{Kind: feed.EventTrade, Trade: &feed.Trade{
			Instrument: "BTCUSDT", Exchange: "binance", Timestamp: ts.Add(2 * time.Second),
			Price: 42000.6, Quantity: 0.02, Side: "sell", TradeID: "100",
		}},
	}
}

func collect(t *testing.T, s *Source) []feed.Event {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ch, err := s.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	var got []feed.Event
	for ev := range ch {
		got = append(got, ev)
	}
	return got
}

func TestRecordReplayRoundTrip(t *testing.T) {
	want := sampleEvents()

	var buf bytes.Buffer
	rec := NewRecorder(&buf)
	for _, ev := range want {
		if err := rec.Record(ev); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}

	got := collect(t, NewReader(&buf))
	assertEventsEqual(t, want, got)
}

func TestReplayFromFile(t *testing.T) {
	want := sampleEvents()
	path := filepath.Join(t.TempDir(), "cap.jsonl")

	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	rec := NewRecorder(f)
	for _, ev := range want {
		if err := rec.Record(ev); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	f.Close()

	got := collect(t, New(path))
	assertEventsEqual(t, want, got)
}

func TestReplayMissingFile(t *testing.T) {
	if _, err := New(filepath.Join(t.TempDir(), "nope.jsonl")).Start(context.Background()); err == nil {
		t.Error("expected error opening missing file")
	}
}

func TestReplaySkipsBlankAndBadLines(t *testing.T) {
	input := "\n{\"kind\":\"trade\",\"trade\":{\"instrument\":\"BTC-USD\",\"price\":1,\"side\":\"buy\"}}\nnot-json\n\n"
	got := collect(t, NewReader(bytes.NewBufferString(input)))
	if len(got) != 1 {
		t.Fatalf("want 1 event (blank+bad skipped), got %d", len(got))
	}
	if got[0].Trade.Instrument != "BTC-USD" {
		t.Errorf("instrument = %q", got[0].Trade.Instrument)
	}
}

func assertEventsEqual(t *testing.T, want, got []feed.Event) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("event count = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Kind != want[i].Kind {
			t.Errorf("event[%d] kind = %v, want %v", i, got[i].Kind, want[i].Kind)
		}
		switch want[i].Kind {
		case feed.EventTrade:
			w, g := want[i].Trade, got[i].Trade
			if g.Instrument != w.Instrument || g.Price != w.Price || g.Side != w.Side || g.TradeID != w.TradeID {
				t.Errorf("event[%d] trade = %+v, want %+v", i, g, w)
			}
			if !g.Timestamp.Equal(w.Timestamp) {
				t.Errorf("event[%d] ts = %v, want %v", i, g.Timestamp, w.Timestamp)
			}
		case feed.EventBook:
			w, g := want[i].Book, got[i].Book
			if g.Instrument != w.Instrument || g.Snapshot != w.Snapshot || len(g.Bids) != len(w.Bids) || len(g.Asks) != len(w.Asks) {
				t.Errorf("event[%d] book = %+v, want %+v", i, g, w)
			}
		}
	}
}
