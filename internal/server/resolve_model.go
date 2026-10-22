package server

import (
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// resolveModel 解析模型名协议（PLAN D6）：
//
//	分布式前缀： "[realm:]model"
//
// 取第一个 ":"，前段恰为 "cn"/"global" 才剥离；否则视为裸名，realm=cn、bare=原串。
// 大小写敏感（前缀必须是精确的小写枚举）。bare 即出站/选号/账本使用的裸模型名。
//
// 规则实现在 upstream.SplitRealmPrefix：出站 body 也在同一次解析里剥前缀，
// 两处必须同口径，故委托而非各写一份。
//
// 导出为 ResolveModel（cmd/server/main.go 粘性闭包需要），包内简写 resolveModel。
func resolveModel(model string) (realm, bare string) {
	return upstream.SplitRealmPrefix(model)
}

// ResolveModel 是 resolveModel 的导出面（跨包调用）。
func ResolveModel(model string) (realm, bare string) { return resolveModel(model) }

// realmExplicitPrefix 判断模型名是否显式声明了域前缀（cn: / global:）。
// 跨域回退（issue #135）只在「裸名」时生效：显式前缀代表调用方的路由意图，
// 永不回退到另一域。
func realmExplicitPrefix(model string) bool {
	m := strings.TrimSpace(model)
	return strings.HasPrefix(m, "cn:") || strings.HasPrefix(m, "global:")
}

// otherRealm 返回另一个账号域（cn ↔ global）。
func otherRealm(realm string) string {
	if realm == "global" {
		return "cn"
	}
	return "global"
}
