package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zeta1999/crypto-exchange-golang/internal/api/httpserver"
	"github.com/zeta1999/crypto-exchange-golang/internal/engine"
	"github.com/zeta1999/crypto-exchange-golang/internal/margin"
	"github.com/zeta1999/crypto-exchange-golang/internal/orderbook"
	"github.com/zeta1999/crypto-exchange-golang/pkg/auth"
	"github.com/zeta1999/crypto-exchange-golang/pkg/decimal"
	"github.com/zeta1999/crypto-exchange-golang/pkg/wal"
)

func TestMatchingFlowIntegration(t *testing.T) {
	walFile := t.TempDir() + "/engine.wal"
	writer, err := wal.New(walFile)
	if err != nil {
		t.Fatalf("wal new: %v", err)
	}
	defer writer.Close()

	book := orderbook.New([]string{"BTC-USD"})
	book.RegisterHook(func(evt string, data interface{}) {
		_ = writer.Append("hook."+evt, data)
	})
	validator := margin.NewValidator(book, margin.WithNotionalLimit(10_000_000))
	eng := engine.New(book, validator, writer)

	ctx := context.Background()
	if _, _, err := eng.PlaceLimit(ctx, &orderbook.Order{ID: "maker", Instrument: "BTC-USD", Price: decimal.FromInt(40_000), Volume: decimal.FromInt(1), Side: orderbook.SideBuy}); err != nil {
		t.Fatalf("place maker: %v", err)
	}
	trades, snap, err := eng.PlaceLimit(ctx, &orderbook.Order{ID: "taker", Instrument: "BTC-USD", Price: decimal.FromInt(39_000), Volume: decimal.FromInt(1), Side: orderbook.SideSell})
	if err != nil {
		t.Fatalf("place taker: %v", err)
	}
	if len(trades) != 1 {
		t.Fatalf("expected 1 trade got %d", len(trades))
	}
	if !snap.BestBid.IsZero() {
		t.Fatalf("expected empty book best bid got %s", snap.BestBid)
	}
}

func TestCancelOrderRemovesLiquidity(t *testing.T) {
	walFile := t.TempDir() + "/cancel.wal"
	writer, err := wal.New(walFile)
	if err != nil {
		t.Fatalf("wal new: %v", err)
	}
	defer writer.Close()

	book := orderbook.New([]string{"BTC-USD"})
	validator := margin.NewValidator(book, margin.WithNotionalLimit(10_000_000))
	eng := engine.New(book, validator, writer)
	ctx := context.Background()

	if _, _, err := eng.PlaceLimit(ctx, &orderbook.Order{ID: "cancel-me", Instrument: "BTC-USD", Price: decimal.FromInt(41_000), Volume: decimal.FromInt(1), Side: orderbook.SideBuy}); err != nil {
		t.Fatalf("place order: %v", err)
	}
	if _, err := eng.CancelOrder(ctx, "BTC-USD", "cancel-me"); err != nil {
		t.Fatalf("cancel order: %v", err)
	}
	snap, err := eng.Snapshot("BTC-USD")
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(snap.Bids) != 0 {
		t.Fatalf("expected empty bids got %v", snap.Bids)
	}
}

func TestHTTPAuthIntegration(t *testing.T) {
	engineStub := &mockEngine{}
	tokenValidator := auth.NewTokenValidator("secret")
	server := httpserver.New(engineStub, tokenValidator, nil, nil)
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	// unauthorized
	reqBody := map[string]interface{}{"instrument": "BTC-USD", "price": 1, "volume": 1, "side": "buy", "client_id": "c"}
	raw, _ := json.Marshal(reqBody)
	resp, err := http.Post(ts.URL+"/orders", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("post unauthorized: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 got %d", resp.StatusCode)
	}

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/orders", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post authorized: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 got %d", resp2.StatusCode)
	}
}

type mockEngine struct{}

func (m *mockEngine) PlaceLimit(ctx context.Context, ord *orderbook.Order) ([]*orderbook.Trade, *orderbook.Snapshot, error) {
	return nil, &orderbook.Snapshot{Instrument: ord.Instrument}, nil
}

func (m *mockEngine) PlaceMarket(ctx context.Context, ord *orderbook.Order) ([]*orderbook.Trade, *orderbook.Snapshot, error) {
	return m.PlaceLimit(ctx, ord)
}

func (m *mockEngine) Snapshot(symbol string) (*orderbook.Snapshot, error) {
	return &orderbook.Snapshot{Instrument: symbol}, nil
}
