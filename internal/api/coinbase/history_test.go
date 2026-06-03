package coinbase

import (
	"net/http"
	"testing"

	"github.com/zeta1999/crypto-exchange-golang/internal/orderbook"
)

// TestHistoricalReturnsTerminalOrders: a FILLED order is retained and returned
// by the historical-orders endpoint (order_status=FILLED), not just OPEN orders.
func TestHistoricalReturnsTerminalOrders(t *testing.T) {
	h := newHarness(t)
	// Resting ask so a market BUY fills immediately → terminal (FILLED).
	wsAddOrder(t, h.book, "seed-ask", orderbook.SideSell, "100", "5")

	body := `{"client_order_id":"hist1","product_id":"BTC-USD","side":"BUY",` +
		`"order_configuration":{"market_market_ioc":{"base_size":"1"}}}`
	resp := h.signedDo(t, http.MethodPost, "/api/v3/brokerage/orders", body)
	resp.Body.Close()

	q := h.signedDo(t, http.MethodGet, "/api/v3/brokerage/orders/historical/batch?product_id=BTC-USD&order_status=FILLED", "")
	defer q.Body.Close()
	var hb historicalBatchResponse
	decode(t, q, &hb)
	if len(hb.Orders) != 1 || hb.Orders[0].Status != "FILLED" {
		t.Fatalf("historical FILLED = %+v, want one FILLED order", hb.Orders)
	}

	// OPEN filter must now exclude the filled order.
	q2 := h.signedDo(t, http.MethodGet, "/api/v3/brokerage/orders/historical/batch?product_id=BTC-USD&order_status=OPEN", "")
	defer q2.Body.Close()
	var open historicalBatchResponse
	decode(t, q2, &open)
	if len(open.Orders) != 0 {
		t.Errorf("historical OPEN = %d, want 0 (order is filled)", len(open.Orders))
	}
}
