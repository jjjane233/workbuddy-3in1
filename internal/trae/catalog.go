package trae

// Trae CN 客户端内置的模型目录（**权威基线**）。
//
// ── 数据来源（可复现）──────────────────────────────────────────────
//
//	%APPDATA%\Trae CN\User\globalStorage\state.vscdb
//	  → ItemTable → key "<uid>_AI.agent.model.model_list_map"          （value 是 7 段 JSON）
//	  → 逆向 ai-modules-chat/dist/index.mjs @11692170 fetchModels()
//	    调 traeApiPort.model.listModels({functions:"solo_agent_remote,solo_work_remote,solo_design_remote",
//	                                    show_custom_model:true})
//	    → 服务端 model_list RPC 返回的 data.list
//
// 该 map 的 7 个 section 是**按 agent 场景**分的（同一个模型可能出现在多段里）：
//
//	chat_v3           对话主力（18 条：Seed-Evolving / GLM-5.3 系 / Kimi-K3 / Qwen3.8 …）
//	builder           Builder 场景
//	builder_v3        Builder v3
//	solo_agent        Solo Agent
//	code_reviewer     代码评审
//	code_review_summary
//	refactor          重构子角色（scoper/finder/planner/incrementer，都指向 glm-4.7）
//
// 合并规则：同名条目取 **chat_v3 优先**的那一份（chat_v3 的能力字段最全：
// 倍率 / 上下文 max+default / 档位 / max_tokens 都有），其余段的独有条目补齐。
// 结果 31 条，就是 Trae 客户端 UI 里能看到的全部。
//
// ── 为什么硬编码 ─────────────────────────────────────────────────
//
//  1. /api/ide/v1/model_list 只认 type=chat / builder 两种枚举（实测 chat_v3、
//     solo_agent、builder_v3 一律 400 code 4001 "param is invalid"），所以
//     chat_v3 那 18 条**永远无法**从上游目录拿到；
//  2. 上游 type=chat 只回 4 条平台模型（seed_m8 / Doubao_1_5_thinking_pro /
//     deepseek-V3 / deepseek-V3-0324），且**只有 name/display_name/status**，
//     不带倍率、不带上下文、不带档位；
//  3. 所以面板要完整呈现「Trae 里到底有哪些模型、各自什么倍率/档位」，
//     只能靠这份客户端目录当元数据字典。
//
// 客户端升级后目录会变：此处是 2026-10-08 快照（Trae CN 1.107.1）。运行时
// 取到真实 model_list 时，同名条目的实时值会覆盖这里的值（见 server.traeModels）。
//
// ── 字段口径 ─────────────────────────────────────────────────────
//
//	Rate          ← features.consumption_rate.data.rate          （Trae UI 显示的倍率）
//	DiscountRate  ← features.discount.data.consumption_rate
//	                / features.activity_discount.data.current.consumption_rate
//	                （会员/限时折扣后的实际倍率；0 表示无折扣）
//	ContextMax    ← context_window_size.max[0]（客户端置 null 表示未公开上限，取 0）
//	ContextDefault← context_window_size.default
//	MaxOutput     ← max_tokens
//	MaxTurn       ← max_turn
//	Tools         ← custom_config.native_function_call
//	Efforts       ← reasoning_effort_config.options，light/extra_high 已归一为
//	                low/xhigh，与面板「思考档位」筛选器同一套词汇
//	Section       ← 该条目的来源 section（面板可展示"仅 Builder 场景"之类）
type CatalogModel struct {
	ID             string
	Name           string
	Vendor         string
	Rate           string
	DiscountRate   string
	ContextMax     int64
	ContextDefault int64
	MaxOutput      int64
	MaxTurn        int64
	Reasoning      bool
	Multimodal     bool
	Tools          bool
	Efforts        []string
	DefaultEffort  string
	IsDefault      bool
	Section        string
}

// IsClientOnlyEntry 判定一条目录条目是否「只有 Trae 客户端能调、上游 API 不认」。
//
// 背景：面板 traeModels 会把客户端内置表里**不在上游可用集**的条目以
// available=false + client_only=true 的形式补在后面，目的是让用户看见
// 「Trae 客户端里有的模型这里为什么没有」（GLM-5.3 / Kimi-K3 / Qwen3.8-Max 等）。
// 这些条目经 /api/ide/v1/chat 与 llm_raw_chat 实测一律回
// code 4023 "model is unknown"，即**服务端不认**。
//
// 因此：
//   · 面板展示（单 provider 视图）要保留它们，否则用户以为网关吞了模型；
//   · 对外目录 /v1/models 与面板「全部上游」视图必须**滤掉**它们，
//     否则客户端会照着目录去调，拿到 100% 的 4023 —— 那才是真的骗人。
//
// 判定按 available=false 或 client_only=true 任一命中（两个标记同源写入，
// 任一被单独设置时也成立）。
func IsClientOnlyEntry(m map[string]any) bool {
	if v, ok := m["available"].(bool); ok && !v {
		return true
	}
	if v, ok := m["client_only"].(bool); ok && v {
		return true
	}
	return false
}

// CallableOnly 过滤掉 client-only 条目，只保留上游 /chat 真正可调的。
// 入参不被修改；nil 入参返回 nil。
func CallableOnly(models []map[string]any) []map[string]any {
	if models == nil {
		return nil
	}
	out := make([]map[string]any, 0, len(models))
	for _, m := range models {
		if IsClientOnlyEntry(m) {
			continue
		}
		out = append(out, m)
	}
	return out
}

// Catalog 返回 Trae CN 客户端内置模型目录的副本。
func Catalog() []CatalogModel {
	out := make([]CatalogModel, len(catalogModels))
	copy(out, catalogModels)
	return out
}

var catalogModels = []CatalogModel{
	{ID: "Doubao-Seed-Evolving", Name: "Seed-Evolving", Vendor: "Doubao", Rate: "0.08", DiscountRate: "0.08", ContextMax: 1000000, ContextDefault: 256000, MaxOutput: 64000, MaxTurn: 70, Reasoning: true, Multimodal: true, Tools: true, Section: "chat_v3"},
	{ID: "Doubao-Seed-2.1-Pro", Name: "Seed-2.1-Pro-0915", Vendor: "Doubao", Rate: "0.08", DiscountRate: "0.08", ContextMax: 1000000, ContextDefault: 256000, MaxOutput: 64000, MaxTurn: 70, Reasoning: true, Multimodal: true, Tools: true, Efforts: []string{"low", "high"}, DefaultEffort: "high", Section: "chat_v3"},
	{ID: "Doubao-Seed-2.1-Turbo", Name: "Seed-2.1-Turbo", Vendor: "Doubao", Rate: "0.2", DiscountRate: "0.2", ContextDefault: 256000, MaxOutput: 32000, MaxTurn: 70, Reasoning: true, Multimodal: true, Tools: true, Efforts: []string{"low", "high"}, DefaultEffort: "high", Section: "chat_v3"},
	{ID: "Doubao-Seed-Code", Name: "Seed-Code", Vendor: "Doubao", Rate: "0.06", DiscountRate: "0.06", ContextDefault: 256000, MaxOutput: 32000, MaxTurn: 70, Reasoning: true, Multimodal: true, Tools: true, Efforts: []string{"low", "high"}, DefaultEffort: "high", IsDefault: true, Section: "chat_v3"},
	{ID: "step-5-preview", Name: "Step-5-Preview", Vendor: "StepFun", Rate: "0.48", ContextMax: 1000000, ContextDefault: 116000, MaxOutput: 64000, MaxTurn: 70, Reasoning: true, Multimodal: true, Tools: true, Efforts: []string{"low", "high"}, DefaultEffort: "high", Section: "chat_v3"},
	{ID: "glm-5.3-flashx", Name: "GLM-5.3-FlashX", Vendor: "Zhipu", Rate: "0.31", ContextMax: 1000000, ContextDefault: 116000, MaxOutput: 64000, MaxTurn: 70, Reasoning: true, Multimodal: true, Tools: true, Efforts: []string{"low", "high", "xhigh"}, DefaultEffort: "high", Section: "chat_v3"},
	{ID: "glm-5.3-flash", Name: "GLM-5.3-Flash", Vendor: "Zhipu", Rate: "0.06", ContextMax: 1000000, ContextDefault: 116000, MaxOutput: 64000, MaxTurn: 70, Reasoning: true, Multimodal: true, Tools: true, Efforts: []string{"low", "high", "xhigh"}, DefaultEffort: "high", Section: "chat_v3"},
	{ID: "glm-5.3", Name: "GLM-5.3", Vendor: "Zhipu", Rate: "0.78", DiscountRate: "0.39", ContextMax: 1000000, ContextDefault: 116000, MaxOutput: 64000, MaxTurn: 70, Reasoning: true, Tools: true, Efforts: []string{"low", "high", "xhigh"}, DefaultEffort: "high", Section: "chat_v3"},
	{ID: "glm-5.2", Name: "GLM-5.2", Vendor: "Zhipu", Rate: "0.78", DiscountRate: "0.39", ContextMax: 1000000, ContextDefault: 116000, MaxOutput: 64000, MaxTurn: 70, Reasoning: true, Tools: true, Efforts: []string{"high", "xhigh"}, DefaultEffort: "high", Section: "chat_v3"},
	{ID: "deepseek-v4.1-flash", Name: "DeepSeek-V4-Flash 正式版", Vendor: "DeepSeek", Rate: "0.15", DiscountRate: "0.15", ContextMax: 1000000, ContextDefault: 116000, MaxOutput: 64000, MaxTurn: 70, Reasoning: true, Multimodal: true, Tools: true, Efforts: []string{"low", "high", "xhigh"}, DefaultEffort: "high", Section: "chat_v3"},
	{ID: "DeepSeek-V4-Flash-Official", Name: "DeepSeek-V4-Flash 正式版", Vendor: "DeepSeek", Rate: "0.2", DiscountRate: "0.2", ContextMax: 1000000, ContextDefault: 116000, MaxOutput: 64000, MaxTurn: 70, Reasoning: true, Tools: true, Efforts: []string{"low", "high", "xhigh"}, DefaultEffort: "high", Section: "chat_v3"},
	{ID: "DeepSeek-V4-Pro-Official", Name: "DeepSeek-V4-Pro 正式版", Vendor: "DeepSeek", Rate: "0.72", DiscountRate: "0.72", ContextMax: 1000000, ContextDefault: 116000, MaxOutput: 64000, MaxTurn: 70, Reasoning: true, Tools: true, Efforts: []string{"low", "high", "xhigh"}, DefaultEffort: "high", Section: "chat_v3"},
	{ID: "kimi-k3", Name: "Kimi-K3", Vendor: "Moonshot", Rate: "1.83", ContextMax: 1000000, ContextDefault: 200000, MaxOutput: 64000, MaxTurn: 200, Reasoning: true, Multimodal: true, Tools: true, Efforts: []string{"low", "high", "xhigh"}, DefaultEffort: "xhigh", Section: "chat_v3"},
	{ID: "kimi-k2.8-preview", Name: "Kimi-K2.8-Preview", Vendor: "Moonshot", Rate: "0.98", ContextMax: 1000000, ContextDefault: 200000, MaxOutput: 64000, MaxTurn: 200, Reasoning: true, Multimodal: true, Tools: true, Efforts: []string{"low", "high", "xhigh"}, DefaultEffort: "xhigh", Section: "chat_v3"},
	{ID: "minimax-m3", Name: "MiniMax-M3", Vendor: "MiniMax", Rate: "0.26", ContextMax: 1000000, ContextDefault: 116000, MaxOutput: 64000, MaxTurn: 70, Reasoning: true, Multimodal: true, Tools: true, Section: "chat_v3"},
	{ID: "qwen3.8-flash", Name: "Qwen3.8-Flash", Vendor: "Alibaba", Rate: "0.08", ContextMax: 1000000, ContextDefault: 200000, MaxOutput: 64000, MaxTurn: 200, Reasoning: true, Multimodal: true, Tools: true, Efforts: []string{"low", "high", "xhigh"}, DefaultEffort: "high", Section: "chat_v3"},
	{ID: "qwen3.8-max", Name: "Qwen3.8-Max", Vendor: "Alibaba", Rate: "1.5", ContextMax: 1000000, ContextDefault: 200000, MaxOutput: 64000, MaxTurn: 200, Reasoning: true, Multimodal: true, Tools: true, Efforts: []string{"low", "high", "xhigh"}, DefaultEffort: "high", Section: "chat_v3"},
	{ID: "qwen-3.7-plus", Name: "Qwen3.7-Plus", Vendor: "Alibaba", Rate: "0.25", ContextMax: 1000000, ContextDefault: 200000, MaxOutput: 64000, MaxTurn: 200, Reasoning: true, Multimodal: true, Tools: true, Section: "chat_v3"},
	{ID: "custom_openai_compatible//step-5-preview", Name: "step-5-preview", Vendor: "StepFun", MaxTurn: 500, Multimodal: true, Section: "chat_v3"},
	{ID: "custom_openai_compatible//deepseek-flash", Name: "deepseek-flash", Vendor: "DeepSeek", MaxTurn: 500, Multimodal: true, Section: "chat_v3"},
	{ID: "kimi-k2.7-code", Name: "Kimi-K2.7-Code", Vendor: "Moonshot", Rate: "0.83", ContextDefault: 116000, MaxOutput: 16000, MaxTurn: 70, Reasoning: true, Multimodal: true, Tools: true, Section: "builder"},
	{ID: "minimax-m2.7", Name: "MiniMax-M2.7", Vendor: "MiniMax", Rate: "0.27", ContextDefault: 116000, MaxOutput: 16000, MaxTurn: 70, Reasoning: true, Tools: true, Section: "builder_v3"},
	{ID: "kimi-k2.6", Name: "Kimi-K2.6", Vendor: "Moonshot", Rate: "0.75", ContextDefault: 116000, MaxOutput: 16000, MaxTurn: 70, Reasoning: true, Multimodal: true, Tools: true, Section: "builder_v3"},
	{ID: "qwen-3.6-plus", Name: "Qwen3.6-Plus", Vendor: "Alibaba", Rate: "0.26", ContextDefault: 116000, MaxOutput: 16000, MaxTurn: 70, Reasoning: true, Multimodal: true, Tools: true, Section: "builder_v3"},
	{ID: "deepseek-V3.1", Name: "DeepSeek-V3.1-Terminus", Vendor: "DeepSeek", Rate: "0.5", ContextDefault: 136192, MaxOutput: 8192, MaxTurn: 35, Reasoning: true, Tools: true, Section: "code_reviewer"},
	{ID: "code-review-judge", Name: "DeepSeek-V3.1-Terminus", Vendor: "DeepSeek", ContextDefault: 112000, MaxOutput: 16000, MaxTurn: 70, Reasoning: true, Tools: true, Section: "code_reviewer"},
	{ID: "glm-4.7", Name: "GLM-4.7", Vendor: "Zhipu", Rate: "0.38", ContextDefault: 116000, MaxOutput: 16000, MaxTurn: 70, Reasoning: true, Tools: true, Section: "code_review_summary"},
	{ID: "refactor_scoper", Name: "glm-4.7", Vendor: "Zhipu", Rate: "0.38", ContextDefault: 116000, MaxOutput: 16000, MaxTurn: 70, Reasoning: true, Tools: true, Section: "refactor"},
	{ID: "refactor_finder", Name: "glm-4.7", Vendor: "Zhipu", Rate: "0.38", ContextDefault: 116000, MaxOutput: 16000, MaxTurn: 70, Reasoning: true, Tools: true, Section: "refactor"},
	{ID: "refactor_planner", Name: "glm-4.7", Vendor: "Zhipu", Rate: "0.38", ContextDefault: 116000, MaxOutput: 16000, MaxTurn: 70, Reasoning: true, Tools: true, Section: "refactor"},
	{ID: "refactor_incrementer", Name: "glm-4.7", Vendor: "Zhipu", Rate: "0.38", ContextDefault: 116000, MaxOutput: 16000, MaxTurn: 70, Reasoning: true, Tools: true, Section: "refactor"},
	// ---- 以下 4 条是**上游 API 仍认、客户端 UI 已下架**的老平台模型 ----
	//
	// 它们不在 model_list_map（客户端 v2 目录，31 条）里，但
	// /api/ide/v1/model_list?type=chat 对一个 CN 账号仍然回这 4 个：
	//
	//	seed_m8                 display=Doubao-1.5-pro
	//	Doubao_1_5_thinking_pro display=Doubao-1.5-thinking-pro
	//	deepseek-V3             display=DeepSeek-V3
	//	deepseek-V3-0324        display=DeepSeek-V3-0324
	//
	// 上游那份响应**只有 name/display_name/status**，不带倍率与上下文，所以下面
	// 这些元数据来自同族条目对齐（Doubao 取 Seed-Code 的 0.06；DeepSeek-V3 取
	// 老客户端 deepseek-v3 的 0.5 与 136192/8192/35），并在 Section 标 "upstream"
	// 以便区分「实测推断」与「客户端明示」。
	{ID: "Doubao-1.5-pro", Name: "Doubao-1.5-pro", Vendor: "Doubao", Rate: "0.06", ContextDefault: 116000, MaxOutput: 16000, MaxTurn: 70, Tools: true, IsDefault: true, Section: "upstream"},
	{ID: "Doubao-1.5-thinking-pro", Name: "Doubao-1.5-thinking-pro", Vendor: "Doubao", Rate: "0.06", ContextDefault: 116000, MaxOutput: 16000, MaxTurn: 70, Reasoning: true, Tools: true, Section: "upstream"},
	{ID: "DeepSeek-V3", Name: "DeepSeek-V3", Vendor: "DeepSeek", Rate: "0.5", ContextDefault: 136192, MaxOutput: 8192, MaxTurn: 35, Tools: true, Section: "upstream"},
	{ID: "DeepSeek-V3-0324", Name: "DeepSeek-V3-0324", Vendor: "DeepSeek", Rate: "0.5", ContextDefault: 136192, MaxOutput: 8192, MaxTurn: 35, Tools: true, Section: "upstream"},
}
