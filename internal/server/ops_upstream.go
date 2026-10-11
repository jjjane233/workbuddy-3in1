package server

import (
	"context"
	"errors"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jjjane233/workbuddy-3in1/internal/auth"
	"github.com/jjjane233/workbuddy-3in1/internal/pool"
	"github.com/jjjane233/workbuddy-3in1/internal/trae"
	"github.com/jjjane233/workbuddy-3in1/internal/zcode"
)

// logUpstream 上游运维日志前缀，与 CodeBuddy 链路的 [upstream] 前缀区分来源。
func logUpstream(format string, args ...any) {
	log.Printf("[upstream] "+format, args...)
}

// Trae / ZCode 的账号运维操作（Phase 5a），平移自 gw2api/internal/server/ops.go。
// 面板按钮与后台调度共用这一层，避免两套调用口径。
//
// 与 gw2api 的适配差异：
//   - 池 API 换 workbuddy 语义：Disable 为 (uid, reason) 两参；无 SetLastErrMsg
//     （本仓库存错误详情在 reqlog，池内只记时间戳）；NoteClaimDone 缺失 → NoteSuccess。
//   - 上下文用 h.cfg.Captcha（可为 nil，此时不预取验证码）。
//   - IDEVersion / BaseURL / HTTPClient 均走包内默认值，等 Phase 5c 加配置段再接。

// ---- Trae：签到 / 额度 / 保活 / 活动 ----

// TraeCheckinResult 签到结果。
type TraeCheckinResult struct {
	UID       string `json:"uid"`
	Name      string `json:"name"`
	Enable    bool   `json:"enable"`
	CheckedIn bool   `json:"checked_in"`
	Claimed   bool   `json:"claimed"`
	Credits   int64  `json:"credits"`
	Message   string `json:"message"`
}

// TraeCheckin 执行签到：状态为可签且未签到时领取。
func (h *Handler) TraeCheckin(ctx context.Context, a *auth.Auth) *TraeCheckinResult {
	res := &TraeCheckinResult{UID: a.UID, Name: a.Display()}
	cli := trae.New(a, trae.Options{})
	st, err := cli.CheckinStatus(ctx)
	if err != nil {
		if err == trae.ErrUnauthorized {
			h.cfg.Pool.Disable(a.UID, "checkin: unauthorized")
		}
		res.Message = err.Error()
		return res
	}
	res.Enable = st.Enable
	res.CheckedIn = st.CheckedIn
	res.Credits = st.Credits
	if !st.Enable {
		res.Message = "签到未开放"
		return res
	}
	if st.CheckedIn {
		h.cfg.Pool.NoteCheckinDone(a.UID)
		res.Message = "今日已签到"
		return res
	}
	cr, err := cli.ClaimCheckin(ctx)
	if err != nil {
		res.Message = err.Error()
		return res
	}
	res.Message = cr.Message
	res.Credits = cr.Credits
	res.Claimed = cr.OK
	if cr.OK {
		h.cfg.Pool.NoteCheckinDone(a.UID)
	}
	return res
}

// TraeUsage 刷新 Trae 额度并写入池。
func (h *Handler) TraeUsage(ctx context.Context, a *auth.Auth) (*trae.Usage, error) {
	cli := trae.New(a, trae.Options{})
	u, err := cli.QueryUsage(ctx, 1)
	if err != nil {
		return nil, err
	}
	h.cfg.Pool.SetCredits(a.UID, u.Remaining, u.Total)
	return u, nil
}

// TraeKeepalive 保活：拉一次付费状态 + 用户信息，维持会话活跃。
func (h *Handler) TraeKeepalive(ctx context.Context, a *auth.Auth) error {
	cli := trae.New(a, trae.Options{})
	if _, err := cli.QueryPayStatus(ctx); err != nil {
		return err
	}
	_, err := cli.GetUserInfo(ctx)
	return err
}

// TraeActivityResult 一次活动巡检的结果（供面板与调度展示/记录）。
type TraeActivityResult struct {
	// Claimed 本轮真正领取成功的活动（业务码 0 / 无 code）。
	Claimed []string
	// Pending 上游明确说"暂不可用/暂无权限"的活动（9090 / 9070 等）。
	// 这类**不是错误**：活动还没到可领时机或账号不满足条件，下轮再试即可。
	Pending []string
	// Failed 参数错 / 网络错等真失败，附原因。
	Failed []string
	// Honored BenefitGrant 的受理结果（通用权益一次性发放）。
	Honored string
}

// Summary 拼一行可读摘要。
func (r TraeActivityResult) Summary() string {
	parts := []string{}
	if len(r.Claimed) > 0 {
		parts = append(parts, "领取成功 "+strconv.Itoa(len(r.Claimed)))
	}
	if len(r.Pending) > 0 {
		parts = append(parts, "暂不可用 "+strconv.Itoa(len(r.Pending)))
	}
	if len(r.Failed) > 0 {
		parts = append(parts, "失败 "+strconv.Itoa(len(r.Failed)))
	}
	if r.Honored != "" {
		parts = append(parts, "权益："+r.Honored)
	}
	if len(parts) == 0 {
		return "无可领取活动"
	}
	return strings.Join(parts, " · ")
}

// TraeActivity 巡检并领取账号当前可领的 Trae 活动。
//
// 为什么不能只看 HTTP 状态码：上游 REST 层对所有业务结果都回 200，
// 真正的成败在 body 的 code 字段（9090 活动暂不可用 / 9070 暂无领取权限 /
// 4001 参数错）。旧实现 `err == nil` 就当成功，于是日志把"暂不可用"
// 记成已领取——这正是活动额度一直没到账的原因。
//
// 领取链路按 activity_type 分流（从 Trae CN 3.3.96 客户端 main.js 逆向）：
//
//	activity_type == 103（裂变）→ work_fission/grant {activity_id}
//	其余类型                   → pay/grant_work_benefit（无 activity_id，
//	                              服务端按账号可领清单发放）
func (h *Handler) TraeActivity(ctx context.Context, a *auth.Auth) (TraeActivityResult, error) {
	var res TraeActivityResult
	cli := trae.New(a, trae.Options{})
	info, err := cli.QueryActivityInfo(ctx, 1)
	if err != nil {
		return res, err
	}

	// 通用权益：一条请求覆盖所有 activity_type != 103 的可领项。
	if st, raw, err := cli.BenefitGrant(ctx, a.DeviceID, 1); err != nil {
		res.Failed = append(res.Failed, "benefit: "+err.Error())
	} else if code, msg, ok := trae.ActivityCode(raw); !ok {
		res.Failed = append(res.Failed, "benefit: http "+strconv.Itoa(st))
	} else if code == 0 {
		res.Honored = "已发放"
		if msg != "" {
			res.Honored = msg
		}
	} else if isPendingCode(code) {
		res.Pending = append(res.Pending, "benefit("+strconv.Itoa(code)+")")
	} else {
		res.Failed = append(res.Failed, "benefit("+strconv.Itoa(code)+"): "+msg)
	}

	for _, act := range info.Activities {
		id := strField(act, "activity_id", "activityId", "id")
		if id == "" || !boolField(act, "Enabled", "enabled") {
			continue
		}
		typ := numField(act, "activity_type")
		extra, _ := act["work_extra"].(map[string]any)
		// 已到手的跳过——上游 granted=true 才是硬事实，
		// grantable=false 但 granted 缺失的活动（如 work_fission_202607）
		// 仍然要试一次，否则会漏掉"活动期间随时可领"的那一类。
		if extra != nil {
			if granted, _ := extra["granted"].(bool); granted {
				continue
			}
		}
		if typ != 103 {
			// 非裂变类型已由 BenefitGrant 统一处理，这里不重复打。
			continue
		}
		eligible := false
		if extra != nil {
			if v, ok := extra["eligible"].(bool); ok {
				eligible = v
			}
		}
		st, raw, err := cli.FissionGrant(ctx, id, eligible, 1)
		if err != nil {
			res.Failed = append(res.Failed, id+": "+err.Error())
			continue
		}
		code, msg, ok := trae.ActivityCode(raw)
		switch {
		case !ok:
			res.Failed = append(res.Failed, id+": http "+strconv.Itoa(st))
		case code == 0:
			res.Claimed = append(res.Claimed, id)
		case isPendingCode(code):
			res.Pending = append(res.Pending, id+"("+strconv.Itoa(code)+")")
		default:
			res.Failed = append(res.Failed, id+"("+strconv.Itoa(code)+"): "+msg)
		}
	}
	return res, nil
}

// isPendingCode 上游「非错误但没领到」的业务码：活动暂不可用 / 暂无领取权限。
//
//	9090 活动暂不可用   9070 暂无领取权限
//
// 这类下轮还会再试，不算失败，避免运维日志被刷成一片红。
func isPendingCode(code int) bool { return trae.IsPendingCode(code) }

// ---- ZCode：余额 / 方案 / 领取 / 积分总览 ----

// zcodeClient 构造 ZCode 客户端（全部走包内默认值）。
func (h *Handler) zcodeClient(a *auth.Auth) *zcode.Client {
	return zcode.New(a, zcode.Options{
		Timeout:     60 * time.Second,
		HTTPClient:  h.cfg.UpstreamHTTP,
		MonitorHost: zcodeMonitorHostFor(a),
	})
}

// zcodeMonitorHostFor 按账号属地挑 monitor 命名空间的首选宿主。
//
// 为什么按属地：「周额度/积分」是各属地独立的后台账，z.ai 的号在
// open.bigmodel.cn 上查会回 401，反之亦然。另一个宿主会自动兜底
// （见 zcode.monitorHosts），这里只决定谁先打。
func zcodeMonitorHostFor(a *auth.Auth) string {
	if a != nil && strings.EqualFold(strings.TrimSpace(a.Source), "zai") {
		return zcode.MonitorHostZAI
	}
	return zcode.MonitorHostBigModel
}

// ZCodeCredits ZCode 账号的**真实积分**快照（monitor 命名空间，2026-10-08 还原）。
//
// 与 billing/* 的区别：billing 给的是"方案权益桶"（token 计量、且
// zcode.z.ai 那套需要官方客户端签名才 200），monitor 给的是**平台积分口径**——
// 即「GLM Coding Lite = 每周 10000 积分」的那个积分数，是用户真正关心的数字。
type ZCodeCredits struct {
	// HasPlan=false 表示该号没有 Coding Plan 套餐（上游明确回"不存在coding plan"），
	// 此时 Remaining/Total 均为 0，但**不是错误**。
	HasPlan      bool    `json:"has_plan"`
	Remaining    int64   `json:"remaining"`
	Total        int64   `json:"total"`
	Used         int64   `json:"used"`
	ResetAt      int64   `json:"reset_at,omitempty"`
	TodayCredits float64 `json:"today_credits"`
	Level        string  `json:"level,omitempty"`
	// Models 本周期各模型的积分/用量明细（quota/limit 的 usageDetails）。
	Models []zcode.MonitorUsageDetail `json:"models,omitempty"`
	// Source 本次数字的来源："quota/limit"（真实）、"billing"（回落）、"none"。
	Source string `json:"source"`
}

// ZCodeCredits 拉取真实积分。三层语义：
//
//  1. monitor/quota/limit 200 → 真实积分（Source="quota/limit"）
//  2. 上游明确回"不存在coding plan" → HasPlan=false（Source="none"，不是错误）
//  3. 其他失败（网络/鉴权）→ 回落到 billing 的权益桶（Source="billing"）
//
// 为什么要有第 3 层：部分号的凭据只对 billing 有效（老号 JWT），
// 此时监控端点 401，但 billing 还能给出 token 桶——有数总比没数好，
// 只要把 Source 标清楚，面板就不会把 token 当积分误导用户。
func (h *Handler) ZCodeCredits(ctx context.Context, a *auth.Auth) (*ZCodeCredits, error) {
	cli := h.zcodeClient(a)
	out := &ZCodeCredits{Source: "none"}
	q, err := cli.QuotaLimit(ctx, false)
	if err == nil {
		if l := pickCreditLimit(q.Limits); l != nil {
			out.HasPlan = true
			out.Total = l.Number
			out.Used = l.Usage
			out.Remaining = l.Remaining
			out.ResetAt = l.NextResetTime
			out.Level = q.Level
			out.Models = l.UsageDetails
			out.Source = "quota/limit"
			// 今日真实消耗（非致命：拿不到就留 0，不影响额度）。
			if act, aerr := cli.CreditActivity(ctx, 1); aerr == nil {
				out.TodayCredits = act.DayCredits(time.Now().Format("2006-01-02"))
			}
			return out, nil
		}
		// 200 但 limits 空：有套餐但周期额度未启用，按无套餐处理。
		out.HasPlan = false
		return out, nil
	}
	var un *zcode.MonitorUnavailableError
	if errors.As(err, &un) {
		// 明确无套餐：不回落（billing 也没有积分），如实回 0。
		return out, nil
	}
	// 真失败：回落 billing 权益桶，标 Source 让面板区分口径。
	b, _, berr := cli.Balance(ctx)
	if berr == nil && b != nil {
		remain, total, earliest := b.Remaining()
		out.HasPlan = total > 0
		out.Remaining = remain
		out.Total = total
		out.ResetAt = earliest
		out.Source = "billing"
		return out, nil
	}
	return out, err
}

// pickCreditLimit 从 limits[] 里挑出"周期积分额度"那条。
//
// 优先级（与官方客户端 zye() 一致）：
//  1. type=TIME_LIMIT         —— 周期额度（每周/每月重置）
//  2. 任意带 remaining 的条目
//  3. 第一条
func pickCreditLimit(ls []zcode.MonitorLimit) *zcode.MonitorLimit {
	if len(ls) == 0 {
		return nil
	}
	for i := range ls {
		if ls[i].Type == "TIME_LIMIT" {
			return &ls[i]
		}
	}
	for i := range ls {
		if ls[i].Remaining != 0 || ls[i].Number != 0 {
			return &ls[i]
		}
	}
	return &ls[0]
}

// ZCodeBalance 刷新 ZCode 账号余额并写入池。
//
// 顺序：先拿真实积分（monitor），拿到就写权威值；没有套餐/失败再回落 billing。
// 这样池里的 credits 对"买了 Coding Plan"的号是**真积分**，对没有的号仍是 token 桶，
// 两种口径都通过 Source 可追溯。
//
// 【口径标注】两条分支写池时都显式声明 creditsUnit（见 pool/credits_unit.go）：
// Coding Plan → 积分；billing 回落 → 按上游 unit_type（老号是 token，量级 1e8）。
// 不标注的代价是 creditFloor（如 100）会拿 1 亿 token 与 100 积分比大小——
// 判据恒过、形同虚设；上游一旦改粗 token 单位，同一个阈值又会突然误拦正常账号。
func (h *Handler) ZCodeBalance(ctx context.Context, a *auth.Auth) (*zcode.Billing, error) {
	if cr, err := h.ZCodeCredits(ctx, a); err == nil && cr != nil && cr.HasPlan {
		h.cfg.Pool.SetCreditsUnitDetailed(a.UID, pool.CreditsUnitCredit,
			cr.Remaining, cr.Total, 0, timeAt(cr.ResetAt), cr.Remaining)
		// 顺带把今日真实消耗缓存给面板（额度卡片的副行）。
		h.noteZCodeCreditsSnapshot(a.UID, cr)
		return &zcode.Billing{
			Balances: []zcode.Balance{{
				ShowName:       "Coding Plan",
				TotalUnits:     cr.Total,
				UsedUnits:      cr.Used,
				RemainingUnits: cr.Remaining,
				ExpiresAt:      cr.ResetAt,
				UnitType:       "credit",
				Meter:          "credit_usage",
				AvailableUnits: cr.Remaining,
			}},
			ServerTime: time.Now().Unix(),
		}, nil
	}
	// 回落：老号 JWT / 无套餐号仍走 billing 权益桶。
	b, _, err := h.zcodeClient(a).Balance(ctx)
	if err != nil {
		return nil, err
	}
	remain, total, earliest := b.Remaining()
	h.cfg.Pool.SetCreditsUnitDetailed(a.UID, zcodeBillingUnit(b), remain, total, 0, timeAt(earliest), remain)
	return b, nil
}

// zcodeBillingUnit 决定 billing 回落分支写池时该标注的余额口径。
//
// 抽成独立函数是为了让"口径决策"可被直接测试——它是本次加固唯一可能写错的地方
// （忘了标注、或标反），而把整条 ZCodeBalance 拉起来测需要给 zcode 客户端搭
// HTTP mock（monitor/billing 宿主是硬编码域名），成本远高于收益。
func zcodeBillingUnit(b *zcode.Billing) pool.CreditsUnit {
	if b == nil {
		// 防御性：Balance 正常不会回 (nil, nil)，但口径判定不该是 panic 的入口。
		// 退到 Credit 即历史行为。
		return pool.CreditsUnitCredit
	}
	return pool.CreditsUnitFromUpstream(b.SoleUnitType())
}

// ZCodeCreditsDetail 供面板「余额」按钮展示的 ZCode 积分明细（真值 + 逐模型）。
//
// 走**可选接口**（panel 侧用类型断言探测）而不是往 panel.UpstreamOps 里加方法：
// 后者会让所有实现了该接口的测试替身与新链路一起改签名，风险远大于收益。
func (h *Handler) ZCodeCreditsDetail(ctx context.Context, a *auth.Auth) (map[string]any, error) {
	cr, err := h.ZCodeCredits(ctx, a)
	if err != nil {
		return nil, err
	}
	out := map[string]any{
		"has_plan":      cr.HasPlan,
		"credits":       cr.Remaining,
		"credits_total": cr.Total,
		"used":          cr.Used,
		"source":        cr.Source,
	}
	if cr.ResetAt > 0 {
		out["reset_at"] = cr.ResetAt
	}
	if cr.TodayCredits > 0 {
		out["today_credits"] = cr.TodayCredits
	}
	if cr.Level != "" {
		out["level"] = cr.Level
	}
	if len(cr.Models) > 0 {
		ms := make([]map[string]any, 0, len(cr.Models))
		for _, m := range cr.Models {
			ms = append(ms, map[string]any{
				"model": m.ModelCode, "name": m.DisplayName, "usage": m.Usage,
			})
		}
		out["models"] = ms
	}
	// 折算率（credits/1K token，当日真值分摊口径）——面板可显示"实测单价"。
	if per1K, ok := h.ZCodeCreditRateFor(a.UID); ok {
		out["credit_per_1k"] = per1K
	}
	return out, nil
}

// zcodeCreditSnapshots 缓存各账号最近一次真实积分快照（面板用，进程内）。
var zcodeCreditSnapshots sync.Map // uid -> *ZCodeCredits

// noteZCodeCreditsSnapshot 记一次真实积分快照。
func (h *Handler) noteZCodeCreditsSnapshot(uid string, cr *ZCodeCredits) {
	if uid == "" || cr == nil {
		return
	}
	zcodeCreditSnapshots.Store(uid, cr)
}

// ZCodeCreditsSnapshot 读某账号最近一次真实积分快照（可能为 nil）。
func ZCodeCreditsSnapshot(uid string) *ZCodeCredits {
	if v, ok := zcodeCreditSnapshots.Load(uid); ok {
		if cr, ok := v.(*ZCodeCredits); ok {
			return cr
		}
	}
	return nil
}

// ZCodePreview 查询可领取的方案。
func (h *Handler) ZCodePreview(ctx context.Context, a *auth.Auth) ([]zcode.Plan, error) {
	return h.zcodeClient(a).Preview(ctx)
}

// ZCodeClaimResult 领取结果（面板 / 调度共享）。
type ZCodeClaimResult struct {
	UID     string `json:"uid"`
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Code    int    `json:"code"`
	Message string `json:"message"`
	PlanID  string `json:"plan_id,omitempty"`
	Raw     string `json:"raw,omitempty"`
}

// ZCodeClaim 领取指定方案；planID 为空时自动挑选第一个可领取方案。
func (h *Handler) ZCodeClaim(ctx context.Context, a *auth.Auth, planID string) *ZCodeClaimResult {
	res := &ZCodeClaimResult{UID: a.UID, Name: a.Display()}
	cli := h.zcodeClient(a)
	if planID == "" {
		// 空 planID 视为"领全部"：单领第一个会漏掉并存的可领方案。
		return h.ZCodeClaimAll(ctx, a)
	}
	var verifyParam, region string
	if h.cfg.Captcha != nil {
		vp, err := h.cfg.Captcha.Get(ctx)
		if err != nil {
			res.Message = "captcha: " + err.Error()
			return res
		}
		verifyParam = vp
		region = a.Region
		if region == "" {
			region = "cn"
		}
	}
	cr, err := cli.Claim(ctx, planID, verifyParam, region)
	if err != nil {
		res.Message = err.Error()
		return res
	}
	res.Code = cr.Code
	res.Message = cr.Msg
	res.Raw = cr.Raw
	res.OK = cr.OK
	if cr.OK {
		// gw2api 用 NoteClaimDone；本池无该方法，NoteSuccess 同样清退避并记成功，
		// 语义上「领到积分」正是一次成功交互。
		h.cfg.Pool.NoteSuccess(a.UID)
	} else if zcode.IsCaptchaError(cr.Raw) && h.cfg.Captcha != nil {
		h.cfg.Captcha.Invalidate()
	}
	return res
}

// ZCodeClaimAll 领取当前**全部**可领方案，返回汇总结果。
//
// 为什么需要它：旧路径 ZCodeClaim(planID="") 只取 plans[0]，当账号同时有
// 两个可领方案（如 广告方案 + 邀请奖励）时第二个永远领不到。
//
// 实现要点：验证码按方案取——一个 verifyParam 通常只对一次 claim 有效，
// 所以每个方案都重新向求解器要一次（求解器有 TTL 缓存，同一秒内不会重复拉起 Chrome）。
// 任何单个方案失败都不中断其余方案，最后按"至少领到一个"判定 OK。
func (h *Handler) ZCodeClaimAll(ctx context.Context, a *auth.Auth) *ZCodeClaimResult {
	res := &ZCodeClaimResult{UID: a.UID, Name: a.Display()}
	cli := h.zcodeClient(a)
	plans, err := cli.Preview(ctx)
	if err != nil {
		res.Message = "preview: " + err.Error()
		return res
	}
	if len(plans) == 0 {
		res.Message = "无可领取方案"
		return res
	}
	var okIDs, failMsgs []string
	for _, p := range plans {
		var verifyParam, region string
		if h.cfg.Captcha != nil {
			vp, err := h.cfg.Captcha.Get(ctx)
			if err != nil {
				failMsgs = append(failMsgs, firstNonEmpty(p.Name, p.PlanID)+": captcha "+err.Error())
				continue
			}
			verifyParam = vp
			region = a.Region
			if region == "" {
				region = "cn"
			}
		}
		cr, err := cli.Claim(ctx, p.PlanID, verifyParam, region)
		if err != nil {
			failMsgs = append(failMsgs, firstNonEmpty(p.Name, p.PlanID)+": "+err.Error())
			continue
		}
		if cr.OK {
			okIDs = append(okIDs, firstNonEmpty(p.Name, p.PlanID))
			continue
		}
		if zcode.IsCaptchaError(cr.Raw) && h.cfg.Captcha != nil {
			h.cfg.Captcha.Invalidate()
		}
		failMsgs = append(failMsgs, firstNonEmpty(p.Name, p.PlanID)+"("+strconv.Itoa(cr.Code)+"): "+cr.Msg)
	}
	res.OK = len(okIDs) > 0
	res.PlanID = strings.Join(okIDs, ",")
	switch {
	case res.OK && len(failMsgs) == 0:
		res.Message = "已领取 " + strings.Join(okIDs, "、")
	case res.OK:
		res.Message = "已领取 " + strings.Join(okIDs, "、") + "；未成功：" + strings.Join(failMsgs, "；")
	case len(failMsgs) > 0:
		res.Message = strings.Join(failMsgs, "；")
	default:
		res.Message = "无可领取方案"
	}
	if res.OK {
		h.cfg.Pool.NoteSuccess(a.UID)
	}
	return res
}

// ZCodeRewards 拉取积分总览（面板）。
func (h *Handler) ZCodeRewards(ctx context.Context, a *auth.Auth) (*zcode.Summary, error) {
	return h.zcodeClient(a).RewardSummary(ctx)
}

// ---- 启动期全量刷新 ----

// RunUpstreamStartupRefresh 启动时按 provider 刷新一次余额/额度（后台 goroutine）。
// 只碰 Trae / ZCode 账号，CodeBuddy 链路的启动逻辑完全不经过这里。
func (h *Handler) RunUpstreamStartupRefresh(ctx context.Context) {
	for _, st := range h.cfg.Pool.List() {
		a := h.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.IsDisabled() {
			continue
		}
		switch a.ProviderOf() {
		case auth.ProviderZCode:
			if _, err := h.ZCodeBalance(ctx, a); err != nil {
				logUpstream("zcode balance %s: %v", a.Display(), err)
			}
		case auth.ProviderTrae:
			if _, err := h.TraeUsage(ctx, a); err != nil {
				logUpstream("trae usage %s: %v", a.Display(), err)
			}
		}
	}
}

// ---- 小helper（平移自 gw2api） ----

func strField(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func boolField(m map[string]any, keys ...string) bool {
	for _, k := range keys {
		if v, ok := m[k].(bool); ok {
			return v
		}
	}
	return false
}

func timeAt(unix int64) time.Time {
	if unix <= 0 {
		return time.Time{}
	}
	return time.Unix(unix, 0)
}
