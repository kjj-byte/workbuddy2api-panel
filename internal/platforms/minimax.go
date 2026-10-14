package platforms

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type MiniMax struct{ network }

func NewMiniMax() *MiniMax {
	return &MiniMax{defaultNetwork("https://agent.minimax.cn", "https://account.minimax.cn")}
}
func (*MiniMax) Info() Provider {
	return Provider{"minimax", "MiniMax Code", "credit", true, true, "device"}
}
func (p *MiniMax) business(ctx context.Context, c Credential, method, path string, body []byte) (map[string]any, error) {
	data, err := p.request(ctx, method, p.api+path, bearer(c), body)
	if err != nil {
		return nil, err
	}
	if code, ok := number(obj(data["base_resp"])["status_code"]); ok && code != 0 {
		return nil, fmt.Errorf("MiniMax 业务错误 %g: %s", code, str(obj(data["base_resp"])["status_msg"]))
	}
	if nested, ok := data["data"].(map[string]any); ok {
		return nested, nil
	}
	return data, nil
}
func (p *MiniMax) Models(ctx context.Context, c Credential) ([]Model, error) {
	data, err := p.business(ctx, c, "GET", "/mavis/api/v1/models?region=cn&buildEnv=prod", nil)
	if err != nil {
		return nil, err
	}
	models := []Model{}
	for _, entry := range list(data["providers"]) {
		provider := obj(entry)
		if str(provider["providerId"]) == "minimax" {
			data = obj(provider["config"])
			break
		}
	}
	root := data
	if config, ok := data["models"].(map[string]any); ok {
		root = config
	}
	var rows []any
	if raw, ok := data["models"].([]any); ok {
		rows = raw
	} else if raw, ok := data["data"].([]any); ok {
		rows = raw
	} else {
		for key, value := range root {
			if m, ok := value.(map[string]any); ok && m["limit"] != nil {
				m["id"] = key
				rows = append(rows, m)
			}
		}
	}
	for _, row := range rows {
		m := obj(row)
		id := str(m["id"])
		if id == "" {
			continue
		}
		window, _ := number(obj(m["limit"])["context"])
		for _, option := range list(m["context_window_options"]) {
			if n, ok := number(option); ok && n > window {
				window = n
			}
		}
		tokens, _ := number(obj(m["limit"])["output"])
		vision := false
		for _, v := range list(obj(m["modalities"])["input"]) {
			if v == "image" {
				vision = true
			}
		}
		models = append(models, Model{ID: id, Name: str(m["name"]), ContextWindow: int64(window), MaxTokens: int64(tokens), Vision: vision})
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("MiniMax 模型目录形状无法识别")
	}
	return models, nil
}
func (p *MiniMax) Balance(ctx context.Context, c Credential) (*Balance, error) {
	data, err := p.business(ctx, c, "GET", "/minimax-cloud/api/v1/credit/details", nil)
	if err != nil {
		return nil, err
	}
	details, has := data["details"]
	if !has && data["total_count"] == nil {
		return nil, fmt.Errorf("MiniMax 余额字段缺失")
	}
	balance := &Balance{Packages: []Package{}}
	for _, row := range list(details) {
		item := obj(row)
		remaining, ok := number(item["remaining_amount"])
		if !ok {
			continue
		}
		balance.Total += remaining
		total, _ := number(item["granted_amount"])
		expiry, _ := number(item["expire_at_ms"])
		balance.Packages = append(balance.Packages, Package{Name: "积分包", Unit: "credit", Remaining: remaining, Total: total, Active: true, ExpiresAt: int64(expiry)})
	}
	return balance, nil
}
func (p *MiniMax) Claim(ctx context.Context, c Credential) (Outcome, error) {
	path := "/minimax-cloud/api/v1/signin/"
	timezone := "?timezone_id=Asia%2FShanghai"
	status, err := p.business(ctx, c, "GET", path+"status"+timezone, nil)
	if err != nil {
		return Outcome{}, err
	}
	if len(list(status["days"])) != 7 {
		return Outcome{}, fmt.Errorf("MiniMax 签到面板缺少七天状态")
	}
	checked := false
	for _, entry := range list(status["days"]) {
		day := obj(entry)
		code, _ := number(day["status"])
		if day["is_today"] == true && code == 3 {
			checked = true
		}
	}
	if checked {
		return Outcome{Kind: "already-claimed", CoversToday: true}, nil
	}
	data, err := p.business(ctx, c, "POST", path+"claim"+timezone, []byte(`{}`))
	if err != nil {
		return Outcome{}, err
	}
	kind, _ := number(data["claim_result"])
	if kind == 2 {
		return Outcome{Kind: "already-claimed", CoversToday: true}, nil
	}
	if kind != 1 {
		return Outcome{}, fmt.Errorf("MiniMax 缺少有效 claim_result")
	}
	points, _ := number(data["points"])
	return Outcome{Kind: "claimed", Credit: points, Unit: "credit", CoversToday: true}, nil
}
func (p *MiniMax) Chat(ctx context.Context, c Credential, body map[string]any, receive func(json.RawMessage) error) error {
	payload, err := toAnthropic(body)
	if err != nil {
		return err
	}
	effort := str(body["reasoning_effort"])
	if strings.HasPrefix(str(body["model"]), "MiniMax-M3.1") {
		payload["thinking"] = map[string]any{"type": "adaptive"}
	} else if effort == "none" {
		payload["thinking"] = map[string]any{"type": "disabled"}
	} else if effort != "" {
		payload["thinking"] = map[string]any{"type": "adaptive"}
	}
	if effort != "" && effort != "none" {
		payload["output_config"] = map[string]any{"effort": effort}
	}
	h := bearer(c)
	h.Set("Accept", "text/event-stream")
	return streamAnthropic(ctx, p.chat, p.api+"/mavis/api/v1/llm/v1/messages", h, payload, receive)
}
func (p *MiniMax) tokenRequest(ctx context.Context, form url.Values) (map[string]any, error) {
	h := make(http.Header)
	h.Set("Content-Type", "application/x-www-form-urlencoded")
	return p.request(ctx, "POST", p.auth+"/oauth2/token", h, []byte(form.Encode()))
}
func minimaxGrant(data map[string]any, previous Credential) (Credential, error) {
	token := str(data["access_token"])
	refresh := str(data["refresh_token"])
	if refresh == "" {
		refresh = str(previous["refresh_token"])
	}
	seconds, ok := number(data["expires_in"])
	if token == "" || refresh == "" || !ok || seconds <= 0 || !strings.EqualFold(str(data["token_type"]), "Bearer") || !strings.Contains(" "+str(data["scope"])+" ", " agent.default ") {
		return nil, fmt.Errorf("MiniMax 令牌响应缺失必要字段或 scope")
	}
	return Credential{"access_token": token, "refresh_token": refresh, "token_type": "Bearer", "scope": str(data["scope"]), "expires_at": time.Now().Add(time.Duration(seconds) * time.Second).UnixMilli()}, nil
}
func (p *MiniMax) Refresh(ctx context.Context, c Credential) (Credential, error) {
	if str(c["refresh_token"]) == "" {
		return nil, fmt.Errorf("MiniMax refresh_token 未配置")
	}
	data, err := p.tokenRequest(ctx, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {str(c["refresh_token"])}, "client_id": {"mcode-public"}, "scope": {"agent.default"}, "audience": {"agent-backend"}})
	if err != nil {
		return nil, err
	}
	return minimaxGrant(data, c)
}
func (p *MiniMax) BeginLogin(ctx context.Context) (DeviceAuthorization, error) {
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return DeviceAuthorization{}, err
	}
	verifier := base64.RawURLEncoding.EncodeToString(random)
	digest := sha256.Sum256([]byte(verifier))
	h := make(http.Header)
	h.Set("Content-Type", "application/x-www-form-urlencoded")
	data, err := p.request(ctx, "POST", p.auth+"/oauth2/device/code", h, []byte(url.Values{"client_id": {"mcode-public"}, "scope": {"agent.default"}, "audience": {"agent-backend"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(digest[:])}, "code_challenge_method": {"S256"}}.Encode()))
	if err != nil {
		return DeviceAuthorization{}, err
	}
	device := str(data["device_code"])
	link := str(data["verification_uri_complete"])
	if link == "" {
		link = str(data["verification_uri"])
	}
	expires, _ := number(data["expires_in"])
	interval, _ := number(data["interval"])
	if interval < 1 {
		interval = 5
	}
	if device == "" || link == "" || expires <= 0 {
		return DeviceAuthorization{}, fmt.Errorf("MiniMax 设备码响应缺失必要字段")
	}
	return DeviceAuthorization{device, verifier, link, time.Duration(interval) * time.Second, time.Duration(expires) * time.Second}, nil
}
func (p *MiniMax) PollLogin(ctx context.Context, auth DeviceAuthorization) (Credential, bool, error) {
	data, err := p.tokenRequest(ctx, url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}, "device_code": {auth.DeviceCode}, "client_id": {"mcode-public"}, "code_verifier": {auth.Verifier}})
	if err != nil {
		if e, ok := err.(*RemoteError); ok && e.Status == 400 {
			return nil, false, nil
		}
		return nil, false, err
	}
	if str(data["status"]) == "pending" || str(data["error"]) == "authorization_pending" || str(data["error"]) == "slow_down" {
		return nil, false, nil
	}
	credential, err := minimaxGrant(data, nil)
	return credential, err == nil, err
}
