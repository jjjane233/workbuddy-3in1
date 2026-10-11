// 账单口径判定（Billing.SoleUnitType）的回归测试。
//
// 为什么需要它：池内 credits 一个字段承载积分与 token 两种不可比的量，回落分支
// （无 Coding Plan 的老号走 billing 权益桶）必须如实告诉池子"这个数字是什么口径"，
// 否则 creditFloor 会拿 1 亿 token 与 100 积分比大小。
package zcode

import "testing"

// TestSoleUnitTypeSingle 单一单位的账单直接给出该单位。
func TestSoleUnitTypeSingle(t *testing.T) {
	cases := []struct {
		name string
		b    *Billing
		want string
	}{
		{"token 桶", &Billing{Balances: []Balance{
			{UnitType: "token", RemainingUnits: 100_000_000},
		}}, "token"},
		{"积分桶", &Billing{Balances: []Balance{
			{UnitType: "credit", RemainingUnits: 10_000},
		}}, "credit"},
		{"多桶同单位", &Billing{Balances: []Balance{
			{UnitType: "token", RemainingUnits: 1},
			{UnitType: "token", RemainingUnits: 2},
		}}, "token"},
		{"大小写/空白归一", &Billing{Balances: []Balance{
			{UnitType: "  TOKEN  ", RemainingUnits: 5},
		}}, "TOKEN"}, // 原样返回，由 pool.CreditsUnitFromUpstream 做大小写归一
		{"未识别单位原样透出", &Billing{Balances: []Balance{
			{UnitType: "points", RemainingUnits: 5},
		}}, "points"},
	}
	for _, c := range cases {
		if got := c.b.SoleUnitType(); got != c.want {
			t.Errorf("%s: SoleUnitType() = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestSoleUnitTypeAbstainsOnMixed 多种单位混用时**不表态**（返回空串）。
//
// 这是刻意的：Remaining() 只是把各桶的 remaining_units 算术相加，混用时那个和
// 没有任何物理意义。此时若冒充某一种单位，等于把一个无意义的和当成权威余额
// 去参与保底判定。返回空串让调用方退回保守口径（积分 = 历史行为）。
func TestSoleUnitTypeAbstainsOnMixed(t *testing.T) {
	b := &Billing{Balances: []Balance{
		{UnitType: "token", RemainingUnits: 100_000_000, TotalUnits: 100_000_000},
		{UnitType: "credit", RemainingUnits: 10_000, TotalUnits: 10_000},
	}}
	if got := b.SoleUnitType(); got != "" {
		t.Errorf("混用单位应不表态，got %q", got)
	}
	// 顺带钉死 Remaining() 的"无条件求和"事实——它正是混用时不能表态的原因。
	remain, total, _ := b.Remaining()
	if remain != 100_010_000 || total != 100_010_000 {
		t.Errorf("Remaining() = %d/%d, want 100010000/100010000", remain, total)
	}
}

// TestSoleUnitTypeEmpty 空账单（新号无任何权益桶）不表态。
// 对应线上现象：新号 billing 返回 balances=[]，池内 credits=0 —— 是真实的"没额度"，
// 不是读取失败。
func TestSoleUnitTypeEmpty(t *testing.T) {
	if got := (&Billing{}).SoleUnitType(); got != "" {
		t.Errorf("空账单 SoleUnitType() = %q, want 空串", got)
	}
	if got := (&Billing{Balances: []Balance{}}).SoleUnitType(); got != "" {
		t.Errorf("空桶列表 SoleUnitType() = %q, want 空串", got)
	}
	// 只有 plan、没有 balance（Balance() 的 fallback 2 形态）：同样无口径可标。
	if got := (&Billing{Plans: []Plan{{PlanID: "p1"}}}).SoleUnitType(); got != "" {
		t.Errorf("仅 plan 时 SoleUnitType() = %q, want 空串", got)
	}
}

// TestSoleUnitTypeSkipsBlankUnitType 单位字段为空的桶被跳过，不参与"是否混用"的判断。
func TestSoleUnitTypeSkipsBlankUnitType(t *testing.T) {
	b := &Billing{Balances: []Balance{
		{UnitType: "", RemainingUnits: 7},
		{UnitType: "token", RemainingUnits: 8},
	}}
	if got := b.SoleUnitType(); got != "token" {
		t.Errorf("空单位桶应被跳过，got %q", got)
	}
	// 全部为空 → 不表态。
	allBlank := &Billing{Balances: []Balance{{UnitType: ""}, {UnitType: "  "}}}
	if got := allBlank.SoleUnitType(); got != "" {
		t.Errorf("全部空单位应不表态，got %q", got)
	}
}
