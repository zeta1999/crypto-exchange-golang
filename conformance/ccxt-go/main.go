// CCXT conformance smoke: point the STOCK ccxt-go binance client at our local
// Binance-compatible edge by overriding ONLY the base URL, then run the normal
// client lifecycle (loadMarkets → fetchOrderBook → createOrder → fetchOpenOrders
// → cancelOrder). No CCXT source is forked; we mutate Urls/Options at runtime.
package main

import (
	"fmt"
	"os"

	ccxt "github.com/ccxt/ccxt/go/v4"
)

func fail(stage string, err error) {
	fmt.Printf("FAIL [%s]: %v\n", stage, err)
	os.Exit(1)
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func main() {
	base := "http://localhost:8092/api/v3"

	// Construct WITHOUT credentials so loadMarkets stays purely public — a
	// credentialed client makes CCXT try signed currency discovery against the
	// real venue. Credentials are set after loadMarkets, before signed calls.
	ex := ccxt.NewBinance(map[string]any{
		"enableRateLimit": false,
	})

	// Only spot markets (our edge serves /api/v3 spot, not fapi/dapi/eapi), and
	// no signed currency discovery (our edge has no sapi endpoint).
	ex.Options.Store("fetchMarkets", []any{"spot"})
	ex.Options.Store("fetchCurrencies", false)

	// *** Modify ONLY the endpoint URL ***  point public+private at our edge.
	urls := ex.Urls.(map[string]any)
	api := urls["api"].(map[string]any)
	api["public"] = base
	api["private"] = base
	urls["api"] = api
	ex.Urls = urls

	// 1) loadMarkets → GET /api/v3/exchangeInfo
	markets, err := ex.LoadMarkets()
	if err != nil {
		fail("loadMarkets", err)
	}
	fmt.Printf("OK  loadMarkets: %d markets\n", len(markets))
	if _, ok := markets["BTC/USD"]; !ok {
		fail("loadMarkets", fmt.Errorf("BTC/USD market not found; got %v keys", len(markets)))
	}
	m := markets["BTC/USD"]
	fmt.Printf("    BTC/USD: id=%v base=%v quote=%v\n", str(m.Id), str(m.BaseCurrency), str(m.QuoteCurrency))

	// Now attach credentials for the signed endpoints.
	ex.ApiKey = "conf-key"
	ex.Secret = "conf-secret"

	// 2) fetchOrderBook → GET /api/v3/depth
	ob, err := ex.FetchOrderBook("BTC/USD")
	if err != nil {
		fail("fetchOrderBook", err)
	}
	fmt.Printf("OK  fetchOrderBook BTC/USD: %d bids, %d asks", len(ob.Bids), len(ob.Asks))
	if len(ob.Bids) > 0 && len(ob.Asks) > 0 {
		fmt.Printf(" (best bid %.2f / ask %.2f)", ob.Bids[0][0], ob.Asks[0][0])
	}
	fmt.Println()

	// 3) createLimitOrder (signed) → POST /api/v3/order
	order, err := ex.CreateLimitOrder("BTC/USD", "buy", 0.01, 40000)
	if err != nil {
		fail("createLimitOrder", err)
	}
	flt := func(p *float64) float64 {
		if p == nil {
			return 0
		}
		return *p
	}
	fmt.Printf("OK  createLimitOrder: id=%v status=%v side=%v price=%.2f amount=%.4f\n",
		str(order.Id), str(order.Status), str(order.Side), flt(order.Price), flt(order.Amount))

	// 4) fetchOpenOrders (signed) → GET /api/v3/openOrders
	open, err := ex.FetchOpenOrders(ccxt.WithFetchOpenOrdersSymbol("BTC/USD"))
	if err != nil {
		fail("fetchOpenOrders", err)
	}
	fmt.Printf("OK  fetchOpenOrders BTC/USD: %d open\n", len(open))

	// 5) cancelOrder (signed) → DELETE /api/v3/order
	if str(order.Id) != "" {
		canc, err := ex.CancelOrder(str(order.Id), ccxt.WithCancelOrderSymbol("BTC/USD"))
		if err != nil {
			fail("cancelOrder", err)
		}
		fmt.Printf("OK  cancelOrder: id=%v status=%v\n", str(canc.Id), str(canc.Status))
	}

	fmt.Println("\nCCXT-GO CONFORMANCE: PASS")
}
