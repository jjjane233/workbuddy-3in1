package panel

import (
	"testing"

	"github.com/jjjane233/workbuddy-3in1/internal/auth"
	"github.com/jjjane233/workbuddy-3in1/internal/pool"
)

// 三合一：积分必须按上游分开聚合——三个上游的额度口径不同，合并求和没有意义。
func TestProviderCreditsSplitsByProvider(t *testing.T) {
	accts := []pool.Status{
		{UID: "w1", Provider: auth.ProviderWorkbuddy, Credits: 120, CreditsTotal: 500},
		{UID: "w2", Provider: "", Credits: 30}, // 空 provider 归一为 workbuddy
		{UID: "t1", Provider: auth.ProviderTrae, Credits: 7, CreditsTotal: 100, CreditsExpiring: 2},
		{UID: "z1", Provider: auth.ProviderZCode, Credits: 100000000, CreditsTotal: 100000000},
	}
	got := providerCredits(accts)

	if c := got[auth.ProviderWorkbuddy]; c["credits"] != 150 || c["total"] != 500 || c["accounts"] != 2 {
		t.Fatalf("workbuddy = %v, want credits=150 total=500 accounts=2", c)
	}
	if c := got[auth.ProviderTrae]; c["credits"] != 7 || c["total"] != 100 || c["expiring"] != 2 || c["accounts"] != 1 {
		t.Fatalf("trae = %v, want credits=7 total=100 expiring=2 accounts=1", c)
	}
	if c := got[auth.ProviderZCode]; c["credits"] != 100000000 || c["total"] != 100000000 || c["accounts"] != 1 {
		t.Fatalf("zcode = %v", c)
	}
	// 恒含三个 key：前端卡片依赖这一点（缺失会渲染成 undefined）。
	for _, k := range []string{auth.ProviderWorkbuddy, auth.ProviderTrae, auth.ProviderZCode} {
		if _, ok := got[k]; !ok {
			t.Fatalf("missing key %q", k)
		}
	}
	// 不能跨上游串味：trae 总额不得被 workbuddy 的 500 污染。
	if got[auth.ProviderTrae]["total"] == 500 {
		t.Fatal("trae total leaked from workbuddy")
	}
}

// 无账号时三个 key 仍存在且全零，避免前端卡片空白。
func TestProviderCreditsEmpty(t *testing.T) {
	got := providerCredits(nil)
	for _, k := range []string{auth.ProviderWorkbuddy, auth.ProviderTrae, auth.ProviderZCode} {
		c, ok := got[k]
		if !ok {
			t.Fatalf("missing key %q", k)
		}
		if c["credits"] != 0 || c["total"] != 0 || c["accounts"] != 0 {
			t.Fatalf("%s = %v, want all zero", k, c)
		}
	}
}

// 未知 provider 不丢数据（未来新增上游时仍能被统计）。
func TestProviderCreditsUnknownProvider(t *testing.T) {
	got := providerCredits([]pool.Status{{UID: "x", Provider: "future", Credits: 9, CreditsTotal: 10}})
	c, ok := got["future"]
	if !ok || c["credits"] != 9 || c["accounts"] != 1 {
		t.Fatalf("unknown provider dropped: %v", got)
	}
}
