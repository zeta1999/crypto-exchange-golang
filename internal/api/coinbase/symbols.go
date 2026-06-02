// Package coinbase implements a documented SUBSET of the Coinbase Advanced
// Trade REST API (/api/v3/brokerage) as an edge in front of the matching
// engine. A stock Coinbase Advanced Trade client (CCXT in HMAC mode) pointed at
// this server — with only the base URL changed — can place, cancel, and query
// orders and read the product book and a product ticker.
//
// This is REST only. The WebSocket market and user channels are a noted
// follow-up and are deliberately NOT implemented here. Coinbase product IDs
// already match the engine's hyphenated instrument identifiers ("BTC-USD"), so
// the symbol map is an identity over a configured allow-list — unknown products
// are rejected with a Coinbase-style INVALID_PRODUCT_ID error.
//
// Authentication uses the legacy Coinbase Exchange HMAC-SHA256 scheme
// (CB-ACCESS-* headers), which Advanced Trade historically accepted and which
// CCXT can produce in HMAC mode. Production Advanced Trade JWT (ES256) auth is
// DEFERRED: verifying ES256 requires the client's EC public key, which the
// emulator does not provision. See auth.go.
package coinbase

import "sync"

// Products is the set of product IDs the edge will serve. Coinbase product IDs
// are identical to engine instruments ("BTC-USD"), so this is an allow-list,
// not a translation table. It is read-only after construction and therefore
// safe for concurrent use.
type Products struct {
	mu    sync.RWMutex
	known map[string]struct{}
	list  []string
}

// NewProducts builds a Products allow-list. Blank entries and later duplicates
// are ignored; registration order is preserved.
func NewProducts(productIDs []string) *Products {
	p := &Products{known: make(map[string]struct{}, len(productIDs))}
	for _, id := range productIDs {
		if id == "" {
			continue
		}
		if _, seen := p.known[id]; seen {
			continue
		}
		p.known[id] = struct{}{}
		p.list = append(p.list, id)
	}
	return p
}

// Resolve checks that productID is configured and returns the engine instrument
// (which is identical). ok is false for an unknown product; the handler then
// returns the Coinbase INVALID_PRODUCT_ID error.
func (p *Products) Resolve(productID string) (string, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	_, ok := p.known[productID]
	if !ok {
		return "", false
	}
	return productID, true
}

// List returns the configured product IDs in registration order.
func (p *Products) List() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]string, len(p.list))
	copy(out, p.list)
	return out
}
