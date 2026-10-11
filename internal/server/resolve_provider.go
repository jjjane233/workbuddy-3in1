package server

import "strings"

// 三合一网关的 provider 路由。
//
// 这是 Phase 4（请求分发）的判据来源：同一个 /v1/chat/completions 入口，
// 根据模型名/请求头把流量导向 CodeBuddy、Trae 或 ZCode 三条上游链路。

const (
	providerWorkbuddy = "workbuddy"
	providerTrae      = "trae"
	providerZCode     = "zcode"
)

// resolveRoute 解析三合一网关的完整路由。
//
// 模型名协议（向后兼容 PLAN D6 的 "[realm:]model"）：
//
//	"[provider/][realm:]model"    或    "[provider:][realm:]model"
//
// provider 段以 "/" 或 ":" 分隔（两者等价，见 splitProviderPrefix），
// 且必须命中已知枚举（含别名）才会被剥离：
//
//	zcode | zai | bigmodel → zcode
//	trae  | traecode       → trae
//	workbuddy             → workbuddy
//
// realm 段以 ":" 分隔（cn|global），沿用 resolveModel 原逻辑，一行未改。
// 剩余部分即裸模型名，供选号、出站 body 重写与账本使用。
//
// provider 判定优先级：显式 "/" 前缀 → X-Provider 头 → **workbuddy**。
//
// 【为什么没有 gw2api 的「按模型名特征推断」那一级】
// gw2api 是 Trae/ZCode 专用网关，默认推断成 zcode 无害；workbuddy 不是——
// 本项目的 CodeBuddy 链路**实际就用 glm-5.2 / glm-5.3 作为模型名**（见
// handler_test.go 全篇），照搬 glm*→zcode 会把存量真实流量整批劫持到 ZCode
// 上游，直接违反「不破坏现有 CodeBuddy 网关」这条硬约束。
// 实测后果：internal/server 29 个用例全部返回 "no available zcode account"。
// 因此这里只认显式声明，宁可多打一个前缀，也不做有歧义的猜测。
func resolveRoute(model, headerProvider string) (provider, realm, bare string) {
	provider, model = splitProviderPrefix(model)
	realm, bare = resolveModel(model)
	if provider == "" {
		provider = providerFromHeader(headerProvider)
	}
	// 兜底：无前缀、无头 → workbuddy。存量零回归的关键，
	// 缺了它会让老调用拿到空串并在选号时匹配不到任何账号。
	if provider == "" {
		provider = providerWorkbuddy
	}
	return provider, realm, bare
}

// splitProviderPrefix 剥离模型名最前面的 provider 段，**两种分隔符都认**：
//
//	"zcode/glm-5.3-flash"  斜杠式（gw2api 历史形态，调用方手写方便）
//	"zcode:glm-5.3-flash"  冒号式（/v1/models 的统一目录就用这个）
//
// 为什么两种都要：统一目录里 CodeBuddy 条目是 "cn:glm-5.2"（realm 前缀），
// 如果再给非 CodeBuddy 条目用 "/"，客户端就得记住"哪种前缀用哪种分隔符"，
// 违背"只用 model-id 区分"的初衷。两种都接受，客户端拿到什么 id 就填什么。
//
// 非已知枚举（含别名）一律原样返回，避免把 "org/model" 这类合法模型名误伤。
// 注意 realm（cn/global）不是 provider，这里刻意不匹配，交给 resolveModel。
func splitProviderPrefix(model string) (provider, rest string) {
	m := strings.TrimSpace(model)
	for _, sep := range []byte{'/', ':'} {
		i := strings.IndexByte(m, sep)
		if i <= 0 {
			continue
		}
		head := strings.ToLower(m[:i])
		switch head {
		case providerWorkbuddy:
			return providerWorkbuddy, m[i+1:]
		case providerZCode, "zai", "bigmodel":
			return providerZCode, m[i+1:]
		case providerTrae, "traecode":
			return providerTrae, m[i+1:]
		}
	}
	return "", model
}

// providerFromHeader 读取 X-Provider 头（与 gw2api 同名同义，便于复用调用方）。
// 非法取值返回空串，继续走下一级（兜底 workbuddy）。
func providerFromHeader(h string) string {
	switch strings.ToLower(strings.TrimSpace(h)) {
	case providerZCode, "zai":
		return providerZCode
	case providerTrae, "traecode":
		return providerTrae
	case providerWorkbuddy:
		return providerWorkbuddy
	}
	return ""
}
