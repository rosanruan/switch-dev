package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"switchdev/creds"
	"switchdev/pricing"
	"switchdev/upstream"
)

// sourceFromUA 从 User-Agent 推断请求来源
func sourceFromUA(ua string) string {
	low := strings.ToLower(ua)
	switch {
	case strings.Contains(low, "claude-cli"), strings.Contains(low, "anthropic"):
		return "Claude Code"
	case strings.Contains(low, "codex"), strings.Contains(low, "openai/"):
		return "Codex"
	case strings.Contains(low, "go-http-client"):
		return "测评"
	case strings.Contains(low, "curl"):
		return "curl"
	case strings.Contains(low, "python"):
		return "Python"
	case strings.Contains(low, "node"):
		return "Node"
	case ua == "":
		return "未知"
	default:
		if len(ua) > 30 {
			return ua[:30]
		}
		return ua
	}
}

// setSource 给日志条目设置请求来源和原始 User-Agent（若为空则从请求 UA 推断）
func setSource(entry *LogEntry, r *http.Request) {
	if r != nil {
		ua := r.Header.Get("User-Agent")
		entry.Source = sourceFromUA(ua)
		entry.UserAgent = ua
	}
}

// 入站请求体上限（50MB，防 OOM；长上下文 Claude Code 请求通常 < 10MB）
const maxRequestBodySize = 50 << 20

// maxStreamCapture 流式响应体捕获上限（64KB，仅日志用，避免内存膨胀）
const maxStreamCapture = 64 << 10

// captureStreamBody 包装 io.Reader，在读取时同时捕获前 maxStreamCapture 字节到 buf
func captureStreamBody(r io.Reader) (io.Reader, *bytes.Buffer) {
	var buf bytes.Buffer
	return io.TeeReader(r, &limitedWriter{w: &buf, n: maxStreamCapture}), &buf
}

// limitedWriter 最多写入 n 字节，超出后静默丢弃
type limitedWriter struct {
	w io.Writer
	n int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.n <= 0 {
		return len(p), nil
	}
	if len(p) > l.n {
		l.w.Write(p[:l.n])
		l.n = 0
		return len(p), nil
	}
	l.n -= len(p)
	return l.w.Write(p)
}

// makeLogEntry 构造日志条目（公共字段）
func (s *Server) makeLogEntry(model, up, status string, code int, duration int64, errMsg, method, path string, stream bool, reqBody, respBody string) *LogEntry {
	return &LogEntry{
		Model:        model,
		Upstream:     up,
		Status:       status,
		Code:         code,
		Duration:     duration,
		ErrorMsg:     errMsg,
		Method:       method,
		Path:         path,
		Stream:       stream,
		RequestBody:  reqBody,
		ResponseBody: respBody,
	}
}

// enrichUsage 从响应体提取 token 用量 + 真实模型，计算费用
func (s *Server) enrichUsage(entry *LogEntry, respBody []byte, requestedModel string) {
	if len(respBody) == 0 {
		return
	}
	var oai OpenAIResponse
	if err := json.Unmarshal(respBody, &oai); err != nil {
		return
	}
	if oai.Usage != nil {
		entry.InputTokens = oai.Usage.PromptTokens
		entry.OutputTokens = oai.Usage.CompletionTokens

		// usage 合理性钳制：某些上游（如 OpenCode Zen）偶发返回远超 context 上限的 inputTokens
		// 明显异常时清零，避免污染今日统计 / 趋势图 / 费率
		limit := modelContextLimit(ResolveModel(requestedModel))
		if limit > 0 && entry.InputTokens > limit {
			entry.InputTokens = 0
		}

		// 缓存命中 token（两种格式兼容）
		if oai.Usage.PromptTokensDetails.CachedTokens > 0 {
			entry.CacheHitTokens = oai.Usage.PromptTokensDetails.CachedTokens
		} else if oai.Usage.CacheReadInputTokens > 0 {
			entry.CacheHitTokens = oai.Usage.CacheReadInputTokens
		}
	}
	if oai.Model != "" {
		entry.RealModel = oai.Model
	}

	// 计算费用（用自有费率库）
	// 优先用 usedModel（降级链实际命中的模型，最可靠的费率查找键）
	// 其次用 resolve 后的请求模型，最后用真实模型（费率表可能是标准名）
	if s.Pricing != nil {
		cost, price := 0.0, (*pricing.Price)(nil)
		if entry.UsedModel != "" {
			cost, price = s.Pricing.CalculateCost(ActualUpstreamModel(entry.UsedModel), entry.InputTokens, entry.OutputTokens)
		}
		if price == nil {
			lookupModel := ResolveModel(requestedModel)
			cost, price = s.Pricing.CalculateCost(lookupModel, entry.InputTokens, entry.OutputTokens)
			if price == nil && entry.RealModel != "" && entry.RealModel != lookupModel {
				cost2, price2 := s.Pricing.CalculateCost(entry.RealModel, entry.InputTokens, entry.OutputTokens)
				if price2 != nil {
					cost, price = cost2, price2
				}
			}
		}
		if price != nil {
			entry.Cost = cost
			entry.CostText = fmt.Sprintf("%s", price.DisplayName)
		}
	}
}

// logRequest 记录日志（含用量/费用富化）
func (s *Server) logRequest(entry *LogEntry, respBody []byte, requestedModel string) {
	s.enrichUsage(entry, respBody, requestedModel)
	s.recordLog(entry)
}

// handleAnthropicMessages Anthropic /v1/messages 入口
func (s *Server) handleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBodySize))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	var body AnthropicRequest
	if err := json.Unmarshal(raw, &body); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("bad json"))
		return
	}

	// 默认流式：stream 未传(nil)时视为 true
	stream := true
	if body.Stream != nil {
		stream = *body.Stream
	}
	requestedModel := body.Model
	if requestedModel == "" {
		requestedModel = "auto"
	}
	source := sourceFromUA(r.Header.Get("User-Agent"))
	userAgent := r.Header.Get("User-Agent")

	// 流式：优先尝试真流式（上游支持 StreamCaller，如 WorkBuddy/OpenCode）
	if stream {
		sr, upName, usedModel, err := s.callUpstreamStream(r.Context(), &body, userAgent)
		if sr != nil {
			// 上游本身就是 Anthropic 协议：SSE 直接透传（同协议）
			if s.upstreamProtocolByName(upName) == "anthropic" {
				s.streamAnthropicPassthrough(w, sr, requestedModel, upName, usedModel, source, userAgent, string(raw))
				return
			}
			s.streamAnthropicResponse(w, sr, requestedModel, upName, usedModel, source, userAgent, string(raw))
			return
		}
		if err != nil {
			entry := s.makeLogEntry(requestedModel, upName, "error", 0, 0, err.Error(), "POST", "/v1/messages", true, string(raw), "")
			entry.Source = source
			entry.UserAgent = userAgent
			s.recordLog(entry)
			writeAnthropicError(w, http.StatusBadGateway, "connection_error", err.Error())
			return
		}
		// sr == nil：无流式上游可用，回退伪流式
	}

	start := time.Now()
	resp, upName, usedModel, err := s.callUpstreamAnthropic(r.Context(), &body, userAgent)
	duration := time.Since(start).Milliseconds()
	reqBodyStr := string(raw)

	if err != nil {
		var credErr *creds.CredentialsError
		if errors.As(err, &credErr) {
			e := s.makeLogEntry(requestedModel, upName, "auth_error", 0, duration, credErr.Error(), "POST", "/v1/messages", stream, reqBodyStr, "")
			e.Source = source
			e.UserAgent = userAgent
			s.recordLog(e)
			writeAnthropicError(w, http.StatusBadGateway, "authentication_error", credErr.Error())
		} else {
			e := s.makeLogEntry(requestedModel, upName, "error", 0, duration, err.Error(), "POST", "/v1/messages", stream, reqBodyStr, "")
			e.Source = source
			e.UserAgent = userAgent
			s.recordLog(e)
			writeAnthropicError(w, http.StatusBadGateway, "connection_error", err.Error())
		}
		return
	}

	trimmed := strings.TrimSpace(string(resp.Body))

	// 检测响应类型（成功 / 上游错误 / 不可解析）
	var generic map[string]interface{}
	parseErr := json.Unmarshal([]byte(trimmed), &generic)
	hasChoices := parseErr == nil && generic["choices"] != nil

	// 上游本身就是 Anthropic 协议：响应是 Anthropic 格式（含 content/stop_reason/type=message），
	// 直接透传给客户端，不做 OpenAI->Anthropic 回转。
	if s.upstreamProtocolByName(upName) == "anthropic" && parseErr == nil &&
		(generic["content"] != nil || generic["stop_reason"] != nil || generic["type"] == "message") {
		entry := s.makeLogEntry(requestedModel, upName, "success", resp.StatusCode, duration, "", "POST", "/v1/messages", stream, reqBodyStr, trimmed)
		entry.UsedModel = usedModel
		entry.Source = source
		entry.UserAgent = userAgent
		// 从 Anthropic usage 提取 token 用量
		if u, ok := generic["usage"].(map[string]interface{}); ok {
			if v, ok := u["input_tokens"].(float64); ok {
				entry.InputTokens = int(v)
			}
			if v, ok := u["output_tokens"].(float64); ok {
				entry.OutputTokens = int(v)
			}
		}
		s.logRequest(entry, resp.Body, requestedModel)
		w.Header().Set("Content-Type", "application/json")
		w.Write(resp.Body)
		return
	}

	if !hasChoices {
		// 检查是否为错误响应
		hasErrorField := parseErr == nil && generic["error"] != nil
		hasCode := parseErr == nil && generic["code"] != nil
		hasErrorCode := parseErr == nil && generic["errorCode"] != nil

		if !hasErrorField && !hasCode && !hasErrorCode {
			// 不可解析
			snippet := trimmed
			if len(snippet) > 200 {
				snippet = snippet[:200]
			}
			s.recordLog(func() *LogEntry {
				e := s.makeLogEntry(requestedModel, upName, "error", resp.StatusCode, duration, "unparsable upstream", "POST", "/v1/messages", stream, reqBodyStr, trimmed)
				e.Source = source
				e.UserAgent = userAgent
				return e
			}())
			writeAnthropicError(w, http.StatusBadGateway, "upstream_error", "unparsable upstream: "+snippet)
			return
		}

		// 上游错误
		errMsg, errCode := extractUpstreamError([]byte(trimmed))
		isDenied := strings.Contains(errMsg, "访问受限") || strings.Contains(strings.ToLower(errCode), "denied")
		fmt.Printf("[switch-dev] 上游错误: [%s] %s\n", errCode, errMsg)
		e := s.makeLogEntry(requestedModel, upName, "error", resp.StatusCode, duration, fmt.Sprintf("[%s] %s", errCode, errMsg), "POST", "/v1/messages", stream, reqBodyStr, trimmed)
		e.Source = source
		e.UserAgent = userAgent
		s.recordLog(e)
		errType := "upstream_error"
		if isDenied {
			errType = "permission_error"
		}
		writeAnthropicError(w, http.StatusBadGateway, errType, fmt.Sprintf("[%s] %s", errCode, errMsg))
		return
	}

	// 成功响应（富化用量/费用/真实模型后记录；model 存请求模型，usedModel 存实际模型）
	entry := s.makeLogEntry(requestedModel, upName, "success", resp.StatusCode, duration, "", "POST", "/v1/messages", stream, reqBodyStr, trimmed)
	entry.UsedModel = usedModel
	entry.Source = source
	entry.UserAgent = userAgent
	s.logRequest(entry, resp.Body, requestedModel)

	var parsed OpenAIResponse
	if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "upstream_error", "parse success response failed")
		return
	}

	// 空内容检测：choices 为空或 message 内容全空 → 502（触发降级链下一模型）
	if isResponseContentEmpty(&parsed) {
		e := s.makeLogEntry(requestedModel, upName, "error", resp.StatusCode, duration, "empty content in upstream response", "POST", "/v1/messages", stream, reqBodyStr, trimmed)
		e.Source = source
		e.UserAgent = userAgent
		s.recordLog(e)
		writeAnthropicError(w, http.StatusBadGateway, "upstream_error", "上游返回空内容")
		return
	}

	if stream {
		setSSEHeaders(w)
		model := parsed.Model
		if model == "" {
			model = body.Model
		}
		WriteAnthropicSSE(w, &parsed, model)
	} else {
		ant := OpenAIToAnthropic(&parsed, resp.ReqID)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(ant)
	}
}

// handleOpenAIChatCompletions OpenAI /v1/chat/completions 直通入口
func (s *Server) handleOpenAIChatCompletions(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBodySize))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	var body map[string]interface{}
	if err := json.Unmarshal(raw, &body); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("bad json"))
		return
	}

	// 默认流式：stream 未传时视为 true
	streamVal, hasStream := body["stream"]
	stream := true
	if hasStream {
		if b, ok := streamVal.(bool); ok {
			stream = b
		}
	}
	requestedModel, _ := body["model"].(string)
	if requestedModel == "" {
		requestedModel = "auto"
	}
	source := sourceFromUA(r.Header.Get("User-Agent"))
	userAgent := r.Header.Get("User-Agent")

	// 流式：优先尝试真流式（上游支持 StreamCaller，如 WorkBuddy/OpenCode）
	if stream {
		sr, upName, usedModel, err := s.callUpstreamStream(r.Context(), body, userAgent)
		if sr != nil {
			s.streamOpenAIResponse(w, sr, requestedModel, upName, usedModel, source, userAgent, string(raw))
			return
		}
		if err != nil {
			e := s.makeLogEntry(requestedModel, upName, "error", 0, 0, err.Error(), "POST", "/v1/chat/completions", true, string(raw), "")
			e.Source = source
			e.UserAgent = userAgent
			s.recordLog(e)
			writeOpenAIError(w, http.StatusBadGateway, err.Error())
			return
		}
		// sr == nil：无流式上游可用，回退伪流式
	}

	start := time.Now()
	resp, upName, usedModel, err := s.callUpstreamOpenAI(r.Context(), body, userAgent)
	duration := time.Since(start).Milliseconds()
	reqBodyStr := string(raw)

	if err != nil {
		var credErr *creds.CredentialsError
		if errors.As(err, &credErr) {
			e := s.makeLogEntry(requestedModel, upName, "auth_error", 0, duration, credErr.Error(), "POST", "/v1/chat/completions", stream, reqBodyStr, "")
			e.Source = source
			e.UserAgent = userAgent
			s.recordLog(e)
			writeOpenAIError(w, http.StatusBadGateway, credErr.Error())
		} else {
			e := s.makeLogEntry(requestedModel, upName, "error", 0, duration, err.Error(), "POST", "/v1/chat/completions", stream, reqBodyStr, "")
			e.Source = source
			e.UserAgent = userAgent
			s.recordLog(e)
			writeOpenAIError(w, http.StatusBadGateway, err.Error())
		}
		return
	}

	// 成功：model 存请求模型，usedModel 存实际模型
	entry := s.makeLogEntry(requestedModel, upName, "success", resp.StatusCode, duration, "", "POST", "/v1/chat/completions", stream, reqBodyStr, string(resp.Body))
	entry.UsedModel = usedModel
	entry.Source = source
	entry.UserAgent = userAgent
	s.logRequest(entry, resp.Body, requestedModel)

	if stream {
		setSSEHeaders(w)
		var parsed OpenAIResponse
		if err := json.Unmarshal(resp.Body, &parsed); err == nil {
			WriteOpenAISSE(w, &parsed)
		} else {
			// 解析失败，原样回
			fmt.Fprintf(w, "data: %s\n\n", string(resp.Body))
			fmt.Fprintf(w, "data: [DONE]\n\n")
		}
	} else {
		w.Header().Set("Content-Type", "application/json")
		w.Write(resp.Body)
	}
}

// isResponseContentEmpty 检测上游响应是否没有有效内容
// choices 为空，或唯一的 choice 的 message 内容全空（无 text、无 tool_calls、无 reasoning）
func isResponseContentEmpty(oai *OpenAIResponse) bool {
	if len(oai.Choices) == 0 {
		return true
	}
	msg := oai.Choices[0].Message
	hasText := msg.Content != nil && *msg.Content != ""
	hasToolCalls := len(msg.ToolCalls) > 0
	hasReasoning := msg.ReasoningContent != ""
	return !hasText && !hasToolCalls && !hasReasoning
}

// setSSEHeaders 设置 SSE 流式响应头（含防中间件截断标记）
func setSSEHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
}

// writeAnthropicError 写 Anthropic 格式错误响应
func writeAnthropicError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"type": "error",
		"error": map[string]interface{}{
			"type":    errType,
			"message": message,
		},
	})
}

// writeAnthropicSSEError 在已发送 SSE 头（200）的流上，通过 SSE error 事件发送错误
func writeAnthropicSSEError(w http.ResponseWriter, errType, message string) {
	fmt.Fprintf(w, "event: error\ndata: %s\n\n", func() string {
		b, _ := json.Marshal(map[string]interface{}{
			"type": "error",
			"error": map[string]interface{}{
				"type":    errType,
				"message": message,
			},
		})
		return string(b)
	}())
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// writeOpenAIError 写 OpenAI 格式错误响应
func writeOpenAIError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]interface{}{
			"message": message,
		},
	})
}

// writeOpenAISSEError 在已发送 SSE 头（200）的流上，通过 SSE error 事件发送 OpenAI 格式错误
func writeOpenAISSEError(w http.ResponseWriter, message string) {
	fmt.Fprintf(w, "event: error\ndata: %s\n\n", func() string {
		b, _ := json.Marshal(map[string]interface{}{
			"error": map[string]interface{}{
				"message": message,
			},
		})
		return string(b)
	}())
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// fillStreamLog 填充流式日志的 usage/费用字段（从转换器捕获的 usage）
func (s *Server) fillStreamLog(entry *LogEntry, usage *OpenAIUsage, requestedModel string) {
	if usage != nil {
		entry.InputTokens = usage.PromptTokens
		entry.OutputTokens = usage.CompletionTokens
		if usage.PromptTokensDetails.CachedTokens > 0 {
			entry.CacheHitTokens = usage.PromptTokensDetails.CachedTokens
		} else if usage.CacheReadInputTokens > 0 {
			entry.CacheHitTokens = usage.CacheReadInputTokens
		}
	}
	if s.Pricing != nil {
		cost, price := 0.0, (*pricing.Price)(nil)
		if entry.UsedModel != "" {
			cost, price = s.Pricing.CalculateCost(ActualUpstreamModel(entry.UsedModel), entry.InputTokens, entry.OutputTokens)
		}
		if price == nil {
			lookupModel := ResolveModel(requestedModel)
			cost, price = s.Pricing.CalculateCost(lookupModel, entry.InputTokens, entry.OutputTokens)
		}
		if price == nil && entry.RealModel != "" {
			cost2, price2 := s.Pricing.CalculateCost(entry.RealModel, entry.InputTokens, entry.OutputTokens)
			if price2 != nil {
				cost, price = cost2, price2
			}
		}
		if price != nil {
			entry.Cost = cost
			entry.CostText = fmt.Sprintf("%s", price.DisplayName)
		}
	}
}

// streamAnthropicPassthrough 真流式转发：上游 Anthropic SSE -> 客户端 Anthropic SSE（同协议透传）
func (s *Server) streamAnthropicPassthrough(w http.ResponseWriter, sr *upstream.StreamResponse, requestedModel, upName, usedModel, source, userAgent, reqBodyStr string) {
	defer sr.Body.Close()
	start := time.Now()
	setSSEHeaders(w)

	streamR, respBuf := captureStreamBody(sr.Body)
	usage, firstByteMs, _ := StreamAnthropicPassthrough(w, streamR)
	duration := time.Since(start).Milliseconds()
	respBody := respBuf.String()

	if firstByteMs == 0 {
		writeAnthropicSSEError(w, "upstream_error", "上游返回空流或连接中断")
		entry := s.makeLogEntry(requestedModel, upName, "error", http.StatusBadGateway, duration, "empty stream", "POST", "/v1/messages", true, reqBodyStr, respBody)
		entry.UsedModel = usedModel
		entry.Source = source
		entry.UserAgent = userAgent
		s.recordLog(entry)
		return
	}

	entry := s.makeLogEntry(requestedModel, upName, "success", sr.StatusCode, duration, "", "POST", "/v1/messages", true, reqBodyStr, respBody)
	entry.UsedModel = usedModel
	entry.Source = source
	entry.UserAgent = userAgent
	entry.FirstByteMs = firstByteMs
	s.fillStreamLog(entry, usage, requestedModel)
	s.recordLog(entry)
}

// streamAnthropicResponse 真流式转发：上游 OpenAI SSE 流 -> Anthropic SSE 事件
func (s *Server) streamAnthropicResponse(w http.ResponseWriter, sr *upstream.StreamResponse, requestedModel, upName, usedModel, source, userAgent, reqBodyStr string) {
	defer sr.Body.Close()
	start := time.Now()
	setSSEHeaders(w)

	streamR, respBuf := captureStreamBody(sr.Body)
	usage, firstByteMs, realModel, streamErr := StreamOpenAIToAnthropic(w, streamR, requestedModel)
	duration := time.Since(start).Milliseconds()
	respBody := respBuf.String()

	if firstByteMs == 0 {
		errMsg := fmt.Sprintf("empty stream (status=%d, dur=%dms)", sr.StatusCode, duration)
		if streamErr != nil {
			errMsg += ": " + streamErr.Error()
		}
		fmt.Printf("[switch-dev] 流式空转: up=%s model=%s %s\n", upName, requestedModel, errMsg)
		writeAnthropicSSEError(w, "upstream_error", "上游返回空流或连接中断")
		entry := s.makeLogEntry(requestedModel, upName, "error", http.StatusBadGateway, duration, errMsg, "POST", "/v1/messages", true, reqBodyStr, respBody)
		entry.UsedModel = usedModel
		entry.Source = source
		entry.UserAgent = userAgent
		if realModel != "" {
			entry.RealModel = realModel
		}
		s.recordLog(entry)
		return
	}

	entry := s.makeLogEntry(requestedModel, upName, "success", sr.StatusCode, duration, "", "POST", "/v1/messages", true, reqBodyStr, respBody)
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

// streamOpenAIResponse 真流式转发：上游 OpenAI SSE 流透传给客户端
func (s *Server) streamOpenAIResponse(w http.ResponseWriter, sr *upstream.StreamResponse, requestedModel, upName, usedModel, source, userAgent, reqBodyStr string) {
	defer sr.Body.Close()
	start := time.Now()
	setSSEHeaders(w)

	streamR, respBuf := captureStreamBody(sr.Body)
	usage, firstByteMs, _ := StreamOpenAIPassthrough(w, streamR)
	duration := time.Since(start).Milliseconds()
	respBody := respBuf.String()

	// 没发任何事件 -> 上游空流或连接中断，返回 502（避免 200 + 空 body）
	if firstByteMs == 0 {
		writeOpenAISSEError(w, "上游返回空流或连接中断")
		entry := s.makeLogEntry(requestedModel, upName, "error", http.StatusBadGateway, duration, "empty stream", "POST", "/v1/chat/completions", true, reqBodyStr, respBody)
		entry.UsedModel = usedModel
		entry.Source = source
		entry.UserAgent = userAgent
		s.recordLog(entry)
		return
	}

	entry := s.makeLogEntry(requestedModel, upName, "success", sr.StatusCode, duration, "", "POST", "/v1/chat/completions", true, reqBodyStr, respBody)
	entry.UsedModel = usedModel
	entry.Source = source
	entry.UserAgent = userAgent
	entry.FirstByteMs = firstByteMs
	s.fillStreamLog(entry, usage, requestedModel)
	s.recordLog(entry)
}
