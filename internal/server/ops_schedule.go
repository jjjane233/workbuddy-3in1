package server

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/jjjane233/workbuddy-3in1/internal/auth"
	"github.com/jjjane233/workbuddy-3in1/internal/zcode"
)

// Trae / ZCode 后台运维排程（Phase 5b）。
//
// 与 CodeBuddy 链路的 scheduler 完全独立：后者跑在 internal/scheduler，处理签到/
// 旅行/成长任务等 CodeBuddy 专属任务；本循环只遍历 provider=trae/zcode 的账号，
// 两边互不干扰——这也正是「调度需能区分三类账号」的落点。
//
// 时点任务（签到/领积分/活动/保活）用「当天小时」做去重键：同一时段内只打一次上游，
// 避免每轮 tick 重复领取。余额/额度是只读查询，按较短周期刷新即可。

// UpstreamSchedule 上游运维排程配置。零值字段走下列默认值。
type UpstreamSchedule struct {
	// Enabled 总开关。false = 完全不启动循环（存量配置零影响）。
	Enabled bool
	// TraeCheckinHours Trae 每日签到小时，默认 [9,21]。
	TraeCheckinHours []int
	// TraeActivityHour 商业活动领取小时，默认 10。
	TraeActivityHour int
	// TraeKeepaliveHour 保活小时，默认 22。
	TraeKeepaliveHour int
	// TraeUsageMinutes 额度刷新周期（分钟），默认 30。
	TraeUsageMinutes int
	// TraeActivityMinutes Trae 活动巡检周期（分钟），默认 30。
	//
	// 为什么是周期而不是固定小时：Trae 的活动（新人/发消息/学生认证/裂变）
	// 是不定时上线的，只在 10 点打一次等于当天 15 点上线的活动要等到明天。
	// 30 分钟一轮既能在活动出现后很快领到，又不会把上游打毛。
	TraeActivityMinutes int
	// ZCodeClaimHours ZCode 领积分小时，默认 [3]。
	//
	// 与 Trae 不同，ZCode 的可领方案在 Preview 里是明列的，固定小时+滚动
	// 兜底即可；但方案同样是**不定时上线**的，所以另有 ZCodePreviewMinutes
	// 做低成本轮询（只看有没有新方案，不触发验证码）。
	ZCodeClaimHours []int
	// ZCodePreviewMinutes ZCode 可领方案巡检周期（分钟），默认 30。
	// 只调 Preview（轻量只读），发现新方案才触发带验证码的 Claim。
	ZCodePreviewMinutes int
	// ZCodeRewardMinutes ZCode 邀请/奖励总览巡检周期（分钟），默认 60。
	ZCodeRewardMinutes int
	// ZCodeBalanceMinutes 余额刷新周期（分钟），默认 5。
	ZCodeBalanceMinutes int
	// TickMinutes 循环粒度，默认 1（每分钟检查一次该跑哪些任务）。
	TickMinutes int
}

func (s UpstreamSchedule) withDefaults() UpstreamSchedule {
	if len(s.TraeCheckinHours) == 0 {
		s.TraeCheckinHours = []int{9, 21}
	}
	if s.TraeActivityHour == 0 {
		s.TraeActivityHour = 10
	}
	if s.TraeKeepaliveHour == 0 {
		s.TraeKeepaliveHour = 22
	}
	if s.TraeUsageMinutes <= 0 {
		s.TraeUsageMinutes = 30
	}
	if s.TraeActivityMinutes <= 0 {
		s.TraeActivityMinutes = 30
	}
	if len(s.ZCodeClaimHours) == 0 {
		s.ZCodeClaimHours = []int{3}
	}
	if s.ZCodeBalanceMinutes <= 0 {
		s.ZCodeBalanceMinutes = 5
	}
	if s.ZCodePreviewMinutes <= 0 {
		s.ZCodePreviewMinutes = 30
	}
	if s.ZCodeRewardMinutes <= 0 {
		s.ZCodeRewardMinutes = 60
	}
	if s.TickMinutes <= 0 {
		s.TickMinutes = 1
	}
	return s
}

// upstreamOps 实例内的排程运行时状态。
type upstreamOps struct {
	mu sync.Mutex
	// lastRun 记录每个时点任务当天的完成标记（key 形如 "trae-checkin:2026-10-05T09"）。
	lastRun map[string]bool
	// lastTick 记录上一次执行的分钟刻度，用于周期性任务去重。
	lastTick map[string]time.Time
	// zcodeSeen 记录每个 ZCode 账号上轮见过的可领方案 id 集合，
	// 用于判断 Preview 是否出现了**新**方案（见 zcodeNewPlans）。
	zcodeSeen map[string]map[string]bool
}

// StartUpstreamOps 启动 Trae / ZCode 后台运维循环。
// Enabled=false 时直接返回，不产生任何 goroutine（存量部署零影响）。
func (h *Handler) StartUpstreamOps(ctx context.Context, sched UpstreamSchedule) {
	if !sched.Enabled {
		return
	}
	s := sched.withDefaults()
	h.opsMu.Lock()
	if h.ops == nil {
		h.ops = &upstreamOps{
			lastRun:   map[string]bool{},
			lastTick:  map[string]time.Time{},
			zcodeSeen: map[string]map[string]bool{},
		}
	}
	h.opsMu.Unlock()

	go func() {
		// 启动即做一次 ZCode 凭据全量自动同步（force）：面板一打开，池里的
		// ZCode 号就已经带上最新签名凭据，新登录的客户端账号也已入池。
		if msg := h.zcodeAutoSync(ctx, true); msg != "" {
			logUpstream("%s", msg)
		}
		// 启动即刷一轮余额/额度，让面板打开就有数据。
		h.RunUpstreamStartupRefresh(ctx)
		t := time.NewTicker(time.Duration(s.TickMinutes) * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				h.runUpstreamOpsOnce(ctx, s, now)
			}
		}
	}()
	logUpstream("调度已启动：checkin=%v activity=%d(定时)+%dm(滚动) keepalive=%d usage=%dm | zcode claim=%v balance=%dm preview=%dm reward=%dm",
		s.TraeCheckinHours, s.TraeActivityHour, s.TraeActivityMinutes, s.TraeKeepaliveHour, s.TraeUsageMinutes,
		s.ZCodeClaimHours, s.ZCodeBalanceMinutes, s.ZCodePreviewMinutes, s.ZCodeRewardMinutes)
}

// runUpstreamOpsOnce 执行本轮该跑的任务。
func (h *Handler) runUpstreamOpsOnce(ctx context.Context, s UpstreamSchedule, now time.Time) {
	day := now.Format("2006-01-02")
	hour := now.Hour()

	// 先做一轮 ZCode 凭据自动同步：本机客户端重解出的 <keyId>.<secret> 会
	// 在下面这轮账号操作里被用到，先同步能避免用旧 secret 打上游。
	// 开关关闭或本机没客户端时这里是空操作。
	if msg := h.zcodeAutoSync(ctx, false); msg != "" {
		logUpstream("%s", msg)
	}

	for _, st := range h.cfg.Pool.List() {
		a := h.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.IsDisabled() {
			continue
		}
		switch a.ProviderOf() {
		case "trae":
			h.runTraeOps(ctx, a, s, day, hour, now)
		case "zcode":
			h.runZCodeOps(ctx, a, s, day, hour, now)
		}
	}
}

// hourlyTask 每日小时任务：命中配置小时且当天该槽位未跑过才执行。
func (h *Handler) hourlyTask(key, day string, hour int, hours []int) bool {
	for _, hh := range hours {
		if hour != hh {
			continue
		}
		k := fmt.Sprintf("%s:%sT%02d", key, day, hh)
		h.opsMu.Lock()
		defer h.opsMu.Unlock()
		if h.ops.lastRun[k] {
			return false
		}
		h.ops.lastRun[k] = true
		// 只保留最近 3 天的标记，避免 map 无限增长。
		if len(h.ops.lastRun) > 64 {
			for kk := range h.ops.lastRun {
				if len(kk) < 10 || kk[:len(day)] < day {
					delete(h.ops.lastRun, kk)
				}
			}
		}
		return true
	}
	return false
}

// periodicTask 周期任务：距上次执行超过 interval 才跑。
func (h *Handler) periodicTask(key string, now time.Time, interval time.Duration) bool {
	h.opsMu.Lock()
	defer h.opsMu.Unlock()
	if last, ok := h.ops.lastTick[key]; ok && now.Sub(last) < interval {
		return false
	}
	h.ops.lastTick[key] = now
	return true
}

func (h *Handler) runTraeOps(ctx context.Context, a *auth.Auth, s UpstreamSchedule, day string, hour int, now time.Time) {
	if h.hourlyTask("trae-checkin", day, hour, s.TraeCheckinHours) {
		if res := h.TraeCheckin(ctx, a); res.Message != "" {
			logUpstream("trae checkin %s: %s", a.Display(), res.Message)
		}
	}
	// 活动领取：**滚动巡检**而不是只在配置小时打一次。
	//
	// 为什么改：Trae 的活动是不定时上线的（新人券、发消息奖励、学生认证、
	// 裂变……），固定 10 点只能覆盖"10 点前已上线"的那些，其余要等明天。
	// 现在每 TraeActivityMinutes（默认 30m）巡检一次，活动一上线就能领到。
	//
	// TraeActivityHour 保留但语义弱化：命中该小时时**额外**跑一次全量巡检
	// （含 benefit 权益发放），其余轮次也跑，只是日志降噪（只在有结果时打）。
	scheduled := hour == s.TraeActivityHour && h.hourlyTask("trae-activity", day, hour, []int{hour})
	if scheduled || h.periodicTask("trae-activity:"+a.UID, now, time.Duration(s.TraeActivityMinutes)*time.Minute) {
		res, err := h.TraeActivity(ctx, a)
		switch {
		case err != nil:
			logUpstream("trae activity %s: %v", a.Display(), err)
		case len(res.Claimed) > 0 || len(res.Failed) > 0:
			// 有实际动作或真失败才打日志，避免每 30 分钟刷一行"无可领取"。
			logUpstream("trae activity %s: %s", a.Display(), res.Summary())
		case scheduled && len(res.Pending) > 0:
			// 定时那轮把"暂不可用"也记一次，便于日报观察活动是否还没开。
			logUpstream("trae activity %s: %s", a.Display(), res.Summary())
		}
	}
	if hour == s.TraeKeepaliveHour && h.hourlyTask("trae-keepalive", day, hour, []int{hour}) {
		if err := h.TraeKeepalive(ctx, a); err != nil {
			logUpstream("trae keepalive %s: %v", a.Display(), err)
		}
	}
	if h.periodicTask("trae-usage:"+a.UID, now, time.Duration(s.TraeUsageMinutes)*time.Minute) {
		if _, err := h.TraeUsage(ctx, a); err != nil {
			logUpstream("trae usage %s: %v", a.Display(), err)
		}
	}
}

func (h *Handler) runZCodeOps(ctx context.Context, a *auth.Auth, s UpstreamSchedule, day string, hour int, now time.Time) {
	if h.hourlyTask("zcode-claim", day, hour, s.ZCodeClaimHours) {
		if res := h.ZCodeClaim(ctx, a, ""); res.Message != "" {
			logUpstream("zcode claim %s: ok=%v %s", a.Display(), res.OK, res.Message)
		}
	}
	if h.periodicTask("zcode-balance:"+a.UID, now, time.Duration(s.ZCodeBalanceMinutes)*time.Minute) {
		if _, err := h.ZCodeBalance(ctx, a); err != nil {
			logUpstream("zcode balance %s: %v", a.Display(), err)
		}
	}
	// 可领方案巡检：只调 Preview（轻量只读、不需要验证码），
	// 发现**新**方案才触发带验证码的 Claim。这是"不定时活动自动领"的低成本路径：
	// 没新方案时一轮只花一次 GET，有新方案时立刻领掉，不用等到凌晨 3 点。
	if h.periodicTask("zcode-preview:"+a.UID, now, time.Duration(s.ZCodePreviewMinutes)*time.Minute) {
		plans, err := h.ZCodePreview(ctx, a)
		switch {
		case err != nil:
			logUpstream("zcode preview %s: %v", a.Display(), err)
		case len(plans) > 0:
			ids := make([]string, 0, len(plans))
			for _, p := range plans {
				ids = append(ids, firstNonEmpty(p.Name, p.PlanID))
			}
			if h.zcodeNewPlans(a.UID, plans) {
				logUpstream("zcode preview %s: 发现新方案 %v，触发领取", a.Display(), ids)
				if res := h.ZCodeClaim(ctx, a, ""); res.Message != "" {
					logUpstream("zcode claim(触发) %s: ok=%v %s", a.Display(), res.OK, res.Message)
				}
			}
		}
	}
	// 邀请/奖励总览巡检：ZCode 的邀请奖励同样是活动期内不定时到账的，
	// 定期拉一次总览即可在面板体现（总览本身不发券，仅供观测与提示）。
	if h.periodicTask("zcode-rewards:"+a.UID, now, time.Duration(s.ZCodeRewardMinutes)*time.Minute) {
		if _, err := h.ZCodeRewards(ctx, a); err != nil {
			logUpstream("zcode rewards %s: %v", a.Display(), err)
		}
	}
}

// zcodeNewPlans 判断本轮 Preview 是否出现了此前没见过的方案。
//
// 为什么要去重：Preview 返回的是**当前全部**可领方案，已领过但还没从列表
// 消失的方案每轮都在。若不加去重就会每 30 分钟触发一次带验证码的 Claim
// （每次要拉起 Chrome 解无痕验证，代价高），得不偿失。
// 只在"方案集合出现了新 plan_id"时才触发领取。
func (h *Handler) zcodeNewPlans(uid string, plans []zcode.Plan) bool {
	cur := make(map[string]bool, len(plans))
	for _, p := range plans {
		cur[p.PlanID] = true
	}
	h.opsMu.Lock()
	defer h.opsMu.Unlock()
	if h.ops == nil {
		return len(plans) > 0
	}
	prev := h.ops.zcodeSeen[uid]
	if len(prev) == 0 {
		// 首轮：不立刻领（留给凌晨那轮或用户手动点），只建档，
		// 避免刚部署就把所有历史方案刷一遍验证码。
		h.ops.zcodeSeen[uid] = cur
		return false
	}
	isNew := false
	for id := range cur {
		if !prev[id] {
			isNew = true
			break
		}
	}
	h.ops.zcodeSeen[uid] = cur
	return isNew
}
