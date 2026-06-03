// Package account provides a per-asset balance ledger for the API edges: a
// single test account whose free/locked balances are seeded from config and
// updated as orders are placed (lock), cancelled (unlock), and filled (settle).
// It is the source of truth behind the Binance /account and Coinbase /accounts
// endpoints — replacing the previous static stubs — so an external client sees
// coherent balances that move as it trades. Transfers/withdrawals are out of
// scope (deferred).
package account

import (
	"sort"
	"sync"

	"github.com/zeta1999/crypto-exchange-golang/pkg/decimal"
)

// Balance is an asset's free (spendable) and locked (reserved by open orders)
// amount.
type Balance struct {
	Asset  string
	Free   decimal.Decimal
	Locked decimal.Decimal
}

type holding struct {
	free   decimal.Decimal
	locked decimal.Decimal
}

// Ledger is a concurrency-safe per-asset balance book.
type Ledger struct {
	mu  sync.Mutex
	bal map[string]*holding
}

// NewLedger seeds a ledger with initial free balances (locked starts at zero).
func NewLedger(initial map[string]decimal.Decimal) *Ledger {
	l := &Ledger{bal: make(map[string]*holding, len(initial))}
	for asset, v := range initial {
		l.bal[asset] = &holding{free: v}
	}
	return l
}

func (l *Ledger) at(asset string) *holding {
	h := l.bal[asset]
	if h == nil {
		h = &holding{}
		l.bal[asset] = h
	}
	return h
}

// Lock moves amt of asset from free to locked, reserving it for an open order.
// It returns false (and changes nothing) if free is insufficient — the caller
// should then reject the order as insufficient balance. A non-positive amt is a
// no-op that succeeds.
func (l *Ledger) Lock(asset string, amt decimal.Decimal) bool {
	if amt.Sign() <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	h := l.at(asset)
	if h.free.Lt(amt) {
		return false
	}
	h.free = h.free.Sub(amt)
	h.locked = h.locked.Add(amt)
	return true
}

// Unlock returns up to amt of asset from locked to free (e.g. the unfilled
// remainder of a cancelled order). It clamps to the available locked amount.
func (l *Ledger) Unlock(asset string, amt decimal.Decimal) {
	if amt.Sign() <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	h := l.at(asset)
	if amt.Gt(h.locked) {
		amt = h.locked
	}
	h.locked = h.locked.Sub(amt)
	h.free = h.free.Add(amt)
}

// SettleFill applies one fill. base/quote are the instrument's assets; qty is
// the filled base amount, fillPrice the execution price. reservedPrice is the
// price funds were locked at when the order rested (a LIMIT order): the
// reservation for the filled qty is released from locked, then the ACTUAL cost
// is charged from free — so a LIMIT buy that fills below its limit refunds the
// price improvement to free, and a fully-filled order leaves nothing locked.
// Pass reservedPrice = 0 for a MARKET order (never locked; debited from free).
// A buy debits quote and credits base; a sell debits base and credits quote.
func (l *Ledger) SettleFill(isBuy bool, base, quote string, qty, fillPrice, reservedPrice decimal.Decimal) {
	l.mu.Lock()
	defer l.mu.Unlock()
	cost := qty.Mul(fillPrice)
	if reservedPrice.Sign() <= 0 {
		// MARKET order: never locked, and not affordability-checked at placement
		// (the price was unknown). Settle only up to available free balance,
		// crediting the counter-asset in PROPORTION to what was actually paid, so
		// an underfunded market order can never mint the bought asset. A funded
		// order (free >= cost) behaves exactly as a full fill.
		if isBuy {
			q := l.at(quote)
			pay := cost
			if pay.Gt(q.free) {
				pay = q.free
			}
			q.free = q.free.Sub(pay)
			credited := qty
			if pay.Lt(cost) && cost.Sign() > 0 {
				credited = qty.Mul(pay).Div(cost) // == pay / fillPrice
			}
			l.at(base).free = l.at(base).free.Add(credited)
		} else {
			b := l.at(base)
			sell := qty
			if sell.Gt(b.free) {
				sell = b.free
			}
			b.free = b.free.Sub(sell)
			l.at(quote).free = l.at(quote).free.Add(sell.Mul(fillPrice))
		}
		return
	}
	// LIMIT order: release the reservation for the filled qty, then charge the
	// ACTUAL cost from free (refunding any price improvement). lock==release so
	// the clamps in release/debitFree never engage and value is conserved.
	if isBuy {
		l.release(quote, qty.Mul(reservedPrice))
		l.debitFree(quote, cost)
		l.at(base).free = l.at(base).free.Add(qty)
	} else {
		l.release(base, qty)
		l.debitFree(base, qty)
		l.at(quote).free = l.at(quote).free.Add(cost)
	}
}

// release moves up to amt of asset from locked back to free (clamped).
func (l *Ledger) release(asset string, amt decimal.Decimal) {
	h := l.at(asset)
	if amt.Gt(h.locked) {
		amt = h.locked
	}
	h.locked = h.locked.Sub(amt)
	h.free = h.free.Add(amt)
}

// debitFree removes amt of asset from free, clamped so it never goes negative.
func (l *Ledger) debitFree(asset string, amt decimal.Decimal) {
	h := l.at(asset)
	if amt.Gt(h.free) {
		amt = h.free
	}
	h.free = h.free.Sub(amt)
}

// Get returns the balance for one asset (zero if untracked).
func (l *Ledger) Get(asset string) Balance {
	l.mu.Lock()
	defer l.mu.Unlock()
	h := l.at(asset)
	return Balance{Asset: asset, Free: h.free, Locked: h.locked}
}

// Snapshot returns all balances, sorted by asset.
func (l *Ledger) Snapshot() []Balance {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Balance, 0, len(l.bal))
	for asset, h := range l.bal {
		out = append(out, Balance{Asset: asset, Free: h.free, Locked: h.locked})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Asset < out[j].Asset })
	return out
}
