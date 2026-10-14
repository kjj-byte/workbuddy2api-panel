package platforms

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func callbackGet(t *testing.T, address string) int {
	t.Helper()
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(address)
	if err != nil {
		t.Fatal("callback connection failed:", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}
func TestLobsterBrowserLoginWritesAccountWithoutExposingToken(t *testing.T) {
	exchanges := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/version" {
			replyJSON(w, map[string]any{"data": map[string]any{"value": map[string]any{"version": "2026.10.1"}}})
			return
		}
		if r.URL.Path != "/api/auth/exchange" {
			t.Error("wrong exchange endpoint")
		}
		if r.Header.Get("Authorization") != "" {
			t.Error("code exchange must be anonymous")
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["authCode"] != "fake-code" || str(body["uuid"]) == "" || str(body["firstKeyfrom"]) == "" || str(body["latestKeyfrom"]) == "" {
			t.Error("missing code exchange identity")
		}
		exchanges++
		replyJSON(w, map[string]any{"code": 0, "data": map[string]any{"accessToken": "TEST-SECRET-ONLY", "refreshToken": "fake-refresh", "expiresIn": 3600, "user": map[string]any{"id": "lobster-user", "nickname": "龙虾测试"}}})
	}))
	defer server.Close()
	c, err := newClient(context.Background(), Config{DataDir: t.TempDir()}, []Adapter{localLobster(server)})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	raw, err := c.RPC(context.Background(), "account.create", map[string]any{"provider": "lobsterai"})
	if err != nil {
		t.Fatal(err)
	}
	var envelope map[string]any
	json.Unmarshal(raw, &envelope)
	value := obj(envelope["value"])
	login, _ := url.Parse(str(value["loginUrl"]))
	fragment, _ := url.Parse("https://local/" + login.Fragment)
	q := fragment.Query()
	callback := q.Get("redirect_uri")
	if login.Host != "lobsterai.youdao.com" || value["loginMode"] != "browser" {
		t.Fatal("wrong portal URL")
	}
	if status := callbackGet(t, callback+"?code=fake-code&state=wrong"); status != 400 {
		t.Fatal("wrong state accepted")
	}
	if exchanges != 0 {
		t.Fatal("invalid callback exchanged a code")
	}
	if status := callbackGet(t, callback+"?"+url.Values{"code": {"fake-code"}, "state": {q.Get("state")}}.Encode()); status != 200 {
		t.Fatal("valid callback failed")
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(c.store.snapshot().Accounts) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	accounts := c.store.snapshot().Accounts
	if len(accounts) != 1 || accounts[0].Nickname != "龙虾测试" || str(accounts[0].Credential["uuid"]) == "" {
		t.Fatal("login did not persist the account")
	}
	raw, err = c.RPC(context.Background(), "account.list", map[string]any{"provider": "lobsterai"})
	if err != nil || strings.Contains(string(raw), "TEST-SECRET-ONLY") {
		t.Fatal("account list leaked credential")
	}
}
func TestCodeArtsBrowserPKCEAndDPoP(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	challenge, redirect := "", ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("code") != "fake-code" || r.Form.Get("redirect_uri") != redirect {
			t.Error("wrong auth-code exchange")
		}
		hash := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if base64.RawURLEncoding.EncodeToString(hash[:]) != challenge {
			t.Error("PKCE verifier does not match login challenge")
		}
		parts := strings.Split(r.Header.Get("DPoP"), ".")
		if len(parts) != 3 {
			t.Fatal("missing DPoP proof")
		}
		head, _ := base64.RawURLEncoding.DecodeString(parts[0])
		payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
		sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
		var header, claims map[string]any
		json.Unmarshal(head, &header)
		json.Unmarshal(payload, &claims)
		jwk := obj(header["jwk"])
		if jwk["d"] != nil || header["typ"] != "dpop+jwt" || claims["htu"] != "http://"+r.Host+r.URL.Path || claims["htm"] != "POST" {
			t.Error("invalid proof header or target")
		}
		x, _ := base64.RawURLEncoding.DecodeString(str(jwk["x"]))
		y, _ := base64.RawURLEncoding.DecodeString(str(jwk["y"]))
		digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
		if len(sig) != 64 || !ecdsa.Verify(&ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}, digest[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
			t.Error("invalid DPoP signature")
		}
		replyJSON(w, map[string]any{"credentials": map[string]any{"access_key_id": "fake-ak", "secret_access_key": "fake-sk", "security_token": "fake-st", "expiration": "2099-01-01T00:00:00Z"}, "refresh_token": "fake-refresh"})
	}))
	defer server.Close()
	p := NewCodeArts()
	p.auth = server.URL + "/tokens"
	p.http = server.Client()
	auth, err := p.BeginBrowserLogin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	login, _ := url.Parse(auth.URL)
	q := login.Query()
	challenge = q.Get("code_challenge")
	redirect = "http://127.0.0.1:" + q.Get("port") + "/oauth/callback"
	if q.Get("code_challenge_method") != "SHA-256" || q.Get("plugin-name") != "snap_AIIDE" || q.Get("auth_callback_url") != "" || login.Host != "codearts.huaweicloud.com" {
		t.Fatal("invalid CodeArts portal parameters")
	}
	if status := callbackGet(t, redirect+"?code=fake-code"); status != 200 {
		t.Fatal("callback failed")
	}
	result := <-auth.Result
	if result.Err != nil || str(result.Credential["refresh_token"]) == "" || str(result.Credential["code_verifier"]) == "" || obj(result.Credential["dpop_private_key_jwk"])["d"] == nil {
		t.Fatal("refresh material was not preserved")
	}
}
func TestCodeArtsBrowserTicketFallback(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("secret") != "fake-secret" || r.Header.Get("Plugin-Name") != "snap_AIIDE" {
			t.Error("wrong ticket lookup")
		}
		replyJSON(w, map[string]any{"credential": map[string]any{"access": "fake-ak", "secret": "fake-sk", "securityToken": "fake-st"}})
	}))
	defer server.Close()
	p := NewCodeArts()
	p.api = server.URL
	p.http = server.Client()
	auth, err := p.BeginBrowserLogin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	login, _ := url.Parse(auth.URL)
	callback := "http://127.0.0.1:" + login.Query().Get("port") + "/oauth/callback?secret=fake-secret"
	if callbackGet(t, callback) != 200 {
		t.Fatal("ticket callback failed")
	}
	result := <-auth.Result
	if result.Err != nil || validatePlatformCredential("codearts", result.Credential) != nil {
		t.Fatal("ticket credentials rejected")
	}
}
func TestTraeBrowserLoginExchangesAndPersistsDevice(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	exchanges := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "ExchangeToken") {
			exchanges++
			var b map[string]any
			json.NewDecoder(r.Body).Decode(&b)
			if b["ClientID"] != "en1oxy7wnw8j9n" || b["RefreshToken"] != "fake-refresh" {
				t.Error("incorrect TRAE exchange")
			}
			replyJSON(w, map[string]any{"Result": map[string]any{"Token": "fake-access", "RefreshToken": "rotated-refresh", "TokenExpireAt": time.Now().Add(time.Hour).Unix()}})
			return
		}
		if r.Header.Get("X-Cloudide-Token") != "fake-access" {
			t.Error("GetUserInfo token missing")
		}
		replyJSON(w, map[string]any{"Result": map[string]any{"UserID": 12345678901, "ScreenName": "TRAE测试"}})
	}))
	defer server.Close()
	p := NewTrae()
	p.auth = server.URL
	p.http = server.Client()
	auth, err := p.BeginBrowserLogin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	login, _ := url.Parse(auth.URL)
	q := login.Query()
	callback, _ := url.Parse(q.Get("auth_callback_url"))
	if q.Get("auth_type") != "local" || q.Get("login_channel") != "native_ide" || len(q.Get("device_id")) != 32 || len(q.Get("login_trace_id")) != 32 {
		t.Fatal("incomplete TRAE login URL")
	}
	// The public authorization page accepts only a bare loopback /authorize URL.
	if callback.Scheme != "http" || callback.Hostname() != "127.0.0.1" || callback.Port() == "" || callback.Path != "/authorize" || callback.RawQuery != "" || callback.Fragment != "" {
		t.Fatal("TRAE authorization page would reject the callback URL")
	}
	params := callback.Query()
	params.Set("refreshToken", "fake-refresh")
	params.Set("userInfo", `{"UserID":"fallback-user"}`)
	for _, trace := range []string{"", "wrong-trace"} {
		params.Set("loginTraceID", trace)
		if callbackGet(t, callback.String()+"?"+params.Encode()) != 400 {
			t.Fatal("TRAE callback with missing or mismatched trace accepted")
		}
	}
	if exchanges != 0 {
		t.Fatal("invalid TRAE callback exchanged credentials")
	}
	params.Set("loginTraceID", q.Get("login_trace_id"))
	callback.RawQuery = params.Encode()
	if callbackGet(t, callback.String()) != 200 {
		t.Fatal("TRAE callback failed")
	}
	result := <-auth.Result
	if result.Err != nil || exchanges != 1 || str(result.Credential["device_id"]) != q.Get("device_id") || str(result.Credential["machine_id"]) != q.Get("machine_id") || str(result.Credential["uid"]) != "12345678901" || str(result.Credential["refresh_token"]) != "rotated-refresh" {
		t.Fatal("TRAE identity or rotated credential was lost")
	}
}
func TestBrowserCallbackCancellationClosesSession(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p := NewTrae()
	auth, err := p.BeginBrowserLogin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case result := <-auth.Result:
		if result.Err == nil {
			t.Fatal("cancellation reported success")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled callback did not finish")
	}
}
