package server

import (
	"encoding/json"
	"strings"
	"testing"
)

// SOLO 链（llm_utils_chat）工具调用的回归测试。
//
// 事故形态（2026-10-12）：buildSoloPayload 把 OpenAI tools 原样透传给上游，而上游把
// function.parameters 声明为 string，于是任何带工具的标准 OpenAI 请求都被 400 拒绝，
// 并被网关误包成 403 model_not_entitled：
//
//	json: cannot unmarshal object into Go struct field
//	FunctionDefinition.tools.function.parameters of type string
//
// 修复后与 /chat 链同口径：提示词注入 + <|FunctionCallBegin|> 内联标记，
// 响应侧由两条链共用的 traeFnParser 翻译回 tool_calls。

// soloTools 是标准的 OpenAI 工具定义（parameters 为对象——正是触发上游 400 的形态）。
const soloTools = `"tools":[{"type":"function","function":{"name":"get_weather",` +
	`"description":"查天气","parameters":{"type":"object",` +
	`"properties":{"city":{"type":"string"}},"required":["city"]}}}]`

func buildSoloFromJSON(t *testing.T, reqJSON string) map[string]any {
	t.Helper()
	var req OAIRequest
	if err := json.Unmarshal([]byte(reqJSON), &req); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(buildSoloPayload(&req, "glm-5.3-flash"), &got); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	return got
}

// soloFirstText 取第 i 条消息 content 里所有 text 块拼起来的正文。
func soloFirstText(t *testing.T, got map[string]any, i int) string {
	t.Helper()
	msgs, _ := got["messages"].([]any)
	if i >= len(msgs) {
		t.Fatalf("messages[%d] 不存在（共 %d 条）", i, len(msgs))
	}
	blocks, _ := msgs[i].(map[string]any)["content"].([]any)
	var b strings.Builder
	for _, blk := range blocks {
		if m, ok := blk.(map[string]any); ok {
			if s, ok := m["text"].(string); ok {
				b.WriteString(s)
			}
		}
	}
	return b.String()
}

// TestBuildSoloPayloadInjectsToolPrompt 工具定义必须注入提示词，且不得再透传 tools。
func TestBuildSoloPayloadInjectsToolPrompt(t *testing.T) {
	got := buildSoloFromJSON(t, `{"messages":[{"role":"user","content":"weather in Tokyo?"}],`+soloTools+`}`)

	// 核心：上游 tools schema 不兼容，透传即 400。
	if _, has := got["tools"]; has {
		t.Fatal("SOLO 链不得透传 tools（上游 parameters 为 string，传对象会被 400 拒绝）")
	}
	if _, has := got["tool_choice"]; has {
		t.Fatal("SOLO 链不得透传 tool_choice")
	}

	txt := soloFirstText(t, got, 0)
	for _, want := range []string{"get_weather", "查天气", traeFnBegin, traeFnEnd, "weather in Tokyo?"} {
		if !strings.Contains(txt, want) {
			t.Errorf("注入的工具说明缺少 %q：\n%s", want, txt)
		}
	}
}

// TestBuildSoloPayloadToolChoiceNoneSkipsPrompt tool_choice=none 时不得注入工具说明，
// 但正文本身照常下发。
func TestBuildSoloPayloadToolChoiceNoneSkipsPrompt(t *testing.T) {
	got := buildSoloFromJSON(t, `{"messages":[{"role":"user","content":"hi"}],`+soloTools+`,"tool_choice":"none"}`)
	txt := soloFirstText(t, got, 0)
	if strings.Contains(txt, traeFnBegin) || strings.Contains(txt, "get_weather") {
		t.Fatalf("tool_choice=none 不得注入工具说明：\n%s", txt)
	}
	if txt != "hi" {
		t.Fatalf("正文应原样保留，got %q", txt)
	}
}

// TestBuildSoloPayloadRendersToolHistory 多轮工具链：assistant 的 tool_calls 还原成
// 调用标记，role:"tool" 的结果包成结果标记并归入 user，且不再下发原始工具字段。
func TestBuildSoloPayloadRendersToolHistory(t *testing.T) {
	got := buildSoloFromJSON(t, `{"messages":[`+
		`{"role":"user","content":"weather?"},`+
		`{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function",`+
		`"function":{"name":"get_weather","arguments":"{\"city\":\"Tokyo\"}"}}]},`+
		`{"role":"tool","tool_call_id":"call_1","content":"sunny, 25C"}`+
		`],`+soloTools+`}`)

	msgs, _ := got["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages 条数 = %d, want 3", len(msgs))
	}

	asst := msgs[1].(map[string]any)
	if _, has := asst["tool_calls"]; has {
		t.Error("assistant 的 tool_calls 应还原成标记文本，不再下发原始字段")
	}
	atxt := soloFirstText(t, got, 1)
	for _, want := range []string{traeFnBegin, "get_weather", "Tokyo", traeFnEnd} {
		if !strings.Contains(atxt, want) {
			t.Errorf("assistant 调用还原缺少 %q：\n%s", want, atxt)
		}
	}

	toolMsg := msgs[2].(map[string]any)
	if toolMsg["role"] != "user" {
		t.Errorf("tool 结果应归入 user（上游无 tool 角色），got %v", toolMsg["role"])
	}
	if _, has := toolMsg["tool_call_id"]; has {
		t.Error("tool 结果应包成标记，不再下发原始 tool_call_id")
	}
	ttxt := soloFirstText(t, got, 2)
	for _, want := range []string{traeFnResult, "sunny, 25C"} {
		if !strings.Contains(ttxt, want) {
			t.Errorf("工具结果标记缺少 %q：\n%s", want, ttxt)
		}
	}
}

// TestBuildSoloPayloadKeepsToolResultHardInstruction 历史里已有工具结果时，
// 工具说明必须追加「禁止再次输出标记」的硬指令，抑制重复调用的调用环。
func TestBuildSoloPayloadKeepsToolResultHardInstruction(t *testing.T) {
	got := buildSoloFromJSON(t, `{"messages":[`+
		`{"role":"user","content":"weather?"},`+
		`{"role":"tool","tool_call_id":"c1","content":"sunny"}`+
		`],`+soloTools+`}`)
	txt := soloFirstText(t, got, 0)
	if !strings.Contains(txt, "严禁再次输出任何") {
		t.Fatalf("已有工具结果时必须追加硬指令：\n%s", txt)
	}
}

// TestBuildSoloPayloadPreservesImageBlocks 注入工具说明不得破坏多模态块：
// 图片块与调用方原文必须原样保留，说明只作为额外 text 块前置。
func TestBuildSoloPayloadPreservesImageBlocks(t *testing.T) {
	got := buildSoloFromJSON(t, `{"messages":[{"role":"user","content":[`+
		`{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}},`+
		`{"type":"text","text":"what is this?"}]}],`+soloTools+`}`)

	msgs, _ := got["messages"].([]any)
	blocks, _ := msgs[0].(map[string]any)["content"].([]any)
	if len(blocks) != 3 {
		t.Fatalf("块数 = %d, want 3（工具说明 + 图片 + 原文）: %#v", len(blocks), blocks)
	}
	if blocks[0].(map[string]any)["type"] != "text" {
		t.Errorf("工具说明应作为首个 text 块前置，got %#v", blocks[0])
	}
	if !strings.Contains(blocks[0].(map[string]any)["text"].(string), traeFnBegin) {
		t.Error("首个块应承载工具说明")
	}
	if blocks[1].(map[string]any)["type"] != "image_url" {
		t.Errorf("图片块必须原样保留，got %#v", blocks[1])
	}
	if blocks[2].(map[string]any)["text"] != "what is this?" {
		t.Errorf("调用方原文不得被覆盖，got %#v", blocks[2])
	}
}

// TestBuildSoloPayloadNoToolsUnchanged 无工具时行为与修复前完全一致
// （单 text 块、正文原样），确保基础对话零回归。
func TestBuildSoloPayloadNoToolsUnchanged(t *testing.T) {
	got := buildSoloFromJSON(t, `{"messages":[{"role":"user","content":"hello"}]}`)
	msgs, _ := got["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages 条数 = %d, want 1", len(msgs))
	}
	blocks, _ := msgs[0].(map[string]any)["content"].([]any)
	if len(blocks) != 1 || blocks[0].(map[string]any)["text"] != "hello" {
		t.Fatalf("无工具时正文形态应不变，got %#v", blocks)
	}
}
