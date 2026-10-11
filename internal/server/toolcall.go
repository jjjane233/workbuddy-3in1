package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// 工具调用（function calling）跨协议适配。
//
// 背景（2026-10-07 三家反代实测）：
//
//	WorkBuddy(CodeBuddy) 上游原生支持 OpenAI tools，直接透传，无需本文件。
//	ZCode 上游是 Anthropic Messages：tools 必须是 {name,description,input_schema}，
//	      tool_choice 必须转成字典（{type:auto} / {type:any} / {type:tool,name}）。
//	      原样下发 OpenAI 的 {type:function,function:{...}} 会被上游 400 拒绝：
//	      "body.tools.0.name: Field required; body.tool_choice: Input should be a valid dictionary"。
//	Trae 上游 /api/ide/v1/chat 会静默忽略 tools 字段（既不报错也不调用，直接按
//	      自己的知识编造答案）。它的工具调用靠提示词注入 + 内联标记
//	      <|FunctionCallBegin|>[{"name":..,"parameters":{..}}]<|FunctionCallEnd|>，
//	      网关需要 ① 请求侧把工具定义注入提示词；② 响应侧把标记翻译成 tool_calls。

// ---- OpenAI 工具协议类型 ----

// oaiTool 是 OpenAI tools[] 元素。
type oaiTool struct {
	Type     string        `json:"type"`
	Function oaiToolFnSpec `json:"function"`
}

// oaiToolFnSpec 是 tools[].function。
type oaiToolFnSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// oaiToolCall 是响应 message.tool_calls[] 元素。
type oaiToolCall struct {
	Index    *int              `json:"index,omitempty"`
	ID       string            `json:"id"`
	Type     string            `json:"type"`
	Function oaiToolCallFnSpec `json:"function"`
}

// oaiToolCallFnSpec 是 tool_calls[].function。
type oaiToolCallFnSpec struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// marshalNoHTML 序列化时关闭 < > & 的 HTML 转义。
// Trae 的工具标记 <|FunctionCallBegin|> 若被转成 \u003c...\u003e，虽然 JSON 语义等价，
// 但注入提示词后模型看到的是转义形态，实测会降低标记遵循率，故这里显式关闭。
func marshalNoHTML(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil
	}
	b := buf.Bytes()
	if n := len(b); n > 0 && b[n-1] == '\n' {
		b = b[:n-1]
	}
	return b
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

// asInt 把 JSON 解出来的数字（float64 / json.Number）安全转 int。
func asInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	}
	return 0
}

// parseOAITools 解析 OpenAI tools（[{type:function,function:{...}}]）。
// 非 function 类型（如内置 web_search）与无名工具一律丢弃：三家上游都不认。
func parseOAITools(raw json.RawMessage) []oaiTool {
	if len(raw) == 0 {
		return nil
	}
	var arr []oaiTool
	if json.Unmarshal(raw, &arr) != nil {
		return nil
	}
	out := make([]oaiTool, 0, len(arr))
	for _, t := range arr {
		if t.Type == "function" && t.Function.Name != "" {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// parseOAIToolCalls 解析请求消息里的 assistant tool_calls。
func parseOAIToolCalls(raw json.RawMessage) []oaiToolCall {
	if len(raw) == 0 {
		return nil
	}
	var arr []oaiToolCall
	if json.Unmarshal(raw, &arr) != nil {
		return nil
	}
	return arr
}

// ---- ZCode：OpenAI tools → Anthropic tools ----

// anthToolsFromOAI 把工具定义转成 Anthropic 形态。
//
// 已是 Anthropic 形态（顶层没有 function、直接带 name / input_schema）的条目
// 原样保留，兼容调用方直接下发 Anthropic 风格 tools 的老行为（实测该形态上游 200）。
func anthToolsFromOAI(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var arr []map[string]any
	if json.Unmarshal(raw, &arr) != nil {
		return nil
	}
	out := make([]map[string]any, 0, len(arr))
	for _, t := range arr {
		fn, hasFn := t["function"].(map[string]any)
		if !hasFn {
			if _, ok := t["name"]; ok {
				out = append(out, t)
			}
			continue
		}
		name, _ := fn["name"].(string)
		if name == "" {
			continue
		}
		nt := map[string]any{"name": name}
		if v, ok := fn["description"]; ok {
			nt["description"] = v
		}
		if v, ok := fn["parameters"]; ok {
			nt["input_schema"] = v
		} else {
			nt["input_schema"] = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, nt)
	}
	if len(out) == 0 {
		return nil
	}
	return mustJSON(out)
}

// toolChoiceIsNone 判断 OpenAI tool_choice 是否为 none（等于禁用工具）。
func toolChoiceIsNone(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.EqualFold(strings.TrimSpace(s), "none")
	}
	return false
}

// anthToolChoiceFromOAI 把 OpenAI tool_choice 转成 Anthropic 形态。
// 返回 nil 表示不下发该字段（交给上游默认 auto）。
func anthToolChoiceFromOAI(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "auto":
			return json.RawMessage(`{"type":"auto"}`)
		case "required", "any":
			return json.RawMessage(`{"type":"any"}`)
		default:
			return nil
		}
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	if t, _ := m["type"].(string); strings.EqualFold(t, "function") {
		if fn, ok := m["function"].(map[string]any); ok {
			if name, _ := fn["name"].(string); name != "" {
				return mustJSON(map[string]any{"type": "tool", "name": name})
			}
		}
		return nil
	}
	if t, _ := m["type"].(string); t == "auto" || t == "any" || t == "tool" {
		return raw
	}
	return nil
}

// ---- Trae：提示词注入 + 内联标记解析 ----

const (
	traeFnBegin = "<|FunctionCallBegin|>"
	traeFnEnd   = "<|FunctionCallEnd|>"
	// traeFnResult 是回灌给模型的工具结果包装（Trae chat_history 只有 user/assistant）。
	traeFnResult = "<|FunctionCallResult|>"
)

// traeToolPrompt 生成注入 Trae 上下文的工具说明。
//
// 上游把 tools 字段整个忽略（实测带 tools 仍回自然语言编造），唯一可行的通路
// 是把「有哪些工具 + 用什么格式调用」写进提示词。格式与官方 IDE 客户端一致。
func traeToolPrompt(raw json.RawMessage) string {
	tools := parseOAITools(raw)
	if len(tools) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("【工具调用能力】你可以调用下列工具来完成任务。\n\n可用工具：\n")
	for _, tl := range tools {
		b.WriteString("- ")
		b.WriteString(tl.Function.Name)
		if tl.Function.Description != "" {
			b.WriteString("：")
			b.WriteString(tl.Function.Description)
		}
		b.WriteString("\n")
		if len(tl.Function.Parameters) > 0 {
			b.WriteString("  参数 JSON Schema：")
			b.WriteString(string(tl.Function.Parameters))
			b.WriteString("\n")
		}
	}
	b.WriteString("\n需要调用工具时，你的整条回复必须且只能包含如下格式，不要有任何解释或多余文字：\n")
	b.WriteString(traeFnBegin)
	b.WriteString(`[{"name":"工具名","parameters":{参数对象}}]`)
	b.WriteString(traeFnEnd)
	b.WriteString("\n一次可调用多个工具，用 JSON 数组表示。不需要调用工具时，用自然语言正常回答。\n")
	b.WriteString("工具执行结果会以如下格式回传给你，收到后请据此给出最终回答，不要重复调用同一工具：\n")
	b.WriteString(traeFnResult)
	b.WriteString(`[{"name":"工具名","result":结果}]`)
	b.WriteString(traeFnEnd)
	return b.String()
}

// renderTraeCalls 把 assistant 历史里的 tool_calls 还原成上游认识的标记文本，
// 否则模型看不到自己上一轮调用过什么，多轮工具链会断。
func renderTraeCalls(calls []oaiToolCall) string {
	type rc struct {
		Name       string          `json:"name"`
		Parameters json.RawMessage `json:"parameters"`
	}
	arr := make([]rc, 0, len(calls))
	for _, c := range calls {
		args := strings.TrimSpace(c.Function.Arguments)
		if args == "" {
			args = "{}"
		}
		arr = append(arr, rc{Name: c.Function.Name, Parameters: json.RawMessage(args)})
	}
	return traeFnBegin + string(mustJSON(arr)) + traeFnEnd
}

// hasToolResult 判断消息历史里是否已有工具执行结果。
func hasToolResult(msgs []OAIMessage) bool {
	for _, m := range msgs {
		if strings.EqualFold(m.Role, "tool") {
			return true
		}
	}
	return false
}

// renderTraeResult 把 OpenAI role:"tool" 的工具结果渲染成 Trae 认识的结果标记。
// 上游 chat_history 没有 tool 角色，直接塞裸文本模型认不出这是工具返回值，
// 会重复调用同一工具；加标记并在提示词里说明格式后，模型能正确续答。
func renderTraeResult(toolCallID, text string) string {
	payload := map[string]any{"result": text}
	if toolCallID != "" {
		payload["id"] = toolCallID
	}
	return traeFnResult + string(mustJSON(payload)) + traeFnEnd
}

// traeFnParser 从 Trae 的正文流里剥离 <|FunctionCallBegin|>...<|FunctionCallEnd|>。
//
// 标记可能跨 output 帧（上游把长回复切成多个 response 帧），因此按 hold-back
// 状态机处理：只吐出「不可能再构成标记前缀」的正文，把可能残缺的尾部留住。
type traeFnParser struct {
	pending string
	inCall  bool
	calls   []oaiToolCall
}

func newTraeFnParser() *traeFnParser { return &traeFnParser{} }

// feed 吃进一段正文，返回可安全吐出的正文与本次新完成的工具调用。
func (p *traeFnParser) feed(chunk string) (string, []oaiToolCall) {
	if p == nil || chunk == "" {
		return "", nil
	}
	p.pending += chunk
	var out strings.Builder
	var fresh []oaiToolCall
	for {
		if p.inCall {
			i := strings.Index(p.pending, traeFnEnd)
			if i < 0 {
				break
			}
			payload := p.pending[:i]
			p.pending = p.pending[i+len(traeFnEnd):]
			p.inCall = false
			if cs := parseTraeCalls(payload); len(cs) > 0 {
				fresh = append(fresh, cs...)
				p.calls = append(p.calls, cs...)
			}
			continue
		}
		j := strings.Index(p.pending, traeFnBegin)
		if j >= 0 {
			out.WriteString(p.pending[:j])
			p.pending = p.pending[j+len(traeFnBegin):]
			p.inCall = true
			continue
		}
		keep := len(traeFnBegin) - 1
		if len(p.pending) > keep {
			out.WriteString(p.pending[:len(p.pending)-keep])
			p.pending = p.pending[len(p.pending)-keep:]
		}
		break
	}
	return out.String(), fresh
}

// flush 收流：把仍被 hold 住的正文吐出；未闭合的调用尽力解析。
func (p *traeFnParser) flush() (string, []oaiToolCall) {
	if p == nil {
		return "", nil
	}
	if p.inCall {
		payload := p.pending
		p.pending = ""
		p.inCall = false
		if cs := parseTraeCalls(payload); len(cs) > 0 {
			p.calls = append(p.calls, cs...)
			return "", cs
		}
		return "", nil
	}
	t := p.pending
	p.pending = ""
	return t, nil
}

// parseTraeCalls 解析标记内载荷：期望 [{"name":..,"parameters":{..}}]，
// 也容忍单对象 {"name":..,"parameters":{..}} 与 name/arguments 混写。
func parseTraeCalls(payload string) []oaiToolCall {
	payload = strings.TrimSpace(payload)
	if payload == "" {
		return nil
	}
	type rawCall struct {
		Name       string          `json:"name"`
		Parameters json.RawMessage `json:"parameters"`
		Arguments  json.RawMessage `json:"arguments"`
	}
	var arr []rawCall
	if json.Unmarshal([]byte(payload), &arr) != nil {
		var one rawCall
		if json.Unmarshal([]byte(payload), &one) != nil || one.Name == "" {
			return nil
		}
		arr = []rawCall{one}
	}
	out := make([]oaiToolCall, 0, len(arr))
	for i, c := range arr {
		if c.Name == "" {
			continue
		}
		args := "{}"
		switch {
		case len(c.Parameters) > 0:
			args = string(c.Parameters)
		case len(c.Arguments) > 0:
			args = string(c.Arguments)
		}
		idx := i
		out = append(out, oaiToolCall{
			Index: &idx,
			ID:    "call_" + newID(""),
			Type:  "function",
			Function: oaiToolCallFnSpec{
				Name:      c.Name,
				Arguments: args,
			},
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// emitToolCallChunks 把一个 tool_call 列表写成 OpenAI SSE 增量帧。
// 每次带 role（首帧才发 role，由 sentRole 控制）与完整 arguments，
// 语义上等价于 OpenAI 的「一次给出整个 function call」。
func emitToolCallChunks(w io.Writer, flusher http.Flusher, id, model string, created int64, sentRole *bool, calls []oaiToolCall) {
	for i := range calls {
		idx := i
		one := calls[i]
		one.Index = &idx
		writeSSE(w, OAIChunk{ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
			Choices: []OAIChoice{{Index: 0, Delta: &OAIDelta{Role: roleOnce(sentRole), ToolCalls: mustJSON([]oaiToolCall{one})}}}})
		if flusher != nil {
			flusher.Flush()
		}
	}
}
