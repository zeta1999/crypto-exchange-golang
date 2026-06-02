package binance

import (
	"sync"

	"github.com/gorilla/websocket"
)

// sendBuffer is the per-connection outbound queue depth. A client that cannot
// keep up fills this buffer; once full it is dropped (its connection closed)
// rather than blocking the engine's matching goroutine — the book "trade" hook
// runs under the book mutex, so it must never block on a slow socket.
const sendBuffer = 256

// wsConn is one upgraded websocket connection with a single writer goroutine.
// All sends go through the buffered out channel; only writePump touches the
// underlying socket for data frames (gorilla forbids concurrent writers).
type wsConn struct {
	ws       *websocket.Conn
	out      chan []byte
	closeOne sync.Once
	closed   chan struct{}
	// combined is true for /stream subscribers (frames wrapped as
	// {"stream":..,"data":..}); false for raw /ws/<stream> subscribers.
	combined bool
}

func newWSConn(ws *websocket.Conn) *wsConn {
	return &wsConn{
		ws:     ws,
		out:    make(chan []byte, sendBuffer),
		closed: make(chan struct{}),
	}
}

// trySend enqueues a pre-marshalled frame without blocking. It reports false if
// the connection is closed or its buffer is full (slow consumer); the caller
// (the broadcaster) then drops the connection. This is the non-blocking path
// the book hook relies on.
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

// Broadcaster is a concurrency-safe hub fanning out market-stream and user-data
// frames to subscribed websocket connections. Market subscribers are keyed by
// Binance stream name (e.g. "btcusdt@trade"); user-data subscribers form a flat
// set (single account, so every executionReport goes to all of them).
type Broadcaster struct {
	mu       sync.RWMutex
	market   map[string]map[*wsConn]struct{} // stream name -> conns
	userData map[*wsConn]struct{}
}

// NewBroadcaster returns an empty hub.
func NewBroadcaster() *Broadcaster {
	return &Broadcaster{
		market:   make(map[string]map[*wsConn]struct{}),
		userData: make(map[*wsConn]struct{}),
	}
}

// subscribeMarket registers c as a subscriber of each stream name.
func (b *Broadcaster) subscribeMarket(c *wsConn, streams []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, s := range streams {
		set := b.market[s]
		if set == nil {
			set = make(map[*wsConn]struct{})
			b.market[s] = set
		}
		set[c] = struct{}{}
	}
}

// subscribeUserData registers c as a user-data stream subscriber.
func (b *Broadcaster) subscribeUserData(c *wsConn) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.userData[c] = struct{}{}
}

// remove unsubscribes c from every stream and the user-data set. Safe to call
// repeatedly (on disconnect).
func (b *Broadcaster) remove(c *wsConn) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for name, set := range b.market {
		delete(set, c)
		if len(set) == 0 {
			delete(b.market, name)
		}
	}
	delete(b.userData, c)
}

// publishMarketFunc sends a per-connection frame (built by frameFn, which lets
// the caller pick the raw vs combined shape per conn) to every subscriber of
// stream. Slow consumers (full buffer) are closed and removed.
func (b *Broadcaster) publishMarketFunc(stream string, frameFn func(*wsConn) []byte) {
	b.mu.RLock()
	var dead []*wsConn
	for c := range b.market[stream] {
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

// publishUserData sends frame to every user-data subscriber.
func (b *Broadcaster) publishUserData(frame []byte) {
	b.mu.RLock()
	var dead []*wsConn
	for c := range b.userData {
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

// hasMarketSubscribers reports whether any connection is subscribed to stream.
// The depth ticker uses this to skip snapshotting unwatched streams.
func (b *Broadcaster) hasMarketSubscribers(stream string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.market[stream]) > 0
}

// marketStreams returns a snapshot of currently-subscribed market stream names.
func (b *Broadcaster) marketStreams() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]string, 0, len(b.market))
	for name := range b.market {
		out = append(out, name)
	}
	return out
}
