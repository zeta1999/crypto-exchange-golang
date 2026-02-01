package auth

import "errors"

var ErrUnauthorized = errors.New("unauthorized")

// TokenValidator authenticates requests using a shared secret.
type TokenValidator struct {
	token string
}

func NewTokenValidator(token string) *TokenValidator {
	return &TokenValidator{token: token}
}

// Validate ensures the provided token matches the configured secret.
func (t *TokenValidator) Validate(presented string) error {
	if presented == "" || presented != t.token {
		return ErrUnauthorized
	}
	return nil
}
