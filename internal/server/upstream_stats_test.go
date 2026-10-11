package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jjjane233/workbuddy-3in1/internal/auth"
)

// TestChatStatUpstreamFields 回归：上游链路（Trae / ZCode）必须把账号、模型、
// token、积分回填进 chatStat，否则 reqlog 归档里这几列全空（用户反馈的缺陷）。
func TestChatStatUpstreamFields(t *testing.T) {
	body := []byte("{\"model\":\"zcode:glm-4.7\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}],\"stream\":true}")
	st := newChatStat(time.Now(), body, true)
	if st.model != "zcode:glm-4.7" {
		t.Fatalf("model = %q, want zcode:glm-4.7", st.model)
	}
	if st.toks != -1 {
		t.Fatalf("initial toks = %d, want -1", st.toks)
	}
	st.noteAccount(&auth.Auth{UID: "70000000000000001", Nickname: "zcodeacct"})
	st.noteStreamUsage(upstreamUsage{PromptTokens: 120, HasPrompt: true, Completion: 34, HasCompletion: true}, "stop")
	if st.uid != "70000000000000001" || st.nick != "zcodeacct" {
		t.Fatalf("account = %q/%q", st.uid, st.nick)
	}
	if st.promptTokens != 120 || st.completionTokens != 34 || st.totalTokens != 154 {
		t.Fatalf("tokens = %d/%d/%d", st.promptTokens, st.completionTokens, st.totalTokens)
	}
	if st.toks != 34 {
		t.Fatalf("toks = %d, want 34", st.toks)
	}
	// 上游响应本身不带 credit、调用方也没折算 -> 保持未知（不伪造 0）。
	if st.hasCredit {
		t.Fatal("credit should stay unknown when upstream gives none")
	}

	// 调用方按实测单价折算后（Trae 双系数 / ZCode 当日真值分摊）必须落到归档：
	// 值进 credit、并且打上 credit_estimated 让面板加"约"。
	st2 := newChatStat(time.Now(), body, true)
	st2.noteStreamUsage(upstreamUsage{
		PromptTokens: 120, HasPrompt: true, Completion: 34, HasCompletion: true,
		CreditComputed: 1.25, HasCreditComputed: true,
	}, "stop")
	if !st2.hasCredit || st2.credit != 1.25 {
		t.Fatalf("折算值未落到归档: credit=%v/%v", st2.credit, st2.hasCredit)
	}
	if !st2.creditEstimated {
		t.Fatal("折算值必须打 credit_estimated 标（面板据此显示约值）")
	}
}

// TestChatStatNoteUpstreamBody 非流式：从 OpenAI 形态响应体回填 token / credit。
func TestChatStatNoteUpstreamBody(t *testing.T) {
	st := newChatStat(time.Now(), []byte("{\"model\":\"trae:seed_m8\",\"messages\":[]}"), false)
	st.noteUpstreamBody([]byte("{\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":22,\"total_tokens\":33,\"credit\":0.5}}"))
	if st.promptTokens != 11 || st.completionTokens != 22 || st.totalTokens != 33 {
		t.Fatalf("tokens = %d/%d/%d", st.promptTokens, st.completionTokens, st.totalTokens)
	}
	if !st.hasCredit || st.credit != 0.5 {
		t.Fatalf("credit = %v/%v", st.credit, st.hasCredit)
	}
	st.noteUpstreamBody([]byte("not json"))
	if st.promptTokens != 11 {
		t.Fatal("malformed body mutated stats")
	}
}

// TestRequestTraceEventCarriesUpstreamStats 归档事件必须带上账号/模型/token。
func TestRequestTraceEventCarriesUpstreamStats(t *testing.T) {
	body := []byte("{\"model\":\"trae:seed_m8\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}")
	st := newChatStat(time.Now(), body, false)
	st.noteAccount(&auth.Auth{UID: "30000000000000001", Nickname: "traeacct"})
	st.noteStreamUsage(upstreamUsage{PromptTokens: 9, HasPrompt: true, Completion: 5, HasCompletion: true}, "stop")
	st.status = 200
	st.outcome = "success"
	tr := &requestTrace{id: "req-1", start: time.Now(), stat: st}
	e := tr.event(200)
	if e.Account == "" || e.Model != "trae:seed_m8" {
		t.Fatalf("event account/model = %q/%q", e.Account, e.Model)
	}
	if e.PromptTokens != 9 || e.CompletionTokens != 5 || e.TotalTokens != 14 {
		t.Fatalf("event tokens = %d/%d/%d", e.PromptTokens, e.CompletionTokens, e.TotalTokens)
	}
	b, _ := json.Marshal(e)
	if !strings.Contains(string(b), "trae:seed_m8") || !strings.Contains(string(b), "traeacct") {
		t.Fatalf("archived json missing fields: %s", b)
	}
}

// TestNoteUpstreamBodyAnthropicKeys 回归：ZCode 非流式的 res.body 是 Anthropic 原生形态
// （input_tokens / output_tokens / cache_read_input_tokens / cache_creation_input_tokens），
// 没有 prompt_tokens / total_tokens，也没有 credit。旧实现只按 OpenAI 键名解析，导致
// 「响应里明明有 usage，归档却一个 token 字段都没有」（2026-10-07 线上实测）。
//
// 缓存三分（4.3 遗留修复，2026-10-08）：
//
//	旧：hit=cache_read=3，miss=cache_creation=1  ← 把「写入」当「未命中」，且丢掉真未命中
//	新：hit=cache_read=3，miss=input_tokens=10，write=cache_creation=1
//
// 同时总输入按 Anthropic 的加法定律还原：input + read + creation = 14
// （Anthropic 的 input_tokens 不含缓存部分，直接当 prompt_tokens 会偏小）。
func TestNoteUpstreamBodyAnthropicKeys(t *testing.T) {
	st := newChatStat(time.Now(), []byte("{\"model\":\"zcode:glm-4.7\",\"messages\":[]}"), false)
	body := []byte("{\"id\":\"msg_1\",\"type\":\"message\",\"model\":\"glm-4.7\"," +
		"\"usage\":{\"input_tokens\":10,\"output_tokens\":2,\"cache_read_input_tokens\":3,\"cache_creation_input_tokens\":1}}")
	st.noteUpstreamBody(body)
	if st.promptTokens != 14 {
		t.Fatalf("promptTokens = %d, want 14 (input 10 + read 3 + creation 1)", st.promptTokens)
	}
	if st.completionTokens != 2 || st.toks != 2 {
		t.Fatalf("completionTokens/toks = %d/%d, want 2/2", st.completionTokens, st.toks)
	}
	// Anthropic 无 total_tokens 字段 -> 按 prompt+completion 兜底。
	if st.totalTokens != 16 {
		t.Fatalf("totalTokens = %d, want 16", st.totalTokens)
	}
	if !st.hasCache || st.cacheHit != 3 || st.cacheMiss != 10 {
		t.Fatalf("cache = %v/%d/%d, want hit=3 miss=10（input_tokens 才是真未命中）",
			st.hasCache, st.cacheHit, st.cacheMiss)
	}
	if !st.hasCacheWrite || st.cacheWrite != 1 {
		t.Fatalf("cacheWrite = %v/%d, want 1", st.hasCacheWrite, st.cacheWrite)
	}
	// 对照：只有 input_tokens、无缓存字段时不声称有缓存（同解析层口径）。
	stOnly := newChatStat(time.Now(), []byte("{\"model\":\"zcode:x\",\"messages\":[]}"), false)
	stOnly.noteUpstreamBody([]byte("{\"usage\":{\"input_tokens\":9,\"output_tokens\":1}}"))
	if stOnly.hasCache {
		t.Fatalf("无缓存字段时不应声称有缓存，实际 hit=%d miss=%d", stOnly.cacheHit, stOnly.cacheMiss)
	}
	// ZCode 上游不给 credit -> 不伪造 0。
	if st.hasCredit {
		t.Fatalf("credit should stay unknown, got %v", st.credit)
	}
}

// TestParseZCodeUsageCacheThreeWay 解析层同样要三分（流式走这条）。
func TestParseZCodeUsageCacheThreeWay(t *testing.T) {
	u := parseZCodeUsage(json.RawMessage(`{"input_tokens":10,"output_tokens":2,` +
		`"cache_read_input_tokens":300,"cache_creation_input_tokens":1000}`))
	if !u.HasPrompt || u.PromptTokens != 1310 {
		t.Fatalf("prompt = %v/%d, want true/1310", u.HasPrompt, u.PromptTokens)
	}
	if !u.HasCache || u.CacheHit != 300 || u.CacheMiss != 10 {
		t.Fatalf("cache = %v/%d/%d, want hit=300 miss=10", u.HasCache, u.CacheHit, u.CacheMiss)
	}
	if !u.HasCacheWrite || u.CacheWrite != 1000 {
		t.Fatalf("cacheWrite = %v/%d, want 1000", u.HasCacheWrite, u.CacheWrite)
	}
	// 只有 input_tokens（上游没回任何缓存字段）时**不得**声称「有缓存维度」：
	// 那是全价输入，不是「整段未命中缓存」。旧实现把它当 CacheMiss 会让命中率
	// 分母凭空多一块，且 cacheMiss 信号（cache_stats.go 的"整段未命中"告警）
	// 会对每个不支持缓存的模型误报。
	only := parseZCodeUsage(json.RawMessage(`{"input_tokens":7,"output_tokens":1}`))
	if only.HasCache {
		t.Fatalf("无缓存字段时不应声称有缓存，实际 hit=%d miss=%d", only.CacheHit, only.CacheMiss)
	}
	if only.HasCacheWrite {
		t.Fatal("无 cache_creation 字段时不应声称有写入")
	}
	if !only.HasPrompt || only.PromptTokens != 7 {
		t.Fatalf("prompt = %v/%d, want true/7", only.HasPrompt, only.PromptTokens)
	}
}

// TestNoteUpstreamBodyOpenAITotalWins 当两套键名同时出现时，优先采信显式 total_tokens。
func TestNoteUpstreamBodyOpenAITotalWins(t *testing.T) {
	st := newChatStat(time.Now(), []byte("{\"model\":\"trae:x\",\"messages\":[]}"), false)
	st.noteUpstreamBody([]byte("{\"usage\":{\"input_tokens\":10,\"output_tokens\":2,\"total_tokens\":99}}"))
	if st.promptTokens != 10 || st.completionTokens != 2 {
		t.Fatalf("tokens = %d/%d", st.promptTokens, st.completionTokens)
	}
	if st.totalTokens != 99 {
		t.Fatalf("totalTokens = %d, want 99", st.totalTokens)
	}
}
