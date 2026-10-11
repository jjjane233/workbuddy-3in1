package server

import (
	"encoding/json"
	"strings"
)

// ---- OpenAI 兼容协议类型 ----
//
// 从 gw2api/internal/server/types.go 平移：workbuddy 对 CodeBuddy 走「原始 body 透传」，
// 本不需要结构化请求；Trae / ZCode 两条上游则必须改写 body（模型名映射、协议转换），
// 因此三合一后引入这组类型。字段名与 gw2api 保持一致，便于后续同步上游修复。

// OAIMessage OpenAI 消息。
type OAIMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	Name       string          `json:"name,omitempty"`
	ToolCalls  json.RawMessage `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

// TextContent 取消息的纯文本内容（兼容 string 与 [{type:text,text}] 两种形态）。
func (m *OAIMessage) TextContent() string {
	if len(m.Content) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(m.Content, &parts) == nil {
		var b strings.Builder
		for _, p := range parts {
			if p.Text != "" {
				if b.Len() > 0 {
					b.WriteString("\n")
				}
				b.WriteString(p.Text)
			}
		}
		return b.String()
	}
	return string(m.Content)
}

// OAIRequest OpenAI /v1/chat/completions 请求。
type OAIRequest struct {
	Model         string          `json:"model"`
	Messages      []OAIMessage    `json:"messages"`
	Stream        bool            `json:"stream,omitempty"`
	MaxTokens     int             `json:"max_tokens,omitempty"`
	Temperature   *float64        `json:"temperature,omitempty"`
	TopP          *float64        `json:"top_p,omitempty"`
	Stop          json.RawMessage `json:"stop,omitempty"`
	Tools         json.RawMessage `json:"tools,omitempty"`
	ToolChoice    json.RawMessage `json:"tool_choice,omitempty"`
	System        json.RawMessage `json:"system,omitempty"`
	Metadata      json.RawMessage `json:"metadata,omitempty"`
	StreamOptions json.RawMessage `json:"stream_options,omitempty"`
	// ReasoningEffort / ReasoningEffortLevel 思考档位。
	//
	// 两个键都收：OpenAI 生态用 reasoning_effort（o 系原生），Trae 客户端与
	// 部分第三方 SDK 用 reasoning_effort_level（逆向 ai-modules-chat 的
	// custom_model 构造可见）。语义相同，取值归一后由各上游转换层自行落地。
	// 空串 = 客户端没指定（走各上游默认），与「显式传了非法值」区分开。
	ReasoningEffort      string `json:"reasoning_effort,omitempty"`
	ReasoningEffortLevel string `json:"reasoning_effort_level,omitempty"`
	// Thinking 是 Anthropic 原生思考开关（{type:enabled|disabled, budget_tokens}）。
	// ZCode 走 Anthropic Messages 协议，客户端若直接给该字段就原样透传，
	// 优先级高于 ReasoningEffort 的档位映射。
	//
	// 注意取值域：thinking.type 只认 enabled/disabled。把档位（low/high/max）
	// 写进去会被上游 400 code 1210 拒绝 —— 档位的载体是 OutputConfig。见下。
	Thinking json.RawMessage `json:"thinking,omitempty"`
	// EnableThinking 是**布尔思考开关**（OpenAI/DeepSeek/第三方 SDK 常见形态）。
	//
	// 与 Thinking 的分工：Thinking 是 Anthropic 原生对象（原样透传）；
	// EnableThinking 是简单开关，网关负责翻成各上游的正确载体。
	// 指针类型是为了区分「没传」与「显式传 false」——前者走上游默认，
	// 后者要求明确关闭思考（对始终思考的模型会被忽略，见 convert_zcode.go）。
	EnableThinking *bool `json:"enable_thinking,omitempty"`
	// OutputConfig 是 Anthropic 的档位载体（{"effort":"low|high|max|..."}）。
	// ZCode 官方客户端就是用它表达思考档位的（能力声明里的
	// path ["output_config","effort"]，见 server.zcodeEffortField 注释）。
	OutputConfig json.RawMessage `json:"output_config,omitempty"`
	// 以下为 Trae 原生直通字段（可选用 X-Trae-Raw 头或直接塞入）。
	ChatHistory    json.RawMessage `json:"chat_history,omitempty"`
	ModelName      string          `json:"model_name,omitempty"`
	CustomModel    json.RawMessage `json:"custom_model,omitempty"`
	ConversationID string          `json:"conversation_id,omitempty"`
	Extra          map[string]any  `json:"-"`
}

// EffortLevel 返回归一后的思考档位（小写、去空格）。
// ReasoningEffort 优先，缺省回落 ReasoningEffortLevel。
func (r *OAIRequest) EffortLevel() string {
	if r == nil {
		return ""
	}
	if v := strings.ToLower(strings.TrimSpace(r.ReasoningEffort)); v != "" {
		return v
	}
	return strings.ToLower(strings.TrimSpace(r.ReasoningEffortLevel))
}

// OAIModel 模型条目。
type OAIModel struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// OAIModelsResponse /v1/models 响应。
type OAIModelsResponse struct {
	Object string     `json:"object"`
	Data   []OAIModel `json:"data"`
}

// OAIChunk OpenAI 流式分片。
type OAIChunk struct {
	ID      string      `json:"id"`
	Object  string      `json:"object"`
	Created int64       `json:"created"`
	Model   string      `json:"model"`
	Choices []OAIChoice `json:"choices"`
	Usage   *OAIUsage   `json:"usage,omitempty"`
}

// OAIChoice 单个候选。
type OAIChoice struct {
	Index        int         `json:"index"`
	Delta        *OAIDelta   `json:"delta,omitempty"`
	Message      *OAIMessage `json:"message,omitempty"`
	FinishReason *string     `json:"finish_reason"`
}

// OAIDelta 增量内容。
// ToolCalls 为 function calling 增量（zcode/trae 链路的工具调用经网关翻译后从这里出）。
type OAIDelta struct {
	Role      string          `json:"role,omitempty"`
	Content   string          `json:"content,omitempty"`
	ToolCalls json.RawMessage `json:"tool_calls,omitempty"`
}

// OAIUsage 用量。
type OAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ---- Anthropic Messages 协议类型 ----
//
// 目前仅供 oaiToZCodeRequest 产出的 ChatRequest 使用；workbuddy 尚未开放
// /v1/messages 对外端点，故未移植 gw2api 的 anthToOAIMessage 反向转换。

// AnthMessage Anthropic 消息。
type AnthMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// AnthRequest Anthropic /v1/messages 请求。
type AnthRequest struct {
	Model       string          `json:"model"`
	Messages    []AnthMessage   `json:"messages"`
	System      json.RawMessage `json:"system,omitempty"`
	MaxTokens   int             `json:"max_tokens"`
	Temperature *float64        `json:"temperature,omitempty"`
	TopP        *float64        `json:"top_p,omitempty"`
	StopSeqs    json.RawMessage `json:"stop_sequences,omitempty"`
	Stream      bool            `json:"stream,omitempty"`
	Tools       json.RawMessage `json:"tools,omitempty"`
	ToolChoice  json.RawMessage `json:"tool_choice,omitempty"`
	Metadata    json.RawMessage `json:"metadata,omitempty"`
}
