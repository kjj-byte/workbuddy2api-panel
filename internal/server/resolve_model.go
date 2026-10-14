package server

import "strings"

// resolveModel 解析模型名协议（PLAN D6）：
//
//	分布式前缀： "[realm:]model"
//
// 取第一个 ":"，前段恰为 "cn"/"global" 才剥离；否则视为裸名，realm=cn、bare=原串。
// 大小写敏感（前缀必须是精确的小写枚举）。bare 即出站/选号/账本使用的裸模型名。
//
// 导出为 ResolveModel（cmd/server/main.go 粘性闭包需要），包内简写 resolveModel。
func resolveModel(model string) (realm, bare string) {
	realm, bare, _ = resolveModelSpec(model)
	return realm, bare
}

// resolveModelSpec 同 resolveModel，但额外回报 realm 是否由客户端**显式**给出。
//
// 为什么需要这个区分（issue：Codex 桌面端不可用）：
//
//	裸名此前被硬解释为 realm=cn，于是 gpt-6-sol 这类**只在 global 域存在**的模型，
//	会被选号谓词 Realm()=="cn" 全部过滤掉 → 池里 4 个健康号全在 global，
//	却报 no_healthy_account（"没有可用账号"，误导性极强）。而 Codex 桌面端会在
//	启动时把 config.toml 的 model 写回裸名（剥掉 "global:" 前缀），用户手改回去
//	也拦不住，于是表现为长期不可用。
//
// explicit=false 时 realm 只是**默认偏好**：调用方应先在 realm 内选号，选不到再
// 回落全域（见 handler.chatCompletions 的裸名回落分支）。explicit=true 时 realm
// 仍是硬约束——显式写了 "cn:" 就不该被悄悄改路由到 global 域。
func resolveModelSpec(model string) (realm, bare string, explicit bool) {
	idx := strings.IndexByte(model, ':')
	if idx < 0 {
		return "cn", model, false
	}
	prefix := model[:idx]
	if prefix != "cn" && prefix != "global" {
		return "cn", model, false
	}
	return prefix, model[idx+1:], true
}

// ResolveModel 是 resolveModel 的导出面（跨包调用）。
func ResolveModel(model string) (realm, bare string) { return resolveModel(model) }

// ResolveModelSpec 是 resolveModelSpec 的导出面（跨包调用）。
// explicit=false 表示 realm 只是默认偏好，调用方可回落全域。
func ResolveModelSpec(model string) (realm, bare string, explicit bool) {
	return resolveModelSpec(model)
}
