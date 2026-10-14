package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestResolveModelSpec 协议解析：裸名 → (cn, 原串, explicit=false)；
// "cn:"/"global:" → 剥前缀且 explicit=true；未知前缀（如 "foo:bar"）按裸名处理
// （不认的域不当域用，与既有 resolveModel 语义一致，避免老客户端模型名带冒号被误判）。
func TestResolveModelSpec(t *testing.T) {
	cases := []struct {
		in           string
		wantRealm    string
		wantBare     string
		wantExplicit bool
	}{
		{"gpt-6-sol", "cn", "gpt-6-sol", false},
		{"cn:glm-5.2", "cn", "glm-5.2", true},
		{"global:gpt-6-sol", "global", "gpt-6-sol", true},
		{"foo:bar", "cn", "foo:bar", false}, // 未知前缀不当域用
		{"", "cn", "", false},
		{"global:", "global", "", true},
	}
	for _, c := range cases {
		realm, bare, explicit := resolveModelSpec(c.in)
		if realm != c.wantRealm || bare != c.wantBare || explicit != c.wantExplicit {
			t.Errorf("resolveModelSpec(%q) = (%q,%q,%v), want (%q,%q,%v)",
				c.in, realm, bare, explicit, c.wantRealm, c.wantBare, c.wantExplicit)
		}
	}
	// 兼容面：老签名（两返回值）行为不变——裸名恒 cn，这是零回归前提。
	if realm, bare := resolveModel("gpt-6-sol"); realm != "cn" || bare != "gpt-6-sol" {
		t.Errorf("resolveModel(bare) = (%q,%q) want (cn,gpt-6-sol)", realm, bare)
	}
	if realm, bare := resolveModel("global:gpt-6-sol"); realm != "global" || bare != "gpt-6-sol" {
		t.Errorf("resolveModel(global:) = (%q,%q) want (global,gpt-6-sol)", realm, bare)
	}
}

// chatOnce 打一次 POST /v1/chat/completions，返回响应记录器。
func chatOnce(h *Handler, model string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	body := `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
	return rec
}

// TestBareModelFallsBackAcrossRealm 裸模型名的域回落（本次修复的核心回归）：
// 池里只有 global 号能服务该模型时，裸名（无前缀）请求必须跨域选到号并 200，
// 而不是按默认域 cn 过滤后报 no_healthy_account（"没有可用账号"，误导性极强）。
//
// 真实触发场景：Codex 桌面端启动时把 config.toml 的 "global:gpt-6-sol" 写回裸名
// "gpt-6-sol"，而 gpt-6-sol 只在 global 域存在 → 4 个健康号全被 cn 谓词滤掉。
func TestBareModelFallsBackAcrossRealm(t *testing.T) {
	auth.SetGlobalEnabled(true)
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	// 只有 global 号：CN 域为空，硬按 cn 过滤必然选不到。
	p := testPoolWith(&auth.Auth{UID: "g1", AccessToken: "at-g1", ExpiresAt: 9999999999, Domain: "www.workbuddy.ai"})
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := chatOnce(h, "gpt-6-sol")
	if rec.Code != http.StatusOK {
		t.Fatalf("裸名 + 仅 global 号：code=%d want 200（域回落应生效）body=%s", rec.Code, rec.Body)
	}
}

// TestExplicitRealmPrefixIsHardConstraint 显式前缀是硬约束，不参与回落：
// 用户写了 "cn:" 就说明他明确要 CN 出口，池里只有 global 号时必须照实报 503，
// 不能被"悄悄改路由到 global"（那会让显式域选择失去意义）。
func TestExplicitRealmPrefixIsHardConstraint(t *testing.T) {
	auth.SetGlobalEnabled(true)
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	p := testPoolWith(&auth.Auth{UID: "g1", AccessToken: "at-g1", ExpiresAt: 9999999999, Domain: "www.workbuddy.ai"})
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := chatOnce(h, "cn:gpt-6-sol")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("显式 cn: + 仅 global 号：code=%d want 503（硬约束不得回落）body=%s", rec.Code, rec.Body)
	}
}

// TestBareModelPrefersDefaultRealm 裸名仍**优先**默认域（回落不是替换）：
// CN 号可用时裸名走 CN，不会因为"存在 global 号"就漂到 global 域——
// 这是纯 CN 部署零回归的关键（老行为：裸名=cn）。
func TestBareModelPrefersDefaultRealm(t *testing.T) {
	auth.SetGlobalEnabled(true)
	var seen []string
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		seen = append(seen, authz)
		return 200, sseOK, true
	})
	// CN 号 credits 更高 → 确定性随机源(r=0)下必被选中，证明首选仍是 cn。
	p := testPoolWith(
		&auth.Auth{UID: "cn1", AccessToken: "at-cn1", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"},
		&auth.Auth{UID: "g1", AccessToken: "at-g1", ExpiresAt: 9999999999, Domain: "www.workbuddy.ai"},
	)
	p.SetCredits("cn1", 5000, 0)
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := chatOnce(h, "glm-5.2")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if len(seen) != 1 || seen[0] != "Bearer at-cn1" {
		t.Fatalf("裸名首选应为 CN 号，upstream 收到 %v（回落不得改变默认域偏好）", seen)
	}
}

// TestExplicitGlobalPrefixRoutesGlobal 显式 "global:" 仍硬按 global 过滤：
// 池里只有 CN 号时必须 503（老语义，回落只作用于裸名，不放松显式前缀）。
func TestExplicitGlobalPrefixRoutesGlobal(t *testing.T) {
	auth.SetGlobalEnabled(true)
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	p := testPoolWith(&auth.Auth{UID: "cn1", AccessToken: "at-cn1", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"})
	h := NewHandler(Config{Pool: p, Upstream: up})

	if rec := chatOnce(h, "global:gpt-6-sol"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("显式 global: + 仅 CN 号：code=%d want 503 body=%s", rec.Code, rec.Body)
	}
	// 对照：同样只有 CN 号时，裸名照常走 CN 成功（回落只在前一分支补位）。
	if rec := chatOnce(h, "gpt-6-sol"); rec.Code != http.StatusOK {
		t.Fatalf("裸名 + 仅 CN 号：code=%d want 200 body=%s", rec.Code, rec.Body)
	}
}
