package zcode

import "strings"

// ZCode 可调（GLM）模型的内置基线目录。
//
// 为什么需要它：上游 /api/v1/client/configs 只列了 GLM-5.2 / GLM-5-Turbo 两条，
// billing 的 capabilities 只喂套餐授权的那一个模型。于是「面板/‑v1/models 的 ZCode
// 段」远小于「凭一把 <keyId>.<secret> 实际能调的模型」——用户看得到 GLM-5.3，
// 却可能因为不在目录里而选不到。
//
// 这张表是 **实测** 出来的：用本机成品 key 打
// https://open.bigmodel.cn/api/anthropic/v1/messages，逐个模型确认 200 出正文
// （2026-10-07）。因此它是「能调」的白名单，而不是客户端内置目录的照抄。
//
// 目录合并规则（见 server.zcodeMergeBuiltin）：内置只补空缺，不覆盖上游实时真值。
type BuiltinModel struct {
	ID            string
	Name          string
	ContextWindow int64
	MaxOutput     int64
	Vision        bool
	// Efforts 思考档位（面板统一口径 low/high/max）。
	//
	// 权威来源是 ZCode 客户端自己的模型声明（%USERPROFILE%\.zcode\v2\config.json）：
	//
	//	"reasoning": {"enabled": true, "variants": ["low","max","high"],
	//	              "defaultVariant": "max"}
	//
	// 上游也印证同一取值域：把 thinking.type 置为 disabled 时
	// open.bigmodel.cn 回 400 code 1210「该模型始终思考，不支持关闭思考；
	// 请使用 low、high 或 max。」（2026-10-08 实测）
	//
	// 空 = 上游未声明档位（面板不展示档位列，不编造）。
	Efforts []string
	// DefaultEffort 上游声明的默认档（defaultVariant）。
	DefaultEffort string
	// AlwaysThinking 该模型思考常开、不接受关闭（GLM-5.3 系列即如此）。
	//
	// 判定依据（2026-10-08 实测，https://open.bigmodel.cn/api/anthropic/v1/messages）：
	//
	//	GLM-5.3  + thinking:{type:"disabled"} → 400 code 1210
	//	  「该模型始终思考，不支持关闭思考；请使用 low、high 或 max。」
	AlwaysThinking bool
	// ThinkingToggle 该模型支持 Anthropic 原生 **思考开关**（thinking.type=enabled/disabled）。
	//
	// 与 Efforts（output_config.effort 档位）是**两种不同的能力**，必须分开记：
	//
	//	GLM-5.2 / GLM-5-Turbo / glm-4.7  有开关：baseline 不出 thinking，
	//	   加 thinking:{type:"enabled"} 立刻出 thinking 块，disabled 关掉；
	//	   但它们**没有档位**（output_config.effort 对思考长度无稳定剂量效应）。
	//	GLM-5.3 系列                       有档位（low/high/max）但**没有开关**
	//	   （始终思考，disabled 直接 400）。
	//
	// 面板据此把「思考开关」与「思考档位」显示成两栏，用户才知道 ZCode 那个
	// 开关到底对哪些模型有效（答案是：不是全部）。
	ThinkingToggle bool
	// Note 说明该模型的定位（面板展示用）。
	Note string
}

// BuiltinCallableModels 返回实测可调的 ZCode 模型基线。
//
// 顺序即面板展示顺序：先旗舰/编码，再多模态，最后轻量。
func BuiltinCallableModels() []BuiltinModel {
	return []BuiltinModel{
		// ── 旗舰 / 编码（1M 上下文，128k 输出）────────────────────────────
		{ID: "GLM-5.3", Name: "GLM-5.3", ContextWindow: 1000000, MaxOutput: 128000, Note: "旗舰，文本",
			Efforts: []string{"low", "high", "max"}, DefaultEffort: "max", AlwaysThinking: true},
		{ID: "GLM-5.3-Flash", Name: "GLM-5.3-Flash", ContextWindow: 1000000, MaxOutput: 128000, Note: "旗舰 Flash",
			Efforts: []string{"low", "high", "max"}, DefaultEffort: "max", AlwaysThinking: true},
		{ID: "GLM-5.3-FlashX", Name: "GLM-5.3-FlashX", ContextWindow: 1000000, MaxOutput: 128000, Note: "FlashX"},
		{ID: "GLM-5.2", Name: "GLM-5.2", ContextWindow: 1000000, MaxOutput: 128000,
			Note: "上一代旗舰（可开关思考）", ThinkingToggle: true},
		{ID: "GLM-5-Turbo", Name: "GLM-5-Turbo", ContextWindow: 1000000, MaxOutput: 128000,
			Note: "Turbo（可开关思考）", ThinkingToggle: true},
		{ID: "GLM-5.1", Name: "GLM-5.1", ContextWindow: 1000000, MaxOutput: 128000},
		{ID: "GLM-5", Name: "GLM-5", ContextWindow: 1000000, MaxOutput: 128000},
		// ── GLM-4.7 / 4.6 系 ──────────────────────────────────────────
		{ID: "glm-4.7", Name: "GLM-4.7", ContextWindow: 1000000, MaxOutput: 128000, ThinkingToggle: true},
		{ID: "glm-4.7-flash", Name: "GLM-4.7-Flash", ContextWindow: 1000000, MaxOutput: 128000,
			Note: "轻量（可开关思考）", ThinkingToggle: true},
		{ID: "glm-4.6", Name: "GLM-4.6", ContextWindow: 1000000, MaxOutput: 128000, ThinkingToggle: true},
		// ── GLM-4.5 系 ────────────────────────────────────────────────
		{ID: "glm-4.5", Name: "GLM-4.5", ContextWindow: 128000, MaxOutput: 32000},
		{ID: "glm-4.5-air", Name: "GLM-4.5-Air", ContextWindow: 128000, MaxOutput: 32000, Note: "轻量"},
		{ID: "glm-4.5-airx", Name: "GLM-4.5-AirX", ContextWindow: 128000, MaxOutput: 32000, Note: "轻量加速"},
		{ID: "glm-4.5-x", Name: "GLM-4.5-X", ContextWindow: 128000, MaxOutput: 32000, Note: "加速"},
		{ID: "glm-4.5-flash", Name: "GLM-4.5-Flash", ContextWindow: 128000, MaxOutput: 32000, Note: "免费 Flash"},
		// ── 多模态（视觉）──────────────────────────────────────────────
		{ID: "glm-5v-turbo", Name: "GLM-5V-Turbo", ContextWindow: 1000000, MaxOutput: 128000, Vision: true, Note: "多模态"},
		{ID: "glm-4.6v", Name: "GLM-4.6V", ContextWindow: 128000, MaxOutput: 32000, Vision: true, Note: "多模态"},
		{ID: "glm-4.6v-flash", Name: "GLM-4.6V-Flash", ContextWindow: 128000, MaxOutput: 32000, Vision: true, Note: "多模态轻量"},
		{ID: "glm-4.5v", Name: "GLM-4.5V", ContextWindow: 128000, MaxOutput: 32000, Vision: true, Note: "多模态"},
		{ID: "glm-4v-plus", Name: "GLM-4V-Plus", ContextWindow: 128000, MaxOutput: 32000, Vision: true, Note: "多模态"},
		{ID: "glm-4v-plus-0111", Name: "GLM-4V-Plus-0111", ContextWindow: 128000, MaxOutput: 32000, Vision: true, Note: "多模态"},
		{ID: "glm-4v", Name: "GLM-4V", ContextWindow: 128000, MaxOutput: 32000, Vision: true, Note: "多模态"},
		{ID: "glm-4v-flash", Name: "GLM-4V-Flash", ContextWindow: 128000, MaxOutput: 32000, Vision: true, Note: "多模态免费"},
		// ── GLM-4 系（legacy，仍可调）──────────────────────────────────
		{ID: "glm-4-plus", Name: "GLM-4-Plus", ContextWindow: 128000, MaxOutput: 32000},
		{ID: "glm-4-plus-0111", Name: "GLM-4-Plus-0111", ContextWindow: 128000, MaxOutput: 32000},
		{ID: "glm-4-air", Name: "GLM-4-Air", ContextWindow: 128000, MaxOutput: 32000},
		{ID: "glm-4-airx", Name: "GLM-4-AirX", ContextWindow: 128000, MaxOutput: 32000},
		{ID: "glm-4-flashx", Name: "GLM-4-FlashX", ContextWindow: 128000, MaxOutput: 32000},
		{ID: "glm-4-flash-250414", Name: "GLM-4-Flash-250414", ContextWindow: 128000, MaxOutput: 32000},
		// ── Z1 推理系 ─────────────────────────────────────────────────
		{ID: "glm-z1-air", Name: "GLM-Z1-Air", ContextWindow: 128000, MaxOutput: 32000, Note: "推理"},
		{ID: "glm-z1-airx", Name: "GLM-Z1-AirX", ContextWindow: 128000, MaxOutput: 32000, Note: "推理加速"},
		{ID: "glm-z1-flash", Name: "GLM-Z1-Flash", ContextWindow: 128000, MaxOutput: 32000, Note: "推理免费"},
	}
}

// EffortSpecs 返回「模型名（小写）→ 档位声明」索引。
//
// 给 server 层按模型名（可能来自 billing capabilities 的裸名，如
// "glm-5.3-flash"）补齐面板条目用；BuiltinCallableModels 里的 ID 大小写
// 不统一（GLM-5.3 vs glm-4.7），所以键统一小写。
func EffortSpecs() map[string]BuiltinModel {
	all := BuiltinCallableModels()
	out := make(map[string]BuiltinModel, len(all))
	for _, b := range all {
		if len(b.Efforts) == 0 && !b.AlwaysThinking && !b.ThinkingToggle {
			continue
		}
		out[strings.ToLower(b.ID)] = b
	}
	return out
}
