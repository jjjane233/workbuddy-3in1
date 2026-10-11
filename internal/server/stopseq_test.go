package server

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// TestStopMatcherHoldsBackPartialPrefix 校准 hold-back：stop 序列跨分片时，
// 前一片结尾不能把可能是 stop 前缀的字符先吐出去（吐了收不回来）。
func TestStopMatcherHoldsBackPartialPrefix(t *testing.T) {
	m := newStopMatcher([]string{"END"})
	var got strings.Builder
	var hits []bool
	for _, chunk := range []string{"hello EN", "D more", "tail"} {
		emit, hit := m.feed(chunk)
		got.WriteString(emit)
		hits = append(hits, hit)
	}
	if got.String() != "hello " {
		t.Fatalf("emit = %q, want %q", got.String(), "hello ")
	}
	if hits[0] || !hits[1] || hits[2] {
		t.Fatalf("hits = %v, want [false true false]", hits)
	}
	if tail := m.flush(); tail != "" {
		t.Fatalf("flush after hit = %q, want empty", tail)
	}
}

// TestStopMatcherFlushReturnsTailWithoutHit stop 未命中时收流必须把 hold 住的尾巴
// 原样补发——否则每个请求的最后 len(stop)-1 个字符都会被静默吞掉。
func TestStopMatcherFlushReturnsTailWithoutHit(t *testing.T) {
	m := newStopMatcher([]string{"STOP"})
	var got strings.Builder
	for _, chunk := range []string{"abc", "XY"} {
		emit, _ := m.feed(chunk)
		got.WriteString(emit)
	}
	got.WriteString(m.flush())
	if got.String() != "abcXY" {
		t.Fatalf("got %q, want %q", got.String(), "abcXY")
	}
}

// TestStopMatcherImmediateHit 第一片就命中：命中点之前的正文要保留，命中串丢弃。
func TestStopMatcherImmediateHit(t *testing.T) {
	m := newStopMatcher([]string{"5"})
	emit, hit := m.feed("1,2,3,4,5,6")
	if !hit || emit != "1,2,3,4," {
		t.Fatalf("emit=%q hit=%v, want %q/true", emit, hit, "1,2,3,4,")
	}
}

// TestParseStopSequences 兼容 string 与 []string 两种形态，空串丢弃、超过 4 条截断。
func TestParseStopSequences(t *testing.T) {
	cases := []struct {
		raw  string
		want []string
	}{
		{`"##"`, []string{"##"}},
		{`["a","b"]`, []string{"a", "b"}},
		{`["","x"]`, []string{"x"}},
		{`["1","2","3","4","5"]`, []string{"1", "2", "3", "4"}},
		{`""`, nil},
		{`[]`, nil},
		{``, nil},
		{`123`, nil},
	}
	for _, c := range cases {
		var raw []byte
		if c.raw != "" {
			raw = []byte(c.raw)
		}
		got := parseStopSequences(raw)
		if len(got) != len(c.want) {
			t.Fatalf("parseStopSequences(%s) = %v, want %v", c.raw, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("parseStopSequences(%s) = %v, want %v", c.raw, got, c.want)
			}
		}
	}
}

// TestStopFilterReaderTruncatesStream 端到端：SSE 流经过滤器后正文在 stop 处截断，
// finish_reason 收敛为 stop，[DONE] 保留。
func TestStopFilterReaderTruncatesStream(t *testing.T) {
	raw := strings.Join([]string{
		`data: {"choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		``,
		`data: {"choices":[{"index":0,"delta":{"content":"1,2,3"},"finish_reason":null}]}`,
		``,
		`data: {"choices":[{"index":0,"delta":{"content":",4,5,6,7"},"finish_reason":null}]}`,
		``,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"length"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	rc := NewStopFilter(io.NopCloser(strings.NewReader(raw)), []string{"5"})
	out, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(out)
	if strings.Contains(body, "6") || strings.Contains(body, "7") {
		t.Fatalf("stop 之后的内容泄漏: %s", body)
	}
	// 正文跨帧，按 chunk 拼接后判定（单帧里不会出现完整串）。
	var joined strings.Builder
	for _, ln := range strings.Split(body, "\n") {
		pfx := "data: "
		if !strings.HasPrefix(ln, pfx) {
			continue
		}
		payload := strings.TrimPrefix(ln, pfx)
		if payload == "[DONE]" {
			continue
		}
		var obj map[string]any
		if json.Unmarshal([]byte(payload), &obj) != nil {
			continue
		}
		ch, _ := obj["choices"].([]any)
		for _, c := range ch {
			cm, _ := c.(map[string]any)
			d, _ := cm["delta"].(map[string]any)
			s, _ := d["content"].(string)
			joined.WriteString(s)
		}
	}
	if joined.String() != "1,2,3,4," {
		t.Fatalf("拼接正文 = %q, want %q (body=%s)", joined.String(), "1,2,3,4,", body)
	}
	if strings.Contains(body, `"finish_reason":"length"`) {
		t.Fatalf("stop 之后的收尾帧 finish_reason 未收敛: %s", body)
	}
	if !strings.Contains(body, `"finish_reason":"stop"`) {
		t.Fatalf("finish_reason 未收敛为 stop: %s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("[DONE] 丢失: %s", body)
	}
}

// TestStopFilterReaderNoStopsIsPassThrough stops 为空时必须逐字节原样返回
// （默认流量零改动，这是「不破坏存量行为」的硬约束）。
func TestStopFilterReaderNoStopsIsPassThrough(t *testing.T) {
	raw := "data: {\"choices\":[{\"delta\":{\"content\":\"abc\"} }]}\n\ndata: [DONE]\n\n"
	rc := NewStopFilter(io.NopCloser(strings.NewReader(raw)), nil)
	out, _ := io.ReadAll(rc)
	if string(out) != raw {
		t.Fatalf("passthrough mismatch: got %q want %q", out, raw)
	}
}

// TestStopFilterReaderKeepsNonContentFrames 无 content 的帧（只有 finish_reason）
// 必须原样通过，不能被改写成 content 帧。
func TestStopFilterReaderKeepsNonContentFrames(t *testing.T) {
	raw := "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"} }]}\n\ndata: [DONE]\n\n"
	rc := NewStopFilter(io.NopCloser(strings.NewReader(raw)), []string{"X"})
	out, _ := io.ReadAll(rc)
	if !bytes.Contains(out, []byte(`"finish_reason":"stop"`)) {
		t.Fatalf("非 content 帧被破坏: %s", out)
	}
}

// TestTruncateAtStop 非流式正文截断。
func TestTruncateAtStop(t *testing.T) {
	got, hit := truncateAtStop("1,2,3,4,5,6", []string{"5"})
	if !hit || got != "1,2,3,4," {
		t.Fatalf("got %q hit %v", got, hit)
	}
	got, hit = truncateAtStop("abcdef", []string{"zz"})
	if hit || got != "abcdef" {
		t.Fatalf("got %q hit %v", got, hit)
	}
}
