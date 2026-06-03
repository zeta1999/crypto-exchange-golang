package custody

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
)

// circleFaucetURL is Circle's testnet faucet drip endpoint. It requires a
// registered Circle API key (CIRCLE_API_KEY); the keyless faucet is the web
// page at faucet.circle.com.
const circleFaucetURL = "https://api.circle.com/v1/faucet/drips"

// circleManualURL is the human (captcha) faucet for USDC across testnets.
const circleManualURL = "https://faucet.circle.com/"

// circleAPIKey returns the configured Circle API key, if any.
func circleAPIKey() string { return os.Getenv("CIRCLE_API_KEY") }

// envOr returns the environment value for key, or def if unset/empty.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// circleDrip requests testnet USDC for address on the given Circle blockchain
// id (e.g. "XLM-TESTNET", "SOL-DEVNET", "ETH-SEPOLIA"). Returns ErrManualFaucet
// when no API key is set so the caller can fall back to the web faucet. The
// blockchain enum follows Circle's faucet API; if a value is rejected, Circle's
// error (listing valid values) is surfaced.
func circleDrip(ctx context.Context, hc *http.Client, blockchain, address string) (string, error) {
	key := circleAPIKey()
	if key == "" {
		return "", ErrManualFaucet
	}
	body, _ := json.Marshal(map[string]any{
		"address":    address,
		"blockchain": blockchain,
		"native":     false,
		"usdc":       true,
		"eurc":       false,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, circleFaucetURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxRespBytes))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("circle faucet http %d: %s", resp.StatusCode, truncate(respBody, 300))
	}
	// Circle returns a drip acknowledgement; surface a short ref if present.
	var ack struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	_ = json.Unmarshal(respBody, &ack)
	if ack.Data.ID != "" {
		return ack.Data.ID, nil
	}
	return "requested", nil
}
