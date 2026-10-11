package server

import (
	"encoding/json"
	"testing"
)

// TestBuildSoloPayloadMatchesDLLContract 验证 DLL 链的字段和 content 块格式
// （与 traecode2api 的同名用例同口径：字段集必须逐一对齐）。
func TestBuildSoloPayloadMatchesDLLContract(t *testing.T) {
	req := &OAIRequest{Messages: []OAIMessage{{Role: "user", Content: json.RawMessage(`"hello"`)}}, Stream: true}
	var got map[string]any
	if err := json.Unmarshal(buildSoloPayload(req, "glm-5.3-flashx"), &got); err != nil {
		t.Fatal(err)
	}
	if got["model"] != "glm-5.3-flashx" || got["config_name"] != "glm-5.3-flashx" || got["function"] != "solo_work_lite" {
		t.Fatalf("unexpected routing fields: %#v", got)
	}
	msgs, ok := got["messages"].([]any)
	if !ok || len(msgs) != 1 {
		t.Fatalf("messages = %#v", got["messages"])
	}
	first := msgs[0].(map[string]any)
	blocks := first["content"].([]any)
	if blocks[0].(map[string]any)["text"] != "hello" {
		t.Fatalf("content block = %#v", blocks[0])
	}
}

// TestBuildSoloPayloadPreservesExplicitModel 显式模型名必须原样透传（不降级 seed_m8）。
func TestBuildSoloPayloadPreservesExplicitModel(t *testing.T) {
	req := &OAIRequest{Messages: []OAIMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}}, ModelName: "step-5-preview"}
	var got map[string]any
	if err := json.Unmarshal(buildSoloPayload(req, "glm-5.3-flash"), &got); err != nil {
		t.Fatal(err)
	}
	if got["model"] != "step-5-preview" || got["config_name"] != "step-5-preview" {
		t.Fatalf("model=%v config_name=%v, want step-5-preview", got["model"], got["config_name"])
	}
}

// TestUseSoloModelChain 别名留在旧 /chat 链，显式 DLL 模型走 llm_utils_chat。
func TestUseSoloModelChain(t *testing.T) {
	for _, m := range []string{"", "seed_m8", "auto", "solo", "agent", "trae-auto", "trae-agent"} {
		if useSoloModelChain(m) {
			t.Errorf("%q should stay on /chat chain", m)
		}
	}
	for _, m := range []string{"glm-5.3-flashx", "glm-5.3-flash", "deepseek-v4.1-flash", "step-5-preview", "kimi-k3", "qwen3.8-max", "doubao-seed-code"} {
		if !useSoloModelChain(m) {
			t.Errorf("%q should use llm_utils_chat", m)
		}
	}
}

// TestTraeErrorClassification 上游错误 → 稳定的 [OI] 错误类别。
func TestTraeErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		msg string
		fn  func(string) bool
	}{
		{"trae solo chat http 400: code 4023 model is unknown", isTraeModelNotFound},
		{"trae upstream error 4011", isTraeEntitlementError},
		{"config lookup failed, code 4001", isTraeEntitlementError},
	} {
		if !tc.fn(tc.msg) {
			t.Fatalf("classification failed for %q", tc.msg)
		}
	}
	if isTraeModelNotFound("ordinary timeout") || isTraeEntitlementError("ordinary timeout") {
		t.Fatal("ordinary errors must not be classified as model or entitlement failures")
	}
}
