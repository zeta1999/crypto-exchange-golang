package coinbase

import (
	"testing"

	"github.com/zeta1999/crypto-exchange-golang/pkg/decimal"
)

// TestOrderFees checks the fee math: total_fees = filled_value * rate; a BUY's
// all-in cost adds the fee, a SELL's net proceeds subtract it.
func TestOrderFees(t *testing.T) {
	rate := decimal.MustParse("0.006")

	buy := orderRecord{Side: "BUY", FilledValue: decimal.MustParse("1000")}
	fees, after := orderFees(buy, rate)
	if fees != "6.00000000" {
		t.Fatalf("buy total_fees = %q, want 6.00000000", fees)
	}
	if after != "1006.00000000" {
		t.Fatalf("buy total_value_after_fees = %q, want 1006.00000000", after)
	}

	sell := orderRecord{Side: "SELL", FilledValue: decimal.MustParse("1000")}
	fees, after = orderFees(sell, rate)
	if fees != "6.00000000" {
		t.Fatalf("sell total_fees = %q, want 6.00000000", fees)
	}
	if after != "994.00000000" {
		t.Fatalf("sell total_value_after_fees = %q, want 994.00000000", after)
	}

	// An unfilled order has zero fees.
	none := orderRecord{Side: "BUY", FilledValue: decimal.Zero}
	if fees, _ := orderFees(none, rate); fees != "0.00000000" {
		t.Fatalf("unfilled total_fees = %q, want 0.00000000", fees)
	}
}

// TestToOrderViewFees confirms the fee fields land on the order view rendered by
// the historical endpoints.
func TestToOrderViewFees(t *testing.T) {
	rec := orderRecord{
		OrderID:     "emu-1",
		ProductID:   "BTC-USD",
		Side:        "BUY",
		OrderType:   "LIMIT",
		Price:       decimal.MustParse("40000"),
		OrigSize:    decimal.MustParse("0.01"),
		FilledSize:  decimal.MustParse("0.01"),
		FilledValue: decimal.MustParse("400"),
		Status:      "FILLED",
	}
	ov := toOrderViewRate(rec, decimal.MustParse("0.006"))
	if ov.TotalFees != "2.40000000" || ov.Fee != "2.40000000" {
		t.Fatalf("total_fees/fee = %q/%q, want 2.40000000", ov.TotalFees, ov.Fee)
	}
	if ov.TotalValueAfterFees != "402.40000000" {
		t.Fatalf("total_value_after_fees = %q, want 402.40000000", ov.TotalValueAfterFees)
	}
	if ov.SizeInclusiveOfFees {
		t.Fatalf("size_inclusive_of_fees should be false")
	}
}

// TestProductPriceIncrement confirms the product document advertises
// price_increment (CCXT reads it for price precision).
func TestProductPriceIncrement(t *testing.T) {
	p := newProductResponse("BTC-USD", "40000")
	if p.PriceIncrement != stepStr(pricePrec) {
		t.Fatalf("price_increment = %q, want %q", p.PriceIncrement, stepStr(pricePrec))
	}
	if p.QuoteIncrement != p.PriceIncrement {
		t.Fatalf("quote_increment %q != price_increment %q", p.QuoteIncrement, p.PriceIncrement)
	}
}
