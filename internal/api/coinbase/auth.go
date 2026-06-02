package coinbase

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// authWindow is how far a request's CB-ACCESS-TIMESTAMP may be from server time
// (in seconds) before it is rejected. Coinbase Exchange uses ~30s; we apply it
// symmetrically around the injected clock.
const authWindow = 30 * time.Second

// Authenticator verifies Coinbase SIGNED requests using the legacy Coinbase
// Exchange HMAC-SHA256 scheme:
//
//	sign = base64(HMAC_SHA256(timestamp + method + requestPath + body, secret))
//
// where the message timestamp is the CB-ACCESS-TIMESTAMP header, method is the
// uppercase HTTP verb, requestPath is the path plus any query string exactly as
// signed, and body is the raw request body (empty for GETs). The API key
// travels in CB-ACCESS-KEY and the signature in CB-ACCESS-SIGN. An optional
// passphrase travels in CB-ACCESS-PASSPHRASE.
//
// Per Coinbase convention the secret is base64-encoded; for the emulator we try
// base64-decoding it and fall back to the raw bytes if that fails, so a plain
// configured secret also works.
//
// Production Advanced Trade JWT (ES256) auth is also supported when a JWT
// verifier is configured (see WithJWT): a request bearing an Authorization:
// Bearer <jwt> header is verified against the configured EC public key instead
// of the HMAC scheme. Clients thus authenticate with either scheme.
//
// The clock is injected (now) so verification is deterministic in tests.
type Authenticator struct {
	apiKey     []byte
	secret     []byte
	passphrase string
	now        func() time.Time
	jwt        *JWTVerifier // optional ES256 JWT verifier (nil = HMAC only)
}

// WithJWT attaches an ES256 JWT verifier; a Bearer token is then accepted in
// place of the CB-ACCESS HMAC headers.
func (a *Authenticator) WithJWT(v *JWTVerifier) *Authenticator {
	a.jwt = v
	return a
}

// VerifyJWT reports whether a raw JWT (e.g. from a WS subscribe message) is
// valid. ok=false if no JWT verifier is configured.
func (a *Authenticator) VerifyJWT(token string) (ok bool, err error) {
	if a.jwt == nil {
		return false, nil
	}
	return true, a.jwt.Verify(token)
}

// NewAuthenticator returns an Authenticator for the given key/secret/passphrase.
// passphrase may be empty (then it is not enforced). now may be nil, in which
// case time.Now is used.
func NewAuthenticator(apiKey, secret, passphrase string, now func() time.Time) *Authenticator {
	if now == nil {
		now = time.Now
	}
	return &Authenticator{
		apiKey:     []byte(apiKey),
		secret:     decodeSecret(secret),
		passphrase: passphrase,
		now:        now,
	}
}

// APIKeyString returns the configured API key as a string. The WS user channel
// uses it for its simplified credential check (see ws.go authWS).
func (a *Authenticator) APIKeyString() string { return string(a.apiKey) }

// decodeSecret returns the HMAC key bytes for a configured secret: the
// base64-decoded form if it decodes cleanly (Coinbase convention), else the raw
// bytes (emulator convenience).
func decodeSecret(secret string) []byte {
	if b, err := base64.StdEncoding.DecodeString(secret); err == nil && len(b) > 0 {
		return b
	}
	return []byte(secret)
}

// Verify validates a SIGNED request:
//   - CB-ACCESS-KEY is present and matches the configured key,
//   - CB-ACCESS-SIGN equals base64(HMAC_SHA256(ts+method+path+body, secret)),
//   - CB-ACCESS-TIMESTAMP is present, numeric, and within authWindow of now,
//   - CB-ACCESS-PASSPHRASE matches when a passphrase is configured.
//
// requestPath is taken from the request URI (path plus raw query) so it matches
// exactly what the client signed. body is the raw request body; callers that
// need the body afterwards must restore r.Body (the handlers re-read via a
// buffered copy).
func (a *Authenticator) Verify(r *http.Request, body []byte) error {
	// Advanced Trade JWT (ES256): a Bearer token is verified against the
	// configured EC public key instead of the HMAC headers. A verification
	// failure is an auth error (401), not an internal error (500).
	if a.jwt != nil {
		if tok, ok := bearerToken(r); ok {
			if err := a.jwt.Verify(tok); err != nil {
				return errUnauthorizedf("invalid jwt: " + err.Error())
			}
			return nil
		}
	}

	key := r.Header.Get("CB-ACCESS-KEY")
	if key == "" {
		return errUnauthorizedf("missing CB-ACCESS-KEY")
	}
	if subtle.ConstantTimeCompare([]byte(key), a.apiKey) != 1 {
		return errUnauthorizedf("invalid api key")
	}

	tsStr := r.Header.Get("CB-ACCESS-TIMESTAMP")
	if tsStr == "" {
		return errUnauthorizedf("missing CB-ACCESS-TIMESTAMP")
	}
	if err := a.checkTimestamp(tsStr); err != nil {
		return err
	}

	if a.passphrase != "" {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("CB-ACCESS-PASSPHRASE")), []byte(a.passphrase)) != 1 {
			return errUnauthorizedf("invalid passphrase")
		}
	}

	sig := r.Header.Get("CB-ACCESS-SIGN")
	if sig == "" {
		return errUnauthorizedf("missing CB-ACCESS-SIGN")
	}
	expected := a.sign(tsStr, r.Method, requestPath(r), body)
	if subtle.ConstantTimeCompare([]byte(sig), []byte(expected)) != 1 {
		return errUnauthorizedf("invalid signature")
	}
	return nil
}

// sign computes base64(HMAC_SHA256(timestamp+method+requestPath+body, secret)).
func (a *Authenticator) sign(timestamp, method, path string, body []byte) string {
	mac := hmac.New(sha256.New, a.secret)
	mac.Write([]byte(timestamp))
	mac.Write([]byte(method))
	mac.Write([]byte(path))
	mac.Write(body)
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// checkTimestamp enforces presence, numeric form, and the ±authWindow bound.
// Coinbase uses epoch seconds (it also accepts fractional seconds; we parse the
// integer second part).
func (a *Authenticator) checkTimestamp(tsStr string) error {
	tsSec, err := strconv.ParseFloat(tsStr, 64)
	if err != nil {
		return errUnauthorizedf("invalid timestamp")
	}
	reqTime := time.Unix(int64(tsSec), 0)
	diff := a.now().Sub(reqTime)
	if diff < 0 {
		diff = -diff
	}
	if diff > authWindow {
		return errUnauthorizedf("request timestamp expired")
	}
	return nil
}

// bearerToken extracts a JWT from an "Authorization: Bearer <token>" header.
func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return strings.TrimSpace(h[len(prefix):]), true
	}
	return "", false
}

// requestPath returns the path plus raw query string exactly as it appears on
// the wire, which is what Coinbase clients sign for GETs (the query is part of
// the signed path).
func requestPath(r *http.Request) string {
	p := r.URL.EscapedPath()
	if r.URL.RawQuery != "" {
		p += "?" + r.URL.RawQuery
	}
	return p
}
