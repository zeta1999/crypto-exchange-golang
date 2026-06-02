package custody

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// httpStatusError carries a non-2xx HTTP status so callers can special-case it
// (e.g. Horizon 404 = unfunded account, not a real error).
type httpStatusError struct {
	status int
	body   string
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("http %d: %s", e.status, e.body)
}

// isHTTPStatus reports whether err is an httpStatusError with the given status.
func isHTTPStatus(err error, status int) bool {
	var he *httpStatusError
	return errors.As(err, &he) && he.status == status
}

// defaultHTTPClient is the shared client for faucet / RPC calls. A modest
// timeout keeps a stuck testnet endpoint from hanging the CLI.
func defaultHTTPClient() *http.Client {
	return &http.Client{Timeout: 30 * time.Second}
}

// maxRespBytes caps a response body read so a hostile/buggy endpoint cannot
// exhaust memory.
const maxRespBytes = 4 << 20

// httpGet performs a GET and returns the (capped) body, erroring on non-2xx.
func httpGet(ctx context.Context, hc *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRespBytes))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return body, &httpStatusError{status: resp.StatusCode, body: truncate(body, 200)}
	}
	return body, nil
}

// jsonRPCRequest is a JSON-RPC 2.0 request envelope.
type jsonRPCRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  []any  `json:"params"`
}

// jsonRPCResponse is a JSON-RPC 2.0 response envelope with a raw result.
type jsonRPCResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// jsonRPC posts a JSON-RPC 2.0 call and unmarshals the result into out.
func jsonRPC(ctx context.Context, hc *http.Client, endpoint, method string, params []any, out any) error {
	reqBody, err := json.Marshal(jsonRPCRequest{JSONRPC: "2.0", ID: 1, Method: method, Params: params})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(reqBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxRespBytes))
	if err != nil {
		return err
	}
	var env jsonRPCResponse
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("rpc %s: decode: %w (%s)", method, err, truncate(raw, 200))
	}
	if env.Error != nil {
		return fmt.Errorf("rpc %s: %d %s", method, env.Error.Code, env.Error.Message)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(env.Result, out)
}

func truncate(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n]) + "…"
	}
	return string(b)
}
