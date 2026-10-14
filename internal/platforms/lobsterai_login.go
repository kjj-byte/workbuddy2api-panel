package platforms

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

func (p *LobsterAI) BeginBrowserLogin(ctx context.Context) (BrowserAuthorization, error) {
	state := identifier()
	installation := uuid4()
	first := strconv.FormatInt(time.Now().UnixMilli(), 10)
	return browserCallback(ctx, "/auth/callback", []string{"https://lobsterai.youdao.com"}, func(callback string) string {
		return "https://lobsterai.youdao.com/portal#/login?" + url.Values{"source": {"electron"}, "redirect_uri": {callback}, "state": {state}}.Encode()
	}, func(ctx context.Context, r *http.Request) (Credential, error) {
		q := r.URL.Query()
		if q.Get("state") != state || q.Get("code") == "" {
			return nil, errCallbackParameters
		}
		latest := strconv.FormatInt(time.Now().UnixMilli(), 10)
		h := lobsterHeaders(nil)
		h.Del("Authorization")
		raw, err := p.request(ctx, "POST", p.api+"/api/auth/exchange", h, jsonBytes(map[string]any{"authCode": q.Get("code"), "firstKeyfrom": first, "latestKeyfrom": latest, "uuid": installation, "version": p.clientVersion(ctx)}))
		if err != nil {
			return nil, fmt.Errorf("LobsterAI 授权码换取失败: %w", err)
		}
		data, err := lobsterEnvelope(raw)
		if err != nil {
			return nil, err
		}
		token := str(data["accessToken"])
		if token == "" {
			return nil, fmt.Errorf("LobsterAI 登录响应缺少 accessToken")
		}
		user := obj(data["user"])
		uid := scalarID(user["id"])
		if uid == "" {
			uid = scalarID(user["userId"])
		}
		if uid == "" {
			uid = scalarID(user["yid"])
		}
		if uid == "" {
			uid = hashHex([]byte(token))[:16]
		}
		userID := scalarID(user["userId"])
		if userID == "" {
			userID = scalarID(user["yid"])
		}
		c := Credential{"access_token": token, "refresh_token": str(data["refreshToken"]), "uid": uid, "user_id": userID, "nickname": str(user["nickname"]), "uuid": installation, "first_keyfrom": first, "latest_keyfrom": latest}
		if seconds, ok := number(data["expiresIn"]); ok && seconds > 0 {
			c["expires_at"] = time.Now().Add(time.Duration(seconds) * time.Second).UnixMilli()
		} else if expiry := jwtExpiry(token); expiry > 0 {
			c["expires_at"] = expiry
		}
		return c, nil
	})
}
