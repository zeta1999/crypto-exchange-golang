package binance

import (
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zeta1999/crypto-exchange-golang/internal/orderbook"
	"github.com/zeta1999/crypto-exchange-golang/pkg/decimal"
)

// enginePrefix tags engine order IDs that originate from the Binance edge, so
// the registry's book hooks can recognise its own orders and ignore synthetic
// emulator / tape / toxic liquidity.
const enginePrefix = "binance:"

// Order statuses (Binance order lifecycle subset).
const (
	statusNew             = "NEW"
	statusPartiallyFilled = "PARTIALLY_FILLED"
	statusFilled          = "FILLED"
	statusCanceled        = "CANCELED"
)

// orderRecord is the per-order state the edge tracks; the engine itself keeps
// no per-account order store.
type orderRecord struct {
	OrderID             int64
	ClientOrderID       string
	EngineID            string // e.g. "binance:42"
	EngineSymbol        string // "BTC-USD"
	BinanceSymbol       string // "BTCUSDT"
	Side                string // BUY / SELL
	Type                string // LIMIT / MARKET
	TimeInForce         string // GTC
	Price               decimal.Decimal
	OrigQty             decimal.Decimal
	ExecutedQty         decimal.Decimal
	CummulativeQuoteQty decimal.Decimal
	Status              string
	Time                int64 // creation, ms
	UpdateTime          int64 // last update, ms
}

// Registry is the edge's order store. It assigns monotonic Binance order IDs,
// maps them to engine order IDs, and keeps per-order fill/cancel state up to
// date via the order book hooks. It is safe for concurrent use.
type Registry struct {
	mu        sync.Mutex
	seq       atomic.Int64
	byOrderID map[int64]*orderRecord
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
		byOrderID: make(map[int64]*orderRecord),
		byEngine:  make(map[string]*orderRecord),
		byClient:  make(map[string]*orderRecord),
		now:       now,
	}
}

// nextID returns the next monotonic Binance order ID.
func (r *Registry) nextID() int64 { return r.seq.Add(1) }

// EngineID builds the engine order ID for a Binance order ID.
func EngineID(orderID int64) string {
	return enginePrefix + strconv.FormatInt(orderID, 10)
}

// Record allocates an order ID + engine ID and stores a NEW record. If
// clientOrderID is empty, one is generated. It returns the stored record (a
// pointer into the registry; callers must not mutate it outside registry
// methods).
func (r *Registry) Record(binanceSym, engineSym, side, typ, tif string, price, qty decimal.Decimal, clientOrderID string) *orderRecord {
	orderID := r.nextID()
	engineID := EngineID(orderID)
	if clientOrderID == "" {
		clientOrderID = "x-emu-" + strconv.FormatInt(orderID, 10)
	}
	nowMs := r.now().UnixMilli()
	rec := &orderRecord{
		OrderID:       orderID,
		ClientOrderID: clientOrderID,
		EngineID:      engineID,
		EngineSymbol:  engineSym,
		BinanceSymbol: binanceSym,
		Side:          side,
		Type:          typ,
		TimeInForce:   tif,
		Price:         price,
		OrigQty:       qty,
		Status:        statusNew,
		Time:          nowMs,
		UpdateTime:    nowMs,
	}
	r.mu.Lock()
	r.byOrderID[orderID] = rec
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
	rec.ExecutedQty = rec.ExecutedQty.Add(qty)
	rec.CummulativeQuoteQty = rec.CummulativeQuoteQty.Add(qty.Mul(price))
	rec.UpdateTime = nowMs
	if rec.Status == statusCanceled {
		return
	}
	if rec.ExecutedQty.Cmp(rec.OrigQty) >= 0 {
		rec.Status = statusFilled
	} else if rec.ExecutedQty.Sign() > 0 {
		rec.Status = statusPartiallyFilled
	}
}

// OnTrade is the order-book "trade" hook. It updates whichever side of the
// trade belongs to a Binance-edge order; synthetic/tape/toxic order IDs (which
// lack the "binance:" prefix) are ignored.
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

// OnCancel is the order-book "cancel" hook. It marks a Binance-edge order
// CANCELED.
func (r *Registry) OnCancel(o *orderbook.Order) {
	if o == nil || !strings.HasPrefix(o.ID, enginePrefix) {
		return
	}
	nowMs := r.now().UnixMilli()
	r.mu.Lock()
	defer r.mu.Unlock()
	if rec := r.byEngine[o.ID]; rec != nil && rec.Status != statusFilled {
		rec.Status = statusCanceled
		rec.UpdateTime = nowMs
	}
}

// MarkCanceled flags a record CANCELED by order ID (used by the cancel handler
// after the engine confirms removal, since a fully-filled order is no longer in
// the book and the hook would not fire).
func (r *Registry) MarkCanceled(orderID int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if rec := r.byOrderID[orderID]; rec != nil && rec.Status != statusFilled {
		rec.Status = statusCanceled
		rec.UpdateTime = r.now().UnixMilli()
	}
}

// Get returns the record for a Binance order ID.
func (r *Registry) Get(orderID int64) (*orderRecord, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.byOrderID[orderID]
	return rec, ok
}

// GetByClientOrderID returns the record for a client order ID.
func (r *Registry) GetByClientOrderID(clientOrderID string) (*orderRecord, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.byClient[clientOrderID]
	return rec, ok
}

// OpenOrders returns NEW/PARTIALLY_FILLED orders, optionally filtered to a
// single engine symbol (empty engineSymbol => all). Results are a stable copy.
func (r *Registry) OpenOrders(engineSymbol string) []orderRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []orderRecord
	for _, rec := range r.byOrderID {
		if rec.Status != statusNew && rec.Status != statusPartiallyFilled {
			continue
		}
		if engineSymbol != "" && rec.EngineSymbol != engineSymbol {
			continue
		}
		out = append(out, *rec)
	}
	return out
}

// snapshot returns a value copy of the record under lock.
func (r *Registry) snapshot(orderID int64) (orderRecord, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.byOrderID[orderID]
	if !ok {
		return orderRecord{}, false
	}
	return *rec, true
}
