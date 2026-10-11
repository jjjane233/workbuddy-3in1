package server

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAnthToolsFromOAI(t *testing.T) {
	raw := json.RawMessage("[{\"type\":\"function\",\"function\":{\"name\":\"get_weather\",\"description\":\"w\",\"parameters\":{\"type\":\"object\",\"properties\":{\"city\":{\"type\":\"string\"}},\"required\":[\"city\"]}}}]")
	out := anthToolsFromOAI(raw)
	if len(out) == 0 {
		t.Fatal("expected anthropic tools")
	}
	var arr []map[string]any
	if err := json.Unmarshal(out, &arr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(arr) != 1 || arr[0]["name"] != "get_weather" {
		t.Fatalf("bad tool: %#v", arr)
	}
	if _, ok := arr[0]["input_schema"].(map[string]any); !ok {
		t.Fatalf("input_schema missing: %#v", arr[0])
	}
	if _, has := arr[0]["function"]; has {
		t.Fatal("openai wrapper leaked")
	}
}

func TestAnthToolsFromOAIPassthrough(t *testing.T) {
	raw := json.RawMessage("[{\"name\":\"get_weather\",\"description\":\"w\",\"input_schema\":{\"type\":\"object\"}}]")
	out := string(anthToolsFromOAI(raw))
	if !strings.Contains(out, "input_schema") || strings.Contains(out, "\"function\"") {
		t.Fatalf("bad passthrough: %s", out)
	}
}

func TestAnthToolChoiceFromOAI(t *testing.T) {
	cases := []struct{ in, want string }{
		{"\"auto\"", "{\"type\":\"auto\"}"},
		{"\"required\"", "{\"type\":\"any\"}"},
		{"\"none\"", ""},
		{"{\"type\":\"function\",\"function\":{\"name\":\"get_weather\"}}", "{\"name\":\"get_weather\",\"type\":\"tool\"}"},
	}
	for _, c := range cases {
		got := string(anthToolChoiceFromOAI(json.RawMessage(c.in)))
		if c.want == "" {
			if got != "" {
				t.Fatalf("in=%s want none got %s", c.in, got)
			}
			continue
		}
		var a, b map[string]any
		if json.Unmarshal([]byte(got), &a) != nil || json.Unmarshal([]byte(c.want), &b) != nil {
			t.Fatalf("bad json in=%s got=%s", c.in, got)
		}
		if a["type"] != b["type"] {
			t.Fatalf("in=%s got=%s want=%s", c.in, got, c.want)
		}
	}
}

func TestAnthMessagesFromOAIToolRoundTrip(t *testing.T) {
	in := []OAIMessage{
		{Role: "user", Content: json.RawMessage("\"北京天气\"")},
		{Role: "assistant", ToolCalls: json.RawMessage("[{\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"{\\\"city\\\":\\\"北京\\\"}\"}}]")},
		{Role: "tool", ToolCallID: "call_1", Content: json.RawMessage("\"晴 20C\"")},
	}
	msgs, sys := anthMessagesFromOAI(in)
	if sys != "" {
		t.Fatalf("unexpected system: %s", sys)
	}
	b, _ := json.Marshal(msgs)
	got := string(b)
	for _, want := range []string{"tool_use", "tool_result", "call_1", "get_weather"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %s in %s", want, got)
		}
	}
}

func TestTraeFnParserBasic(t *testing.T) {
	p := newTraeFnParser()
	out, calls := p.feed("<|FunctionCallBegin|>[{\"name\":\"get_weather\",\"parameters\":{\"city\":\"北京\"}}]<|FunctionCallEnd|>")
	if out != "" {
		t.Fatalf("expected empty text, got %q", out)
	}
	if len(calls) != 1 || calls[0].Function.Name != "get_weather" {
		t.Fatalf("bad calls: %#v", calls)
	}
	if calls[0].Function.Arguments != "{\"city\":\"北京\"}" {
		t.Fatalf("bad args: %s", calls[0].Function.Arguments)
	}
}

func TestTraeFnParserSplitAcrossChunks(t *testing.T) {
	p := newTraeFnParser()
	var text strings.Builder
	var calls []oaiToolCall
	chunks := []string{"好的，", "<|Func", "tionCallBe", "gin|>[{\"na", "me\":\"get_weather\",\"parameters\":{\"city\":\"上海\"}}]<|FunctionCall", "End|>", "完成了"}
	for _, c := range chunks {
		o, cs := p.feed(c)
		text.WriteString(o)
		calls = append(calls, cs...)
	}
	o, cs := p.flush()
	text.WriteString(o)
	calls = append(calls, cs...)
	if len(calls) != 1 || calls[0].Function.Name != "get_weather" {
		t.Fatalf("bad calls: %#v", calls)
	}
	if calls[0].Function.Arguments != "{\"city\":\"上海\"}" {
		t.Fatalf("bad args: %s", calls[0].Function.Arguments)
	}
	if text.String() != "好的，完成了" {
		t.Fatalf("bad text: %q", text.String())
	}
}

func TestTraeFnParserNoMarkerPassThrough(t *testing.T) {
	p := newTraeFnParser()
	var text strings.Builder
	for _, c := range []string{"普通", "回答", "没有工具"} {
		o, _ := p.feed(c)
		text.WriteString(o)
	}
	o, _ := p.flush()
	text.WriteString(o)
	if text.String() != "普通回答没有工具" {
		t.Fatalf("got %q", text.String())
	}
}

func TestParseTraeCallsSingleObject(t *testing.T) {
	cs := parseTraeCalls("{\"name\":\"f\",\"parameters\":{\"a\":1}}")
	if len(cs) != 1 || cs[0].Function.Name != "f" {
		t.Fatalf("bad: %#v", cs)
	}
}

func TestToolChoiceIsNone(t *testing.T) {
	if !toolChoiceIsNone(json.RawMessage("\"none\"")) {
		t.Fatal("none not detected")
	}
	if toolChoiceIsNone(json.RawMessage("\"auto\"")) {
		t.Fatal("auto misdetected")
	}
	if toolChoiceIsNone(nil) {
		t.Fatal("nil misdetected")
	}
}

func TestTraeToolPromptContainsSchema(t *testing.T) {
	raw := json.RawMessage("[{\"type\":\"function\",\"function\":{\"name\":\"get_weather\",\"description\":\"查天气\",\"parameters\":{\"type\":\"object\"}}}]")
	p := traeToolPrompt(raw)
	if !strings.Contains(p, "get_weather") || !strings.Contains(p, "<|FunctionCallBegin|>") {
		t.Fatalf("bad prompt: %s", p)
	}
}

func TestBuildTraePayloadInjectsToolPrompt(t *testing.T) {
	tools, _ := json.Marshal([]any{map[string]any{"type": "function", "function": map[string]any{"name": "get_weather", "description": "查天气", "parameters": map[string]any{"type": "object"}}}})
	req := &OAIRequest{Messages: []OAIMessage{{Role: "user", Content: json.RawMessage("\"北京天气\"")}}, Tools: tools, ToolChoice: json.RawMessage("\"auto\"")}
	raw := buildTraePayload(req, "seed_m8")
	s := string(raw)
	for _, want := range []string{"get_weather", "<|FunctionCallBegin|>", "<|FunctionCallResult|>"} {
		if !strings.Contains(s, want) {
			t.Fatalf("payload missing %s:\n%s", want, s)
		}
	}
	if strings.Contains(s, "tools") {
		t.Fatalf("raw tools field leaked: %s", s)
	}
}

func TestBuildTraePayloadNoToolsWhenNone(t *testing.T) {
	tools, _ := json.Marshal([]any{map[string]any{"type": "function", "function": map[string]any{"name": "get_weather", "parameters": map[string]any{"type": "object"}}}})
	req := &OAIRequest{Messages: []OAIMessage{{Role: "user", Content: json.RawMessage("\"hi\"")}}, Tools: tools, ToolChoice: json.RawMessage("\"none\"")}
	raw := buildTraePayload(req, "seed_m8")
	if strings.Contains(string(raw), "<|FunctionCallBegin|>") {
		t.Fatalf("tool prompt injected despite tool_choice=none: %s", raw)
	}
}

func TestRenderTraeCallsAndResult(t *testing.T) {
	calls := []oaiToolCall{{ID: "call_1", Type: "function", Function: oaiToolCallFnSpec{Name: "get_weather", Arguments: "{\"city\":\"北京\"}"}}}
	r := renderTraeCalls(calls)
	if !strings.Contains(r, "<|FunctionCallBegin|>") || !strings.Contains(r, "get_weather") || !strings.Contains(r, "city") {
		t.Fatalf("bad calls: %s", r)
	}
	res := renderTraeResult("call_1", "晴 20C")
	if !strings.Contains(res, "<|FunctionCallResult|>") || !strings.Contains(res, "晴 20C") {
		t.Fatalf("bad result: %s", res)
	}
}

func TestBuildTraePayloadToolResultMarker(t *testing.T) {
	req := &OAIRequest{Messages: []OAIMessage{
		{Role: "user", Content: json.RawMessage("\"北京天气\"")},
		{Role: "assistant", ToolCalls: json.RawMessage("[{\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"{}\"}}]")},
		{Role: "tool", ToolCallID: "call_1", Content: json.RawMessage("\"晴 20C\"")},
	}}
	s := string(buildTraePayload(req, "seed_m8"))
	if !strings.Contains(s, "<|FunctionCallResult|>") {
		t.Fatalf("tool result not rendered: %s", s)
	}
}
