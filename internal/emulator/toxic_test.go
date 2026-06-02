package emulator

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/zeta1999/crypto-exchange-golang/internal/feed"
	"github.com/zeta1999/crypto-exchange-golang/internal/orderbook"
	"github.com/zeta1999/crypto-exchange-golang/internal/reference"
	"github.com/zeta1999/crypto-exchange-golang/internal/toxicity"
	"github.com/zeta1999/crypto-exchange-golang/pkg/decimal"
)

// toxicSetup builds an engine with a resting user SELL at the ask, a reference
// book with a 100/101 touch, and a toxicity injector at the given scale.
func toxicSetup(t *testing.T, scale float64) (*reference.Book, *ToxicInjector, func() int) {
	t.Helper()
	eng := newEngine("BTC-USD")
	// Resting user sell at the ask — a buy sweep should pick it off.
	if _, _, err := eng.PlaceLimit(context.Background(), userLimit("user-sell", orderbook.SideSell, 101, 1)); err != nil {
		t.Fatal(err)
	}
	ref := reference.NewBook("BTC-USD", "coinbase")
	ref.Apply(bookSnap(1, time.Unix(1700000000, 0), true,
		[]feed.LOBLevel{lvl(100, 1)}, []feed.LOBLevel{lvl(101, 1)}))

	model := toxicity.New(toxicity.Config{
		Scale: scale, VPINWeight: 1, KyleWeight: 1,
		BucketVolume: 1, Buckets: 5, WindowTrades: 50, Seed: 1,
	})
	inj := NewToxicInjector(eng, ref, model, "BTC-USD", 1)

	// userSellRemaining returns how much of the user sell still rests at 101.
	remaining := func() int {
		snap, _ := eng.Snapshot("BTC-USD")
		for _, l := range snap.Asks {
			if l.Price.Eq(decimal.FromFloat(101)) {
				return int(l.Volume.Float64() * 1000) // milli-units, avoids float ==
			}
		}
		return 0
	}
	return ref, inj, remaining
}

func TestToxicityHighScalePicksOffUserOrder(t *testing.T) {
	_, inj, remaining := toxicSetup(t, 1.0)

	fills := 0
	for i := 0; i < 30; i++ {
		// Imbalanced buy flow drives VPIN→1, so Score→1 and (scale 1) it fires.
		tr, err := inj.Observe(context.Background(), tapeTrade("buy", 100.5, 1))
		if err != nil {
			t.Fatal(err)
		}
		fills += len(tr)
		if remaining() == 0 {
			break
		}
	}
	if fills == 0 {
		t.Fatal("high toxicity should have injected adverse fills")
	}
	if remaining() != 0 {
		t.Errorf("user sell should have been picked off, %d milli-units remain", remaining())
	}
}

func TestToxicityScaleZeroIsPureRTR(t *testing.T) {
	_, inj, remaining := toxicSetup(t, 0.0)

	fills := 0
	for i := 0; i < 100; i++ {
		tr, err := inj.Observe(context.Background(), tapeTrade("buy", 100.5, 1))
		if err != nil {
			t.Fatal(err)
		}
		fills += len(tr)
	}
	if fills != 0 {
		t.Errorf("scale=0 must never inject (pure RTR), got %d fills", fills)
	}
	if remaining() != 1000 { // full 1.0 still resting
		t.Errorf("user sell must be untouched at scale=0, %d milli-units remain", remaining())
	}
}

func TestToxicityNonFiniteDoesNotPanic(t *testing.T) {
	_, inj, _ := toxicSetup(t, 1.0)
	for _, bad := range []*feed.Trade{
		{Instrument: "BTC-USD", Side: "buy", Price: math.NaN(), Quantity: 1},
		{Instrument: "BTC-USD", Side: "sell", Price: math.Inf(1), Quantity: 1},
		{Instrument: "BTC-USD", Side: "buy", Price: 100, Quantity: math.Inf(1)},
	} {
		if tr, err := inj.Observe(context.Background(), bad); err != nil || len(tr) != 0 {
			t.Errorf("non-finite %+v: trades=%v err=%v (want dropped)", bad, tr, err)
		}
	}
}

func TestToxicityIgnoresOtherInstrumentAndSide(t *testing.T) {
	_, inj, _ := toxicSetup(t, 1.0)
	if tr, _ := inj.Observe(context.Background(), &feed.Trade{Instrument: "ETH-USD", Side: "buy", Price: 1, Quantity: 1}); len(tr) != 0 {
		t.Error("other instrument should be ignored")
	}
	if tr, _ := inj.Observe(context.Background(), &feed.Trade{Instrument: "BTC-USD", Side: "weird", Price: 1, Quantity: 1}); len(tr) != 0 {
		t.Error("unknown side should be ignored")
	}
}
