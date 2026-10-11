package pool

import (
	"testing"
	"time"

	"github.com/jjjane233/workbuddy-3in1/internal/auth"
)

// 全冷却兜底的 provider 串号防线（线上事故回归，2026-10-12）。
//
// 事故形态：兜底原先只过滤 realm、不过滤 provider。某 provider 的号全部冷却后，
// 兜底会捞走**另一个 provider** 的号，把 A 家凭据发到 B 家端点上换回 401，
// 并把该错误记到 B 家号头上：
//
//	ZCode 池空 → 兜底抓 Trae 号 → 拿 Trae 凭据打 ZCode 端点 → 401
//	           → noteUpstreamHard(uid, "401 未授权") 记在 Trae 号上，硬冷却到次日 04:00
//	Trae  池空 → 兜底抓 ZCode 号 → 拿 ZCode 凭据打 Trae 端点 → 401 code 1001
//	           → noteUpstreamSoft(uid, "trae chat error: ...") 记在 ZCode 号上并熔断
//
// 双向投毒、互相冷却。下游症状是 trae: 模型整片 503 no_available_account，
// 面板上则是「ZCode 号报 trae 的 401」这种自相矛盾的状态。

// TestFallbackDoesNotCrossProviderIntoZCode 锁死反向投毒：zcode 池无候选时，
// 不得抓走可兜底的 trae 号；同时同 provider 兜底本身照常工作。
func TestFallbackDoesNotCrossProviderIntoZCode(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "tr1", Provider: auth.ProviderTrae})
	p.Add(&auth.Auth{UID: "zc1", Provider: auth.ProviderZCode})

	// zcode 侧无兜底候选（CoolHard 被兜底排除）；trae 号软冷却、可兜底。
	// 修复前：zcode 作用域会选中 tr1，把 trae 凭据打去 zcode 端点。
	p.Cooldown("zc1", CoolHard, time.Hour, "402 额度不足")
	p.Cooldown("tr1", CoolSoft, 2*time.Hour, "429")

	if got := p.PickExcludingForScope(nil, "", "", auth.ProviderZCode); got != nil {
		t.Fatalf("zcode 作用域不得兜底到 trae 号，got %+v", got)
	}
	// 同 provider 兜底不受影响：trae 作用域仍应选中 tr1。
	got := p.PickExcludingForScope(nil, "", "", auth.ProviderTrae)
	if got == nil || got.UID != "tr1" {
		t.Fatalf("trae 作用域应兜底选 tr1，got %+v", got)
	}
}

// TestFallbackDoesNotCrossProviderIntoTrae 锁死事故的正向投毒（面板上「ZCode 号
// 报 trae 401」那一行）与「本 provider 无候选时兜底必须返回 nil」的语义。
func TestFallbackDoesNotCrossProviderIntoTrae(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "tr1", Provider: auth.ProviderTrae})
	p.Add(&auth.Auth{UID: "zc1", Provider: auth.ProviderZCode})

	// Trae 号 hard 冷却（事故现场就是被误硬冷却到次日 04:00），
	// ZCode 号软冷却且到期更早——修复前它正是被 trae 作用域捞走的那一个。
	p.Cooldown("tr1", CoolHard, 3*time.Hour, "401 未授权")
	p.Cooldown("zc1", CoolSoft, time.Minute, "429")

	if got := p.PickExcludingForScope(nil, "glm-5.3-flash", "", auth.ProviderTrae); got != nil {
		t.Fatalf("trae 作用域不得兜底到 zcode 号，got %+v", got)
	}
	// zcode 作用域内 zc1 可兜底，理应正常返回（同 provider，不误伤）。
	got := p.PickExcludingForScope(nil, "glm-4.7", "", auth.ProviderZCode)
	if got == nil || got.UID != "zc1" {
		t.Fatalf("zcode 作用域应兜底选 zc1，got %+v", got)
	}
}

// TestFallbackEarliestExpiryWithinProvider provider 过滤不得破坏「取最早到期」的
// 原有排序语义：跨 provider 的更早到期号必须被跳过，本 provider 内仍取最早者。
func TestFallbackEarliestExpiryWithinProvider(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "tr-late", Provider: auth.ProviderTrae})
	p.Add(&auth.Auth{UID: "tr-early", Provider: auth.ProviderTrae})
	p.Add(&auth.Auth{UID: "zc-soonest", Provider: auth.ProviderZCode})

	p.Cooldown("tr-late", CoolSoft, 2*time.Hour, "x")
	p.Cooldown("tr-early", CoolSoft, time.Hour, "x")
	p.Cooldown("zc-soonest", CoolSoft, time.Minute, "x") // 到期最早，但 provider 不符

	got := p.PickExcludingForScope(nil, "", "", auth.ProviderTrae)
	if got == nil || got.UID != "tr-early" {
		t.Fatalf("trae 兜底应选本 provider 内最早到期的 tr-early，got %+v", got)
	}
}

// TestFallbackRespectsProviderAndRealm provider 与 realm 是两个正交的硬过滤维度，
// 兜底必须同时满足；任一维不符都不得选中。
func TestFallbackRespectsProviderAndRealm(t *testing.T) {
	withNoPickGap(t)
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	p := New("")
	p.Add(&auth.Auth{UID: "tr-cn", Provider: auth.ProviderTrae})
	p.Add(&auth.Auth{UID: "tr-global", Provider: auth.ProviderTrae, Domain: "www.workbuddy.ai"})
	p.Add(&auth.Auth{UID: "zc-global", Provider: auth.ProviderZCode, Domain: "www.workbuddy.ai"})

	p.Cooldown("tr-cn", CoolSoft, time.Minute, "x")     // 到期最早，但 realm 不符
	p.Cooldown("tr-global", CoolSoft, 2*time.Hour, "x") // 唯一同时满足 global + trae
	p.Cooldown("zc-global", CoolSoft, time.Minute, "x") // realm 符合，但 provider 不符

	got := p.PickExcludingForScope(nil, "", "global", auth.ProviderTrae)
	if got == nil || got.UID != "tr-global" {
		t.Fatalf("global+trae 兜底应选 tr-global，got %+v", got)
	}

	// cn + trae：tr-global/zc-global 被 realm 排除，tr-cn 应被选中。
	got = p.PickExcludingForScope(nil, "", "cn", auth.ProviderTrae)
	if got == nil || got.UID != "tr-cn" {
		t.Fatalf("cn+trae 兜底应选 tr-cn，got %+v", got)
	}
}
