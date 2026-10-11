package server

// zcode_credit_rate.go —— ZCode 逐次调用的**积分**折算（2026-10-08）。
//
// 事实（线上实测，见 internal/zcode/monitor.go）：
//
//	ZCode 对话响应（Anthropic Messages）的 usage 只有 token，没有 credit：
//	  {"usage":{"input_tokens":14,"output_tokens":32,
//	            "cache_read_input_tokens":0,"server_tool_use":{...}}}
//	真积分只存在于**聚合**端点：
//	  GET /api/monitor/credit-usage/activity   -> series[].totalCredits（逐日真值）
//	  GET /api/monitor/usage/quota/limit       -> limits[].{number,usage,remaining}
//
// 所以「逐次积分」不可能从响应体直接读到。这里用**真值锚定的按 token 分摊**：
//
//	rate = 当日真实消耗积分 / 当日累计 token        （credits per token）
//	credit(本次) = rate × 本次 token
//
// 为什么这个估算比"拍一个静态单价"可靠得多：
//   · 分子是上游给的**真值**（不是猜的）；
//   · 分母是本网关当日实际转发给该号的 token 总量（自己数的，精确）；
//   · 两者相除得到的 rate 天然吸收了模型差异、缓存折扣、时段折扣——
//     只要是同一个号、同一天，rate 就是该号当天的**实际综合单价**。
//
// 已知误差来源（如实标注，不掩饰）：
//   1. 分母只统计**经本网关**的 token；用户在官方客户端里自己用的量不在内，
//      会把 rate 算高。网关独占该号时误差可忽略，混用时会偏高。
//   2. 当日真值有刷新延迟（上游按小时/天聚合），刚起量的头几笔会用到旧 rate。
//   3. rate 未标定前（新号、当日还没数据）不编造数字，如实回 ok=false。

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jjjane233/workbuddy-3in1/internal/auth"
)

// zcodeCreditState 单账号当日的积分折算状态（进程内）。
type zcodeCreditState struct {
	mu   sync.Mutex
	day  string  // YYYY-MM-DD（本地时区；上游 series 也是 Asia/Shanghai 日粒度）
	toks int64   // 当日经本网关转发的 token 累计
	cred float64 // 当日真实积分（来自 monitor，0 = 未标定）
	// hasCred 区分"真值是 0"与"还没拿到"：前者说明当天没消耗，rate 无效；
	// 后者说明还没刷新过，要触发一次拉取。
	hasCred    bool
	lastFetch  time.Time
	refreshing bool
}

// zcodeCreditStates 各账号的折算状态（进程内，随重启清空——重建成本只是一次拉取）。
var zcodeCreditStates sync.Map // uid -> *zcodeCreditState

// zcodeCreditFetchTTL 真值刷新间隔。上游 activity 是日粒度聚合，5 分钟足够新，
// 又不会把 monitor 端点打爆（一个号 288 次/天）。
const zcodeCreditFetchTTL = 5 * time.Minute

// zcodeState 取（或建）某账号当日状态；跨天自动重置。
func zcodeState(uid string) *zcodeCreditState {
	day := time.Now().Format("2006-01-02")
	v, _ := zcodeCreditStates.LoadOrStore(uid, &zcodeCreditState{day: day})
	st := v.(*zcodeCreditState)
	st.mu.Lock()
	if st.day != day {
		// 跨天：token 归零、真值作废（昨天的 rate 不能拿来算今天）。
		st.day, st.toks, st.cred, st.hasCred = day, 0, 0, false
		st.lastFetch = time.Time{}
	}
	st.mu.Unlock()
	return st
}

// noteZCodeTokens 累加当日 token 并返回累加值。
func noteZCodeTokens(uid string, tokens int64) int64 {
	if uid == "" || tokens <= 0 {
		if uid == "" {
			return 0
		}
	}
	st := zcodeState(uid)
	st.mu.Lock()
	defer st.mu.Unlock()
	st.toks += tokens
	return st.toks
}

// zcodeCreditRate 返回当前可用的折算率（credits / token）。
//
// ok=false 表示尚未标定（当日没有真值、或当日还没有 token 分母）——
// 此时调用方不得编造积分，应按"上游未提供"处理。
func zcodeCreditRate(uid string) (float64, bool) {
	if uid == "" {
		return 0, false
	}
	st := zcodeState(uid)
	st.mu.Lock()
	defer st.mu.Unlock()
	if !st.hasCred || st.cred <= 0 || st.toks <= 0 {
		return 0, false
	}
	return st.cred / float64(st.toks), true
}

// setZCodeRealCredits 写入当日真值（由刷新协程调用）。
func setZCodeRealCredits(uid string, credits float64) {
	st := zcodeState(uid)
	st.mu.Lock()
	defer st.mu.Unlock()
	st.cred = credits
	st.hasCred = true
	st.lastFetch = time.Now()
}

// zcodeCreditPer1KAtomic 按 token 兜底单价（credits/1K token），面板热改即时生效。
var zcodeCreditPer1KAtomic atomic.Value // float64

// setZCodeCreditPer1K 写入兜底单价（0 = 关闭兜底）。
func setZCodeCreditPer1K(v float64) { zcodeCreditPer1KAtomic.Store(v) }

// getZCodeCreditPer1K 读取兜底单价（未设置回 0）。
func getZCodeCreditPer1K() float64 {
	if v, ok := zcodeCreditPer1KAtomic.Load().(float64); ok {
		return v
	}
	return 0
}

// estimateZCodeCredit 折算一次 ZCode 调用的积分。
//
// 先按本次 token 累加当日分母（顺序很重要：分母必须含本次，否则首笔请求会
// 用 0 分母），再按当前 rate 折算。rate 未标定时回落到配置的按 token 兜底
// 单价（credits/1K）；兜底也关闭（0）时才回 ok=false。
func estimateZCodeCredit(uid, model string, tokens int64) (credit float64, ok bool) {
	if uid == "" || tokens <= 0 {
		return 0, false
	}
	noteZCodeTokens(uid, tokens)
	if rate, rok := zcodeCreditRate(uid); rok {
		return rate * float64(tokens), true
	}
	if per1K := getZCodeCreditPer1K(); per1K > 0 {
		return per1K * float64(tokens) / 1000, true
	}
	return 0, false
}

// zcodeCreditUsage 是 chat_upstream.go 的统一入口：给一次 ZCode 尝试填上
// 折算积分（CreditComputed / HasCreditComputed）。
//
//   - token 不可用（上游没给 usage）→ 原样返回，绝不编造积分；
//   - rate 未标定（新号 / 当日上游还没出数）→ 原样返回，归档保持
//     credit_known=false、面板显示「上游未提供」。
//
// **只调一次**：本函数内部会把本次 token 累加进当日分母，重复调用会让分母虚高。
func zcodeCreditUsage(uid, model string, u upstreamUsage) upstreamUsage {
	t, hasT := u.total()
	if !hasT || t <= 0 {
		return u
	}
	cr, ok := estimateZCodeCredit(uid, model, t)
	if !ok || cr <= 0 {
		return u
	}
	u.CreditComputed, u.HasCreditComputed = cr, true
	return u
}

// maybeRefreshZCodeCredits 在后台（不阻塞请求）刷新该号当日真积分。
//
// 触发条件：距上次拉取超过 TTL。用 refreshing 标志保证同一账号同时只有
// 一个在飞请求——批量并发时不会对上游打成风暴。
func (h *Handler) maybeRefreshZCodeCredits(a *auth.Auth) {
	if a == nil || a.ProviderValue() != auth.ProviderZCode {
		return
	}
	uid := a.UID
	st := zcodeState(uid)
	st.mu.Lock()
	if st.refreshing || time.Since(st.lastFetch) < zcodeCreditFetchTTL {
		st.mu.Unlock()
		return
	}
	st.refreshing = true
	st.mu.Unlock()

	go func() {
		defer func() {
			st.mu.Lock()
			st.refreshing = false
			st.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		act, err := h.zcodeClient(a).CreditActivity(ctx, 1)
		if err != nil {
			return
		}
		setZCodeRealCredits(uid, act.DayCredits(time.Now().Format("2006-01-02")))
	}()
}

// SetZCodeCreditPer1K 面板保存配置后的热生效入口（0 = 关闭兜底）。
func SetZCodeCreditPer1K(v float64) { setZCodeCreditPer1K(v) }
// ZCodeCreditRateFor 供面板/诊断读取某账号当前的折算率（credits/1K token）与是否已标定。
// 真值锚定可用时返回当日分摊率；否则返回配置的兜底单价（calibrated=false）。
func (h *Handler) ZCodeCreditRateFor(uid string) (per1K float64, calibrated bool) {
	if r, ok := zcodeCreditRate(uid); ok {
		return r * 1000, true
	}
	if v := getZCodeCreditPer1K(); v > 0 {
		return v, false
	}
	return 0, false
}
