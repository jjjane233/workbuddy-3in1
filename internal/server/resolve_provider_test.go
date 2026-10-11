package server

import "testing"

// TestResolveRoute 锁住三合一网关的路由判据。
// 重点有二：
//  1. 存量 CodeBuddy 调用（无 provider 信息）必须仍归 workbuddy —— 零回归；
//  2. provider 只认显式前缀与 X-Provider 头，**不做模型名特征推断**——
//     CodeBuddy 链路本身就用 glm-5.2/glm-5.3，推断会劫持存量流量。
func TestResolveRoute(t *testing.T) {
	cases := []struct {
		name   string
		model  string
		header string
		prov   string
		realm  string
		bare   string
	}{
		// ---- 存量行为：必须保持不变 ----
		{"裸名回落 workbuddy", "claude-3-5-sonnet", "", providerWorkbuddy, "cn", "claude-3-5-sonnet"},
		{"realm 前缀原样保留", "cn:gpt-4o", "", providerWorkbuddy, "cn", "gpt-4o"},
		{"global realm 前缀", "global:claude-opus", "", providerWorkbuddy, "global", "claude-opus"},
		{"未知模型默认 workbuddy 而非 zcode", "totally-unknown-model", "", providerWorkbuddy, "cn", "totally-unknown-model"},

		// ---- 显式 provider 前缀 ----
		{"trae 前缀", "trae/trae-auto", "", providerTrae, "cn", "trae-auto"},
		{"zcode 前缀", "zcode/glm-4.6", "", providerZCode, "cn", "glm-4.6"},
		{"workbuddy 显式前缀", "workbuddy/claude-3-5", "", providerWorkbuddy, "cn", "claude-3-5"},
		{"别名 traecode", "traecode/trae-solo", "", providerTrae, "cn", "trae-solo"},
		{"别名 zai", "zai/glm-4", "", providerZCode, "cn", "glm-4"},
		{"别名 bigmodel", "bigmodel/glm-4-air", "", providerZCode, "cn", "glm-4-air"},

		// ---- 前缀 + realm 组合 ----
		{"provider 与 realm 叠加", "trae/cn:claude", "", providerTrae, "cn", "claude"},

		// ---- X-Provider 头 ----
		{"头指定 trae", "whatever-model", "trae", providerTrae, "cn", "whatever-model"},
		{"头指定 zcode 别名", "whatever-model", "zai", providerZCode, "cn", "whatever-model"},
		{"头非法时忽略", "whatever-model", "bogus", providerWorkbuddy, "cn", "whatever-model"},

		// ---- 【无模型名推断】裸模型名一律归 workbuddy ----
		// 这一组是刻意的行为锁：CodeBuddy 链路实际使用 glm-5.2/glm-5.3 作为模型名
		// （handler_test.go 全篇），任何 glm*/trae* 特征推断都会劫持存量真实流量。
		{"裸 glm 归 workbuddy", "glm-4.6", "", providerWorkbuddy, "cn", "glm-4.6"},
		{"裸 GLM 归 workbuddy", "GLM-5.3", "", providerWorkbuddy, "cn", "GLM-5.3"},
		{"裸 trae-agent 归 workbuddy", "trae-agent", "", providerWorkbuddy, "cn", "trae-agent"},
		{"裸 solo 归 workbuddy", "solo", "", providerWorkbuddy, "cn", "solo"},
		{"存量 CodeBuddy 模型名不受影响", "glm-5.2", "", providerWorkbuddy, "cn", "glm-5.2"},

		// ---- 优先级：显式前缀 > 头 > 兜底 ----
		{"前缀压过头", "zcode/glm-x", "trae", providerZCode, "cn", "glm-x"},
		{"头压过裸模型名", "whatever-glm", "trae", providerTrae, "cn", "whatever-glm"},

		// ---- 显式前缀下裸名保留（选号用裸名）----
		{"前缀剥离后裸名可与存量同名", "zcode/glm-5.2", "", providerZCode, "cn", "glm-5.2"},

		// ---- 冒号式 provider 前缀（/v1/models 统一目录的 id 形态）----
		// 统一目录里 CodeBuddy 条目是 "cn:glm-5.2"（realm 前缀），非 CodeBuddy 条目
		// 用 "provider:"（zcode:glm-5.3-flash）。两种分隔符都认，客户端拿到什么填什么。
		{"冒号式 zcode 前缀", "zcode:glm-5.3-flash", "", providerZCode, "cn", "glm-5.3-flash"},
		{"冒号式 trae 前缀", "trae:glm-5.3-flash", "", providerTrae, "cn", "glm-5.3-flash"},
		{"冒号式 workbuddy 前缀", "workbuddy:claude-3-5", "", providerWorkbuddy, "cn", "claude-3-5"},
		{"冒号式别名 zai", "zai:glm-4", "", providerZCode, "cn", "glm-4"},
		{"冒号式别名 bigmodel", "bigmodel:glm-4-air", "", providerZCode, "cn", "glm-4-air"},
		{"冒号式别名 traecode", "traecode:trae-solo", "", providerTrae, "cn", "trae-solo"},
		// realm 不是 provider：cn:/global: 必须原样交给 resolveModel
		{"冒号 realm cn 不被当 provider", "cn:glm-5.2", "", providerWorkbuddy, "cn", "glm-5.2"},
		{"冒号 realm global 不被当 provider", "global:claude", "", providerWorkbuddy, "global", "claude"},
		// 冒号式前缀 + realm 共存
		{"冒号 provider + realm", "zcode:global:glm-x", "", providerZCode, "global", "glm-x"},

		// ---- 误伤防护：非枚举的 "/" 不剥 ----
		{"组织名斜杠不误伤", "org/model-name", "", providerWorkbuddy, "cn", "org/model-name"},
		{"前导斜杠不误伤", "/leading", "", providerWorkbuddy, "cn", "/leading"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			prov, realm, bare := resolveRoute(c.model, c.header)
			if prov != c.prov {
				t.Errorf("provider = %q, want %q", prov, c.prov)
			}
			if realm != c.realm {
				t.Errorf("realm = %q, want %q", realm, c.realm)
			}
			if bare != c.bare {
				t.Errorf("bare = %q, want %q", bare, c.bare)
			}
		})
	}
}

// TestResolveRouteBackwardCompat 专项回归：只喂存量协议形态，
// 断言一律落到 workbuddy，确保三合一不会把老调用方改道。
func TestResolveRouteBackwardCompat(t *testing.T) {
	legacy := []string{
		"claude-3-5-sonnet",
		"cn:claude-3-5-sonnet",
		"global:claude-3-5-sonnet",
		"gpt-4o",
		"cn:gpt-4o",
	}
	for _, m := range legacy {
		prov, _, _ := resolveRoute(m, "")
		if prov != providerWorkbuddy {
			t.Errorf("resolveRoute(%q) provider = %q, want %q（存量零回归）", m, prov, providerWorkbuddy)
		}
	}
}
