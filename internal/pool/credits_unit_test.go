// 余额口径（credits_unit.go）的回归测试。
//
// 事故形态：entry.credits 被两种不可比的量共用——平台积分（量级 10^2~10^4）与
// ZCode 的 token 权益桶（量级 10^6~10^8）。creditFloor（线上为 100）此前无条件
// 与这个字段直接比较，于是：
//
//	1 亿 token 的桶被当成"1 亿积分"，floor 判据恒过、形同虚设；
//	一旦上游把 token 单位改粗（千/万 token），同一个阈值又会突然开始拦截
//	完全正常的账号，且没有任何日志能解释原因。
package pool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jjjane233/workbuddy-3in1/internal/auth"
)

// TestCreditsUnitFromUpstream 口径映射：只认 token，其余一切形态退回积分（历史行为）。
func TestCreditsUnitFromUpstream(t *testing.T) {
	cases := []struct {
		in   string
		want CreditsUnit
	}{
		{"token", CreditsUnitToken},
		{"TOKEN", CreditsUnitToken},
		{"  token  ", CreditsUnitToken},
		{"credit", CreditsUnitCredit},
		{"", CreditsUnitCredit},        // 空桶：无口径可标，退回默认
		{"credits", CreditsUnitCredit}, // 未知形态不得触发换判据
		{"points", CreditsUnitCredit},
	}
	for _, c := range cases {
		if got := CreditsUnitFromUpstream(c.in); got != c.want {
			t.Errorf("CreditsUnitFromUpstream(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestCreditsUnitString 口径名是 /status 与落盘共用的唯一来源。
func TestCreditsUnitString(t *testing.T) {
	if got := CreditsUnitCredit.String(); got != "credit" {
		t.Errorf("Credit.String() = %q, want credit", got)
	}
	if got := CreditsUnitToken.String(); got != "token" {
		t.Errorf("Token.String() = %q, want token", got)
	}
	// 越界值也必须回落到 credit 的名字（与判定分支同口径）。
	if got := CreditsUnit(7).String(); got != "credit" {
		t.Errorf("CreditsUnit(7).String() = %q, want credit", got)
	}
}

// TestNormalizeCreditsUnitDropsGarbage 持久化恢复时的越界值清洗。
// 不清洗会出现"显示 credit、判定按 token"的错位——最难排查的一类不一致。
func TestNormalizeCreditsUnitDropsGarbage(t *testing.T) {
	if got := normalizeCreditsUnit(7); got != CreditsUnitCredit {
		t.Errorf("normalizeCreditsUnit(7) = %v, want Credit", got)
	}
	if got := normalizeCreditsUnit(CreditsUnitToken); got != CreditsUnitToken {
		t.Errorf("normalizeCreditsUnit(Token) = %v, want Token", got)
	}
	if got := normalizeCreditsUnit(CreditsUnitCredit); got != CreditsUnitCredit {
		t.Errorf("normalizeCreditsUnit(Credit) = %v, want Credit", got)
	}
}

// floorFixture 搭一个 floor=150 的池（确定性随机源、关闭探索改道）。
func floorFixture(t *testing.T) *Pool {
	t.Helper()
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.SetCreditFloor(150)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	return p
}

// paidBalance 声明"该号在该模型上实测收费"，并把余额**设为其后**。
//
// 【顺序很重要】NoteModelCost 的 credit 参数是**本次消耗量**，它会顺手扣减余额
// （state.go 的 credits 签到外回写）。先设余额再记账会被扣掉 1，把一个"非空"的
// 桶变成空桶，于是测到的是扣减而非口径。所有用例统一先记账、后设余额。
func paidBalance(t *testing.T, p *Pool, uid string, unit CreditsUnit, credits int64) {
	t.Helper()
	p.Add(&auth.Auth{UID: uid})
	p.NoteModelCost(uid, "paid-model", 1.0, 1000)
	p.SetCreditsUnit(uid, unit, credits, credits)
}

// TestCreditFloorTokenUnitNotBlockedWhenNonEmpty 核心判据：token 桶只要有余额就不该被
// 积分阈值拦下。credits=50 在积分口径下触底（50 < 150），在 token 口径下只是"还有 50 个
// token"，两个口径必须给出不同结论——这正是本次加固要修的点。
func TestCreditFloorTokenUnitNotBlockedWhenNonEmpty(t *testing.T) {
	p := floorFixture(t)
	paidBalance(t, p, "tok", CreditsUnitToken, 50)
	paidBalance(t, p, "crd", CreditsUnitCredit, 50)

	p.mu.RLock()
	now := time.Now()
	blockedTok := p.floorBlockedForRealmModel(p.byUID["tok"], "paid-model", "", now)
	blockedCrd := p.floorBlockedForRealmModel(p.byUID["crd"], "paid-model", "", now)
	p.mu.RUnlock()

	if blockedTok {
		t.Errorf("token 桶 credits=50 > 0 未触底，不得被积分阈值 150 拦截")
	}
	if !blockedCrd {
		t.Errorf("积分 credits=50 < floor=150 且模型收费，必须拦截（既有语义）")
	}
}

// TestCreditFloorTokenUnitBlocksWhenEmpty token 桶归零则拦——0 在任何单位下都是 0，
// 这条判据不依赖任何阈值标定，因此不会误伤。
func TestCreditFloorTokenUnitBlocksWhenEmpty(t *testing.T) {
	p := floorFixture(t)
	paidBalance(t, p, "tok", CreditsUnitToken, 0)

	p.mu.RLock()
	blocked := p.floorBlockedForRealmModel(p.byUID["tok"], "paid-model", "", time.Now())
	p.mu.RUnlock()
	if !blocked {
		t.Error("token 桶已空（0）且模型收费，必须拦截")
	}
}

// TestCreditFloorTokenUnitNumbersAreIrrelevant token 口径下余额的**数值大小**与 floor
// 无关（只与"是否为空"有关）：1 亿与 1 都不该被 150 这个积分阈值干预。
func TestCreditFloorTokenUnitNumbersAreIrrelevant(t *testing.T) {
	p := floorFixture(t)
	paidBalance(t, p, "big", CreditsUnitToken, 100_000_000)
	paidBalance(t, p, "small", CreditsUnitToken, 1)

	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	for _, uid := range []string{"big", "small"} {
		if p.floorBlockedForRealmModel(p.byUID[uid], "paid-model", "", now) {
			t.Errorf("%s：token 桶非空即不该按积分阈值 150 拦截", uid)
		}
	}
}

// TestCreditFloorZeroDisabledUnitAgnostic floor=0（关闭保底）时两种口径都不拦。
func TestCreditFloorZeroDisabledUnitAgnostic(t *testing.T) {
	p := New("")
	p.SetCreditFloor(0)
	p.Add(&auth.Auth{UID: "crd"})
	p.Add(&auth.Auth{UID: "tok"})
	p.NoteModelCost("crd", "paid-model", 1.0, 1000)
	p.NoteModelCost("tok", "paid-model", 1.0, 1000)
	p.SetCredits("crd", 0, 0)
	p.SetCreditsUnit("tok", CreditsUnitToken, 0, 0)

	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	if p.floorBlockedForRealmModel(p.byUID["crd"], "paid-model", "", now) {
		t.Error("floor=0 时不得拦截（积分口径）")
	}
	if p.floorBlockedForRealmModel(p.byUID["tok"], "paid-model", "", now) {
		t.Error("floor=0 时不得拦截（token 口径）")
	}
}

// TestSetCreditsKeepsUnit 关键回归：SetCredits / SetCreditsDetailed 是"口径不明"的
// 刷新路径（面板余额按钮、批量刷新），绝不能把已有的 token 口径重置成积分——
// 否则面板点一次「余额」，加固就被静默抹掉了。
func TestSetCreditsKeepsUnit(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "tok"})
	p.SetCreditsUnit("tok", CreditsUnitToken, 100_000_000, 100_000_000)

	p.SetCredits("tok", 12, 34)
	if got := unitOf(t, p, "tok"); got != CreditsUnitToken {
		t.Errorf("SetCredits 后口径 = %v, want Token（口径不明路径不得改动口径）", got)
	}
	if got := creditsOf(t, p, "tok"); got != 12 {
		t.Errorf("SetCredits 后 credits = %d, want 12", got)
	}

	now := time.Now()
	p.SetCreditsDetailed("tok", 56, 78, 9, now.Add(time.Hour), 9)
	if got := unitOf(t, p, "tok"); got != CreditsUnitToken {
		t.Errorf("SetCreditsDetailed 后口径 = %v, want Token", got)
	}
	if got := creditsOf(t, p, "tok"); got != 56 {
		t.Errorf("SetCreditsDetailed 后 credits = %d, want 56", got)
	}
}

// TestSetCreditsUnitDetailedUpdatesUnit 显式口径入口必须真的改口径（两条分支互换）。
func TestSetCreditsUnitDetailedUpdatesUnit(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "zc"})
	p.SetCredits("zc", 1, 1) // 默认 Credit
	if got := unitOf(t, p, "zc"); got != CreditsUnitCredit {
		t.Fatalf("初始口径 = %v, want Credit", got)
	}
	p.SetCreditsUnitDetailed("zc", CreditsUnitToken, 1_000_000, 2_000_000, 0, time.Time{}, 0)
	if got := unitOf(t, p, "zc"); got != CreditsUnitToken {
		t.Errorf("显式标注后口径 = %v, want Token", got)
	}
	p.SetCreditsUnitDetailed("zc", CreditsUnitCredit, 500, 600, 0, time.Time{}, 0)
	if got := unitOf(t, p, "zc"); got != CreditsUnitCredit {
		t.Errorf("套餐生效后口径应翻回 Credit，got %v", got)
	}
}

func unitOf(t *testing.T, p *Pool, uid string) CreditsUnit {
	t.Helper()
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		t.Fatalf("uid %s 不在池内", uid)
	}
	return e.creditsUnit
}

func creditsOf(t *testing.T, p *Pool, uid string) int64 {
	t.Helper()
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.byUID[uid].credits
}

// TestCreditsUnitPersistRoundTrip 口径必须跨重启存活：丢了它，重启后 token 桶会被
// 当成巨额积分参与保底（加固失效）。
func TestCreditsUnitPersistRoundTrip(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")

	src := New(fp)
	src.Add(&auth.Auth{UID: "tok"})
	src.Add(&auth.Auth{UID: "crd"})
	src.SetCreditsUnit("tok", CreditsUnitToken, 100_000_000, 100_000_000)
	src.SetCredits("crd", 700, 900)
	src.Flush()

	// token 口径必须落盘。落盘是缩进 JSON（字段后有空格）。
	// Credit 是零值 + omitempty 会被省略，这是有意的：既有 state.json 体积不变。
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if !strings.Contains(string(raw), `"credits_unit": 1`) {
		t.Fatalf("state.json 未记录 token 口径: %s", raw)
	}

	dst := New(fp)
	if got := unitOf(t, dst, "tok"); got != CreditsUnitToken {
		t.Errorf("重启后 tok 口径 = %v, want Token", got)
	}
	if got := unitOf(t, dst, "crd"); got != CreditsUnitCredit {
		t.Errorf("重启后 crd 口径 = %v, want Credit", got)
	}
	if got := creditsOf(t, dst, "tok"); got != 100_000_000 {
		t.Errorf("重启后 tok credits = %d, want 100000000", got)
	}
}

// TestCreditsUnitGarbageOnRestore 被改坏的 state.json（越界口径值）必须回落到 Credit，
// 不能出现"显示 credit、判定按 token"的错位。
func TestCreditsUnitGarbageOnRestore(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	legacy := `{"accounts":{"u1":{"credits":50,"credits_unit":7}}}`
	if err := os.WriteFile(fp, []byte(legacy), 0o600); err != nil {
		t.Fatalf("write state: %v", err)
	}
	p := New(fp)
	if got := unitOf(t, p, "u1"); got != CreditsUnitCredit {
		t.Fatalf("越界口径应回落 Credit，got %v", got)
	}
	// 与 String() 的分支保持一致（同一份真相）。
	p.mu.RLock()
	st := p.statusOf("u1", p.byUID["u1"])
	p.mu.RUnlock()
	if st.CreditsUnit != "credit" {
		t.Errorf("/status credits_unit = %q, want credit", st.CreditsUnit)
	}
}

// TestStatusExposesCreditsUnit 运维必须能从 /status 看出数字的口径——
// 这正是本次排障时缺失的那块信息（credits=0 到底是"没积分"还是"没 token"）。
func TestStatusExposesCreditsUnit(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "tok"})
	p.Add(&auth.Auth{UID: "crd"})
	p.SetCreditsUnit("tok", CreditsUnitToken, 100_000_000, 100_000_000)
	p.SetCredits("crd", 879, 1350)

	p.mu.RLock()
	defer p.mu.RUnlock()
	if got := p.statusOf("tok", p.byUID["tok"]).CreditsUnit; got != "token" {
		t.Errorf("token 号 credits_unit = %q, want token", got)
	}
	if got := p.statusOf("crd", p.byUID["crd"]).CreditsUnit; got != "credit" {
		t.Errorf("积分号 credits_unit = %q, want credit", got)
	}
}

// TestPeakCreditsSeparatesUnits 峰值按口径分桶求值。
func TestPeakCreditsSeparatesUnits(t *testing.T) {
	cands := []*entry{
		{credits: 10_000, creditsUnit: CreditsUnitCredit},
		{credits: 20_000, creditsUnit: CreditsUnitCredit},
		{credits: 100_000_000, creditsUnit: CreditsUnitToken},
		{credits: 300_000_000, creditsUnit: CreditsUnitToken},
	}
	peaks := peakCredits(cands)
	if peaks.credit != 20_000 {
		t.Errorf("credit 峰值 = %d, want 20000", peaks.credit)
	}
	if peaks.token != 300_000_000 {
		t.Errorf("token 峰值 = %d, want 300000000", peaks.token)
	}
	if got := peaks.of(cands[0]); got != 20_000 {
		t.Errorf("积分号的峰值基准 = %d, want 20000（不得被 token 桶抬高）", got)
	}
	if got := peaks.of(cands[2]); got != 300_000_000 {
		t.Errorf("token 号的峰值基准 = %d, want 300000000", got)
	}
}

// TestWeightNormalizesPerUnit 混口径池里的加权不得让小口径账号被压成陪跑。
//
// 修复前：maxCredits 取全集最大（3 亿 token），1 万积分号的 credits 项 ≈ 0.0003，
// 在抽签里彻底失去话语权；修复后两者各自归一，credits 项都是满值 10。
func TestWeightNormalizesPerUnit(t *testing.T) {
	p := New("")
	now := time.Now()
	crd := &entry{credits: 10_000, creditsUnit: CreditsUnitCredit}
	tok := &entry{credits: 300_000_000, creditsUnit: CreditsUnitToken}
	// lastUsed=now 让闲置因子归零，断言只反映 credits 项。
	crd.lastUsed, tok.lastUsed = now, now

	peaks := peakCredits([]*entry{crd, tok})
	wc := p.weightOf(crd, peaks, now)
	wt := p.weightOf(tok, peaks, now)

	// 1.0 基线 + 10.0（满额 credits 项）+ 0（闲置） = 11.0
	if !almostEqual(wc, 11.0) {
		t.Errorf("积分号权重 = %v, want 11（同口径归一应给满 credits 项）", wc)
	}
	if !almostEqual(wt, 11.0) {
		t.Errorf("token 号权重 = %v, want 11", wt)
	}
	if !almostEqual(wc, wt) {
		t.Errorf("同口径归一后两者权重应相等：积分=%v token=%v", wc, wt)
	}
}

// TestWeightUnchangedWhenUnitsUniform 零漂移保证：候选同口径时新实现与旧实现逐位相同。
// 旧实现 = credits / 全集最大 credits × 10。
func TestWeightUnchangedWhenUnitsUniform(t *testing.T) {
	p := New("")
	now := time.Now()
	cands := []*entry{
		{credits: 1000, lastUsed: now},
		{credits: 400, lastUsed: now},
		{credits: 0, lastUsed: now},
	}
	peaks := peakCredits(cands)
	for _, e := range cands {
		got := p.weightOf(e, peaks, now)
		want := 1.0 + float64(e.credits)/1000.0*10
		if !almostEqual(got, want) {
			t.Errorf("credits=%d 权重 = %v, want %v（同口径下必须与旧实现一致）", e.credits, got, want)
		}
	}
}

func almostEqual(a, b float64) bool {
	d := a - b
	return d < 1e-9 && d > -1e-9
}
