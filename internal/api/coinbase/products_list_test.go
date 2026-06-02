package coinbase

import (
	"net/http"
	"testing"
)

// TestProductsList verifies the market-discovery list a stock client reads in
// loadMarkets(): every configured product, split into base/quote currency ids.
func TestProductsList(t *testing.T) {
	h := newHarness(t)

	resp := h.get(t, "/api/v3/brokerage/products")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var list productsListResponse
	decode(t, resp, &list)

	if list.NumProducts != 2 || len(list.Products) != 2 {
		t.Fatalf("num_products=%d len=%d, want 2/2", list.NumProducts, len(list.Products))
	}
	byID := map[string]productResponse{}
	for _, p := range list.Products {
		byID[p.ProductID] = p
	}
	btc, ok := byID["BTC-USD"]
	if !ok {
		t.Fatalf("BTC-USD missing; got %v", byID)
	}
	if btc.BaseCur != "BTC" || btc.QuoteCur != "USD" {
		t.Errorf("BTC-USD base/quote = %q/%q, want BTC/USD", btc.BaseCur, btc.QuoteCur)
	}
	if btc.Status != "online" || btc.TradingDisabled {
		t.Errorf("BTC-USD not active: status=%q trading_disabled=%v", btc.Status, btc.TradingDisabled)
	}
	// CCXT derives precision/limits from these — they must be present.
	if btc.BaseIncrement != "0.00000001" || btc.QuoteIncrement != "0.00000001" {
		t.Errorf("BTC-USD increments = %q/%q, want 0.00000001 each", btc.BaseIncrement, btc.QuoteIncrement)
	}
	if btc.BaseMinSize == "" {
		t.Errorf("BTC-USD base_min_size is empty")
	}

	// The bare list path must not collide with the single-product subtree.
	one := h.get(t, "/api/v3/brokerage/products/ETH-USD")
	if one.StatusCode != http.StatusOK {
		t.Fatalf("single-product route broke: status %d", one.StatusCode)
	}
	var pr productResponse
	decode(t, one, &pr)
	if pr.ProductID != "ETH-USD" {
		t.Errorf("single product = %q, want ETH-USD", pr.ProductID)
	}
}
