package coinbase

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// JWTVerifier verifies Coinbase Advanced Trade ES256 (ECDSA P-256) JWTs against
// a configured EC public key — the production auth scheme (CDP keys). A client
// signs a short-lived JWT with its EC private key; the emulator is configured
// with the matching public key (PEM, PKIX) and verifies the signature + claims.
//
// We verify: header alg == ES256, the ECDSA-P256 signature over the
// base64url(header).base64url(payload) signing input, and the exp/nbf time
// bounds (±leeway). If keyName is set, the token's sub (or header kid) must
// match it. We deliberately do NOT bind the `uri` claim to the request host:
// clients typically sign the real venue host (api.coinbase.com), not the
// emulator's address, so a host match would reject every real client; the
// signature already binds the token, and exp bounds replay.
type JWTVerifier struct {
	pub     *ecdsa.PublicKey
	keyName string // expected sub/kid; "" = don't check
	now     func() time.Time
	leeway  time.Duration
}

// NewJWTVerifier parses a PEM-encoded EC (P-256) public key and returns a
// verifier. keyName, when non-empty, is the expected JWT sub/kid. now may be nil
// (defaults to time.Now).
func NewJWTVerifier(pemPublicKey, keyName string, now func() time.Time) (*JWTVerifier, error) {
	block, _ := pem.Decode([]byte(pemPublicKey))
	if block == nil {
		return nil, fmt.Errorf("coinbase jwt: no PEM block in public key")
	}
	pubAny, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("coinbase jwt: parse public key: %w", err)
	}
	pub, ok := pubAny.(*ecdsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("coinbase jwt: public key is not ECDSA")
	}
	if pub.Curve != elliptic.P256() {
		return nil, fmt.Errorf("coinbase jwt: public key curve is %s, want P-256 (ES256)", pub.Curve.Params().Name)
	}
	if now == nil {
		now = time.Now
	}
	return &JWTVerifier{pub: pub, keyName: keyName, now: now, leeway: 30 * time.Second}, nil
}

// Verify checks a compact JWS (header.payload.signature). Returns nil iff the
// token is a well-formed ES256 JWT with a valid signature and unexpired claims.
func (v *JWTVerifier) Verify(token string) error {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return fmt.Errorf("jwt: expected 3 segments, got %d", len(parts))
	}
	b64 := base64.RawURLEncoding

	hdrBytes, err := b64.DecodeString(parts[0])
	if err != nil {
		return fmt.Errorf("jwt: bad header encoding: %w", err)
	}
	var hdr struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
		Typ string `json:"typ"`
	}
	if err := json.Unmarshal(hdrBytes, &hdr); err != nil {
		return fmt.Errorf("jwt: bad header json: %w", err)
	}
	if hdr.Alg != "ES256" {
		return fmt.Errorf("jwt: alg %q unsupported (want ES256)", hdr.Alg)
	}

	clmBytes, err := b64.DecodeString(parts[1])
	if err != nil {
		return fmt.Errorf("jwt: bad payload encoding: %w", err)
	}
	var clm struct {
		Sub string `json:"sub"`
		Iss string `json:"iss"`
		Exp int64  `json:"exp"`
		Nbf int64  `json:"nbf"`
	}
	if err := json.Unmarshal(clmBytes, &clm); err != nil {
		return fmt.Errorf("jwt: bad payload json: %w", err)
	}
	now := v.now()
	if clm.Exp != 0 && now.After(time.Unix(clm.Exp, 0).Add(v.leeway)) {
		return fmt.Errorf("jwt: token expired")
	}
	if clm.Nbf != 0 && now.Add(v.leeway).Before(time.Unix(clm.Nbf, 0)) {
		return fmt.Errorf("jwt: token not yet valid")
	}
	if v.keyName != "" && clm.Sub != v.keyName && hdr.Kid != v.keyName {
		return fmt.Errorf("jwt: subject/kid does not match configured key")
	}

	sig, err := b64.DecodeString(parts[2])
	if err != nil {
		return fmt.Errorf("jwt: bad signature encoding: %w", err)
	}
	// ES256 signature is the raw concatenation r||s, each a 32-byte big-endian
	// P-256 scalar (NOT ASN.1 DER).
	if len(sig) != 64 {
		return fmt.Errorf("jwt: ES256 signature must be 64 bytes, got %d", len(sig))
	}
	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:])
	h := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(v.pub, h[:], r, s) {
		return fmt.Errorf("jwt: signature verification failed")
	}
	return nil
}
