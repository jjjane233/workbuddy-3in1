package panel

import (
	"context"
	"net/http"

	"github.com/jjjane233/workbuddy-3in1/internal/auth"
)

// UpstreamOps 三合一网关的 Trae / ZCode 账号操作，由 server.Handler 实现。
//
// 【为什么用接口而不是直接 import server】
// 依赖方向固定为 panel → server 之外还要留出降级空间：cmd/server 同时导入两者，
// 构造顺序是「先建面板，再建 handler」，因此面板创建时还拿不到 *server.Handler。
// 用接口 + SetUpstreamOps 注入，既避免依赖环，又让面板在 ops 未注入时（裸用、
// 单元测试、只跑 CodeBuddy 的部署）安全降级为「该操作未启用」而不是 panic。
//
// 【为什么签名用基本类型】
// 不把 server.TraeCheckinResult / zcode.Billing 之类结构体泄漏进面板，
// 两个包只在「做了什么、拿到什么」这一层对齐，后续改结果结构不会牵连面板。
type UpstreamOps interface {
	// Checkin 签到（Trae）。返回今日是否已签 + 可展示的说明。
	Checkin(ctx context.Context, a *auth.Auth) (done bool, msg string)
	// Balance 刷新余额/额度（Trae 额度、ZCode 积分）。返回剩余与总额。
	Balance(ctx context.Context, a *auth.Auth) (remain, total int64, err error)
	// Activity 领取商业活动（Trae）。返回处理成功的条数。
	Activity(ctx context.Context, a *auth.Auth) (n int, err error)
	// Keepalive 保活（Trae）。
	Keepalive(ctx context.Context, a *auth.Auth) error
	// Claim 领取积分（ZCode）。
	Claim(ctx context.Context, a *auth.Auth) (ok bool, msg string)
	// --- 面板对等视图（与 CodeBuddy 的 /api/models、/api/packages、任务中心同构）---

	// Models 该上游的模型与档位列表（供「模型与档位」页按 provider 分流）。
	// 返回 (模型条目, 错误)；模型条目字段与 /panel/api/models 的 CodeBuddy 条目同形。
	Models(ctx context.Context, provider string) ([]map[string]any, error)
	// ModelsFor 同 Models，但携带账号上下文。
	//
	// 为什么需要单独一个方法：ZCode 的可调模型分两个来源——
	// 公开目录（匿名）与余额授权（需要账号）。后者只有拿到账号才能拉，
	// 所以面板必须把账号传进来，否则「领了额度却看不到模型」的 bug 会回来。
	// a 为 nil 时行为等同 Models。
	ModelsFor(ctx context.Context, provider string, a *auth.Auth) ([]map[string]any, error)
	// Packages 该上游账号的积分/额度构成（供「积分构成」页按 provider 分流）。
	Packages(ctx context.Context, a *auth.Auth) (UpstreamPackageView, error)
	// Tasks 该上游账号的任务/权益清单（供「任务中心」与单号弹窗按 provider 分流）。
	Tasks(ctx context.Context, a *auth.Auth) ([]UpstreamTask, error)
	// TaskClaim 领取单个任务/权益奖励；code 为空表示「全部领取」。
	TaskClaim(ctx context.Context, a *auth.Auth, code string) (ok bool, msg string)
	// ActivityScan 立刻跑一轮该账号的上游活动巡检（Trae 活动 / ZCode 可领方案），
	// 返回可读摘要。后台排程也在跑同一套逻辑，这里给运维一个"现在就试"的入口。
	ActivityScan(ctx context.Context, a *auth.Auth) (msg string, err error)
}

// SetUpstreamOps 注入上游操作（cmd/server 在 handler 构建完成后调用）。
// 面板 handler 是并发服务，注入必须发生在 http.Server 开始监听之前——
// main.go 的构造顺序天然满足（先建 panel，再建 handler，最后 ListenAndServe）。
func (p *Panel) SetUpstreamOps(ops UpstreamOps) { p.upstream = ops }

// upstreamOrErr 返回注入的 ops；未注入时返回 false，调用方给 501 而不是崩。
func (p *Panel) upstreamOrErr(w http.ResponseWriter) (UpstreamOps, bool) {
	if p.upstream == nil {
		writeErr(w, http.StatusNotImplemented, "上游操作未启用（该部署只运行 CodeBuddy 链路）")
		return nil, false
	}
	return p.upstream, true
}

// UpstreamPackageView 一个上游账号的积分/额度构成（provider 无关的展示视图）。
//
// 字段刻意与 CodeBuddy 的 packages 行同形（remain/size/packages），
// 面板前端不必为 Trae / ZCode 写第二套渲染；provider 字段用于排查来源。
type UpstreamPackageView struct {
	Provider string            `json:"provider"`
	Remain   int64             `json:"remain"`
	Size     int64             `json:"size"`
	Packages []UpstreamPackage `json:"packages"`
	Note     string            `json:"note,omitempty"`
	// Source 账号来源标注（ZCode 的 zai / bigmodel；Trae 的 cn）。
	// 同一 uid 在两个账号体系里额度互相独立，不标出来会被误读成"额度丢了"。
	Source string `json:"source,omitempty"`
	// PendingTotal 可领取但尚未领取的额度合计（单位与上游一致）。
	// 与 Remain 分开统计：Remain 是"已到手能用"，PendingTotal 是"点一下就有"。
	PendingTotal int64 `json:"pending_total,omitempty"`
}

// UpstreamPackage 单个积分/额度包。
type UpstreamPackage struct {
	Name   string `json:"name"`
	Remain int64  `json:"remain"`
	Used   int64  `json:"used"`
	Size   int64  `json:"size"`
	// ExpiresAt unix 秒；0 = 无到期
	ExpiresAt int64  `json:"expires_at,omitempty"`
	Note      string `json:"note,omitempty"`
	// Pending 为 true 表示这是**可领取**项（尚未领取），不是已到手的额度。
	// 面板把它渲染成"待领取"样式，并给出跳任务中心的入口。
	Pending bool `json:"pending,omitempty"`
	// PlanID 可领取方案的上游标识（供「任务中心」直接领取）。
	PlanID string `json:"plan_id,omitempty"`
	// GrantSize 该可领取项的额度面额（上游 grant_units 原值）。
	GrantSize int64 `json:"grant_size,omitempty"`
	// Models 本包可用的模型标识列表（如 ["glm-5.3-flash"]）。
	//
	// 上游把「这笔额度能喂给哪些模型」写在 capabilities 而不是名字里。
	// 不透出来的话，用户会看到「余额 100000000 / 100000000」
	// 却完全不知道这笔钱只能花在 glm-5.3-flash 上。
	Models []string `json:"models,omitempty"`
}

// UpstreamTask 一条任务/权益（面板「任务中心」的行）。
type UpstreamTask struct {
	Code         string `json:"task_code"`
	Name         string `json:"name"`
	Status       string `json:"status,omitempty"` // done / pending / claimable / locked
	AcceptStatus string `json:"accept_status,omitempty"`
	Claimed      bool   `json:"claimed"`
	Locked       bool   `json:"locked"`
	Claimable    bool   `json:"claimable"`
	Current      int64  `json:"current,omitempty"`
	Target       int64  `json:"target,omitempty"`
	Credit       int64  `json:"credit,omitempty"`
	Message      string `json:"message,omitempty"`
}
