package platforms

import (
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Loomy struct {
	network
	accessID, accessSecret, appID string
}

func NewLoomy() *Loomy {
	return &Loomy{defaultNetwork("https://loomyad.xunfei.cn/api/v1", "https://account.xfinfr.com"), "2thryby66wxi53sk", "zsak6eadrbawz683wf5r3m2snrwj868r", "GM3LOOMY"}
}
func (*Loomy) Info() Provider {
	return Provider{"loomy", "Loomy (讯飞)", "credit", true, true, "sms"}
}
func (p *Loomy) business(ctx context.Context, c Credential, method, path string, body any) (map[string]any, error) {
	h := bearer(c)
	h.Set("token", str(c["access_token"]))
	var payload []byte
	if body != nil {
		payload = jsonBytes(body)
	}
	result, err := p.request(ctx, method, p.api+path, h, payload)
	if err != nil {
		return nil, err
	}
	if str(result["code"]) != "000000" {
		return nil, fmt.Errorf("Loomy %s: %s", str(result["code"]), str(result["desc"]))
	}
	return obj(result["data"]), nil
}
func (p *Loomy) Models(ctx context.Context, c Credential) ([]Model, error) {
	h := bearer(c)
	h.Set("token", str(c["access_token"]))
	raw, err := p.request(ctx, "GET", p.api+"/models", h, nil)
	if err != nil {
		return nil, err
	}
	if code, ok := raw["code"]; ok && str(code) != "000000" {
		return nil, fmt.Errorf("Loomy model list: %s", str(raw["desc"]))
	}
	rows := list(raw["data"])
	models := []Model{}
	for _, row := range rows {
		m := obj(row)
		if str(m["id"]) == "" || (str(m["type"]) != "" && str(m["type"]) != "chat") {
			continue
		}
		window, _ := number(m["context_length"])
		models = append(models, Model{ID: str(m["id"]), Name: str(m["name"]), ContextWindow: int64(window)})
	}
	return models, nil
}
func (p *Loomy) Balance(ctx context.Context, c Credential) (*Balance, error) {
	data, err := p.business(ctx, c, "GET", "/points/records?pageNo=1&pageSize=1&recordType=all", nil)
	if err != nil {
		return nil, err
	}
	permanent, ok := number(data["balance"])
	if !ok {
		return nil, fmt.Errorf("Loomy balance field missing")
	}
	daily, _ := number(data["dailyBalance"])
	total, ok := number(data["availableBalance"])
	if !ok {
		total = permanent + daily
	}
	return &Balance{Total: total, Packages: []Package{{Name: "永久积分", Unit: "credit", Remaining: permanent, Total: permanent, Active: true}, {Name: "每日赠送", Unit: "credit", Remaining: daily, Total: daily, Active: true}}}, nil
}
func (p *Loomy) Claim(ctx context.Context, c Credential) (Outcome, error) {
	data, err := p.business(ctx, c, "POST", "/points/first-login", map[string]any{})
	if err != nil {
		return Outcome{}, err
	}
	if data["alreadyProcessed"] == true {
		return Outcome{Kind: "already-claimed", Message: "今日额度已初始化", CoversToday: true}, nil
	}
	quota, hasQuota := number(data["dailyQuota"])
	used, hasUsed := number(data["dailyConsumed"])
	if hasQuota && hasUsed {
		quota -= used
	}
	if quota < 0 {
		quota = 0
	}
	return Outcome{Kind: "claimed", Credit: quota, Unit: "credit", CoversToday: true}, nil
}
func (*Loomy) Refresh(context.Context, Credential) (Credential, error) {
	return nil, fmt.Errorf("Loomy 不支持续期，请重新短信登录")
}
func (p *Loomy) Chat(ctx context.Context, c Credential, body map[string]any, receive func(json.RawMessage) error) error {
	h := bearer(c)
	h.Set("token", str(c["access_token"]))
	return streamOpenAI(ctx, p.chat, p.api+"/chat/completions", h, body, receive)
}
func loomySignature(method, path string, body []byte, date, nonce, secret string) (string, string) {
	digest := md5.Sum(body)
	md5Text := base64.StdEncoding.EncodeToString(digest[:])
	if len(body) == 0 {
		md5Text = ""
	}
	segments := strings.Split(strings.TrimSuffix(path, "/"), "/")
	for i, s := range segments {
		segments[i] = strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
	}
	canonical := strings.Join([]string{method, strings.Join(segments, "/"), "", md5Text, "application/json", date, nonce, "", ""}, "\n")
	mac := hmac.New(sha1.New, []byte(secret))
	_, _ = mac.Write([]byte(canonical))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil)), md5Text
}
func (p *Loomy) accountRequest(ctx context.Context, path string, param any) (map[string]any, error) {
	body := jsonBytes(map[string]any{"base": map[string]any{"appid": p.appID, "modelid": "Web", "version": "1.0.0", "devid": "web", "ua": "Loomy|Desktop|Electron|macOS", "traceid": identifier()}, "param": param})
	date := time.Now().UTC().Format(http.TimeFormat)
	nonce := identifier()
	signature, md5text := loomySignature("POST", path, body, date, nonce, p.accessSecret)
	h := make(http.Header)
	h.Set("Content-Type", "application/json")
	h.Set("Content-MD5", md5text)
	h.Set("Date", date)
	h.Set("Nonce", nonce)
	h.Set("Authorization", "account "+p.accessID+":"+signature)
	raw, err := p.request(ctx, "POST", p.auth+path, h, body)
	if err != nil {
		return nil, err
	}
	if str(raw["code"]) != "000000" {
		return nil, fmt.Errorf("Loomy 登录失败: %s", str(raw["desc"]))
	}
	return obj(raw["data"]), nil
}
func (p *Loomy) SendSMS(ctx context.Context, phone string) (string, error) {
	data, err := p.accountRequest(ctx, "/login/phone/sendMsgCode", map[string]any{"ccode": "86", "phone": phone, "expire": 300})
	if err != nil {
		return "", err
	}
	id := str(data["msgid"])
	if id == "" {
		return "", fmt.Errorf("短信响应缺少 msgid")
	}
	return id, nil
}
func (p *Loomy) LoginSMS(ctx context.Context, phone, code, msgID string) (Credential, error) {
	data, err := p.accountRequest(ctx, "/login/phone/checkCode", map[string]any{"ccode": "86", "phone": phone, "mcode": code, "msgid": msgID, "expire": 1209600})
	if err != nil {
		return nil, err
	}
	if str(data["session"]) == "" || str(data["userid"]) == "" {
		return nil, fmt.Errorf("登录响应缺少 session 或 userid")
	}
	return Credential{"access_token": str(data["session"]), "userid": str(data["userid"]), "phone": phone, "expires_at": time.Now().Add(14 * 24 * time.Hour).UnixMilli()}, nil
}
