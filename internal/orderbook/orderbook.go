package orderbook

import (
	"errors"
	"sort"
	"sync"
	"time"
)

// Side enumerates supported order directions.
type Side string

const (
	SideBuy  Side = "buy"
	SideSell Side = "sell"
)

// Order represents a limit or market order request.
type Order struct {
	ID         string
	Instrument string
	Price      float64
	Volume     float64
	Side       Side
	IsMarket   bool
	Metadata   map[string]string
}

// Trade contains execution details emitted when orders match.
type Trade struct {
	BuyOrderID  string
	SellOrderID string
	Instrument  string
	Price       float64
	Volume      float64
	ExecutedAt  time.Time
}

// Snapshot aggregates the visible state of the book for GUIs or monitoring.
type Snapshot struct {
	Instrument string
	Bids       []Level
	Asks       []Level
	LastTrade  *Trade
	BestBid    float64
	BestAsk    float64
}

// Level represents liquidity available at a specific price.
type Level struct {
	Price  float64
	Volume float64
}

var (
	ErrUnknownInstrument = errors.New("unknown instrument")
	ErrOrderNotFound     = errors.New("order not found")
)

// Hook is a callback triggered after key operations (fills, cancels, triggers).
type Hook func(evt string, data interface{})

type orderQueue struct {
	orders []*Order
}

type instrumentBook struct {
	mu        sync.RWMutex
	bids      orderQueue
	asks      orderQueue
	lastTrade *Trade
}

// OrderBook manages locks per instrument and coordinates operations.
type OrderBook struct {
	mu          sync.RWMutex
	instruments map[string]*instrumentBook
	hooks       []Hook
}

// New creates an order book supporting the provided instruments.
func New(instruments []string) *OrderBook {
	inst := make(map[string]*instrumentBook, len(instruments))
	for _, symbol := range instruments {
		inst[symbol] = &instrumentBook{}
	}
	return &OrderBook{instruments: inst}
}

// RegisterHook adds a callback triggered for every execution or trigger.
func (b *OrderBook) RegisterHook(h Hook) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.hooks = append(b.hooks, h)
}

func (b *OrderBook) fire(evt string, data interface{}) {
	b.mu.RLock()
	hooks := append([]Hook(nil), b.hooks...)
	b.mu.RUnlock()
	for _, h := range hooks {
		h(evt, data)
	}
}

// AddLimitOrder inserts a limit order and attempts immediate matches.
func (b *OrderBook) AddLimitOrder(ord *Order) ([]*Trade, error) {
	book, ok := b.instruments[ord.Instrument]
	if !ok {
		return nil, ErrUnknownInstrument
	}
	book.mu.Lock()
	defer book.mu.Unlock()

	if ord.Volume == 0 {
		// Zero-volume orders act as triggers.
		b.fire("trigger", ord)
		return nil, nil
	}

	trades := book.matchLocked(ord)
	if ord.Volume > 0 && !ord.IsMarket {
		book.enqueueLocked(ord)
	}
	for _, t := range trades {
		b.fire("trade", t)
	}
	return trades, nil
}

// ExecuteMarketOrder matches a market order without resting liquidity.
func (b *OrderBook) ExecuteMarketOrder(ord *Order) ([]*Trade, error) {
	ord.IsMarket = true
	return b.AddLimitOrder(ord)
}

// CancelOrder removes a resting order by identifier if present.
func (b *OrderBook) CancelOrder(symbol, orderID string) (*Order, error) {
	book, ok := b.instruments[symbol]
	if !ok {
		return nil, ErrUnknownInstrument
	}
	book.mu.Lock()
	defer book.mu.Unlock()
	if removed, ok := removeOrder(&book.bids.orders, orderID); ok {
		b.fire("cancel", removed)
		return removed, nil
	}
	if removed, ok := removeOrder(&book.asks.orders, orderID); ok {
		b.fire("cancel", removed)
		return removed, nil
	}
	return nil, ErrOrderNotFound
}

// Snapshot returns a copy of the current book state for the instrument.
func (b *OrderBook) Snapshot(symbol string) (*Snapshot, error) {
	book, ok := b.instruments[symbol]
	if !ok {
		return nil, ErrUnknownInstrument
	}
	book.mu.RLock()
	defer book.mu.RUnlock()

	bids := aggregateLevels(book.bids.orders, true)
	asks := aggregateLevels(book.asks.orders, false)
	snap := &Snapshot{
		Instrument: symbol,
		Bids:       bids,
		Asks:       asks,
		LastTrade:  book.lastTrade,
	}
	if len(bids) > 0 {
		snap.BestBid = bids[0].Price
	}
	if len(asks) > 0 {
		snap.BestAsk = asks[0].Price
	}
	return snap, nil
}

func (b *instrumentBook) enqueueLocked(ord *Order) {
	if ord.Side == SideBuy {
		b.bids.orders = append(b.bids.orders, ord)
		sort.SliceStable(b.bids.orders, func(i, j int) bool {
			return b.bids.orders[i].Price > b.bids.orders[j].Price
		})
	} else {
		b.asks.orders = append(b.asks.orders, ord)
		sort.SliceStable(b.asks.orders, func(i, j int) bool {
			return b.asks.orders[i].Price < b.asks.orders[j].Price
		})
	}
}

func (b *instrumentBook) matchLocked(incoming *Order) []*Trade {
	var trades []*Trade
	var queue *[]*Order
	if incoming.Side == SideBuy {
		queue = &b.asks.orders
	} else {
		queue = &b.bids.orders
	}

	i := 0
	for incoming.Volume > 0 && i < len(*queue) {
		resting := (*queue)[i]
		if !incoming.IsMarket {
			if incoming.Side == SideBuy && incoming.Price < resting.Price {
				break
			}
			if incoming.Side == SideSell && incoming.Price > resting.Price {
				break
			}
		}
		traded := min(incoming.Volume, resting.Volume)
		incoming.Volume -= traded
		resting.Volume -= traded
		trade := &Trade{
			Instrument: incoming.Instrument,
			Price:      resting.Price,
			Volume:     traded,
			ExecutedAt: time.Now().UTC(),
		}
		if incoming.Side == SideBuy {
			trade.BuyOrderID = incoming.ID
			trade.SellOrderID = resting.ID
		} else {
			trade.SellOrderID = incoming.ID
			trade.BuyOrderID = resting.ID
		}
		trades = append(trades, trade)
		b.lastTrade = trade
		if resting.Volume == 0 {
			i++
		} else {
			(*queue)[i] = resting
			break
		}
	}
	*queue = (*queue)[i:]
	return trades
}

func removeOrder(orders *[]*Order, orderID string) (*Order, bool) {
	queue := *orders
	for i, ord := range queue {
		if ord.ID == orderID {
			removed := ord
			queue = append(queue[:i], queue[i+1:]...)
			*orders = queue
			return removed, true
		}
	}
	return nil, false
}

func aggregateLevels(orders []*Order, desc bool) []Level {
	levels := make(map[float64]float64)
	for _, o := range orders {
		levels[o.Price] += o.Volume
	}
	var prices []float64
	for price := range levels {
		prices = append(prices, price)
	}
	sort.Slice(prices, func(i, j int) bool {
		if desc {
			return prices[i] > prices[j]
		}
		return prices[i] < prices[j]
	})
	result := make([]Level, 0, len(prices))
	for _, price := range prices {
		result = append(result, Level{Price: price, Volume: levels[price]})
	}
	return result
}

func min(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
