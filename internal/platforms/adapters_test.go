package platforms

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func localLoomy(server *httptest.Server) *Loomy {
	p := NewLoomy()
	p.api = server.URL
	p.auth = server.URL
	p.http = server.Client()
	p.chat = server.Client()
	return p
}
func localMiniMax(server *httptest.Server) *MiniMax {
	p := NewMiniMax()
	p.api = server.URL
	p.auth = server.URL
	p.http = server.Client()
	p.chat = server.Client()
	return p
}
func replyJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func TestLoomyBalanceIsReadOnlyAndUsesBothPools(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/points/records" {
			t.Errorf("unexpected write: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("token") != "test-session" {
			t.Error("missing platform token header")
		}
		replyJSON(w, map[string]any{"code": "000000", "data": map[string]any{"balance": 15000, "dailyBalance": 4992, "availableBalance": 19992}})
	}))
	defer server.Close()
	b, err := localLoomy(server).Balance(context.Background(), Credential{"access_token": "test-session"})
	if err != nil {
		t.Fatal(err)
	}
	if b.Total != 19992 || len(b.Packages) != 2 || b.Packages[0].Remaining != 15000 || b.Packages[1].Remaining != 4992 {
		t.Fatalf("wrong pools %+v", b)
	}
}
func TestLoomyClaimBusinessCodeAndIdempotence(t *testing.T) {
	for _, tc := range []struct {
		response, kind string
		credit         float64
		fail           bool
	}{
		{`{"code":"000000","data":{"dailyQuota":5000,"dailyConsumed":8,"alreadyProcessed":false}}`, "claimed", 4992, false},
		{`{"code":"000000","data":{"alreadyProcessed":true}}`, "already-claimed", 0, false},
		{`{"code":"100002","desc":"expired"}`, "", 0, true},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "POST" || r.URL.Path != "/points/first-login" {
				t.Error("wrong claim endpoint")
			}
			fmt.Fprint(w, tc.response)
		}))
		out, err := localLoomy(server).Claim(context.Background(), Credential{"access_token": "x"})
		server.Close()
		if tc.fail {
			if err == nil {
				t.Fatal("HTTP 200 business error accepted")
			}
			continue
		}
		if err != nil || out.Kind != tc.kind || out.Credit != tc.credit || !out.CoversToday {
			t.Fatalf("%+v %v", out, err)
		}
	}
}
func TestLoomySMSRequestIsSignedOverExactBytes(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		if json.Unmarshal(body, &payload) != nil {
			t.Fatal("invalid JSON")
		}
		nonce := r.Header.Get("Nonce")
		date := r.Header.Get("Date")
		signature, md5value := loomySignature(r.Method, r.URL.Path, body, date, nonce, "test-secret")
		canonical := strings.Join([]string{r.Method, r.URL.Path, "", md5value, "application/json", date, nonce, "", ""}, "\n")
		mac := hmac.New(sha1.New, []byte("test-secret"))
		mac.Write([]byte(canonical))
		if signature != base64.StdEncoding.EncodeToString(mac.Sum(nil)) {
			t.Error("signature mismatch")
		}
		if r.Header.Get("Authorization") != "account test-ak:"+signature {
			t.Error("wire signature differs from sent body")
		}
		if r.Header.Get("Content-MD5") != md5value {
			t.Error("MD5 header mismatch")
		}
		if str(obj(payload["base"])["appid"]) != "test-app" {
			t.Error("wrong app ID")
		}
		replyJSON(w, map[string]any{"code": "000000", "data": map[string]any{"msgid": "msg-test", "session": "s-test", "userid": "u-test"}})
	}))
	defer server.Close()
	p := localLoomy(server)
	p.accessID = "test-ak"
	p.accessSecret = "test-secret"
	p.appID = "test-app"
	msg, err := p.SendSMS(context.Background(), "13800000000")
	if err != nil || msg != "msg-test" {
		t.Fatal(msg, err)
	}
	credential, err := p.LoginSMS(context.Background(), "13800000000", "1234", msg)
	if err != nil || str(credential["access_token"]) != "s-test" || calls.Load() != 2 {
		t.Fatal(credential, err)
	}
}
func TestMiniMaxCreditBalanceUsesAmountsNotRecordCount(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		replyJSON(w, map[string]any{"total_count": 1, "details": []any{map[string]any{"remaining_amount": "800.00", "granted_amount": "800.00"}}, "base_resp": map[string]any{"status_code": 0}})
	}))
	defer server.Close()
	balance, err := localMiniMax(server).Balance(context.Background(), Credential{"access_token": "x"})
	if err != nil || balance.Total != 800 {
		t.Fatalf("%+v %v", balance, err)
	}
}
func miniPanel(status int) []any {
	days := []any{}
	for i := 1; i <= 7; i++ {
		code := 1
		if i == 1 {
			code = status
		}
		days = append(days, map[string]any{"day_no": i, "is_today": i == 1, "status": code, "points": 800, "bonus_points": 400})
	}
	return days
}
func TestMiniMaxClaimPrecheckAndPoints(t *testing.T) {
	for _, already := range []bool{false, true} {
		claims := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("timezone_id") != "Asia/Shanghai" {
				t.Error("timezone must be a query parameter")
			}
			if strings.HasSuffix(r.URL.Path, "status") {
				status := 2
				if already {
					status = 3
				}
				replyJSON(w, map[string]any{"data": map[string]any{"scene": 2, "days": miniPanel(status)}})
			} else {
				claims++
				replyJSON(w, map[string]any{"data": map[string]any{"claim_result": 1, "points": 800, "bonus_points": 400}})
			}
		}))
		out, err := localMiniMax(server).Claim(context.Background(), Credential{"access_token": "x"})
		server.Close()
		if err != nil {
			t.Fatal(err)
		}
		if already {
			if claims != 0 || out.Kind != "already-claimed" {
				t.Fatal("already claimed account was posted again")
			}
		} else if claims != 1 || out.Credit != 800 {
			t.Fatalf("bonus double counted: %+v", out)
		}
	}
}
func TestMiniMaxDeviceFlowPKCEAndPendingDialects(t *testing.T) {
	polls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if strings.HasSuffix(r.URL.Path, "/device/code") {
			if r.Form.Get("code_challenge_method") != "S256" || r.Form.Get("code_challenge") == "" {
				t.Error("missing PKCE")
			}
			replyJSON(w, map[string]any{"device_code": "dev", "user_code": "user", "verification_uri": "https://example.com/login", "expires_in": 600, "interval": 1})
			return
		}
		if r.Form.Get("code_verifier") == "" {
			t.Error("missing verifier")
		}
		polls++
		if polls == 1 {
			replyJSON(w, map[string]any{"status": "pending"})
		} else if polls == 2 {
			w.WriteHeader(400)
			replyJSON(w, map[string]any{"error": "authorization_pending"})
		} else {
			replyJSON(w, map[string]any{"access_token": "test-access", "refresh_token": "test-refresh", "expires_in": 3600, "scope": "agent.default", "token_type": "Bearer"})
		}
	}))
	defer server.Close()
	p := localMiniMax(server)
	auth, err := p.BeginLogin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		credential, done, err := p.PollLogin(context.Background(), auth)
		if err != nil {
			t.Fatal(err)
		}
		if done != (i == 2) {
			t.Fatal("incorrect pending handling")
		}
		if done && str(credential["access_token"]) != "test-access" {
			t.Fatal("missing credentials")
		}
	}
}
func TestMiniMaxCatalogReadsProviderConfig(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		replyJSON(w, map[string]any{"providers": []any{map[string]any{"providerId": "minimax", "config": map[string]any{"models": map[string]any{"long-id": map[string]any{"name": "short-name", "limit": map[string]any{"context": 512000, "output": 128000}, "context_window_options": []any{512000, 1000000}, "modalities": map[string]any{"input": []any{"text", "image"}}}}}}}})
	}))
	defer server.Close()
	models, err := localMiniMax(server).Models(context.Background(), Credential{"access_token": "x"})
	if err != nil || len(models) != 1 || models[0].ID != "long-id" || models[0].Name != "short-name" || models[0].ContextWindow != 1000000 || !models[0].Vision {
		t.Fatalf("%+v %v", models, err)
	}
}
