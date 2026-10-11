package server

import (
	"encoding/json"
	"testing"
)

// ZCode 思考档位的载体是 output_config.effort，**不是** thinking.type。
//
// 依据（2026-10-08 实测，open.bigmodel.cn/api/anthropic/v1/messages，GLM-5.3）：
//
//	thinking:{type:"low"|"high"|"max"|"disabled"} → 400 code 1210
//	thinking:{type:"enabled"}                     → 200
//	output_config:{effort:"low"}  → thinking 0 字符 / out 127 tok
//	output_config:{effort:"high"} → thinking 71 字符 / out 140 tok
//	output_config:{effort:"max"}  → thinking 1941 字符 / out 699 tok
//	output_config:{effort:"bogus"} → 400「reasoning_effort 参数值非法」
//
// 所以这里既锁住映射表，也锁住「档位不得写进 thinking.type」这条回归。
func TestZCodeEffortFieldMapping(t *testing.T) {
	cases := []struct {
		lvl    string
		effort string
	}{
		{"low", "low"},
		{"light", "low"},
		{"minimal", "low"},
		{"medium", "medium"},
		{"high", "high"},
		{"xhigh", "xhigh"},
		{"extra_high", "xhigh"},
		{"max", "xhigh"},
		{"ultra", "xhigh"},
		{"none", "none"},
		{"off", "none"},
		{"disabled", "none"},
	}
	for _, c := range cases {
		req := &OAIRequest{ReasoningEffort: c.lvl}
		raw := zcodeEffortField(req)
		if len(raw) == 0 {
			t.Fatalf("effort %q: 期望有 output_config，实际为空", c.lvl)
		}
		var got struct {
			Effort string `json:"effort"`
		}
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("effort %q: 解析失败 %v", c.lvl, err)
		}
		if got.Effort != c.effort {
			t.Fatalf("effort %q: 期望 %q，实际 %q", c.lvl, c.effort, got.Effort)
		}
	}
}

// 未知档位不猜测：上游对非法 reasoning_effort 直接 400，网关宁可不下发。
func TestZCodeEffortFieldUnknownIsNil(t *testing.T) {
	for _, lvl := range []string{"bogus", "??", "very-high"} {
		if raw := zcodeEffortField(&OAIRequest{ReasoningEffort: lvl}); len(raw) != 0 {
			t.Fatalf("未知档位 %q 不应产生 output_config，实际 %s", lvl, raw)
		}
	}
	if raw := zcodeEffortField(&OAIRequest{}); len(raw) != 0 {
		t.Fatalf("未指定档位不应产生 output_config，实际 %s", raw)
	}
}

// 客户端自己带了 output_config 就原样透传（网关不越权改写）。
func TestZCodeEffortFieldPassthrough(t *testing.T) {
	req := &OAIRequest{
		ReasoningEffort: "low",
		OutputConfig:    json.RawMessage(`{"effort":"max","verbosity":"high"}`),
	}
	raw := zcodeEffortField(req)
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got["effort"] != "max" {
		t.Fatalf("应原样透传 effort=max，实际 %v", got["effort"])
	}
	if got["verbosity"] != "high" {
		t.Fatalf("透传时不应丢字段，实际 %v", got)
	}
}

// thinking 只做原生透传：档位**不得**被写进 thinking.type（那会 400 code 1210）。
func TestZCodeThinkingFieldNeverCarriesEffort(t *testing.T) {
	req := &OAIRequest{ReasoningEffort: "high"}
	if raw := zcodeThinkingField(req); len(raw) != 0 {
		t.Fatalf("档位不应进 thinking，实际 %s", raw)
	}
	native := json.RawMessage(`{"type":"enabled","budget_tokens":8192}`)
	req2 := &OAIRequest{Thinking: native, ReasoningEffort: "low"}
	if string(zcodeThinkingField(req2)) != string(native) {
		t.Fatalf("原生 thinking 应原样透传，实际 %s", zcodeThinkingField(req2))
	}
}

// 端到端：oaiToZCodeRequest 同时把 output_config 与（可选）thinking 落到请求上。
func TestOaiToZCodeRequestCarriesEffort(t *testing.T) {
	req := &OAIRequest{
		Model:           "zcode:GLM-5.3",
		Messages:        []OAIMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		ReasoningEffort: "xhigh",
	}
	out := oaiToZCodeRequest(req, "GLM-5.3")
	if len(out.OutputConfig) == 0 {
		t.Fatal("output_config 未落到 ChatRequest")
	}
	var oc struct {
		Effort string `json:"effort"`
	}
	if json.Unmarshal(out.OutputConfig, &oc) != nil || oc.Effort != "xhigh" {
		t.Fatalf("output_config = %s", out.OutputConfig)
	}
	if len(out.Thinking) != 0 {
		t.Fatalf("未给原生 thinking 时不应注入 thinking，实际 %s", out.Thinking)
	}
	// 序列化后确认字段名就是上游认的 output_config.effort。
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if json.Unmarshal(raw, &wire) != nil {
		t.Fatal("wire unmarshal")
	}
	ocw, ok := wire["output_config"].(map[string]any)
	if !ok || ocw["effort"] != "xhigh" {
		t.Fatalf("wire output_config = %v", wire["output_config"])
	}
	if _, has := wire["thinking"]; has {
		t.Fatalf("wire 不应出现 thinking：%v", wire["thinking"])
	}
}

// ZCode 布尔思考开关（enable_thinking）→ Anthropic thinking.type 的分流。
//
// 依据（2026-10-08 实测）：
//
//	GLM-5.2 / GLM-5-Turbo / glm-4.7   thinking:{type:"enabled"} 有效、disabled 可关
//	GLM-5.3 系列                       thinking:{type:"disabled"} → 400 code 1210
//
// 因此 enable_thinking=false 对"始终思考"的模型必须被忽略（不能注入 disabled，
// 否则 400）；enable_thinking=true 对所有模型都注入 enabled。
func TestZCodeToggleFieldPerModel(t *testing.T) {
	tru := true
	fal := false

	// true → 一律 enabled（任何模型都合法）。
	for _, m := range []string{"GLM-5.3", "GLM-5.2", "glm-4.7", "GLM-5-Turbo"} {
		raw := zcodeToggleField(&OAIRequest{EnableThinking: &tru}, m)
		if len(raw) == 0 {
			t.Fatalf("%s: enable_thinking=true 应注入 thinking", m)
		}
		var got struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(raw, &got)
		if got.Type != "enabled" {
			t.Fatalf("%s: type=%q want enabled", m, got.Type)
		}
	}

	// false + 始终思考的模型 → 不注入（否则上游 400 code 1210）。
	if raw := zcodeToggleField(&OAIRequest{EnableThinking: &fal}, "GLM-5.3"); len(raw) != 0 {
		t.Fatalf("GLM-5.3 始终思考，enable_thinking=false 必须被忽略，实际注入了 %s", raw)
	}

	// false + 可开关的模型 → 注入 disabled。
	raw := zcodeToggleField(&OAIRequest{EnableThinking: &fal}, "GLM-5.2")
	if len(raw) == 0 {
		t.Fatal("GLM-5.2 可关思考，应注入 thinking.disabled")
	}
	var got struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(raw, &got)
	if got.Type != "disabled" {
		t.Fatalf("type=%q want disabled", got.Type)
	}

	// 没传 → 不注入（保持上游默认）。
	if raw := zcodeToggleField(&OAIRequest{}, "GLM-5.3"); len(raw) != 0 {
		t.Fatal("未指定开关时不得注入 thinking")
	}
}

// 端到端：enable_thinking 必须落到 oaiToZCodeRequest 的 Thinking 字段上。
func TestOaiToZCodeRequestHonoursEnableThinking(t *testing.T) {
	tru := true
	req := &OAIRequest{Model: "zcode:GLM-5.2", Messages: nil, EnableThinking: &tru}
	creq := oaiToZCodeRequest(req, "GLM-5.2")
	if len(creq.Thinking) == 0 {
		t.Fatal("enable_thinking=true 未透传到 thinking 字段")
	}
	var got struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(creq.Thinking, &got)
	if got.Type != "enabled" {
		t.Fatalf("thinking.type=%q want enabled", got.Type)
	}
}
