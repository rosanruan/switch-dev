package proxy

import "encoding/json"

// ====== Anthropic 请求/响应结构 ======

type AnthropicRequest struct {
	Model     string          `json:"model"`
	Messages  []AnthropicMsg  `json:"messages"`
	System    json.RawMessage `json:"system,omitempty"` // string 或 []block
	MaxTokens int             `json:"max_tokens,omitempty"`
	Stream    *bool           `json:"stream,omitempty"` // nil=未传(默认流式), false=显式非流式, true=显式流式
	Temperature *float64      `json:"temperature,omitempty"`
	TopP       *float64       `json:"top_p,omitempty"`
	StopSequences []string    `json:"stop_sequences,omitempty"`
	Tools     []AnthropicTool `json:"tools,omitempty"`
}

type AnthropicMsg struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string 或 []block
}

type AnthropicSystemBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type AnthropicContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"` // tool_result 的 content
	IsError   bool            `json:"is_error,omitempty"` // tool_result 是否为错误结果
}

type AnthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

type AnthropicResponse struct {
	ID           string               `json:"id"`
	Type         string               `json:"type"`
	Role         string               `json:"role"`
	Model        string               `json:"model"`
	Content      []AnthropicContentBlock `json:"content"`
	StopReason   string               `json:"stop_reason"`
	StopSequence *string              `json:"stop_sequence"`
	Usage        AnthropicUsage       `json:"usage"`
}

type AnthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

// ====== OpenAI 请求/响应结构 ======

type OpenAIRequest struct {
	Model       string           `json:"model"`
	Messages    []OpenAIMessage  `json:"messages"`
	MaxTokens   int              `json:"max_tokens,omitempty"`
	Stream      bool             `json:"stream,omitempty"`
	Temperature *float64         `json:"temperature,omitempty"`
	TopP        *float64         `json:"top_p,omitempty"`
	Stop        []string         `json:"stop,omitempty"`
	Tools       []OpenAITool     `json:"tools,omitempty"`

	// JoyCode 业务字段
	Tenant        string `json:"tenant,omitempty"`
	OrgFullName   string `json:"orgFullName,omitempty"`
	UserID        string `json:"userId,omitempty"`
	Client        string `json:"client,omitempty"`
	ClientVersion string `json:"clientVersion,omitempty"`
	Language      string `json:"language,omitempty"`
}

type OpenAIMessage struct {
	Role             string           `json:"role"`
	Content          *string          `json:"content,omitempty"`
	ReasoningContent string           `json:"reasoning_content,omitempty"` // 推理模型思维链（JoyAI-Code-1.5 等输出在此）
	ToolCalls        []OpenAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID       *string          `json:"tool_call_id,omitempty"`
}

type OpenAIToolCall struct {
	ID       string             `json:"id"`
	Type     string             `json:"type"`
	Function OpenAIFunctionCall `json:"function"`
}

type OpenAIFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type OpenAITool struct {
	Type     string         `json:"type"`
	Function OpenAIFunction `json:"function"`
}

type OpenAIFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type OpenAIResponse struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []OpenAIChoice `json:"choices"`
	Usage   *OpenAIUsage   `json:"usage,omitempty"`
}

type OpenAIChoice struct {
	Index        int            `json:"index"`
	Message      OpenAIMessage  `json:"message"`
	FinishReason string         `json:"finish_reason"`
}

type OpenAIUsage struct {
	PromptTokens     int                        `json:"prompt_tokens"`
	CompletionTokens int                        `json:"completion_tokens"`
	TotalTokens      int                        `json:"total_tokens"`
	// 缓存字段
	CacheReadInputTokens int `json:"cache_read_input_tokens,omitempty"` // Anthropic 标准
	PromptTokensDetails  struct {
		CachedTokens int `json:"cached_tokens,omitempty"` // OpenCode/DeepSeek
	} `json:"prompt_tokens_details,omitempty"`
}

// ====== 代理状态 ======

type ProxyStatus struct {
	Running  bool   `json:"running"`
	Port     int    `json:"port"`
	Host     string `json:"host"`
	Mode     string `json:"mode"` // "auto" | "manual"
	Requests int64  `json:"requests"`
}

// ====== 请求日志条目 ======

type LogEntry struct {
	ID        string `json:"id"`
	Timestamp string `json:"timestamp"` // "15:04:05"
	DateTime  string `json:"dateTime"`  // "2026-08-08 15:04:05" 精确到秒，用于持久化排序
	Date      string `json:"date"`      // "2026-08-08" 用于按天查
	Model     string `json:"model"`
	UsedModel string `json:"usedModel,omitempty"` // auto 模式下实际用到的模型
	Source    string `json:"source,omitempty"`    // 请求来源（从 User-Agent 推断：Claude Code/Codex/Benchmark/curl...）
	Upstream  string `json:"upstream"`
	Status    string `json:"status"` // "success" | "error" | "auth_error" | "fallback"
	Code      int    `json:"code"`
	Duration  int64  `json:"duration"` // ms
	ErrorMsg  string `json:"errorMsg,omitempty"`

	// 详细字段（持久化用，前端展开查看）
	Method       string `json:"method,omitempty"`       // "POST"
	Path         string `json:"path,omitempty"`         // "/v1/messages"
	Stream       bool   `json:"stream,omitempty"`       // 是否流式请求
	RequestBody  string `json:"requestBody,omitempty"`  // 请求体
	ResponseBody string `json:"responseBody,omitempty"` // 响应体

	// 用量/费用字段
	InputTokens    int     `json:"inputTokens,omitempty"`    // 输入 token
	OutputTokens   int     `json:"outputTokens,omitempty"`   // 输出 token
	CacheHitTokens int     `json:"cacheHitTokens,omitempty"` // 命中缓存的输入 token
	Cost           float64 `json:"cost,omitempty"`           // 本次费用（美元）
	CostText     string  `json:"costText,omitempty"`     // 费率说明（如 "$1.40/M 入 / $4.40/M 出"）
	RealModel    string  `json:"realModel,omitempty"`    // 响应里真实使用的模型
	FirstByteMs  int64   `json:"firstByteMs,omitempty"`  // 首字节用时（ms，伪流式下 ≈ 总用时）

	// 不序列化到前端/JSONL，仅供 db 层记录 source
	UserAgent string `json:"-"`
}