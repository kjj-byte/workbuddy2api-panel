package platforms

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sync"
	"time"
)

// LobsterAI uses an activity slot and a per-activity context before claiming.
// An unavailable activity must remain retryable; it is not a successful check-in.
type LobsterAI struct {
	network
	versionURL string
	mu         sync.Mutex
	version    string
	versionAt  time.Time
}

func NewLobsterAI() *LobsterAI {
	return &LobsterAI{network: defaultNetwork("https://lobsterai-server.youdao.com", ""), versionURL: "https://api-overmind.youdao.com/openapi/get/luna/hardware/lobsterai/prod/update"}
}
func (*LobsterAI) Info() Provider {
	return Provider{"lobsterai", "LobsterAI (有道龙虾)", "credit", true, true, "import"}
}

var lobsterVersionPattern = regexp.MustCompile(`^\d+(?:\.\d+)*(?:-[0-9A-Za-z.-]+)?$`)

func (p *LobsterAI) clientVersion(ctx context.Context) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.version != "" && time.Since(p.versionAt) < 12*time.Hour {
		return p.version
	}
	raw, err := p.request(ctx, "GET", p.versionURL, http.Header{"Accept": {"application/json"}}, nil)
	if err == nil {
		v := str(obj(obj(raw["data"])["value"])["version"])
		if lobsterVersionPattern.MatchString(v) {
			p.version = v
			p.versionAt = time.Now()
			return v
		}
	}
	return "2026.9.4"
}
func lobsterHeaders(c Credential) http.Header {
	h := bearer(c)
	h.Set("User-Agent", "LobsterAI/0.1.0")
	return h
}
func (p *LobsterAI) headers(ctx context.Context, c Credential) http.Header {
	h := lobsterHeaders(c)
	h.Set("X-LobsterAI-Client-Version", p.clientVersion(ctx))
	h.Set("X-LobsterAI-Client-Capabilities", "kimi-k3-agentic-v1,thinking-level-control-v1")
	return h
}
func lobsterEnvelope(raw map[string]any) (map[string]any, error) {
	code, ok := number(raw["code"])
	if !ok || code != 0 {
		return nil, fmt.Errorf("LobsterAI 请求失败: %v %s", raw["code"], str(raw["msg"]))
	}
	data, ok := raw["data"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("LobsterAI 响应缺少 data 对象")
	}
	return data, nil
}
func (p *LobsterAI) business(ctx context.Context, c Credential, method, path string, body any) (map[string]any, error) {
	var b []byte
	if body != nil {
		b = jsonBytes(body)
	}
	raw, err := p.request(ctx, method, p.api+path, lobsterHeaders(c), b)
	if err != nil {
		return nil, err
	}
	return lobsterEnvelope(raw)
}
func lobsterIdentity(c Credential, version string) map[string]any {
	b := map[string]any{"firstKeyfrom": str(c["first_keyfrom"]), "latestKeyfrom": str(c["latest_keyfrom"]), "version": version}
	for target, source := range map[string]string{"uuid": "uuid", "userId": "user_id"} {
		if v := str(c[source]); v != "" {
			b[target] = v
		}
	}
	return b
}
func (p *LobsterAI) Models(ctx context.Context, c Credential) ([]Model, error) {
	h := p.headers(ctx, c)
	q := url.Values{}
	for k, v := range lobsterIdentity(c, h.Get("X-LobsterAI-Client-Version")) {
		if str(v) != "" {
			q.Set(k, str(v))
		}
	}
	raw, err := p.request(ctx, "GET", p.api+"/api/models/available?"+q.Encode(), h, nil)
	if err != nil {
		return nil, err
	}
	code, ok := number(raw["code"])
	if !ok || code != 0 {
		return nil, fmt.Errorf("LobsterAI 模型列表失败")
	}
	rows := list(raw["data"])
	if rows == nil {
		rows = list(obj(raw["data"])["data"])
	}
	if rows == nil {
		return nil, fmt.Errorf("LobsterAI 模型列表格式异常")
	}
	result := []Model{}
	for _, v := range rows {
		m := obj(v)
		id := str(m["modelId"])
		if id == "" {
			continue
		}
		window, _ := number(m["contextWindow"])
		max, _ := number(m["maxTokens"])
		result = append(result, Model{ID: id, Name: str(m["modelName"]), ContextWindow: int64(window), MaxTokens: int64(max), Vision: m["supportsImage"] == true})
	}
	return result, nil
}
func (p *LobsterAI) Balance(ctx context.Context, c Credential) (*Balance, error) {
	data, err := p.business(ctx, c, "GET", "/api/user/profile-summary", nil)
	if err != nil {
		return nil, err
	}
	total, ok := number(data["totalCreditsRemaining"])
	if !ok {
		return nil, fmt.Errorf("LobsterAI 响应缺少余额")
	}
	if total < 0 {
		total = 0
	}
	b := &Balance{Total: total, Packages: []Package{}}
	for _, v := range list(data["creditItems"]) {
		item := obj(v)
		n, _ := number(item["creditsRemaining"])
		if n < 0 {
			n = 0
		}
		name := str(item["label"])
		if name == "" {
			name = str(item["type"])
		}
		if name == "" {
			name = "积分包"
		}
		pkg := Package{Name: name, Unit: "credit", Remaining: n, Active: true}
		expiry := str(item["expiresAt"])
		for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05"} {
			if t, e := time.ParseInLocation(layout, expiry, time.FixedZone("UTC+8", 8*3600)); e == nil {
				pkg.ExpiresAt = t.UnixMilli()
				pkg.Active = time.Now().Before(t)
				break
			}
		}
		if !pkg.Active {
			b.ExpiredTotal += n
		}
		b.Packages = append(b.Packages, pkg)
	}
	return b, nil
}
func uuid4() string {
	id := identifier()
	return id[:8] + "-" + id[8:12] + "-4" + id[13:16] + "-8" + id[17:20] + "-" + id[20:]
}
func (p *LobsterAI) Claim(ctx context.Context, c Credential) (Outcome, error) {
	q := url.Values{"placement": {"desktop_sidebar"}, "containerApiVersion": {"2"}, "platform": {"win32"}, "clientVersion": {p.clientVersion(ctx)}}
	slot, err := p.business(ctx, c, "GET", "/api/client-activities/slot?"+q.Encode(), nil)
	if err != nil {
		return Outcome{}, err
	}
	activity := obj(slot["activity"])
	code := str(activity["activityCode"])
	if str(slot["slotState"]) != "available" || code == "" {
		return Outcome{Kind: "inactive", Message: "无可用签到活动"}, nil
	}
	revision, ok := number(activity["configRevision"])
	if !ok {
		return Outcome{}, fmt.Errorf("LobsterAI 活动缺少 configRevision")
	}
	base := "/api/client-activities/" + url.PathEscape(code)
	data, err := p.business(ctx, c, "GET", base+fmt.Sprintf("/context?configRevision=%v", revision), nil)
	if err != nil {
		return Outcome{}, err
	}
	if obj(data["state"])["claimedToday"] == true {
		return Outcome{Kind: "already-claimed", CoversToday: true}, nil
	}
	allowed := false
	for _, action := range list(data["actions"]) {
		if str(action) == "check_in" {
			allowed = true
		}
	}
	if !allowed {
		return Outcome{Kind: "inactive", Message: "当前不可签到"}, nil
	}
	data, err = p.business(ctx, c, "POST", base+"/actions/check_in", map[string]any{"configRevision": revision, "idempotencyKey": uuid4(), "payload": map[string]any{}})
	if err != nil {
		return Outcome{}, err
	}
	result := obj(data["result"])
	credit := float64(0)
	for _, key := range []string{"creditsGranted", "rewardCredits", "credits"} {
		if n, ok := number(result[key]); ok {
			credit = n
			break
		}
	}
	return Outcome{Kind: "claimed", Credit: credit, Unit: "credit", Message: str(result["message"]), CoversToday: true}, nil
}
func (p *LobsterAI) Refresh(ctx context.Context, c Credential) (Credential, error) {
	if str(c["refresh_token"]) == "" {
		return nil, fmt.Errorf("LobsterAI 缺少刷新令牌，请重新导入")
	}
	body := lobsterIdentity(c, p.clientVersion(ctx))
	body["refreshToken"] = str(c["refresh_token"])
	h := lobsterHeaders(nil)
	h.Del("Authorization")
	raw, err := p.request(ctx, "POST", p.api+"/api/auth/refresh", h, jsonBytes(body))
	if err != nil {
		return nil, err
	}
	data, err := lobsterEnvelope(raw)
	if err != nil {
		return nil, err
	}
	if str(data["accessToken"]) == "" {
		return nil, fmt.Errorf("LobsterAI 刷新响应缺少 accessToken")
	}
	next := Credential{}
	for k, v := range c {
		next[k] = v
	}
	next["access_token"] = str(data["accessToken"])
	if str(data["refreshToken"]) != "" {
		next["refresh_token"] = str(data["refreshToken"])
	}
	if sec, ok := number(data["expiresIn"]); ok && sec > 0 {
		next["expires_at"] = time.Now().Add(time.Duration(sec) * time.Second).UnixMilli()
	} else {
		delete(next, "expires_at")
	}
	return next, nil
}
func (p *LobsterAI) Chat(ctx context.Context, c Credential, body map[string]any, receive func(json.RawMessage) error) error {
	h := p.headers(ctx, c)
	out := map[string]any{}
	for k, v := range body {
		out[k] = v
	}
	delete(out, "tool_choice")
	delete(out, "prompt_cache_key")
	for k, v := range lobsterIdentity(c, h.Get("X-LobsterAI-Client-Version")) {
		out[k] = v
	}
	return streamOpenAI(ctx, p.chat, p.api+"/api/proxy/v1/chat/completions", h, out, receive)
}
