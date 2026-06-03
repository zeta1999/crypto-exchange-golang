package coinbase

import (
	"net/http"
	"testing"
)

// TestMarketAliasRoutes pins the public "market/" path aliases that ccxt-go
// (>= v4.5) uses for market discovery + order book: loadMarkets() fetches
// /api/v3/brokerage/market/products and fetchOrderBook() fetches
// /api/v3/brokerage/market/product_book. They must mirror the legacy
// (non-market) paths. Regression guard for the CCXT-go Coinbase conformance.
func TestMarketAliasRoutes(t *testing.T) {
	h := newHarness(t)

	// market/products == products (full list).
	resp := h.get(t, "/api/v3/brokerage/market/products")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("market/products status = %d, want 200", resp.StatusCode)
	}
	var list productsListResponse
	decode(t, resp, &list)
	if list.NumProducts != 2 {
		t.Errorf("market/products num_products=%d, want 2", list.NumProducts)
	}

	// market/products/{id} == products/{id} (single product).
	one := h.get(t, "/api/v3/brokerage/market/products/BTC-USD")
	if one.StatusCode != http.StatusOK {
		t.Fatalf("market/products/BTC-USD status = %d, want 200", one.StatusCode)
	}
	var pr productResponse
	decode(t, one, &pr)
	if pr.ProductID != "BTC-USD" {
		t.Errorf("market/products/{id} = %q, want BTC-USD", pr.ProductID)
	}

	// market/product_book == product_book (depth).
	book := h.get(t, "/api/v3/brokerage/market/product_book?product_id=BTC-USD")
	if book.StatusCode != http.StatusOK {
		t.Fatalf("market/product_book status = %d, want 200", book.StatusCode)
	}
	book.Body.Close()
}
