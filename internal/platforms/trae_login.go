package platforms

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
)

func (p *Trae) BeginBrowserLogin(ctx context.Context) (BrowserAuthorization, error) {
	machine, device, trace := identifier(), identifier(), identifier()
	clientID := "en1oxy7wnw8j9n"
	return browserCallback(ctx, "/authorize", []string{"https://www.trae.cn", "https://trae.cn"}, func(callback string) string {
		// TRAE rejects callback URLs containing a query or fragment. Its page
		// echoes login_trace_id as loginTraceID, which binds this callback instead.
		return "https://www.trae.cn/authorization?" + url.Values{"login_version": {"1"}, "auth_from": {"solo"}, "login_channel": {"native_ide"}, "plugin_version": {"2.3.62834"}, "auth_type": {"local"}, "client_id": {clientID}, "redirect": {"0"}, "login_trace_id": {trace}, "auth_callback_url": {callback}, "machine_id": {machine}, "device_id": {device}, "x_device_id": {device}, "x_machine_id": {machine}, "x_device_brand": {"PC"}, "x_device_type": {"PC"}, "x_os_version": {"1.0"}, "x_app_version": {traeVersion}, "x_app_type": {"stable"}}.Encode()
	}, func(ctx context.Context, r *http.Request) (Credential, error) {
		q := r.URL.Query()
		if q.Get("loginTraceID") != trace {
			return nil, errCallbackParameters
		}
		user := jsonParam(q.Get("userInfo"))
		tokens := jsonParam(q.Get("userJwt"))
		refresh := q.Get("refreshToken")
		if refresh == "" {
			refresh = str(tokens["RefreshToken"])
		}
		token := str(tokens["Token"])
		if refresh == "" && token == "" {
			if q.Get("code") != "" || q.Get("authCodeInfo") != "" {
				return nil, fmt.Errorf("TRAE 返回了授权码流程，请使用客户端本地登录授权后重试")
			}
			return nil, errCallbackParameters
		}
		c := Credential{"access_token": token, "refresh_token": refresh, "machine_id": machine, "device_id": device, "client_id": clientID, "uid": scalarID(user["UserID"]), "nickname": str(user["ScreenName"]), "enterprise_id": scalarID(user["TenantID"])}
		if refresh != "" {
			var err error
			c, err = p.Refresh(ctx, c)
			if err != nil {
				return nil, err
			}
		} else if expiry := jwtExpiry(token); expiry > 0 {
			c["expires_at"] = expiry
		}
		h := http.Header{"Content-Type": {"application/json"}, "User-Agent": {"Trae/" + traeVersion}, "X-Cloudide-Token": {str(c["access_token"])}}
		raw, err := p.request(ctx, "POST", p.auth+"/cloudide/api/v3/trae/GetUserInfo", h, jsonBytes(map[string]any{"ReqSource": "IDE", "IDEVersion": traeVersion}))
		if err == nil {
			info := obj(raw["Result"])
			if id := scalarID(info["UserID"]); id != "" {
				c["uid"] = id
			}
			if name := str(info["ScreenName"]); name != "" {
				c["nickname"] = name
			}
			if tenant := scalarID(info["TenantID"]); tenant != "" {
				c["enterprise_id"] = tenant
			}
		}
		if str(c["uid"]) == "" {
			return nil, fmt.Errorf("TRAE 登录响应缺少用户 ID")
		}
		return c, nil
	})
}
