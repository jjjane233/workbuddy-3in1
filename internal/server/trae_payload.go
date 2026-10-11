package server

import (
	"encoding/json"
	"io"
	"strings"
	"sync"

	"github.com/jjjane233/workbuddy-3in1/internal/trae"
)

// 从 gw2api/internal/server/trae_payload.go 平移：OpenAI 请求 → Trae IDE 私有协议请求体。

// chanReader 将字符串通道适配为 io.ReadCloser（供 SSE 透传）。
type chanReader struct {
	ch   <-chan string
	err  <-chan error
	buf  []byte
	once sync.Once
}

func (r *chanReader) Read(p []byte) (int, error) {
	for len(r.buf) == 0 {
		v, ok := <-r.ch
		if !ok {
			select {
			case e := <-r.err:
				if e != nil {
					return 0, e
				}
			default:
			}
			return 0, io.EOF
		}
		if v == "" {
			continue
		}
		r.buf = []byte(v)
		if !strings.HasSuffix(v, "\n") {
			r.buf = append(r.buf, '\n')
		}
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}

func (r *chanReader) Close() error {
	r.once.Do(func() {})
	return nil
}

// defaultTraeIntent 上游唯一被接受的通用问答意图。传非法值上游直接回
// "intent ... not found"，因此这里不开放改写。
const defaultTraeIntent = "general_qa_intent"

// lastUserInput 从 chat_history 里取最后一条 user 消息的文本。
// 已实测：user_input 缺失时上游返回兜底话术（"请描述具体编程问题"）而不真正作答。
func lastUserInput(v any) string {
	hist, _ := v.([]any)
	for i := len(hist) - 1; i >= 0; i-- {
		m, _ := hist[i].(map[string]any)
		if m == nil {
			continue
		}
		if role, _ := m["role"].(string); !strings.EqualFold(role, "user") {
			continue
		}
		if c, ok := m["content"].(string); ok {
			return c
		}
	}
	return ""
}

// buildTraePayload 将 OpenAI 请求转换为 Trae /api/ide/v1/chat 请求体。
//
// 逆向结论（与 traecode2api 实测口径一致）：
//   - 对话内容放 `chat_history`（数组，元素 {role,content}）；`messages` **不被识别**，
//     早期版本发 messages 会被上游忽略或回兜底话术。
//   - `user_input` 是 prompt pipeline 的关键输入，必须是最后一条 user 文本，
//     缺失时模型只会回 "请描述具体编程问题"。
//   - `intent_name` 必须合法，否则报 "intent ... not found"。
//   - system/developer 消息并入紧随其后的 user 消息；tool 结果并入 user
//     （上游 chat_history 只认 user/assistant 两种角色）。
//   - 上游只提供流式，故 stream 恒为 true。
func buildTraePayload(req *OAIRequest, model string) []byte {
	// 原生直通：调用方直接给了 chat_history 时，只补齐上游必需的三个字段。
	if len(req.ChatHistory) > 0 {
		body := map[string]any{}
		_ = json.Unmarshal(req.ChatHistory, &body)
		if v, _ := body["model_name"].(string); v == "" {
			body["model_name"] = traeModelName(model)
		}
		if _, ok := body["intent_name"]; !ok {
			body["intent_name"] = defaultTraeIntent
		}
		body["stream"] = true
		if v, _ := body["user_input"].(string); v == "" {
			body["user_input"] = lastUserInput(body["chat_history"])
		}
		if req.ConversationID != "" {
			body["conversation_id"] = req.ConversationID
		}
		// 直通分支同样补档位：调用方给了 chat_history 但没自己带档位时，
		// 由网关按请求里的 reasoning_effort 归一注入（已带则不覆盖）。
		if _, has := body["custom_model"]; !has && len(req.CustomModel) == 0 {
			injectTraeEffort(body, req, req.ModelName, model)
		}
		return marshalNoHTML(body)
	}

	// 标准 OpenAI messages → chat_history（system 前置合并，tool 归入 user）。
	//
	// 工具调用：Trae 上游会**静默忽略** tools 字段（2026-10-07 实测：带 tools
	// 仍回自然语言编造答案），应该用的通路是把工具说明注入提示词，
	// 让模型输出 <|FunctionCallBegin|>...<|FunctionCallEnd|> 内联标记，
	// 再由网关侧（traeFnParser）翻译成 tool_calls。
	toolPrompt := ""
	if !toolChoiceIsNone(req.ToolChoice) {
		toolPrompt = traeToolPrompt(req.Tools)
	}
	// 历史里已有工具结果时，追加一条硬指令。Trae 上游对标记的遵循率不稳定，
	// 不加这句时模型会重复调用同一工具（实测 T2 偶发重复），形成调用环。
	if toolPrompt != "" && hasToolResult(req.Messages) {
		toolPrompt += "\n\n注意：上面的工具已经执行完毕，结果已在对话中给出。你必须直接依据结果给出最终自然语言回答，严禁再次输出任何 " + traeFnBegin + " 标记。"
	}
	hist := make([]map[string]any, 0, len(req.Messages))
	var systemPrompt string
	lastUser := ""
	injected := false
	for _, m := range req.Messages {
		role := m.Role
		text := m.TextContent()
		switch strings.ToLower(role) {
		case "system", "developer":
			if text != "" {
				if systemPrompt != "" {
					systemPrompt += "\n"
				}
				systemPrompt += text
			}
			continue
		case "assistant":
			role = "assistant"
			// 历史里的 tool_calls 还原成标记文本，否则多轮工具链会断。
			if calls := parseOAIToolCalls(m.ToolCalls); len(calls) > 0 {
				rendered := renderTraeCalls(calls)
				if text != "" {
					text = text + "\n" + rendered
				} else {
					text = rendered
				}
			}
		case "tool", "function":
			// 工具执行结果：包成结果标记回灌，否则模型认不出、会重复调用同一工具。
			role = "user"
			if text != "" {
				text = renderTraeResult(m.ToolCallID, text)
			}
		default:
			// user / 其它一律按 user 处理。
			role = "user"
		}
		if role == "assistant" {
			if text != "" {
				hist = append(hist, map[string]any{"role": role, "content": text})
			}
			continue
		}
		// 首条 user 承载 system + 工具说明（上游 chat_history 只有 user/assistant）。
		if !injected {
			pre := ""
			if toolPrompt != "" {
				pre = toolPrompt
			}
			if systemPrompt != "" {
				if pre != "" {
					pre += "\n\n"
				}
				pre += systemPrompt
				systemPrompt = ""
			}
			if pre != "" {
				if text != "" {
					text = pre + "\n\n" + text
				} else {
					text = pre
				}
			}
			injected = true
		}
		lastUser = text
		hist = append(hist, map[string]any{"role": role, "content": text})
	}
	// 没有 user 消息（或首条 user 之前没有 user）：把 system / 工具说明补成首条 user。
	if !injected && (systemPrompt != "" || toolPrompt != "") {
		combined := toolPrompt
		if systemPrompt != "" {
			if combined != "" {
				combined += "\n\n"
			}
			combined += systemPrompt
		}
		hist = append([]map[string]any{{"role": "user", "content": combined}}, hist...)
		lastUser = combined
	}
	if len(hist) == 0 {
		hist = append(hist, map[string]any{"role": "user", "content": ""})
	}
	body := map[string]any{
		"chat_history": hist,
		"user_input":   lastUser,
		"model_name":   traeModelName(model),
		"intent_name":  defaultTraeIntent,
		"stream":       true,
	}
	if req.ConversationID != "" {
		body["conversation_id"] = req.ConversationID
	}
	injectTraeEffort(body, req, req.ModelName, model)
	return marshalNoHTML(body)
}

// injectTraeEffort 把客户端的思考档位落到 Trae /chat 请求体的
// custom_model.reasoning_effort_level（客户端口径 light/high/extra_high）。
//
// 载体来自逆向（ai-modules-chat/dist/index.mjs @11500921）：
//
//	let h = model.reasoning_effort_level ?? model.reasoning_effort;
//	... custom_model: {provider, is_preset, config_name, model_name,
//	    display_model_name, config_source, use_remote_service, multimodal,
//	    ...(h ? {reasoning_effort_level: h} : {})}
//
// 注意 custom_model 是**消息级**字段（随每轮请求发出），和「模型列表里的
// effort 能力」不是一回事；档位只在客户端选了才带，未选时整个字段缺席，
// 上游按模型默认档应答。
//
// 客户端用 light/high/extra_high，OpenAI 生态（含本网关面板）用
// low/medium/high/xhigh。这里做双向归一：客户端口径原样透传，
// 面板口径映射到客户端口径，其余（含旧版 max/extra_high）落 extra_high。
func injectTraeEffort(body map[string]any, req *OAIRequest, cfgName, model string) {
	lvl := traeEffortClient(req)
	if lvl == "" {
		return
	}
	if cfgName == "" {
		cfgName = traeModelName(model)
	}
	name := traeModelName(model)
	cm := map[string]any{
		"provider":               "",
		"is_preset":              true,
		"config_name":            cfgName,
		"config_source":          1, // ModelConfigSource.Trae（内置平台模型）
		"model_name":             name,
		"display_model_name":     name,
		"use_remote_service":     true,
		"multimodal":             false,
		"reasoning_effort_level": lvl,
	}
	body["custom_model"] = cm
	// 顶层同名字段也一并带上：Trae 客户端有两条上行路径都发它——
	//   · 规划器（ai-modules-chat @11496199）把 reasoning_effort_level 放**顶层**
	//   · 会话消息（@11500921）把它放 custom_model 里
	// 网关无法预知上游认哪条，按客户端口径两条都给，与单发 custom_model 相比
	// 不引入任何新字段语义（同名同值）。
	if _, has := body["reasoning_effort_level"]; !has {
		body["reasoning_effort_level"] = lvl
	}
}

// traeEffortClient 归一请求里的档位为 Trae 客户端口径；无指定返回空串。
func traeEffortClient(req *OAIRequest) string {
	lvl := req.EffortLevel()
	switch lvl {
	case "":
		return ""
	// 客户端原生口径：原样透传
	case "light", "high", "extra_high":
		return lvl
	// OpenAI 生态口径 → 客户端口径
	case "low", "minimal":
		return "light"
	case "medium", "xhigh", "max", "ultra":
		return "extra_high"
	}
	// 未知值不猜测：交给上游按默认档处理，避免把非法值塞进去被整条请求拒绝。
	return ""
}

// traeModelName 归一 Trae 模型名（别名 → 上游真实 model_name）。
//
// 2026-10-06 上线实测：上游 /api/ide/v1/chat 只认
//
//	seed_m8、Doubao_1_5_thinking_pro、deepseek-V3、deepseek-V3-0324
//
// 其余一律回 "[trae error] model is unknown"（含 auto/solo/agent）。
// 因此别名必须指向这四个，不能再跟旧版一样取 "auto"/"solo"/"agent"。
func traeModelName(m string) string {
	switch strings.ToLower(strings.TrimSpace(m)) {
	// 空 / 通用别名 → 默认可用模型
	case "", "seed_m8", "seed-m8", "doubao-1.5-pro", "doubao_1_5_pro",
		"trae-auto", "trae-auto-with-agent", "auto", "default":
		return traeDefaultModelName
	case "trae-solo", "solo", "trae-agent", "agent":
		// 旧版把 solo/agent 当独立 model_name 发上去，实测均 unknown。
		// 上游没有这两个模型，归一到默认可用模型（行为与 auto 一致）。
		return traeDefaultModelName
	case "doubao", "doubao-1.5-thinking-pro", "doubao_1_5_thinking_pro":
		return "Doubao_1_5_thinking_pro"
	case "deepseek", "deepseek-v3":
		return "deepseek-V3"
	case "deepseek-v3-0324":
		return "deepseek-V3-0324"
	}
	return strings.TrimSpace(m)
}

// traeDefaultModelName Trae 平台默认可用模型（model_list 里 is_default=true）。
const traeDefaultModelName = "seed_m8"

// useSoloModelChain 判断模型是否应走 DLL/SOLO 链（llm_utils_chat）。
// trae-* 前缀是 /chat 兼容别名，走旧链；其余显式模型名统一走 SOLO。
// 空 / seed_m8 / 已知 /chat 别名保留旧链（与 traecode2api 口径一致）。
func useSoloModelChain(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" || m == "seed_m8" {
		return false
	}
	// /chat 兼容别名（auto/solo/agent/trae-* 等）：这些名字上游 /chat 层
	// 会归一成平台模型，保持旧链行为不变。
	switch m {
	case "auto", "default", "solo", "agent":
		return false
	}
	return !strings.HasPrefix(m, "trae-")
}

// buildSoloPayload 将 [OI] messages 转成 llm_utils_chat 的 DLL 模型格式。
// SOLO 链要求 messages.content 为 [{type:"text",text:"..."}]，并同时携带
// model/config_name/function；它与 /api/ide/v1/chat 的 chat_history 不是同一协议。
// 模型名必须原样透传（此前 gw2api 的静默 seed_m8 降级会让请求看似成功、
// 实际调用了另一模型），上游负责校验模型是否属于当前账号。
//
// 工具调用与 /chat 链同口径（2026-10-12 修复）：SOLO 上游的 tools schema 与本网关
// 对外的 OpenAI 形态**不兼容**——它把 function.parameters 声明为 string，透传对象
// 会被上游 400 拒绝：
//
//	json: cannot unmarshal object into Go struct field
//	FunctionDefinition.tools.function.parameters of type string
//
// 而响应侧的 traeFnParser 由 /chat 与 SOLO 两条链共用（streamTraeUpstream /
// collectTraeUpstream），故这里同样走「提示词注入 + <|FunctionCallBegin|> 内联标记」，
// 不再把 tools / tool_choice 透传给上游。
func buildSoloPayload(req *OAIRequest, model string) []byte {
	// 工具说明（与 buildTraePayload 同源，见 toolcall.go）。
	toolPrompt := ""
	if !toolChoiceIsNone(req.ToolChoice) {
		toolPrompt = traeToolPrompt(req.Tools)
	}
	// 历史里已有工具结果时追加硬指令：上游对标记的遵循率不稳定，不加这句
	// 模型会重复调用同一工具，形成调用环（/chat 链实测 T2 偶发重复）。
	if toolPrompt != "" && hasToolResult(req.Messages) {
		toolPrompt += "\n\n注意：上面的工具已经执行完毕，结果已在对话中给出。你必须直接依据结果给出最终自然语言回答，严禁再次输出任何 " + traeFnBegin + " 标记。"
	}

	messages := make([]map[string]any, 0, len(req.Messages))
	injected := false
	for _, m := range req.Messages {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		if role == "developer" {
			role = "system"
		}
		if role == "" {
			role = "user"
		}
		// content 与 text 互斥：content 非空 = 调用方给了结构化块（可能含图片等
		// 非文本块），此时正文一律走块结构，网关只在首/尾追加文本块，绝不改写原块。
		content := []map[string]any{}
		var text string
		if len(m.Content) > 0 {
			var blocks []map[string]any
			if json.Unmarshal(m.Content, &blocks) == nil && len(blocks) > 0 {
				content = blocks
			} else {
				text = m.TextContent()
			}
		}

		// pre 前置、post 后置：两者都只由网关生成，调用方给的正文原样保留。
		pre, post := "", ""
		switch role {
		case "assistant":
			// 历史里的 tool_calls 还原成标记文本，否则模型看不到自己调用过什么，
			// 多轮工具链会断（与 /chat 链同处理）。
			if calls := parseOAIToolCalls(m.ToolCalls); len(calls) > 0 {
				post = renderTraeCalls(calls)
			}
		case "tool", "function":
			// 工具执行结果：上游没有 tool 角色，包成结果标记并归入 user。
			// 裸文本模型认不出这是工具返回值，会重复调用同一工具。
			role = "user"
			text = renderTraeResult(m.ToolCallID, m.TextContent())
			content = nil
		}
		// 首条 user 承载工具说明。SOLO 链能收 system，但放这里与 /chat 链行为一致，
		// 且不改变消息条数——改动面最小。
		if role == "user" && !injected && toolPrompt != "" {
			injected = true
			pre = toolPrompt
		}
		messages = append(messages, map[string]any{
			"role":    role,
			"content": soloContent(content, text, pre, post),
		})
	}
	if len(messages) == 0 {
		messages = append(messages, map[string]any{"role": "user", "content": []map[string]any{{"type": "text", "text": ""}}})
	}
	modelName := strings.TrimSpace(model)
	if strings.TrimSpace(req.ModelName) != "" {
		modelName = strings.TrimSpace(req.ModelName)
	}
	body := map[string]any{
		"messages":    messages,
		"model":       modelName,
		"config_name": modelName,
		"function":    trae.SoloFunction,
		"stream":      true,
	}
	if req.MaxTokens > 0 {
		body["max_tokens"] = req.MaxTokens
	}
	if req.Temperature != nil {
		body["temperature"] = req.Temperature
	}
	if req.TopP != nil {
		body["top_p"] = req.TopP
	}
	return marshalNoHTML(body)
}

// soloContent 组装 SOLO 链要求的 content 块数组。
//
//   - content 为空（调用方给的是纯文本）：pre + text + post 合成单个 text 块。
//   - content 非空（调用方给了结构化块）：原块**原样保留**，pre/post 只作为独立的
//     text 块插入首/尾。图片等非文本块因此不会被工具说明或标记覆盖掉。
//
// 「唯一块且是文本」这一常见形态走原地前置，块数与形态保持不变（改动面最小）。
func soloContent(content []map[string]any, text, pre, post string) []map[string]any {
	if len(content) == 0 {
		body := text
		if pre != "" {
			if body != "" {
				body = pre + "\n\n" + body
			} else {
				body = pre
			}
		}
		if post != "" {
			if body != "" {
				body = body + "\n" + post
			} else {
				body = post
			}
		}
		return []map[string]any{{"type": "text", "text": body}}
	}
	if pre != "" {
		merged := false
		if len(content) == 1 {
			if t, _ := content[0]["type"].(string); t == "text" {
				if cur, _ := content[0]["text"].(string); cur != "" {
					content[0]["text"] = pre + "\n\n" + cur
				} else {
					content[0]["text"] = pre
				}
				merged = true
			}
		}
		if !merged {
			content = append([]map[string]any{{"type": "text", "text": pre}}, content...)
		}
	}
	if post != "" {
		content = append(content, map[string]any{"type": "text", "text": post})
	}
	return content
}
