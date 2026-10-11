package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/jjjane233/workbuddy-3in1/internal/auth"
	"github.com/jjjane233/workbuddy-3in1/internal/panel"
	"github.com/jjjane233/workbuddy-3in1/internal/trae"
	"github.com/jjjane233/workbuddy-3in1/internal/zcode"
)

// 本文件是 server → panel 的适配层：把已有的 Trae*/ZCode* 富结果方法，
// 收敛成 panel.UpstreamOps 要求的基本类型签名。
//
// Go 的接口是结构性的——Handler 只需方法签名一致即可满足 panel.UpstreamOps，
// **无需 import panel**，因此不会形成 panel ↔ server 的依赖环
// （cmd/server 同时导入两者，构造时由它完成注入）。
//
// 方法名（Checkin/Balance/Activity/Keepalive/Claim）与接口逐字对应，
// 编译期由 main.go 的 pn.SetUpstreamOps(h) 自动校验。

// Checkin 签到：Trae 走签到接口；ZCode 没有签到体系，返回可展示的提示而非错误。
func (h *Handler) Checkin(ctx context.Context, a *auth.Auth) (bool, string) {
	switch a.ProviderValue() {
	case auth.ProviderTrae:
		res := h.TraeCheckin(ctx, a)
		return res.Claimed || res.CheckedIn, res.Message
	case auth.ProviderZCode:
		return false, "ZCode 无签到体系，请用「余额」刷新积分"
	default:
		return false, "该账号上游不支持签到"
	}
}

// Balance 刷新余额/额度：Trae 读 ide_user_ent_usage，ZCode 读 billing。
func (h *Handler) Balance(ctx context.Context, a *auth.Auth) (int64, int64, error) {
	switch a.ProviderValue() {
	case auth.ProviderTrae:
		u, err := h.TraeUsage(ctx, a)
		if err != nil {
			return 0, 0, err
		}
		return u.Remaining, u.Total, nil
	case auth.ProviderZCode:
		b, err := h.ZCodeBalance(ctx, a)
		if err != nil {
			return 0, 0, err
		}
		remain, total, _ := b.Remaining()
		return remain, total, nil
	default:
		return 0, 0, errUnsupportedProvider
	}
}

// Activity 领取商业活动（仅 Trae；ZCode 的领取走 Claim）。
func (h *Handler) Activity(ctx context.Context, a *auth.Auth) (int, error) {
	if a.ProviderValue() != auth.ProviderTrae {
		return 0, errUnsupportedProvider
	}
	res, err := h.TraeActivity(ctx, a)
	if err != nil {
		return 0, err
	}
	return len(res.Claimed), nil
}

// ActivityScan 立刻跑一轮上游活动巡检（面板「巡检领取」按钮 / 后台排程共用）。
//
// Trae：跑完整的 TraeActivity（benefit 权益 + 裂变领取 + 结果分类）。
// ZCode：先看 Preview 有没有可领方案，有就走 ClaimAll 全领。
func (h *Handler) ActivityScan(ctx context.Context, a *auth.Auth) (string, error) {
	switch a.ProviderValue() {
	case auth.ProviderTrae:
		res, err := h.TraeActivity(ctx, a)
		if err != nil {
			return "", err
		}
		return res.Summary(), nil
	case auth.ProviderZCode:
		plans, err := h.ZCodePreview(ctx, a)
		if err != nil {
			return "方案查询失败: " + err.Error(), nil
		}
		if len(plans) == 0 {
			return "暂无可领方案（已巡检）", nil
		}
		names := make([]string, 0, len(plans))
		for _, p := range plans {
			names = append(names, firstNonEmpty(p.Name, p.PlanID))
		}
		res := h.ZCodeClaimAll(ctx, a)
		if res.OK {
			return "已领取: " + res.Message, nil
		}
		return "可领 " + strconv.Itoa(len(plans)) + " 项（" + strings.Join(names, "、") + "），未领到: " + res.Message, nil
	default:
		return "", errUnsupportedProvider
	}
}

// Keepalive 保活（仅 Trae）。
func (h *Handler) Keepalive(ctx context.Context, a *auth.Auth) error {
	if a.ProviderValue() != auth.ProviderTrae {
		return errUnsupportedProvider
	}
	return h.TraeKeepalive(ctx, a)
}

// Claim 领取积分（仅 ZCode）。
func (h *Handler) Claim(ctx context.Context, a *auth.Auth) (bool, string) {
	if a.ProviderValue() != auth.ProviderZCode {
		return false, "该上游没有可领取的积分方案"
	}
	res := h.ZCodeClaim(ctx, a, "")
	return res.OK, res.Message
}

// errUnsupportedProvider 当前账号的上游不支持该操作（与「出错」区分开：
// 调用方应提示不支持，而不是把账号标记为异常）。
var errUnsupportedProvider = errStr("该上游不支持此操作")

type errStr string

func (e errStr) Error() string { return string(e) }

// Models 面板「模型与档位」的 provider 分流实现。
//
//	workbuddy : 不在这里（面板仍走 upstream.Client.FetchModels / FetchGlobalModelInfos，
//	            只有它是"真实上游目录"，条目字段最富）。
//	trae      : cli.ModelList("chat") 的 model_configs；登录态失效时回落静态已知名单
//	            （docs/USAGE.md 记录的 trae/* 四款），保证面板不空白。
//	zcode     : 匿名可访问的 /api/v1/client/configs（不需要账号，也不需要签名），
//	            按 modelId 去重，附上 context/maxOutput/default/priority。
func (h *Handler) Models(ctx context.Context, provider string) ([]map[string]any, error) {
	switch provider {
	case auth.ProviderTrae:
		return h.traeModels(ctx)
	case auth.ProviderZCode:
		return h.zcodeModels(ctx)
	default:
		return nil, errUnsupportedProvider
	}
}

// ModelsFor 同 Models，但把账号传给需要登录态的 provider。
func (h *Handler) ModelsFor(ctx context.Context, provider string, a *auth.Auth) ([]map[string]any, error) {
	if provider == auth.ProviderZCode {
		return h.zcodeModelsWithAccount(ctx, a)
	}
	return h.Models(ctx, provider)
}

// traeModels 拉 Trae 模型目录。
//
// 优先级：
//  1. 登录态可用 → /api/ide/v1/model_list 得到 available=true 段（实时、权威）
//  2. 再把 trae.Catalog()（客户端内置目录）里**不在**上面的条目以
//     available=false + client_only=true 补在后面（含倍率/上下文/档位），
//     如实标注「仅客户端可用」，避免用户以为网关吞了模型
//
// 为什么不再用那 4 个字符串兜底：旧兜底只有 seed_m8 /
// Doubao_1_5_thinking_pro / deepseek-V3 / deepseek-V3-0324，是 Trae 老版目录，
// 与客户端现在真正可用的 20+ 个模型（Seed-Evolving / GLM-5.3 / Kimi-K3 等）
// 完全对不上，且不带任何能力字段，用户看到的只有一排 "—"。
func (h *Handler) traeModels(ctx context.Context) ([]map[string]any, error) {
	// 实时 model_list 是**唯一**的可用集来源；内置表只当元数据字典。
	//
	// 为什么不再拿内置表打底（2026-10-06 实测修正）：
	// trae.Catalog() 是 Trae **客户端**里的模型表（33 条，含 glm-5.3/kimi-k3/
	// qwen3.8-max 等），不是上游**当前账号**能调的模型。线上实测：
	// /api/ide/v1/model_list 的 type=chat 只回 6 条，其中 step-5-preview /
	// deepseek-flash 是客户端自带的自定义模型（带 ak，服务端不认），
	// 真正能调通的只有 seed_m8 / Doubao_1_5_thinking_pro / deepseek-V3 /
	// deepseek-V3-0324 四个。把内置表当打底列出来，用户选 glm-5.3-flash 会拿到
	// "[trae error] model is unknown" —— 列表必须等于「能调」，否则等于骗人。
	//
	// 元数据（倍率 / 上下文 / 档位）仍从内置表按 name 取，因为上游 model_list
	// 只回 name/display_name/status，不带消费倍率。
	meta := make(map[string]trae.CatalogModel)
	for _, c := range trae.Catalog() {
		meta[strings.ToLower(c.ID)] = c
		meta[strings.ToLower(c.Name)] = c
	}
	out := make([]map[string]any, 0, 16)

	acct := firstTraeAccount(h)
	if acct == nil {
		// 无号时返回**完整的客户端内置目录**（全部标 client-only）。
		//
		// 为什么改（2026-10-08 用户反馈「TraeCode 为什么就这几个模型」）：
		// 旧行为是「无号就不列」，面板上直接空白/只剩几行；但用户在 Trae
		// 客户端里明明能看到 20+ 个模型，观感上就是「网关把模型吞了」。
		// 内置目录是客户端的权威口径，没有帐号也照样能展示，只是必须标成
		// client-only（available=false），否则用户会照它去调、拿到 4023。
		return traeClientOnlyCatalog(), nil
	}
	cli := trae.New(acct, trae.Options{HTTPClient: h.cfg.UpstreamHTTP})
	raw, err := cli.ModelList(ctx, "chat")
	if err != nil {
		logUpstream("trae model_list: %v（回落客户端内置目录）", err)
		// 登录态失效：同样回落到完整客户端目录，别让面板空着。
		return traeClientOnlyCatalog(), nil
	}
	var resp struct {
		ModelConfigs []map[string]any `json:"model_configs"`
	}
	if json.Unmarshal(raw, &resp) != nil || len(resp.ModelConfigs) == 0 {
		logUpstream("trae model_list: 响应无可解析条目（回落客户端内置目录）")
		return traeClientOnlyCatalog(), nil
	}
	for _, m := range resp.ModelConfigs {
		id := strField(m, "name", "model_name", "model_id", "modelName")
		if id == "" {
			continue
		}
		// custom_openai_compatible 且带 ak 的是客户端本地自定义模型：
		// model_list 会列出来，但服务端不认（实测 model is unknown）。
		// 只认平台模型（不带 provider 或非 custom）。
		if prov := strField(m, "provider"); prov == "custom_openai_compatible" {
			continue
		}
		// status=false 表示该模型对当前账号不可用，不列。
		if st, ok := m["status"].(bool); ok && !st {
			continue
		}
		e := traeModelEntry(m)
		// 用内置表补元数据（倍率 / 上下文 / 档位 / 厂商）。
		//
		// 上游 model_list 的 id 与客户端内置表的键**不同名**：
		// 上游回 seed_m8 / Doubao_1_5_thinking_pro / deepseek-V3-0324，
		// 内置表主键是 Doubao-1.5-pro / Doubao-1.5-thinking-pro / DeepSeek-V3-0324。
		// 只按原名查会静默 miss，于是所有元数据（倍率/上下文/档位）全丢。
		// 这里再走一遍别名映射（traeCatalogAliasKeys），别名命中即回填。
		if c, ok := traeMetaLookup(meta, id, strField(m, "display_name", "displayName", "name")); ok {
			e = mergeTraeCatalogMeta(e, c)
		} else {
			// 连别名都没有：至少保证面板有可读的 display 名。
			if isEmptyVal(e["name"]) {
				e["name"] = id
			}
		}
		e["available"] = true
		out = append(out, e)
	}
	// DLL/SOLO 模型段：get_detail_param 的 config_info_list 是当前账号可用的
	// DLL 内置模型全集（glm-5.3-flashx / deepseek-v4.1-flash / step-5-preview 等），
	// 与 model_list 的平台模型互不重叠。这些模型走 llm_utils_chat 链（2026-10-11
	// 全量实测 79/79 返回 OK），因此标 available=true + route=dll，与 /chat 平台
	// 模型一起构成「列表即能调」。拉取失败只影响本段，不阻塞其余目录。
	for _, name := range h.traeDLLModels(ctx, cli) {
		if name == "seed_m8" {
			continue
		}
		e := traeDLLModelEntry(meta, name)
		out = append(out, e)
	}
	// 客户端目录里**其余**条目以「仅客户端可用」的身份补在后面。
	//
	// 为什么要列（2026-10-08 决定）：用户看到的 Trae 客户端里有 20+ 个模型
	// （Seed-Evolving / GLM-5.3 / Kimi-K3 / Qwen3.8-Max …），面板却只有
	// 上游 /api/ide/v1/chat 认的那几条，观感上就像「网关把模型吞了」。
	// 实测（多轮复现，见 docs）这些条目经 /chat 与 llm_raw_chat 调用一律
	// 回 code 4023 "model is unknown"（glm-5.3-flash / deepseek-v4.1-flash
	// 会先排队再回 unknown），即**服务端不认、只有客户端（带 IDE 签名/会话
	// 上下文）能调**。所以它们不能混进「能调」段，但必须可见、且如实标注，
	// 否则用户永远在问「为什么我这儿没有 GLM-5.3」。
	//
	// 标记口径：
	//   available=true   上游 /chat 直接可调（列表即能调，旧语义不变）
	//   available=false  + client_only=true  仅 Trae 客户端可用
	// 去重按 id 与别名归一后的键，已在上面的可调段里的不再重复列出。
	seen := make(map[string]bool, len(out)+len(meta))
	for _, e := range out {
		for _, k := range traeCatalogAliasKeys(strField(e, "id"), strField(e, "name")) {
			seen[strings.ToLower(k)] = true
		}
	}
	for _, c := range trae.Catalog() {
		keys := traeCatalogAliasKeys(c.ID, c.Name)
		skip := false
		for _, k := range keys {
			if seen[strings.ToLower(k)] {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		for _, k := range keys {
			seen[strings.ToLower(k)] = true
		}
		e := traeCatalogEntry(c)
		e["available"] = false
		e["client_only"] = true
		e["note"] = "仅 Trae 客户端可用（上游 /api/ide/v1/chat 不认该 model_name）"
		if tags, ok := e["tags"].([]string); ok {
			e["tags"] = append(tags, "client-only")
		}
		out = append(out, e)
	}
	return out, nil
}

// traeDLLModels 拉取当前账号可用的 DLL 内置模型名（get_detail_param）。
// 返回空切片表示上游不可用或账号没有 DLL 模型（调用方静默跳过本段）。
func (h *Handler) traeDLLModels(ctx context.Context, cli *trae.Client) []string {
	detail, err := cli.DetailParam(ctx)
	if err != nil {
		logUpstream("trae get_detail_param: %v（DLL 模型段跳过）", err)
		return nil
	}
	var dp struct {
		ConfigInfoList []struct {
			ConfigName string `json:"config_name"`
		} `json:"config_info_list"`
	}
	if json.Unmarshal(detail, &dp) != nil {
		return nil
	}
	seen := make(map[string]bool, len(dp.ConfigInfoList))
	dllOut := make([]string, 0, len(dp.ConfigInfoList))
	for _, m := range dp.ConfigInfoList {
		name := strings.TrimSpace(m.ConfigName)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		dllOut = append(dllOut, name)
	}
	return dllOut
}

// traeDLLModelEntry 把一个 DLL 模型名转成面板条目（元数据从客户端内置目录补齐）。
func traeDLLModelEntry(meta map[string]trae.CatalogModel, name string) map[string]any {
	e := map[string]any{
		"id":   name,
		"name": name,
		"tags": []string{"trae", "dll"},
		"note": "Trae DLL 内置模型（走 llm_utils_chat/SOLO 链）",
	}
	if cc, ok := traeMetaLookup(meta, name, name); ok {
		e = mergeTraeCatalogMeta(e, cc)
		if isEmptyVal(e["name"]) {
			e["name"] = name
		}
	}
	e["available"] = true
	e["route"] = "dll"
	return e
}

// firstTraeAccount 取第一个登录态非空的 Trae 账号（与 server 的
// firstUpstreamAccount 同语义，这里单独留一份避免跨文件耦合）。
func firstTraeAccount(h *Handler) *auth.Auth {
	for _, st := range h.cfg.Pool.List() {
		if st.Provider != auth.ProviderTrae || st.Disabled {
			continue
		}
		if a := h.cfg.Pool.AuthByUID(st.UID); a != nil && a.TokenValue() != "" {
			return a
		}
	}
	return nil
}

// mergeTraeCatalogMeta 把内置表的元数据补进实时条目（不覆盖上游的实时真值）。
func mergeTraeCatalogMeta(e map[string]any, c trae.CatalogModel) map[string]any {
	setIfEmpty := func(k string, v any) {
		if isEmptyVal(e[k]) && !isEmptyVal(v) {
			e[k] = v
		}
	}
	setIfEmpty("name", c.Name)
	setIfEmpty("vendor", c.Vendor)
	setIfEmpty("credits", c.Rate)
	// context_length 的口径与 traeCatalogEntry 保持一致：优先 max，没有 max 就退回 default。
	//
	// 上游 model_list 不带任何上下文数字（2026-10-07 实测 features/max_tokens/
	// context_window_size 全为 null），所以这里的回退是**唯一**能给出上下文的地方。
	// 老实现只认 ContextMax，于是内置表里那些 ContextMax=0、只有 ContextDefault
	// 的条目（DeepSeek 系等）在面板上上下文一栏永远是空的。
	switch {
	case !isEmptyVal(c.ContextMax):
		setIfEmpty("context_length", c.ContextMax)
	case !isEmptyVal(c.ContextDefault):
		setIfEmpty("context_length", c.ContextDefault)
	}
	if !isEmptyVal(c.ContextDefault) {
		setIfEmpty("context_default", c.ContextDefault)
	}
	if !isEmptyVal(c.MaxOutput) {
		setIfEmpty("max_output_tokens", c.MaxOutput)
	}
	if !isEmptyVal(c.MaxTurn) {
		setIfEmpty("max_turns", c.MaxTurn)
	}
	if c.Reasoning {
		setIfEmpty("supports_reasoning", true)
	}
	if c.Multimodal {
		setIfEmpty("supports_images", true)
	}
	if c.Tools {
		setIfEmpty("supports_tool_call", true)
	}
	if len(c.Efforts) > 0 {
		setIfEmpty("supported_efforts", c.Efforts)
	}
	if c.DefaultEffort != "" {
		setIfEmpty("default_effort", c.DefaultEffort)
	}
	if c.DiscountRate != "" {
		setIfEmpty("discount_rate", c.DiscountRate)
	}
	if c.Section != "" {
		setIfEmpty("section", c.Section)
	}
	if _, has := e["tags"]; !has {
		e["tags"] = []string{"trae"}
	}
	return e
}

// traeCatalogEntry 把内置目录一行转成面板条目。
func traeCatalogEntry(c trae.CatalogModel) map[string]any {
	e := map[string]any{
		"id": c.ID, "name": c.Name, "vendor": c.Vendor,
		"credits": c.Rate, "tags": []string{"trae"},
	}
	// 上下文上限：客户端目录里有一半模型只给了 context_window_size.default
	// （没有 max），这类型号的上限对客户端保密，只能退而展示 default ——
	// 不回落的话面板会显示 "—"，而用户在 Trae 里明明能看到该模型的上下文长度。
	switch {
	case c.ContextMax > 0:
		e["context_length"] = c.ContextMax
	case c.ContextDefault > 0:
		e["context_length"] = c.ContextDefault
	}
	if c.ContextDefault > 0 {
		e["context_default"] = c.ContextDefault
	}
	if c.MaxOutput > 0 {
		e["max_output_tokens"] = c.MaxOutput
	}
	if c.Reasoning {
		e["supports_reasoning"] = true
	}
	if c.Multimodal {
		e["supports_images"] = true
	}
	if c.Tools {
		e["supports_tool_call"] = true
	}
	if c.MaxTurn > 0 {
		e["max_turns"] = c.MaxTurn
	}
	if len(c.Efforts) > 0 {
		e["supported_efforts"] = c.Efforts
	}
	if c.DefaultEffort != "" {
		e["default_effort"] = c.DefaultEffort
	}
	if c.DiscountRate != "" {
		e["discount_rate"] = c.DiscountRate
	}
	if c.Section != "" {
		e["section"] = c.Section
	}
	if c.IsDefault {
		e["is_default"] = true
	}
	return e
}

// isEmptyVal 判断覆盖用的值是否"空"，空的就不覆盖内置值。
func isEmptyVal(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case int64:
		return x == 0
	case float64:
		return x == 0
	case []string:
		return len(x) == 0
	case []any:
		return len(x) == 0
	}
	return false
}

// traeModelEntry 把 model_list 的一行转成面板条目（字段名以实际响应为准，做多键兼容）。
// traeCatalogAliasKeys 把上游 model_list 的 id/显示名映射到客户端内置表的键。
//
// 上游与客户端命名不同（逆向 %APPDATA%\Trae CN\User\globalStorage\state.vscdb
// 的 AI.agent.model.model_list_map，对照 /api/ide/v1/model_list 实测）：
//
//	上游 seed_m8                 ↔ 内置 Doubao-1.5-pro
//	上游 Doubao_1_5_thinking_pro ↔ 内置 Doubao-1.5-thinking-pro
//	上游 deepseek-V3             ↔ 内置 DeepSeek-V3
//	上游 deepseek-V3-0324        ↔ 内置 DeepSeek-V3-0324
//
// 没有这层映射时 mergeTraeCatalogMeta 永远 miss，面板里 Trae 四个模型
// 的倍率 / 上下文 / 思考档位全是「—」。
// traeCatalogAliasKeys 返回「上游 model_name/display_name → 内置目录键」的候选列表。
//
// 为什么需要它（2026-10-07 线上实测）：上游 /api/ide/v1/model_list 对一个 CN 账号
// 只回 4 个平台模型，且**只带 name/display_name/status**：
//
//	seed_m8                 display=Doubao-1.5-pro
//	Doubao_1_5_thinking_pro display=Doubao-1.5-thinking-pro
//	deepseek-V3             display=DeepSeek-V3
//	deepseek-V3-0324        display=DeepSeek-V3-0324
//
// 而客户端内置目录（trae.Catalog）里这四条的主键是显示名形态
// （Doubao-1.5-pro / DeepSeek-V3 / …）。上游那串下划线名直接 ToLower 查表必然 miss，
// 于是倍率/上下文/档位全部丢失 —— 面板上就只剩一个裸名字。
//
// 这张表把两边对上：键是**归一化**（去掉所有非字母数字）后的上游名，
// 值就是内置目录里的真实 ID。命中后由 mergeTraeCatalogMeta 只补空缺。
func traeCatalogAliasKeys(id, display string) []string {
	l := func(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
	keys := []string{l(id), l(display)}
	// 归一化：去掉分隔符与大小写差异后比较（seed_m8 ↔ seedm8，Doubao_1.5-pro ↔ doubao15pro）
	norm := func(s string) string {
		var b strings.Builder
		for _, r := range strings.ToLower(s) {
			if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
				b.WriteRune(r)
			}
		}
		return b.String()
	}
	// 上游真名 → 内置目录 ID（值必须与 internal/trae/catalog.go 的 ID 逐字一致）。
	table := map[string]string{
		// seed_m8 = Doubao-1.5-pro（model_list 的 display_name 也是它）
		"seedm8":      "Doubao-1.5-pro",
		"doubao15pro": "Doubao-1.5-pro",
		"doubao15":    "Doubao-1.5-pro",
		// Doubao_1_5_thinking_pro = Doubao-1.5-thinking-pro
		"doubao15thinkingpro": "Doubao-1.5-thinking-pro",
		// deepseek-V3 = DeepSeek-V3
		"deepseekv3": "DeepSeek-V3",
		// deepseek-V3-0324 = DeepSeek-V3-0324
		"deepseekv30324": "DeepSeek-V3-0324",
		// 老客户端曾用过的 display 形态（兼容上游改回这种命名时不再丢元数据）
		"doubao15pro32k": "Doubao-1.5-pro",
	}
	for _, raw := range []string{id, display} {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		if alias, ok := table[norm(raw)]; ok {
			keys = append(keys, alias)
		}
	}
	// 去重，保持顺序
	seen := make(map[string]bool, len(keys))
	out := keys[:0]
	for _, k := range keys {
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, k)
	}
	return out
}

// traeMetaLookup 依次用原名、显示名、别名在 meta 里找内置目录条目。
func traeMetaLookup(meta map[string]trae.CatalogModel, id, display string) (trae.CatalogModel, bool) {
	for _, k := range traeCatalogAliasKeys(id, display) {
		if k == "" {
			continue
		}
		if c, ok := meta[k]; ok {
			return c, true
		}
	}
	return trae.CatalogModel{}, false
}

func traeModelEntry(m map[string]any) map[string]any {
	id := strField(m, "model_name", "model_id", "name", "modelName")
	e := map[string]any{
		"id":   id,
		"name": strField(m, "display_name", "displayName", "name", id),
	}
	if v := numField(m, "max_input_tokens", "context_window", "contextWindow", "max_tokens"); v > 0 {
		e["context_length"] = v
	}
	if v := numField(m, "max_output_tokens", "maxOutputTokens"); v > 0 {
		e["max_output_tokens"] = v
	}
	if d := strField(m, "description", "desc"); d != "" {
		e["description"] = d
	}
	e["tags"] = []string{"trae"}
	if b, ok := m["supports_reasoning"].(bool); ok {
		e["supports_reasoning"] = b
	}
	// 倍率位于 features.consumption_rate.data.rate，不在顶层。
	// Trae 客户端 UI 显示的就是这个值（如 Seed-Code 0.06 → "x0.06"）。
	if f, ok := m["features"].(map[string]any); ok {
		if cr, ok := f["consumption_rate"].(map[string]any); ok {
			if d, ok := cr["data"].(map[string]any); ok {
				if r, ok := d["rate"].(float64); ok {
					e["credits"] = "x" + strconv.FormatFloat(r, 'f', -1, 64)
				}
			}
		}
	}
	// 思考档位：options 是 light/high/extra_high，面板口径是 low/high/xhigh。
	if rec, ok := m["reasoning_effort_config"].(map[string]any); ok {
		if opts, ok := rec["options"].([]any); ok {
			eff := make([]string, 0, len(opts))
			for _, o := range opts {
				if sv, ok := o.(string); ok {
					eff = append(eff, traeEffortLevel(sv))
				}
			}
			if len(eff) > 0 {
				e["supported_efforts"] = eff
			}
		}
		if dl, ok := rec["default_level"].(string); ok && dl != "" {
			e["default_effort"] = traeEffortLevel(dl)
		}
	}
	// 工具调用：custom_config 是一串被转义的 JSON 字符串，
	// 其中 native_function_call 为 true 表示模型支持 function calling。
	if cc, ok := m["custom_config"].(string); ok && cc != "" {
		var cfg struct {
			NativeFunctionCall bool `json:"native_function_call"`
		}
		if json.Unmarshal([]byte(cc), &cfg) == nil && cfg.NativeFunctionCall {
			e["supports_tool_call"] = true
		}
	}
	if v := numField(m, "max_turn", "maxTurn"); v > 0 {
		e["max_turns"] = v
	}
	// 上下文：实时响应里是嵌套对象 {max:[N], default:N}。
	if cw, ok := m["context_window_size"].(map[string]any); ok {
		if arr, ok := cw["max"].([]any); ok && len(arr) > 0 {
			if v, ok := arr[0].(float64); ok && v > 0 {
				e["context_length"] = int64(v)
			}
		}
		if v, ok := cw["default"].(float64); ok && v > 0 {
			e["context_default"] = int64(v)
			// 实时响应 max 为 null 时同样回落 default，理由同 traeCatalogEntry。
			if _, has := e["context_length"]; !has {
				e["context_length"] = int64(v)
			}
		}
	}
	if b, ok := m["is_default"].(bool); ok && b {
		e["is_default"] = true
	}
	return e
}

// traeClientOnlyCatalog 把客户端内置目录整体转成「仅客户端可用」条目。
//
// 用于「没有 Trae 账号」与「登录态失效」两种情形：此时上游 model_list 拿不到，
// 但客户端目录仍是权威的展示口径。全部条目 available=false + client_only=true，
// 于是：
//
//	· 面板单 provider 视图会列出全部 31 条（用户能看到 GLM-5.3 / Kimi-K3 / Qwen3.8-Max）；
//	· /v1/models 与面板「全部上游」视图会经 trae.CallableOnly 全部滤掉（因为一条都调不通），
//	  不会误导任何客户端。
func traeClientOnlyCatalog() []map[string]any {
	cat := trae.Catalog()
	out := make([]map[string]any, 0, len(cat))
	for _, c := range cat {
		e := traeCatalogEntry(c)
		e["available"] = false
		e["client_only"] = true
		e["note"] = traeClientOnlyNote
		if tags, ok := e["tags"].([]string); ok {
			e["tags"] = append(tags, "client-only")
		}
		out = append(out, e)
	}
	return out
}

// traeClientOnlyNote 是 client-only 条目的统一说明（面板 tooltip 用）。
const traeClientOnlyNote = "仅 Trae 客户端可用：网关直调 /api/ide/v1/chat 与 llm_raw_chat 实测" +
	"一律返回 code 4023 model is unknown（该 model_name 只在客户端会话/签名上下文里有效）"

// traeEffortLevel 把 Trae 客户端的思考档位名转成面板统一口径。
//
// 为什么要转：客户端用 light/high/extra_high，而面板的档位筛选与展示用
// low/high/xhigh。不转的话同一页会出现两套词汇，档位筛选对 trae 完全失效。
func traeEffortLevel(s string) string {
	switch s {
	case "light":
		return "low"
	case "extra_high":
		return "xhigh"
	}
	return s
}

// zcodeModels 拉 ZCode 模型目录（匿名端点，无需账号）。
func (h *Handler) zcodeModels(ctx context.Context) ([]map[string]any, error) {
	raw, err := zcode.FetchClientConfigs(ctx, h.cfg.UpstreamHTTP, zcode.DefaultAppVersion)
	if err != nil {
		return nil, err
	}
	return zcodeModelEntries(raw), nil
}

// zcodeModelsWithAccount 在匿名目录之上，追加「本账号套餐授权的模型」。
//
// 一定要与 zcodeModels 分开：
//
//	client/configs 是公共目录（上游目前只有 GLM-5.2 / GLM-5-Turbo）
//	billing 的 capabilities 才是账号实际能调的模型（如 Start Plan 送的 GLM-5.3-Flash）
//
// 两者并集合后每条都带 source 标记，面板上就不会再出现
// 「我领了 GLM-5.3-Flash，可模型列表里根本没有它」。
func (h *Handler) zcodeModelsWithAccount(ctx context.Context, a *auth.Auth) ([]map[string]any, error) {
	out, err := h.zcodeModels(ctx)
	if err != nil {
		return nil, err
	}
	if a == nil {
		return out, nil
	}
	b, err := h.ZCodeBalance(ctx, a)
	if err != nil {
		// 目录已拉到就不要因为账号失败而整页报错，
		// 但必须把原因带出去，不能默默吞掉。
		for _, e := range out {
			e["account_error"] = err.Error()
		}
		return out, nil
	}
	out = zcodeMergeBuiltin(out, zcode.BuiltinCallableModels())
	return zcodeMergeEntitled(out, b.EntitledModels()), nil
}

// zcodeMergeEntitled 把套餐授权模型并入目录（按 id 去重，不覆盖公共目录里的真实配置）。
func zcodeMergeEntitled(base []map[string]any, ents []zcode.EntitledModel) []map[string]any {
	// 建索引：已有模型只补 source 与套餐信息，不重新出行。
	idx := make(map[string]int, len(base))
	for i, e := range base {
		id, _ := e["id"].(string)
		idx[strings.ToLower(id)] = i
	}
	for _, m := range ents {
		if m.Model == "" {
			continue
		}
		key := strings.ToLower(m.Model)
		if i, ok := idx[key]; ok {
			e := base[i]
			if srcs, _ := e["entitled_from"].([]string); len(srcs) == 0 {
				e["entitled_from"] = []string{m.Source}
			}
			e["entitled"] = true
			if m.Remain > 0 || m.Total > 0 {
				e["entitled_remain"] = m.Remain
				e["entitled_total"] = m.Total
			}
			if m.ExpiresAt > 0 {
				e["entitled_expires_at"] = m.ExpiresAt
			}
			continue
		}
		idx[key] = len(base)
		e := map[string]any{
			"id":            m.Model,
			"name":          firstNonEmpty(m.ShowName, m.Model),
			"vendor":        "ZCode 套餐",
			"tags":          []string{"zcode", "entitled"},
			"entitled":      true,
			"entitled_from": []string{m.Source},
			"source":        "billing-capabilities",
			// 上游对 glm-5.3 系列的权威参数（见 zcode-builtin 的 modelRules）。
			"context_length":    int64(1000000),
			"max_output_tokens": int64(128000),
			// 额度来自套餐，不在 client/configs 里，必须标出来。
			"note": "由套餐授权，未列入公开模型目录",
		}
		// 套餐授权条目同样带思考档位：GLM-5.3 系列实测始终思考、
		// 取值域 low/high/max（默认 max），与内置目录同源。
		zcodeFillEffort(e, zcodeEffortSpec(m.Model))
		if m.Remain > 0 || m.Total > 0 {
			e["entitled_remain"] = m.Remain
			e["entitled_total"] = m.Total
		}
		if m.ExpiresAt > 0 {
			e["entitled_expires_at"] = m.ExpiresAt
		}
		if m.UnitType != "" {
			e["unit_type"] = m.UnitType
		}
		base = append(base, e)
	}
	return base
}

// zcodeModelEntries 把 client/configs 的 providers 展开为去重后的模型条目。
// zcodeMergeBuiltin 把「实测可调」的内置 ZCode 模型目录并进列表。
//
// 上游 client/configs 只列 GLM-5.2 / GLM-5-Turbo，套餐 capabilities 只喂授权模型，
// 两者都覆盖不到「同一把 key 实际能调的全部 GLM」。内置表（zcode.BuiltinCallableModels）
// 是逐模型实测 200 的结论，所以并进来让列表 = 能调。
//
// 只补空缺：上游实时条目里的同 id 条目保持原值（真值优先）。
func zcodeMergeBuiltin(base []map[string]any, builtins []zcode.BuiltinModel) []map[string]any {
	idx := make(map[string]int, len(base))
	for i, e := range base {
		if id, _ := e["id"].(string); id != "" {
			idx[strings.ToLower(id)] = i
		}
	}
	for _, b := range builtins {
		if b.ID == "" || b.ID == "custom" {
			continue
		}
		key := strings.ToLower(b.ID)
		if i, ok := idx[key]; ok {
			// 已有条目：只补缺失的上下文/输出上限与多模态能力。
			e := base[i]
			if isEmptyVal(e["context_length"]) && b.ContextWindow > 0 {
				e["context_length"] = b.ContextWindow
			}
			if isEmptyVal(e["max_output_tokens"]) && b.MaxOutput > 0 {
				e["max_output_tokens"] = b.MaxOutput
			}
			if b.Vision {
				if _, has := e["supports_images"]; !has {
					e["supports_images"] = true
				}
			}
			zcodeFillEffort(e, b)
			continue
		}
		e := map[string]any{
			"id":     b.ID,
			"name":   firstNonEmpty(b.Name, b.ID),
			"vendor": "BigModel",
			"tags":   []string{"zcode", "builtin"},
			"source": "builtin-catalog",
		}
		if b.ContextWindow > 0 {
			e["context_length"] = b.ContextWindow
		}
		if b.MaxOutput > 0 {
			e["max_output_tokens"] = b.MaxOutput
		}
		if b.Vision {
			e["supports_images"] = true
		}
		if b.Note != "" {
			e["note"] = b.Note
		}
		zcodeFillEffort(e, b)
		idx[key] = len(base)
		base = append(base, e)
	}
	return base
}

func zcodeModelEntries(cfg *zcode.ClientConfigs) []map[string]any {
	seen := map[string]bool{}
	out := make([]map[string]any, 0)
	for _, p := range cfg.Providers {
		for _, m := range p.Models {
			if m.ModelID == "" || seen[m.ModelID] {
				continue
			}
			seen[m.ModelID] = true
			e := map[string]any{
				"id":       m.ModelID,
				"name":     firstNonEmpty(m.Name, m.ModelID),
				"vendor":   p.Name,
				"base_url": p.BaseURL,
				"schema":   p.Schema,
				"tags":     []string{"zcode"},
			}
			if m.ContextWindow > 0 {
				e["context_length"] = m.ContextWindow
			}
			if m.MaxCompletionTokens > 0 {
				e["max_output_tokens"] = m.MaxCompletionTokens
			}
			if p.DefaultModel == m.ModelID {
				e["is_default"] = true
			}
			// 客户端目录里的 capabilities/reasoning 常为空对象（上游实测：GLM-5.2
			// 与 GLM-5-Turbo 都是 {}），档位/开关信息要靠内置实测表补齐，
			// 否则面板上这两条会显示"不支持思考"，而实际它们是可以开关思考的。
			zcodeFillEffort(e, zcodeEffortSpec(m.ModelID))
			out = append(out, e)
		}
	}
	return out
}

// Packages 面板「积分构成」的 provider 分流实现。
func (h *Handler) Packages(ctx context.Context, a *auth.Auth) (panel.UpstreamPackageView, error) {
	switch a.ProviderValue() {
	case auth.ProviderTrae:
		return h.traePackages(ctx, a)
	case auth.ProviderZCode:
		return h.zcodePackages(ctx, a)
	default:
		return panel.UpstreamPackageView{}, errUnsupportedProvider
	}
}

func (h *Handler) traePackages(ctx context.Context, a *auth.Auth) (panel.UpstreamPackageView, error) {
	u, err := h.TraeUsage(ctx, a)
	if err != nil {
		return panel.UpstreamPackageView{}, err
	}
	view := panel.UpstreamPackageView{Provider: auth.ProviderTrae, Remain: u.Remaining, Size: u.Total}
	// ide_user_ent_usage 的真实条目结构（线上实测，2026-10-06）：
	//
	//	{"display_desc":"每月登录赠送",
	//	 "entitlement_base_info":{"quota":{"credits_limit":500}},
	//	 "usage":{"credits_amount":4.462},
	//	 "group_name":"每月登录积分","expire_time":1793462399,"status":1}
	//
	// 名称在 display_desc（不在 package_name），额度在 entitlement_base_info.quota.credits_limit
	// （不在顶层 capacity/total），已用在顶层 usage.credits_amount。旧代码按顶层字段取，
	// 全部落空，于是面板把 6 个权益包渲染成一排一模一样的空「权益包 — / —」。
	// expire_time 已是秒级，旧代码又除了一次 1000，时间也一并错。
	for _, raw := range u.Packs {
		var m map[string]any
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		name := strField(m, "display_desc", "package_name", "name", "sub_product_name")
		var size, used int64
		if bi, ok := m["entitlement_base_info"].(map[string]any); ok {
			if q, ok := bi["quota"].(map[string]any); ok {
				size = numField(q, "credits_limit", "total", "capacity")
			}
		}
		if size == 0 { // 兼容旧版本把额度放顶层的形态
			size = numField(m, "capacity", "total", "total_credits", "credits_total", "cycle_capacity")
		}
		if us, ok := m["usage"].(map[string]any); ok {
			used = numField(us, "credits_amount", "used", "used_credits")
		}
		if used == 0 {
			used = numField(m, "used", "used_credits", "credits_used", "cycle_used")
		}
		if size == 0 && used == 0 {
			// 免费包这类没有额度数字，但有明显语义：用于标记权益是否可用。
			// 不编造金额，只把「已激活」写进名称让用户看得懂。
			if g := strField(m, "group_name"); g != "" {
				name = firstNonEmpty(name, g)
			} else {
				name = firstNonEmpty(name, "权益包")
			}
		}
		exp := numField(m, "expire_time", "expired_time", "package_end_time", "cycle_end_time")
		if exp > 100000000000 { // 毫秒时间戳才换算，秒级原样保留
			exp /= 1000
		}
		view.Packages = append(view.Packages, panel.UpstreamPackage{
			Name:      firstNonEmpty(name, "权益包"),
			Remain:    max0(size - used),
			Used:      used,
			Size:      size,
			ExpiresAt: exp,
		})
	}
	if len(view.Packages) == 0 {
		view.Packages = append(view.Packages, panel.UpstreamPackage{
			Name: "Trae 额度", Remain: u.Remaining, Size: u.Total,
		})
	}
	return view, nil
}

func (h *Handler) zcodePackages(ctx context.Context, a *auth.Auth) (panel.UpstreamPackageView, error) {
	cli := h.zcodeClient(a)
	b, err := h.ZCodeBalance(ctx, a)
	if err != nil {
		return panel.UpstreamPackageView{}, err
	}
	remain, total, _ := b.Remaining()
	view := panel.UpstreamPackageView{Provider: auth.ProviderZCode, Remain: remain, Size: total}
	// 来源标注（zai / bigmodel）：同一个 uid 在两个账号体系下是两套独立额度，
	// 面板不标出来会让人以为"我明明开通了却没额度"。
	//
	// 只认 Source，不做 Region/Realm 兜底：Region 是阿里云无痕验证的地域（恒 "cn"），
	// 拿它当来源会在徽标上写出一个假的 "cn"，比不显示更误导。老账号（本次改动前
	// 落的盘）Source 为空 → 不显示徽标，下次重新登录即带上。
	view.Source = a.Source
	for _, x := range b.Balances {
		// Note 里必须带上「可用模型」，否则用户看到 1 亿却不知道该喂给谁：
		// 上游把可调用模型写在 capabilities，而不是写在 show_name 里。
		note := zcodeCapabilityNote(x.Capabilities, x.UnitType)
		if x.Meter != "" && x.Meter != "model_usage" {
			note = strings.TrimSpace(note + " · 计量 " + x.Meter)
		}
		view.Packages = append(view.Packages, panel.UpstreamPackage{
			Name:      firstNonEmpty(x.ShowName, x.Model, "额度"),
			Remain:    x.RemainingUnits,
			Used:      x.UsedUnits,
			Size:      x.TotalUnits,
			ExpiresAt: x.ExpiresAt,
			Note:      note,
			Models:    zcodeCapabilityModels(x.Capabilities),
		})
	}
	for _, p := range b.Plans {
		note := p.Status
		caps := planCapabilities(p)
		if len(caps) > 0 {
			note = strings.TrimSpace(note + " · 覆盖 " + strings.Join(caps, "/"))
		}
		view.Packages = append(view.Packages, panel.UpstreamPackage{
			Name: "方案 · " + firstNonEmpty(p.Name, p.PlanID),
			Size: 0, Note: note,
			ExpiresAt: p.EndsAt,
			Models:    caps,
		})
	}
	// 占位项的时机推迟到 preview 之后（见函数末尾）：只有「既没有已到手额度、
	// 也没有可领取项」时才需要它，否则它会和「待领取」行说同一件事的两遍。
	hasEarned := len(view.Packages) > 0

	// 可领取方案（billing/preview）与邀请奖励总览。
	//
	// 为什么必须补这两块："余额为空"对未开通账号是**正确答案**而不是故障，
	// 面板若只渲染 balance 就会出现"整页空白、看不出能做什么"。preview 返回的
	// 就是帐号当前**能领**的东西（含 white-label 的 grant_units / 周期），
	// 把它作为"待领取"行展示，页面立刻从"没额度"变成"有东西可领，去任务中心"。
	var previewErr, rewardErr string
	if plans, err := cli.Preview(ctx); err != nil {
		previewErr = err.Error()
	} else {
		for _, p := range plans {
			units := int64(0)
			models := make([]string, 0, len(p.Entitlements))
			for _, e := range p.Entitlements {
				if e.GrantUnits > 0 {
					units += e.GrantUnits
				}
				if e.ShowName != "" {
					models = append(models, e.ShowName)
				}
			}
			note := "可领取"
			if len(models) > 0 {
				note += " · 覆盖 " + strings.Join(models, "/")
			}
			view.Packages = append(view.Packages, panel.UpstreamPackage{
				Name:   "待领取 · " + firstNonEmpty(p.Name, p.PlanID),
				Remain: 0, Used: 0, Size: 0, // 未领取不计入已到手额度
				Note:      note,
				Pending:   true,
				PlanID:    p.PlanID,
				GrantSize: units,
			})
			view.PendingTotal += units
		}
	}
	if sum, err := cli.RewardSummary(ctx); err != nil {
		rewardErr = err.Error()
	} else {
		for _, r := range sum.ClaimableRewards() {
			view.Packages = append(view.Packages, panel.UpstreamPackage{
				Name:    "奖励 · " + firstNonEmpty(r.Title, r.RewardType, "邀请奖励"),
				Note:    fmt.Sprintf("进度 %d/%d · 进行中", r.Progress.Current, r.Progress.Target),
				Pending: true, GrantSize: r.GrantUnits,
			})
			view.PendingTotal += r.GrantUnits
		}
		if txt := zcodeSummaryText(sum); txt != "" {
			view.Note = strings.TrimSpace(strings.Join([]string{view.Note, txt}, " "))
		}
	}
	if previewErr != "" || rewardErr != "" {
		view.Note = strings.TrimSpace(strings.Join([]string{
			view.Note,
			firstNonEmpty(errPrefix("preview", previewErr), errPrefix("rewards", rewardErr)),
		}, " "))
	}
	// zcodeSummaryText 在没有任何邀请数据时会吐出「玩法状态: unknown」——
	// 这是上游对"该账号没参与这个玩法"的占位，展示给用户只会制造困惑，丢掉。
	view.Note = strings.TrimSpace(strings.ReplaceAll(view.Note, "玩法状态: unknown", ""))
	if !hasEarned && len(view.Packages) == 0 {
		view.Packages = append(view.Packages, panel.UpstreamPackage{Name: "未激活编码方案", Remain: 0, Size: 0})
		view.Note = strings.TrimSpace(view.Note + " 该账号尚未开通编码方案，可在下方任务里领取")
	}
	return view, nil
}

// errPrefix 把 (标签, 错误) 拼成一句可展示的说明；err 为空时返回空串。
func errPrefix(label, err string) string {
	if strings.TrimSpace(err) == "" {
		return ""
	}
	return label + " 查询失败: " + err
}

// Tasks 面板「任务中心」的 provider 分流实现。
//
//	Trae : 签到 + 活动 + 裂变/权益（FissionGrant / BenefitGrant）。
//	ZCode: 可领取方案（Preview）+ 邀请/奖励总览（RewardSummary）。
func (h *Handler) Tasks(ctx context.Context, a *auth.Auth) ([]panel.UpstreamTask, error) {
	switch a.ProviderValue() {
	case auth.ProviderTrae:
		return h.traeTasks(ctx, a)
	case auth.ProviderZCode:
		return h.zcodeTasks(ctx, a)
	default:
		return nil, errUnsupportedProvider
	}
}

func (h *Handler) traeTasks(ctx context.Context, a *auth.Auth) ([]panel.UpstreamTask, error) {
	cli := trae.New(a, trae.Options{})
	out := make([]panel.UpstreamTask, 0, 8)
	st, err := cli.CheckinStatus(ctx)
	if err == nil {
		t := panel.UpstreamTask{Code: "trae/checkin", Name: "每日签到", Credit: st.Credits}
		if st.CheckedIn {
			t.Status, t.Claimed = "done", true
			t.Message = "今日已签到"
		} else if st.Enable {
			t.Status, t.Claimable = "claimable", true
		} else {
			t.Status, t.Locked, t.Message = "pending", true, "签到未开放"
		}
		out = append(out, t)
	} else {
		out = append(out, panel.UpstreamTask{Code: "trae/checkin", Name: "每日签到", Status: "pending", Message: err.Error()})
	}
	if info, err := cli.QueryActivityInfo(ctx, 1); err == nil {
		for _, act := range info.Activities {
			id := strField(act, "activity_id", "activityId", "id")
			if id == "" {
				continue
			}
			t := panel.UpstreamTask{
				Code:   "trae/activity/" + id,
				Name:   firstNonEmpty(strField(act, "activity_name", "name", "title"), "活动 "+id),
				Credit: numField(act, "credits", "credit", "reward"),
			}
			// 奖励数额不在顶层：签到券在 work_credits，其余在 general_credits。
			extra, _ := act["work_extra"].(map[string]any)
			if t.Credit == 0 && extra != nil {
				t.Credit = numField(extra, "general_credits", "work_credits", "credits")
			}
			typ := numField(act, "activity_type")
			if b, ok := act["Enabled"].(bool); ok && !b {
				t.Locked, t.Message = true, "未开放"
			}
			if extra != nil {
				if g, _ := extra["granted"].(bool); g {
					t.Claimed, t.Status = true, "done"
					t.Message = "已领取"
				}
			}
			// 领取方式提示：103 走裂变领取，其余走权益发放。
			// 不再无条件标 claimable——上游明确 grantable=false 时点了也没用。
			if !t.Claimed && !t.Locked {
				grantable, hasGrantable := false, false
				if extra != nil {
					if g, ok := extra["grantable"].(bool); ok {
						grantable, hasGrantable = g, true
					}
				}
				switch {
				case hasGrantable && !grantable:
					// 例如 code_fission_202609 / work_desktop_download：
					// 权益条件没满足（学生认证未做、桌面端没下载）。
					t.Locked, t.Message = true, "条件未满足"
				case typ == 103:
					t.Status, t.Claimable = "claimable", true
					t.Message = "裂变活动，点领取或等自动巡检"
				default:
					t.Status, t.Claimable = "claimable", true
					t.Message = "权益活动，点领取或等自动巡检"
				}
			}
			out = append(out, t)
		}
	}
	return out, nil
}

func (h *Handler) zcodeTasks(ctx context.Context, a *auth.Auth) ([]panel.UpstreamTask, error) {
	cli := h.zcodeClient(a)
	out := make([]panel.UpstreamTask, 0, 6)
	plans, err := cli.Preview(ctx)
	if err == nil {
		for _, p := range plans {
			out = append(out, panel.UpstreamTask{
				Code: "zcode/plan/" + p.PlanID, Name: "领取 · " + firstNonEmpty(p.Name, p.PlanID),
				Status: "claimable", Claimable: true,
			})
		}
	}
	if sum, err := cli.RewardSummary(ctx); err == nil {
		for _, r := range sum.ClaimableRewards() {
			out = append(out, panel.UpstreamTask{
				Code:   "zcode/reward/" + firstNonEmpty(r.Title, r.RewardType),
				Name:   firstNonEmpty(r.Title, r.RewardType, "邀请奖励"),
				Status: "claimable", Claimable: true,
				Current: int64(r.Progress.Current), Target: int64(r.Progress.Target),
				Credit: r.GrantUnits,
			})
		}
		// 只有拼出有效信息（邀请码 / 拉新状态）时才加总览行，
		// 否则会多出一条内容为空的「邀请/奖励总览」干扰任务中心。
		if txt := zcodeSummaryText(sum); txt != "" {
			out = append(out, panel.UpstreamTask{
				Code: "zcode/summary", Name: "邀请/奖励总览", Status: "done",
				Message: txt,
			})
		}
	}
	// 一个都没有时**返回空列表**，不要造一条 "zcode/plan/none" 占位行。
	//
	// 为什么：占位行会被前端渲染成「可领取方案 / 未接受」，
	// 让用户以为真的有东西可领而反复点。空列表会让弹窗显示
	// 「该账号暂无待办」，语义才对。
	return out, nil
}

// TaskClaim 面板任务中心的 provider 分流实现。
func (h *Handler) TaskClaim(ctx context.Context, a *auth.Auth, code string) (bool, string) {
	switch a.ProviderValue() {
	case auth.ProviderTrae:
		return h.traeTaskClaim(ctx, a, code)
	case auth.ProviderZCode:
		return h.zcodeTaskClaim(ctx, a, code)
	default:
		return false, "该上游不支持任务领取"
	}
}

func (h *Handler) traeTaskClaim(ctx context.Context, a *auth.Auth, code string) (bool, string) {
	cli := trae.New(a, trae.Options{})
	if code == "" || code == "trae/checkin" {
		res := h.TraeCheckin(ctx, a)
		return res.Claimed || res.CheckedIn, res.Message
	}
	if id, ok := strings.CutPrefix(code, "trae/activity/"); ok {
		return h.traeActivityClaimOne(ctx, a, cli, id)
	}
	return false, "未知任务：" + code
}

// traeActivityClaimOne 领取单个活动（按 activity_type 走对应端点）。
//
// 判定口径与 Trae 客户端一致：HTTP 200 且 body.code 缺失或 0 才算领到。
// 9090/9070 是"暂不可用/暂无权限"，属于下轮再试的正常状态，不能报成功。
func (h *Handler) traeActivityClaimOne(ctx context.Context, a *auth.Auth, cli *trae.Client, id string) (bool, string) {
	info, err := cli.QueryActivityInfo(ctx, 1)
	if err != nil {
		return false, err.Error()
	}
	var typ int64 = 103 // 默认按裂变处理：只有它需要 activity_id
	found := false
	for _, act := range info.Activities {
		if strField(act, "activity_id", "activityId", "id") != id {
			continue
		}
		found = true
		typ = numField(act, "activity_type")
		break
	}
	if !found {
		return false, "活动不存在或已下线：" + id
	}
	var st int
	var raw []byte
	if typ == 103 {
		st, raw, err = cli.FissionGrant(ctx, id, true, 1)
	} else {
		st, raw, err = cli.BenefitGrant(ctx, a.DeviceID, 1)
	}
	if err != nil {
		return false, err.Error()
	}
	code, msg, ok := trae.ActivityCode(raw)
	if !ok {
		return false, "上游响应异常（http " + strconv.Itoa(st) + "）"
	}
	if code == 0 {
		if msg != "" {
			return true, msg
		}
		return true, "已领取"
	}
	if isPendingCode(code) {
		return false, "上游返回 " + strconv.Itoa(code) + "（活动暂不可用/暂无权限，稍后自动重试）"
	}
	return false, "上游错误 " + strconv.Itoa(code) + "：" + msg
}

func (h *Handler) zcodeTaskClaim(ctx context.Context, a *auth.Auth, code string) (bool, string) {
	if code != "" {
		if id, ok := strings.CutPrefix(code, "zcode/plan/"); ok {
			res := h.ZCodeClaim(ctx, a, id)
			return res.OK, res.Message
		}
	}
	res := h.ZCodeClaimAll(ctx, a)
	return res.OK, res.Message
}

// ---- 小助手 ----

// zcodeSummaryText 拼「邀请/奖励总览」一行的说明。
func zcodeSummaryText(sum *zcode.Summary) string {
	// 玩法状态为空时**不要**拼成 "unknown" 展示。
	//
	// 为什么：Gameplay.Status 空值只是表示"该账号没参与裂变/邀请玩法"的
	// 默认值，写成"玩法状态: unknown" 会让用户以为面板出了故障。
	// 没有任何可展示信息时直接返回空串，调用方会省掉该行。
	var parts []string
	if st := strings.TrimSpace(sum.Gameplay.Status); st != "" && st != "unknown" {
		parts = append(parts, "玩法状态: "+st)
	}
	if sum.Inviter != nil && sum.Inviter.Visible {
		parts = append(parts, "我的邀请码: "+firstNonEmpty(sum.Inviter.InviteCode, "—"))
		if sum.Inviter.State != "" {
			parts = append(parts, "邀请人状态: "+sum.Inviter.State)
		}
	}
	if sum.Invitee != nil && sum.Invitee.Visible && sum.Invitee.State != "" {
		parts = append(parts, "被邀请状态: "+sum.Invitee.State)
	}
	return strings.Join(parts, " · ")
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func numField(m map[string]any, keys ...string) int64 {
	for _, k := range keys {
		switch v := m[k].(type) {
		case float64:
			return int64(v)
		case int64:
			return v
		case json.Number:
			if n, err := v.Int64(); err == nil {
				return n
			}
		case string:
			var n int64
			if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
				return n
			}
		}
	}
	return 0
}

func max0(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}

// zcodeCapabilityModels 把 capabilities 里的 "model:xxx" 拆成纯模型名列表。
func zcodeCapabilityModels(caps []string) []string {
	if len(caps) == 0 {
		return nil
	}
	out := make([]string, 0, len(caps))
	for _, c := range caps {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if strings.HasPrefix(c, "model:") {
			c = strings.TrimSpace(c[len("model:"):])
		}
		if c != "" {
			out = append(out, c)
		}
	}
	return out
}

// zcodeCapabilityNote 把 capabilities + unit_type 组成一句可读的备注。
//
// 这是面板上唯一告诉用户「这笔额度能用在哪些模型上」的地方。
func zcodeCapabilityNote(caps []string, unitType string) string {
	ms := zcodeCapabilityModels(caps)
	parts := make([]string, 0, 2)
	if len(ms) > 0 {
		parts = append(parts, "可用模型 "+strings.Join(ms, "/"))
	}
	if unitType != "" {
		parts = append(parts, "计作 "+unitType)
	}
	return strings.Join(parts, " · ")
}

// planCapabilities 收集一个方案下所有权益的模型标识。
func planCapabilities(p zcode.Plan) []string {
	var out []string
	seen := map[string]bool{}
	for _, e := range p.Entitlements {
		for _, m := range zcodeCapabilityModels(e.Capabilities) {
			if !seen[m] {
				seen[m] = true
				out = append(out, m)
			}
		}
	}
	return out
}

// zcodeFillEffort 把内置目录里声明的思考档位补进面板条目（只补空缺）。
//
// 只写三样：档位表、默认档、思考是否可关。前三者都来自 ZCode 客户端自身的
// 模型声明（config.json 的 reasoning 段）与上游 400 文案的取值域，
// 不是猜的；没有声明（Efforts 为空）就什么都不写，面板自然不显示档位列。
func zcodeFillEffort(e map[string]any, b zcode.BuiltinModel) {
	if len(b.Efforts) > 0 {
		if _, has := e["supported_efforts"]; !has {
			e["supported_efforts"] = b.Efforts
		}
		if _, has := e["supports_reasoning"]; !has {
			e["supports_reasoning"] = true
		}
	}
	if b.DefaultEffort != "" {
		if _, has := e["default_effort"]; !has {
			e["default_effort"] = b.DefaultEffort
		}
	}
	if b.AlwaysThinking {
		if _, has := e["can_disable_thinking"]; !has {
			// 常开即不可关：面板据此把 "off（可关）" 排除掉。
			e["can_disable_thinking"] = false
			e["always_thinking"] = true
		}
	}
	// 思考**开关**（thinking.type）与思考**档位**（output_config.effort）是两种能力，
	// 分开透出：ZCode 里 GLM-5.2/5-Turbo/4.7 有开关没档位，GLM-5.3 系列有档位没开关。
	// 不分开的话面板只能显示"不支持思考"，用户就以为 ZCode 那个开关是摆设。
	if b.ThinkingToggle {
		if _, has := e["thinking_toggle"]; !has {
			e["thinking_toggle"] = true
		}
		if _, has := e["supports_reasoning"]; !has {
			e["supports_reasoning"] = true
		}
	}
}

// zcodeEffortSpec 按模型名取档位声明（大小写不敏感）；未声明返回零值。
func zcodeEffortSpec(model string) zcode.BuiltinModel {
	name := strings.ToLower(strings.TrimSpace(model))
	if name == "" {
		return zcode.BuiltinModel{}
	}
	if b, ok := zcodeEffortIndex()[name]; ok {
		return b
	}
	return zcode.BuiltinModel{}
}

// zcodeEffortIndex 惰性构建一次（内置目录是静态表，不必每次遍历）。
var zcodeEffortIndex = sync.OnceValue(func() map[string]zcode.BuiltinModel {
	return zcode.EffortSpecs()
})
