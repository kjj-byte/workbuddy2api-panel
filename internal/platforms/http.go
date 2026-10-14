package platforms

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
)

func identifier() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
func str(v any) string { s, _ := v.(string); return s }
func number(v any) (float64, bool) {
	var n float64
	var err error
	switch x := v.(type) {
	case float64:
		n = x
	case string:
		n, err = strconv.ParseFloat(x, 64)
	case json.Number:
		n, err = x.Float64()
	case int:
		n = float64(x)
	case int64:
		n = float64(x)
	default:
		return 0, false
	}
	return n, err == nil && !math.IsNaN(n) && !math.IsInf(n, 0)
}
func obj(v any) map[string]any {
	m, _ := v.(map[string]any)
	if m == nil {
		return map[string]any{}
	}
	return m
}
func list(v any) []any { s, _ := v.([]any); return s }
func bearer(c Credential) http.Header {
	h := make(http.Header)
	h.Set("Authorization", "Bearer "+str(c["access_token"]))
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "application/json")
	return h
}
func (n network) request(ctx context.Context, method, url string, headers http.Header, body []byte) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header = headers
	resp, err := n.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	var data map[string]any
	var decoded any
	if err = json.Unmarshal(raw, &decoded); err != nil {
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, &RemoteError{fmt.Sprintf("platform HTTP %d", resp.StatusCode), resp.StatusCode}
		}
		return nil, fmt.Errorf("platform returned invalid JSON")
	}
	if rows, ok := decoded.([]any); ok {
		data = map[string]any{"data": rows}
	} else {
		data = obj(decoded)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if !(resp.StatusCode == 400 && data["error"] != nil && strings.HasSuffix(req.URL.Path, "/oauth2/token")) {
			return nil, &RemoteError{fmt.Sprintf("platform HTTP %d", resp.StatusCode), resp.StatusCode}
		}
	}
	return data, nil
}
func jsonBytes(v any) []byte { raw, _ := json.Marshal(v); return raw }
func emitChunk(receive func(json.RawMessage) error, v any) error {
	return receive(json.RawMessage(jsonBytes(v)))
}
func validateCredential(c Credential) error {
	if str(c["access_token"]) == "" {
		return fmt.Errorf("access_token is required")
	}
	return nil
}
func validatePlatformCredential(provider string, c Credential) error {
	if provider == "codearts" {
		for _, key := range []string{"access_key_id", "secret_access_key", "security_token"} {
			if str(c[key]) == "" {
				return fmt.Errorf("CodeArts 缺少 %s", key)
			}
		}
		return nil
	}
	return validateCredential(c)
}
