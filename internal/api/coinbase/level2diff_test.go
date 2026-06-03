package coinbase

import (
	"encoding/json"
	"testing"

	"github.com/zeta1999/crypto-exchange-golang/internal/orderbook"
	"github.com/zeta1999/crypto-exchange-golang/pkg/decimal"
)

func l2Snap(bids, asks [][2]string) *orderbook.Snapshot {
	lv := func(pairs [][2]string) []orderbook.Level {
		out := make([]orderbook.Level, 0, len(pairs))
		for _, p := range pairs {
			out = append(out, orderbook.Level{Price: decimal.MustParse(p[0]), Volume: decimal.MustParse(p[1])})
		}
		return out
	}
	return &orderbook.Snapshot{Bids: lv(bids), Asks: lv(asks)}
}

// TestLevel2Differ checks incremental semantics deterministically: first diff
// seeds silently; a changed/added level is emitted; a removed level reports
// new_quantity "0"; an unchanged book produces no update.
func TestLevel2Differ(t *testing.T) {
	d := newLevel2Differ()
	const et = "t"

	// First sighting seeds the baseline, emits nothing.
	if _, changed := d.diff("BTC-USD", l2Snap([][2]string{{"100", "5"}}, [][2]string{{"101", "3"}}), et); changed {
		t.Fatalf("first diff should seed silently")
	}

	// Add a bid level + change the ask qty.
	ups, changed := d.diff("BTC-USD", l2Snap([][2]string{{"100", "5"}, {"99", "2"}}, [][2]string{{"101", "4"}}), et)
	if !changed {
		t.Fatalf("expected a change")
	}
	got := map[string]string{} // "side@price" -> qty
	for _, u := range ups {
		got[u.Side+"@"+u.PriceLevel] = u.NewQuantity
	}
	if got["bid@99.00000000"] != "2.00000000" {
		t.Fatalf("missing new bid 99: %+v", got)
	}
	if got["offer@101.00000000"] != "4.00000000" {
		t.Fatalf("missing changed ask 101: %+v", got)
	}
	if len(ups) != 2 {
		t.Fatalf("expected exactly 2 changed levels, got %+v", ups)
	}

	// Remove the 99 bid -> reported as "0".
	ups, changed = d.diff("BTC-USD", l2Snap([][2]string{{"100", "5"}}, [][2]string{{"101", "4"}}), et)
	if !changed || len(ups) != 1 || ups[0].Side != "bid" || ups[0].PriceLevel != "99.00000000" || ups[0].NewQuantity != "0" {
		t.Fatalf("removal diff = %+v, want bid 99 -> 0", ups)
	}

	// No change -> no update.
	if _, changed := d.diff("BTC-USD", l2Snap([][2]string{{"100", "5"}}, [][2]string{{"101", "4"}}), et); changed {
		t.Fatalf("identical book should not produce an update")
	}
}

// TestWSLevel2Incremental: after the snapshot, a new book level arrives as an
// "update" frame carrying only that changed level (not the whole book).
func TestWSLevel2Incremental(t *testing.T) {
	h := newWSHarness(t)
	wsAddOrder(t, h.book, "seed-bid", orderbook.SideBuy, "100", "5")
	wsAddOrder(t, h.book, "seed-ask", orderbook.SideSell, "101", "3")

	c := h.dial(t)
	wsSend(t, c, map[string]interface{}{
		"type":        "subscribe",
		"channel":     "level2",
		"product_ids": []string{"BTC-USD"},
	})

	// First l2_data is the snapshot (baseline seeded from the book above).
	snapEnv := wsReadChannel(t, c, "l2_data")
	var snapEv l2Event
	if err := json.Unmarshal(snapEnv.Events[0], &snapEv); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	if snapEv.Type != "snapshot" {
		t.Fatalf("first frame type = %q, want snapshot", snapEv.Type)
	}

	// Introduce a brand-new bid level; the next l2_data must be an incremental
	// update carrying only that level.
	wsAddOrder(t, h.book, "new-bid", orderbook.SideBuy, "99", "2")

	updEnv := wsReadChannel(t, c, "l2_data")
	var updEv l2Event
	if err := json.Unmarshal(updEnv.Events[0], &updEv); err != nil {
		t.Fatalf("decode update: %v", err)
	}
	if updEv.Type != "update" {
		t.Fatalf("frame type = %q, want update", updEv.Type)
	}
	if len(updEv.Updates) != 1 {
		t.Fatalf("update should carry only the changed level, got %+v", updEv.Updates)
	}
	u := updEv.Updates[0]
	if u.Side != "bid" || u.PriceLevel != "99.00000000" || u.NewQuantity != "2.00000000" {
		t.Fatalf("update = %+v, want bid 99 -> 2", u)
	}
}
