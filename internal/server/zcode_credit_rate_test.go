package server

import (
	"testing"
	"time"
)

// ZCode 逐次积分折算：用「当日真值 ÷ 当日网关 token」得到的实际单价分摊。
//
// 这些用例把三条硬约束钉死：
//  1. 真值未标定时绝不能编造数字（ok=false）；
//  2. 分母必须含本次请求（否则首笔永远算不出）；
//  3. 跨天必须重置（昨天的 rate 不能算今天的账）。
func TestZCodeCreditRateAnchorsToRealCredits(t *testing.T) {
	const uid = "rate-test-uid-1"
	resetZCodeStateForTest(uid)

	// 未标定：有 token 也没有 rate。
	if _, ok := estimateZCodeCredit(uid, "GLM-5.3", 1000); ok {
		t.Fatal("未标定时不得给出积分")
	}
	// 上游给出当日真值 12 积分。
	setZCodeRealCredits(uid, 12)
	// 此时分母=1000（上一次调用已累加），rate = 12/1000。
	cr, ok := estimateZCodeCredit(uid, "GLM-5.3", 2000)
	if !ok {
		t.Fatal("标定后应能折算")
	}
	// 本次前分母 1000，累加后 3000 → rate=12/3000=0.004 → 2000*0.004 = 8
	if cr < 7.99 || cr > 8.01 {
		t.Fatalf("credit = %v, want ~8（12 积分 / 3000 token × 2000）", cr)
	}
	// 再打一次，分母继续涨，rate 下降（同一笔真值被更多 token 摊），这是预期行为。
	cr2, ok := estimateZCodeCredit(uid, "GLM-5.3", 1000)
	if !ok {
		t.Fatal("第二次折算失败")
	}
	// 分母 4000 → rate=0.003 → 1000*0.003 = 3
	if cr2 < 2.99 || cr2 > 3.01 {
		t.Fatalf("credit = %v, want ~3", cr2)
	}
}

// token 不可用时（上游没给 usage）不得累加分母、也不得产出积分。
func TestZCodeCreditUsageNoTokensNoCredit(t *testing.T) {
	const uid = "rate-test-uid-2"
	resetZCodeStateForTest(uid)
	setZCodeRealCredits(uid, 5)
	u := zcodeCreditUsage(uid, "GLM-5.3", upstreamUsage{})
	if u.HasCreditComputed {
		t.Fatal("无 token 时不得产出积分")
	}
	// 有拆分的 prompt/completion 时 total 由 total() 合成，应能折算。
	u2 := zcodeCreditUsage(uid, "GLM-5.3", upstreamUsage{PromptTokens: 900, HasPrompt: true, Completion: 100, HasCompletion: true})
	if !u2.HasCreditComputed || u2.CreditComputed <= 0 {
		t.Fatalf("有 token 应折算出积分，得到 %+v", u2)
	}
}

// 跨天：昨天的真值与分母都必须清掉，不能拿旧 rate 算今天的账。
func TestZCodeCreditStateResetsAcrossDays(t *testing.T) {
	const uid = "rate-test-uid-3"
	resetZCodeStateForTest(uid)
	setZCodeRealCredits(uid, 100)
	noteZCodeTokens(uid, 1000)
	if _, ok := zcodeCreditRate(uid); !ok {
		t.Fatal("同日内应已标定")
	}
	// 手动把状态日期改成昨天，模拟跨天。
	st := zcodeState(uid)
	st.mu.Lock()
	st.day = time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	st.mu.Unlock()
	// 再取一次状态会触发重置：真值作废、分母归零。
	if _, ok := zcodeCreditRate(uid); ok {
		t.Fatal("跨天后旧 rate 必须作废")
	}
}

// ZCodeCreditRateFor 暴露给面板的折算率是 credits/1K token。
func TestZCodeCreditRateForPer1K(t *testing.T) {
	const uid = "rate-test-uid-4"
	resetZCodeStateForTest(uid)
	setZCodeRealCredits(uid, 2)
	noteZCodeTokens(uid, 1000) // rate = 2/1000 = 0.002 credits/token
	h := &Handler{}
	per1K, ok := h.ZCodeCreditRateFor(uid)
	if !ok {
		t.Fatal("应已标定")
	}
	if per1K < 1.99 || per1K > 2.01 {
		t.Fatalf("per1K = %v, want ~2 credits/1K", per1K)
	}
}

// resetZCodeStateForTest 清掉某账号的折算状态（用例间互不干扰）。
func resetZCodeStateForTest(uid string) { zcodeCreditStates.Delete(uid) }
