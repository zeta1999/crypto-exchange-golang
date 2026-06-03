package binance

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/zeta1999/crypto-exchange-golang/internal/orderbook"
	"github.com/zeta1999/crypto-exchange-golang/pkg/decimal"
)

// WS tuning. depthInterval is the partial-book push cadence (Binance's
// @depth20@100ms uses 100ms); pong/ping deadlines keep half-open sockets from
// lingering.
const (
	depthInterval = 100 * time.Millisecond
	pongWait      = 60 * time.Second
	pingPeriod    = (pongWait * 9) / 10
	writeWait     = 10 * time.Second
)

// depthLevels is the number of book levels a @depth20 stream carries.
const depthLevels = 20

// upgrader is shared; the emulator does not enforce Origin (it is a local test
// harness, not a browser-facing service).
var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin:     func(*http.Request) bool { return true },
}

// streamKind classifies a parsed market stream name.
type streamKind int

const (
	kindUnknown streamKind = iota
	kindTrade
	kindDepth     // @depth20: partial-book snapshot push
	kindDepthDiff // @depth: incremental depthUpdate diff stream
)

// parsedStream is a validated market stream: its original wire name, the engine
// instrument it maps to, and its kind.
type parsedStream struct {
	name      string // "btcusdt@trade"
	binSym    string // "btcusdt" (lowercase, as sent)
	engineSym string // "BTC-USD"
	kind      streamKind
}

// parseStreamName validates a single stream name and resolves its symbol. It
// accepts "<sym>@trade", the partial-book "<sym>@depth20[@100ms]", and the
// incremental diff "<sym>@depth[@100ms]". The symbol is matched
// case-insensitively against the configured Binance symbols.
func (s *Server) parseStreamName(name string) (parsedStream, bool) {
	at := strings.IndexByte(name, '@')
	if at <= 0 {
		return parsedStream{}, false
	}
	sym := name[:at]
	suffix := name[at+1:]
	engSym, ok := s.resolveStreamSymbol(sym)
	if !ok {
		return parsedStream{}, false
	}
	ps := parsedStream{name: name, binSym: sym, engineSym: engSym}
	switch {
	case suffix == "trade":
		ps.kind = kindTrade
	case suffix == "depth20" || suffix == "depth20@100ms":
		ps.kind = kindDepth
	case suffix == "depth" || suffix == "depth@100ms":
		ps.kind = kindDepthDiff
	default:
		return parsedStream{}, false
	}
	return ps, true
}

// resolveStreamSymbol maps a stream symbol segment (e.g. "btcusdt") to an engine
// instrument, trying the symbol as sent and upper-cased (Binance stream names
// are lowercase, but the SymbolMap is keyed on the canonical upper form).
func (s *Server) resolveStreamSymbol(sym string) (string, bool) {
	if eng, ok := s.symbols.ToEngine(sym); ok {
		return eng, true
	}
	return s.symbols.ToEngine(strings.ToUpper(sym))
}

// --- listenKey REST lifecycle ---

// handleUserDataStream multiplexes POST/PUT/DELETE on /api/v3/userDataStream.
// All require a valid X-MBX-APIKEY (no signature, matching Binance).
func (s *Server) handleUserDataStream(w http.ResponseWriter, r *http.Request) {
	if err := s.auth.VerifyAPIKey(r); err != nil {
		writeError(w, err)
		return
	}
	switch r.Method {
	case http.MethodPost:
		key := s.listenKeys.create()
		writeJSON(w, map[string]string{"listenKey": key})
	case http.MethodPut:
		_ = r.ParseForm()
		key := userDataStreamKey(r)
		if key == "" || !s.listenKeys.keepalive(key) {
			writeError(w, errMandatoryParam("listenKey"))
			return
		}
		writeJSON(w, struct{}{})
	case http.MethodDelete:
		_ = r.ParseForm()
		key := userDataStreamKey(r)
		if key == "" || !s.listenKeys.remove(key) {
			writeError(w, errMandatoryParam("listenKey"))
			return
		}
		writeJSON(w, struct{}{})
	default:
		writeError(w, errIllegalParam("method"))
	}
}

// userDataStreamKey extracts the listenKey from query or form (Binance accepts
// either for PUT/DELETE).
func userDataStreamKey(r *http.Request) string {
	if v := r.URL.Query().Get("listenKey"); v != "" {
		return v
	}
	return r.PostFormValue("listenKey")
}

// --- HTTP handlers (registered on the mux in New) ---

// handleRawStream serves GET /ws/<segment>. The segment is either a live
// listenKey (=> user-data stream) or a single market stream name.
func (s *Server) handleRawStream(w http.ResponseWriter, r *http.Request) {
	seg := strings.TrimPrefix(r.URL.Path, "/ws/")
	if seg == "" || strings.Contains(seg, "/") {
		http.Error(w, "invalid stream", http.StatusBadRequest)
		return
	}
	// A path segment matching a live listenKey is a user-data stream.
	if s.listenKeys.valid(seg) {
		s.upgradeUserData(w, r)
		return
	}
	ps, ok := s.parseStreamName(seg)
	if !ok {
		http.Error(w, "unknown stream", http.StatusBadRequest)
		return
	}
	s.upgradeMarket(w, r, []parsedStream{ps}, false)
}

// handleCombinedStream serves GET /stream?streams=a/b/c (combined form: each
// data frame is wrapped as {"stream":..,"data":..}).
func (s *Server) handleCombinedStream(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("streams")
	if raw == "" {
		http.Error(w, "missing streams", http.StatusBadRequest)
		return
	}
	names := strings.Split(raw, "/")
	// Bound attacker-controlled work before upgrade (Binance caps at 1024).
	if len(names) > 1024 {
		http.Error(w, "too many streams", http.StatusBadRequest)
		return
	}
	var parsed []parsedStream
	for _, name := range names {
		if name == "" {
			continue
		}
		ps, ok := s.parseStreamName(name)
		if !ok {
			http.Error(w, "unknown stream: "+name, http.StatusBadRequest)
			return
		}
		parsed = append(parsed, ps)
	}
	if len(parsed) == 0 {
		http.Error(w, "missing streams", http.StatusBadRequest)
		return
	}
	s.upgradeMarket(w, r, parsed, true)
}

// upgradeMarket upgrades the connection and registers it as a subscriber of the
// given market streams. combined controls whether frames are wrapped.
func (s *Server) upgradeMarket(w http.ResponseWriter, r *http.Request, streams []parsedStream, combined bool) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return // Upgrade already wrote the error response.
	}
	c := newWSConn(ws)
	// Set combined BEFORE registering the conn: once subscribed, the book hook
	// goroutine may read c.combined in fanout, so it must be fixed beforehand
	// (it is never mutated afterwards).
	c.combined = combined
	names := make([]string, 0, len(streams))
	for _, ps := range streams {
		names = append(names, ps.name)
	}
	s.broadcaster.subscribeMarket(c, names)

	// On a fresh @depth subscription, push an immediate snapshot so the client
	// does not wait up to depthInterval for its first book frame.
	for _, ps := range streams {
		if ps.kind == kindDepth {
			s.pushDepthTo(c, ps)
		}
	}

	s.runConn(c)
}

// upgradeUserData upgrades and registers a user-data (executionReport) stream.
func (s *Server) upgradeUserData(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	c := newWSConn(ws)
	s.broadcaster.subscribeUserData(c)
	s.runConn(c)
}

// runConn starts the per-connection write pump and read pump (the read pump
// handles control frames and detects close), then blocks until the read pump
// returns, finally unsubscribing.
func (s *Server) runConn(c *wsConn) {
	go s.writePump(c)
	s.readPump(c) // blocks until the conn closes
	c.close()
	s.broadcaster.remove(c)
}

// writePump is the SOLE writer for the socket's data + ping frames. It drains
// the buffered out channel and sends periodic pings.
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

// readPump drains inbound frames (clients may send none) so gorilla can process
// control frames (pong/close) and detect disconnect. It does not write.
func (s *Server) readPump(c *wsConn) {
	c.ws.SetReadLimit(512)
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
		if _, _, err := c.ws.ReadMessage(); err != nil {
			return
		}
	}
}

// --- depth ticker ---

// Start runs the periodic @depth20 push loop until ctx is cancelled. It is safe
// to call once; ListenAndServe starts it automatically. Each tick snapshots the
// engine book for every subscribed @depth stream and broadcasts a partial-book
// frame.
func (s *Server) Start(ctx context.Context) {
	if s.broadcaster == nil {
		return
	}
	ticker := time.NewTicker(depthInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.pushDepthTick()
		}
	}
}

// pushDepthTick broadcasts a fresh @depth20 snapshot for every subscribed depth
// stream.
func (s *Server) pushDepthTick() {
	for _, name := range s.broadcaster.marketStreams() {
		ps, ok := s.parseStreamName(name)
		if !ok {
			continue
		}
		if !s.broadcaster.hasMarketSubscribers(name) {
			continue
		}
		switch ps.kind {
		case kindDepth:
			s.broadcastDepth(ps)
		case kindDepthDiff:
			s.broadcastDepthDiff(ps)
		}
	}
}

// --- event marshalling + broadcast ---

// depthEvent is the @depth20 partial-book payload.
type depthEvent struct {
	LastUpdateID int64       `json:"lastUpdateId"`
	Bids         [][2]string `json:"bids"`
	Asks         [][2]string `json:"asks"`
}

// depthUpdateEvent is the @depth diff-stream payload (Binance "depthUpdate"):
// only the levels that changed since the previous push, with the inclusive
// update-id range U..u. A client buffers these after a REST /depth snapshot and
// drops any event whose final id u <= the snapshot's lastUpdateId.
type depthUpdateEvent struct {
	EventType     string      `json:"e"` // "depthUpdate"
	EventTime     int64       `json:"E"`
	Symbol        string      `json:"s"` // upper-case Binance symbol, e.g. BTCUSDT
	FirstUpdateID int64       `json:"U"`
	FinalUpdateID int64       `json:"u"`
	Bids          [][2]string `json:"b"`
	Asks          [][2]string `json:"a"`
}

// tradeEvent is the @trade payload (Binance aggregate-of-fields subset).
type tradeEvent struct {
	EventType string `json:"e"`
	EventTime int64  `json:"E"`
	Symbol    string `json:"s"`
	TradeID   int64  `json:"t"`
	Price     string `json:"p"`
	Quantity  string `json:"q"`
	TradeTime int64  `json:"T"`
	BuyerMkr  bool   `json:"m"`
	Ignore    bool   `json:"M"`
}

// executionReport is the user-data order-update payload (subset).
type executionReport struct {
	EventType       string `json:"e"`
	EventTime       int64  `json:"E"`
	Symbol          string `json:"s"`
	ClientOrderID   string `json:"c"`
	Side            string `json:"S"`
	OrderType       string `json:"o"`
	TimeInForce     string `json:"f"`
	OrigQty         string `json:"q"`
	Price           string `json:"p"`
	OrderStatus     string `json:"X"`
	ExecType        string `json:"x"`
	OrderID         int64  `json:"i"`
	LastFilledQty   string `json:"l"`
	CumFilledQty    string `json:"z"`
	LastFilledPrice string `json:"L"`
	TransactTime    int64  `json:"T"`
}

// combinedFrame wraps a data payload for /stream subscribers.
type combinedFrame struct {
	Stream string      `json:"stream"`
	Data   interface{} `json:"data"`
}

// broadcastDepth snapshots the engine book and broadcasts a @depth20 frame.
func (s *Server) broadcastDepth(ps parsedStream) {
	snap, err := s.engine.Snapshot(ps.engineSym)
	if err != nil {
		return
	}
	ev := s.depthPayload(snap)
	s.fanout(ps.name, ev)
}

// broadcastDepthDiff computes the book delta since the last push and, when the
// book changed, broadcasts a depthUpdate frame. A diff stream sends nothing on a
// quiet tick (and nothing on the first tick, which silently seeds the baseline)
// — clients get their starting book from REST /api/v3/depth.
func (s *Server) broadcastDepthDiff(ps parsedStream) {
	snap, err := s.engine.Snapshot(ps.engineSym)
	if err != nil {
		return
	}
	first, final, bids, asks, changed := s.depthDiffer.diff(ps.engineSym, snap)
	if !changed {
		return
	}
	if bids == nil {
		bids = [][2]string{}
	}
	if asks == nil {
		asks = [][2]string{}
	}
	ev := depthUpdateEvent{
		EventType:     "depthUpdate",
		EventTime:     s.now().UnixMilli(),
		Symbol:        strings.ToUpper(ps.binSym),
		FirstUpdateID: first,
		FinalUpdateID: final,
		Bids:          bids,
		Asks:          asks,
	}
	s.fanout(ps.name, ev)
}

// pushDepthTo sends a single depth snapshot to one connection (used on initial
// subscribe so the client gets an immediate book).
func (s *Server) pushDepthTo(c *wsConn, ps parsedStream) {
	snap, err := s.engine.Snapshot(ps.engineSym)
	if err != nil {
		return
	}
	ev := s.depthPayload(snap)
	frame := s.frameFor(c, ps.name, ev)
	if frame != nil {
		_ = c.trySend(frame)
	}
}

func (s *Server) depthPayload(snap *orderbook.Snapshot) depthEvent {
	return depthEvent{
		LastUpdateID: s.now().UnixMilli(),
		Bids:         levelsToPairs(snap.Bids, depthLevels),
		Asks:         levelsToPairs(snap.Asks, depthLevels),
	}
}

// fanout marshals ev once per wrapping mode and pushes to subscribers of stream.
// Because raw and combined subscribers may both watch the same stream name, we
// build per-conn frames respecting each conn's combined flag.
func (s *Server) fanout(stream string, payload interface{}) {
	// Pre-marshal both shapes once to avoid re-encoding per connection.
	rawBytes, err := json.Marshal(payload)
	if err != nil {
		return
	}
	combinedBytes, err := json.Marshal(combinedFrame{Stream: stream, Data: payload})
	if err != nil {
		return
	}
	s.broadcaster.publishMarketFunc(stream, func(c *wsConn) []byte {
		if c.combined {
			return combinedBytes
		}
		return rawBytes
	})
}

// frameFor builds the wire frame for one connection respecting its combined flag.
func (s *Server) frameFor(c *wsConn, stream string, payload interface{}) []byte {
	if c.combined {
		b, err := json.Marshal(combinedFrame{Stream: stream, Data: payload})
		if err != nil {
			return nil
		}
		return b
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return nil
	}
	return b
}

// --- order-book trade hook -> @trade market stream ---

// tradeSeq generates monotonic websocket trade IDs. The order book Trade carries
// no id, so we synthesize one per emitted @trade event. It is independent of the
// REST order-id sequence.
var tradeSeq atomic.Int64

// onBookTrade is the second book "trade" hook (registered after the registry's
// in AttachHooks). It broadcasts a Binance @trade event for the trade's symbol.
// It reads no registry state, so ordering relative to the registry hook does not
// matter for market streams.
func (s *Server) onBookTrade(t *orderbook.Trade) {
	if t == nil {
		return
	}
	binSym, ok := s.symbols.ToBinance(t.Instrument)
	if !ok {
		return
	}
	stream := strings.ToLower(binSym) + "@trade"
	if !s.broadcaster.hasMarketSubscribers(stream) {
		return
	}
	nowMs := s.now().UnixMilli()
	tradeTime := nowMs
	if !t.ExecutedAt.IsZero() {
		tradeTime = t.ExecutedAt.UnixMilli()
	}
	// Buyer-is-maker (m): the buyer is the maker exactly when the taker (the
	// aggressing/incoming order) is the seller. matchLocked records the taker
	// side on each Trade, so this is now exact rather than a fixed convention.
	buyerMaker := t.TakerSide == orderbook.SideSell
	ev := tradeEvent{
		EventType: "trade",
		EventTime: nowMs,
		Symbol:    binSym,
		TradeID:   tradeSeq.Add(1),
		Price:     t.Price.StringPrec(pricePrec),
		Quantity:  t.Volume.StringPrec(qtyPrec),
		TradeTime: tradeTime,
		BuyerMkr:  buyerMaker,
		Ignore:    true,
	}
	s.fanout(stream, ev)
}

// --- order-book hooks -> executionReport user-data stream ---

// emitExecutionReport reads the (already-updated) registry record for engineID
// and broadcasts an executionReport to all user-data subscribers. execType is
// the Binance "x" field (NEW/TRADE/CANCELED). It MUST be called after the
// registry hook has folded the trade/cancel into the record (guaranteed by hook
// registration order in AttachHooks).
func (s *Server) emitExecutionReport(engineID, execType string, lastFillQty, lastFillPrice decimal.Decimal) {
	rec, ok := s.registry.getByEngine(engineID)
	if !ok {
		return
	}
	nowMs := s.now().UnixMilli()
	lastQty := decimalZeroStr()
	lastPrice := decimalZeroStr()
	if execType == execTypeTrade {
		// l/L are the quantity and price of THIS fill (Binance semantics); z
		// carries the cumulative. The per-trade values come straight from the
		// order book "trade" event, so they're exact per execution.
		lastQty = lastFillQty.StringPrec(qtyPrec)
		lastPrice = lastFillPrice.StringPrec(pricePrec)
	}
	ev := executionReport{
		EventType:       "executionReport",
		EventTime:       nowMs,
		Symbol:          rec.BinanceSymbol,
		ClientOrderID:   rec.ClientOrderID,
		Side:            rec.Side,
		OrderType:       rec.Type,
		TimeInForce:     rec.TimeInForce,
		OrigQty:         rec.OrigQty.StringPrec(qtyPrec),
		Price:           rec.Price.StringPrec(pricePrec),
		OrderStatus:     rec.Status,
		ExecType:        execType,
		OrderID:         rec.OrderID,
		LastFilledQty:   lastQty,
		CumFilledQty:    rec.ExecutedQty.StringPrec(qtyPrec),
		LastFilledPrice: lastPrice,
		TransactTime:    nowMs,
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return
	}
	// Fill reports may be held back by the configured fill-report latency; the
	// report was built from the fill-time state above, so a delayed delivery
	// still reflects what happened at execution, just later. NEW/CANCELED
	// updates are never delayed. The hook runs under the engine lock, so the
	// delay must be asynchronous (time.AfterFunc), never a synchronous sleep.
	if execType == execTypeTrade && s.fillDelay != nil {
		if d := s.fillDelay(); d > 0 {
			time.AfterFunc(d, func() { s.broadcaster.publishUserData(b) })
			return
		}
	}
	s.broadcaster.publishUserData(b)
}

const (
	execTypeNew      = "NEW"
	execTypeTrade    = "TRADE"
	execTypeCanceled = "CANCELED"
)

func decimalZeroStr() string { return "0" }
