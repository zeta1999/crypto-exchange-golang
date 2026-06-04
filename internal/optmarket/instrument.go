// Package optmarket is the emulator's crypto-options market-data surface (CR-9):
// a set of European, cash-settled option instruments priced with Black–Scholes
// off the underlying spot index, exposed in Binance-EAPI-compatible shapes
// (mark price + implied vol + greeks + a synthetic book). It is engine-agnostic
// — it takes an index-price source and a clock — so it is deterministic and
// unit-testable, and its output can be captured as recorded non-regression
// fixtures for offline tests (Vivaldi's vivaldi-optdata is the consumer).
//
// Semantics modeled (verified against vendor docs, 2026-06-04):
//   - European exercise, CASH-settled in the quote asset (USDT). No early
//     exercise, no physical delivery.
//   - Expiry at 08:00 UTC on the contract date (Deribit/Binance convention).
//   - Inverse/coin-margined and Deribit's index TWAP settlement are out of scope
//     for this first cut (linear USDT, point-in-time index). Noted, not faked.
package optmarket

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/zeta1999/crypto-exchange-golang/pkg/options"
)

// expiryHourUTC is the settlement hour (08:00 UTC) shared by Deribit and Binance
// options. The contract date in the symbol denotes this instant.
const expiryHourUTC = 8

// Instrument is one European, cash-settled option.
type Instrument struct {
	// Coin is the symbol prefix (e.g. "BTC"); Underlying is the index pair the
	// spot price is read from (e.g. "BTCUSDT"). Quote is the cash-settlement
	// asset (e.g. "USDT").
	Coin       string
	Underlying string
	Quote      string
	Strike     float64
	Kind       options.Kind
	// Expiry is the settlement instant (date @ 08:00 UTC).
	Expiry time.Time
}

// NewInstrument builds an instrument from its parts. `date` is the calendar
// expiry day; the settlement instant is set to 08:00 UTC on that day.
func NewInstrument(coin, underlying, quote string, strike float64, kind options.Kind, date time.Time) Instrument {
	y, m, d := date.UTC().Date()
	return Instrument{
		Coin:       coin,
		Underlying: underlying,
		Quote:      quote,
		Strike:     strike,
		Kind:       kind,
		Expiry:     time.Date(y, m, d, expiryHourUTC, 0, 0, 0, time.UTC),
	}
}

// Symbol returns the Binance-EAPI instrument symbol: COIN-YYMMDD-STRIKE-{C|P},
// e.g. "BTC-200730-9000-C".
func (in Instrument) Symbol() string {
	side := "C"
	if in.Kind == options.Put {
		side = "P"
	}
	return fmt.Sprintf("%s-%s-%s-%s",
		in.Coin,
		in.Expiry.UTC().Format("060102"),
		strconv.FormatFloat(in.Strike, 'f', -1, 64),
		side,
	)
}

// ExpiryMillis is the settlement instant in epoch-ms (EAPI's expiryDate field).
func (in Instrument) ExpiryMillis() int64 { return in.Expiry.UnixMilli() }

// TimeToExpiry returns the year fraction (ACT/365) from `now` to expiry, clamped
// at 0 for an expired contract.
func (in Instrument) TimeToExpiry(now time.Time) float64 {
	dt := in.Expiry.Sub(now).Seconds()
	if dt <= 0 {
		return 0
	}
	return dt / (365.0 * 24 * 3600)
}

// IsExpired reports whether the contract has settled as of `now`.
func (in Instrument) IsExpired(now time.Time) bool { return !now.Before(in.Expiry) }

// ParseSymbol parses a Binance-EAPI option symbol (COIN-YYMMDD-STRIKE-{C|P}).
// The quote asset is not encoded in the symbol; the caller supplies it (USDT for
// Binance linear options). The returned instrument's Underlying is Coin+Quote.
func ParseSymbol(symbol, quote string) (Instrument, error) {
	parts := strings.Split(symbol, "-")
	if len(parts) != 4 {
		return Instrument{}, fmt.Errorf("optmarket: bad option symbol %q (want COIN-YYMMDD-STRIKE-C|P)", symbol)
	}
	coin, dateStr, strikeStr, sideStr := parts[0], parts[1], parts[2], parts[3]

	date, err := time.Parse("060102", dateStr)
	if err != nil {
		return Instrument{}, fmt.Errorf("optmarket: bad expiry %q in %q: %w", dateStr, symbol, err)
	}
	strike, err := strconv.ParseFloat(strikeStr, 64)
	if err != nil || strike <= 0 {
		return Instrument{}, fmt.Errorf("optmarket: bad strike %q in %q", strikeStr, symbol)
	}
	var kind options.Kind
	switch strings.ToUpper(sideStr) {
	case "C":
		kind = options.Call
	case "P":
		kind = options.Put
	default:
		return Instrument{}, fmt.Errorf("optmarket: bad side %q in %q (want C or P)", sideStr, symbol)
	}
	return NewInstrument(coin, coin+quote, quote, strike, kind, date), nil
}
