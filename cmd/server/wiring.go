package main

import (
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/server"
)

// realmAwareAvailableForModel 构造会话粘性路由按模型可用口径的 realm 感知闭包。
//
// 粘性分配的模型名可能带 realm 前缀（"global:gpt-5.4" / "cn:glm-5.2"）：必须按前缀剥出
// realm + bareModel，再交给分池选号域过滤——否则裸名取池子全集，global 号会被粘性分配给
// CN 前缀请求（跨 realm 泄漏）。
//
// 裸名（未写前缀）自本版起传 realm=""：与 handler 选号的语义对齐——裸名的默认域 cn 只是
// **偏好**而非硬约束（resolveModelSpec 的 explicit 语义）。此前裸名硬传 "cn"，导致
// gpt-6-sol 这类只在 global 域存在的模型在粘性层就被判为"无可用账号"，ResolveForModel
// 直接返回 false，于是每轮都重新轮换、永不建立粘性绑定（提示词缓存命中率受损）。
// 显式 "cn:"/"global:" 仍是硬约束，跨 realm 泄漏防护不变。
//
// realm 为空串时 pool.WeightedAvailableUIDsForModelRealm 退化为现状
// （AvailableUIDsForModel），老调用（无前缀模型名）语义零改动。
//
// 返回列表可能对快过期账号重复同一 UID，作为虚拟实例权重；会话哈希分配无需感知
// 权重细节，已有绑定的快路径仍直接返回原账号，不做迁移。
func realmAwareAvailableForModel(p *pool.Pool) func(model string) []string {
	return func(model string) []string {
		realm, bare, explicit := server.ResolveModelSpec(model)
		if !explicit {
			realm = ""
		}
		return p.WeightedAvailableUIDsForModelRealm(bare, realm)
	}
}
