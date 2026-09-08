package proxy

import (
	"encoding/json"
	"fmt"
	"strings"
)

// AnthropicToOpenAI 把 Anthropic /v1/messages 请求体转成 OpenAI Chat Completions 格式（JoyCode 用）
// 业务字段（tenant/userId/client 等）由 JoyCode 上游适配器在发送前注入
func AnthropicToOpenAI(body *AnthropicRequest) (*OpenAIRequest, bool) {
	requestedModel := body.Model
	resolvedModel := ResolveModel(requestedModel)
	isAuto := requestedModel == "" || strings.ToLower(requestedModel) == "auto"

	messages, tools := anthropicMessagesToOpenAI(body)

	maxTokens := ClampMaxTokens(body.MaxTokens, 0)
	if jcMeta, ok := LookupModel(resolvedModel); ok {
		maxTokens = ClampMaxTokens(body.MaxTokens, jcMeta.Output)
	}

	openai := &OpenAIRequest{
		Model:     resolvedModel,
		Messages:  messages,
		Stream:    false, // 上游统一非流式
		MaxTokens: maxTokens,
	}

	if body.Temperature != nil {
		openai.Temperature = body.Temperature
	}
	if body.TopP != nil {
		openai.TopP = body.TopP
	}
	if len(body.StopSequences) > 0 {
		openai.Stop = body.StopSequences
	}
	if tools != nil {
		openai.Tools = tools
	}

	return openai, isAuto
}

// AnthropicToOpenAIDeveco 把 Anthropic 请求转成 DevEco 的 OpenAI body（不注入业务字段）
// 目录里查不到的模型（如接口新上、种子还没收录的）原样透传 model 名、不钳制 max_tokens：
// 让上游自己报错，胜过静默换成另一个模型。
func AnthropicToOpenAIDeveco(body *AnthropicRequest) *OpenAIRequest {
	requestedModel := body.Model
	resolvedModel := ResolveModel(requestedModel)

	messages, tools := anthropicMessagesToOpenAI(body)

	maxTokens := 4096
	if body.MaxTokens > 0 {
		maxTokens = body.MaxTokens
	}

	wireModel := ActualUpstreamModel(resolvedModel)
	if dm, ok := LookupModel(resolvedModel); ok && dm.Output > 0 && maxTokens > dm.Output {
		maxTokens = dm.Output
	}

	openai := &OpenAIRequest{
		Model:     wireModel,
		Messages:  messages,
		Stream:    false,
		MaxTokens: maxTokens,
	}

	if body.Temperature != nil {
		openai.Temperature = body.Temperature
	}
	if body.TopP != nil {
		openai.TopP = body.TopP
	}
	if len(body.StopSequences) > 0 {
		openai.Stop = body.StopSequences
	}
	if tools != nil {
		openai.Tools = tools
	}
	return openai
}

// AnthropicToOpenAIOpencode 把 Anthropic 请求转成 OpenCode 的 OpenAI body
func AnthropicToOpenAIOpencode(body *AnthropicRequest) *OpenAIRequest {
	requestedModel := body.Model
	resolvedModel := ResolveModel(requestedModel)

	messages, tools := anthropicMessagesToOpenAI(body)

	maxTokens := 4096
	if body.MaxTokens > 0 {
		maxTokens = body.MaxTokens
	}
	if ocMeta, ok := LookupModel(resolvedModel); ok && ocMeta.Output > 0 && maxTokens > ocMeta.Output {
		maxTokens = ocMeta.Output
	}

	openai := &OpenAIRequest{
		Model:     resolvedModel,
		Messages:  messages,
		Stream:    false,
		MaxTokens: maxTokens,
	}

	if body.Temperature != nil {
		openai.Temperature = body.Temperature
	}
	if body.TopP != nil {
		openai.TopP = body.TopP
	}
	if len(body.StopSequences) > 0 {
		openai.Stop = body.StopSequences
	}
	if tools != nil {
		openai.Tools = tools
	}
	return openai
}

// AnthropicToOpenAIWorkbuddy 把 Anthropic 请求转成 WorkBuddy 的 OpenAI body
// model 去掉 wb/ 前缀；stream 设 false（WorkBuddy 上游强制 stream:true，由 Call 内部覆盖）
func AnthropicToOpenAIWorkbuddy(body *AnthropicRequest) *OpenAIRequest {
	requestedModel := body.Model
	resolvedModel := ResolveModel(requestedModel)

	messages, tools := anthropicMessagesToOpenAI(body)

	maxTokens := 4096
	if body.MaxTokens > 0 {
		maxTokens = body.MaxTokens
	}
	if wbMeta, ok := LookupModel(resolvedModel); ok && wbMeta.Output > 0 && maxTokens > wbMeta.Output {
		maxTokens = wbMeta.Output
	}

	openai := &OpenAIRequest{
		Model:     stripWbPrefix(resolvedModel),
		Messages:  messages,
		Stream:    false,
		MaxTokens: maxTokens,
	}

	if body.Temperature != nil {
		openai.Temperature = body.Temperature
	}
	if body.TopP != nil {
		openai.TopP = body.TopP
	}
	if len(body.StopSequences) > 0 {
		openai.Stop = body.StopSequences
	}
	if tools != nil {
		openai.Tools = tools
	}
	return openai
}

// anthropicMessagesToOpenAI 共用：Anthropic body 的 messages/system/tools 转成 OpenAI 的 messages + tools
func anthropicMessagesToOpenAI(body *AnthropicRequest) ([]OpenAIMessage, []OpenAITool) {
	var messages []OpenAIMessage

	// system 字段 → 第一条 system message
	if len(body.System) > 0 {
		var sysText string
		// 尝试作为 string 解析
		var sysStr string
		if err := json.Unmarshal(body.System, &sysStr); err == nil {
			sysText = sysStr
		} else {
			// 作为 []block 解析
			var blocks []AnthropicSystemBlock
			if err := json.Unmarshal(body.System, &blocks); err == nil {
				var parts []string
				for _, b := range blocks {
					if b.Text != "" {
						parts = append(parts, b.Text)
					}
				}
				sysText = strings.Join(parts, "\n")
			}
		}
		if sysText != "" {
			messages = append(messages, OpenAIMessage{Role: "system", Content: &sysText})
		}
	}

	// Anthropic messages → OpenAI messages
	for _, msg := range body.Messages {
		role := msg.Role
		if role == "assistant" {
			// keep
		} else {
			role = "user"
		}

		// 尝试作为 string 解析
		var contentStr string
		if err := json.Unmarshal(msg.Content, &contentStr); err == nil {
			messages = append(messages, OpenAIMessage{Role: role, Content: &contentStr})
			continue
		}

		// 作为 []block 解析
		var blocks []AnthropicContentBlock
		if err := json.Unmarshal(msg.Content, &blocks); err != nil {
			// fallback: 直接用 raw string
			raw := string(msg.Content)
			messages = append(messages, OpenAIMessage{Role: role, Content: &raw})
			continue
		}

		var textParts []string
		var toolCalls []OpenAIToolCall

		// user 消息里夹杂文本时，文本需在 tool_result 前后各自成 user 消息。
		// 用 pendingText 暂存文本，遇到 tool_result 先冲刷为 user 消息。
		flushText := func(roleForText string) {
			if len(textParts) == 0 {
				return
			}
			t := strings.Join(textParts, "")
			messages = append(messages, OpenAIMessage{Role: roleForText, Content: &t})
			textParts = nil
		}

		for _, block := range blocks {
			switch block.Type {
			case "text":
				if block.Text != "" {
					textParts = append(textParts, block.Text)
				}
			case "tool_use":
				// assistant 消息里的 tool_use：收集，最后与文本合并成一条
				toolCalls = append(toolCalls, OpenAIToolCall{
					ID:   block.ID,
					Type: "function",
					Function: OpenAIFunctionCall{
						Name:      block.Name,
						Arguments: safeToolArgs(block.Input),
					},
				})
			case "tool_result":
				// 每个 tool_result 独立成一条 role:tool 消息；先冲刷前面的文本
				flushText(role)
				content := extractTextFromContent(block.Content)
				if block.IsError && content != "" {
					content = "[工具执行失败] " + content
				}
				toolCallID := block.ToolUseID
				messages = append(messages, OpenAIMessage{
					Role:       "tool",
					Content:    &content,
					ToolCallID: &toolCallID,
				})
			}
		}

		// 收尾：tool_use 合并成 assistant 消息（携带前导文本）；否则冲刷剩余文本
		if len(toolCalls) > 0 {
			var content *string
			if len(textParts) > 0 {
				t := strings.Join(textParts, "")
				content = &t
			}
			messages = append(messages, OpenAIMessage{
				Role:      role,
				Content:   content,
				ToolCalls: toolCalls,
			})
		} else {
			flushText(role)
		}
	}

	// tools 转换
	var openaiTools []OpenAITool
	if len(body.Tools) > 0 {
		for _, t := range body.Tools {
			params := t.InputSchema
			if len(params) == 0 {
				params = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			openaiTools = append(openaiTools, OpenAITool{
				Type: "function",
				Function: OpenAIFunction{
					Name:        t.Name,
					Description: t.Description,
					Parameters:  params,
				},
			})
		}
	}

	return messages, openaiTools
}

// OpenAIToAnthropic OpenAI Chat Completions 响应 → Anthropic Messages 响应
func OpenAIToAnthropic(oai *OpenAIResponse, reqID string) *AnthropicResponse {
	choice := &OpenAIChoice{}
	if len(oai.Choices) > 0 {
		choice = &oai.Choices[0]
	}
	msg := choice.Message

	var content []AnthropicContentBlock

	// reasoning_content -> text block（推理模型思维链，如 JoyAI-Code-1.5 输出主要在此）
	if msg.ReasoningContent != "" {
		content = append(content, AnthropicContentBlock{
			Type: "text",
			Text: msg.ReasoningContent,
		})
	}
	// tool_calls → tool_use blocks
	if len(msg.ToolCalls) > 0 {
		for _, tc := range msg.ToolCalls {
			content = append(content, AnthropicContentBlock{
				Type:  "tool_use",
				ID:    tc.ID,
				Name:  tc.Function.Name,
				Input: json.RawMessage(safeToolArgs(json.RawMessage(tc.Function.Arguments))),
			})
		}
		if msg.Content != nil && *msg.Content != "" {
			content = append(content, AnthropicContentBlock{
				Type: "text",
				Text: *msg.Content,
			})
		}
	} else if msg.Content != nil && *msg.Content != "" {
		content = append(content, AnthropicContentBlock{
			Type: "text",
			Text: *msg.Content,
		})
	}

	// 兜底：确保 content 不是 nil（nil 序列化为 null，Anthropic 规范要求是数组）
	if content == nil {
		content = []AnthropicContentBlock{}
	}

	stopReason := "end_turn"
	if choice.FinishReason == "tool_calls" {
		stopReason = "tool_use"
	} else if choice.FinishReason == "length" {
		stopReason = "max_tokens"
	}

	usage := AnthropicUsage{}
	if oai.Usage != nil {
		usage.InputTokens = oai.Usage.PromptTokens
		usage.OutputTokens = oai.Usage.CompletionTokens
	}

	// 上游响应未回传 model 时保持空串，不再硬编码成 glm-5.1（避免误报实际模型）
	model := oai.Model

	return &AnthropicResponse{
		ID:         fmt.Sprintf("msg_%s", reqID),
		Type:       "message",
		Role:       "assistant",
		Model:      model,
		Content:    content,
		StopReason: stopReason,
		Usage:      usage,
	}
}

// SafeJSONParse 安全解析 JSON
func SafeJSONParse(data []byte, v interface{}) {
	_ = json.Unmarshal(data, v)
}

// extractTextFromContent 从 Anthropic content 字段提取纯文本
// content 可能是 JSON 字符串、[]content block（含 text 字段），或其他格式
func extractTextFromContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// 1. 纯字符串
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	// 2. []block（可能含多段 text，以及 image 等非文本块——只取 text）
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var parts []string
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	// 3. 兜底：去掉 JSON 转义后返回原文
	return string(raw)
}

// safeToolArgs 把 tool_use 的 input 转成合法的 JSON object 字符串
// 空/非法输入兜底为 "{}"，避免上游收到非法 JSON
func safeToolArgs(input json.RawMessage) string {
	if len(input) == 0 {
		return "{}"
	}
	s := strings.TrimSpace(string(input))
	if s == "" || s == "null" {
		return "{}"
	}
	// 校验是合法 JSON
	var v interface{}
	if err := json.Unmarshal(input, &v); err != nil {
		return "{}"
	}
	return s
}
