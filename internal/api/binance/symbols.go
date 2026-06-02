// Package binance implements a documented SUBSET of the Binance spot REST API
// (api/v3) as an edge in front of the matching engine. A stock Binance client
// (CCXT, python-binance) pointed at this server — with only the base URL
// changed — can place, cancel, and query orders and read depth/ticker.
//
// This is REST only. The WebSocket market and user-data streams are a noted
// follow-up and are deliberately NOT implemented here. Where Binance exposes
// fields the engine cannot populate (commissions, account balances) the edge
// returns sensible zeros and documents the stub at the call site.
package binance

import "sync"

// SymbolPair maps a Binance concatenated symbol ("BTCUSDT") to the engine's
// hyphenated instrument ("BTC-USD").
type SymbolPair struct {
	Binance string
	Engine  string
}

// SymbolMap translates symbols between the Binance wire form and the engine's
// instrument identifiers in both directions. It is read-only after
// construction and therefore safe for concurrent use.
type SymbolMap struct {
	mu          sync.RWMutex
	toEngine    map[string]string
	toBinance   map[string]string
	binanceList []string
}

// NewSymbolMap builds a SymbolMap from a list of pairs. Later duplicates
// overwrite earlier ones.
func NewSymbolMap(pairs []SymbolPair) *SymbolMap {
	m := &SymbolMap{
		toEngine:  make(map[string]string, len(pairs)),
		toBinance: make(map[string]string, len(pairs)),
	}
	for _, p := range pairs {
		if p.Binance == "" || p.Engine == "" {
			continue
		}
		if _, seen := m.toEngine[p.Binance]; !seen {
			m.binanceList = append(m.binanceList, p.Binance)
		}
		m.toEngine[p.Binance] = p.Engine
		m.toBinance[p.Engine] = p.Binance
	}
	return m
}

// ToEngine resolves a Binance symbol ("BTCUSDT") to an engine instrument
// ("BTC-USD"). ok is false for an unknown symbol; the handler then returns the
// Binance -1121 "Invalid symbol" error.
func (m *SymbolMap) ToEngine(binanceSym string) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	v, ok := m.toEngine[binanceSym]
	return v, ok
}

// ToBinance resolves an engine instrument ("BTC-USD") back to a Binance symbol
// ("BTCUSDT"). ok is false for an unknown instrument.
func (m *SymbolMap) ToBinance(engineSym string) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	v, ok := m.toBinance[engineSym]
	return v, ok
}

// BinanceSymbols returns the configured Binance symbols in registration order.
func (m *SymbolMap) BinanceSymbols() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, len(m.binanceList))
	copy(out, m.binanceList)
	return out
}

// Pairs returns the configured (Binance, engine) symbol pairs in registration
// order. Used by exchangeInfo to enumerate markets for client loadMarkets().
func (m *SymbolMap) Pairs() []SymbolPair {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]SymbolPair, 0, len(m.binanceList))
	for _, b := range m.binanceList {
		out = append(out, SymbolPair{Binance: b, Engine: m.toEngine[b]})
	}
	return out
}
