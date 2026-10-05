package optmarket

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/zeta1999/crypto-exchange-golang/pkg/options"
)

// DeribitName is BTC-31DEC26-50000-C. The book itself stays on Symbol().
func (in Instrument) DeribitName() string {
	return fmt.Sprintf("%s-%s-%s-%s", in.Coin, deribitDate(in.Expiry), strikeText(in.Strike), sideLetter(in.Kind))
}

// BybitName matches Deribit's date form: BTC-31DEC26-50000-C.
func (in Instrument) BybitName() string { return in.DeribitName() }

// OKXName is BTC-USD-261231-50000-C. OKX lists the family as COIN-USD.
func (in Instrument) OKXName() string {
	return fmt.Sprintf("%s-USD-%s-%s-%s",
		in.Coin, in.Expiry.UTC().Format("060102"), strikeText(in.Strike), sideLetter(in.Kind))
}

func sideLetter(kind options.Kind) string {
	if kind == options.Put {
		return "P"
	}
	return "C"
}

func strikeText(strike float64) string {
	return strconv.FormatFloat(strike, 'f', -1, 64)
}

func deribitDate(t time.Time) string {
	return strings.ToUpper(t.UTC().Format("2Jan06"))
}

// ParseDeribitName parses BTC-31DEC26-50000-C. The quote asset is not in the
// name; the caller looks the instrument up with Find.
func ParseDeribitName(name string) (coin string, strike float64, kind options.Kind, expiry time.Time, err error) {
	return parseDated(name, true)
}

// ParseBybitName parses the same BTC-31DEC26-50000-C form as Deribit.
func ParseBybitName(name string) (coin string, strike float64, kind options.Kind, expiry time.Time, err error) {
	return parseDated(name, true)
}

// ParseOKXName parses BTC-USD-261231-50000-C.
func ParseOKXName(name string) (coin string, strike float64, kind options.Kind, expiry time.Time, err error) {
	parts := strings.Split(name, "-")
	if len(parts) != 5 || parts[1] != "USD" {
		return "", 0, 0, time.Time{}, fmt.Errorf("optmarket: bad OKX option %q", name)
	}
	return parseTail(parts[0], parts[2], parts[3], parts[4], false)
}

func parseDated(name string, deribitDate bool) (string, float64, options.Kind, time.Time, error) {
	parts := strings.Split(name, "-")
	if len(parts) != 4 {
		return "", 0, 0, time.Time{}, fmt.Errorf("optmarket: bad option name %q", name)
	}
	return parseTail(parts[0], parts[1], parts[2], parts[3], deribitDate)
}

func parseTail(coin, dateStr, strikeStr, sideStr string, deribit bool) (string, float64, options.Kind, time.Time, error) {
	var (
		date time.Time
		err  error
	)
	if deribit {
		// 31DEC26 -> 31Dec26. time.Parse month is "Jan".
		date, err = time.Parse("2Jan06", deribitToGo(dateStr))
	} else {
		date, err = time.Parse("060102", dateStr)
	}
	if err != nil {
		return "", 0, 0, time.Time{}, fmt.Errorf("optmarket: bad expiry %q: %w", dateStr, err)
	}
	strike, err := strconv.ParseFloat(strikeStr, 64)
	if err != nil || strike <= 0 {
		return "", 0, 0, time.Time{}, fmt.Errorf("optmarket: bad strike %q", strikeStr)
	}
	var kind options.Kind
	switch strings.ToUpper(sideStr) {
	case "C":
		kind = options.Call
	case "P":
		kind = options.Put
	default:
		return "", 0, 0, time.Time{}, fmt.Errorf("optmarket: bad side %q", sideStr)
	}
	y, m, d := date.UTC().Date()
	expiry := time.Date(y, m, d, expiryHourUTC, 0, 0, 0, time.UTC)
	return coin, strike, kind, expiry, nil
}

// deribitToGo turns 31DEC26 into 31Dec26 for time.Parse.
func deribitToGo(s string) string {
	s = strings.ToUpper(s)
	if len(s) < 5 {
		return s
	}
	// day is the leading digits, then 3-letter month, then 2-digit year.
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == 0 || i+5 > len(s) {
		return s
	}
	mon := s[i : i+3]
	return s[:i] + mon[:1] + strings.ToLower(mon[1:]) + s[i+3:]
}
