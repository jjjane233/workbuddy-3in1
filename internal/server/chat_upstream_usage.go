package server

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/jjjane233/workbuddy-3in1/internal/auth"
	"github.com/jjjane233/workbuddy-3in1/internal/pool"
	"github.com/jjjane233/workbuddy-3in1/internal/trae"
	"github.com/jjjane233/workbuddy-3in1/internal/usage"
)

// 三合一网关：Trae / ZCode 上游链路的**用量记账**（Phase 4c）。
//
// 为什么要单独一个文件：CodeBuddy 链路的记账汇聚在 handler.go 的 recordAttempt，
// 那里同时改池内累计器与 usage.Recorder。Trae / ZCode 走的是另一条完全独立的
// 调用路径（chat_upstream.go），且上游给 usage 的**位置与字段名都不同**：
//
//	ZCode（Anthropic Messages）  非流式：body.usage.{input_tokens,output_tokens}
//	                             流式：message_start.message.usage.input_tokens
//	                                   message_delta.usage.output_tokens
//	Trae（IDE 私有 SSE）         event: token_usage → data.{prompt_tokens,
//	                             completion_tokens,total_tokens,reasoning_tokens}
//
// 两条链路最终都汇聚到 recordUpstreamUsage：池内累计器（账号行的
// prompt/completion/合计）与 usage.Recorder（用量页的 provider 维度）同源，
// 两条口径不会漂移。
//
// 记账口径与 CodeBuddy 链路对齐：**ok 由「上游是否给出 usage」判定**。
// 传输错误 / 4xx / 解析失败都没有 usage，一律计为失败尝试——用量页的
// 「失败尝试」列正是靠它才能看见重试放大。

// upstreamUsage 一次上游尝试观测到的 token 用量。
// 三个 Has* 显式区分「上游回了 0」与「上游没有这个字段」：后者不该覆盖累计值，
// 也不该把一个未观测的尝试算成成功样本。
type upstreamUsage struct {
	PromptTokens  int64
	HasPrompt     bool
	Completion    int64
	HasCompletion bool
	Total         int64
	HasTotal      bool
	CacheHit      int64
	CacheMiss     int64
	HasCache      bool
	// CacheWrite 是**写入缓存**的 token（Anthropic 的 cache_creation_input_tokens）。
	//
	// 为什么必须与 CacheMiss 分开（4.3 遗留之一，2026-10-08 修）：
	// Anthropic 的语义是三段而非两段 ——
	//
	//	input_tokens                本次**未命中**、要全价处理的输入
	//	cache_read_input_tokens     命中缓存、走缓存价的输入
	//	cache_creation_input_tokens 本回合**新建**缓存所写入的输入（1.25x 价）
	//
	// 旧实现把 cache_creation 直接映射到 CacheMiss，等于「写入」当「未命中」，
	// 而**真正的未命中** input_tokens 反而被丢掉 —— 命中率会系统性偏高。
	// 现在三分：hit = cache_read，miss = input_tokens，write = cache_creation。
	CacheWrite    int64
	HasCacheWrite bool
	// CreditComputed / HasCreditComputed 上游没给积分真值时的网关折算值
	// （Trae 链路按实测单价 × token 估）。由 traeCreditUsage 填，
	// 记账双写（池内台账 + usage 估算槽）与归档打标共用同一份值。
	CreditComputed    float64
	HasCreditComputed bool
}

// traeCreditUsage 给一次 Trae 尝试按实测标定的双系数折算积分，写回 u 并返回。
//
// 只认 trae（provider 判定由调用方保证）：ZCode 是套餐 token 计量，
// 与积分不是一个量纲，折算会造出假数据。token 不可用（上游没给 usage）
// 时原样返回，绝不编造积分。
//
// 为什么用 EstimateCreditTokens 而不是单倍率 EstimateCredit：
// Trae 的 token_usage 帧同时给 prompt_tokens 与 completion_tokens，而实测
// 标定表明计费是「输入/输出双系数」（输出系数约是输入的 3~4 倍，见
// internal/trae/credit_rate.go 的样本表）。单倍率会把长 prompt/短输出的请求
// 算高、把短 prompt/长输出的请求算低（seed_m8 实测偏差可达 60%），
// 双系数在同批干净样本上误差 ±5% 内。
//
// 只有 total 而无拆分的降级帧才回落到 EstimateCredit（单倍率）。
func traeCreditUsage(model string, u upstreamUsage, ok bool) upstreamUsage {
	if !ok {
		return u
	}
	var cr float64
	var cok bool
	if u.HasPrompt || u.HasCompletion {
		cr, _, cok = trae.EstimateCreditTokens(model, u.PromptTokens, u.Completion)
	} else if tot, hasT := u.total(); hasT {
		cr, _, cok = trae.EstimateCredit(model, tot)
	}
	if !cok || cr <= 0 {
		return u
	}
	u.CreditComputed, u.HasCreditComputed = cr, true
	return u
}

// any 至少观测到一个 token 维度（判定这次尝试是否真的拿到了 usage）。
func (u upstreamUsage) any() bool {
	return u.HasPrompt || u.HasCompletion || u.HasTotal
}

// total 合计 token：上游没给 total 时用 prompt+completion 兜底。
func (u upstreamUsage) total() (int64, bool) {
	if u.HasTotal {
		return u.Total, true
	}
	if u.HasPrompt || u.HasCompletion {
		return u.PromptTokens + u.Completion, true
	}
	return 0, false
}

// recordUpstreamUsage 把一次 Trae / ZCode 尝试计入池内累计器与用量台账。
//
// provider 一律取自账号本身（a.ProviderValue()），不信任调用方传值：
// 面板的 provider 维度、账号行与上游链路必须指向同一个标识，否则
// 「用量」页会与「账号池」页对不上号。
func (h *Handler) recordUpstreamUsage(a *auth.Auth, model string, u upstreamUsage, latency time.Duration, ok bool) {
	if a == nil {
		return
	}
	uid := a.UID
	provider := a.ProviderValue()
	realm := "cn"
	if r := a.Realm(); r != "" {
		realm = r
	}
	if model == "" {
		model = "(unknown)"
	}

	latencyMs := latency.Milliseconds()
	if latencyMs < 1 {
		latencyMs = 1
	}

	// 池内累计器：与 CodeBuddy 链路同一个入口（面板账号行的 Token 列）。
	delta := pool.TokenUsageDelta{
		Model:               model,
		HasPromptTokens:     u.HasPrompt,
		PromptTokens:        u.PromptTokens,
		HasCompletionTokens: u.HasCompletion,
		CompletionTokens:    u.Completion,
		HasLatencyMs:        true,
		LatencyMs:           latencyMs,
	}
	if t, hasT := u.total(); hasT {
		delta.HasTotalTokens = true
		delta.TotalTokens = t
	}
	if u.HasCompletion && latencyMs > 0 {
		// 吐字速率只用真实采样到的 completion（观测不到就不编造样本）。
		delta.HasTokensPerSecond = true
		delta.TokensPerSecond = float64(u.Completion) * 1000 / float64(latencyMs)
	}
	h.cfg.Pool.RecordTokenUsage(uid, delta)

	// 积分折算（Trae）：上游响应里没有 credit 字段，只有 token。折算值由
	// traeCreditUsage 在调用方填进 u（见 chat_upstream.go），这里只负责落到
	// 两个消费端：
	//   · 池内成本台账（NoteModelCost）——账号行的积分成本、选号的积分保底
	//   · usage.Recorder 的估算槽（EstimatedCredits）——用量页的估算列
	// 积分折算值由**调用方**算好放进 u（Trae 走 traeCreditUsage、ZCode 走
	// zcodeCreditUsage），这里只做消费。为什么不在本函数里算：
	// 折算需要"只累加一次"的当日 token 分母（ZCode 的 rate 依赖它），
	// 若归档与台账各算一遍，分母会被重复累加，rate 系统性偏低。
	var computedCredit float64
	var hasComputed bool
	if u.HasCreditComputed {
		computedCredit, hasComputed = u.CreditComputed, true
		if t, hasT := u.total(); hasT {
			h.cfg.Pool.NoteModelCost(uid, model, computedCredit, int(t))
		}
	}
	// ZCode：顺手触发一次后台真值刷新（非阻塞，带 TTL 与单飞保护）。
	if provider == auth.ProviderZCode {
		h.maybeRefreshZCodeCredits(a)
	}

	if h.cfg.Usage == nil {
		return
	}
	total, hasTotal := u.total()
	d := usage.Delta{
		PromptTokens:      u.PromptTokens,
		HasPromptTokens:   u.HasPrompt,
		CompletionTokens:  u.Completion,
		HasCompletion:     u.HasCompletion,
		TotalTokens:       total,
		HasTotal:          hasTotal,
		HasCacheTokens:    u.HasCache,
		CacheHitTokens:    u.CacheHit,
		CacheMissTokens:   u.CacheMiss,
		LatencyMs:         latencyMs,
		HasLatency:        true,
		TokensPerSecond:   delta.TokensPerSecond,
		HasTPS:            delta.HasTokensPerSecond,
		CreditComputed:    computedCredit,
		HasCreditComputed: hasComputed,
	}
	// 有效样本标准与 CodeBuddy 链路一致：拿到任一 token 维度即算成功观测。
	h.cfg.Usage.AddProvider(time.Now(), provider, realm, uid, model, d, ok && u.any())
}

// parseZCodeUsage 从 Anthropic 风格 usage 对象取 token 计数。
//
// 字段来源（两处形态一致，故复用）：
//   - 非流式 body.usage
//   - 流式 message_start.message.usage / message_delta.usage
//
// 缓存字段的映射（4.3 遗留之一，2026-10-08 修正为精确三分）：
//
//	cache_read_input_tokens     → CacheHit   （命中，走缓存价）
//	input_tokens                → CacheMiss  （**真未命中**，全价）
//	cache_creation_input_tokens → CacheWrite （写入缓存，既非命中亦非未命中）
//
// 旧实现把 cache_creation 当 CacheMiss，把 input_tokens 丢掉，命中率会偏高。
// PromptTokens（总输入）按 Anthropic 的加法定律还原：input + read + creation，
// 否则面板「Prompt Tokens」会比真实小一大截（Anthropic 的 input_tokens 不含缓存部分）。
func parseZCodeUsage(raw json.RawMessage) upstreamUsage {
	var u struct {
		InputTokens              *int64 `json:"input_tokens"`
		OutputTokens             *int64 `json:"output_tokens"`
		CacheReadInputTokens     *int64 `json:"cache_read_input_tokens"`
		CacheCreationInputTokens *int64 `json:"cache_creation_input_tokens"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &u) != nil {
		return upstreamUsage{}
	}
	var out upstreamUsage
	var inTok, readTok, createTok int64
	if u.InputTokens != nil {
		inTok = *u.InputTokens
	}
	if u.CacheReadInputTokens != nil {
		readTok = *u.CacheReadInputTokens
	}
	if u.CacheCreationInputTokens != nil {
		createTok = *u.CacheCreationInputTokens
	}
	// 总输入 = 未命中 + 命中 + 新建（Anthropic 的 input_tokens 只是第一段）。
	if u.InputTokens != nil || u.CacheReadInputTokens != nil || u.CacheCreationInputTokens != nil {
		out.PromptTokens = inTok + readTok + createTok
		out.HasPrompt = true
	}
	if u.OutputTokens != nil {
		out.Completion = *u.OutputTokens
		out.HasCompletion = true
	}
	// 只有上游**显式**给了缓存维度才算有缓存。只回 input_tokens 的响应是全价输入，
	// 声称「有缓存」会让命中率分母凭空多出一块，反而失真。
	if u.CacheReadInputTokens != nil || u.CacheCreationInputTokens != nil {
		out.HasCache = true
		out.CacheHit = readTok
		out.CacheMiss = inTok // 真未命中是 input_tokens，不是 cache_creation
	}
	if u.CacheCreationInputTokens != nil {
		out.CacheWrite = createTok
		out.HasCacheWrite = true
	}
	return out
}

// mergeZCodeUsage 合并同一响应的多帧 usage（流式下 input 在 message_start、
// output 在 message_delta，末帧计数才是终值）。后到的字段覆盖先到的，缺席的不动。
func mergeZCodeUsage(dst upstreamUsage, src upstreamUsage) upstreamUsage {
	if src.HasPrompt {
		dst.PromptTokens, dst.HasPrompt = src.PromptTokens, true
	}
	if src.HasCompletion {
		dst.Completion, dst.HasCompletion = src.Completion, true
	}
	if src.HasTotal {
		dst.Total, dst.HasTotal = src.Total, true
	}
	if src.HasCache {
		dst.CacheHit, dst.CacheMiss, dst.HasCache = src.CacheHit, src.CacheMiss, true
	}
	if src.HasCacheWrite {
		dst.CacheWrite, dst.HasCacheWrite = src.CacheWrite, true
	}
	return dst
}

// parseTraeUsage 从 Trae 的 token_usage 事件载荷取计数。
// 兼容两种承载：字段平铺（prompt_tokens 在顶层）与包一层 data。
func parseTraeUsage(data string) upstreamUsage {
	var m map[string]json.RawMessage
	if json.Unmarshal([]byte(data), &m) != nil {
		return upstreamUsage{}
	}
	// 上游降级形态：{ "data": { ... } }
	if inner, ok := m["data"]; ok {
		var m2 map[string]json.RawMessage
		if json.Unmarshal(inner, &m2) == nil {
			m = m2
		}
	}
	pick := func(keys ...string) *int64 {
		for _, k := range keys {
			raw, ok := m[k]
			if !ok {
				continue
			}
			var n int64
			if json.Unmarshal(raw, &n) == nil {
				return &n
			}
			var f float64
			if json.Unmarshal(raw, &f) == nil {
				v := int64(f)
				return &v
			}
		}
		return nil
	}
	var out upstreamUsage
	if v := pick("prompt_tokens", "input_tokens"); v != nil {
		out.PromptTokens, out.HasPrompt = *v, true
	}
	if v := pick("completion_tokens", "output_tokens"); v != nil {
		out.Completion, out.HasCompletion = *v, true
	}
	if v := pick("total_tokens"); v != nil {
		out.Total, out.HasTotal = *v, true
	}
	return out
}

// traeUsageEvent 判定一个 SSE 事件是否是 token_usage（事件名或载荷类型命中）。
func traeUsageEvent(eventName, data string) bool {
	if n := strings.ToLower(strings.TrimSpace(eventName)); n != "" {
		if n == "token_usage" || n == "usage" || strings.Contains(n, "token_usage") {
			return true
		}
	}
	// 事件名缺失/被上游省略时按载荷特征兜底：含 prompt_tokens 或 total_tokens
	// 才当 usage——普通 content 帧不会带这两个键，不会误判成用量。
	return strings.Contains(data, "prompt_tokens") || strings.Contains(data, "total_tokens")
}
