package emulator

import (
	"testing"
	"time"

	"github.com/zeta1999/crypto-exchange-golang/internal/feed"
	"github.com/zeta1999/crypto-exchange-golang/pkg/decimal"
)

func bookEvent() feed.Event {
	return feed.Event{
		Kind: feed.EventBook,
		Book: &feed.LOBSnapshot{
			Instrument: "BTC-USD",
			Timestamp:  time.Unix(1, 0),
			Snapshot:   true,
			Bids: []feed.LOBLevel{
				{Price: 100, Quantity: 2, PriceDecimal: "100", QuantityDecimal: "2"},
				{Price: 99.5, Quantity: 1, PriceDecimal: "99.5", QuantityDecimal: "1"},
			},
			Asks: []feed.LOBLevel{
				{Price: 101, Quantity: 3, PriceDecimal: "101", QuantityDecimal: "3"},
			},
		},
	}
}

func tradeEvent() feed.Event {
	return feed.Event{
		Kind: feed.EventTrade,
		Trade: &feed.Trade{
			Instrument:      "BTC-USD",
			Timestamp:       time.Unix(1, 0),
			Price:           100,
			Quantity:        2,
			Side:            "buy",
			PriceDecimal:    "100",
			QuantityDecimal: "2",
		},
	}
}

func TestPriceShiftIdentity(t *testing.T) {
	cases := []PriceShift{
		{},                       // zero value
		{OffsetBps: 0, Scale: 0}, // Scale 0 → treated as 1
		NewPriceShift(0, 0),      // constructor, both off
		NewPriceShift(0, 1),      // explicit no-op
	}
	for _, p := range cases {
		if !p.IsIdentity() {
			t.Fatalf("expected identity for %+v", p)
		}
		in := bookEvent()
		out := p.ApplyEvent(in)
		for i, l := range out.Book.Bids {
			if l.Price != in.Book.Bids[i].Price || l.PriceDecimal != in.Book.Bids[i].PriceDecimal {
				t.Fatalf("identity changed bid %d: %+v", i, l)
			}
		}
		tr := p.ApplyEvent(tradeEvent())
		if tr.Trade.Price != 100 || tr.Trade.PriceDecimal != "100" {
			t.Fatalf("identity changed trade: %+v", tr.Trade)
		}
	}
}

func TestPriceShiftOffsetBps(t *testing.T) {
	// +15 bps → factor 1.0015.
	p := NewPriceShift(15, 0)
	if p.IsIdentity() {
		t.Fatal("15bps shift should not be identity")
	}
	out := p.ApplyEvent(bookEvent())

	wantBid0 := decimal.MustParse("100").MulFloat(1.0015)
	got := out.Book.Bids[0]
	if got.PriceDecimal != wantBid0.StringPrec(decimal.ScaleDigits) {
		t.Fatalf("bid0 PriceDecimal = %q want %q", got.PriceDecimal, wantBid0.StringPrec(decimal.ScaleDigits))
	}
	if got.Price != wantBid0.Float64() {
		t.Fatalf("bid0 Price = %v want %v", got.Price, wantBid0.Float64())
	}
	if got.Quantity != 2 || got.QuantityDecimal != "2" {
		t.Fatalf("quantity changed: %+v", got)
	}
	// Ask shifted too.
	wantAsk := decimal.MustParse("101").MulFloat(1.0015)
	if out.Book.Asks[0].PriceDecimal != wantAsk.StringPrec(decimal.ScaleDigits) {
		t.Fatalf("ask PriceDecimal = %q want %q", out.Book.Asks[0].PriceDecimal, wantAsk.StringPrec(decimal.ScaleDigits))
	}

	// Trade event.
	tr := p.ApplyEvent(tradeEvent())
	wantTr := decimal.MustParse("100").MulFloat(1.0015)
	if tr.Trade.PriceDecimal != wantTr.StringPrec(decimal.ScaleDigits) {
		t.Fatalf("trade PriceDecimal = %q want %q", tr.Trade.PriceDecimal, wantTr.StringPrec(decimal.ScaleDigits))
	}
	if tr.Trade.Price != wantTr.Float64() {
		t.Fatalf("trade Price = %v want %v", tr.Trade.Price, wantTr.Float64())
	}
}

func TestPriceShiftScale(t *testing.T) {
	p := NewPriceShift(0, 2)
	out := p.ApplyEvent(bookEvent())
	want := decimal.MustParse("100").MulFloat(2)
	if out.Book.Bids[0].PriceDecimal != want.StringPrec(decimal.ScaleDigits) {
		t.Fatalf("scaled bid = %q want %q", out.Book.Bids[0].PriceDecimal, want.StringPrec(decimal.ScaleDigits))
	}
}

func TestPriceShiftCompose(t *testing.T) {
	// scale 2 and +100 bps → factor 2 * 1.01 = 2.02.
	p := NewPriceShift(100, 2)
	out := p.ApplyEvent(bookEvent())
	want := decimal.MustParse("100").MulFloat(2 * 1.01)
	if out.Book.Bids[0].PriceDecimal != want.StringPrec(decimal.ScaleDigits) {
		t.Fatalf("composed bid = %q want %q", out.Book.Bids[0].PriceDecimal, want.StringPrec(decimal.ScaleDigits))
	}
}

func TestPriceShiftDoesNotMutateInput(t *testing.T) {
	in := bookEvent()
	origBidPrice := in.Book.Bids[0].Price
	origBidDec := in.Book.Bids[0].PriceDecimal
	p := NewPriceShift(50, 0)
	_ = p.ApplyEvent(in)
	if in.Book.Bids[0].Price != origBidPrice || in.Book.Bids[0].PriceDecimal != origBidDec {
		t.Fatalf("input book mutated: %+v", in.Book.Bids[0])
	}

	tin := tradeEvent()
	origTrPrice := tin.Trade.Price
	origTrDec := tin.Trade.PriceDecimal
	_ = p.ApplyEvent(tin)
	if tin.Trade.Price != origTrPrice || tin.Trade.PriceDecimal != origTrDec {
		t.Fatalf("input trade mutated: %+v", tin.Trade)
	}
}

func TestPriceShiftFallbackEmptyDecimal(t *testing.T) {
	// Empty/unparseable PriceDecimal falls back to FromFloat(price).
	p := NewPriceShift(0, 2)
	ev := feed.Event{Kind: feed.EventBook, Book: &feed.LOBSnapshot{
		Bids: []feed.LOBLevel{{Price: 50, Quantity: 1}}, // no PriceDecimal
	}}
	out := p.ApplyEvent(ev)
	want := decimal.FromFloat(50).MulFloat(2)
	if out.Book.Bids[0].PriceDecimal != want.StringPrec(decimal.ScaleDigits) {
		t.Fatalf("fallback = %q want %q", out.Book.Bids[0].PriceDecimal, want.StringPrec(decimal.ScaleDigits))
	}
}

func TestNewPriceShiftRejectsDestructiveFactor(t *testing.T) {
	// offset_bps = -10000 → factor 0; negative scale → negative factor.
	// Both must collapse to the identity rather than zero/flip prices.
	for _, p := range []PriceShift{
		NewPriceShift(-10000, 1),
		NewPriceShift(-20000, 1),
		NewPriceShift(0, -1),
	} {
		if !p.IsIdentity() {
			t.Errorf("destructive config %+v should be identity, factor=%v", p, p.factor())
		}
	}
}
