package server

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// TestApplyStopToAggregate WorkBuddy 非流式：正文在 stop 处截断，finish_reason 收敛，
// usage 不被动（网关截断不该改变 token 账）。
func TestApplyStopToAggregate(t *testing.T) {
	resp := map[string]any{
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": "1,2,3,4,5,6"},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{"prompt_tokens": 9, "completion_tokens": 12, "total_tokens": 21},
	}
	applyStopToAggregate(resp, []string{"5"})
	ch := resp["choices"].([]any)[0].(map[string]any)
	if got := ch["message"].(map[string]any)["content"]; got != "1,2,3,4," {
		t.Fatalf("content = %q, want %q", got, "1,2,3,4,")
	}
	if ch["finish_reason"] != "stop" {
		t.Fatalf("finish_reason = %v", ch["finish_reason"])
	}
	u := resp["usage"].(map[string]any)
	if u["completion_tokens"] != 12 {
		t.Fatalf("usage 被网关改写: %v", u)
	}
}

// TestApplyStopToAggregateNoHit 未命中时正文逐字不动。
func TestApplyStopToAggregateNoHit(t *testing.T) {
	resp := map[string]any{
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": "abcdef"},
			"finish_reason": "stop",
		}},
	}
	applyStopToAggregate(resp, []string{"zz"})
	if got := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"]; got != "abcdef" {
		t.Fatalf("content = %q", got)
	}
}

// TestStopFilterReaderStreamedAcrossChunks stop 串跨 chunk 分片（本片尾 + 下片头）：
// hold-back 必须保证不把已吐出的字节判错，命中点之后彻底静默。
func TestStopFilterReaderStreamedAcrossChunks(t *testing.T) {
	raw := strings.Join([]string{
		`data: {"choices":[{"index":0,"delta":{"content":"hello wor"},"finish_reason":null}]}`,
		``,
		`data: {"choices":[{"index":0,"delta":{"content":"ld STOP tail"},"finish_reason":null}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	rc := NewStopFilter(io.NopCloser(strings.NewReader(raw)), []string{"STOP"})
	out, _ := io.ReadAll(rc)
	body := string(out)
	if strings.Contains(body, "tail") {
		t.Fatalf("stop 之后内容泄漏: %s", body)
	}
	// 正文跨帧（hold-back 会把 "wor" 留到下一帧），按 chunk 拼接后判定。
	var joined strings.Builder
	for _, ln := range strings.Split(body, "\n") {
		if !strings.HasPrefix(ln, "data: ") || ln == "data: [DONE]" {
			continue
		}
		var obj map[string]any
		if json.Unmarshal([]byte(strings.TrimPrefix(ln, "data: ")), &obj) != nil {
			continue
		}
		chs, _ := obj["choices"].([]any)
		for _, c := range chs {
			cm, _ := c.(map[string]any)
			d, _ := cm["delta"].(map[string]any)
			sv, _ := d["content"].(string)
			joined.WriteString(sv)
		}
	}
	if joined.String() != "hello world " {
		t.Fatalf("拼接正文 = %q, want %q (body=%s)", joined.String(), "hello world ", body)
	}
}
