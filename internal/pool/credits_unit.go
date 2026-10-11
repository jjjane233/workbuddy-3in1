// credits_unit.go —— 余额口径（2026-10-12）。
//
// 背景：entry.credits 一个 int64 字段承载了两种**不可比**的量——
//
//	积分   CodeBuddy/Trae 的 UserResource、ZCode Coding Plan 的 monitor/quota/limit
//	       （"GLM Coding Lite = 每周 10000 积分"那个口径），量级 10^2~10^4；
//	token  ZCode billing.Balances[].remaining_units（unit_type="token"），量级 10^6~10^8。
//
// 在引入本标记之前，creditFloor（如 100）被无条件地与这个字段直接比较：
// ZCode 的 token 桶一度被当成"1 亿积分"参与判定，候选恒过阈值——判据形同虚设；
// 更危险的是反向：上游哪天把 token 单位改粗（千 token / 万 token），同一个阈值
// 会**突然开始拦截**原本正常的账号，而且没有任何日志能解释原因（静默失真）。
//
// 本文件把口径显式化：写入方声明口径，消费方（选号保底、加权路由）按口径分流。
//
// 零值必须是 CreditsUnitCredit ——这是**有意的默认**：历史 state.json 与所有未声明
// 口径的写入路径（CodeBuddy / Trae / ZCode Coding Plan，全部是积分）都落到它，
// 于是本次加固对既有部署是零行为漂移的（见 pick.go 的 creditPeaks 同理论证）。
package pool

import "strings"

// CreditsUnit 余额口径。零值必须是 CreditsUnitCredit（见包注释）。
type CreditsUnit uint8

const (
	// CreditsUnitCredit 平台积分：creditFloor 的数值阈值对此口径有意义。
	CreditsUnitCredit CreditsUnit = iota
	// CreditsUnitToken token 权益桶：与积分不可比，floor 只判"是否已空"。
	CreditsUnitToken
)

// String 返回口径名。/status 展示与 state.json 落盘共用它，口径名一处定义。
func (u CreditsUnit) String() string {
	switch u {
	case CreditsUnitToken:
		return "token"
	default:
		return "credit"
	}
}

// CreditsUnitFromUpstream 把上游 unit_type 映射为池内口径。
//
// 只认 "token" 是刻意的：一切未识别的形态（空串、多种单位混用、上游新增单位）
// 都退回 Credit，即**历史行为**。宁可让一个未知单位沿用旧的数值比较，也不要
// 在不掌握其量纲时擅自换用另一套判据——换判据的代价是静默拦截正常账号。
func CreditsUnitFromUpstream(s string) CreditsUnit {
	if strings.EqualFold(strings.TrimSpace(s), "token") {
		return CreditsUnitToken
	}
	return CreditsUnitCredit
}

// normalizeCreditsUnit 清洗持久化恢复的口径值：越界/未知一律回落到 Credit。
// 不这样做的话，一个被手工改坏的 state.json（如 "credits_unit": 7）会落进
// String() 的 default 分支显示 "credit"，却仍以 Token 分支参与保底判定——
// 展示与实际判据不一致是最难排查的一类问题。
func normalizeCreditsUnit(u CreditsUnit) CreditsUnit {
	if u == CreditsUnitToken {
		return CreditsUnitToken
	}
	return CreditsUnitCredit
}
