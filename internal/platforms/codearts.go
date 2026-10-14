package platforms

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

type CodeArts struct {
	network
	gateway  string
	mu       sync.RWMutex
	benefits map[string]bool
}

func NewCodeArts() *CodeArts {
	return &CodeArts{network: defaultNetwork("https://snap-access.cn-north-4.myhuaweicloud.com", "https://sts.cn-north-4.myhuaweicloud.com/v1/oauth2/tokens"), gateway: "https://opengw.developer.huaweicloud.com/api/v1/gateway/config", benefits: map[string]bool{"glm-5.3-flash": true, "deepseek-v4.1-flash": true}}
}
func (*CodeArts) Info() Provider {
	return Provider{"codearts", "CodeArts Agent (华为)", "credit", true, true, "import"}
}
func hashHex(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
func huaweiHeaders(c Credential, method, address string, body []byte, extra map[string]string, now time.Time) (http.Header, error) {
	u, err := url.Parse(address)
	if err != nil {
		return nil, err
	}
	if str(c["access_key_id"]) == "" || str(c["secret_access_key"]) == "" || str(c["security_token"]) == "" {
		return nil, fmt.Errorf("CodeArts 缺少 AK/SK/security_token")
	}
	stamp := now.UTC().Format("20060102T150405Z")
	fields := map[string]string{"host": u.Host, "x-sdk-date": stamp, "x-sdk-content-sha256": hashHex(body), "x-security-token": str(c["security_token"])}
	if method != "GET" {
		fields["content-type"] = "application/json"
	}
	for k, v := range extra {
		fields[strings.ToLower(k)] = v
	}
	keys := []string{}
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	lines := []string{}
	h := http.Header{}
	for _, k := range keys {
		lines = append(lines, k+":"+strings.TrimSpace(fields[k]))
		h.Set(k, fields[k])
	}
	path := u.EscapedPath()
	if !strings.HasSuffix(path, "/") {
		path += "/"
	}
	canonical := strings.Join([]string{method, path, u.RawQuery, strings.Join(lines, "\n"), "", strings.Join(keys, ";"), hashHex(body)}, "\n")
	mac := hmac.New(sha256.New, []byte(str(c["secret_access_key"])))
	mac.Write([]byte("SDK-HMAC-SHA256\n" + stamp + "\n" + hashHex([]byte(canonical))))
	h.Set("Authorization", "SDK-HMAC-SHA256 Access="+str(c["access_key_id"])+",SignedHeaders="+strings.Join(keys, ";")+",Signature="+hex.EncodeToString(mac.Sum(nil)))
	// These routing headers are deliberately appended after signing.
	h.Set("Agent-Type", "PromptCenter")
	h.Set("X-Language", "zh-cn")
	h.Set("Accept", "application/json")
	return h, nil
}
func (p *CodeArts) signed(ctx context.Context, c Credential, method, address string, body any) (map[string]any, error) {
	var b []byte
	if body != nil {
		b = jsonBytes(body)
	}
	h, err := huaweiHeaders(c, method, address, b, nil, time.Now())
	if err != nil {
		return nil, err
	}
	raw, err := p.request(ctx, method, address, h, b)
	if err != nil {
		return nil, err
	}
	if raw["error_code"] != nil {
		return nil, fmt.Errorf("CodeArts: %s", str(raw["error_code"]))
	}
	if v, ok := raw["code"]; ok {
		n, valid := number(v)
		if !valid || n != 0 {
			return nil, fmt.Errorf("CodeArts 业务失败: %v %s", v, str(raw["message"]))
		}
		data, ok := raw["data"].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("CodeArts 缺少 data")
		}
		return data, nil
	}
	return raw, nil
}
func (p *CodeArts) Balance(ctx context.Context, c Credential) (*Balance, error) {
	data, err := p.signed(ctx, c, "GET", p.api+"/snap-manager/v1/statistics/plugin", nil)
	if err != nil {
		return nil, err
	}
	b := &Balance{Packages: []Package{}}
	found := false
	totalSet := false
	for _, v := range list(data["metrics"]) {
		m := obj(v)
		key := str(m["name"])
		names := map[string]string{"usageTotalPackageCredit": "总积分包", "usageBasicPackageCredit": "基础积分包", "usageOnDemandPackageCredit": "按需积分包", "usageBonusPackageCredit": "赠送积分包"}
		name, ok := names[key]
		if !ok {
			continue
		}
		n, valid := number(m["package_credit_remain"])
		if !valid {
			continue
		}
		found = true
		total, _ := number(m["package_credit_amount"])
		if key == "usageTotalPackageCredit" {
			b.Total = n
			totalSet = true
			continue
		}
		b.Packages = append(b.Packages, Package{Name: name, Unit: "credit", Remaining: n, Total: total, Active: true})
	}
	if !found {
		return nil, fmt.Errorf("当前套餐无积分余额字段")
	}
	if !totalSet {
		for _, pkg := range b.Packages {
			b.Total += pkg.Remaining
		}
	}
	return b, nil
}
func truth(v any) bool { return v == true || str(v) == "true" }
func (p *CodeArts) Claim(ctx context.Context, c Credential) (Outcome, error) {
	info, err := p.signed(ctx, c, "GET", p.api+"/snap-manager/v1/statistics/plugin", nil)
	if err != nil {
		return Outcome{}, err
	}
	if !truth(obj(info["package"])["is_credit_package"]) {
		return Outcome{Kind: "inactive", Message: "当前不是积分计费账户"}, nil
	}
	data, err := p.signed(ctx, c, "GET", p.api+"/v1/ops/delivery?channel=IDE", nil)
	if err != nil {
		return Outcome{}, err
	}
	items, ok := data["items"].([]any)
	if !ok {
		return Outcome{}, fmt.Errorf("CodeArts 缺少活动列表")
	}
	for _, v := range items {
		a := obj(v)
		if str(a["type"]) != "USER_LOGIN" {
			continue
		}
		if !truth(a["claimable"]) {
			switch str(a["status"]) {
			case "CLAIMED", "CONFIRMED", "CONSUMED":
				return Outcome{Kind: "already-claimed", CoversToday: true}, nil
			}
			return Outcome{Kind: "inactive", Message: "当前不可领取"}, nil
		}
		id := str(a["campaignId"])
		if id == "" {
			if n, ok := number(a["campaignId"]); ok {
				id = fmt.Sprint(n)
			}
		}
		if id == "" {
			return Outcome{}, fmt.Errorf("CodeArts 缺少 campaignId")
		}
		result, err := p.signed(ctx, c, "POST", p.api+"/v1/ops/claim", map[string]any{"campaignId": id, "channel": "IDE"})
		if err != nil {
			return Outcome{}, err
		}
		message := ""
		if result["id"] != nil {
			_, e := p.signed(ctx, c, "POST", p.api+"/v1/ops/confirm", map[string]any{"campaignId": id})
			if e != nil {
				message = "已领取，确认失败，请在官方客户端检查入账状态"
			}
		}
		credit, _ := number(a["benefitAmount"])
		for _, key := range []string{"benefitAmount", "credit", "credits", "creditAmount", "amount"} {
			if n, ok := number(result[key]); ok && n > 0 {
				credit = n
				break
			}
		}
		return Outcome{Kind: "claimed", Credit: credit, Unit: "credit", Message: message, CoversToday: true}, nil
	}
	return Outcome{Kind: "inactive", Message: "未找到每日签到活动"}, nil
}
func (p *CodeArts) Models(ctx context.Context, c Credential) ([]Model, error) {
	result := []Model{}
	seen := map[string]bool{}
	var firstErr error
	for _, endpoint := range []string{p.gateway, p.api + "/v1/model/builtin"} {
		raw, err := p.signed(ctx, c, "GET", endpoint, nil)
		if err != nil {
			firstErr = err
			continue
		}
		rows := list(raw["builtinModels"])
		benefit := endpoint == p.gateway
		if benefit {
			rows = list(obj(raw["result"])["models"])
		}
		for _, v := range rows {
			m := obj(v)
			id := str(m["model_id"])
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			result = append(result, Model{ID: id, Name: str(m["model_name"])})
			if benefit {
				p.mu.Lock()
				p.benefits[id] = true
				p.mu.Unlock()
			}
		}
	}
	if len(result) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return result, nil
}
func (p *CodeArts) Chat(ctx context.Context, c Credential, body map[string]any, receive func(json.RawMessage) error) error {
	out := map[string]any{}
	for k, v := range body {
		out[k] = v
	}
	out["stream"] = true
	out["stream_options"] = map[string]any{"include_usage": true}
	if out["max_tokens"] == nil {
		out["max_tokens"] = 65536
	}
	p.mu.RLock()
	benefit := p.benefits[str(out["model"])]
	p.mu.RUnlock()
	extra := map[string]string{}
	if benefit {
		extra["maas_type"] = "benefit"
	}
	address := p.api + "/api/v2/chat/completions"
	h, err := huaweiHeaders(c, "POST", address, jsonBytes(out), extra, time.Now())
	if err != nil {
		return err
	}
	h.Set("Chat-Id", uuid4())
	h.Set("Session-Id", uuid4())
	h.Set("lang", "en")
	return streamOpenAI(ctx, p.chat, address, h, out, receive)
}
func (p *CodeArts) Refresh(ctx context.Context, c Credential) (Credential, error) {
	jwk := obj(c["dpop_private_key_jwk"])
	decode := func(k string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(str(jwk[k])) }
	d, e := decode("d")
	if e != nil || len(d) == 0 {
		return nil, fmt.Errorf("CodeArts 缺少有效 DPoP 私钥")
	}
	x, e := decode("x")
	if e != nil {
		return nil, e
	}
	y, e := decode("y")
	if e != nil {
		return nil, e
	}
	curve := elliptic.P256()
	pub := ecdsa.PublicKey{Curve: curve, X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
	scalar := new(big.Int).SetBytes(d)
	if !curve.IsOnCurve(pub.X, pub.Y) || scalar.Sign() <= 0 || scalar.Cmp(curve.Params().N) >= 0 {
		return nil, fmt.Errorf("CodeArts DPoP 私钥无效")
	}
	private := &ecdsa.PrivateKey{PublicKey: pub, D: scalar}
	expectedX, expectedY := curve.ScalarBaseMult(d)
	if expectedX.Cmp(pub.X) != 0 || expectedY.Cmp(pub.Y) != 0 {
		return nil, fmt.Errorf("CodeArts DPoP 公私钥不匹配")
	}
	if str(c["refresh_token"]) == "" || str(c["code_verifier"]) == "" {
		return nil, fmt.Errorf("CodeArts 缺少续期材料")
	}
	enc := base64.RawURLEncoding.EncodeToString
	head := enc(jsonBytes(map[string]any{"alg": "ES256", "typ": "dpop+jwt", "jwk": map[string]any{"kty": "EC", "crv": "P-256", "x": str(jwk["x"]), "y": str(jwk["y"])}}))
	payload := enc(jsonBytes(map[string]any{"htm": "POST", "htu": p.auth, "iat": time.Now().Unix(), "jti": identifier() + identifier()}))
	signing := head + "." + payload
	digest := sha256.Sum256([]byte(signing))
	r, s, e := ecdsa.Sign(rand.Reader, private, digest[:])
	if e != nil {
		return nil, e
	}
	signature := make([]byte, 64)
	r.FillBytes(signature[:32])
	s.FillBytes(signature[32:])
	h := http.Header{"Dpop": {signing + "." + enc(signature)}, "Content-Type": {"application/x-www-form-urlencoded"}}
	form := url.Values{"client_id": {"codearts-agent"}, "code_verifier": {str(c["code_verifier"])}, "grant_type": {"refresh_token"}, "refresh_token": {str(c["refresh_token"])}}
	raw, err := p.request(ctx, "POST", p.auth, h, []byte(form.Encode()))
	if err != nil {
		return nil, err
	}
	tokens := obj(raw["credentials"])
	next := Credential{}
	for k, v := range c {
		next[k] = v
	}
	for _, key := range []string{"access_key_id", "secret_access_key", "security_token"} {
		if str(tokens[key]) == "" {
			return nil, fmt.Errorf("CodeArts 刷新缺少 %s", key)
		}
		next[key] = tokens[key]
	}
	if str(raw["refresh_token"]) != "" {
		next["refresh_token"] = str(raw["refresh_token"])
	}
	next["expires_at"] = str(tokens["expiration"])
	return next, nil
}
