package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"strings"
)

// 网关侧的 stop 序列支持。
//
// 为什么需要：OpenAI 协议里 stop 是「最多 4 个字符串，出现即停止生成且不含该串」。
// 实测（2026-10-07，三家反代）：
//
//	ZCode  -> 生效（oaiToZCodeRequest 把 stop 映射成 Anthropic 的 stop_sequences）
//	Trae   -> 不生效（Trae 私有协议没有该字段，buildTraePayload 无从下发）
//	WorkBuddy -> 不生效（上游忽略该字段，实测带 stop=["5"] 仍输出完整 "1,...,10"）
//
// 客户端（IDE 插件 / agent 框架）大量依赖 stop 来切分结构化输出，静默失效会让
// 解析器拿到多余内容。这里在**网关侧**补上该语义。
//
// 设计约束：只在请求确实带了非空 stop 时才包这一层（见 handler.go），默认流量
// 逐字节不变——不碰存量行为。

// stopMatcher 是流式下的 stop 截断状态机。
//
// 难点是 stop 序列可能跨分片（本片结尾是 "ab"、下一片开头是 "cd"，stop="abcd"），
// 而 SSE 已经写出去的字节收不回来。因此采用 hold-back：每次只吐出「确定不可能
// 再构成任何 stop 前缀」的部分，把最长 stop 长度 - 1 的尾巴留住。
type stopMatcher struct {
	stops []string
	held  string
	done  bool
}

func newStopMatcher(stops []string) *stopMatcher {
	cleaned := make([]string, 0, len(stops))
	for _, s := range stops {
		if s != "" {
			cleaned = append(cleaned, s)
		}
	}
	if len(cleaned) == 0 {
		return nil
	}
	return &stopMatcher{stops: cleaned}
}

// indexAnyStop 返回最早出现的 stop 及其下标；没有返回 -1。
// 平局时取更长的那个（更早结束生成，符合「出现即停」语义）。
func indexAnyStop(s string, stops []string) (int, string) {
	best, bestStop := -1, ""
	for _, st := range stops {
		i := strings.Index(s, st)
		if i < 0 {
			continue
		}
		if best < 0 || i < best || (i == best && len(st) > len(bestStop)) {
			best, bestStop = i, st
		}
	}
	return best, bestStop
}

// feed 吃进一段正文，返回本次可以安全吐出的部分；hit 表示本次触发了停止。
func (m *stopMatcher) feed(chunk string) (emit string, hit bool) {
	if m == nil {
		return chunk, false
	}
	if m.done {
		return "", false
	}
	buf := m.held + chunk
	if i, _ := indexAnyStop(buf, m.stops); i >= 0 {
		m.done = true
		m.held = ""
		return buf[:i], true
	}
	keep := 0
	for _, s := range m.stops {
		if len(s)-1 > keep {
			keep = len(s) - 1
		}
	}
	if len(buf) <= keep {
		m.held = buf
		return "", false
	}
	cut := len(buf) - keep
	out := buf[:cut]
	m.held = buf[cut:]
	return out, false
}

// flush 收流时吐出仍被 hold 住的尾巴（没有命中 stop 时应原样补发）。
func (m *stopMatcher) flush() string {
	if m == nil || m.done {
		return ""
	}
	out := m.held
	m.held = ""
	return out
}

// truncateAtStop 非流式正文的 stop 截断：返回截断后文本与是否命中。
func truncateAtStop(text string, stops []string) (string, bool) {
	if i, _ := indexAnyStop(text, stops); i >= 0 {
		return text[:i], true
	}
	return text, false
}

// parseStopSequences 解析请求体里的 OpenAI stop 字段：字符串或字符串数组都认，
// 空串与超过 4 条的部分按 OpenAI 规范丢弃。
func parseStopSequences(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var one string
	if json.Unmarshal(raw, &one) == nil {
		if one == "" {
			return nil
		}
		return []string{one}
	}
	var many []string
	if json.Unmarshal(raw, &many) != nil {
		return nil
	}
	out := make([]string, 0, 4)
	for _, s := range many {
		if s == "" {
			continue
		}
		out = append(out, s)
		if len(out) == 4 {
			break
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// stopFilterReader 把上游的 OpenAI SSE 流按 stop 语义就地截断。
//
// 逐行处理，只改 data: 行里 JSON 的正文/结束原因，其余字节原样透传；
// 任何解析异常都退化成原文透传（绝不因网关改写把好好的流弄坏）。
type stopFilterReader struct {
	src   *bufio.Reader
	inner io.Closer
	m     *stopMatcher
	out   bytes.Buffer
	eof   bool
}

// lineSuffix 返回该行原本的行尾风格（保留 CRLF，避免把 \r\n 流改成 \n 流）。
func lineSuffix(line string) string {
	if strings.HasSuffix(line, "\r\n") {
		return "\r\n"
	}
	return "\n"
}

// NewStopFilter 包一层 stop 截断（stops 为空时原样返回 src，零开销）。
func NewStopFilter(rc io.ReadCloser, stops []string) io.ReadCloser {
	m := newStopMatcher(stops)
	if m == nil || rc == nil {
		return rc
	}
	return &stopFilterReader{src: bufio.NewReaderSize(rc, 64*1024), inner: rc, m: m}
}

func (f *stopFilterReader) Close() error {
	if f.inner != nil {
		return f.inner.Close()
	}
	return nil
}

func (f *stopFilterReader) Read(p []byte) (int, error) {
	for f.out.Len() == 0 {
		if f.eof {
			return 0, io.EOF
		}
		line, err := f.src.ReadString('\n')
		f.processLine(line)
		if err != nil {
			if err == io.EOF {
				f.eof = true
				// 收流：把 hold 住的尾巴补发，别吞掉正文。
				if tail := f.m.flush(); tail != "" {
					f.emitContentFrame(tail, true)
				}
			} else {
				return 0, err
			}
		}
		if f.out.Len() == 0 && err == nil {
			continue
		}
	}
	return f.out.Read(p)
}

// processLine 处理一行：data: 行的正文按 stop 截断后重写，其余原样透传。
func (f *stopFilterReader) processLine(line string) {
	trimmed := strings.TrimRight(line, "\r\n")
	if !strings.HasPrefix(trimmed, "data: ") {
		f.out.WriteString(line)
		return
	}
	payload := strings.TrimPrefix(trimmed, "data: ")
	if payload == "[DONE]" {
		// DONE 之前先把 hold 住的尾巴补发（上游没触发 stop 时的正常收尾）。
		if tail := f.m.flush(); tail != "" {
			f.emitContentFrame(tail, true)
		}
		f.out.WriteString(line)
		return
	}
	var obj map[string]any
	if json.Unmarshal([]byte(payload), &obj) != nil {
		f.out.WriteString(line)
		return
	}
	if idx, ok := f.contentSlot(obj); ok {
		text := f.textAt(obj, idx)
		emit, hit := f.m.feed(text)
		if hit {
			f.setFinishStop(obj, idx)
		}
		f.setText(obj, idx, emit)
		if raw, err := json.Marshal(obj); err == nil {
			// 只重写这一行本身；帧分隔的空行由原始流原样透传。
			// 之前自己再补一个换行会导致每帧多出一个空行，严格的 SSE 解析器
			// 可能把它当成帧边界，从而看到空 data 帧。
			f.out.WriteString("data: " + string(raw) + lineSuffix(line))
			return
		}
	}
	// 已经停止后：正文一律清空，且**所有** choice 的 finish_reason 收敛成 stop。
	// 这一步不能只在「带 content 的帧」上做——上游的收尾帧常常是 delta:{} + length，
	// 漏掉它客户端就会看到 stop 之后又跟一个 length，判定为被截断。
	if f.m.done {
		changed := false
		if choices, ok := obj["choices"].([]any); ok {
			for _, c := range choices {
				cm, ok := c.(map[string]any)
				if !ok {
					continue
				}
				if _, has := cm["finish_reason"]; has {
					if cm["finish_reason"] != "stop" {
						cm["finish_reason"] = "stop"
						changed = true
					}
				}
				if d, ok := cm["delta"].(map[string]any); ok {
					if v, ok := d["content"].(string); ok && v != "" {
						d["content"] = ""
						changed = true
					}
				}
			}
		}
		if changed {
			if raw, err := json.Marshal(obj); err == nil {
				f.out.WriteString("data: " + string(raw) + lineSuffix(line))
				return
			}
		}
	}
	f.out.WriteString(line)
}

// emitContentFrame 产出一帧只带 content 的 chunk（收流补发 hold 尾巴用）。
func (f *stopFilterReader) emitContentFrame(text string, final bool) {
	frame := map[string]any{
		"object": "chat.completion.chunk",
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{"content": text},
		}},
	}
	if final {
		frame["choices"].([]any)[0].(map[string]any)["finish_reason"] = nil
	}
	raw, err := json.Marshal(frame)
	if err != nil {
		return
	}
	f.out.WriteString("data: " + string(raw) + "\n\n")
}

// contentSlot 找到 choices 数组里带内容的那个下标（delta.content / message.content / text）。
func (f *stopFilterReader) contentSlot(obj map[string]any) (int, bool) {
	choices, _ := obj["choices"].([]any)
	for i, c := range choices {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if d, ok := m["delta"].(map[string]any); ok {
			if _, ok := d["content"].(string); ok {
				return i, true
			}
			continue
		}
		if msg, ok := m["message"].(map[string]any); ok {
			if _, ok := msg["content"].(string); ok {
				return i, true
			}
			continue
		}
		if _, ok := m["text"].(string); ok {
			return i, true
		}
	}
	return 0, false
}

func (f *stopFilterReader) textAt(obj map[string]any, idx int) string {
	c := obj["choices"].([]any)[idx].(map[string]any)
	if d, ok := c["delta"].(map[string]any); ok {
		s, _ := d["content"].(string)
		return s
	}
	if msg, ok := c["message"].(map[string]any); ok {
		s, _ := msg["content"].(string)
		return s
	}
	s, _ := c["text"].(string)
	return s
}

func (f *stopFilterReader) setText(obj map[string]any, idx int, v string) {
	c := obj["choices"].([]any)[idx].(map[string]any)
	if d, ok := c["delta"].(map[string]any); ok {
		d["content"] = v
		return
	}
	if msg, ok := c["message"].(map[string]any); ok {
		msg["content"] = v
		return
	}
	c["text"] = v
}

func (f *stopFilterReader) setFinishStop(obj map[string]any, idx int) {
	c := obj["choices"].([]any)[idx].(map[string]any)
	c["finish_reason"] = "stop"
}

// applyStopToAggregate 对非流式聚合响应（map 形态）做正文截断。
// 只动 choices[].message.content，其余字段（含 usage）一律不碰——用量按上游
// 真实产出记账，网关的截断不该改变 token 账。
func applyStopToAggregate(resp map[string]any, stops []string) {
	if resp == nil || len(stops) == 0 {
		return
	}
	choices, _ := resp["choices"].([]any)
	for _, c := range choices {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		msg, ok := cm["message"].(map[string]any)
		if !ok {
			continue
		}
		text, ok := msg["content"].(string)
		if !ok || text == "" {
			continue
		}
		if cut, hit := truncateAtStop(text, stops); hit {
			msg["content"] = cut
			cm["finish_reason"] = "stop"
		}
	}
}
