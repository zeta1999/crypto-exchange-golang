package binance

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"
	"time"
)

const (
	testAPIKey = "test-key"
	testSecret = "test-secret"
)

// sign computes the Binance signature for a query string (everything before
// &signature=).
func sign(secret, queryString string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(queryString))
	return hex.EncodeToString(mac.Sum(nil))
}

func fixedClock(ms int64) func() time.Time {
	t := time.UnixMilli(ms).UTC()
	return func() time.Time { return t }
}

// signedRequest builds an http.Request with the given query (without signature)
// signed and the API key header set.
func signedRequest(t *testing.T, key, secret, query string) *http.Request {
	t.Helper()
	sig := sign(secret, query)
	r, err := http.NewRequest(http.MethodPost, "http://x/api/v3/order?"+query+"&signature="+sig, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	r.Header.Set("X-MBX-APIKEY", key)
	return r
}

// TestVerify_BodySignedPOST is the CCXT/SDK convention: a SIGNED POST carries
// its params (including timestamp) AND the signature in a form-urlencoded body
// with an EMPTY query string, signing Urlencode(params). Binance defines the
// signed material as totalParams = queryString + body, so the edge must verify
// over the body and read the timestamp from it. (Regression: this path returned
// -1022 then -1102 before signedPayload/checkTimestamp read the body.)
func TestVerify_BodySignedPOST(t *testing.T) {
	now := int64(1_700_000_000_000)
	a := NewAuthenticator(testAPIKey, testSecret, fixedClock(now))

	query := "symbol=BTCUSDT&side=BUY&type=LIMIT&timeInForce=GTC&quantity=0.01&price=40000&timestamp=1700000000000"
	sig := sign(testSecret, query)
	body := query + "&signature=" + sig

	r, err := http.NewRequest(http.MethodPost, "http://x/api/v3/order", strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	r.Header.Set("X-MBX-APIKEY", testAPIKey)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	if err := a.Verify(r); err != nil {
		t.Fatalf("body-signed POST should verify, got: %v", err)
	}
	// After Verify the params must be parseable by the handler. Verify reads the
	// body for signing/timestamp, but caches them in r.PostForm (via ParseForm),
	// so the handler's r.PostFormValue still returns the body params.
	if got := r.PostFormValue("symbol"); got != "BTCUSDT" {
		t.Errorf("symbol from body = %q, want BTCUSDT", got)
	}
	if got := r.PostFormValue("price"); got != "40000" {
		t.Errorf("price from body = %q, want 40000", got)
	}
}

// TestVerify_BodyTamperedRejected confirms a mutated body fails the signature.
func TestVerify_BodyTamperedRejected(t *testing.T) {
	now := int64(1_700_000_000_000)
	a := NewAuthenticator(testAPIKey, testSecret, fixedClock(now))

	query := "symbol=BTCUSDT&side=BUY&type=MARKET&quantity=0.01&timestamp=1700000000000"
	sig := sign(testSecret, query)
	// Tamper: bump the quantity after signing.
	body := strings.Replace(query, "quantity=0.01", "quantity=1.0", 1) + "&signature=" + sig

	r, _ := http.NewRequest(http.MethodPost, "http://x/api/v3/order", strings.NewReader(body))
	r.Header.Set("X-MBX-APIKEY", testAPIKey)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	if err := a.Verify(r); err == nil {
		t.Fatal("tampered body must fail signature verification")
	}
}

func TestVerify_ValidSignature(t *testing.T) {
	now := int64(1_700_000_000_000)
	a := NewAuthenticator(testAPIKey, testSecret, fixedClock(now))
	query := "symbol=BTCUSDT&side=BUY&type=LIMIT&quantity=1&price=100&timestamp=1700000000000"
	r := signedRequest(t, testAPIKey, testSecret, query)
	if err := a.Verify(r); err != nil {
		t.Fatalf("expected valid, got %v", err)
	}
}

func TestVerify_TamperedSignature(t *testing.T) {
	now := int64(1_700_000_000_000)
	a := NewAuthenticator(testAPIKey, testSecret, fixedClock(now))
	query := "symbol=BTCUSDT&timestamp=1700000000000"
	r, _ := http.NewRequest(http.MethodPost, "http://x/api/v3/order?"+query+"&signature=deadbeef", nil)
	r.Header.Set("X-MBX-APIKEY", testAPIKey)
	err := a.Verify(r)
	ae, ok := err.(*apiError)
	if !ok || ae.Code != codeInvalidSignature {
		t.Fatalf("expected -1022 signature error, got %v", err)
	}
}

func TestVerify_OldTimestamp(t *testing.T) {
	now := int64(1_700_000_000_000)
	a := NewAuthenticator(testAPIKey, testSecret, fixedClock(now))
	// timestamp 10s in the past, default recvWindow 5000ms.
	query := "symbol=BTCUSDT&timestamp=1699999990000"
	r := signedRequest(t, testAPIKey, testSecret, query)
	err := a.Verify(r)
	ae, ok := err.(*apiError)
	if !ok || ae.Code != codeInvalidTimestamp {
		t.Fatalf("expected -1021 timestamp error, got %v", err)
	}
}

func TestVerify_MissingTimestamp(t *testing.T) {
	now := int64(1_700_000_000_000)
	a := NewAuthenticator(testAPIKey, testSecret, fixedClock(now))
	query := "symbol=BTCUSDT"
	r := signedRequest(t, testAPIKey, testSecret, query)
	err := a.Verify(r)
	ae, ok := err.(*apiError)
	if !ok || ae.Code != codeMandatoryParam {
		t.Fatalf("expected -1102 mandatory param, got %v", err)
	}
}

func TestVerify_WrongAPIKey(t *testing.T) {
	now := int64(1_700_000_000_000)
	a := NewAuthenticator(testAPIKey, testSecret, fixedClock(now))
	query := "symbol=BTCUSDT&timestamp=1700000000000"
	r := signedRequest(t, "wrong-key", testSecret, query)
	err := a.Verify(r)
	ae, ok := err.(*apiError)
	if !ok || ae.Code != codeRejectedKey {
		t.Fatalf("expected -2015 rejected key, got %v", err)
	}
}

func TestVerify_MissingAPIKey(t *testing.T) {
	a := NewAuthenticator(testAPIKey, testSecret, fixedClock(1_700_000_000_000))
	query := "symbol=BTCUSDT&timestamp=1700000000000"
	sig := sign(testSecret, query)
	r, _ := http.NewRequest(http.MethodPost, "http://x/api/v3/order?"+query+"&signature="+sig, nil)
	err := a.Verify(r)
	ae, ok := err.(*apiError)
	if !ok || ae.Code != codeBadAPIKeyFmt {
		t.Fatalf("expected -2014 bad key format, got %v", err)
	}
}

func TestVerify_RespectsRawQueryOrder(t *testing.T) {
	// Signature must be computed over the literal on-wire string; a different
	// param order yields a different (and here, mismatched) signature.
	now := int64(1_700_000_000_000)
	a := NewAuthenticator(testAPIKey, testSecret, fixedClock(now))
	query := "timestamp=1700000000000&symbol=BTCUSDT&side=SELL"
	r := signedRequest(t, testAPIKey, testSecret, query)
	if err := a.Verify(r); err != nil {
		t.Fatalf("expected valid for literal order, got %v", err)
	}
}
