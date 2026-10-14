package platforms

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

func (p *CodeArts) BeginBrowserLogin(ctx context.Context) (BrowserAuthorization, error) {
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return BrowserAuthorization{}, err
	}
	enc := base64.RawURLEncoding.EncodeToString
	x, y, d := make([]byte, 32), make([]byte, 32), make([]byte, 32)
	private.X.FillBytes(x)
	private.Y.FillBytes(y)
	private.D.FillBytes(d)
	random := make([]byte, 48)
	if _, err := rand.Read(random); err != nil {
		return BrowserAuthorization{}, err
	}
	verifier := enc(random)
	challenge := sha256.Sum256([]byte(verifier))
	ticket := identifier() + identifier()
	seed := Credential{"code_verifier": verifier, "dpop_private_key_jwk": map[string]any{"kty": "EC", "crv": "P-256", "x": enc(x), "y": enc(y), "d": enc(d)}}
	return browserCallback(ctx, "/oauth/callback", []string{"https://codearts.huaweicloud.com", "https://auth.huaweicloud.com", "https://devcloud.cn-north-4.huaweicloud.com"}, func(callback string) string {
		u, _ := url.Parse(callback)
		return "https://codearts.huaweicloud.com/portal/authorize?" + url.Values{"theme": {"2"}, "locale": {"zh-cn"}, "uri_scheme": {"codearts-agent"}, "client_id": {"codearts-agent"}, "port": {u.Port()}, "code_challenge": {enc(challenge[:])}, "code_challenge_method": {"SHA-256"}, "ticket_id": {ticket}, "plugin-name": {"snap_AIIDE"}, "plugin-version": {"5.2.0"}}.Encode()
	}, func(ctx context.Context, r *http.Request) (Credential, error) {
		q := r.URL.Query()
		if code := q.Get("code"); code != "" {
			form := url.Values{"client_id": {"codearts-agent"}, "grant_type": {"authorization_code"}, "code": {code}, "code_verifier": {verifier}, "redirect_uri": {"http://" + r.Host + "/oauth/callback"}}
			return p.tokenRequest(ctx, seed, form)
		}
		if secret := q.Get("secret"); secret != "" {
			return p.pollLoginTicket(ctx, ticket, secret)
		}
		return nil, errCallbackParameters
	})
}
func (p *CodeArts) pollLoginTicket(ctx context.Context, ticket, secret string) (Credential, error) {
	deadline, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	address := p.api + "/snap-manager/v1/login/ticket?" + url.Values{"ticket_id": {ticket}, "secret": {secret}}.Encode()
	headers := http.Header{"Content-Type": {"application/json;charset=UTF-8"}, "plugin-name": {"snap_AIIDE"}, "plugin-version": {"5.2.0"}}
	for {
		raw, err := p.request(deadline, "GET", address, headers, nil)
		if err == nil {
			source := obj(raw["credential"])
			c := Credential{"access_key_id": str(source["access"]), "secret_access_key": str(source["secret"]), "security_token": str(source["securitytoken"]), "expires_at": source["expires_at"], "user_name": str(raw["user_name"])}
			if str(c["security_token"]) == "" {
				c["security_token"] = str(source["securityToken"])
			}
			if c["expires_at"] == nil {
				c["expires_at"] = source["expiresAt"]
			}
			if str(c["access_key_id"]) == "" {
				source = obj(raw["result"])
				c = Credential{"access_key_id": source["accessKeyId"], "secret_access_key": source["secretAccessKey"], "security_token": source["securityToken"], "expires_at": source["expiration"]}
			}
			if validatePlatformCredential("codearts", c) == nil {
				return c, nil
			}
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-deadline.Done():
			timer.Stop()
			return nil, fmt.Errorf("CodeArts 登录票据等待超时，请重试")
		case <-timer.C:
		}
	}
}
