package binance

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// defaultRecvWindow is Binance's default recvWindow (milliseconds): a SIGNED
// request whose timestamp is more than this far from server time is rejected.
const defaultRecvWindow = 5000

// maxRecvWindow caps a client-supplied recvWindow; Binance rejects values above
// 60000ms.
const maxRecvWindow = 60000

// Authenticator verifies Binance SIGNED requests. Binance signs the literal
// query string: signature = hex(HMAC_SHA256(queryString_without_signature,
// secret)), and the API key travels in the X-MBX-APIKEY header.
//
// The clock is injected (now) so verification is deterministic in tests.
type Authenticator struct {
	apiKey string
	secret []byte
	now    func() time.Time
}

// NewAuthenticator returns an Authenticator for the given key/secret. now may
// be nil, in which case time.Now is used.
func NewAuthenticator(apiKey, secret string, now func() time.Time) *Authenticator {
	if now == nil {
		now = time.Now
	}
	return &Authenticator{apiKey: apiKey, secret: []byte(secret), now: now}
}

// Verify validates a SIGNED request:
//   - X-MBX-APIKEY header is present and matches the configured key,
//   - the signature query param equals HMAC_SHA256 over the on-wire query
//     string up to (but excluding) "&signature=" / "signature=",
//   - timestamp is present and within recvWindow of server time.
//
// The HMAC is recomputed over the exact raw query bytes — params are never
// re-encoded or reordered — because Binance signs the literal string.
func (a *Authenticator) Verify(r *http.Request) error {
	key := r.Header.Get("X-MBX-APIKEY")
	if key == "" {
		return &apiError{Code: codeBadAPIKeyFmt, Msg: "API-key format invalid.", status: http.StatusUnauthorized}
	}
	if subtle.ConstantTimeCompare([]byte(key), []byte(a.apiKey)) != 1 {
		return &apiError{Code: codeRejectedKey, Msg: "Invalid API-key, IP, or permissions for action.", status: http.StatusUnauthorized}
	}

	raw := rawQuery(r)
	signed, sig, ok := splitSignature(raw)
	if !ok {
		return &apiError{Code: codeInvalidSignature, Msg: "Signature for this request is not valid.", status: http.StatusBadRequest}
	}

	mac := hmac.New(sha256.New, a.secret)
	mac.Write([]byte(signed))
	expected := hex.EncodeToString(mac.Sum(nil))
	// Binance signatures are lowercase hex; compare case-insensitively but in
	// constant time against the normalised forms.
	if subtle.ConstantTimeCompare([]byte(strings.ToLower(sig)), []byte(expected)) != 1 {
		return &apiError{Code: codeInvalidSignature, Msg: "Signature for this request is not valid.", status: http.StatusBadRequest}
	}

	return a.checkTimestamp(r)
}

// checkTimestamp enforces presence and the recvWindow bound. It reads the
// parsed form (signing already validated the raw bytes, so re-parsing here is
// safe).
func (a *Authenticator) checkTimestamp(r *http.Request) error {
	q := r.URL.Query()
	tsStr := q.Get("timestamp")
	if tsStr == "" {
		return errMandatoryParam("timestamp")
	}
	tsMs, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil {
		return errIllegalParam("timestamp")
	}

	recv := int64(defaultRecvWindow)
	if rw := q.Get("recvWindow"); rw != "" {
		v, err := strconv.ParseInt(rw, 10, 64)
		if err != nil {
			return errIllegalParam("recvWindow")
		}
		if v > maxRecvWindow {
			return &apiError{Code: codeIllegalParam, Msg: "recvWindow must be less than 60000.", status: http.StatusBadRequest}
		}
		recv = v
	}

	nowMs := a.now().UnixMilli()
	// Binance: reject if timestamp is in the future beyond ~1s, or older than
	// recvWindow. We treat the window symmetrically around server time.
	diff := nowMs - tsMs
	if diff > recv || diff < -1000 {
		return &apiError{Code: codeInvalidTimestamp, Msg: "Timestamp for this request is outside of the recvWindow.", status: http.StatusBadRequest}
	}
	return nil
}

// rawQuery returns the on-wire query string (everything after '?'), preserving
// the client's exact ordering and encoding.
func rawQuery(r *http.Request) string {
	return r.URL.RawQuery
}

// splitSignature separates the signed portion of a query string from the
// signature value. Binance always appends signature last, so we split on the
// final "signature=" occurrence. Returns ok=false if no signature is present.
func splitSignature(raw string) (signed, sig string, ok bool) {
	const key = "signature="
	idx := strings.LastIndex(raw, key)
	if idx < 0 {
		return "", "", false
	}
	sig = raw[idx+len(key):]
	// Trim any trailing params after the signature (Binance puts none, but be
	// defensive) and the separator preceding "signature=".
	if amp := strings.IndexByte(sig, '&'); amp >= 0 {
		sig = sig[:amp]
	}
	signed = raw[:idx]
	signed = strings.TrimRight(signed, "&")
	if sig == "" {
		return "", "", false
	}
	return signed, sig, true
}
