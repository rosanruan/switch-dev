package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"switchdev/creds"
	"switchdev/upstream"
)

// handleResponses OpenAI /v1/responses 入口（Codex 使用）
// 内部把 Responses 请求转成 Chat Completions，复用降级链，再转回 Responses 格式
func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBodySize))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	var req ResponsesRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("bad json"))
		return
	}

	// stream 字段：Responses API 用 query 参数 ?stream=true 或 body.stream
	stream := req.Stream
	if r.URL.Query().Get("stream") == "true" {
		stream = true
	} else if r.URL.Query().Get("stream") == "false" {
		stream = false
	}

	requestedModel := req.Model
	if requestedModel == "" {
		requestedModel = "auto"
	}

	// 转成 Chat Completions body（map 形式，复用 buildOpenAIPassthroughBody 的模型映射）
	chatBody, err := responsesToChatBody(&req)
	if err != nil {
		writeResponsesError(w, http.StatusBadRequest, err.Error())
		return
	}

	reqBodyStr := string(raw)
	source := sourceFromUA(r.Header.Get("User-Agent"))
	userAgent := r.Header.Get("User-Agent")

	if stream {
		s.forwardResponsesStream(w, r, chatBody, requestedModel, source, userAgent, reqBodyStr)
		return
	}

	start := time.Now()
	resp, upName, usedModel, err := s.callUpstreamOpenAI(r.Context(), chatBody, userAgent)
	duration := time.Since(start).Milliseconds()

	if err != nil {
		s.recordResponsesError(w, requestedModel, upName, duration, err, false, source, userAgent, reqBodyStr)
		return
	}

	trimmed := strings.TrimSpace(string(resp.Body))

	var generic map[string]interface{}
	parseErr := json.Unmarshal([]byte(trimmed), &generic)
	hasChoices := parseErr == nil && generic["choices"] != nil
	if !hasChoices {
		hasErrorField := parseErr == nil && generic["error"] != nil
		hasCode := parseErr == nil && generic["code"] != nil
		if !hasErrorField && !hasCode {
			snippet := trimmed
			if len(snippet) > 200 {
				snippet = snippet[:200]
			}
			e := s.makeLogEntry(requestedModel, upName, "error", resp.StatusCode, duration, "unparsable upstream", "POST", "/v1/responses", false, reqBodyStr, trimmed)
			e.Source = source
			e.UserAgent = userAgent
			s.recordLog(e)
			writeResponsesError(w, http.StatusBadGateway, "unparsable upstream: "+snippet)
			return
		}
		errMsg, errCode := extractUpstreamError([]byte(trimmed))
		e := s.makeLogEntry(requestedModel, upName, "error", resp.StatusCode, duration, "["+errCode+"] "+errMsg, "POST", "/v1/responses", false, reqBodyStr, trimmed)
		e.Source = source
		e.UserAgent = userAgent
		s.recordLog(e)
		writeResponsesError(w, http.StatusBadGateway, "["+errCode+"] "+errMsg)
		return
	}

	var parsed OpenAIResponse
	if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
		writeResponsesError(w, http.StatusBadGateway, "parse success response failed")
		return
	}

	if isResponseContentEmpty(&parsed) {
		e := s.makeLogEntry(requestedModel, upName, "error", resp.StatusCode, duration, "empty content in upstream response", "POST", "/v1/responses", false, reqBodyStr, trimmed)
		e.Source = source
		e.UserAgent = userAgent
		s.recordLog(e)
		writeResponsesError(w, http.StatusBadGateway, "上游返回空内容")
		return
	}

	entry := s.makeLogEntry(requestedModel, upName, "success", resp.StatusCode, duration, "", "POST", "/v1/responses", false, reqBodyStr, trimmed)
	entry.UsedModel = usedModel
	entry.Source = source
	entry.UserAgent = userAgent
	s.logRequest(entry, resp.Body, requestedModel)

	out := openAIToResponses(&parsed, resp.ReqID)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// forwardResponsesStream 流式：真流式优先（StreamCaller），否则伪流式
func (s *Server) forwardResponsesStream(w http.ResponseWriter, r *http.Request, chatBody map[string]interface{}, requestedModel, source, userAgent, reqBodyStr string) {
	// 真流式
	sr, upName, usedModel, err := s.callUpstreamStream(r.Context(), chatBody, userAgent)
	if sr != nil {
		s.streamResponsesResponse(w, sr, requestedModel, upName, usedModel, source, userAgent, reqBodyStr)
		return
	}
	if err != nil {
		s.recordResponsesError(w, requestedModel, upName, 0, err, true, source, userAgent, reqBodyStr)
		return
	}

	// 伪流式：非流式 Call 拿到完整响应，再拆成 Responses SSE
	start := time.Now()
	resp, upName, usedModel, err := s.callUpstreamOpenAI(r.Context(), chatBody, userAgent)
	duration := time.Since(start).Milliseconds()

	if err != nil {
		s.recordResponsesError(w, requestedModel, upName, duration, err, true, source, userAgent, reqBodyStr)
		return
	}

	trimmed := strings.TrimSpace(string(resp.Body))
	var parsed OpenAIResponse
	if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
		e := s.makeLogEntry(requestedModel, upName, "error", resp.StatusCode, duration, "parse failed", "POST", "/v1/responses", true, reqBodyStr, trimmed)
		e.Source = source
		e.UserAgent = userAgent
		s.recordLog(e)
		writeResponsesError(w, http.StatusBadGateway, "parse success response failed")
		return
	}
	if isResponseContentEmpty(&parsed) {
		e := s.makeLogEntry(requestedModel, upName, "error", resp.StatusCode, duration, "empty content", "POST", "/v1/responses", true, reqBodyStr, trimmed)
		e.Source = source
		e.UserAgent = userAgent
		s.recordLog(e)
		writeResponsesError(w, http.StatusBadGateway, "上游返回空内容")
		return
	}

	entry := s.makeLogEntry(requestedModel, upName, "success", resp.StatusCode, duration, "", "POST", "/v1/responses", true, reqBodyStr, trimmed)
	entry.UsedModel = usedModel
	entry.Source = source
	entry.UserAgent = userAgent
	s.logRequest(entry, resp.Body, requestedModel)

	setSSEHeaders(w)
	writeResponsesPseudoStream(w, &parsed)
}

// streamResponsesResponse 真流式转发：上游 OpenAI SSE -> Responses SSE
func (s *Server) streamResponsesResponse(w http.ResponseWriter, sr *upstream.StreamResponse, requestedModel, upName, usedModel, source, userAgent, reqBodyStr string) {
	defer sr.Body.Close()
	start := time.Now()
	setSSEHeaders(w)

	usage, firstByteMs, realModel, _ := StreamOpenAIToResponses(w, sr.Body, requestedModel)
	duration := time.Since(start).Milliseconds()

	if firstByteMs == 0 {
		writeResponsesSSEError(w, "上游返回空流或连接中断")
		entry := s.makeLogEntry(requestedModel, upName, "error", http.StatusBadGateway, duration, "empty stream", "POST", "/v1/responses", true, reqBodyStr, "")
		entry.UsedModel = usedModel
		entry.Source = source
		entry.UserAgent = userAgent
		if realModel != "" {
			entry.RealModel = realModel
		}
		s.recordLog(entry)
		return
	}

	entry := s.makeLogEntry(requestedModel, upName, "success", sr.StatusCode, duration, "", "POST", "/v1/responses", true, reqBodyStr, "")
	entry.UsedModel = usedModel
	entry.Source = source
	entry.UserAgent = userAgent
	if realModel != "" {
		entry.RealModel = realModel
	}
	entry.FirstByteMs = firstByteMs
	s.fillStreamLog(entry, usage, requestedModel)
	s.recordLog(entry)
}

// writeResponsesPseudoStream 把完整 OpenAIResponse 拆成 Responses SSE 事件流
func writeResponsesPseudoStream(w http.ResponseWriter, oai *OpenAIResponse) {
	flusher, canFlush := w.(http.Flusher)
	write := func(event string, data interface{}) {
		jsonData, _ := json.Marshal(data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, string(jsonData))
		if canFlush {
			flusher.Flush()
		}
	}

	respID := fmt.Sprintf("%d", oai.Created)
	model := oai.Model
	createdAt := oai.Created

	write("response.created", map[string]interface{}{"type": "response.created", "response": baseResponsesResponse(model, respID, createdAt, "in_progress")})
	write("response.in_progress", map[string]interface{}{"type": "response.in_progress"})

	choice := &OpenAIChoice{}
	if len(oai.Choices) > 0 {
		choice = &oai.Choices[0]
	}
	msg := choice.Message

	idx := 0
	for _, tc := range msg.ToolCalls {
		write("response.output_item.added", map[string]interface{}{
			"type": "response.output_item.added", "output_index": idx,
			"item": map[string]interface{}{
				"type": "function_call", "id": tc.ID, "call_id": tc.ID,
				"name": tc.Function.Name, "arguments": "",
			},
		})
		write("response.function_call_arguments.delta", map[string]interface{}{
			"type":         "response.function_call_arguments.delta",
			"output_index": idx, "delta": tc.Function.Arguments,
		})
		write("response.function_call_arguments.done", map[string]interface{}{
			"type":         "response.function_call_arguments.done",
			"output_index": idx, "arguments": tc.Function.Arguments,
		})
		write("response.output_item.done", map[string]interface{}{
			"type": "response.output_item.done", "output_index": idx,
			"item": map[string]interface{}{
				"type": "function_call", "id": tc.ID, "call_id": tc.ID,
				"name": tc.Function.Name, "arguments": tc.Function.Arguments,
			},
		})
		idx++
	}

	var text strings.Builder
	if msg.ReasoningContent != "" {
		text.WriteString(msg.ReasoningContent)
	}
	if msg.Content != nil {
		text.WriteString(*msg.Content)
	}
	if text.Len() > 0 {
		write("response.output_item.added", map[string]interface{}{
			"type": "response.output_item.added", "output_index": idx,
			"item": map[string]interface{}{
				"type": "message", "id": "msg_" + respID, "role": "assistant",
				"content": []interface{}{},
			},
		})
		write("response.content_part.added", map[string]interface{}{
			"type": "response.content_part.added", "output_index": idx,
			"content_index": 0, "part": map[string]interface{}{"type": "output_text", "text": ""},
		})
		write("response.output_text.delta", map[string]interface{}{
			"type": "response.output_text.delta", "output_index": idx,
			"content_index": 0, "delta": text.String(),
		})
		write("response.output_text.done", map[string]interface{}{
			"type": "response.output_text.done", "output_index": idx,
			"content_index": 0, "text": text.String(),
		})
		write("response.content_part.done", map[string]interface{}{
			"type": "response.content_part.done", "output_index": idx,
			"content_index": 0, "part": map[string]interface{}{"type": "output_text", "text": text.String()},
		})
		write("response.output_item.done", map[string]interface{}{
			"type": "response.output_item.done", "output_index": idx,
			"item": map[string]interface{}{
				"type": "message", "id": "msg_" + respID, "role": "assistant",
				"content": []interface{}{map[string]interface{}{"type": "output_text", "text": text.String()}},
			},
		})
	}

	finalResp := baseResponsesResponse(model, respID, createdAt, "completed")
	finalResp["output"] = buildPseudoOutput(choice, respID)
	usage := map[string]interface{}{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0}
	if oai.Usage != nil {
		usage = map[string]interface{}{
			"input_tokens":  oai.Usage.PromptTokens,
			"output_tokens": oai.Usage.CompletionTokens,
			"total_tokens":  oai.Usage.TotalTokens,
		}
	}
	finalResp["usage"] = usage
	write("response.completed", map[string]interface{}{"type": "response.completed", "response": finalResp})
}

// buildPseudoOutput 构造 completed 事件的 output 数组
func buildPseudoOutput(choice *OpenAIChoice, respID string) []interface{} {
	var output []interface{}
	msg := choice.Message
	for _, tc := range msg.ToolCalls {
		output = append(output, map[string]interface{}{
			"type": "function_call", "id": tc.ID, "call_id": tc.ID,
			"name": tc.Function.Name, "arguments": tc.Function.Arguments,
		})
	}
	var text strings.Builder
	if msg.ReasoningContent != "" {
		text.WriteString(msg.ReasoningContent)
	}
	if msg.Content != nil {
		text.WriteString(*msg.Content)
	}
	if text.Len() > 0 {
		output = append(output, map[string]interface{}{
			"type": "message", "id": "msg_" + respID, "role": "assistant",
			"content": []interface{}{map[string]interface{}{"type": "output_text", "text": text.String()}},
		})
	}
	if output == nil {
		output = []interface{}{}
	}
	return output
}

// recordResponsesError 统一记录/返回错误
func (s *Server) recordResponsesError(w http.ResponseWriter, requestedModel, upName string, duration int64, err error, stream bool, source, userAgent, reqBodyStr string) {
	var credErr *creds.CredentialsError
	if errors.As(err, &credErr) {
		e := s.makeLogEntry(requestedModel, upName, "auth_error", 0, duration, credErr.Error(), "POST", "/v1/responses", stream, reqBodyStr, "")
		e.Source = source
		e.UserAgent = userAgent
		s.recordLog(e)
		writeResponsesError(w, http.StatusBadGateway, credErr.Error())
		return
	}
	e := s.makeLogEntry(requestedModel, upName, "error", 0, duration, err.Error(), "POST", "/v1/responses", stream, reqBodyStr, "")
	e.Source = source
	e.UserAgent = userAgent
	s.recordLog(e)
	writeResponsesError(w, http.StatusBadGateway, err.Error())
}

// writeResponsesError 写 Responses 格式错误
func writeResponsesError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]interface{}{
			"message": message,
			"type":    "server_error",
		},
	})
}

// writeResponsesSSEError 在已发送 SSE 头（200）的流上，通过 SSE error 事件发送 Responses 格式错误
func writeResponsesSSEError(w http.ResponseWriter, message string) {
	fmt.Fprintf(w, "event: error\ndata: %s\n\n", func() string {
		b, _ := json.Marshal(map[string]interface{}{
			"error": map[string]interface{}{
				"message": message,
				"type":    "server_error",
			},
		})
		return string(b)
	}())
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}
