package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jjjane233/workbuddy-3in1/internal/auth"
	"github.com/jjjane233/workbuddy-3in1/internal/pool"
	"github.com/jjjane233/workbuddy-3in1/internal/reqlog"
	"github.com/jjjane233/workbuddy-3in1/internal/trae"
	"github.com/jjjane233/workbuddy-3in1/internal/zcode"
)

// 三合一网关的 Trae / ZCode 出口（Phase 4b）。
//
// CodeBuddy 链路一行未改：chatCompletions 判定 provider 后，非 workbuddy 才转发到这里，
// 因此存量行为完全不受影响。本文件的结构与调用口径对齐 gw2api/internal/server/chat.go，
// 差异只在「池与冷却」换用 workbuddy 自己的 API（见 noteUpstream* 三个适配函数）。

// 上游（Trae/ZCode）单请求最多换号次数的默认值与硬上限。
//
// 默认值历史就是 6（与 gw2api 的 maxAccountAttempts 同值）；上硬上限是为了防止
// 面板误配把单请求放大成无限重试风暴。真正生效的值由 Config.UpstreamMaxAttempts
// 经 livecfg 热改，读取口是 (*Handler).maxUpstreamAttempts()。
const (
	defaultUpstreamMaxAttempts = 6
	maxUpstreamMaxAttempts     = 32
)

// upstreamResult 单次上游尝试的结果。
type upstreamResult struct {
	streaming bool
	// 非流式
	status int
	body   []byte
	// 流式
	stream io.ReadCloser
	// usage 非流式响应的 token 用量（流式由转换函数边写边解析，回填在返回值里）。
	usage upstreamUsage
	// 控制
	retry  bool
	errMsg string
	// cancel 释放本次 Trae 上游请求的 context。排队预检命中时立即调用，
	// 避免上游连接继续占用账号并发槽。
	cancel context.CancelFunc
}

// ---- 池与冷却适配层 ----
//
// workbuddy 的池没有 gw2api 的 CooldownSoft/CooldownHard/SetLastErrMsg：
//   - 软冷却用 Cooldown(CoolSoft, ...)（固定时长，不叠指数）
//   - 硬冷却用 CooldownUntilTomorrow4AM（与 CodeBuddy 的余额耗尽同语义）
//   - 错误详情不进池（本仓库存 reqlog），只记时间戳，避免两套口径漂移

func (h *Handler) noteUpstreamSoft(uid, reason string) {
	h.cfg.Pool.Cooldown(uid, pool.CoolSoft, h.softCooldown(), reason)
}

func (h *Handler) noteUpstreamHard(uid, reason string) {
	h.cfg.Pool.CooldownUntilTomorrow4AM(uid, reason)
}

// ---- 对外入口 ----

// chatUpstreamProvider 把 Trae / ZCode 请求代理到对应上游。
// raw 是原始 body：Trae 的 buildTraePayload 需要它做 chat_history 直通。
func (h *Handler) chatUpstreamProvider(w http.ResponseWriter, r *http.Request, provider, bare string, raw []byte, st *chatStat) {
	var req OAIRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "invalid JSON: "+err.Error())
		return
	}
	// 模型名保持客户端原始串（含 provider 前缀，如 "zcode:glm-4.7"）：newChatStat
	// 已从 body 解析好，这里不再用剥离前缀的 bare 覆盖——否则归档里 trae/zcode 的
	// "glm-4.7" 会与 workbuddy 的同名模型无法区分。
	// 请求体里的 OpenAI stop 在这里解析出来（string 或 []string 都认）。
	// Trae 私有协议没有该字段、WorkBuddy 上游会忽略它，两条链路都由网关侧
	// 自行截断补齐语义（见 stopseq.go）；ZCode 会把它映射成 Anthropic 的
	// stop_sequences 交给上游，网关层只做兜底（同一份 matcher 对已停止的
	// 流是幂等的，不会重复截断）。
	stops := parseStopSequences(req.Stop)
	switch provider {
	case providerTrae:
		h.chatTraeUpstream(w, r, &req, bare, raw, stops, st)
	default:
		h.chatZCodeUpstream(w, r, &req, bare, stops, st)
	}
}

// ---- ZCode（Anthropic Messages 上游，OpenAI 协议对外） ----

func (h *Handler) chatZCodeUpstream(w http.ResponseWriter, r *http.Request, req *OAIRequest, model string, stops []string, st *chatStat) {
	tried := map[string]bool{}
	var lastErr string
	for attempt := 0; attempt < h.maxUpstreamAttempts(); attempt++ {
		acct := h.cfg.Pool.PickExcludingForScope(tried, model, "", providerZCode)
		if acct == nil {
			break
		}
		tried[acct.UID] = true
		if !h.cfg.Pool.Acquire(acct.UID) {
			continue // 在途满：换下一个号
		}
		st.noteAccount(acct)
		started := time.Now()
		res, err := h.tryZCodeOnce(r.Context(), acct, req, model, captureHeaders(r))
		h.cfg.Pool.Release(acct.UID)
		st.attempts++ // 每次真实发出的上游调用都计入，与 CodeBuddy 链路口径一致
		if res == nil {
			// 传输错误没有 usage：计入失败尝试，让用量页看得见重试放大。
			h.recordUpstreamUsage(acct, model, upstreamUsage{}, time.Since(started), false)
			lastErr = errString(err)
			continue
		}
		if res.retry {
			// 401/402/403/429/405 等：同样计为失败尝试（与本仓 CodeBuddy 链路同口径）。
			h.recordUpstreamUsage(acct, model, res.usage, time.Since(started), false)
			lastErr = res.errMsg
			continue
		}
		if res.streaming {
			// 上游（Anthropic 风格）可能不回 stop 语义，网关层再兜一层。
			if len(stops) > 0 {
				res.stream = NewStopFilter(res.stream, stops)
			}
			u := h.streamZCodeUpstream(w, res, model)
			// 一次折算、两处消费（归档 + 台账），与 Trae 链路同构；
			// 详见 zcode_credit_rate.go（ZCode 响应不含 credit，用当日真值分摊）。
			u = zcodeCreditUsage(acct.UID, model, u)
			st.noteStreamUsage(u, "stop")
			st.status = http.StatusOK
			st.outcome = reqlog.OutcomeSuccess
			h.recordUpstreamUsage(acct, model, u, time.Since(started), true)
			return
		}
		res.usage = zcodeCreditUsage(acct.UID, model, res.usage)
		h.recordUpstreamUsage(acct, model, res.usage, time.Since(started), true)
		st.noteUpstreamBody(res.body)
		// 非流式的 token 由上面从响应体解析，折算积分来自 usage 结构，单独并入归档
		// （否则同步请求的 credit 永远是「上游未提供」，只有流式才有值）。
		st.noteCredit(res.usage)
		st.status = http.StatusOK
		st.outcome = reqlog.OutcomeSuccess
		writeJSON(w, http.StatusOK, anthToOAIMessageWithStop(res.body, model, stops))
		return
	}
	// 注意这里的 code 不能一律用 no_available_account。
	//
	// 上游拒绝（405 + code 3012 "unusual activity"）与「账号池空了」
	// 是两回事：账号存在且余额正常，只是对话端点被风控拦了。
	// 用 no_available_account 会让人去狂添账号，而真正的排查方向
	// （客户端签名 / 风控）被盖掉。
	code, msg := "no_available_account", "no available zcode account: "+lastErr
	if zcode.IsBlocked(lastErr) {
		code = "upstream_blocked"
		msg = "上游拒绝了本次请求（风控/客户端签名）：" + lastErr
	} else if zcode.IsModelNotEntitled(lastErr) {
		code = "model_not_entitled"
		msg = "该模型不在本账号套餐授权范围内：" + lastErr
	}
	st.status = http.StatusServiceUnavailable
	st.outcome = reqlog.OutcomeHTTPError
	writeOpenAIError(w, http.StatusServiceUnavailable, code, msg)
}

func (h *Handler) tryZCodeOnce(ctx context.Context, a *auth.Auth, req *OAIRequest, model string, incoming http.Header) (*upstreamResult, error) {
	cli := zcode.New(a, zcode.Options{Timeout: 5 * time.Minute})

	var verifyParam string
	if h.cfg.Captcha != nil {
		if vp, err := h.cfg.Captcha.Get(ctx); err == nil {
			verifyParam = vp
		}
	}

	creq := oaiToZCodeRequest(req, model)
	resp, err := cli.Chat(ctx, creq, verifyParam, incoming)
	if err != nil {
		h.cfg.Pool.NoteError(a.UID)
		h.noteUpstreamSoft(a.UID, "zcode chat error: "+err.Error())
		return nil, err
	}
	// zcode.z.ai 的 zcode-plan 对话端点会对非官方客户端回 405 + 3012
	// 「unusual activity」，且与签名/模型/头部无关（2026-10-06 全矩阵实测）。
	// 此时别把 3012 直接甩给用户：改用同一把凭据走 BigModel/Z.AI 的
	// Anthropic 端点（实测 200 出正文），并记录是哪条通路成功的。
	if resp.StatusCode == http.StatusMethodNotAllowed || resp.StatusCode == http.StatusNotAcceptable {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		resp.Body.Close()
		if zcode.IsBlocked(string(body)) {
			if key := zcodeFallbackKey(a); key != "" {
				if alt, aerr := cli.ChatViaKey(ctx, creq, key, a.Source == "zai"); aerr == nil && alt != nil {
					h.noteUpstreamSoft(a.UID, "zcode 3012 -> anthropic fallback ok")
					return h.finishZCodeResponse(ctx, a, req, creq, alt), nil
				} else if aerr != nil {
					h.noteUpstreamSoft(a.UID, "zcode 3012 fallback failed: "+aerr.Error())
				}
			}
		}
		return &upstreamResult{retry: true, errMsg: "zcode 405 blocked: " + truncate(string(body), 200)}, nil
	}

	switch resp.StatusCode {
	case http.StatusOK:
		h.cfg.Pool.NoteSuccess(a.UID)
	case http.StatusUnauthorized:
		h.noteUpstreamHard(a.UID, "401 未授权")
		resp.Body.Close()
		return &upstreamResult{retry: true, errMsg: "zcode 401 unauthorized"}, nil
	case http.StatusPaymentRequired:
		h.noteUpstreamHard(a.UID, "402 额度不足")
		resp.Body.Close()
		return &upstreamResult{retry: true, errMsg: "zcode 402 insufficient"}, nil
	case http.StatusForbidden:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		resp.Body.Close()
		b := string(body)
		if zcode.IsCaptchaError(b) {
			if h.cfg.Captcha != nil {
				h.cfg.Captcha.Invalidate()
			}
			return &upstreamResult{retry: true, errMsg: "zcode captcha rejected"}, nil
		}
		h.noteUpstreamHard(a.UID, "403 拒绝")
		return &upstreamResult{retry: true, errMsg: "zcode 403: " + truncate(b, 200)}, nil
	case http.StatusNotAcceptable:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		resp.Body.Close()
		h.noteUpstreamSoft(a.UID, "405 需客户端签名")
		return &upstreamResult{retry: true, errMsg: "zcode 405 blocked: " + truncate(string(body), 200)}, nil
	case http.StatusTooManyRequests:
		h.noteUpstreamSoft(a.UID, "429 限流")
		resp.Body.Close()
		return &upstreamResult{retry: true, errMsg: "zcode 429 rate limited"}, nil
	}

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		resp.Body.Close()
		h.cfg.Pool.NoteError(a.UID)
		return &upstreamResult{retry: true,
			errMsg: fmt.Sprintf("zcode http %d: %s", resp.StatusCode, truncate(string(body), 200))}, nil
	}

	if req.Stream {
		return &upstreamResult{streaming: true, stream: resp.Body}, nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	resp.Body.Close()
	return &upstreamResult{status: http.StatusOK, body: body, usage: usageFromZCodeBody(body)}, nil
}

// zcodeFallbackKey 返回可用于 BigModel / Z.AI Anthropic 端点的 key。
//
// 优先级：
//
//  1. SignCredential —— "<keyId>.<secret>"，实测在 open.bigmodel.cn /
//     api.z.ai 的 /api/anthropic/v1/messages 上直接 200；
//  2. APIKey —— 面板手填的 api_key（同样是 key 形态）。
//
// 返回空串表示两条路都没有可用凭据，此时调用方应把原始 3012 透出。
func zcodeFallbackKey(a *auth.Auth) string {
	if a == nil {
		return ""
	}
	if k := a.SignCredential(); k != "" {
		return k
	}
	return a.APIKeyValue()
}

// finishZCodeResponse 把 fallback 拿到的 Anthropic 响应整理成 upstreamResult。
func (h *Handler) finishZCodeResponse(ctx context.Context, a *auth.Auth, req *OAIRequest,
	creq *zcode.ChatRequest, resp *http.Response) *upstreamResult {
	defer resp.Body.Close()
	h.cfg.Pool.NoteSuccess(a.UID)
	if req.Stream {
		return &upstreamResult{streaming: true, stream: resp.Body}
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	return &upstreamResult{status: http.StatusOK, body: body, usage: usageFromZCodeBody(body)}
}

// usageFromZCodeBody 从 Anthropic Messages 非流式响应体取 usage。
func usageFromZCodeBody(body []byte) upstreamUsage {
	var wrap struct {
		Usage json.RawMessage `json:"usage"`
	}
	if json.Unmarshal(body, &wrap) != nil {
		return upstreamUsage{}
	}
	return parseZCodeUsage(wrap.Usage)
}

// streamZCodeUpstream 将 Anthropic SSE 转换为 OpenAI SSE 并写回客户端。
// 返回本次流观测到的 usage（Anthropic 把 input 放在 message_start、
// output 放在末尾 message_delta，必须整段读完才能拿到终值）。
func (h *Handler) streamZCodeUpstream(w http.ResponseWriter, res *upstreamResult, model string) upstreamUsage {
	defer res.stream.Close()
	flusher := startOpenAIStream(w)
	id := "chatcmpl-" + newID("")
	created := time.Now().Unix()

	sc := bufio.NewScanner(res.stream)
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
	sentRole := false
	var got upstreamUsage
	// Anthropic 的 tool_use 分片：content_block_start 给 id/name，紧随的一串
	// input_json_delta 拼出 JSON 参数，content_block_stop 收口。按块下标累积。
	type pendingTool struct {
		id   string
		name string
		args strings.Builder
	}
	tools := map[int]*pendingTool{}
	var toolOrder []int
	finish := "stop"
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var ev map[string]any
		if json.Unmarshal([]byte(data), &ev) != nil {
			continue
		}
		// usage 帧：message_start.message.usage / message_delta.usage。
		// 两处都取、后帧覆盖前帧（Anthropic 的 output_tokens 会随生成递增）。
		var ue struct {
			Usage   json.RawMessage `json:"usage"`
			Message struct {
				Usage json.RawMessage `json:"usage"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(data), &ue) == nil {
			if len(ue.Message.Usage) > 0 {
				got = mergeZCodeUsage(got, parseZCodeUsage(ue.Message.Usage))
			}
			if len(ue.Usage) > 0 {
				got = mergeZCodeUsage(got, parseZCodeUsage(ue.Usage))
			}
		}
		typ, _ := ev["type"].(string)
		switch typ {
		case "content_block_start":
			idx := asInt(ev["index"])
			blk, _ := ev["content_block"].(map[string]any)
			if bt, _ := blk["type"].(string); bt == "tool_use" {
				pt := &pendingTool{}
				pt.id, _ = blk["id"].(string)
				pt.name, _ = blk["name"].(string)
				// 有些上游会在 start 帧直接给完整 input，先塞进去。
				if in, ok := blk["input"]; ok && in != nil {
					if raw, err := json.Marshal(in); err == nil && string(raw) != "{}" && string(raw) != "null" {
						pt.args.Write(raw)
					}
				}
				tools[idx] = pt
				toolOrder = append(toolOrder, idx)
			}
		case "content_block_delta":
			delta, _ := ev["delta"].(map[string]any)
			dt, _ := delta["type"].(string)
			if dt == "input_json_delta" {
				idx := asInt(ev["index"])
				if pt := tools[idx]; pt != nil {
					if pj, ok := delta["partial_json"].(string); ok {
						pt.args.WriteString(pj)
					}
				}
				continue
			}
			text, _ := delta["text"].(string)
			if text == "" {
				continue
			}
			writeSSE(w, OAIChunk{ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
				Choices: []OAIChoice{{Index: 0, Delta: &OAIDelta{Role: roleOnce(&sentRole), Content: text}}}})
			if flusher != nil {
				flusher.Flush()
			}
		case "message_delta":
			if d, ok := ev["delta"].(map[string]any); ok {
				if sr, _ := d["stop_reason"].(string); sr != "" {
					switch sr {
					case "max_tokens":
						finish = "length"
					case "tool_use":
						finish = "tool_calls"
					default:
						finish = "stop"
					}
				}
			}
		}
	}
	// 收口：把累积到的 tool_use 块转成 OpenAI 的 tool_calls 增量帧。
	if len(toolOrder) > 0 {
		for i, idx := range toolOrder {
			pt := tools[idx]
			if pt == nil || pt.name == "" {
				continue
			}
			args := strings.TrimSpace(pt.args.String())
			if args == "" {
				args = "{}"
			}
			cid := pt.id
			if cid == "" {
				cid = "call_" + newID("")
			}
			callIdx := i
			emitToolCallChunks(w, flusher, id, model, created, &sentRole, []oaiToolCall{{
				Index: &callIdx,
				ID:    cid,
				Type:  "function",
				Function: oaiToolCallFnSpec{
					Name:      pt.name,
					Arguments: args,
				},
			}})
		}
		if finish == "stop" {
			finish = "tool_calls"
		}
	}
	writeSSE(w, OAIChunk{ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
		Choices: []OAIChoice{{Index: 0, Delta: &OAIDelta{}, FinishReason: &finish}}})
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
	return got
}

// ---- Trae（SSE 上游，OpenAI 协议对外） ----

func (h *Handler) chatTraeUpstream(w http.ResponseWriter, r *http.Request, req *OAIRequest, model string, raw []byte, stops []string, st *chatStat) {
	tried := map[string]bool{}
	var lastErr string
	for attempt := 0; attempt < h.maxUpstreamAttempts(); attempt++ {
		acct := h.cfg.Pool.PickExcludingForScope(tried, model, "", providerTrae)
		if acct == nil {
			break
		}
		tried[acct.UID] = true
		if !h.cfg.Pool.Acquire(acct.UID) {
			continue
		}
		st.noteAccount(acct)
		started := time.Now()
		res, err := h.tryTraeOnce(r.Context(), acct, req, model, raw)
		h.cfg.Pool.Release(acct.UID)
		st.attempts++
		if res == nil {
			// Trae 是纯流式上游：建连失败即无 usage，计为失败尝试。
			h.recordUpstreamUsage(acct, model, upstreamUsage{}, time.Since(started), false)
			lastErr = errString(err)
			continue
		}
		if res.retry {
			if res.cancel != nil {
				res.cancel()
			}
			h.recordUpstreamUsage(acct, model, res.usage, time.Since(started), false)
			lastErr = res.errMsg
			continue
		}
		// 上游只有流式，但客户端要什么协议就给什么协议：
		//   stream:true  → 逐帧转成 OpenAI SSE；
		//   stream:false → 网关侧聚合后回一条 OpenAI 非流式 JSON。
		// 旧实现无论客户端要什么一律回 SSE，Content-Type 也是 text/event-stream，
		// 于是 stream:false 的调用方（IDE 插件 / agent / 批处理脚本）拿到流式体
		// 后解析失败——这是「Trae 接口不能用」最直接的一条。
		var u upstreamUsage
		if res.streaming {
			if req.Stream {
				u = h.streamTraeUpstream(w, res, model, stops)
			} else {
				u = h.collectTraeUpstream(w, res, model, stops)
			}
		}
		if res.cancel != nil {
			res.cancel()
		}
		// 一次折算、两处消费：归档（noteStreamUsage）与台账（recordUpstreamUsage）
		// 共用同一个估算值，避免两边分别算出现漂移。
		u = traeCreditUsage(model, u, true)
		h.recordUpstreamUsage(acct, model, u, time.Since(started), true)
		st.noteStreamUsage(u, "stop")
		st.status = http.StatusOK
		st.outcome = reqlog.OutcomeSuccess
		return
	}
	// 错误分类（与 traecode2api 口径一致）：模型不存在 / 权益不足要给出可诊断的
	// 状态码，不能一律包成 503 no_available_account —— 否则用户会去狂添账号，
	// 而真正的问题（模型名打错 / 该模型不在账号套餐里）被盖掉。
	if isTraeModelNotFound(lastErr) {
		st.status = http.StatusNotFound
		st.outcome = reqlog.OutcomeHTTPError
		writeOpenAIError(w, http.StatusNotFound, "model_not_found",
			"trae model is not available for this account: "+lastErr)
		return
	}
	if isTraeEntitlementError(lastErr) {
		st.status = http.StatusForbidden
		st.outcome = reqlog.OutcomeHTTPError
		writeOpenAIError(w, http.StatusForbidden, "model_not_entitled",
			"trae model endpoint is not enabled for this account: "+lastErr)
		return
	}
	st.status = http.StatusServiceUnavailable
	st.outcome = reqlog.OutcomeHTTPError
	writeOpenAIError(w, http.StatusServiceUnavailable, "no_available_account",
		"no available trae account: "+lastErr)
}

// isTraeModelNotFound 识别上游明确的模型不存在/未登记错误（4023 / model is unknown）。
func isTraeModelNotFound(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "4023") || strings.Contains(m, "model is unknown") ||
		strings.Contains(m, "unknown model") || strings.Contains(m, "model not found")
}

// isTraeEntitlementError 识别端点或账号权益不足（4011 / 4001）。
func isTraeEntitlementError(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "4011") || strings.Contains(m, "4001") ||
		strings.Contains(m, "not entitled") || strings.Contains(m, "config lookup")
}

func (h *Handler) tryTraeOnce(ctx context.Context, a *auth.Auth, req *OAIRequest, model string, raw []byte) (*upstreamResult, error) {
	attemptCtx, cancel := context.WithCancel(ctx)
	cli := trae.New(a, trae.Options{})
	// 凭证过期则先换票（ExchangeToken 滚动刷新），失败不阻断——上游会用 401 回我们。
	_ = cli.EnsureFreshToken(attemptCtx, a, 10*time.Minute)
	// DLL 内置模型（glm-5.3-flashx / deepseek-v4.1-flash / step-5-preview 等）
	// 走独立的 SOLO 链 llm_utils_chat：/api/ide/v1/chat 不认这些 model_name
	// （实测 code 4023 model is unknown）。模型名原样透传，不降级 seed_m8。
	var dataCh <-chan string
	var errCh <-chan error
	var err error
	if useSoloModelChain(model) {
		dataCh, errCh, err = cli.SoloChatStream(attemptCtx, buildSoloPayload(req, model), nil)
	} else {
		dataCh, errCh, err = cli.ChatStream(attemptCtx, buildTraePayload(req, model), nil)
	}
	if err != nil {
		cancel()
		h.cfg.Pool.NoteError(a.UID)
		h.noteUpstreamSoft(a.UID, "trae chat error: "+err.Error())
		return nil, err
	}
	res := &upstreamResult{streaming: true, stream: &chanReader{ch: dataCh, err: errCh}, cancel: cancel}
	if queued, reader := preflightTraeStream(res.stream); queued {
		_ = reader.Close()
		h.noteUpstreamSoft(a.UID, "trae request queued; rotate account")
		return &upstreamResult{retry: true, errMsg: "trae request queued", cancel: cancel}, nil
	} else {
		res.stream = reader
	}
	return res, nil
}

// preflightTraeStream 读取 Trae SSE 的首段，直到看到首个 output 或明确的排队状态。
// 排队中的免费账号会先返回 progress/queue 事件并保持连接；若直接交给下游，
// 请求会长时间占住该账号。命中排队后返回 queued=true，由外层轮换账号。
// 已读取的首段会通过 prefixReadCloser 放回，正常 output 不会丢失。
func preflightTraeStream(src io.ReadCloser) (queued bool, out io.ReadCloser) {
	br := bufio.NewReaderSize(src, 64*1024)
	var prefix bytes.Buffer
	event := ""
	for {
		line, err := br.ReadString('\n')
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if err != nil && line == "" {
			return false, &prefixReadCloser{Reader: io.MultiReader(bytes.NewReader(prefix.Bytes()), br), src: src}
		}
		prefix.WriteString(line)
		prefix.WriteByte('\n')
		if strings.HasPrefix(line, "event:") {
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			if traeQueueEvent(event, "") {
				return true, src
			}
			continue
		}
		if !strings.HasPrefix(line, "data:") && !strings.HasPrefix(line, "{") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if event == "output" || event == "done" || traeUsageEvent(event, data) || traeDeltaAvailable(data) {
			return false, &prefixReadCloser{Reader: io.MultiReader(bytes.NewReader(prefix.Bytes()), br), src: src}
		}
		if traeQueueEvent(event, data) {
			return true, src
		}
		event = ""
		if err != nil {
			return false, &prefixReadCloser{Reader: io.MultiReader(bytes.NewReader(prefix.Bytes()), br), src: src}
		}
	}
}

func traeDeltaAvailable(data string) bool {
	_, _, ok := traeDelta(data)
	return ok
}

// traeQueueEvent 同时覆盖事件名和 JSON 文案。不同 Trae 版本分别使用
// queue/waiting 事件，或 progress_notice 中的 status/message/content 字段。
func traeQueueEvent(event, data string) bool {
	ev := strings.ToLower(strings.TrimSpace(event))
	// timing_cost 的 queue_timing 是耗时字段，不是排队状态；只有明确的
	// 队列事件，或 progress/error/status 类事件里的状态文案才触发轮换。
	if ev == "queue" || ev == "queued" || ev == "queueing" || ev == "waiting" || ev == "wait_queue" {
		return true
	}
	if ev != "progress_notice" && ev != "waiting" && ev != "status" && ev != "error" && ev != "message" {
		return false
	}
	s := strings.ToLower(strings.TrimSpace(data))
	for _, marker := range []string{
		"queued", "queueing", "waiting", "wait_queue", "queue position",
		"request queue", "排队", "排隊", "队列", "隊列", "等待处理", "等待中",
		`\"status\":\"queue\"`, `\"status\":\"waiting\"`,
		`\"state\":\"queue\"`, `\"state\":\"waiting\"`,
	} {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}

type prefixReadCloser struct {
	io.Reader
	src io.Closer
}

func (r *prefixReadCloser) Close() error { return r.src.Close() }

// traeDelta 从 Trae 的 output 事件载荷取出正文与思考链。
//
// 逆向结论（2026-10-07 线上抓包，真实帧如下）：
//
//	event: output
//	data: {"response":"OK","reasoning_content":"……","tool_calls":null,
//	       "multimodal_contents":null,"phase":null}
//
//	event: token_usage
//	data: {"name":"ide_prompts","prompt_tokens":95,"completion_tokens":49,
//	       "total_tokens":144,"reasoning_tokens":0,"cache_read_input_tokens":0,...}
//
//	event: progress_notice
//	data: {"name":"construct_prompt","content":"Building prompt: ide_prompts"}
//
// response 是正文，reasoning_content 是思考链（与正文分列，两者都在 output 帧里）。
//
// 返回值 ok=false 表示这一帧既不是正文也不是思考链（progress_notice / metadata /
// prompt / done 等控制帧）。调用方必须跳过，绝不能像旧实现那样对任意 data 做
// 「content/text 字段兜底」，否则会命中
// {"name":"construct_prompt","content":"Building prompt: ide_prompts"} 的 content，
// 把上游的进度提示当成回复正文吐给客户端——这是「回复开头挂着
// Building prompt: ide_prompts」的唯一来源。
func traeDelta(data string) (content, reasoning string, ok bool) {
	var m struct {
		Response         *string `json:"response"`
		ReasoningContent *string `json:"reasoning_content"`
	}
	if json.Unmarshal([]byte(data), &m) != nil {
		return "", "", false
	}
	if m.Response == nil && m.ReasoningContent == nil {
		return "", "", false
	}
	if m.Response != nil {
		content = *m.Response
	}
	if m.ReasoningContent != nil {
		reasoning = *m.ReasoningContent
	}
	return content, reasoning, true
}

// streamTraeUpstream 将 Trae 的 SSE 数据帧转换为 OpenAI SSE。
// 返回本次流观测到的 usage（上游在 event: token_usage 帧里给计数）。
//
// 事件分派严格按事件名（与 traecode2api 的 handleTraeEvent 同口径）：
//
//	output         → response 进正文；reasoning_content 丢弃（OpenAI 的 delta
//	                 没有思考链的标准槽位，塞进 content 会让正文混入思维过程）
//	token_usage    → 取计数，不进正文
//	error          → 把上游错误文案附在正文并收流
//	done           → 立即收流（不等连接关闭，避免客户端卡在最后一帧）
//	其余（progress_notice / metadata / prompt）→ 纯控制帧，一律丢弃
func (h *Handler) streamTraeUpstream(w http.ResponseWriter, res *upstreamResult, model string, stops []string) upstreamUsage {
	defer res.stream.Close()
	flusher := startOpenAIStream(w)
	id := "chatcmpl-" + newID("")
	created := time.Now().Unix()

	sentRole := false
	var got upstreamUsage
	matcher := newStopMatcher(stops)
	// Trae 的工具调用是内联标记文本，先剥离再进 stop 匹配（标记不该出现在正文里）。
	fnp := newTraeFnParser()
	var toolCalls []oaiToolCall
	sc := bufio.NewScanner(res.stream)
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
	pendingEvent := ""
	stop := false
	for !stop && sc.Scan() {
		line := sc.Text()
		// event: 与 data: 成对出现；事件名先到，先暂存再与紧随的 data 配对。
		if strings.HasPrefix(line, "event:") {
			pendingEvent = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		data := ""
		switch {
		case strings.HasPrefix(line, "data:"):
			data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		case strings.HasPrefix(line, "{"):
			data = strings.TrimSpace(line)
		}
		if data == "" || data == "[DONE]" {
			pendingEvent = ""
			continue
		}
		// usage 帧不是正文。事件名缺失时按载荷特征兜底（含 prompt_tokens）。
		if traeUsageEvent(pendingEvent, data) {
			got = parseTraeUsage(data)
			pendingEvent = ""
			continue
		}
		name := pendingEvent
		pendingEvent = ""
		switch name {
		case "done":
			stop = true
		case "error":
			var e struct {
				Message string `json:"message"`
			}
			_ = json.Unmarshal([]byte(data), &e)
			if e.Message != "" {
				writeSSE(w, OAIChunk{ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
					Choices: []OAIChoice{{Index: 0, Delta: &OAIDelta{Role: roleOnce(&sentRole), Content: "\n[trae error] " + e.Message}}}})
				if flusher != nil {
					flusher.Flush()
				}
			}
			stop = true
		case "output":
			raw, _, ok := traeDelta(data)
			if !ok || raw == "" {
				continue
			}
			// 先剥工具标记：命中则进 toolCalls，正文只剩标记之外的部分。
			content, calls := fnp.feed(raw)
			if len(calls) > 0 {
				toolCalls = append(toolCalls, calls...)
			}
			if content == "" {
				continue
			}
			// stop 截断（Trae 私有协议没有 stop 字段，只能网关侧补）。
			if matcher != nil {
				emit, hit := matcher.feed(content)
				if emit != "" {
					writeSSE(w, OAIChunk{ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
						Choices: []OAIChoice{{Index: 0, Delta: &OAIDelta{Role: roleOnce(&sentRole), Content: emit}}}})
					if flusher != nil {
						flusher.Flush()
					}
				}
				if hit {
					stop = true
				}
				continue
			}
			writeSSE(w, OAIChunk{ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
				Choices: []OAIChoice{{Index: 0, Delta: &OAIDelta{Role: roleOnce(&sentRole), Content: content}}}})
			if flusher != nil {
				flusher.Flush()
			}
		default:
			// 控制帧（progress_notice / metadata / prompt / 心跳）不进正文。
		}
	}
	// 收流：把 fnParser 与 stop matcher 各自 hold 住的尾巴补发（否则最后几字被吞）。
	if ftail, fcalls := fnp.flush(); true {
		if len(fcalls) > 0 {
			toolCalls = append(toolCalls, fcalls...)
		}
		if ftail != "" {
			if matcher != nil {
				emit, _ := matcher.feed(ftail)
				if emit != "" {
					writeSSE(w, OAIChunk{ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
						Choices: []OAIChoice{{Index: 0, Delta: &OAIDelta{Role: roleOnce(&sentRole), Content: emit}}}})
				}
			} else {
				writeSSE(w, OAIChunk{ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
					Choices: []OAIChoice{{Index: 0, Delta: &OAIDelta{Role: roleOnce(&sentRole), Content: ftail}}}})
			}
		}
	}
	if matcher != nil {
		if tail := matcher.flush(); tail != "" {
			writeSSE(w, OAIChunk{ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
				Choices: []OAIChoice{{Index: 0, Delta: &OAIDelta{Role: roleOnce(&sentRole), Content: tail}}}})
		}
	}
	// 工具调用：OpenAI 规范的 tool_calls 增量帧，随后 finish_reason=tool_calls。
	if len(toolCalls) > 0 {
		emitToolCallChunks(w, flusher, id, model, created, &sentRole, toolCalls)
	}
	finish := "stop"
	if len(toolCalls) > 0 {
		finish = "tool_calls"
	}
	writeSSE(w, OAIChunk{ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
		Choices: []OAIChoice{{Index: 0, Delta: &OAIDelta{}, FinishReason: &finish}}})
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
	return got
}

// traeFinish 非流式收尾的 finish_reason：有 tool_calls 就是 tool_calls，否则 stop。
func traeFinish(hasTools bool) string {
	if hasTools {
		return "tool_calls"
	}
	return "stop"
}

// collectTraeUpstream 读完整条 Trae 流并聚合成一条 OpenAI 非流式响应。
//
// 上游只有 SSE，而 OpenAI 的 stream:false 语义要求「一次 JSON、一次 usage」。
// 网关在这里聚合（与 traecode2api 的 traeStream.collect 同口径），返回的
// Content-Type 是 application/json，调用方可直接 json.Unmarshal。
func (h *Handler) collectTraeUpstream(w http.ResponseWriter, res *upstreamResult, model string, stops []string) upstreamUsage {
	defer res.stream.Close()
	var content, reasoning strings.Builder
	var got upstreamUsage
	matcher := newStopMatcher(stops)
	// 工具标记解析（见 streamTraeUpstream 的同类说明）。
	fnp := newTraeFnParser()
	var toolCalls []oaiToolCall
	stopHit := false
	errMsg := ""
	sc := bufio.NewScanner(res.stream)
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
	pendingEvent := ""
	stop := false
	for !stop && sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "event:") {
			pendingEvent = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		data := ""
		switch {
		case strings.HasPrefix(line, "data:"):
			data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		case strings.HasPrefix(line, "{"):
			data = strings.TrimSpace(line)
		}
		if data == "" || data == "[DONE]" {
			pendingEvent = ""
			continue
		}
		if traeUsageEvent(pendingEvent, data) {
			got = parseTraeUsage(data)
			pendingEvent = ""
			continue
		}
		name := pendingEvent
		pendingEvent = ""
		switch name {
		case "done":
			stop = true
		case "error":
			var e struct {
				Message string `json:"message"`
			}
			if json.Unmarshal([]byte(data), &e) == nil {
				errMsg = e.Message
			}
			stop = true
		case "output":
			raw, r, ok := traeDelta(data)
			if !ok {
				continue
			}
			if r != "" {
				reasoning.WriteString(r)
			}
			if stopHit {
				continue
			}
			// 先剥工具标记，命中则累积 toolCalls，正文只留其余部分。
			c, calls := fnp.feed(raw)
			if len(calls) > 0 {
				toolCalls = append(toolCalls, calls...)
			}
			if c == "" {
				continue
			}
			// stop 截断（Trae 上游没有该语义，网关侧补）。
			if matcher != nil {
				emit, hit := matcher.feed(c)
				content.WriteString(emit)
				if hit {
					stopHit = true
				}
				continue
			}
			content.WriteString(c)
		default:
			// 控制帧不进正文。
		}
	}
	if ftail, fcalls := fnp.flush(); true {
		if len(fcalls) > 0 {
			toolCalls = append(toolCalls, fcalls...)
		}
		if ftail != "" {
			if matcher != nil {
				emit, _ := matcher.feed(ftail)
				content.WriteString(emit)
			} else {
				content.WriteString(ftail)
			}
		}
	}
	if matcher != nil && !stopHit {
		content.WriteString(matcher.flush())
	}
	msg := map[string]any{"role": "assistant", "content": content.String()}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}
	if reasoning.Len() > 0 {
		msg["reasoning_content"] = reasoning.String()
	}
	if errMsg != "" {
		msg["content"] = strings.TrimRight(content.String(), "\n") + "\n[trae error] " + errMsg
	}
	var prompt, completion int64
	if got.HasPrompt {
		prompt = got.PromptTokens
	}
	if got.HasCompletion {
		completion = got.Completion
	}
	var total int64
	if t, ok := got.total(); ok {
		total = t
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":      "chatcmpl-" + newID(""),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": traeFinish(len(toolCalls) > 0)}},
		"usage": map[string]any{
			"prompt_tokens":     prompt,
			"completion_tokens": completion,
			"total_tokens":      total,
		},
	})
	return got
}

// ---- 辅助（平移自 gw2api） ----

// startOpenAIStream 写出 SSE 响应头并返回 flusher（不支持 flush 时返回 nil）。
func startOpenAIStream(w http.ResponseWriter) http.Flusher {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	f, _ := w.(http.Flusher)
	return f
}

// roleOnce 首次返回 "assistant"，其后返回空串（OpenAI 流式约定：role 只发一次）。
func roleOnce(sent *bool) string {
	if *sent {
		return ""
	}
	*sent = true
	return "assistant"
}

func writeSSE(w io.Writer, v any) {
	b, _ := json.Marshal(v)
	_, _ = io.WriteString(w, "data: ")
	_, _ = w.Write(b)
	_, _ = io.WriteString(w, "\n\n")
}

// newID 生成响应 id（chatcmpl- 后缀）。
func newID(prefix string) string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}

// extractTraeText 从 Trae SSE 数据帧中取文本。
//
// 字段优先级来自真实事件形态：output 事件的正文在 `response`
// （traecode2api 实测口径，见其 traeOutput.Response），思考链在
// `reasoning_content`；其余字段是历史形态兜底，避免上游换名后一片空白。
func extractTraeText(data string) string {
	var m map[string]any
	if json.Unmarshal([]byte(data), &m) != nil {
		return ""
	}
	if v, ok := m["response"].(string); ok && v != "" {
		return v
	}
	if v, ok := m["reasoning_content"].(string); ok && v != "" {
		return v
	}
	if d, ok := m["data"].(map[string]any); ok {
		if v, ok := d["response"].(string); ok && v != "" {
			return v
		}
		if v, ok := d["reasoning_content"].(string); ok && v != "" {
			return v
		}
	}
	for _, k := range []string{"content", "text", "delta"} {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	if d, ok := m["delta"].(map[string]any); ok {
		if v, ok := d["content"].(string); ok {
			return v
		}
	}
	if msg, ok := m["message"].(map[string]any); ok {
		if v, ok := msg["content"].(string); ok {
			return v
		}
	}
	if d, ok := m["data"].(map[string]any); ok {
		if v, ok := d["content"].(string); ok {
			return v
		}
	}
	return ""
}

// previewOf 取最后一条 user 消息作为请求预览（日志用）。
func previewOf(req *OAIRequest) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if strings.EqualFold(req.Messages[i].Role, "user") {
			return truncate(strings.ReplaceAll(req.Messages[i].TextContent(), "\n", " "), 80)
		}
	}
	return ""
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// captureHeaders 复制入站头（剔除与上游鉴权冲突的字段）。
func captureHeaders(r *http.Request) http.Header {
	out := http.Header{}
	drop := map[string]bool{
		"host": true, "content-length": true, "connection": true,
		"authorization": true, "x-api-key": true,
	}
	for k, vs := range r.Header {
		if drop[strings.ToLower(k)] {
			continue
		}
		for _, v := range vs {
			out.Add(k, v)
		}
	}
	return out
}

// anthToOAIMessageWithStop 在 anthToOAIMessage 基础上对正文做网关侧 stop 截断。
// stops 为空时与 anthToOAIMessage 逐字段一致（默认流量零改动）。
func anthToOAIMessageWithStop(body []byte, model string, stops []string) map[string]any {
	out := anthToOAIMessage(body, model)
	if len(stops) == 0 {
		return out
	}
	choices, _ := out["choices"].([]map[string]any)
	for _, c := range choices {
		msg, _ := c["message"].(map[string]any)
		if msg == nil {
			continue
		}
		s, _ := msg["content"].(string)
		if s == "" {
			continue
		}
		if cut, hit := truncateAtStop(s, stops); hit {
			msg["content"] = cut
			c["finish_reason"] = "stop"
		}
	}
	return out
}

// anthToOAIMessage 将 Anthropic Messages 响应转换为 OpenAI chat.completion（非流式用）。
// Anthropic content 只取 text 块；thinking 块不进正文；tool_use 块翻成 tool_calls。
func anthToOAIMessage(body []byte, model string) map[string]any {
	var anth struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Content []struct {
			Type     string          `json:"type"`
			Text     string          `json:"text"`
			Thinking string          `json:"thinking"`
			ID       string          `json:"id"`
			Name     string          `json:"name"`
			Input    json.RawMessage `json:"input"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	out := map[string]any{
		"id":      "chatcmpl-" + newID(""),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
	}
	if json.Unmarshal(body, &anth) != nil {
		out["choices"] = []map[string]any{{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": string(body)},
			"finish_reason": "stop",
		}}
		return out
	}
	var sb strings.Builder
	toolCalls := make([]oaiToolCall, 0, 2)
	for _, c := range anth.Content {
		switch c.Type {
		case "tool_use":
			if c.Name == "" {
				continue
			}
			args := "{}"
			if len(c.Input) > 0 {
				args = string(c.Input)
			}
			id := c.ID
			if id == "" {
				id = "call_" + newID("")
			}
			toolCalls = append(toolCalls, oaiToolCall{
				ID:   id,
				Type: "function",
				Function: oaiToolCallFnSpec{
					Name:      c.Name,
					Arguments: args,
				},
			})
		default:
			if c.Text != "" {
				sb.WriteString(c.Text)
			}
		}
	}
	if anth.Model != "" {
		out["model"] = anth.Model
	}
	if anth.ID != "" {
		out["id"] = anth.ID
	}
	finish := "stop"
	switch anth.StopReason {
	case "max_tokens":
		finish = "length"
	case "tool_use":
		finish = "tool_calls"
	}
	msg := map[string]any{"role": "assistant", "content": sb.String()}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
		if finish == "stop" {
			// Anthropic 回 tool_use 时 stop_reason 已是 tool_use；上游若漏填，
			// 有 tool_calls 就必须收敛成 tool_calls，否则客户端不会去执行工具。
			finish = "tool_calls"
		}
	}
	out["choices"] = []map[string]any{{
		"index":         0,
		"message":       msg,
		"finish_reason": finish,
	}}
	out["usage"] = map[string]any{
		"prompt_tokens":     anth.Usage.InputTokens,
		"completion_tokens": anth.Usage.OutputTokens,
		"total_tokens":      anth.Usage.InputTokens + anth.Usage.OutputTokens,
	}
	return out
}
