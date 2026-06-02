package coinbase

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"testing"
	"time"
)

// signES256 builds a compact ES256 JWT (raw r||s signature) for tests.
func signES256(t *testing.T, key *ecdsa.PrivateKey, hdr, claims map[string]any) string {
	t.Helper()
	b64 := base64.RawURLEncoding
	h, _ := json.Marshal(hdr)
	c, _ := json.Marshal(claims)
	signingInput := b64.EncodeToString(h) + "." + b64.EncodeToString(c)
	sum := sha256.Sum256([]byte(signingInput))
	r, s, err := ecdsa.Sign(rand.Reader, key, sum[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	// 32-byte big-endian r||s.
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return signingInput + "." + b64.EncodeToString(sig)
}

func pubPEM(t *testing.T, key *ecdsa.PrivateKey) string {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

func newTestVerifier(t *testing.T, key *ecdsa.PrivateKey, keyName string, now time.Time) *JWTVerifier {
	t.Helper()
	v, err := NewJWTVerifier(pubPEM(t, key), keyName, func() time.Time { return now })
	if err != nil {
		t.Fatalf("NewJWTVerifier: %v", err)
	}
	return v
}

func TestJWTVerifyValid(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	now := time.Unix(1_700_000_000, 0)
	v := newTestVerifier(t, key, "my-key", now)
	tok := signES256(t, key,
		map[string]any{"alg": "ES256", "typ": "JWT", "kid": "my-key"},
		map[string]any{"sub": "my-key", "iss": "cdp", "nbf": now.Unix(), "exp": now.Add(2 * time.Minute).Unix()})
	if err := v.Verify(tok); err != nil {
		t.Errorf("valid token rejected: %v", err)
	}
}

func TestJWTVerifyRejects(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	now := time.Unix(1_700_000_000, 0)
	v := newTestVerifier(t, key, "my-key", now)

	good := func() string {
		return signES256(t, key,
			map[string]any{"alg": "ES256", "typ": "JWT", "kid": "my-key"},
			map[string]any{"sub": "my-key", "exp": now.Add(2 * time.Minute).Unix()})
	}

	cases := []struct {
		name string
		tok  string
	}{
		{"wrong key", signES256(t, other,
			map[string]any{"alg": "ES256", "typ": "JWT"},
			map[string]any{"sub": "my-key", "exp": now.Add(time.Minute).Unix()})},
		{"expired", signES256(t, key,
			map[string]any{"alg": "ES256", "typ": "JWT"},
			map[string]any{"sub": "my-key", "exp": now.Add(-time.Hour).Unix()})},
		{"not yet valid", signES256(t, key,
			map[string]any{"alg": "ES256", "typ": "JWT"},
			map[string]any{"sub": "my-key", "nbf": now.Add(time.Hour).Unix(), "exp": now.Add(2 * time.Hour).Unix()})},
		{"wrong alg", signES256(t, key,
			map[string]any{"alg": "HS256", "typ": "JWT"},
			map[string]any{"sub": "my-key", "exp": now.Add(time.Minute).Unix()})},
		{"wrong subject", signES256(t, key,
			map[string]any{"alg": "ES256", "typ": "JWT"},
			map[string]any{"sub": "someone-else", "exp": now.Add(time.Minute).Unix()})},
		{"not three segments", "a.b"},
		{"garbage", "not-a-jwt"},
	}
	for _, c := range cases {
		if err := v.Verify(c.tok); err == nil {
			t.Errorf("%s: expected rejection, got nil", c.name)
		}
	}

	// Tampered payload (re-base64 a mutated claim, keep the original signature).
	tok := good()
	parts := splitDots(tok)
	clm, _ := base64.RawURLEncoding.DecodeString(parts[1])
	tampered := base64.RawURLEncoding.EncodeToString(append(clm, ' ')) // any change
	if err := v.Verify(parts[0] + "." + tampered + "." + parts[2]); err == nil {
		t.Error("tampered payload should fail signature check")
	}
}

func TestJWTVerifierRejectsNonP256Key(t *testing.T) {
	k384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	der, _ := x509.MarshalPKIXPublicKey(&k384.PublicKey)
	p := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	if _, err := NewJWTVerifier(p, "", nil); err == nil {
		t.Error("P-384 key should be rejected (ES256 requires P-256)")
	}
	if _, err := NewJWTVerifier("not pem", "", nil); err == nil {
		t.Error("non-PEM should be rejected")
	}
}

func splitDots(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

// TestAuthenticatorBearerJWT covers the Verify() wiring: a Bearer JWT
// authenticates when a verifier is configured; HMAC still works; bad/expired
// Bearer is rejected.
func TestAuthenticatorBearerJWT(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	now := time.Unix(1_700_000_000, 0)
	authn := NewAuthenticator("hmac-key", "hmac-secret", "", func() time.Time { return now })
	authn.WithJWT(newTestVerifier(t, key, "", now))

	mk := func(exp time.Time) string {
		return signES256(t, key,
			map[string]any{"alg": "ES256", "typ": "JWT"},
			map[string]any{"sub": "x", "exp": exp.Unix()})
	}
	req := func(bearer string) *http.Request {
		r, _ := http.NewRequest("POST", "/api/v3/brokerage/orders", nil)
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		return r
	}

	if err := authn.Verify(req(mk(now.Add(time.Minute))), nil); err != nil {
		t.Errorf("valid bearer jwt rejected: %v", err)
	}
	if err := authn.Verify(req(mk(now.Add(-time.Hour))), nil); err == nil {
		t.Error("expired bearer jwt should be rejected")
	}
	// No Bearer + no HMAC headers → falls through to HMAC path → missing key.
	if err := authn.Verify(req(""), nil); err == nil {
		t.Error("unauthenticated request should be rejected")
	}
}
