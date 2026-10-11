package server

import (
	"encoding/json"
	"strings"

	"github.com/jjjane233/workbuddy-3in1/internal/zcode"
)

// oaiToZCodeRequest 将 OpenAI 请求转换为 ZCode（Anthropic Messages）请求。
// 平移自 gw2api/internal/server/convert.go，仅改 import 路径。
//
// 关键语义：
//   - system 消息从 messages 里抽出来单独作为 Anthropic 的顶层 system 字段，
//     否则上游会把系统提示当成用户消息参与上下文；
//   - role "tool" 在 Anthropic 侧无对应，转成 user 里的 tool_result 块
//     （保留 tool_use_id 关联，否则多轮工具链断）；
//   - assistant 的 tool_calls 转成 tool_use 内容块回灌上下文；
//   - tools / tool_choice 必须转成 Anthropic 形态：原样下发 OpenAI 的
//     {type:function,function:{...}} 会被上游 400 拒绝（2026-10-07 实测：
//     "body.tools.0.name: Field required; body.tool_choice: Input should be a valid dictionary"）；
//   - MaxTokens 缺省补 8192：Anthropic 该字段必填，0 会被上游拒绝。
func oaiToZCodeRequest(req *OAIRequest, model string) *zcode.ChatRequest {
	out := &zcode.ChatRequest{
		Model:       model,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stream:      req.Stream,
		Metadata:    req.Metadata,
	}
	if out.MaxTokens <= 0 {
		out.MaxTokens = 8192
	}
	// stop → stop_sequences
	if len(req.Stop) > 0 {
		out.Stop = req.Stop
	}
	// tools / tool_choice → Anthropic 形态（OpenAI 原样会被上游 400 拒）。
	if ts := anthToolsFromOAI(req.Tools); len(ts) > 0 && !toolChoiceIsNone(req.ToolChoice) {
		out.Tools = ts
		if tc := anthToolChoiceFromOAI(req.ToolChoice); len(tc) > 0 {
			out.ToolChoice = tc
		}
	}
	// 思考开关（Anthropic 原生 thinking）：只在客户端自己带了才透传。
	if th := zcodeThinkingField(req); len(th) > 0 {
		out.Thinking = th
	} else if th := zcodeToggleField(req, model); len(th) > 0 {
		// 客户端用布尔 enable_thinking 表达开关时，由网关翻成
		// Anthropic 的 thinking.type=enabled/disabled（见 zcodeToggleField）。
		out.Thinking = th
	}
	// 思考档位：走 output_config.effort（**不是** thinking.type）。
	// 证据与剂量效应见 zcodeEffortField 的注释。
	if oc := zcodeEffortField(req); len(oc) > 0 {
		out.OutputConfig = oc
	}
	// system 提示
	if len(req.System) > 0 {
		out.System = req.System
	}
	msgs, sysText := anthMessagesFromOAI(req.Messages)
	out.Messages, _ = json.Marshal(msgs)
	if len(out.System) == 0 && sysText != "" {
		out.System, _ = json.Marshal(sysText)
	}
	return out
}

// anthContentBlock 是 Anthropic 消息内容块的最小形态。
// 用 map 构造以便按需带上 text / tool_use / tool_result 不同字段。
type anthContentBlock = map[string]any

// anthMessagesFromOAI 把 OpenAI messages 转成 Anthropic messages 数组，
// 并把 system/developer 文本单独返回。
//
// 转换规则：
//
//	user      → {role:user, content:[{type:text,text:...}]}
//	assistant → {role:assistant, content:[{type:text,..}?,{type:tool_use,id,name,input}...]}
//	system    → 抽出为顶层 system 字符串（不参与 messages）
//	tool      → {role:user, content:[{type:tool_result,tool_use_id,content}]}
func anthMessagesFromOAI(in []OAIMessage) ([]map[string]any, string) {
	out := make([]map[string]any, 0, len(in))
	var sysParts []string
	for _, m := range in {
		switch m.Role {
		case "system", "developer":
			if txt := m.TextContent(); txt != "" {
				sysParts = append(sysParts, txt)
			}
			continue
		case "assistant":
			blocks := make([]anthContentBlock, 0, 2)
			if txt := m.TextContent(); txt != "" {
				blocks = append(blocks, anthContentBlock{"type": "text", "text": txt})
			}
			for _, tc := range parseOAIToolCalls(m.ToolCalls) {
				if tc.Function.Name == "" {
					continue
				}
				var input any = map[string]any{}
				if a := strings.TrimSpace(tc.Function.Arguments); a != "" {
					if json.Unmarshal([]byte(a), &input) != nil {
						input = map[string]any{}
					}
				}
				id := tc.ID
				if id == "" {
					id = "toolu_" + newID("")
				}
				blocks = append(blocks, anthContentBlock{
					"type": "tool_use", "id": id, "name": tc.Function.Name, "input": input,
				})
			}
			if len(blocks) == 0 {
				continue
			}
			out = append(out, map[string]any{"role": "assistant", "content": blocks})
			continue
		case "tool":
			id := m.ToolCallID
			if id == "" {
				id = "toolu_" + newID("")
			}
			out = append(out, map[string]any{"role": "user", "content": []anthContentBlock{{
				"type": "tool_result", "tool_use_id": id, "content": m.TextContent(),
			}}})
			continue
		default:
			out = append(out, map[string]any{"role": "user", "content": []anthContentBlock{{
				"type": "text", "text": m.TextContent(),
			}}})
		}
	}
	// Anthropic 要求 messages 非空：只有 system 时补一条占位 user，避免上游 400。
	if len(out) == 0 {
		out = append(out, map[string]any{"role": "user", "content": []anthContentBlock{{"type": "text", "text": ""}}})
	}
	return out, strings.Join(sysParts, "\n")
}

// zcodeThinkingField 计算 Anthropic **thinking** 字段（原样透传路径）。
//
// 只做一件事：客户端自己带了 Anthropic 原生 thinking（json 里有 type）时原样
// 透传。网关**不再**把档位映射进 thinking.type —— 那条路是错的，见 zcodeEffortField。
//
// 实测依据（2026-10-08，open.bigmodel.cn/api/anthropic/v1/messages，模型 GLM-5.3）：
//
//	thinking:{type:"low"}      → 400 code 1210
//	thinking:{type:"high"}     → 400 code 1210
//	thinking:{type:"max"}      → 400 code 1210
//	thinking:{type:"disabled"} → 400 code 1210
//	（1210 文案「该模型始终思考，不支持关闭思考；请使用 low、high 或 max。」
//	  说的 low/high/max 是 reasoning_effort 取值，不是 thinking.type 的取值域）
//	thinking:{type:"enabled",budget_tokens:N} → 200，正常出 thinking 块
//
// 也就是说 thinking.type 的合法域只有 enabled/disabled，把档位塞进去必然 400。
func zcodeThinkingField(req *OAIRequest) json.RawMessage {
	if req == nil || len(req.Thinking) == 0 {
		return nil
	}
	var probe struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(req.Thinking, &probe) == nil && probe.Type != "" {
		return req.Thinking
	}
	return nil
}

// zcodeToggleField 把客户端的**布尔思考开关**（enable_thinking）翻成 Anthropic 的
// thinking.type。返回 nil 表示不注入（保持上游默认）。
//
// 为什么必须按模型分流（2026-10-08 实测，open.bigmodel.cn/api/anthropic/v1/messages）：
//
//	GLM-5.2 / GLM-5-Turbo / glm-4.7
//	  baseline（不传 thinking）        → 不出 thinking 块
//	  thinking:{type:"enabled"}        → 出 thinking 块（开关有效）
//	  thinking:{type:"disabled"}       → 不出（可关）
//	GLM-5.3 系列
//	  thinking:{type:"disabled"}       → 400 code 1210「该模型始终思考，不支持关闭思考」
//
// 所以：对「始终思考」的模型，enable_thinking=false 会被**静默忽略**（不注入
// disabled），否则用户的每一次调用都会 400 掉——这比"没关掉思考"糟得多。
// enable_thinking=true 则一律注入 enabled（对所有模型都合法）。
func zcodeToggleField(req *OAIRequest, model string) json.RawMessage {
	if req == nil || req.EnableThinking == nil {
		return nil
	}
	if *req.EnableThinking {
		raw, err := json.Marshal(map[string]any{"type": "enabled"})
		if err != nil {
			return nil
		}
		return raw
	}
	// 显式关闭：只对"可开关"的模型注入 disabled；始终思考的模型忽略。
	if spec := zcodeEffortSpec(model); spec.AlwaysThinking {
		return nil
	}
	raw, err := json.Marshal(map[string]any{"type": "disabled"})
	if err != nil {
		return nil
	}
	return raw
}

// zcodeEffortField 计算 Anthropic **output_config** 字段，这才是思考档位的真正载体。
//
// 载体逆向自 ZCode 官方客户端的能力声明（匿名可访问）：
//
//	GET https://zcode.z.ai/api/v1/client/configs?app_version=3.14.3&platform=win32-x64
//	→ data.builtinModels[].reasoning.levels.<level>.anthropic.set
//	  = [{"path":["output_config","effort"],"value":"low"|"high"|"max"}]
//	  + reasoning.defaultLevel = "max"
//
// 实测剂量效应（同一 prompt「一个农夫有17只羊…」，GLM-5.3，返回 thinking 块字符数）：
//
//	无 effort（=默认 max）→ thinking 1485 字符，output_tokens 561
//	effort=low            → thinking    0 字符，output_tokens 127
//	effort=high           → thinking   71 字符，output_tokens 140
//	effort=max            → thinking 1941 字符，output_tokens 699
//
// 非法值上游回 400：「reasoning_effort 参数值非法，可选值为：
// none、minimal、low、medium、high、xhigh、max」——取值域比客户端声明的更宽。
//
// 映射（面板/OpenAI 口径 → 上游 reasoning_effort）：
//
//	off/disabled/none        → none
//	minimal/light            → low
//	low                      → low
//	medium                   → medium
//	high                     → high
//	xhigh/extra_high/max/ultra → xhigh（GLM-5.3 声明的最高档是 max，实测 max
//	                            同样被接受；这里统一落到上游显式列出的 xhigh，
//	                            语义等价且不会被未来收窄取值域时打回）
//
// 优先级：客户端已带 output_config → 原样透传（不覆盖）；否则按档位构造。
// 未指定档位 → nil（网关不注入，上游按模型默认档处理）。
func zcodeEffortField(req *OAIRequest) json.RawMessage {
	if req == nil {
		return nil
	}
	// 1. 原生透传：客户端自己给了 output_config.effort 就照它的。
	if len(req.OutputConfig) > 0 {
		var probe struct {
			Effort string `json:"effort"`
		}
		if json.Unmarshal(req.OutputConfig, &probe) == nil && strings.TrimSpace(probe.Effort) != "" {
			return req.OutputConfig
		}
	}
	// 2. 档位映射
	lvl := req.EffortLevel()
	if lvl == "" {
		return nil
	}
	var effort string
	switch lvl {
	case "off", "disabled", "none":
		effort = "none"
	case "minimal", "light", "low":
		effort = "low"
	case "medium", "mid":
		effort = "medium"
	case "high":
		effort = "high"
	case "xhigh", "extra_high", "max", "ultra":
		effort = "xhigh"
	default:
		return nil // 未知档位不猜测（上游对非法值直接 400）
	}
	raw, err := json.Marshal(map[string]any{"effort": effort})
	if err != nil {
		return nil
	}
	return raw
}
