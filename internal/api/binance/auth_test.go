package binance

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
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
