package coinbase

import (
	"sync"

	"github.com/gorilla/websocket"
)

// sendBuffer is the per-connection outbound queue depth. A client that cannot
// keep up fills this buffer; once full it is dropped (its connection closed)
// rather than blocking the engine's matching goroutine — the book "trade" and
// "cancel" hooks run under the book mutex, so they must never block on a slow
// socket.
const sendBuffer = 256

// WS channel names (the Advanced Trade subscribe-message channel values).
const (
	chanLevel2        = "level2"
	chanMarketTrades  = "market_trades"
	chanUser          = "user"
	chanSubscriptions = "subscriptions"
)

// wsConn is one upgraded websocket connection with a single writer goroutine.
// All sends go through the buffered out channel; only writePump touches the
// underlying socket for data frames (gorilla forbids concurrent writers).
//
// Unlike the Binance edge (URL-stream based), Coinbase subscriptions are
// message-driven: the client sends subscribe/unsubscribe frames after the
// upgrade. All subscription state lives in the Broadcaster's maps (market by
// channel→product→conns, plus a flat user set), guarded by Broadcaster.mu;
// the read pump mutates it through the locked subscribe/unsubscribe API and
// the book hooks/ticker read it under the same lock. User-channel delivery is
// gated solely by membership in the broadcaster's user set (added only after
// authWS passes).
type wsConn struct {
	ws       *websocket.Conn
	out      chan []byte
	closeOne sync.Once
	closed   chan struct{}

	// seq is this connection's monotonic sequence_num counter (per-conn scope,
	// matching how a real Advanced Trade socket numbers frames per connection).
	mu  sync.Mutex
	seq uint64
}

func newWSConn(ws *websocket.Conn) *wsConn {
	return &wsConn{
		ws:     ws,
		out:    make(chan []byte, sendBuffer),
		closed: make(chan struct{}),
	}
}

// nextSeq returns the next per-connection monotonic sequence number.
func (c *wsConn) nextSeq() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seq++
	return c.seq
}

// trySend enqueues a pre-marshalled frame without blocking. It reports false if
// the connection is closed or its buffer is full (slow consumer); the caller
// (the broadcaster) then drops the connection. This is the non-blocking path
// the book hooks rely on.
func (c *wsConn) trySend(b []byte) bool {
	select {
	case <-c.closed:
		return false
	default:
	}
	select {
	case c.out <- b:
		return true
	default:
		return false
	}
}

// close idempotently signals the write pump to stop. The actual socket Close is
// performed by the goroutines once they observe c.closed.
func (c *wsConn) close() {
	c.closeOne.Do(func() { close(c.closed) })
}

// Broadcaster is a concurrency-safe hub fanning out level2, market_trades, and
// user frames to subscribed websocket connections. level2 and market_trades
// are keyed by channel name -> product_id -> conns; the user channel is a flat
// set of authenticated conns (single account, so every order update goes to
// all of them).
type Broadcaster struct {
	mu sync.RWMutex
	// market[channel][productID] -> set of conns
	market map[string]map[string]map[*wsConn]struct{}
	user   map[*wsConn]struct{}
}

// NewBroadcaster returns an empty hub.
func NewBroadcaster() *Broadcaster {
	return &Broadcaster{
		market: map[string]map[string]map[*wsConn]struct{}{
			chanLevel2:       {},
			chanMarketTrades: {},
		},
		user: make(map[*wsConn]struct{}),
	}
}

// subscribeMarket registers c as a subscriber of (channel, product). channel
// must be chanLevel2 or chanMarketTrades.
func (b *Broadcaster) subscribeMarket(c *wsConn, channel, product string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	byProduct := b.market[channel]
	if byProduct == nil {
		byProduct = make(map[string]map[*wsConn]struct{})
		b.market[channel] = byProduct
	}
	set := byProduct[product]
	if set == nil {
		set = make(map[*wsConn]struct{})
		byProduct[product] = set
	}
	set[c] = struct{}{}
}

// unsubscribeMarket removes c from (channel, product).
func (b *Broadcaster) unsubscribeMarket(c *wsConn, channel, product string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	byProduct := b.market[channel]
	if byProduct == nil {
		return
	}
	if set := byProduct[product]; set != nil {
		delete(set, c)
		if len(set) == 0 {
			delete(byProduct, product)
		}
	}
}

// subscribeUser registers c as a user-channel subscriber (must already be
// authenticated).
func (b *Broadcaster) subscribeUser(c *wsConn) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.user[c] = struct{}{}
}

// unsubscribeUser removes c from the user channel.
func (b *Broadcaster) unsubscribeUser(c *wsConn) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.user, c)
}

// remove unsubscribes c from every channel/product and the user set. Safe to
// call repeatedly (on disconnect).
func (b *Broadcaster) remove(c *wsConn) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, byProduct := range b.market {
		for product, set := range byProduct {
			delete(set, c)
			if len(set) == 0 {
				delete(byProduct, product)
			}
		}
	}
	delete(b.user, c)
}

// publishMarket sends a per-connection frame (built by frameFn, so the caller
// can stamp a per-conn sequence number) to every subscriber of (channel,
// product). Slow consumers (full buffer) are closed and removed.
func (b *Broadcaster) publishMarket(channel, product string, frameFn func(*wsConn) []byte) {
	b.mu.RLock()
	var dead []*wsConn
	if byProduct := b.market[channel]; byProduct != nil {
		for c := range byProduct[product] {
			frame := frameFn(c)
			if frame == nil {
				continue
			}
			if !c.trySend(frame) {
				dead = append(dead, c)
			}
		}
	}
	b.mu.RUnlock()
	b.dropAll(dead)
}

// publishUser sends a per-connection frame to every authenticated user-channel
// subscriber.
func (b *Broadcaster) publishUser(frameFn func(*wsConn) []byte) {
	b.mu.RLock()
	var dead []*wsConn
	for c := range b.user {
		frame := frameFn(c)
		if frame == nil {
			continue
		}
		if !c.trySend(frame) {
			dead = append(dead, c)
		}
	}
	b.mu.RUnlock()
	b.dropAll(dead)
}

func (b *Broadcaster) dropAll(dead []*wsConn) {
	for _, c := range dead {
		c.close() // write pump observes c.closed, removes itself and closes socket
	}
}

// hasMarketSubscribers reports whether any connection is subscribed to
// (channel, product). The level2 ticker and trade hook use this to skip work
// for unwatched products.
func (b *Broadcaster) hasMarketSubscribers(channel, product string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	byProduct := b.market[channel]
	return byProduct != nil && len(byProduct[product]) > 0
}

// hasUserSubscribers reports whether any authenticated user subscriber exists.
func (b *Broadcaster) hasUserSubscribers() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.user) > 0
}

// marketProducts returns a snapshot of the product IDs currently subscribed on
// channel (used by the level2 ticker to iterate watched products).
func (b *Broadcaster) marketProducts(channel string) []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	byProduct := b.market[channel]
	out := make([]string, 0, len(byProduct))
	for product := range byProduct {
		out = append(out, product)
	}
	return out
}
