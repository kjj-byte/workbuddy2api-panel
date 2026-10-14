package platforms

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func localLobster(s *httptest.Server) *LobsterAI {
	p := NewLobsterAI()
	p.api = s.URL
	p.versionURL = s.URL + "/version"
	p.http = s.Client()
	p.chat = s.Client()
	return p
}
func TestLobsterActivityProtocolAndBalance(t *testing.T) {
	claims := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/version" {
			replyJSON(w, map[string]any{"data": map[string]any{"value": map[string]any{"version": "2026.10.1"}}})
			return
		}
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Error("missing auth")
		}
		var data any
		switch r.URL.Path {
		case "/api/client-activities/slot":
			if r.URL.Query().Get("clientVersion") != "2026.10.1" || r.URL.Query().Get("containerApiVersion") != "2" {
				t.Error("wrong slot query")
			}
			data = map[string]any{"slotState": "available", "activity": map[string]any{"activityCode": "daily", "configRevision": 7}}
		case "/api/client-activities/daily/context":
			data = map[string]any{"state": map[string]any{"claimedToday": claims > 0}, "actions": []string{"check_in"}}
		case "/api/client-activities/daily/actions/check_in":
			claims++
			var b map[string]any
			json.NewDecoder(r.Body).Decode(&b)
			if b["configRevision"] != float64(7) || len(str(b["idempotencyKey"])) != 36 || b["payload"] == nil {
				t.Error("wrong claim body")
			}
			data = map[string]any{"result": map[string]any{"creditsGranted": 100}}
		case "/api/user/profile-summary":
			data = map[string]any{"totalCreditsRemaining": 200, "creditItems": []any{map[string]any{"label": "每日登录奖励", "creditsRemaining": 100, "expiresAt": "2099-01-01T00:00:00"}, map[string]any{"creditsRemaining": 50, "expiresAt": "2000-01-01T00:00:00"}}}
		default:
			t.Fatalf("unexpected %s", r.URL.Path)
		}
		replyJSON(w, map[string]any{"code": 0, "data": data})
	}))
	defer server.Close()
	p := localLobster(server)
	c := Credential{"access_token": "token"}
	out, err := p.Claim(context.Background(), c)
	if err != nil || out.Credit != 100 || !out.CoversToday {
		t.Fatalf("%+v %v", out, err)
	}
	out, err = p.Claim(context.Background(), c)
	if err != nil || out.Kind != "already-claimed" || claims != 1 {
		t.Fatalf("duplicate write %+v %v", out, err)
	}
	b, err := p.Balance(context.Background(), c)
	if err != nil || b.Total != 200 || b.ExpiredTotal != 50 || b.Packages[0].Name != "每日登录奖励" {
		t.Fatalf("%+v %v", b, err)
	}
}
func TestLobsterRefreshPreservesInstallationIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/version" {
			replyJSON(w, map[string]any{"data": map[string]any{"value": map[string]any{"version": "2026.10.1"}}})
			return
		}
		if r.Header.Get("Authorization") != "" {
			t.Error("refresh must be anonymous")
		}
		var b map[string]any
		json.NewDecoder(r.Body).Decode(&b)
		if b["uuid"] != "install" || b["firstKeyfrom"] != "100" || b["latestKeyfrom"] != "200" || b["refreshToken"] != "refresh" {
			t.Error("identity lost")
		}
		replyJSON(w, map[string]any{"code": 0, "data": map[string]any{"accessToken": "new", "expiresIn": 3600}})
	}))
	defer server.Close()
	p := localLobster(server)
	next, err := p.Refresh(context.Background(), Credential{"access_token": "old", "refresh_token": "refresh", "uuid": "install", "first_keyfrom": "100", "latest_keyfrom": "200"})
	if err != nil || next["refresh_token"] != "refresh" || next["access_token"] != "new" || next["uuid"] != "install" {
		t.Fatalf("%v %v", next, err)
	}
}
func TestCodeArtsDailyOnlyAndExactSignedBody(t *testing.T) {
	credential := Credential{"access_key_id": "ak", "secret_access_key": "sk", "security_token": "st"}
	claimed, confirmed := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		stamp, err := time.Parse("20060102T150405Z", r.Header.Get("X-Sdk-Date"))
		if err != nil {
			t.Fatal(err)
		}
		expected, err := huaweiHeaders(credential, r.Method, "http://"+r.Host+r.URL.RequestURI(), raw, nil, stamp)
		if err != nil || r.Header.Get("Authorization") != expected.Get("Authorization") {
			t.Error("signature does not cover exact bytes")
		}
		if strings.Contains(r.Header.Get("Authorization"), "agent-type") || r.Header.Get("Agent-Type") != "PromptCenter" {
			t.Error("routing header signed incorrectly")
		}
		switch r.URL.Path {
		case "/snap-manager/v1/statistics/plugin":
			replyJSON(w, map[string]any{"package": map[string]any{"is_credit_package": true}, "metrics": []any{map[string]any{"name": "usageTotalPackageCredit", "package_credit_remain": 1100}, map[string]any{"name": "usageBonusPackageCredit", "package_credit_remain": 1000, "package_credit_amount": 1000}}})
		case "/v1/ops/delivery":
			replyJSON(w, map[string]any{"code": 0, "data": map[string]any{"items": []any{map[string]any{"type": "NEW_USER_REGISTER", "claimable": true, "campaignId": 99}, map[string]any{"type": "USER_LOGIN", "claimable": true, "campaignId": 1, "benefitAmount": 1000}}}})
		case "/v1/ops/claim":
			claimed++
			if !strings.Contains(string(raw), `"campaignId":"1"`) {
				t.Error("daily campaign ID lost")
			}
			replyJSON(w, map[string]any{"code": 0, "data": map[string]any{"id": 42, "benefitAmount": 1000}})
		case "/v1/ops/confirm":
			confirmed++
			replyJSON(w, map[string]any{"code": 0, "data": map[string]any{}})
		default:
			t.Fatalf("unexpected %s", r.URL.Path)
		}
	}))
	defer server.Close()
	p := NewCodeArts()
	p.api = server.URL
	p.http = server.Client()
	out, err := p.Claim(context.Background(), credential)
	if err != nil || out.Credit != 1000 || claimed != 1 || confirmed != 1 {
		t.Fatalf("%+v %v", out, err)
	}
	b, err := p.Balance(context.Background(), credential)
	if err != nil || b.Total != 1100 {
		t.Fatalf("double-counted aggregate %+v %v", b, err)
	}
}
func TestQoderRefreshWindowReplayAndEmptyClaimBody(t *testing.T) {
	replay := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cosy-Clienttype") != "10" {
			t.Error("wrong billing identity")
		}
		if strings.HasSuffix(r.URL.Path, "/claim") {
			b, _ := io.ReadAll(r.Body)
			if len(b) != 0 {
				t.Error("claim body must be empty")
			}
			replyJSON(w, map[string]any{"status": "CLAIMED", "replayed": replay, "benefit": map[string]any{"kind": "CREDITS", "amount": 100}})
			return
		}
		replyJSON(w, map[string]any{"campaigns": []any{map[string]any{"campaignId": "daily", "actionType": "CLAIM_BENEFIT", "claimStatus": "CLAIMABLE"}}})
	}))
	defer server.Close()
	p := NewQoder(true)
	p.api = server.URL
	p.http = server.Client()
	p.now = func() time.Time { return time.Date(2026, 10, 14, 1, 0, 0, 0, time.UTC) }
	out, err := p.Claim(context.Background(), Credential{"access_token": "token"})
	if err != nil || out.Credit != 100 || out.CoversToday {
		t.Fatalf("before 10 must not mark today: %+v %v", out, err)
	}
	replay = true
	p.now = func() time.Time { return time.Date(2026, 10, 14, 3, 0, 0, 0, time.UTC) }
	out, err = p.Claim(context.Background(), Credential{"access_token": "token"})
	if err != nil || out.Kind != "already-claimed" || out.Credit != 0 || !out.CoversToday {
		t.Fatalf("replay became a gain: %+v %v", out, err)
	}
}
func TestQoderSignedCatalogAndDevicePending(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "deviceToken/poll") {
			w.WriteHeader(404)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != qoderEncode([]byte("{}")) {
			t.Error("GET body was dropped")
		}
		parts := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer COSY."), ".")
		if len(parts) != 2 {
			t.Fatal("bad COSY bearer")
		}
		payload, err := base64.StdEncoding.DecodeString(parts[0])
		if err != nil || !strings.Contains(string(payload), `"info"`) {
			t.Error("missing encrypted identity")
		}
		sum := md5.Sum([]byte(parts[0] + "\n" + r.Header.Get("Cosy-Key") + "\n" + r.Header.Get("Cosy-Date") + "\n" + string(body) + "\n/api/v2/model/list"))
		if hex.EncodeToString(sum[:]) != parts[1] {
			t.Error("invalid COSY signature")
		}
		replyJSON(w, map[string]any{"chat": []any{map[string]any{"key": "qmodel", "display_name": "Qwen", "enable": true, "max_input_tokens": 200000}}})
	}))
	defer server.Close()
	p := NewQoder(false)
	p.gateway = server.URL
	p.api = server.URL
	p.http = server.Client()
	models, err := p.Models(context.Background(), Credential{"access_token": "token", "uid": "user"})
	if err != nil || len(models) != 1 || models[0].ID != "qmodel" {
		t.Fatalf("%v %v", models, err)
	}
	a, _ := p.BeginLogin(context.Background())
	_, done, err := p.PollLogin(context.Background(), a)
	if err != nil || done {
		t.Fatalf("404 must remain pending: %v %v", done, err)
	}
}
func TestQoderEnvelopeStreamAndToolHistory(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, payload := range []any{map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": "OK"}, "finish_reason": "stop"}}}, "[DONE]"} {
			inner := payload
			if _, ok := payload.(string); !ok {
				inner = string(jsonBytes(payload))
			}
			fmt.Fprintf(w, "data: %s\n\n", jsonBytes(map[string]any{"statusCodeValue": 200, "body": inner}))
		}
	}))
	defer server.Close()
	p := NewQoder(false)
	p.gateway = server.URL
	p.chat = server.Client()
	chunks := ""
	err := p.Chat(context.Background(), Credential{"access_token": "token", "uid": "user"}, map[string]any{"model": "qmodel", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}, func(raw json.RawMessage) error { chunks += string(raw); return nil })
	if err != nil || !strings.Contains(chunks, `"text":"OK"`) {
		t.Fatalf("%s %v", chunks, err)
	}
	_, err = qoderBody(map[string]any{"messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "image_url"}}}}}, nil)
	if err == nil {
		t.Fatal("must not silently discard image input")
	}
}
func TestTraeCheckinUsesStatusAndRetainsQuotaPackages(t *testing.T) {
	claims := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Cloud-IDE-JWT token" || r.Header.Get("X-Device-Id") != "existing-device" {
			t.Error("account device identity changed")
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/status"):
			replyJSON(w, map[string]any{"code": 0, "enable": true, "checked_in": claims > 0, "credits": 150, "extra_credits": 50})
		case strings.HasSuffix(r.URL.Path, "/claim"):
			claims++
			replyJSON(w, map[string]any{"code": 0, "message": "success"})
		case strings.HasSuffix(r.URL.Path, "/ide_user_ent_usage"):
			replyJSON(w, map[string]any{"code": 0, "user_entitlement_pack_list": []any{map[string]any{"entitlement_base_info": map[string]any{"display_desc": "签到奖励", "quota": map[string]any{"credits_limit": 200}}, "usage": map[string]any{"credits_amount": 20}, "expire_time": time.Now().Add(time.Hour).Unix()}}})
		default:
			t.Fatalf("unexpected %s", r.URL.Path)
		}
	}))
	defer server.Close()
	p := NewTrae()
	p.ug = server.URL
	p.http = server.Client()
	c := Credential{"access_token": "token", "device_id": "existing-device"}
	out, err := p.Claim(context.Background(), c)
	if err != nil || out.Credit != 200 {
		t.Fatalf("%+v %v", out, err)
	}
	out, err = p.Claim(context.Background(), c)
	if err != nil || out.Kind != "already-claimed" || claims != 1 {
		t.Fatalf("duplicate claim %+v %v", out, err)
	}
	b, err := p.Balance(context.Background(), c)
	if err != nil || b.Total != 180 || b.Packages[0].ExpiresAt < 1e12 {
		t.Fatalf("%+v %v", b, err)
	}
}
func TestTraeBodyAndSSEPreserveToolProtocol(t *testing.T) {
	body := map[string]any{"model": "model", "messages": []any{map[string]any{"role": "developer", "content": "system"}, map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "call1", "function": map[string]any{"name": "lookup", "arguments": "{}"}}}}}, "tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "lookup", "parameters": map[string]any{"type": "object"}}}}}
	out := traeBody(body, "solo_work_lite")
	if out["function"] != "solo_work_lite" || obj(list(out["messages"])[0])["role"] != "system" || obj(list(obj(list(out["messages"])[1])["tool_calls"])[0])["function_call"] == nil {
		t.Fatal("tool history transformation failed")
	}
	if obj(list(body["messages"])[0])["role"] != "developer" {
		t.Fatal("caller body mutated")
	}
	chunks := ""
	input := "event: output\ndata: {\"reasoning_content\":\"think\",\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"function_call\":{\"name\":\"lookup\",\"arguments\":\"{}\"}}]}\n\nevent: token_usage\ndata: {\"prompt_tokens\":10,\"completion_tokens\":2}\n\nevent: done\ndata: {\"finish_reason\":\"tool_calls\"}\n\n"
	err := consumeTrae(&http.Response{Body: io.NopCloser(strings.NewReader(input))}, func(raw json.RawMessage) error { chunks += string(raw); return nil })
	if err != nil || !strings.Contains(chunks, "tool-call-delta") || !strings.Contains(chunks, "reasoning-delta") || !strings.Contains(chunks, "tool-calls") {
		t.Fatalf("%s %v", chunks, err)
	}
	if err := consumeTrae(&http.Response{Body: io.NopCloser(strings.NewReader("event: output\ndata: {\"response\":\"partial\"}\n\n"))}, func(json.RawMessage) error { return nil }); err == nil {
		t.Fatal("truncated stream accepted")
	}
}
