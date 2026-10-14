package platforms

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

//go:embed qoder_body.json
var qoderBodyTemplate []byte

type Qoder struct {
	network
	id, name, gateway, clientID string
	sessions                    sync.Map
	now                         func() time.Time
}

func NewQoder(cn bool) *Qoder {
	p := &Qoder{network: defaultNetwork("https://openapi.qoder.sh", "https://qoder.com"), id: "qoder", name: "Qoder", gateway: "https://api2.qoder.sh", clientID: "e883ade2-e6e3-4d6d-adf7-f92ceff5fdcb", now: time.Now}
	if cn {
		p.api = "https://openapi.qoder.com.cn"
		p.auth = "https://qoder.cn"
		p.gateway = "https://gateway.qoder.com.cn"
		p.id = "qodercn"
		p.name = "Qoder 中国版"
		p.clientID = "732aef47-9cf2-46a2-95fe-4cebb5d0d1fa"
	}
	return p
}
func (p *Qoder) Info() Provider { return Provider{p.id, p.name, "credit", true, true, "device"} }
func qoderToken(c Credential) string {
	if s := str(c["security_oauth_token"]); s != "" {
		return s
	}
	return str(c["access_token"])
}
func qoderBillingHeaders(c Credential) http.Header {
	h := bearer(c)
	h.Set("Authorization", "Bearer "+qoderToken(c))
	h.Set("Cosy-ClientType", "10")
	h.Set("User-Agent", "Qoder")
	for field, key := range map[string]string{"Cosy-MachineId": "machine_id", "Cosy-MachineToken": "machine_token", "Cosy-MachineType": "machine_type"} {
		if v := str(c[key]); v != "" {
			h.Set(field, v)
		}
	}
	return h
}
func (p *Qoder) Balance(ctx context.Context, c Credential) (*Balance, error) {
	raw, err := p.request(ctx, "GET", p.api+"/sash/api/v2/me/usage", qoderBillingHeaders(c), nil)
	if err != nil {
		return nil, err
	}
	usage, ok := raw["qoderUsage"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("Qoder 没有可解析的个人额度")
	}
	b := &Balance{Packages: []Package{}}
	add := func(name string, m map[string]any) {
		remaining, hasRemaining := number(m["remaining"])
		total, hasTotal := number(m["total"])
		used, _ := number(m["used"])
		if !hasRemaining && !hasTotal {
			return
		}
		if !hasRemaining {
			remaining = total - used
		}
		if remaining < 0 {
			remaining = 0
		}
		b.Packages = append(b.Packages, Package{Name: name, Unit: "credit", Remaining: remaining, Total: total, Active: true})
		b.Total += remaining
	}
	add("套餐额度", obj(usage["userQuota"]))
	add("资源包", obj(usage["addOnQuota"]))
	for _, v := range list(usage["dedicatedResourcePackages"]) {
		m := obj(v)
		add(str(m["name"]), m)
	}
	if len(b.Packages) == 0 {
		return nil, fmt.Errorf("Qoder 响应缺少额度字段")
	}
	return b, nil
}
func (p *Qoder) Claim(ctx context.Context, c Credential) (Outcome, error) {
	raw, err := p.request(ctx, "GET", p.api+"/sash/api/v1/me/campaigns", qoderBillingHeaders(c), nil)
	if err != nil {
		return Outcome{}, err
	}
	rows, ok := raw["campaigns"].([]any)
	if !ok {
		return Outcome{}, fmt.Errorf("Qoder 响应缺少活动列表")
	}
	refreshed := p.now().In(time.FixedZone("UTC+8", 8*3600)).Hour() >= 10
	credit := float64(0)
	claimed, already := false, false
	for _, v := range rows {
		a := obj(v)
		if str(a["actionType"]) != "CLAIM_BENEFIT" {
			continue
		}
		if str(a["claimStatus"]) == "CLAIMED" {
			already = true
			continue
		}
		if str(a["claimStatus"]) != "CLAIMABLE" {
			continue
		}
		id := str(a["campaignId"])
		if id == "" {
			return Outcome{}, fmt.Errorf("Qoder 活动缺少 campaignId")
		}
		result, err := p.request(ctx, "POST", p.api+"/sash/api/v1/me/campaigns/"+url.PathEscape(id)+"/claim", qoderBillingHeaders(c), nil)
		if err != nil {
			return Outcome{}, err
		}
		if result["replayed"] == true {
			already = true
			continue
		}
		if str(result["status"]) != "CLAIMED" {
			return Outcome{}, fmt.Errorf("Qoder 未确认领取成功: %s", str(result["status"]))
		}
		claimed = true
		n, _ := number(obj(result["benefit"])["amount"])
		credit += n
	}
	if claimed {
		return Outcome{Kind: "claimed", Credit: credit, Unit: "credit", CoversToday: refreshed}, nil
	}
	if already {
		return Outcome{Kind: "already-claimed", CoversToday: refreshed, Message: "活动已领取；每日 10:00 (UTC+8) 刷新"}, nil
	}
	return Outcome{Kind: "inactive", Message: "当前没有可领取的每日活动；请在官方客户端确认活动资格"}, nil
}
func (p *Qoder) sign(c Credential, body, address string) (http.Header, error) {
	if str(c["uid"]) == "" {
		return nil, fmt.Errorf("Qoder 加密请求需要 uid，请重新登录或补全凭据")
	}
	key := hashHex([]byte(qoderToken(c) + "\x00" + str(c["uid"])))
	cached, ok := p.sessions.Load(key)
	var session *cosySession
	if ok {
		session = cached.(*cosySession)
	} else {
		id := cosyIdentity{Name: str(c["nickname"]), AID: str(c["aid"]), UID: str(c["uid"]), YxUID: str(c["yx_uid"]), OrganizationID: str(c["organization_id"]), OrganizationName: str(c["organization_name"]), UserType: str(c["user_type"]), SecurityOauthToken: qoderToken(c), RefreshToken: str(c["refresh_token"])}
		if id.AID == "" {
			id.AID = id.UID
		}
		if id.UserType == "" {
			id.UserType = "personal_standard"
		}
		var err error
		session, err = newCosySession(id)
		if err != nil {
			return nil, err
		}
		for key, field := range map[string]*string{"machine_id": &session.MachineID, "machine_token": &session.MachineToken, "machine_type": &session.MachineType} {
			if v := str(c[key]); v != "" {
				*field = v
			}
		}
		actual, _ := p.sessions.LoadOrStore(key, session)
		session = actual.(*cosySession)
	}
	fields, err := session.headers(str(c["uid"]), body, address, "application/json", true)
	if err != nil {
		return nil, err
	}
	h := http.Header{}
	for k, v := range fields {
		h.Set(k, v)
	}
	return h, nil
}
func (p *Qoder) Models(ctx context.Context, c Credential) ([]Model, error) {
	address := p.gateway + "/algo/api/v2/model/list?Encode=1"
	body := qoderEncode([]byte("{}"))
	h, err := p.sign(c, body, address)
	if err != nil {
		return nil, err
	}
	raw, err := p.request(ctx, "GET", address, h, []byte(body))
	if err != nil {
		return nil, err
	}
	rows, ok := raw["chat"].([]any)
	if !ok {
		return nil, fmt.Errorf("Qoder 模型列表缺少 chat 场景")
	}
	models := []Model{}
	for _, v := range rows {
		m := obj(v)
		if m["enable"] == false || str(m["key"]) == "" {
			continue
		}
		window, _ := number(m["max_input_tokens"])
		for _, entry := range list(m["context_config"]) {
			config := obj(entry)
			for _, key := range []string{"context_window", "context_length", "max_input_tokens"} {
				if n, ok := number(config[key]); ok && n > window {
					window = n
				}
			}
		}
		models = append(models, Model{ID: str(m["key"]), Name: str(m["display_name"]), ContextWindow: int64(window)})
	}
	return models, nil
}
func qoderBody(body map[string]any, c Credential) ([]byte, error) {
	var out map[string]any
	if err := json.Unmarshal(qoderBodyTemplate, &out); err != nil {
		return nil, err
	}
	messages := []any{}
	prompt := ""
	for _, v := range list(body["messages"]) {
		m := map[string]any{}
		for k, v := range obj(v) {
			m[k] = v
		}
		if str(m["role"]) == "developer" {
			m["role"] = "system"
		}
		if parts, ok := m["content"].([]any); ok {
			texts := []string{}
			for _, v := range parts {
				part := obj(v)
				if str(part["type"]) != "text" {
					return nil, fmt.Errorf("当前 Qoder Go 通道仅支持文本输入")
				}
				texts = append(texts, str(part["text"]))
			}
			m["content"] = strings.Join(texts, "\n")
		}
		if str(m["role"]) == "user" {
			prompt = str(m["content"])
		}
		if str(m["role"]) == "assistant" && m["tool_calls"] != nil && str(m["content"]) == "" {
			m["content"] = "Calling tools."
		}
		messages = append(messages, m)
	}
	if prompt == "" {
		return nil, fmt.Errorf("Qoder 需要用户文本消息")
	}
	out["messages"] = messages
	if body["tools"] != nil {
		out["tools"] = body["tools"]
	}
	for _, key := range []string{"request_id", "chat_record_id", "request_set_id", "session_id"} {
		out[key] = uuid4()
	}
	model := str(body["model"])
	obj(out["model_config"])["key"] = model
	obj(out["model_config"])["is_reasoning"] = true
	cc := obj(out["chat_context"])
	obj(cc["text"])["text"] = prompt
	extra := obj(cc["extra"])
	obj(extra["originalContent"])["text"] = prompt
	obj(extra["modelConfig"])["key"] = model
	obj(extra["modelConfig"])["is_reasoning"] = true
	params := obj(out["parameters"])
	if body["max_tokens"] != nil {
		params["max_tokens"] = body["max_tokens"]
	}
	if effort := str(body["reasoning_effort"]); effort != "" {
		params["enable_thinking"] = effort != "none"
		if effort != "none" {
			params["reasoning_effort"] = effort
		}
	}
	biz := obj(out["business"])
	biz["id"] = uuid4()
	biz["begin_at"] = time.Now().UnixMilli()
	biz["name"] = "API conversation"
	if str(c["user_type"]) != "" {
		out["aliyun_user_type"] = str(c["user_type"])
	}
	return json.Marshal(out)
}
func (p *Qoder) Chat(ctx context.Context, c Credential, body map[string]any, receive func(json.RawMessage) error) error {
	plain, err := qoderBody(body, c)
	if err != nil {
		return err
	}
	encoded := qoderEncode(plain)
	address := p.gateway + "/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1"
	h, err := p.sign(c, encoded, address)
	if err != nil {
		return err
	}
	h.Set("Accept", "text/event-stream")
	req, err := http.NewRequestWithContext(ctx, "POST", address, strings.NewReader(encoded))
	if err != nil {
		return err
	}
	req.Header = h
	resp, err := p.chat.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return &RemoteError{fmt.Sprintf("Qoder HTTP %d", resp.StatusCode), resp.StatusCode}
	}
	// Decode the outer SSE envelope lazily; closing the pipe cancels the producer.
	reader, writer := io.Pipe()
	defer reader.Close()
	go func() {
		err := readSSE(resp.Body, func(data string) error {
			payload := data
			if data != "[DONE]" {
				var outer map[string]any
				if json.Unmarshal([]byte(data), &outer) != nil {
					return fmt.Errorf("Qoder 事件格式错误")
				}
				if n, _ := number(outer["statusCodeValue"]); n >= 400 {
					return &RemoteError{"Qoder 推理事件被拒绝", int(n)}
				}
				if b, ok := outer["body"]; ok {
					if s, ok := b.(string); ok {
						payload = s
					} else {
						payload = string(jsonBytes(b))
					}
				}
				if payload != "[DONE]" {
					var chunk map[string]any
					if json.Unmarshal([]byte(payload), &chunk) != nil {
						return fmt.Errorf("Qoder 内层事件格式错误")
					}
					if chunk["error"] == nil && chunk["choices"] == nil && chunk["usage"] == nil {
						return nil
					}
				}
			}
			_, err := fmt.Fprintf(writer, "data: %s\n\n", payload)
			return err
		})
		writer.CloseWithError(err)
	}()
	return consumeOpenAIResponse(&http.Response{Body: reader, Header: http.Header{"Content-Type": {"text/event-stream"}}}, receive)
}
func (p *Qoder) BeginLogin(context.Context) (DeviceAuthorization, error) {
	verifier := identifier() + identifier()
	sum := sha256.Sum256([]byte(verifier))
	nonce, machine := uuid4(), uuid4()
	q := url.Values{"challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "challenge_method": {"S256"}, "nonce": {nonce}, "machine_id": {machine}, "client_id": {p.clientID}}
	return DeviceAuthorization{DeviceCode: string(jsonBytes(map[string]string{"nonce": nonce, "machine_id": machine})), Verifier: verifier, URL: p.auth + "/device/selectAccounts?" + q.Encode(), Interval: 3 * time.Second, Expires: 15 * time.Minute}, nil
}
func (p *Qoder) tokenCredential(raw map[string]any, previous Credential) (Credential, error) {
	token := ""
	for _, key := range []string{"token", "device_token", "access_token"} {
		if str(raw[key]) != "" {
			token = str(raw[key])
			break
		}
	}
	if token == "" {
		return nil, fmt.Errorf("Qoder 令牌响应缺少 token")
	}
	next := Credential{}
	for k, v := range previous {
		next[k] = v
	}
	next["access_token"] = token
	next["security_oauth_token"] = token
	for target, source := range map[string]string{"refresh_token": "refresh_token", "uid": "user_id", "nickname": "user_name"} {
		if str(raw[source]) != "" {
			next[target] = raw[source]
		}
	}
	for _, key := range []string{"expires_at", "expiresAt"} {
		if n, ok := number(raw[key]); ok {
			if n < 1e12 {
				n *= 1000
			}
			next["expires_at"] = n
			break
		}
		if parsed, err := time.Parse(time.RFC3339, str(raw[key])); err == nil {
			next["expires_at"] = parsed.UnixMilli()
			break
		}
	}
	return next, nil
}
func (p *Qoder) PollLogin(ctx context.Context, auth DeviceAuthorization) (Credential, bool, error) {
	var session map[string]string
	json.Unmarshal([]byte(auth.DeviceCode), &session)
	q := url.Values{"nonce": {session["nonce"]}, "verifier": {auth.Verifier}, "challenge_method": {"S256"}}
	raw, err := p.request(ctx, "GET", p.api+"/api/v1/deviceToken/poll?"+q.Encode(), http.Header{"Accept": {"application/json"}}, nil)
	var remote *RemoteError
	if errors.As(err, &remote) && remote.Status == 404 {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if str(raw["token"]) == "" && str(raw["device_token"]) == "" && str(raw["access_token"]) == "" {
		return nil, false, nil
	}
	c, err := p.tokenCredential(raw, Credential{"machine_id": session["machine_id"]})
	return c, err == nil, err
}
func (p *Qoder) Refresh(ctx context.Context, c Credential) (Credential, error) {
	if str(c["refresh_token"]) == "" {
		return nil, fmt.Errorf("Qoder 缺少刷新令牌")
	}
	raw, err := p.request(ctx, "POST", p.api+"/api/v1/deviceToken/refresh", http.Header{"Content-Type": {"application/json"}}, jsonBytes(map[string]any{"refresh_token": c["refresh_token"], "machine_id": c["machine_id"]}))
	if err != nil {
		return nil, err
	}
	next, err := p.tokenCredential(raw, c)
	if err == nil {
		p.sessions.Delete(hashHex([]byte(qoderToken(c) + "\x00" + str(c["uid"]))))
	}
	return next, err
}
