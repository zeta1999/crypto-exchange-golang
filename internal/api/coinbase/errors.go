package coinbase

import "net/http"

// apiError is a Coinbase-shaped error. Coinbase Advanced Trade returns
// {"error":"<CODE>","error_details":"...","message":"..."} with an HTTP status.
// It carries the status the edge should respond with (not serialised).
type apiError struct {
	Err     string `json:"error"`
	Details string `json:"error_details,omitempty"`
	Msg     string `json:"message"`
	status  int    // HTTP status, not serialised
}

func (e *apiError) Error() string { return e.Msg }

// HTTPStatus returns the HTTP status code associated with the error.
func (e *apiError) HTTPStatus() int {
	if e.status == 0 {
		return http.StatusBadRequest
	}
	return e.status
}

// Coinbase Advanced Trade error codes used by this subset. These mirror the
// strings Coinbase returns in the "error" field; consumers branch on them.
const (
	errUnauthorized      = "unauthorized"
	errInvalidRequest    = "INVALID_ARGUMENT"
	errUnknownProduct    = "INVALID_PRODUCT_ID"
	errInternal          = "INTERNAL"
	errUnknownCancel     = "UNKNOWN_CANCEL_ORDER"
	errOrderNotFound     = "UNKNOWN_ORDER"
	errInvalidOrderType  = "INVALID_ORDER_TYPE"
	errInvalidSideString = "INVALID_SIDE"
	errRateLimited       = "rate_limit_exceeded"
)

func errUnauthorizedf(msg string) *apiError {
	return &apiError{Err: errUnauthorized, Msg: msg, status: http.StatusUnauthorized}
}

func errInvalidArgument(msg string) *apiError {
	return &apiError{Err: errInvalidRequest, Msg: msg, status: http.StatusBadRequest}
}

func errInvalidProduct(productID string) *apiError {
	return &apiError{Err: errUnknownProduct, Msg: "Unknown product_id: " + productID, status: http.StatusBadRequest}
}

func errInternalf(msg string) *apiError {
	return &apiError{Err: errInternal, Msg: msg, status: http.StatusInternalServerError}
}

// errRateLimitedf is the Coinbase rate-limit error with HTTP 429.
func errRateLimitedf() *apiError {
	return &apiError{Err: errRateLimited, Msg: "rate limit exceeded", status: http.StatusTooManyRequests}
}
