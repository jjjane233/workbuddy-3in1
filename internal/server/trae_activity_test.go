package server

import "testing"

// TraeActivityResult.Summary 是运维日志的唯一出口，必须把三类结果分开说清，
// 不能把「暂不可用」混进「领取成功」。
func TestTraeActivitySummary(t *testing.T) {
	empty := TraeActivityResult{}
	if empty.Summary() != "无可领取活动" {
		t.Fatalf("empty summary = %q", empty.Summary())
	}
	r := TraeActivityResult{
		Claimed: []string{"a"},
		Pending: []string{"b(9090)", "c(9090)"},
		Failed:  []string{"d: 4001"},
		Honored: "已发放",
	}
	got := r.Summary()
	for _, want := range []string{"领取成功 1", "暂不可用 2", "失败 1", "权益：已发放"} {
		if !contains(got, want) {
			t.Fatalf("summary=%q 缺少 %q", got, want)
		}
	}
}

// 巡检把 pending 和 failed 分开：9090/9070 不算失败，否则每轮日志一片红。
func TestIsPendingCodeServer(t *testing.T) {
	for _, c := range []int{9069, 9090, 9070} {
		if !isPendingCode(c) {
			t.Fatalf("%d 应为 pending", c)
		}
	}
	if isPendingCode(4001) || isPendingCode(0) {
		t.Fatal("4001/0 不应为 pending")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
