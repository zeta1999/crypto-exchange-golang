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

// expiryHourUTC is the settlement hour (08:00 UTC) shared by Deribit and
// Binance options. The contract date in the symbol denotes this instant.
const expiryHourUTC = 8

// Exotic is one listed FLOW exotic on the options surface: a bespoke payoff
// (barrier, Asian, autocall, lookback, digital, …) that has no closed form
// and therefore cannot — and must not — be priced by the vanilla
// Black–Scholes path. It carries the same contract terms a vanilla
// Instrument would (coin / underlying index / strike / settlement instant)
// plus the payoff and per-contract model overrides.
//
// VenueSymbol is the operator-chosen venue symbol (unique in the book). Its
// FINAL TOKEN MUST NOT be a bare "C" or "P" (e.g. "BTC-261231-50000-BAR");
// that keeps exotic symbols disjoint from the four venues' vanilla symbol
// parsers, which read the last token as the side. If left empty, the
// constructor sets it to COIN-YYMMDD-STRIKE-EXOT.
//
// Exactly one of Payoff (inline Aria script) or PayoffFile (.aria path
// relative to the saga checkout root) must be non-empty. The payoff — the .aria
// script — is the instrument; Strike here is used only to derive a fallback
// vol from the underlying surface and to list the contract, not to build a
// vanilla payoff. Vol/Model, when set, override the shared pricer model for
// this contract only; a zero effective vol falls back to the underlying vol
// surface at the strike.
//
// Pricing is one call to the attached Pricer per cache interval. There is no
// vanilla-leg decomposition for an exotic — the payoff IS the script.
type Exotic struct {
	VenueSymbol string
	Coin        string
	Underlying  string
	Quote       string
	Strike      float64
	Kind        options.Kind
	Expiry      time.Time
	Payoff      string // inline Aria script
	PayoffFile  string // .aria path relative to the saga checkout root
	Vol         float64
	Model       string
}

// NewExoticInstrument builds a flow exotic. Kind is carried for the contract
// listing only (the payoff defines the economics); pass options.Call or Put to
// match the dominant side. If venueSymbol is empty it defaults to
// COIN-YYMMDD-STRIKE-EXOT.
func NewExoticInstrument(coin, underlying, quote string, strike float64, kind options.Kind, date time.Time, venueSymbol string) Exotic {
	y, m, d := date.UTC().Date()
	ex := Exotic{
		Coin:       coin,
		Underlying: underlying,
		Quote:      quote,
		Strike:     strike,
		Kind:       kind,
		Expiry:     time.Date(y, m, d, expiryHourUTC, 0, 0, 0, time.UTC),
	}
	if venueSymbol == "" {
		ex.VenueSymbol = fmt.Sprintf("%s-%s-%s-%s",
			coin, ex.Expiry.UTC().Format("060102"),
			strconv.FormatFloat(strike, 'f', -1, 64), "EXOT")
	} else {
		ex.VenueSymbol = venueSymbol
	}
	return ex
}

// TimeToExpiry returns the year fraction (ACT/365) from `now` to expiry,
// clamped at 0 for an expired contract.
func (e Exotic) TimeToExpiry(now time.Time) float64 {
	dt := e.Expiry.Sub(now).Seconds()
	if dt <= 0 {
		return 0
	}
	return dt / (365.0 * 24 * 3600)
}

// ExpiryMillis is the settlement instant in epoch-ms (EAPI convention).
func (e Exotic) ExpiryMillis() int64 { return e.Expiry.UnixMilli() }

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
