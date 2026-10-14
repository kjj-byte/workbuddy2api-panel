package main

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// TestRealmAwareAvailableForModelBareIsRealmFree 粘性层的域口径必须与 handler 选号层一致：
// 裸名不得硬传 "cn"，否则只在 global 域存在的模型（gpt-6-sol 等）在粘性层就被判
// "无可用账号" → session.ResolveForModel 恒返回 false → 每轮重新轮换、永不建立粘性
// 绑定（多轮对话的提示词缓存命中率受损，且表现为"换号频繁"）。
//
// 显式前缀（"cn:"/"global:"）仍是硬约束：跨 realm 泄漏防护不得因此放松。
func TestRealmAwareAvailableForModelBareIsRealmFree(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	p := pool.New("")
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai"})
	p.Add(&auth.Auth{UID: "cn1", Domain: "www.codebuddy.cn"})
	avail := realmAwareAvailableForModel(p)

	contains := func(uids []string, uid string) bool {
		for _, u := range uids {
			if u == uid {
				return true
			}
		}
		return false
	}

	// 裸名：不过滤域 → global 号必须在列（修复前被 cn 谓词滤掉，列表为空）。
	if got := avail("gpt-6-sol"); !contains(got, "g1") {
		t.Fatalf("裸名的粘性可用列表 %v 缺 global 号 g1（裸名不应再硬按 cn 过滤）", got)
	}
	// 显式 global:：仍按 global 过滤 → 不得含 cn1。
	if got := avail("global:gpt-6-sol"); contains(got, "cn1") {
		t.Fatalf("global: 的粘性可用列表 %v 混入 CN 号（跨 realm 泄漏）", got)
	}
	// 显式 cn:：仍按 cn 过滤 → 不得含 g1。
	if got := avail("cn:gpt-6-sol"); contains(got, "g1") {
		t.Fatalf("cn: 的粘性可用列表 %v 混入 global 号（跨 realm 泄漏）", got)
	}
}
