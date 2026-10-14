package platforms

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

type Trae struct {
	network
	ug       string
	mu       sync.RWMutex
	channels map[string]string
}

func NewTrae() *Trae {
	return &Trae{network: defaultNetwork("https://trae-api-cn.mchost.guru", "https://api.trae.com.cn"), ug: "https://api.trae.cn", channels: map[string]string{}}
}
func (*Trae) Info() Provider {
	return Provider{"trae", "TRAE (字节)", "credit", true, true, "browser"}
}

const traeVersion = "0.1.61"

func traeHeaders(c Credential) http.Header {
	h := http.Header{"Content-Type": {"application/json"}, "Accept": {"application/json"}, "User-Agent": {"Trae/" + traeVersion}, "Authorization": {"Cloud-IDE-JWT " + str(c["access_token"])}, "X-Cloudide-Token": {str(c["access_token"])}, "X-Ide-Token": {str(c["access_token"])}, "X-App-Id": {"6eefa01c-1036-4c7e-9ca5-d891f63bfcd8"}, "X-App-Version": {"default"}, "X-Ide-Version": {traeVersion}, "X-Ide-Version-Code": {"20260820"}, "X-App-Version-Code": {"20260820"}, "X-Ide-Version-Type": {"stable"}, "X-Device-Type": {"windows"}, "X-OS-Version": {"Windows 11 Pro"}, "X-Device-Brand": {"83DG"}, "Request-Traffic-Type": {"prod"}}
	for header, key := range map[string]string{"X-Uid": "uid", "X-Machine-Id": "machine_id", "X-Device-Id": "device_id"} {
		if v := str(c[key]); v != "" {
			h.Set(header, v)
		}
	}
	return h
}
func traeUgHeaders(c Credential) http.Header {
	h := http.Header{"Content-Type": {"application/json"}, "Accept": {"*/*"}, "Authorization": {"Cloud-IDE-JWT " + str(c["access_token"])}, "User-Agent": {"VSCode 1.107.1 (TRAE SOLO CN)"}, "Accept-Language": {"zh-CN"}, "X-Market-Client-Id": {"VSCode 1.107.1"}, "X-User-Region": {"CN"}, "X-Lgw-Req-Sdk-Type": {"3"}, "Package-Type": {"stable_cn"}, "App-Version": {traeVersion}, "X-Device-Type": {"windows"}, "X-Device-Brand": {"83DG"}, "X-OS-Version": {"Windows 11 Pro"}, "Sec-Fetch-Dest": {"empty"}, "Sec-Fetch-Mode": {"no-cors"}, "Sec-Fetch-Site": {"none"}}
	id := uuid4()
	h.Set("X-Request-Id", id)
	h.Set("X-TT-Trace-Id", "00-"+identifier()+"-"+strings.ReplaceAll(id, "-", "")[:16]+"-01")
	for header, key := range map[string]string{"X-Device-Id": "device_id", "X-Market-User-Id": "market_user_id", "Vscode-Sessionid": "session_id"} {
		if v := str(c[key]); v != "" {
			h.Set(header, v)
		}
	}
	return h
}
func (p *Trae) ugRequest(ctx context.Context, c Credential, path string, body any) (map[string]any, error) {
	raw, err := p.request(ctx, "POST", p.ug+path, traeUgHeaders(c), jsonBytes(body))
	if err != nil {
		return nil, err
	}
	code, ok := number(raw["code"])
	if !ok {
		return nil, fmt.Errorf("TRAE 响应缺少业务状态码")
	}
	if code != 0 {
		return nil, fmt.Errorf("TRAE 业务错误 %g: %s", code, str(raw["message"]))
	}
	return raw, nil
}
func (p *Trae) Claim(ctx context.Context, c Credential) (Outcome, error) {
	data, err := p.ugRequest(ctx, c, "/trae/api/v2/ug/checkin_credits/status", map[string]any{})
	if err != nil {
		return Outcome{}, err
	}
	if data["checked_in"] == true {
		return Outcome{Kind: "already-claimed", CoversToday: true}, nil
	}
	if data["enable"] == false {
		return Outcome{Kind: "inactive", Message: "当前账号未开放签到活动"}, nil
	}
	_, err = p.ugRequest(ctx, c, "/trae/api/v2/ug/checkin_credits/claim", map[string]any{})
	if err != nil {
		return Outcome{}, err
	}
	credit, _ := number(data["credits"])
	extra, _ := number(data["extra_credits"])
	message := ""
	after, e := p.ugRequest(ctx, c, "/trae/api/v2/ug/checkin_credits/status", map[string]any{})
	if e == nil {
		if n, ok := number(after["credits"]); ok {
			credit = n
		}
		extra, _ = number(after["extra_credits"])
	} else {
		message = "上游已确认领取，状态复查失败"
	}
	return Outcome{Kind: "claimed", Credit: credit + extra, Unit: "credit", Message: message, CoversToday: true}, nil
}
func (p *Trae) Balance(ctx context.Context, c Credential) (*Balance, error) {
	data, err := p.ugRequest(ctx, c, "/trae/api/v2/pay/ide_user_ent_usage", map[string]any{"require_usage": true, "req_source": 2})
	if err != nil {
		return nil, err
	}
	rows, ok := data["user_entitlement_pack_list"].([]any)
	if !ok {
		return nil, fmt.Errorf("TRAE 响应缺少资源包列表")
	}
	b := &Balance{Packages: []Package{}}
	for _, v := range rows {
		m := obj(v)
		base := obj(m["entitlement_base_info"])
		limit, ok := number(obj(base["quota"])["credits_limit"])
		if !ok || limit <= 0 {
			continue
		}
		used, _ := number(obj(m["usage"])["credits_amount"])
		remaining := limit - used
		if remaining < 0 {
			remaining = 0
		}
		name := str(base["display_desc"])
		if name == "" {
			name = str(m["display_desc"])
		}
		if name == "" {
			name = "资源包"
		}
		expiry, _ := number(m["expire_time"])
		pkg := Package{Name: name, Unit: "credit", Total: limit, Remaining: remaining, Active: true}
		if expiry > 0 {
			pkg.ExpiresAt = int64(expiry * 1000)
			pkg.Active = time.Now().UnixMilli() < pkg.ExpiresAt
		}
		if pkg.Active {
			b.Total += remaining
		} else {
			b.ExpiredTotal += remaining
		}
		b.Packages = append(b.Packages, pkg)
	}
	return b, nil
}

var traeChannels = []string{"solo_agent", "solo_work_lite", "solo_agent_remote", "solo_work_remote", "solo_agent_lite", "solo_design_lite", "solo_design_remote", "solo_coder", "chat_v3", "builder_v3", "git_ai", "code_reviewer", "code_review_summary", "multimodal", "system_diagnosis"}

func (p *Trae) Models(ctx context.Context, c Credential) ([]Model, error) {
	body := map[string]any{"functions": traeChannels, "agent_type": "", "current_config_info": map[string]any{"config_name": "", "is_custom_model": false}, "mode_type": 0, "access_type": 0, "ab_force_vids": "", "ab_autotest_advanced_mode": 0, "show_custom_model": true}
	raw, err := p.request(ctx, "POST", p.api+"/api/ide/v1/batch_get_detail_param", traeHeaders(c), jsonBytes(body))
	if err != nil {
		return nil, err
	}
	if n, ok := number(raw["code"]); ok && n != 0 {
		return nil, fmt.Errorf("TRAE 模型业务错误 %g", n)
	}
	groups, ok := raw["function_configs"].([]any)
	if !ok {
		return nil, fmt.Errorf("TRAE 模型响应缺少 function_configs")
	}
	models := []Model{}
	routes := map[string]string{}
	for _, channel := range traeChannels {
		for _, v := range groups {
			g := obj(v)
			if str(g["function"]) != channel {
				continue
			}
			for _, v := range list(g["config_info_list"]) {
				m := obj(v)
				display := obj(m["display_config"])
				id := str(m["config_name"])
				if id == "" || routes[id] != "" || m["config_switch"] == false || m["is_invisible_to_user"] == true || display["is_custom_model"] == true {
					continue
				}
				if usage := str(m["usage"]); usage != "" && usage != "chat_completion" {
					continue
				}
				routes[id] = channel
				window, _ := number(obj(m["context_window_tokens"])["dev"])
				max := float64(0)
				for _, detail := range list(m["model_detail_list"]) {
					d := obj(detail)
					n, _ := number(d["max_tokens"])
					if max == 0 || strings.HasSuffix(str(d["model_name"]), "__dev") {
						max = n
					}
				}
				models = append(models, Model{ID: id, Name: str(display["display_name"]), ContextWindow: int64(window), MaxTokens: int64(max), Vision: display["multimodal"] == true})
			}
		}
	}
	p.mu.Lock()
	for id, channel := range routes {
		p.channels[id] = channel
	}
	p.mu.Unlock()
	return models, nil
}
func (p *Trae) Refresh(ctx context.Context, c Credential) (Credential, error) {
	if str(c["refresh_token"]) == "" {
		return nil, fmt.Errorf("TRAE 缺少刷新令牌，请重新导入凭据")
	}
	clientID := str(c["client_id"])
	if clientID == "" {
		clientID = "en1oxy7wnw8j9n"
	}
	raw, err := p.request(ctx, "POST", p.auth+"/cloudide/api/v3/trae/oauth/ExchangeToken", http.Header{"Content-Type": {"application/json"}, "User-Agent": {"Trae/" + traeVersion}}, jsonBytes(map[string]any{"ClientID": clientID, "RefreshToken": c["refresh_token"], "ClientSecret": "-", "UserID": ""}))
	if err != nil {
		return nil, err
	}
	result := obj(raw["Result"])
	if len(result) == 0 {
		result = obj(raw["result"])
	}
	if str(result["Token"]) == "" {
		return nil, fmt.Errorf("TRAE 续期响应缺少 Token，请确认凭据所属客户端")
	}
	next := Credential{}
	for k, v := range c {
		next[k] = v
	}
	next["access_token"] = result["Token"]
	if str(result["RefreshToken"]) != "" {
		next["refresh_token"] = result["RefreshToken"]
	}
	if n, ok := number(result["TokenExpireAt"]); ok {
		if n < 1e12 {
			n *= 1000
		}
		next["expires_at"] = n
	} else {
		delete(next, "expires_at")
	}
	return next, nil
}
func traeBody(body map[string]any, channel string) map[string]any {
	out := map[string]any{}
	for _, key := range []string{"messages", "tools", "tool_choice", "temperature", "top_p", "max_tokens", "stop", "presence_penalty", "frequency_penalty", "reasoning_effort"} {
		if v, ok := body[key]; ok {
			out[key] = v
		}
	}
	out = objClone(out)
	out["stream"] = true
	out["model"] = body["model"]
	out["config_name"] = body["model"]
	out["function"] = channel
	for _, v := range list(out["messages"]) {
		m := obj(v)
		if str(m["role"]) == "developer" {
			m["role"] = "system"
		}
		if text, ok := m["content"].(string); ok {
			m["content"] = []any{map[string]any{"type": "text", "text": text}}
		}
		for _, v := range list(m["tool_calls"]) {
			tc := obj(v)
			if fn, ok := tc["function"]; ok {
				tc["function_call"] = fn
				delete(tc, "function")
			}
		}
	}
	choice := str(out["tool_choice"])
	if c, ok := out["tool_choice"].(map[string]any); ok {
		choice = str(c["type"])
		if choice == "function" {
			choice = str(obj(c["function"])["name"])
		}
		out["tool_choice"] = choice
	}
	if choice == "none" {
		delete(out, "tools")
		delete(out, "tool_choice")
	}
	for _, v := range list(out["tools"]) {
		fn := obj(obj(v)["function"])
		if params, ok := fn["parameters"].(map[string]any); ok {
			fn["parameters"] = string(jsonBytes(params))
		}
	}
	return out
}
func objClone(m map[string]any) map[string]any {
	var out map[string]any
	_ = json.Unmarshal(jsonBytes(m), &out)
	return out
}
func (p *Trae) Chat(ctx context.Context, c Credential, body map[string]any, receive func(json.RawMessage) error) error {
	model := str(body["model"])
	p.mu.RLock()
	channel := p.channels[model]
	p.mu.RUnlock()
	if channel == "" {
		if _, err := p.Models(ctx, c); err != nil {
			return err
		}
		p.mu.RLock()
		channel = p.channels[model]
		p.mu.RUnlock()
		if channel == "" {
			return fmt.Errorf("TRAE 当前目录未提供该模型")
		}
	}
	h := traeHeaders(c)
	h.Set("Accept", "text/event-stream")
	resp, err := openStream(ctx, p.chat, p.api+"/api/agent/v3/llm_utils_chat", h, traeBody(body, channel))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return consumeTrae(resp, receive)
}
func consumeTrae(resp *http.Response, receive func(json.RawMessage) error) error {
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 65536), 16<<20)
	event := ""
	data := []string{}
	ended, seen := false, false
	dispatch := func() error {
		if len(data) == 0 {
			return nil
		}
		payload := strings.Join(data, "\n")
		data = nil
		var m map[string]any
		if err := json.Unmarshal([]byte(payload), &m); err != nil {
			return fmt.Errorf("TRAE SSE 数据格式错误")
		}
		switch event {
		case "error":
			code, _ := number(m["code"])
			status := 502
			if code == 4008 || code == 1005 {
				status = 402
			}
			return &RemoteError{fmt.Sprintf("TRAE 流内业务错误 %g", code), status}
		case "output":
			for _, field := range []struct{ key, kind string }{{"response", "text-delta"}, {"reasoning_content", "reasoning-delta"}} {
				if text := str(m[field.key]); text != "" {
					seen = true
					if err := emitChunk(receive, map[string]any{"type": field.kind, "text": text}); err != nil {
						return err
					}
				}
			}
			for i, v := range list(m["tool_calls"]) {
				tc := obj(v)
				fn := obj(tc["function_call"])
				if len(fn) == 0 {
					fn = obj(tc["function"])
				}
				index, ok := number(tc["index"])
				if !ok {
					index = float64(i)
				}
				args := str(fn["arguments"])
				if a, ok := fn["arguments"].(map[string]any); ok {
					args = string(jsonBytes(a))
				}
				seen = true
				if err := emitChunk(receive, map[string]any{"type": "tool-call-delta", "index": int(index), "id": str(tc["id"]), "name": str(fn["name"]), "argumentsDelta": args}); err != nil {
					return err
				}
			}
		case "token_usage":
			input, _ := number(m["prompt_tokens"])
			output, _ := number(m["completion_tokens"])
			cached, _ := number(m["cached_tokens"])
			reasoning, _ := number(m["reasoning_tokens"])
			return emitChunk(receive, map[string]any{"type": "usage", "usage": map[string]any{"inputTokens": input - cached, "outputTokens": output, "cacheReadTokens": cached, "reasoningTokens": reasoning}})
		case "done":
			ended = true
			kind := "stop"
			switch str(m["finish_reason"]) {
			case "length":
				kind = "max-tokens"
			case "tool_calls":
				kind = "tool-calls"
			}
			if !seen {
				return fmt.Errorf("TRAE 返回空响应")
			}
			return emitChunk(receive, map[string]any{"type": "finish", "reason": map[string]any{"kind": kind}})
		}
		return nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := dispatch(); err != nil {
				return err
			}
			event = ""
		} else if strings.HasPrefix(line, "event:") {
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		} else if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if err := dispatch(); err != nil {
		return err
	}
	if !ended {
		return fmt.Errorf("TRAE 流未正常结束")
	}
	return nil
}
