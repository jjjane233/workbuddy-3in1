package trae

import "strings"

// Trae 积分换算（credits / 1K token）。
//
// ── 为什么需要这张表 ──────────────────────────────────────────────
// Trae 的 /api/ide/v1/chat 响应**不带任何积分字段**：token_usage 事件只有
// prompt/completion/total/reasoning_tokens（2026-10-08 实测原始帧见下）。
// 逐次接口 POST /api/v1/commercial/get_session_usage 需要 agent/UI 场景的
// 真实 userMessageId，网关拿不到（传 session_id / prompt_completion_id /
// 伪造 user_message_id 一律返回全 0）。因此只能**实测标定**。
//
// ── 标定方法（可复现）─────────────────────────────────────────────
//	POST https://api.trae.cn/trae/api/v2/pay/ide_user_ent_usage
//	body {"require_usage":true,"req_source":1}
//	→ usage_summary.consumed_amount
//
// 同一账号在**批量请求前后**各取一次 consumed_amount，差值 = 该批真实扣掉的
// 积分。批量（每批 6 次同 prompt）能压过 consumed_amount 只保留 2 位小数的
// 量化误差。2026-10-08 实测静默 60s 的漂移为 **0.000**（无后台扣费），
// 所以差值全部来自本批调用。
//
// ── 关键结论：计费是「输入/输出双系数」，不是单一倍率 ──────────────
// 用单倍率（credits/1K total）拟合时同模型样本互相打架（seed_m8 短请求
// 1K 单价 0.049、长请求 0.079，差 60%）；改成
//	credit = a·prompt_tokens + b·completion_tokens
// 后所有干净样本（含 6 次/批的批量点）误差都在 ±5% 以内，因此本表按
// **双系数**建模，a/b 单位同为 credits/1K token。
//
// 数据（值 = credits/1K token）：
//
//	模型                        a(输入)   b(输出)   样本数
//	seed_m8                     0.0268   0.0820     5
//	Doubao_1_5_thinking_pro     0.1589   0.6405     7
//	deepseek-V3                 0.0789   0.3356     5
//	deepseek-V3-0324            0.1607   0.4809     5
//
// 原始样本（prompt, completion, Δcredits）：
//
//	seed_m8                (95,109,0.01) (95,97,0.01) (143,4276,0.35) (143,4296,0.36) (774,3285,0.29)
//	Doubao_1_5_thinking_pro(785,90,0.18) (785,92,0.19) (805,284,0.31) (805,302,0.32) (809,4134,2.77) (809,4146,2.79) (4812,1835,1.94)
//	deepseek-V3            (781,12,0.06) (781,19,0.07) (801,277,0.16) (801,246,0.14) (4788,126,0.42)
//	deepseek-V3-0324       (781,18,0.13) (781,22,0.13) (801,250,0.25) (801,234,0.24) (4788,126,0.83)
//
// ── 误差口径（如实声明，不吹）────────────────────────────────────
//   - 输出系数 b 通常是输入系数 a 的 3~4 倍，所以**估算对 completion 敏感**；
//     prompt 长、输出短的请求估算会略偏低，反之略偏高。
//   - 上游调价 / 活动期倍率（客户端 features.activity_discount 有补贴倍率）
//     会改变真实扣费。本表是 2026-10-08 快照，调用方必须把结果标成
//     estimated，不得当成上游真值。
//   - 未标定模型用 fallback（取 DeepSeek-V3 档），宁可略高也不要把花钱报少。

// CreditRate 一个模型的计费双系数（单位：credits / 1K token）。
type CreditRate struct {
	Input  float64 `json:"input_per_1k"`
	Output float64 `json:"output_per_1k"`
	// Measured=true 表示来自实测标定；false = 兜底档。
	Measured bool `json:"measured"`
}

// creditRates 实测标定表（键为小写上游模型名）。
var creditRates = map[string]CreditRate{
	"seed_m8":                 {Input: 0.0268, Output: 0.0820, Measured: true},
	"doubao_1_5_thinking_pro": {Input: 0.1589, Output: 0.6405, Measured: true},
	"deepseek-v3":             {Input: 0.0789, Output: 0.3356, Measured: true},
	"deepseek-v3-0324":        {Input: 0.1607, Output: 0.4809, Measured: true},
}

// creditRateFallback 未标定模型的兜底系数。
//
// 取 DeepSeek-V3 档而不是最小值：Trae 里多数是付费模型，用最便宜的档会
// 系统性低估成本；宁可略高也不要把花钱报少。
var creditRateFallback = CreditRate{Input: 0.0789, Output: 0.3356, Measured: false}

// CreditRateFor 返回模型的双系数（大小写不敏感 + 后缀匹配）。
//
// 后缀匹配是为了兼容网关拿到的带前缀值（"trae:seed_m8"）与显示名形态。
func CreditRateFor(model string) CreditRate {
	k := strings.ToLower(strings.TrimSpace(model))
	if k == "" {
		return creditRateFallback
	}
	if r, ok := creditRates[k]; ok {
		return r
	}
	for name, r := range creditRates {
		if strings.HasSuffix(k, name) {
			return r
		}
	}
	return creditRateFallback
}

// CreditPer1K 返回模型折算单价（credits / 1K token）与是否来自实测标定。
//
// 这是"单一数字"的向后兼容视图：按 30% 输入 / 70% 输出的典型对话构成加权。
// 需要精确折算请用 EstimateCreditTokens（它分别乘输入/输出系数）。
func CreditPer1K(model string) (float64, bool) {
	r := CreditRateFor(model)
	return blendedPer1K(r), r.Measured
}

// blendedPer1K 把双系数压成一个 credits/1K 数字（30/70 混合）。
func blendedPer1K(r CreditRate) float64 {
	return 0.3*r.Input + 0.7*r.Output
}

// EstimateCreditTokens 按输入的 token 构成估算本次 Trae 调用扣掉的积分。
//
// 这是**首选**接口：Trae 的 token_usage 帧同时给 prompt_tokens 与
// completion_tokens，双系数建模比单倍率准得多（实测 ±5% 内）。
//
// 返回值 ok=false 表示 token 不可用（上游没给 usage），此时不得编造积分。
// calibrated 表示系数来自实测标定（false = 兜底档，误差更大）。
func EstimateCreditTokens(model string, promptTokens, completionTokens int64) (credit float64, calibrated, ok bool) {
	if promptTokens <= 0 && completionTokens <= 0 {
		return 0, false, false
	}
	r := CreditRateFor(model)
	c := (r.Input*float64(promptTokens) + r.Output*float64(completionTokens)) / 1000
	return c, r.Measured, true
}

// EstimateCredit 按 total token 用量估算积分（只有 total 可用时的回落路径）。
//
// 语义与旧版一致：单倍率 × total。当上游只给了 total_tokens、没给
// prompt/completion 拆分时用这条；能拿到拆分时请用 EstimateCreditTokens。
func EstimateCredit(model string, totalTokens int64) (credit float64, calibrated, ok bool) {
	if totalTokens <= 0 {
		return 0, false, false
	}
	r := CreditRateFor(model)
	return blendedPer1K(r) * float64(totalTokens) / 1000, r.Measured, true
}

// ModelCostPer1K 供面板展示：返回 (输入系数, 输出系数, 是否实测)。
func ModelCostPer1K(model string) (in, out float64, measured bool) {
	r := CreditRateFor(model)
	return r.Input, r.Output, r.Measured
}
