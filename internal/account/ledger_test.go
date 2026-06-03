package account

import (
	"testing"

	"github.com/zeta1999/crypto-exchange-golang/pkg/decimal"
)

func d(s string) decimal.Decimal { return decimal.MustParse(s) }

func seed(b map[string]string) *Ledger {
	m := make(map[string]decimal.Decimal, len(b))
	for k, v := range b {
		m[k] = d(v)
	}
	return NewLedger(m)
}

func (l *Ledger) assert(t *testing.T, asset, free, locked string) {
	t.Helper()
	b := l.Get(asset)
	if b.Free.String() != d(free).String() || b.Locked.String() != d(locked).String() {
		t.Errorf("%s = free %s / locked %s, want free %s / locked %s", asset, b.Free, b.Locked, free, locked)
	}
}

func TestLedgerLockUnlock(t *testing.T) {
	l := seed(map[string]string{"USD": "1000"})
	if !l.Lock("USD", d("600")) {
		t.Fatal("lock 600 should succeed")
	}
	l.assert(t, "USD", "400", "600")
	if l.Lock("USD", d("500")) {
		t.Fatal("lock 500 should fail (only 400 free)")
	}
	l.assert(t, "USD", "400", "600") // unchanged on failed lock
	l.Unlock("USD", d("600"))
	l.assert(t, "USD", "1000", "0")
}

// TestSettleBuyLimitPriceImprovement: a buy that fills below its limit refunds
// the difference to free; nothing remains locked after a full fill.
func TestSettleBuyLimitPriceImprovement(t *testing.T) {
	l := seed(map[string]string{"USD": "1000"})
	l.Lock("USD", d("1000")) // buy 1 BTC @ limit 1000
	l.SettleFill(true, "BTC", "USD", d("1"), d("900"), d("1000"))
	l.assert(t, "USD", "100", "0") // spent 900, 100 refunded
	l.assert(t, "BTC", "1", "0")
}

// TestSettleBuyPartialThenCancel: partial fill settles its portion; cancel
// releases exactly the unfilled reservation.
func TestSettleBuyPartialThenCancel(t *testing.T) {
	l := seed(map[string]string{"USD": "1000"})
	l.Lock("USD", d("1000")) // buy 1 @ 1000
	l.SettleFill(true, "BTC", "USD", d("0.4"), d("1000"), d("1000"))
	l.assert(t, "USD", "0", "600")
	l.assert(t, "BTC", "0.4", "0")
	// cancel: unlock remaining 0.6 @ 1000 = 600
	l.Unlock("USD", d("600"))
	l.assert(t, "USD", "600", "0")
}

func TestSettleSellLimit(t *testing.T) {
	l := seed(map[string]string{"BTC": "5"})
	l.Lock("BTC", d("2")) // sell 2 @ limit 100
	l.SettleFill(false, "BTC", "USD", d("2"), d("110"), d("100"))
	l.assert(t, "BTC", "3", "0")
	l.assert(t, "USD", "220", "0") // 2 * 110
}

func TestSettleMarketFromFree(t *testing.T) {
	l := seed(map[string]string{"USD": "1000"})
	// MARKET buy: reservedPrice 0 → debited from free, no lock.
	l.SettleFill(true, "BTC", "USD", d("1"), d("950"), decimal.Zero)
	l.assert(t, "USD", "50", "0")
	l.assert(t, "BTC", "1", "0")
}

// TestSettleMarketUnderfundedNoMint: an underfunded MARKET buy must not mint
// the bought asset — it credits base only in proportion to what it could pay.
func TestSettleMarketUnderfundedNoMint(t *testing.T) {
	l := seed(map[string]string{"USD": "100"})
	// MARKET buy 1 BTC @ 950 needs 950, only 100 free → buy 100/950 ≈ 0.105 BTC.
	l.SettleFill(true, "BTC", "USD", d("1"), d("950"), decimal.Zero)
	if got := l.Get("USD").Free; got.Sign() != 0 {
		t.Errorf("USD free = %s, want 0 (all spent)", got)
	}
	btc := l.Get("BTC").Free
	if !btc.Lt(d("1")) || btc.Sign() <= 0 {
		t.Errorf("BTC credited = %s, want a proportional fraction < 1 (no minting)", btc)
	}
	// Conservation: BTC * fillPrice must equal what was actually paid (100).
	if paid := btc.Mul(d("950")); paid.Gt(d("100")) {
		t.Errorf("credited BTC worth %s > 100 paid — minted value", paid)
	}
}

// TestSettleMarketSellUnderfunded: selling more base than held sells only what
// is held (no negative base, no over-credit of quote).
func TestSettleMarketSellUnderfunded(t *testing.T) {
	l := seed(map[string]string{"BTC": "0.5"})
	l.SettleFill(false, "BTC", "USD", d("2"), d("100"), decimal.Zero) // try to sell 2, have 0.5
	l.assert(t, "BTC", "0", "0")
	l.assert(t, "USD", "50", "0") // 0.5 * 100
}

func TestLedgerSnapshotSorted(t *testing.T) {
	l := seed(map[string]string{"USD": "1", "BTC": "2", "ETH": "3"})
	snap := l.Snapshot()
	if len(snap) != 3 || snap[0].Asset != "BTC" || snap[1].Asset != "ETH" || snap[2].Asset != "USD" {
		t.Fatalf("snapshot not sorted: %+v", snap)
	}
}
