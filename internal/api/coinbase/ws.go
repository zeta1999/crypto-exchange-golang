package coinbase

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"time"

	"github.com/gorilla/websocket"

	"github.com/zeta1999/crypto-exchange-golang/internal/orderbook"
)

// WS tuning. level2Interval is the partial-book refresh cadence; pong/ping
// deadlines keep half-open sockets from lingering.
const (
	level2Interval = 250 * time.Millisecond
	pongWait       = 60 * time.Second
	pingPeriod     = (pongWait * 9) / 10
	writeWait      = 10 * time.Second
)

// level2Levels caps the number of book levels carried in a level2 frame.
const level2Levels = 50

// readLimit bounds an inbound subscribe message; Coinbase product_ids lists are
// small, but allow generous room for a multi-product subscription.
const readLimit = 16 * 1024

// upgrader is shared; the emulator does not enforce Origin (it is a local test
// harness, not a browser-facing service).
var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin:     func(*http.Request) bool { return true },
}

// --- inbound (client -> server) ---

// subscribeMsg is the client subscribe/unsubscribe frame. The user channel may
// additionally carry HMAC credentials (CB-ACCESS-style fields) or a jwt/token.
type subscribeMsg struct {
	Type       string   `json:"type"` // "subscribe" | "unsubscribe"
	Channel    string   `json:"channel"`
	ProductIDs []string `json:"product_ids"`
	// User-channel auth (emulator subset): any one of these may carry the key.
	APIKey string `json:"api_key"`
	JWT    string `json:"jwt"`
	Token  string `json:"token"`
}

// --- outbound (server -> client) ---

// wsEnvelope is the Advanced Trade frame envelope.
type wsEnvelope struct {
	Channel     string        `json:"channel"`
	Timestamp   string        `json:"timestamp"`
	SequenceNum uint64        `json:"sequence_num"`
	Events      []interface{} `json:"events"`
}

// errorFrame is a non-fatal error reported to the client (e.g. unauthenticated
// user subscribe, unknown channel/product). The connection stays open.
type errorFrame struct {
	Type      string `json:"type"`    // "error"
	Channel   string `json:"channel"` // echo of the offending channel
	Message   string `json:"message"`
	Timestamp string `json:"timestamp"`
}

// subscriptionsEvent is the body of a "subscriptions" ack frame: a map of
// channel -> the product IDs currently subscribed on this connection.
type subscriptionsEvent struct {
	Subscriptions map[string][]string `json:"subscriptions"`
}

// l2Update is one price-level change in a level2 frame.
type l2Update struct {
	Side        string `json:"side"` // "bid" | "offer"
	EventTime   string `json:"event_time"`
	PriceLevel  string `json:"price_level"`
	NewQuantity string `json:"new_quantity"`
}

// l2Event is one product's level2 payload.
type l2Event struct {
	Type      string     `json:"type"` // "snapshot" | "update"
	ProductID string     `json:"product_id"`
	Updates   []l2Update `json:"updates"`
}

// mtTrade is one trade in a market_trades frame.
type mtTrade struct {
	TradeID   string `json:"trade_id"`
	ProductID string `json:"product_id"`
	Price     string `json:"price"`
	Size      string `json:"size"`
	Side      string `json:"side"` // "BUY" | "SELL" (aggressor)
	Time      string `json:"time"`
}

// mtEvent is the market_trades payload.
type mtEvent struct {
	Type   string    `json:"type"`
	Trades []mtTrade `json:"trades"`
}

// userOrder is one order's state in a user-channel frame.
type userOrder struct {
	OrderID            string `json:"order_id"`
	ClientOrderID      string `json:"client_order_id"`
	ProductID          string `json:"product_id"`
	Side               string `json:"side"`
	OrderType          string `json:"order_type"`
	Status             string `json:"status"`
	CumulativeQuantity string `json:"cumulative_quantity"`
	AvgPrice           string `json:"avg_price"`
	TotalFees          string `json:"total_fees"`
}

// userEvent is the user-channel payload.
type userEvent struct {
	Type   string      `json:"type"`
	Orders []userOrder `json:"orders"`
}

// --- HTTP handler ---

// handleWS upgrades GET /ws and runs the message-driven subscription protocol.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return // Upgrade already wrote the error response.
	}
	c := newWSConn(ws)
	s.runConn(c)
}

// runConn starts the write pump and runs the read pump (which processes inbound
// subscribe/unsubscribe frames and control frames), then unsubscribes on exit.
func (s *Server) runConn(c *wsConn) {
	go s.writePump(c)
	s.readPump(c) // blocks until the conn closes
	c.close()
	s.broadcaster.remove(c)
}

// writePump is the SOLE writer for the socket's data + ping frames.
func (s *Server) writePump(c *wsConn) {
	ticker := time.NewTicker(pingPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-c.closed:
			_ = c.ws.WriteControl(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
				time.Now().Add(writeWait))
			_ = c.ws.Close()
			return
		case msg := <-c.out:
			_ = c.ws.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.ws.WriteMessage(websocket.TextMessage, msg); err != nil {
				c.close()
				_ = c.ws.Close()
				return
			}
		case <-ticker.C:
			_ = c.ws.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(writeWait)); err != nil {
				c.close()
				_ = c.ws.Close()
				return
			}
		}
	}
}

// readPump reads inbound frames: subscribe/unsubscribe JSON messages plus
// control frames (pong/close). It is the only mutator of the conn's
// subscription state. It never writes to the socket (it enqueues frames via
// trySend, which the write pump drains).
func (s *Server) readPump(c *wsConn) {
	c.ws.SetReadLimit(readLimit)
	_ = c.ws.SetReadDeadline(time.Now().Add(pongWait))
	c.ws.SetPongHandler(func(string) error {
		return c.ws.SetReadDeadline(time.Now().Add(pongWait))
	})
	for {
		select {
		case <-c.closed:
			return
		default:
		}
		_, data, err := c.ws.ReadMessage()
		if err != nil {
			return
		}
		var msg subscribeMsg
		if err := json.Unmarshal(data, &msg); err != nil {
			c.trySendErr(s, "", "invalid message JSON")
			continue
		}
		s.handleSubscribe(c, &msg)
	}
}

// handleSubscribe applies one subscribe/unsubscribe message to the connection's
// subscription state and acks it. Errors are reported as error frames; the
// connection is never dropped for a bad message.
func (s *Server) handleSubscribe(c *wsConn, msg *subscribeMsg) {
	sub := msg.Type == "subscribe"
	unsub := msg.Type == "unsubscribe"
	if !sub && !unsub {
		c.trySendErr(s, msg.Channel, "unknown message type: "+msg.Type)
		return
	}

	switch msg.Channel {
	case chanLevel2, chanMarketTrades:
		s.handleMarketSub(c, msg, sub)
	case chanUser:
		s.handleUserSub(c, msg, sub)
	default:
		c.trySendErr(s, msg.Channel, "unknown channel: "+msg.Channel)
	}
}

// handleMarketSub processes a level2 / market_trades subscribe or unsubscribe.
func (s *Server) handleMarketSub(c *wsConn, msg *subscribeMsg, sub bool) {
	if len(msg.ProductIDs) == 0 {
		c.trySendErr(s, msg.Channel, "product_ids is required")
		return
	}
	var accepted []string
	for _, product := range msg.ProductIDs {
		if _, ok := s.products.Resolve(product); !ok {
			c.trySendErr(s, msg.Channel, "unknown product_id: "+product)
			continue
		}
		if sub {
			s.broadcaster.subscribeMarket(c, msg.Channel, product)
			accepted = append(accepted, product)
		} else {
			s.broadcaster.unsubscribeMarket(c, msg.Channel, product)
			accepted = append(accepted, product)
		}
	}
	if len(accepted) == 0 {
		return // every product was invalid; error frames already sent.
	}
	s.sendAck(c, msg.Channel, accepted)
	// On a fresh level2 subscribe, push an immediate snapshot so the client
	// does not wait up to level2Interval for its first book frame.
	if sub && msg.Channel == chanLevel2 {
		for _, product := range accepted {
			s.pushLevel2(c, product, true)
		}
	}
}

// handleUserSub processes a user-channel subscribe or unsubscribe. Subscribing
// requires valid credentials; an unauthenticated subscribe is rejected with an
// error frame (the connection stays open).
func (s *Server) handleUserSub(c *wsConn, msg *subscribeMsg, sub bool) {
	if !sub {
		s.broadcaster.unsubscribeUser(c)
		s.sendAck(c, chanUser, nil)
		return
	}
	if !s.authWS(msg) {
		c.trySendErr(s, chanUser, "user channel requires authentication")
		return
	}
	// Authentication gate is enforced here; membership in the broadcaster's
	// user set is the source of truth for delivery.
	s.broadcaster.subscribeUser(c)
	s.sendAck(c, chanUser, nil)
}

// authWS validates the credentials in a user subscribe message. The emulator
// subset accepts the configured API key in any of api_key / jwt / token (a
// faithful-but-simple substitute for the production ES256 JWT, which would
// require the client's EC public key the emulator does not provision). If no
// API key is configured (empty), authentication is disabled and any user
// subscribe is accepted.
func (s *Server) authWS(msg *subscribeMsg) bool {
	// Production scheme: the subscribe carries an ES256 JWT. If a verifier is
	// configured, accept the user channel iff the jwt verifies.
	if msg.JWT != "" {
		if ok, err := s.auth.VerifyJWT(msg.JWT); ok {
			return err == nil
		}
	}
	want := s.auth.APIKeyString()
	if want == "" {
		return true
	}
	for _, got := range []string{msg.APIKey, msg.JWT, msg.Token} {
		if got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1 {
			return true
		}
	}
	return false
}

// sendAck sends a subscriptions ack frame echoing the products subscribed on
// channel (products may be nil for the user channel).
func (s *Server) sendAck(c *wsConn, channel string, products []string) {
	body := subscriptionsEvent{Subscriptions: map[string][]string{channel: products}}
	frame := s.frame(c, chanSubscriptions, body)
	if frame != nil {
		_ = c.trySend(frame)
	}
}

// trySendErr enqueues an error frame to the connection (non-fatal).
func (c *wsConn) trySendErr(s *Server, channel, message string) {
	ef := errorFrame{
		Type:      "error",
		Channel:   channel,
		Message:   message,
		Timestamp: s.now().UTC().Format(time.RFC3339),
	}
	if b, err := json.Marshal(ef); err == nil {
		_ = c.trySend(b)
	}
}

// frame builds a wire envelope for one connection, stamping a per-conn
// sequence number and the current timestamp.
func (s *Server) frame(c *wsConn, channel string, event interface{}) []byte {
	env := wsEnvelope{
		Channel:     channel,
		Timestamp:   s.now().UTC().Format(time.RFC3339),
		SequenceNum: c.nextSeq(),
		Events:      []interface{}{event},
	}
	b, err := json.Marshal(env)
	if err != nil {
		return nil
	}
	return b
}

// --- level2 ticker ---

// Start runs the periodic level2 refresh loop until ctx is cancelled. It is
// safe to call once; ListenAndServe starts it automatically. Each tick pushes a
// fresh top-N book "update" frame for every subscribed product.
//
// For this subset, a level2 "update" re-sends a bounded top-N book as a partial
// refresh rather than computing true incremental diffs — documented as a
// partial-book refresh.
func (s *Server) Start(ctx context.Context) {
	if s.broadcaster == nil {
		return
	}
	ticker := time.NewTicker(level2Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.pushLevel2Tick()
		}
	}
}

// pushLevel2Tick broadcasts a fresh level2 update for every subscribed product.
func (s *Server) pushLevel2Tick() {
	for _, product := range s.broadcaster.marketProducts(chanLevel2) {
		s.broadcastLevel2(product)
	}
}

// broadcastLevel2 snapshots the engine book and broadcasts a level2 "update"
// frame to all subscribers of product.
func (s *Server) broadcastLevel2(product string) {
	updates, ok := s.level2Updates(product)
	if !ok {
		return
	}
	ev := l2Event{Type: "update", ProductID: product, Updates: updates}
	s.broadcaster.publishMarket(chanLevel2, product, func(c *wsConn) []byte {
		return s.frame(c, "l2_data", ev)
	})
}

// pushLevel2 sends a single level2 frame to one connection. snapshot selects
// the "snapshot" type (sent on subscribe) vs "update".
func (s *Server) pushLevel2(c *wsConn, product string, snapshot bool) {
	updates, ok := s.level2Updates(product)
	if !ok {
		return
	}
	typ := "update"
	if snapshot {
		typ = "snapshot"
	}
	ev := l2Event{Type: typ, ProductID: product, Updates: updates}
	if frame := s.frame(c, "l2_data", ev); frame != nil {
		_ = c.trySend(frame)
	}
}

// level2Updates builds the top-N bid/offer levels for product as l2 updates.
// Bids are emitted with side "bid", asks with side "offer" (Coinbase wire).
func (s *Server) level2Updates(product string) ([]l2Update, bool) {
	engSym, ok := s.products.Resolve(product)
	if !ok {
		return nil, false
	}
	snap, err := s.engine.Snapshot(engSym)
	if err != nil {
		return nil, false
	}
	eventTime := s.now().UTC().Format(time.RFC3339)
	updates := make([]l2Update, 0, level2Levels*2)
	updates = appendLevels(updates, snap.Bids, "bid", eventTime)
	updates = appendLevels(updates, snap.Asks, "offer", eventTime)
	return updates, true
}

// appendLevels appends up to level2Levels of levels as l2 updates of the given
// side.
func appendLevels(dst []l2Update, levels []orderbook.Level, side, eventTime string) []l2Update {
	n := len(levels)
	if n > level2Levels {
		n = level2Levels
	}
	for i := 0; i < n; i++ {
		dst = append(dst, l2Update{
			Side:        side,
			EventTime:   eventTime,
			PriceLevel:  levels[i].Price.StringPrec(pricePrec),
			NewQuantity: levels[i].Volume.StringPrec(sizePrec),
		})
	}
	return dst
}

// --- book trade hook -> market_trades + user channels ---

// onBookTrade is a book "trade" hook (registered after the registry's hook in
// AttachHooks, so the registry record already reflects this fill when the user
// emission reads it). It broadcasts a market_trades frame for the product and a
// user frame for any Coinbase-edge order touched by the trade.
func (s *Server) onBookTrade(t *orderbook.Trade) {
	if t == nil {
		return
	}
	product := t.Instrument // engine instrument == Coinbase product_id
	s.emitMarketTrade(product, t)
	s.emitUserForTrade(t)
}

// emitMarketTrade broadcasts a market_trades frame for the trade. side is the
// aggressor side: BUY when the taker bought, SELL when the taker sold.
func (s *Server) emitMarketTrade(product string, t *orderbook.Trade) {
	if !s.broadcaster.hasMarketSubscribers(chanMarketTrades, product) {
		return
	}
	side := "BUY"
	if t.TakerSide == orderbook.SideSell {
		side = "SELL"
	}
	tradeTime := s.now().UTC()
	if !t.ExecutedAt.IsZero() {
		tradeTime = t.ExecutedAt.UTC()
	}
	tr := mtTrade{
		TradeID:   s.nextTradeID(),
		ProductID: product,
		Price:     t.Price.StringPrec(pricePrec),
		Size:      t.Volume.StringPrec(sizePrec),
		Side:      side,
		Time:      tradeTime.Format(time.RFC3339),
	}
	ev := mtEvent{Type: "update", Trades: []mtTrade{tr}}
	s.broadcaster.publishMarket(chanMarketTrades, product, func(c *wsConn) []byte {
		return s.frame(c, chanMarketTrades, ev)
	})
}

// emitUserForTrade emits a user-channel order update for whichever side(s) of
// the trade belong to a Coinbase-edge order. The registry record is already
// updated (hook ordering), so cumulative_quantity/status/avg are current.
func (s *Server) emitUserForTrade(t *orderbook.Trade) {
	if !s.broadcaster.hasUserSubscribers() {
		return
	}
	for _, id := range []string{t.BuyOrderID, t.SellOrderID} {
		if rec, ok := s.registry.getByEngine(id); ok {
			s.deliverUserFill(rec)
		}
	}
}

// onBookCancel is a book "cancel" hook (registered after the registry's). It
// emits a user-channel update for a cancelled Coinbase-edge order.
func (s *Server) onBookCancel(o *orderbook.Order) {
	if o == nil || !s.broadcaster.hasUserSubscribers() {
		return
	}
	if rec, ok := s.registry.getByEngine(o.ID); ok {
		s.emitUser(rec)
	}
}

// EmitUserByOrderID emits a user-channel update for a Coinbase order_id. The
// REST create handler calls this to surface the initial OPEN/NEW state and any
// terminal state not produced by a book hook (e.g. a fully-filled market order
// whose cancel hook never fires).
func (s *Server) EmitUserByOrderID(orderID string) {
	if s.broadcaster == nil || !s.broadcaster.hasUserSubscribers() {
		return
	}
	if rec, ok := s.registry.snapshot(orderID); ok {
		s.emitUser(rec)
	}
}

// emitUser broadcasts a user-channel frame for one order record to all
// authenticated user subscribers.
func (s *Server) emitUser(rec orderRecord) {
	totalFees, _ := orderFees(rec, s.effFeeRate())
	uo := userOrder{
		OrderID:            rec.OrderID,
		ClientOrderID:      rec.ClientOrderID,
		ProductID:          rec.ProductID,
		Side:               rec.Side,
		OrderType:          rec.OrderType,
		Status:             rec.Status,
		CumulativeQuantity: rec.FilledSize.StringPrec(sizePrec),
		AvgPrice:           rec.avgFilledPrice().StringPrec(pricePrec),
		TotalFees:          totalFees,
	}
	ev := userEvent{Type: "update", Orders: []userOrder{uo}}
	s.broadcaster.publishUser(func(c *wsConn) []byte {
		return s.frame(c, chanUser, ev)
	})
}
