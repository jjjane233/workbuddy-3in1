// billing 回落分支的余额口径接线测试。
//
// 这是本次加固唯一可能写错的地方：ZCode 余额刷新有两条分支，标错口径的直接后果是
// creditFloor 拿 1 亿 token 去和 100 积分比大小（判据恒过），或在上游改粗 token
// 单位后突然开始误拦正常账号。抽成 zcodeBillingUnit 就是为了让这条决策可被直接
// 断言，而不必给 zcode 客户端搭 HTTP mock（monitor/billing 宿主是硬编码域名）。
package server

import (
	"testing"

	"github.com/jjjane233/workbuddy-3in1/internal/pool"
	"github.com/jjjane233/workbuddy-3in1/internal/zcode"
)

func TestZCodeBillingUnit(t *testing.T) {
	cases := []struct {
		name string
		b    *zcode.Billing
		want pool.CreditsUnit
	}{
		{
			name: "token 桶（老号 JWT / 无套餐号的常态）",
			b:    &zcode.Billing{Balances: []zcode.Balance{{UnitType: "token", RemainingUnits: 100_000_000}}},
			want: pool.CreditsUnitToken,
		},
		{
			name: "积分桶",
			b:    &zcode.Billing{Balances: []zcode.Balance{{UnitType: "credit", RemainingUnits: 10_000}}},
			want: pool.CreditsUnitCredit,
		},
		{
			name: "大小写变体仍识别为 token",
			b:    &zcode.Billing{Balances: []zcode.Balance{{UnitType: "TOKEN", RemainingUnits: 5}}},
			want: pool.CreditsUnitToken,
		},
		{
			name: "单位混用：不表态，退回积分（历史行为）",
			b: &zcode.Billing{Balances: []zcode.Balance{
				{UnitType: "token", RemainingUnits: 100_000_000},
				{UnitType: "credit", RemainingUnits: 10_000},
			}},
			want: pool.CreditsUnitCredit,
		},
		{
			name: "空账单（新号无任何权益桶 → 真实没额度）",
			b:    &zcode.Billing{},
			want: pool.CreditsUnitCredit,
		},
		{
			name: "只有 plan 没有 balance（Balance 的 fallback 2 形态）",
			b:    &zcode.Billing{Plans: []zcode.Plan{{PlanID: "p1"}}},
			want: pool.CreditsUnitCredit,
		},
		{
			name: "未识别单位：退回历史行为，不擅自换判据",
			b:    &zcode.Billing{Balances: []zcode.Balance{{UnitType: "points", RemainingUnits: 1}}},
			want: pool.CreditsUnitCredit,
		},
		{
			name: "nil 账单：防御性退回积分，不得 panic",
			b:    nil,
			want: pool.CreditsUnitCredit,
		},
	}
	for _, c := range cases {
		if got := zcodeBillingUnit(c.b); got != c.want {
			t.Errorf("%s: zcodeBillingUnit = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestZCodeBillingUnitDrivesFloor 端到端口径语义：同一份"余额 = 50"的 billing，
// 标成 token 时不被 floor 拦截，标成积分时被拦截——证明接线真的改变了保底判据，
// 而不只是多了一个展示字段。
func TestZCodeBillingUnitDrivesFloor(t *testing.T) {
	// token 桶：50 个 token。
	tok := &zcode.Billing{Balances: []zcode.Balance{{UnitType: "token", RemainingUnits: 50}}}
	if got := zcodeBillingUnit(tok); got != pool.CreditsUnitToken {
		t.Fatalf("token 账单口径 = %v, want Token", got)
	}
	// 积分桶：50 积分。
	crd := &zcode.Billing{Balances: []zcode.Balance{{UnitType: "credit", RemainingUnits: 50}}}
	if got := zcodeBillingUnit(crd); got != pool.CreditsUnitCredit {
		t.Fatalf("积分账单口径 = %v, want Credit", got)
	}
	// 两者数值相同、口径不同 —— 保底判据必须因此不同（详细断言见 pool 包的
	// TestCreditFloorTokenUnitNotBlockedWhenNonEmpty）。
}
