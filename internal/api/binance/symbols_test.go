package binance

import "testing"

func newTestSymbolMap() *SymbolMap {
	return NewSymbolMap([]SymbolPair{
		{Binance: "BTCUSDT", Engine: "BTC-USD"},
		{Binance: "ETHUSDT", Engine: "ETH-USD"},
	})
}

func TestSymbolMap_RoundTrip(t *testing.T) {
	m := newTestSymbolMap()

	eng, ok := m.ToEngine("BTCUSDT")
	if !ok || eng != "BTC-USD" {
		t.Fatalf("ToEngine(BTCUSDT) = %q,%v want BTC-USD,true", eng, ok)
	}
	bin, ok := m.ToBinance("BTC-USD")
	if !ok || bin != "BTCUSDT" {
		t.Fatalf("ToBinance(BTC-USD) = %q,%v want BTCUSDT,true", bin, ok)
	}
}

func TestSymbolMap_Unknown(t *testing.T) {
	m := newTestSymbolMap()
	if _, ok := m.ToEngine("DOGEUSDT"); ok {
		t.Fatalf("ToEngine(DOGEUSDT) ok = true, want false")
	}
	if _, ok := m.ToBinance("DOGE-USD"); ok {
		t.Fatalf("ToBinance(DOGE-USD) ok = true, want false")
	}
}

func TestSymbolMap_BinanceSymbols(t *testing.T) {
	m := newTestSymbolMap()
	got := m.BinanceSymbols()
	if len(got) != 2 || got[0] != "BTCUSDT" || got[1] != "ETHUSDT" {
		t.Fatalf("BinanceSymbols() = %v want [BTCUSDT ETHUSDT]", got)
	}
}
