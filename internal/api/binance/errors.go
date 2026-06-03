package binance

import "net/http"

// apiError is a Binance-shaped error. It serialises to {"code":-XXXX,"msg":...}
// and carries the HTTP status the edge should respond with.
type apiError struct {
	Code   int    `json:"code"`
	Msg    string `json:"msg"`
	status int    // HTTP status, not serialised
}

func (e *apiError) Error() string { return e.Msg }

// HTTPStatus returns the HTTP status code associated with the error.
func (e *apiError) HTTPStatus() int {
	if e.status == 0 {
		return http.StatusBadRequest
	}
	return e.status
}

// Binance error code constants used by this subset. See
// https://binance-docs.github.io/apidocs/spot/en/#error-codes.
const (
	codeUnknown          = -1000
	codeMandatoryParam   = -1102 // a mandatory parameter was empty/malformed
	codeIllegalParam     = -1100 // illegal characters / bad parameter
	codeInvalidTimestamp = -1021 // timestamp outside recvWindow
	codeInvalidSignature = -1022 // signature for this request is not valid
	codeTooManyRequests  = -1003 // too many requests; rate limited
	codeInvalidSymbol    = -1121 // invalid symbol
	codeUnknownOrder     = -2011 // CANCEL_REJECTED / unknown order
	codeBadAPIKeyFmt     = -2014 // API-key format invalid
	codeRejectedKey      = -2015 // invalid API-key, IP, or permissions
)

func errMandatoryParam(name string) *apiError {
	return &apiError{Code: codeMandatoryParam, Msg: "Mandatory parameter '" + name + "' was not sent, was empty/null, or malformed.", status: http.StatusBadRequest}
}

func errIllegalParam(name string) *apiError {
	return &apiError{Code: codeIllegalParam, Msg: "Illegal characters found in parameter '" + name + "'.", status: http.StatusBadRequest}
}

func errInvalidSymbol() *apiError {
	return &apiError{Code: codeInvalidSymbol, Msg: "Invalid symbol.", status: http.StatusBadRequest}
}

func errUnknownOrder() *apiError {
	return &apiError{Code: codeUnknownOrder, Msg: "Unknown order sent.", status: http.StatusBadRequest}
}

func errInternal(msg string) *apiError {
	return &apiError{Code: codeUnknown, Msg: msg, status: http.StatusBadRequest}
}

// errInsufficientBalance is Binance -2010 (order rejected: not enough balance).
func errInsufficientBalance() *apiError {
	return &apiError{Code: -2010, Msg: "Account has insufficient balance for requested action.", status: http.StatusBadRequest}
}

// errDuplicateOrder is Binance -2010 with the "Duplicate order sent." message,
// returned when a place reuses a still-known newClientOrderId.
func errDuplicateOrder() *apiError {
	return &apiError{Code: -2010, Msg: "Duplicate order sent.", status: http.StatusBadRequest}
}

// errTooManyRequests is the Binance rate-limit error (-1003) with HTTP 429.
func errTooManyRequests() *apiError {
	return &apiError{Code: codeTooManyRequests, Msg: "Too many requests.", status: http.StatusTooManyRequests}
}
