package binance

import (
	"testing"
	"time"

	"github.com/zeta1999/crypto-exchange-golang/internal/orderbook"
	"github.com/zeta1999/crypto-exchange-golang/pkg/decimal"
)

func mkSnap(bids, asks [][2]string) *orderbook.Snapshot {
	lv := func(pairs [][2]string) []orderbook.Level {
		out := make([]orderbook.Level, 0, len(pairs))
		for _, p := range pairs {
			out = append(out, orderbook.Level{
				Price:  decimal.MustParse(p[0]),
				Volume: decimal.MustParse(p[1]),
			})
		}
		return out
	}
	return &orderbook.Snapshot{Bids: lv(bids), Asks: lv(asks)}
}

// TestDepthDiffer exercises the diff/U-u semantics deterministically without
// any timing: first sighting seeds silently; later changes advance the id by
// one; removed levels report qty "0"; a quiet diff reports no change.
func TestDepthDiffer(t *testing.T) {
	base := int64(1_700_000_000_000)
	d := newDepthDiffer(func() time.Time { return time.UnixMilli(base).UTC() })

	// First call seeds the baseline and emits nothing.
	if _, _, _, _, changed := d.diff("BTC-USD", mkSnap(
		[][2]string{{"100", "5"}}, [][2]string{{"101", "3"}})); changed {
		t.Fatalf("first diff should seed silently (changed=false)")
	}
	if got := d.current("BTC-USD"); got != base {
		t.Fatalf("current id = %d, want seed %d", got, base)
	}

	// Add a new bid level + change an ask qty: one diff, id advances by 1.
	first, final, bids, asks, changed := d.diff("BTC-USD", mkSnap(
		[][2]string{{"100", "5"}, {"99", "2"}}, [][2]string{{"101", "4"}}))
	if !changed {
		t.Fatalf("expected a change")
	}
	if first != base+1 || final != base+1 {
		t.Fatalf("U/u = %d/%d, want %d/%d", first, final, base+1, base+1)
	}
	if len(bids) != 1 || bids[0] != [2]string{"99.00000000", "2.00000000"} {
		t.Fatalf("bid diff = %v, want the new 99 level", bids)
	}
	if len(asks) != 1 || asks[0] != [2]string{"101.00000000", "4.00000000"} {
		t.Fatalf("ask diff = %v, want the changed 101 qty", asks)
	}
	if got := d.current("BTC-USD"); got != base+1 {
		t.Fatalf("current after change = %d, want %d", got, base+1)
	}

	// Remove the 99 bid: reported as qty "0"; id advances to base+2 (U==prev u+1).
	first, final, bids, _, changed = d.diff("BTC-USD", mkSnap(
		[][2]string{{"100", "5"}}, [][2]string{{"101", "4"}}))
	if !changed || first != base+2 || final != base+2 {
		t.Fatalf("removal diff U/u = %d/%d changed=%v, want %d/%d true", first, final, changed, base+2, base+2)
	}
	if len(bids) != 1 || bids[0] != [2]string{"99.00000000", "0"} {
		t.Fatalf("removal diff = %v, want 99 -> 0", bids)
	}

	// No change: no diff, id unchanged.
	if _, _, _, _, changed := d.diff("BTC-USD", mkSnap(
		[][2]string{{"100", "5"}}, [][2]string{{"101", "4"}})); changed {
		t.Fatalf("identical book should not produce a diff")
	}
	if got := d.current("BTC-USD"); got != base+2 {
		t.Fatalf("current after no-op = %d, want %d", got, base+2)
	}
}
