package server

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jjjane233/workbuddy-3in1/internal/auth"
	"github.com/jjjane233/workbuddy-3in1/internal/pool"
	"github.com/jjjane233/workbuddy-3in1/internal/usage"
)

// 三合一网关上游记账的单元测试。全部离线（不联网），只验证「上游给什么、
// 我们记什么」——这是 Trae / ZCode 在用量页能否出现的前提。

// TestParseZCodeUsage 覆盖 Anthropic usage 的三种形态：全字段、只有 input、
// 只有 output（流式分帧的常态），以及缓存字段映射。
func TestParseZCodeUsage(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want upstreamUsage
	}{
		{
			// Anthropic 三分（4.3 遗留修复，2026-10-08）：
			//   PromptTokens = input + read + creation = 120 + 100 + 7 = 227
			//   CacheHit  = cache_read（命中）
			//   CacheMiss = input_tokens（**真未命中**；旧实现错把它丢了、拿 creation 顶）
			//   CacheWrite= cache_creation（写入缓存，独立维度，不进命中率）
			name: "full",
			raw:  `{"input_tokens":120,"output_tokens":34,"cache_read_input_tokens":100,"cache_creation_input_tokens":7}`,
			want: upstreamUsage{PromptTokens: 227, HasPrompt: true, Completion: 34, HasCompletion: true,
				CacheHit: 100, CacheMiss: 120, HasCache: true, CacheWrite: 7, HasCacheWrite: true},
		},
		{
			name: "input-only",
			raw:  `{"input_tokens":120}`,
			want: upstreamUsage{PromptTokens: 120, HasPrompt: true},
		},
		{
			name: "output-only",
			raw:  `{"output_tokens":0}`,
			want: upstreamUsage{Completion: 0, HasCompletion: true},
		},
		{name: "empty", raw: ``, want: upstreamUsage{}},
		{name: "garbage", raw: `not-json`, want: upstreamUsage{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseZCodeUsage(json.RawMessage(c.raw))
			if got != c.want {
				t.Fatalf("parseZCodeUsage(%s) = %+v, want %+v", c.raw, got, c.want)
			}
		})
	}
}

// TestMergeZCodeUsage 验证流式分帧合并：input 先到、output 后到（且被后续帧刷新），
// 缺席的字段不覆盖已有值。
func TestMergeZCodeUsage(t *testing.T) {
	var got upstreamUsage
	got = mergeZCodeUsage(got, upstreamUsage{PromptTokens: 10, HasPrompt: true})
	got = mergeZCodeUsage(got, upstreamUsage{Completion: 5, HasCompletion: true})
	got = mergeZCodeUsage(got, upstreamUsage{Completion: 9, HasCompletion: true, CacheHit: 3, HasCache: true})
	want := upstreamUsage{PromptTokens: 10, HasPrompt: true, Completion: 9, HasCompletion: true, CacheHit: 3, HasCache: true}
	if got != want {
		t.Fatalf("merged = %+v, want %+v", got, want)
	}
}

// TestParseTraeUsage 覆盖平铺与 data 包裹两种承载，以及数值为浮点的降级。
func TestParseTraeUsage(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want upstreamUsage
	}{
		{name: "flat", raw: `{"prompt_tokens":11,"completion_tokens":22,"total_tokens":33}`,
			want: upstreamUsage{PromptTokens: 11, HasPrompt: true, Completion: 22, HasCompletion: true, Total: 33, HasTotal: true}},
		{name: "wrapped", raw: `{"data":{"prompt_tokens":11,"completion_tokens":22,"total_tokens":33}}`,
			want: upstreamUsage{PromptTokens: 11, HasPrompt: true, Completion: 22, HasCompletion: true, Total: 33, HasTotal: true}},
		{name: "float", raw: `{"prompt_tokens":11.0,"total_tokens":33.0}`,
			want: upstreamUsage{PromptTokens: 11, HasPrompt: true, Total: 33, HasTotal: true}},
		{name: "alt-keys", raw: `{"input_tokens":7,"output_tokens":8}`,
			want: upstreamUsage{PromptTokens: 7, HasPrompt: true, Completion: 8, HasCompletion: true}},
		{name: "empty", raw: `{}`, want: upstreamUsage{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseTraeUsage(c.raw); got != c.want {
				t.Fatalf("parseTraeUsage(%s) = %+v, want %+v", c.raw, got, c.want)
			}
		})
	}
}

// TestTraeUsageEvent 验证 usage 帧识别：事件名命中、载荷特征命中、正文帧不误判。
func TestTraeUsageEvent(t *testing.T) {
	if !traeUsageEvent("token_usage", `{"prompt_tokens":1}`) {
		t.Error("event name should match")
	}
	if !traeUsageEvent("", `{"total_tokens":9}`) {
		t.Error("payload heuristic should match")
	}
	if traeUsageEvent("output", `{"response":"你好"}`) {
		t.Error("content frame must not be treated as usage")
	}
}

// TestUpstreamUsageTotals 验证 total 兜底与 any() 判定（决定成功/失败样本口径）。
func TestUpstreamUsageTotals(t *testing.T) {
	u := upstreamUsage{PromptTokens: 10, HasPrompt: true, Completion: 4, HasCompletion: true}
	if v, ok := u.total(); !ok || v != 14 {
		t.Fatalf("total() = %d,%v want 14,true", v, ok)
	}
	if !u.any() {
		t.Fatal("any() should be true with prompt+completion")
	}
	if _, ok := (upstreamUsage{}).total(); ok {
		t.Fatal("empty usage must not report a total")
	}
}

// TestRecordUpstreamUsageLandsInProviderDimension 是本组的核心断言：
// 一次上游尝试必须同时进「池内累计器」与「用量台账的 provider 维度」，
// 且 provider 取自账号本身（trae），不是调用方传参。
func TestRecordUpstreamUsageLandsInProviderDimension(t *testing.T) {
	rec := usage.New("")
	p := pool.New("")
	a := &auth.Auth{UID: "trae-1", Provider: auth.ProviderTrae, AccessToken: "at", ExpiresAt: 9999999999}
	p.Add(a)

	h := NewHandler(Config{Pool: p, Usage: rec})
	h.recordUpstreamUsage(a, "seed_m8", upstreamUsage{
		PromptTokens: 100, HasPrompt: true, Completion: 40, HasCompletion: true,
	}, 2*time.Second, true)

	// 池内累计器（面板账号行的 Token 列）。
	st, ok := p.Status("trae-1")
	if !ok {
		t.Fatal("account missing from pool")
	}
	if st.TokenUsage.RequestCount != 1 || st.TokenUsage.PromptTokens != 100 ||
		st.TokenUsage.CompletionTokens != 40 || st.TokenUsage.TotalTokens != 140 {
		t.Fatalf("pool token usage = %+v", st.TokenUsage)
	}

	// 用量台账（用量页的 provider 维度）。
	snap := rec.Snapshot(24, nil)
	if snap.Totals.Requests != 1 || snap.Totals.TotalTokens != 140 {
		t.Fatalf("totals = %+v", snap.Totals)
	}
	if len(snap.ByProvider) != 1 || snap.ByProvider[0].Key != auth.ProviderTrae {
		t.Fatalf("by_provider = %+v, want single trae row", snap.ByProvider)
	}
	if snap.ByProvider[0].TotalTokens != 140 {
		t.Fatalf("trae row tokens = %d, want 140", snap.ByProvider[0].TotalTokens)
	}
	if len(snap.ByAccount) != 1 || snap.ByAccount[0].Provider != auth.ProviderTrae {
		t.Fatalf("by_account = %+v, want provider=trae", snap.ByAccount)
	}
}

// TestRecordUpstreamUsageCountsFailure 验证失败尝试也计数（无 usage 时），
// 这是用量页「失败尝试」列与重试放大的可见性来源。
func TestRecordUpstreamUsageCountsFailure(t *testing.T) {
	rec := usage.New("")
	p := pool.New("")
	a := &auth.Auth{UID: "zc-1", Provider: auth.ProviderZCode, AccessToken: "at", ExpiresAt: 9999999999}
	p.Add(a)

	h := NewHandler(Config{Pool: p, Usage: rec})
	h.recordUpstreamUsage(a, "GLM-5.2", upstreamUsage{}, time.Second, false)

	snap := rec.Snapshot(24, nil)
	if snap.Totals.Requests != 1 || snap.Totals.Errors != 1 {
		t.Fatalf("totals = %+v, want 1 request 1 error", snap.Totals)
	}
	if len(snap.ByProvider) != 1 || snap.ByProvider[0].Key != auth.ProviderZCode || snap.ByProvider[0].Errors != 1 {
		t.Fatalf("by_provider = %+v", snap.ByProvider)
	}
}

// TestRecordUpstreamUsageZeroTokensStillSucceeds 验证「上游明确回 0 个 completion」
// 仍算成功样本——Has* 与零值的区别必须保住，否则空回复会被误记成失败。
func TestRecordUpstreamUsageZeroTokensStillSucceeds(t *testing.T) {
	rec := usage.New("")
	p := pool.New("")
	a := &auth.Auth{UID: "zc-2", Provider: auth.ProviderZCode, AccessToken: "at", ExpiresAt: 9999999999}
	p.Add(a)

	h := NewHandler(Config{Pool: p, Usage: rec})
	h.recordUpstreamUsage(a, "GLM-5.2", upstreamUsage{PromptTokens: 5, HasPrompt: true, Completion: 0, HasCompletion: true},
		500*time.Millisecond, true)

	snap := rec.Snapshot(24, nil)
	if snap.Totals.Errors != 0 || snap.Totals.Requests != 1 || snap.Totals.TotalTokens != 5 {
		t.Fatalf("totals = %+v, want 1 request 0 errors 5 tokens", snap.Totals)
	}
}

// TestStreamTraeSkipsUsageFrame 验证 usage 帧不会被当成正文吐给客户端：
// 转换后的 OpenAI SSE 里不应出现 "prompt_tokens"，但正文与 [DONE] 必须保留。
func TestStreamTraeSkipsUsageFrame(t *testing.T) {
	raw := strings.Join([]string{
		`event: output`,
		`data: {"response":"你好"}`,
		`event: token_usage`,
		`data: {"prompt_tokens":11,"completion_tokens":22,"total_tokens":33}`,
		`data: [DONE]`,
		"",
	}, "\n")

	rr := httptest.NewRecorder()
	res := &upstreamResult{streaming: true, stream: io.NopCloser(strings.NewReader(raw))}
	got := (&Handler{}).streamTraeUpstream(rr, res, "trae/seed_m8", nil)

	body := rr.Body.String()
	if strings.Contains(body, "prompt_tokens") {
		t.Fatalf("usage frame leaked into client stream: %s", body)
	}
	if !strings.Contains(body, "你好") || !strings.Contains(body, "[DONE]") {
		t.Fatalf("content or terminator missing: %s", body)
	}
	if got.PromptTokens != 11 || !got.HasPrompt || got.Completion != 22 || got.Total != 33 {
		t.Fatalf("observed usage = %+v", got)
	}
}

func TestTraeQueueEvent(t *testing.T) {
	for _, tc := range []struct {
		event, data string
		want        bool
	}{
		{"queue", `{"status":"waiting"}`, true},
		{"progress_notice", `{"message":"request queued, position 3"}`, true},
		{"progress_notice", `{"content":"Building prompt"}`, false},
		{"output", `{"response":"OK"}`, false},
	} {
		if got := traeQueueEvent(tc.event, tc.data); got != tc.want {
			t.Errorf("traeQueueEvent(%q, %q)=%v, want %v", tc.event, tc.data, got, tc.want)
		}
	}
}

func TestPreflightTraeStreamRotatesQueuedResponse(t *testing.T) {
	raw := strings.NewReader("event: progress_notice\ndata: {\"message\":\"request queued\"}\n")
	queued, out := preflightTraeStream(io.NopCloser(raw))
	if !queued {
		t.Fatal("queued response was not detected")
	}
	_ = out.Close()
}

func TestPreflightTraeStreamPreservesFirstOutput(t *testing.T) {
	raw := strings.NewReader("event: output\ndata: {\"response\":\"OK\"}\nevent: done\ndata: {}\n")
	queued, out := preflightTraeStream(io.NopCloser(raw))
	if queued {
		t.Fatal("normal output was classified as queued")
	}
	b, err := io.ReadAll(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != rawInputOutputForTest() {
		t.Fatalf("prefetched stream changed: %q", b)
	}
	_ = out.Close()
}

func rawInputOutputForTest() string {
	return "event: output\ndata: {\"response\":\"OK\"}\nevent: done\ndata: {}\n"
}

// TestTraeDeltaOnlyAcceptsOutputFrames 回归「Building prompt 泄漏进正文」：
// 旧实现对任意 data 帧做 content/text 兜底，于是 progress_notice 帧
// {"name":"construct_prompt","content":"Building prompt: ide_prompts"}
// 的 content 被当成回复正文吐给客户端，每次回复开头都挂着这句上游进度提示。
// 新实现只认 response / reasoning_content 两个字段，控制帧一律返回 ok=false。
func TestTraeDeltaOnlyAcceptsOutputFrames(t *testing.T) {
	cases := []struct {
		raw           string
		content, reas string
		ok            bool
	}{
		// 真实 output 帧（线上抓包，2026-10-07）
		{`{"response":"OK","reasoning_content":"直接输出","tool_calls":null,"multimodal_contents":null,"phase":null}`, "OK", "直接输出", true},
		// 真实 token_usage 帧：没有 response / reasoning_content
		{`{"name":"ide_prompts","prompt_tokens":95,"completion_tokens":49,"total_tokens":144}`, "", "", false},
		// 真实 progress_notice 帧：content 字段**必须不被当成正文**
		{`{"name":"construct_prompt","content":"Building prompt: ide_prompts"}`, "", "", false},
		{`{"name":"construct_prompt","content":"Completed building prompt..."}`, "", "", false},
		// metadata / prompt 控制帧
		{`{"model":"","session_id":"3c5c","app_version":"default"}`, "", "", false},
		{`{"name":"prompt","string":null,"messages":null}`, "", "", false},
		{`{"finish_reason":"stop"}`, "", "", false},
		// 空 response 帧是合法的（只有思考链）
		{`{"response":"","reasoning_content":"用户"}`, "", "用户", true},
		{`not json`, "", "", false},
	}
	for _, c := range cases {
		content, reas, ok := traeDelta(c.raw)
		if ok != c.ok || content != c.content || reas != c.reas {
			t.Errorf("traeDelta(%s) = (%q,%q,%v), want (%q,%q,%v)",
				c.raw, content, reas, ok, c.content, c.reas, c.ok)
		}
	}
}

// TestStreamTraeDropsProgressNoticeFrames 端到端回归：整条流里 progress_notice
// 与 metadata 不得出现在客户端收到的正文中，只留 output 的 response。
func TestStreamTraeDropsProgressNoticeFrames(t *testing.T) {
	raw := strings.Join([]string{
		`event: progress_notice`,
		`data: {"name":"construct_prompt","content":"Building prompt: ide_prompts"}`,
		`event: progress_notice`,
		`data: {"name":"construct_prompt","content":"Completed building prompt..."}`,
		`event: prompt`,
		`data: {"name":"prompt","string":null,"messages":null}`,
		`event: metadata`,
		`data: {"model":"","session_id":"3c5c","app_version":"default"}`,
		`event: output`,
		`data: {"response":"","reasoning_content":"用户只回复OK"}`,
		`event: output`,
		`data: {"response":"OK","reasoning_content":"就可以了。"}`,
		`event: token_usage`,
		`data: {"name":"ide_prompts","prompt_tokens":95,"completion_tokens":49,"total_tokens":144}`,
		`event: done`,
		`data: {"finish_reason":"stop"}`,
		"",
	}, "\n")

	rr := httptest.NewRecorder()
	res := &upstreamResult{streaming: true, stream: io.NopCloser(strings.NewReader(raw))}
	got := (&Handler{}).streamTraeUpstream(rr, res, "trae/seed_m8", nil)
	body := rr.Body.String()

	for _, leak := range []string{"Building prompt", "Completed building prompt", "construct_prompt", "session_id", "ide_prompts", "reasoning_content"} {
		if strings.Contains(body, leak) {
			t.Fatalf("控制帧/思考链泄漏进客户端流 (%q): %s", leak, body)
		}
	}
	if !strings.Contains(body, "OK") || !strings.Contains(body, "[DONE]") {
		t.Fatalf("正文或终止帧缺失: %s", body)
	}
	if got.PromptTokens != 95 || !got.HasPrompt || got.Completion != 49 || got.Total != 144 {
		t.Fatalf("observed usage = %+v", got)
	}
}

// TestCollectTraeUpstreamNonStream 回归「stream:false 必须回 JSON」：
// 上游只有流式，但客户端要非流式时网关要聚合，Content-Type 必须是
// application/json 且体是一条 chat.completion（旧实现无论怎样都回 SSE）。
func TestCollectTraeUpstreamNonStream(t *testing.T) {
	raw := strings.Join([]string{
		`event: progress_notice`,
		`data: {"name":"construct_prompt","content":"Building prompt: ide_prompts"}`,
		`event: output`,
		`data: {"response":"你","reasoning_content":"想"}`,
		`event: output`,
		`data: {"response":"好","reasoning_content":"想好了"}`,
		`event: token_usage`,
		`data: {"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}`,
		`event: done`,
		`data: {"finish_reason":"stop"}`,
		"",
	}, "\n")

	rr := httptest.NewRecorder()
	res := &upstreamResult{streaming: true, stream: io.NopCloser(strings.NewReader(raw))}
	got := (&Handler{}).collectTraeUpstream(rr, res, "trae/seed_m8", nil)

	if ct := rr.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("非流式 Content-Type = %q, want application/json", ct)
	}
	var out struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Role             string `json:"role"`
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("非流式体不是合法 JSON: %v (%s)", err, rr.Body.String())
	}
	if out.Object != "chat.completion" || len(out.Choices) != 1 {
		t.Fatalf("响应结构异常: %+v", out)
	}
	if out.Choices[0].Message.Content != "你好" {
		t.Fatalf("正文 = %q, want %q (不得含 progress_notice)", out.Choices[0].Message.Content, "你好")
	}
	if strings.Contains(out.Choices[0].Message.Content, "Building prompt") {
		t.Fatalf("progress_notice 泄漏进非流式正文: %q", out.Choices[0].Message.Content)
	}
	if out.Usage.PromptTokens != 5 || out.Usage.CompletionTokens != 2 || out.Usage.TotalTokens != 7 {
		t.Fatalf("usage = %+v", out.Usage)
	}
	if got.Completion != 2 || !got.HasCompletion {
		t.Fatalf("observed usage = %+v", got)
	}
}

// TestBuildTraePayloadUsesChatHistory 回归本次修复：上游只认 chat_history +
// user_input + intent_name，发 messages 会拿到兜底话术。
func TestBuildTraePayloadUsesChatHistory(t *testing.T) {
	req := &OAIRequest{Messages: []OAIMessage{
		{Role: "system", Content: json.RawMessage(`"你是助手"`)},
		{Role: "user", Content: json.RawMessage(`"你好"`)},
		{Role: "assistant", Content: json.RawMessage(`"在"`)},
		{Role: "user", Content: json.RawMessage(`"再说一次"`)},
	}}
	var body map[string]any
	if err := json.Unmarshal(buildTraePayload(req, "seed_m8"), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, bad := body["messages"]; bad {
		t.Error("messages must not be sent: upstream ignores it")
	}
	if body["intent_name"] != defaultTraeIntent {
		t.Errorf("intent_name = %v, want %s", body["intent_name"], defaultTraeIntent)
	}
	if body["model_name"] != "seed_m8" {
		t.Errorf("model_name = %v", body["model_name"])
	}
	if body["stream"] != true {
		t.Errorf("stream = %v, want true", body["stream"])
	}
	// user_input 必须是最后一条 user 文本（上游 prompt pipeline 的关键输入）。
	if body["user_input"] != "再说一次" {
		t.Errorf("user_input = %v, want 再说一次", body["user_input"])
	}
	hist, ok := body["chat_history"].([]any)
	if !ok || len(hist) != 3 {
		t.Fatalf("chat_history = %v, want 3 entries (system merged into first user)", body["chat_history"])
	}
	first, _ := hist[0].(map[string]any)
	if first["role"] != "user" || first["content"] != "你是助手\n\n你好" {
		t.Errorf("first entry = %v, want system merged into user", first)
	}
}

// TestBuildTraePayloadPassthroughChatHistory 验证原生直通分支补齐三个必需字段。
func TestBuildTraePayloadPassthroughChatHistory(t *testing.T) {
	req := &OAIRequest{
		ChatHistory: json.RawMessage(`{"chat_history":[{"role":"user","content":"直通"}]}`),
		Model:       "trae/seed_m8",
	}
	var body map[string]any
	if err := json.Unmarshal(buildTraePayload(req, "seed_m8"), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body["intent_name"] != defaultTraeIntent || body["stream"] != true {
		t.Errorf("required fields missing: %v", body)
	}
	if body["user_input"] != "直通" {
		t.Errorf("user_input = %v, want 直通", body["user_input"])
	}
}
