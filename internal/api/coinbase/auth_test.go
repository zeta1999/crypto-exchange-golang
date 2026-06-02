package coinbase

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	testAPIKey     = "test-key"
	testSecret     = "test-secret"
	testPassphrase = "test-pass"
)

func fixedClock(unixSec int64) func() time.Time {
	t := time.Unix(unixSec, 0).UTC()
	return func() time.Time { return t }
}

// signMessage computes base64(HMAC_SHA256(ts+method+path+body, raw-secret)).
func signMessage(secret, ts, method, path, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + method + path + body))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// signedRequest builds a SIGNED request with valid CB-ACCESS-* headers.
func signedRequest(t *testing.T, key, secret, pass string, ts int64, method, path, body string) *http.Request {
	t.Helper()
	tsStr := strconv.FormatInt(ts, 10)
	r, err := http.NewRequest(method, "http://x"+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	r.Header.Set("CB-ACCESS-KEY", key)
	r.Header.Set("CB-ACCESS-TIMESTAMP", tsStr)
	r.Header.Set("CB-ACCESS-SIGN", signMessage(secret, tsStr, method, path, body))
	if pass != "" {
		r.Header.Set("CB-ACCESS-PASSPHRASE", pass)
	}
	return r
}

func TestVerify_ValidSignature(t *testing.T) {
	now := int64(1_700_000_000)
	a := NewAuthenticator(testAPIKey, testSecret, testPassphrase, fixedClock(now))
	body := `{"product_id":"BTC-USD"}`
	r := signedRequest(t, testAPIKey, testSecret, testPassphrase, now, http.MethodPost, "/api/v3/brokerage/orders", body)
	if err := a.Verify(r, []byte(body)); err != nil {
		t.Fatalf("expected valid, got %v", err)
	}
}

func TestVerify_TamperedSignature(t *testing.T) {
	now := int64(1_700_000_000)
	a := NewAuthenticator(testAPIKey, testSecret, testPassphrase, fixedClock(now))
	body := `{"product_id":"BTC-USD"}`
	r := signedRequest(t, testAPIKey, testSecret, testPassphrase, now, http.MethodPost, "/api/v3/brokerage/orders", body)
	r.Header.Set("CB-ACCESS-SIGN", "ZGVhZGJlZWY=") // base64("deadbeef")
	err := a.Verify(r, []byte(body))
	ae, ok := err.(*apiError)
	if !ok || ae.Err != errUnauthorized || ae.HTTPStatus() != http.StatusUnauthorized {
		t.Fatalf("expected unauthorized, got %v", err)
	}
}

func TestVerify_StaleTimestamp(t *testing.T) {
	now := int64(1_700_000_000)
	a := NewAuthenticator(testAPIKey, testSecret, testPassphrase, fixedClock(now))
	body := ""
	// 60s in the past, beyond the ±30s window.
	r := signedRequest(t, testAPIKey, testSecret, testPassphrase, now-60, http.MethodGet, "/api/v3/brokerage/accounts", body)
	err := a.Verify(r, []byte(body))
	ae, ok := err.(*apiError)
	if !ok || ae.Err != errUnauthorized {
		t.Fatalf("expected unauthorized for stale ts, got %v", err)
	}
	if !strings.Contains(ae.Msg, "expired") {
		t.Fatalf("expected expiry message, got %q", ae.Msg)
	}
}

func TestVerify_WrongKey(t *testing.T) {
	now := int64(1_700_000_000)
	a := NewAuthenticator(testAPIKey, testSecret, testPassphrase, fixedClock(now))
	body := ""
	r := signedRequest(t, "wrong-key", testSecret, testPassphrase, now, http.MethodGet, "/api/v3/brokerage/accounts", body)
	err := a.Verify(r, []byte(body))
	ae, ok := err.(*apiError)
	if !ok || ae.Err != errUnauthorized {
		t.Fatalf("expected unauthorized for wrong key, got %v", err)
	}
}

func TestVerify_MissingKey(t *testing.T) {
	a := NewAuthenticator(testAPIKey, testSecret, "", fixedClock(1_700_000_000))
	r, _ := http.NewRequest(http.MethodGet, "http://x/api/v3/brokerage/accounts", nil)
	err := a.Verify(r, nil)
	if ae, ok := err.(*apiError); !ok || ae.Err != errUnauthorized {
		t.Fatalf("expected unauthorized for missing key, got %v", err)
	}
}

func TestVerify_WrongPassphrase(t *testing.T) {
	now := int64(1_700_000_000)
	a := NewAuthenticator(testAPIKey, testSecret, testPassphrase, fixedClock(now))
	body := ""
	r := signedRequest(t, testAPIKey, testSecret, "bad-pass", now, http.MethodGet, "/api/v3/brokerage/accounts", body)
	err := a.Verify(r, []byte(body))
	if ae, ok := err.(*apiError); !ok || ae.Err != errUnauthorized {
		t.Fatalf("expected unauthorized for wrong passphrase, got %v", err)
	}
}

func TestVerify_NoPassphraseConfigured(t *testing.T) {
	// When no passphrase is configured, a missing CB-ACCESS-PASSPHRASE is fine.
	now := int64(1_700_000_000)
	a := NewAuthenticator(testAPIKey, testSecret, "", fixedClock(now))
	body := ""
	r := signedRequest(t, testAPIKey, testSecret, "", now, http.MethodGet, "/api/v3/brokerage/accounts", body)
	if err := a.Verify(r, []byte(body)); err != nil {
		t.Fatalf("expected valid with no passphrase, got %v", err)
	}
}

func TestVerify_Base64Secret(t *testing.T) {
	// A base64-encoded secret must be decoded before use as the HMAC key.
	now := int64(1_700_000_000)
	rawKey := "super-secret-bytes"
	b64 := base64.StdEncoding.EncodeToString([]byte(rawKey))
	a := NewAuthenticator(testAPIKey, b64, "", fixedClock(now))
	body := ""
	tsStr := strconv.FormatInt(now, 10)
	path := "/api/v3/brokerage/accounts"
	r, _ := http.NewRequest(http.MethodGet, "http://x"+path, nil)
	r.Header.Set("CB-ACCESS-KEY", testAPIKey)
	r.Header.Set("CB-ACCESS-TIMESTAMP", tsStr)
	r.Header.Set("CB-ACCESS-SIGN", signMessage(rawKey, tsStr, http.MethodGet, path, body))
	if err := a.Verify(r, []byte(body)); err != nil {
		t.Fatalf("expected valid with base64 secret, got %v", err)
	}
}

func TestVerify_PathIncludesQuery(t *testing.T) {
	// For GETs the signed requestPath includes the query string exactly.
	now := int64(1_700_000_000)
	a := NewAuthenticator(testAPIKey, testSecret, "", fixedClock(now))
	body := ""
	path := "/api/v3/brokerage/orders/historical/batch?product_id=BTC-USD&order_status=OPEN"
	r := signedRequest(t, testAPIKey, testSecret, "", now, http.MethodGet, path, body)
	if err := a.Verify(r, []byte(body)); err != nil {
		t.Fatalf("expected valid with query in path, got %v", err)
	}
	// A signature computed over the path WITHOUT the query must fail.
	tsStr := strconv.FormatInt(now, 10)
	r2 := signedRequest(t, testAPIKey, testSecret, "", now, http.MethodGet, path, body)
	r2.Header.Set("CB-ACCESS-SIGN", signMessage(testSecret, tsStr, http.MethodGet, "/api/v3/brokerage/orders/historical/batch", body))
	if err := a.Verify(r2, []byte(body)); err == nil {
		t.Fatalf("expected failure when query omitted from signed path")
	}
}
