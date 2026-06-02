package coinbase

import (
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zeta1999/crypto-exchange-golang/internal/orderbook"
	"github.com/zeta1999/crypto-exchange-golang/pkg/decimal"
)

// enginePrefix tags engine order IDs that originate from the Coinbase edge, so
// the registry's book hooks can recognise its own orders and ignore synthetic
// emulator / tape / toxic liquidity (and other edges, e.g. binance:).
const enginePrefix = "coinbase:"

// Order statuses (Coinbase Advanced Trade lifecycle subset).
const (
	statusOpen      = "OPEN"
	statusFilled    = "FILLED"
	statusCancelled = "CANCELLED"
)

// orderRecord is the per-order state the edge tracks; the engine itself keeps
// no per-account order store.
type orderRecord struct {
	OrderID       string // Coinbase order_id, e.g. "emu-42" (UUID-like)
	ClientOrderID string // client_order_id from the request
	EngineID      string // e.g. "coinbase:42"
	ProductID     string // "BTC-USD" (== engine instrument)
	Side          string // BUY / SELL
	OrderType     string // LIMIT / MARKET
	PostOnly      bool
	Price         decimal.Decimal // limit price (zero for market)
	OrigSize      decimal.Decimal // base_size
	FilledSize    decimal.Decimal
	FilledValue   decimal.Decimal // cumulative quote value (sum of size*price)
	Status        string
	CreatedMs     int64
	UpdatedMs     int64
}

// avgFilledPrice returns the volume-weighted average fill price, or zero if
// nothing filled. Caller need not hold the lock (operates on a value copy).
func (rec orderRecord) avgFilledPrice() decimal.Decimal {
	if rec.FilledSize.Sign() <= 0 {
		return decimal.Zero
	}
	return rec.FilledValue.Div(rec.FilledSize)
}

// completionPercent returns 0..100 filled as a decimal.
func (rec orderRecord) completionPercent() decimal.Decimal {
	if rec.OrigSize.Sign() <= 0 {
		return decimal.Zero
	}
	return rec.FilledSize.Mul(decimal.FromInt(100)).Div(rec.OrigSize)
}

// Registry is the edge's order store. It assigns Coinbase order IDs, maps them
// to engine order IDs, and keeps per-order fill/cancel state up to date via the
// order book hooks. It is safe for concurrent use.
type Registry struct {
	mu        sync.Mutex
	seq       atomic.Int64
	byOrderID map[string]*orderRecord
	byEngine  map[string]*orderRecord
	byClient  map[string]*orderRecord
	now       func() time.Time
}

// NewRegistry returns an empty Registry. now may be nil (defaults to time.Now).
func NewRegistry(now func() time.Time) *Registry {
	if now == nil {
		now = time.Now
	}
	return &Registry{
		byOrderID: make(map[string]*orderRecord),
		byEngine:  make(map[string]*orderRecord),
		byClient:  make(map[string]*orderRecord),
		now:       now,
	}
}

// nextID returns the next monotonic sequence number.
func (r *Registry) nextID() int64 { return r.seq.Add(1) }

// EngineID builds the engine order ID for a sequence number.
func EngineID(seq int64) string {
	return enginePrefix + strconv.FormatInt(seq, 10)
}

// orderID builds the Coinbase-facing order_id (UUID-like, deterministic) for a
// sequence number.
func orderID(seq int64) string {
	return "emu-" + strconv.FormatInt(seq, 10)
}

// Record allocates an order ID + engine ID and stores an OPEN record. If
// clientOrderID is empty, one is generated. It returns the stored record (a
// pointer into the registry; callers must not mutate it outside registry
// methods).
func (r *Registry) Record(productID, side, orderType string, postOnly bool, price, size decimal.Decimal, clientOrderID string) *orderRecord {
	seq := r.nextID()
	oid := orderID(seq)
	engineID := EngineID(seq)
	if clientOrderID == "" {
		clientOrderID = "emu-client-" + strconv.FormatInt(seq, 10)
	}
	nowMs := r.now().UnixMilli()
	rec := &orderRecord{
		OrderID:       oid,
		ClientOrderID: clientOrderID,
		EngineID:      engineID,
		ProductID:     productID,
		Side:          side,
		OrderType:     orderType,
		PostOnly:      postOnly,
		Price:         price,
		OrigSize:      size,
		Status:        statusOpen,
		CreatedMs:     nowMs,
		UpdatedMs:     nowMs,
	}
	r.mu.Lock()
	r.byOrderID[oid] = rec
	r.byEngine[engineID] = rec
	r.byClient[clientOrderID] = rec
	r.mu.Unlock()
	return rec
}

// applyFill folds a filled quantity at a price into a record and recomputes its
// status. Caller holds r.mu.
func (rec *orderRecord) applyFill(qty, price decimal.Decimal, nowMs int64) {
	if qty.Sign() <= 0 {
		return
	}
	rec.FilledSize = rec.FilledSize.Add(qty)
	rec.FilledValue = rec.FilledValue.Add(qty.Mul(price))
	rec.UpdatedMs = nowMs
	if rec.Status == statusCancelled {
		return
	}
	if rec.FilledSize.Cmp(rec.OrigSize) >= 0 {
		rec.Status = statusFilled
	}
}

// OnTrade is the order-book "trade" hook. It updates whichever side of the
// trade belongs to a Coinbase-edge order; synthetic/tape/toxic and other-edge
// order IDs (which lack the "coinbase:" prefix) are ignored.
func (r *Registry) OnTrade(t *orderbook.Trade) {
	if t == nil {
		return
	}
	nowMs := r.now().UnixMilli()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, id := range []string{t.BuyOrderID, t.SellOrderID} {
		if !strings.HasPrefix(id, enginePrefix) {
			continue
		}
		if rec := r.byEngine[id]; rec != nil {
			rec.applyFill(t.Volume, t.Price, nowMs)
		}
	}
}

// OnCancel is the order-book "cancel" hook. It marks a Coinbase-edge order
// CANCELLED.
func (r *Registry) OnCancel(o *orderbook.Order) {
	if o == nil || !strings.HasPrefix(o.ID, enginePrefix) {
		return
	}
	nowMs := r.now().UnixMilli()
	r.mu.Lock()
	defer r.mu.Unlock()
	if rec := r.byEngine[o.ID]; rec != nil && rec.Status != statusFilled {
		rec.Status = statusCancelled
		rec.UpdatedMs = nowMs
	}
}

// MarkCancelled flags a record CANCELLED by order ID (used by the cancel
// handler after the engine confirms removal, since a fully-filled order is no
// longer in the book and the hook would not fire).
func (r *Registry) MarkCancelled(oid string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if rec := r.byOrderID[oid]; rec != nil && rec.Status != statusFilled {
		rec.Status = statusCancelled
		rec.UpdatedMs = r.now().UnixMilli()
	}
}

// Get returns the record for a Coinbase order ID.
func (r *Registry) Get(oid string) (*orderRecord, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.byOrderID[oid]
	return rec, ok
}

// OpenOrders returns OPEN orders, optionally filtered to a single product
// (empty productID => all). Results are a stable copy sorted by order ID.
func (r *Registry) OpenOrders(productID string) []orderRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []orderRecord
	for _, rec := range r.byOrderID {
		if rec.Status != statusOpen {
			continue
		}
		if productID != "" && rec.ProductID != productID {
			continue
		}
		out = append(out, *rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OrderID < out[j].OrderID })
	return out
}

// Remove deletes a record from all indexes. Used to roll back an order that was
// recorded before placement when the engine then rejected it.
func (r *Registry) Remove(oid string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.byOrderID[oid]
	if !ok {
		return
	}
	delete(r.byOrderID, oid)
	delete(r.byEngine, rec.EngineID)
	delete(r.byClient, rec.ClientOrderID)
}

// getByEngine returns a value copy of the record for an engine order ID (e.g.
// "coinbase:42"), under lock. Used by the WS user channel to read the
// just-updated record from a book hook. ok is false for non-edge IDs.
func (r *Registry) getByEngine(engineID string) (orderRecord, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.byEngine[engineID]
	if !ok {
		return orderRecord{}, false
	}
	return *rec, true
}

// snapshot returns a value copy of the record under lock.
func (r *Registry) snapshot(oid string) (orderRecord, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.byOrderID[oid]
	if !ok {
		return orderRecord{}, false
	}
	return *rec, true
}
