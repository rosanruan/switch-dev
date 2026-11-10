package proxy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ====== OpenAI Responses API 请求/响应结构（Codex 使用的子集） ======
// 参考：https://platform.openai.com/docs/api-reference/responses

// ResponsesRequest POST /v1/responses 请求
type ResponsesRequest struct {
	Model           string          `json:"model"`
	Input           json.RawMessage `json:"input"` // string | []ResponseInputItem
	Instructions    string          `json:"instructions,omitempty"`
	Stream          bool            `json:"stream,omitempty"`
	Tools           []ResponseTool  `json:"tools,omitempty"`
	ToolChoice      interface{}     `json:"tool_choice,omitempty"`
	Temperature     *float64        `json:"temperature,omitempty"`
	TopP            *float64        `json:"top_p,omitempty"`
	MaxOutputTokens int             `json:"max_output_tokens,omitempty"`
	// PreviousResponseID / store / reasoning 等高级字段忽略
}

// ResponseTool function 类型工具
type ResponseTool struct {
	Type        string          `json:"type"` // 目前只处理 function
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// ResponseInputItem input 数组中的一项
type ResponseInputItem struct {
	Type    string          `json:"type"` // message | function_call | function_call_output
	Role    string          `json:"role,omitempty"`
	Content json.RawMessage `json:"content,omitempty"` // string | []content part
	// function_call（assistant 历史）
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	// function_call_output（工具结果）
	Output string `json:"output,omitempty"`
}

// responseInputContentPart 消息内容片段
type responseInputContentPart struct {
	Type string `json:"type"` // input_text | output_text | input_image ...
	Text string `json:"text,omitempty"`
}

// ====== 非流式响应 ======

// ResponsesResponse /v1/responses 成功响应
type ResponsesResponse struct {
	ID        string           `json:"id"`
	Object    string           `json:"object"`
	CreatedAt int64            `json:"created_at"`
	Model     string           `json:"model"`
	Output    []ResponseOutput `json:"output"`
	Usage     *ResponsesUsage  `json:"usage,omitempty"`
}

// ResponseOutput output 数组项（message 或 function_call）
type ResponseOutput struct {
	Type      string                  `json:"type"` // message | function_call
	ID        string                  `json:"id,omitempty"`
	Role      string                  `json:"role,omitempty"`
	Content   []ResponseOutputContent `json:"content,omitempty"`
	CallID    string                  `json:"call_id,omitempty"`
	Name      string                  `json:"name,omitempty"`
	Arguments string                  `json:"arguments,omitempty"`
	Status    string                  `json:"status,omitempty"`
}

// ResponseOutputContent message 内容片段
type ResponseOutputContent struct {
	Type string `json:"type"` // output_text
	Text string `json:"text"`
}

// ResponsesUsage token 用量
type ResponsesUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// responsesToChatBody 把 Responses 请求转成 OpenAI Chat Completions 的 map
// 复用 buildOpenAIPassthroughBody 的模型映射逻辑（deveco upstream 名、wb/ 前缀等）
func responsesToChatBody(req *ResponsesRequest) (map[string]interface{}, error) {
	messages, err := responsesInputToMessages(req.Input, req.Instructions)
	if err != nil {
		return nil, err
	}

	body := map[string]interface{}{
		"model":    req.Model,
		"messages": messages,
		"stream":   false, // 上游统一非流式；流式由代理伪流式拆分
	}
	if req.MaxOutputTokens > 0 {
		body["max_tokens"] = req.MaxOutputTokens
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		body["top_p"] = *req.TopP
	}
	if len(req.Tools) > 0 {
		tools := make([]map[string]interface{}, 0, len(req.Tools))
		for _, t := range req.Tools {
			if t.Type != "function" {
				continue // 只支持 function 工具（hosted tools/MCP 不转发）
			}
			params := t.Parameters
			if len(params) == 0 {
				params = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			tools = append(tools, map[string]interface{}{
				"type": "function",
				"function": map[string]interface{}{
					"name":        t.Name,
					"description": t.Description,
					"parameters":  params,
				},
			})
		}
		if len(tools) > 0 {
			body["tools"] = tools
		}
		if tc := convertResponsesToolChoice(req.ToolChoice); tc != nil {
			body["tool_choice"] = tc
		}
	}
	return body, nil
}

// convertResponsesToolChoice 把 Responses API 的 tool_choice 转成 Chat Completions 格式
//   - 字符串 "none"/"auto"/"required"：原样透传
//   - 对象 {"type":"function","name":"x"}：转成 {"type":"function","function":{"name":"x"}}
//   - 其他/缺失：返回 nil（不设置，由上游默认）
func convertResponsesToolChoice(raw interface{}) interface{} {
	if raw == nil {
		return nil
	}
	// 字符串形式
	if s, ok := raw.(string); ok {
		return s
	}
	// 对象形式：解析 type + name
	m, ok := raw.(map[string]interface{})
	if !ok {
		return nil
	}
	t, _ := m["type"].(string)
	name, _ := m["name"].(string)
	switch t {
	case "function":
		if name != "" {
			return map[string]interface{}{
				"type":     "function",
				"function": map[string]interface{}{"name": name},
			}
		}
		return map[string]interface{}{"type": "function"}
	case "none", "auto", "required":
		return t
	default:
		if name != "" {
			return map[string]interface{}{
				"type":     "function",
				"function": map[string]interface{}{"name": name},
			}
		}
		return nil
	}
}

// responsesInputToMessages 把 Responses input 转成 OpenAI chat messages
func responsesInputToMessages(input json.RawMessage, instructions string) ([]map[string]interface{}, error) {
	var messages []map[string]interface{}

	if instructions != "" {
		messages = append(messages, map[string]interface{}{
			"role": "system", "content": instructions,
		})
	}

	// input 可能是字符串
	var asString string
	if err := json.Unmarshal(input, &asString); err == nil {
		messages = append(messages, map[string]interface{}{
			"role": "user", "content": asString,
		})
		return messages, nil
	}

	// 否则是数组
	var items []ResponseInputItem
	if err := json.Unmarshal(input, &items); err != nil {
		return nil, fmt.Errorf("解析 input 失败: %w", err)
	}

	for _, item := range items {
		switch item.Type {
		case "message":
			messages = append(messages, responseMessageToChat(item))
		case "function_call":
			// 历史中的 assistant function_call -> assistant tool_calls
			messages = append(messages, map[string]interface{}{
				"role":    "assistant",
				"content": nil,
				"tool_calls": []map[string]interface{}{{
					"id":   item.CallID,
					"type": "function",
					"function": map[string]interface{}{
						"name":      item.Name,
						"arguments": item.Arguments,
					},
				}},
			})
		case "function_call_output":
			messages = append(messages, map[string]interface{}{
				"role": "tool", "tool_call_id": item.CallID, "content": item.Output,
			})
		case "reasoning":
			// 模型上一轮的思考摘要，作为 assistant 文本喂回（content 可能是 []summary part）
			if text := responseContentText(item.Content); text != "" {
				messages = append(messages, map[string]interface{}{
					"role": "assistant", "content": text,
				})
			}
		default:
			// 未知类型，尝试按 message 处理
			if s := responseContentText(item.Content); s != "" {
				messages = append(messages, map[string]interface{}{
					"role": "user", "content": s,
				})
			}
		}
	}
	return messages, nil
}

// responseMessageToChat Response message item -> chat message
func responseMessageToChat(item ResponseInputItem) map[string]interface{} {
	role := item.Role
	// Responses API 用 developer/system，统一映射到 system（OpenAI 兼容上游通常只认 system）
	switch role {
	case "developer", "system":
		role = "system"
	case "assistant":
		role = "assistant"
	default:
		role = "user"
	}
	text := responseContentText(item.Content)
	return map[string]interface{}{"role": role, "content": text}
}

// responseContentText 从 message content（string 或 parts 数组）提取纯文本
func responseContentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []responseInputContentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Text != "" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// openAIToResponses 把内部 OpenAIResponse 转成 Responses 格式
func openAIToResponses(oai *OpenAIResponse, reqID string) *ResponsesResponse {
	resp := &ResponsesResponse{
		ID:        "resp_" + reqID,
		Object:    "response",
		CreatedAt: oai.Created,
		Model:     oai.Model,
		Output:    []ResponseOutput{},
	}
	if oai.Usage != nil {
		resp.Usage = &ResponsesUsage{
			InputTokens:  oai.Usage.PromptTokens,
			OutputTokens: oai.Usage.CompletionTokens,
			TotalTokens:  oai.Usage.TotalTokens,
		}
	}

	if len(oai.Choices) == 0 {
		return resp
	}
	msg := oai.Choices[0].Message

	// function calls -> function_call output items
	if len(msg.ToolCalls) > 0 {
		for _, tc := range msg.ToolCalls {
			resp.Output = append(resp.Output, ResponseOutput{
				Type:      "function_call",
				ID:        tc.ID,
				CallID:    tc.ID,
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			})
		}
	}

	// 文本内容（含 reasoning_content 拼到 output_text 前面）
	var text strings.Builder
	if msg.ReasoningContent != "" {
		text.WriteString(msg.ReasoningContent)
	}
	if msg.Content != nil {
		text.WriteString(*msg.Content)
	}
	if text.Len() > 0 {
		resp.Output = append(resp.Output, ResponseOutput{
			Type: "message",
			ID:   "msg_" + reqID,
			Role: "assistant",
			Content: []ResponseOutputContent{{
				Type: "output_text",
				Text: text.String(),
			}},
		})
	}

	return resp
}

// ====== 流式 SSE 转换（OpenAI chat SSE -> Responses SSE 事件） ======

// buildResponsesCreatedEvent 构造 response.created 事件（output 为空）
func buildResponsesCreatedEvent(model, respID string, createdAt int64) map[string]interface{} {
	return map[string]interface{}{
		"type":        "response.created",
		"response":    baseResponsesResponse(model, respID, createdAt, "in_progress"),
		"sequence_id": 0,
	}
}

// buildResponsesCompletedEvent 构造 response.completed 事件（携带最终 output/usage）
func buildResponsesCompletedEvent(model, respID string, createdAt int64, output []*ResponseOutput, usage *ResponsesUsage) map[string]interface{} {
	resp := baseResponsesResponse(model, respID, createdAt, "completed")
	if output == nil {
		output = []*ResponseOutput{}
	}
	resp["output"] = output
	if usage != nil {
		resp["usage"] = usage
	} else {
		resp["usage"] = map[string]interface{}{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0}
	}
	return map[string]interface{}{"type": "response.completed", "response": resp}
}

func baseResponsesResponse(model, respID string, createdAt int64, status string) map[string]interface{} {
	return map[string]interface{}{
		"id":         "resp_" + respID,
		"object":     "response",
		"created_at": createdAt,
		"status":     status,
		"model":      model,
		"output":     []interface{}{},
	}
}

// responsesItemIndex 跟踪流式输出项的索引分配
type responsesStreamState struct {
	funcItemIndex map[int]int // openai tc.index -> output item index
	nextIndex     int
}

// StreamOpenAIToResponses 读上游 OpenAI SSE，转成 Responses SSE 写给客户端
// 返回捕获的 usage 和首字节用时（ms）
func StreamOpenAIToResponses(w io.Writer, r io.Reader, model string) (*OpenAIUsage, int64, string, error) {
	flusher, canFlush := w.(http.Flusher)
	start := time.Now()
	var firstByteMs int64
	var writeErr error

	flush := func() {
		if canFlush {
			flusher.Flush()
		}
	}
	writeEvent := func(event string, data interface{}) {
		if writeErr != nil {
			return
		}
		jsonData, _ := json.Marshal(data)
		_, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, string(jsonData))
		if err != nil && writeErr == nil {
			writeErr = err
		}
		if firstByteMs == 0 {
			firstByteMs = time.Since(start).Milliseconds()
		}
		flush()
	}

	respID := fmt.Sprintf("%d", time.Now().Unix())
	createdAt := time.Now().Unix()
	if model == "" {
		model = "unknown"
	}

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	state := &responsesStreamState{funcItemIndex: map[int]int{}}
	var capturedUsage *OpenAIUsage
	started := false
	messageStarted := false
	textItemIndex := -1
	textItemDone := false

	// 聚合最终 output，供 completed 事件使用（指针切片，流式修改会同步到最终输出）
	var finalOutput []*ResponseOutput
	textBuf := strings.Builder{}
	funcItems := map[int]*ResponseOutput{} // tc.index -> function_call item
	funcItemDone := map[int]bool{}

	ensureStarted := func() {
		if started {
			return
		}
		writeEvent("response.created", buildResponsesCreatedEvent(model, respID, createdAt))
		writeEvent("response.in_progress", map[string]interface{}{"type": "response.in_progress"})
		started = true
	}

	// finishFuncItem 补发某 function_call 的 output_item.done
	finishFuncItem := func(tcIdx int) {
		if funcItemDone[tcIdx] {
			return
		}
		idx := state.funcItemIndex[tcIdx]
		writeEvent("response.output_item.done", map[string]interface{}{
			"type": "response.output_item.done", "output_index": idx, "item": funcItems[tcIdx],
		})
		funcItemDone[tcIdx] = true
	}
	finishTextItem := func() {
		if textItemDone || !messageStarted {
			return
		}
		finalOutput[textItemIndex].Content = []ResponseOutputContent{{Type: "output_text", Text: textBuf.String()}}
		writeEvent("response.output_text.done", map[string]interface{}{
			"type": "response.output_text.done", "output_index": textItemIndex,
			"content_index": 0, "text": textBuf.String(),
		})
		writeEvent("response.content_part.done", map[string]interface{}{
			"type": "response.content_part.done", "output_index": textItemIndex,
			"content_index": 0,
			"part":          map[string]interface{}{"type": "output_text", "text": textBuf.String()},
		})
		writeEvent("response.output_item.done", map[string]interface{}{
			"type": "response.output_item.done", "output_index": textItemIndex, "item": finalOutput[textItemIndex],
		})
		textItemDone = true
	}

	for scanner.Scan() {
		if writeErr != nil {
			break // 客户端已断开
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var chunk streamOpenAIChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if chunk.Model != "" && model == "unknown" {
			model = chunk.Model
		}
		ensureStarted()

		if len(chunk.Choices) == 0 {
			if chunk.Usage != nil {
				capturedUsage = chunk.Usage
			}
			continue
		}
		c := chunk.Choices[0]
		delta := c.Delta

		// reasoning_content 作为 output_text 一并输出（Codex 能接受文本块）
		textDelta := delta.Content
		if delta.ReasoningContent != "" {
			textDelta = delta.ReasoningContent + textDelta
		}

		// function call deltas
		for _, tc := range delta.ToolCalls {
			idx, exists := state.funcItemIndex[tc.Index]
			if !exists {
				idx = state.nextIndex
				state.nextIndex++
				state.funcItemIndex[tc.Index] = idx
				item := &ResponseOutput{
					Type: "function_call", ID: tc.ID, CallID: tc.ID, Name: tc.Function.Name,
				}
				funcItems[tc.Index] = item
				finalOutput = append(finalOutput, item)
				writeEvent("response.output_item.added", map[string]interface{}{
					"type": "response.output_item.added", "output_index": idx,
					"item": map[string]interface{}{
						"type": "function_call", "id": tc.ID, "call_id": tc.ID,
						"name": tc.Function.Name, "arguments": "",
					},
				})
			} else {
				item := funcItems[tc.Index]
				if tc.ID != "" && item.ID == "" {
					item.ID = tc.ID
					item.CallID = tc.ID
				}
				if tc.Function.Name != "" && item.Name == "" {
					item.Name = tc.Function.Name
				}
			}
			if tc.Function.Arguments != "" {
				item := funcItems[tc.Index]
				item.Arguments += tc.Function.Arguments
				writeEvent("response.function_call_arguments.delta", map[string]interface{}{
					"type":         "response.function_call_arguments.delta",
					"output_index": idx, "delta": tc.Function.Arguments,
				})
			}
		}

		if textDelta != "" {
			if !messageStarted {
				messageStarted = true
				textItemIndex = state.nextIndex
				state.nextIndex++
				item := &ResponseOutput{
					Type: "message", ID: "msg_" + respID, Role: "assistant",
					Content: []ResponseOutputContent{},
				}
				finalOutput = append(finalOutput, item)
				writeEvent("response.output_item.added", map[string]interface{}{
					"type": "response.output_item.added", "output_index": textItemIndex,
					"item": map[string]interface{}{
						"type": "message", "id": "msg_" + respID, "role": "assistant",
						"content": []interface{}{},
					},
				})
				writeEvent("response.content_part.added", map[string]interface{}{
					"type": "response.content_part.added", "output_index": textItemIndex,
					"content_index": 0, "part": map[string]interface{}{"type": "output_text", "text": ""},
				})
			}
			textBuf.WriteString(textDelta)
			writeEvent("response.output_text.delta", map[string]interface{}{
				"type": "response.output_text.delta", "output_index": textItemIndex,
				"content_index": 0, "delta": textDelta,
			})
		}

		// finish_reason -> 收尾各 item
		if c.FinishReason != "" {
			if chunk.Usage != nil {
				capturedUsage = chunk.Usage
			}
			for tcIdx := range state.funcItemIndex {
				finishFuncItem(tcIdx)
			}
			finishTextItem()
		}
	}
	if err := scanner.Err(); err != nil {
		return capturedUsage, firstByteMs, model, fmt.Errorf("读取 SSE 流失败: %w", err)
	}

	// 兜底：完全没有任何输出项，补一个空文本 item
	if started && len(finalOutput) == 0 {
		idx := state.nextIndex
		item := &ResponseOutput{
			Type: "message", ID: "msg_" + respID, Role: "assistant",
			Content: []ResponseOutputContent{{Type: "output_text", Text: ""}},
		}
		finalOutput = append(finalOutput, item)
		writeEvent("response.output_item.added", map[string]interface{}{
			"type": "response.output_item.added", "output_index": idx,
			"item": map[string]interface{}{
				"type": "message", "id": "msg_" + respID, "role": "assistant",
				"content": []interface{}{},
			},
		})
		writeEvent("response.output_item.done", map[string]interface{}{
			"type": "response.output_item.done", "output_index": idx, "item": item,
		})
	} else if started {
		// 流截断（无 finish_reason）：补发未收尾的 item
		for tcIdx := range state.funcItemIndex {
			finishFuncItem(tcIdx)
		}
		finishTextItem()
	}

	var usage *ResponsesUsage
	if capturedUsage != nil {
		usage = &ResponsesUsage{
			InputTokens:  capturedUsage.PromptTokens,
			OutputTokens: capturedUsage.CompletionTokens,
			TotalTokens:  capturedUsage.TotalTokens,
		}
	}
	writeEvent("response.completed", buildResponsesCompletedEvent(model, respID, createdAt, finalOutput, usage))
	return capturedUsage, firstByteMs, model, nil
}
