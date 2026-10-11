package zcode

import (
	"encoding/json"
	"testing"
)

// entitledFixture 是 2026-10-06 从线上账号真实抓到的 billing/balance 响应
// （只删了 logid / bucket_id / user_plan_id 这类无意义标识）。
//
// 关键事实（这组测试就是用来钉死它们的）：
//   - 1 亿额度是 token 计量，capabilities 只有 model:glm-5.3-flash
//   - expires_at=1791302400 = 2026-10-07 00:00:00 UTC+8，当天 24:00 就过期
//   - 公共模型目录（client/configs）里根本没有 glm-5.3-flash
//
// 谁再顺手改坏 EntitledModels，这组用例会立刻红。
const entitledFixture = `{
  "plans":[{"plan_id":"zcode-v3-start-plan-trust-1006","name":"ZCode Trust Build",
    "status":"active","starts_at":1791251818,"ends_at":1791302400,
    "entitlements":[{"entitlement_id":"zcode-v3-start-plan-trust-1006",
      "show_name":"GLM-5.3-Flash","meter":"model_usage","unit_type":"token",
      "capabilities":["model:glm-5.3-flash"],"grant_units":100000000,
      "period":"one_time","priority":110,"effective_at":0}]}],
  "balances":[{"show_name":"GLM-5.3-Flash","meter":"model_usage","unit_type":"token",
    "capabilities":["model:glm-5.3-flash"],"plan_id":"zcode-v3-start-plan-trust-1006",
    "total_units":100000000,"used_units":0,"remaining_units":100000000,
    "available_units":100000000,"period_start":1791251818,"period_end":1791302400,
    "expires_at":1791302400}]}`

func parseFixture(t *testing.T) *Billing {
	t.Helper()
	var b Billing
	if err := json.Unmarshal([]byte(entitledFixture), &b); err != nil {
		t.Fatalf("fixture unmarshal: %v", err)
	}
	return &b
}

func TestBalanceParsesCapabilitiesAndExpiry(t *testing.T) {
	b := parseFixture(t)
	if len(b.Balances) != 1 {
		t.Fatalf("balances = %d, want 1", len(b.Balances))
	}
	x := b.Balances[0]
	if x.UnitType != "token" {
		t.Fatalf("unit_type = %q, want token", x.UnitType)
	}
	if x.ExpiresAt != 1791302400 {
		t.Fatalf("expires_at = %d, want 1791302400", x.ExpiresAt)
	}
	if len(x.Capabilities) != 1 || x.Capabilities[0] != "model:glm-5.3-flash" {
		t.Fatalf("capabilities = %v", x.Capabilities)
	}
	if x.AvailableUnits != 100000000 {
		t.Fatalf("available_units = %d", x.AvailableUnits)
	}
}

func TestEntitledModelsStripsModelPrefix(t *testing.T) {
	b := parseFixture(t)
	ms := b.EntitledModels()
	if len(ms) != 1 {
		t.Fatalf("entitled models = %d (%v), want 1", len(ms), ms)
	}
	if ms[0].Model != "glm-5.3-flash" {
		t.Fatalf("model = %q, want glm-5.3-flash（必须剥掉 model: 前缀）", ms[0].Model)
	}
	if ms[0].Total != 100000000 || ms[0].Remain != 100000000 {
		t.Fatalf("total/remain = %d/%d", ms[0].Total, ms[0].Remain)
	}
	if ms[0].ExpiresAt != 1791302400 {
		t.Fatalf("expires = %d", ms[0].ExpiresAt)
	}
}

// TestEntitledModelsDedupesPlanAndBalance 同一个模型在 plan 与 balance 里各出现一次，
// 结果必须只有一条，且带上 balance 的额度信息。
func TestEntitledModelsDedupesPlanAndBalance(t *testing.T) {
	b := parseFixture(t)
	ms := b.EntitledModels()
	if len(ms) != 1 {
		t.Fatalf("expected dedupe to 1, got %d: %v", len(ms), ms)
	}
	if ms[0].Remain != 100000000 {
		t.Fatalf("dedupe 丢了额度: remain=%d", ms[0].Remain)
	}
}

func TestModelFromCapability(t *testing.T) {
	cases := map[string]string{
		"model:glm-5.3-flash": "glm-5.3-flash",
		"model:GLM-5.2":       "GLM-5.2",
		"glm-5-turbo":         "glm-5-turbo", // 裸模型名兼容
		"":                    "",
	}
	for in, want := range cases {
		if got := modelFromCapability(in); got != want {
			t.Fatalf("modelFromCapability(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestIsBlocked 钉死 3012 的识别：这就是用户看到 405 的真实原因。
func TestIsBlocked(t *testing.T) {
	yes := []string{
		`zcode http 405: {"code":3012,"msg":"request has been blocked due to unusual activity."}`,
		"code=3012",
		`{"msg":"request has been blocked"}`,
	}
	for _, s := range yes {
		if !IsBlocked(s) {
			t.Fatalf("IsBlocked(%q) = false, want true", s)
		}
	}
	no := []string{"", "http 400 parameter error", "no available account"}
	for _, s := range no {
		if IsBlocked(s) {
			t.Fatalf("IsBlocked(%q) = true, want false", s)
		}
	}
}
