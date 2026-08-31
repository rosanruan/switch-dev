package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// pendingToolCall 缓冲尚未拿到完整 id/name 的流式 tool_use block
type pendingToolCall struct {
	anthropicIdx int
	id           string
	name         string
	bufferedArgs strings.Builder
}

// streamOpenAIChunk OpenAI 流式 chunk 解析结构（供 StreamOpenAIToAnthropic / StreamOpenAIPassthrough 共用）
type streamOpenAIChunk struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Created int64  `json:"created"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role             string `json:"role"`
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *OpenAIUsage `json:"usage"`
}

// StreamOpenAIToAnthropic 读上游 OpenAI SSE 流，边转成 Anthropic SSE 事件写给客户端
// 返回捕获的 usage（供日志）、首字节用时（ms）、上游真实模型名、错误
func StreamOpenAIToAnthropic(w io.Writer, r io.Reader, model string) (*OpenAIUsage, int64, string, error) {
	flusher, canFlush := w.(http.Flusher)
	start := time.Now()
	var firstByteMs int64
	var writeErr error // 客户端断开时记录写入错误，用于早退

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
		if canFlush {
			flusher.Flush()
		}
	}

	messageStarted := false
	nextBlockIndex := 0
	textBlockIndex := -1
	reasoningBlockIndex := -1
	toolCallBlocks := map[int]int{} // tc.index -> anthropic block index
	var blockOpenOrder []int       // block 开启顺序，finish 时按此顺序关闭
	// 待启动的 tool_use block（id/name 尚未齐全时缓冲参数）
	pendingToolStart := map[int]*pendingToolCall{}
	var capturedUsage *OpenAIUsage
	// 累计输出字符数，用于上游不返回 usage 时兜底估算 output token
	var outputRunes int
	// finish 状态：finish_reason 出现时先关 block，message_delta/stop 推迟到流结束后统一发
	var finishReason string
	var finishedNormal bool

	messageID := fmt.Sprintf("msg_%d", time.Now().Unix())

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	var firstDataLines []string // 调试：记录前几行 data（firstByteMs==0 时用于排查）
	var firstParseErr error
	var firstChoicesCount int = -1
	for scanner.Scan() {
		if writeErr != nil {
			break // 客户端已断开，停止读上游
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		if len(firstDataLines) < 3 {
			firstDataLines = append(firstDataLines, data)
		}

		var chunk streamOpenAIChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			if firstParseErr == nil {
				firstParseErr = err
			}
			continue
		}
		if firstChoicesCount == -1 {
			firstChoicesCount = len(chunk.Choices)
		}
		if chunk.Model != "" && model == "" {
			model = chunk.Model
		}

		// 首个有效 chunk -> 发 message_start
		if !messageStarted && len(chunk.Choices) > 0 {
			writeEvent("message_start", map[string]interface{}{
				"type": "message_start",
				"message": map[string]interface{}{
					"id":            messageID,
					"type":          "message",
					"role":          "assistant",
					"model":         model,
					"content":       []interface{}{},
					"stop_reason":   nil,
					"stop_sequence": nil,
					"usage": map[string]interface{}{
						"input_tokens":  0,
						"output_tokens": 0,
					},
				},
			})
			messageStarted = true
		}

		if len(chunk.Choices) == 0 {
			if chunk.Usage != nil {
				capturedUsage = chunk.Usage
			}
			continue
		}
		c := chunk.Choices[0]
		delta := c.Delta

		// tool_calls -> tool_use block（按 tc.index 分配 anthropic block index）
		for _, tc := range delta.ToolCalls {
			// tool_call 参数也是输出 token 的一部分，统一计入估算
			if tc.Function.Arguments != "" {
				outputRunes += len([]rune(tc.Function.Arguments))
			}
			anthropicIdx, exists := toolCallBlocks[tc.Index]
			if !exists {
				anthropicIdx = nextBlockIndex
				nextBlockIndex++
				toolCallBlocks[tc.Index] = anthropicIdx
				blockOpenOrder = append(blockOpenOrder, anthropicIdx)
				pendingToolStart[tc.Index] = &pendingToolCall{anthropicIdx: anthropicIdx}
			}
			p := pendingToolStart[tc.Index]
			if p != nil {
				// 累积 id/name，直到齐全才发 content_block_start
				if tc.ID != "" {
					p.id = tc.ID
				}
				if tc.Function.Name != "" {
					p.name = tc.Function.Name
				}
				if p.id != "" && p.name != "" {
					writeEvent("content_block_start", map[string]interface{}{
						"type":  "content_block_start",
						"index": p.anthropicIdx,
						"content_block": map[string]interface{}{
							"type":  "tool_use",
							"id":    p.id,
							"name":  p.name,
							"input": map[string]interface{}{},
						},
					})
					// 补发缓冲的参数
					if p.bufferedArgs.Len() > 0 {
						writeEvent("content_block_delta", map[string]interface{}{
							"type":  "content_block_delta",
							"index": p.anthropicIdx,
							"delta": map[string]interface{}{
								"type":         "input_json_delta",
								"partial_json": p.bufferedArgs.String(),
							},
						})
					}
					pendingToolStart[tc.Index] = nil
				} else if tc.Function.Arguments != "" {
					// id/name 还没齐，先缓冲参数
					p.bufferedArgs.WriteString(tc.Function.Arguments)
				}
				anthropicIdx = p.anthropicIdx
			}
			if tc.Function.Arguments != "" && pendingToolStart[tc.Index] == nil {
				writeEvent("content_block_delta", map[string]interface{}{
					"type":  "content_block_delta",
					"index": anthropicIdx,
					"delta": map[string]interface{}{
						"type":         "input_json_delta",
						"partial_json": tc.Function.Arguments,
					},
				})
			}
		}

		// reasoning_content -> text block（推理模型思维链）
		if delta.ReasoningContent != "" {
			if reasoningBlockIndex == -1 {
				reasoningBlockIndex = nextBlockIndex
				nextBlockIndex++
				blockOpenOrder = append(blockOpenOrder, reasoningBlockIndex)
				writeEvent("content_block_start", map[string]interface{}{
					"type":          "content_block_start",
					"index":         reasoningBlockIndex,
					"content_block": map[string]interface{}{"type": "text", "text": ""},
				})
			}
			writeEvent("content_block_delta", map[string]interface{}{
				"type":  "content_block_delta",
				"index": reasoningBlockIndex,
				"delta": map[string]interface{}{"type": "text_delta", "text": delta.ReasoningContent},
			})
			outputRunes += len([]rune(delta.ReasoningContent))
		}

		// content -> text block
		if delta.Content != "" {
			if textBlockIndex == -1 {
				textBlockIndex = nextBlockIndex
				nextBlockIndex++
				blockOpenOrder = append(blockOpenOrder, textBlockIndex)
				writeEvent("content_block_start", map[string]interface{}{
					"type":          "content_block_start",
					"index":         textBlockIndex,
					"content_block": map[string]interface{}{"type": "text", "text": ""},
				})
			}
			writeEvent("content_block_delta", map[string]interface{}{
				"type":  "content_block_delta",
				"index": textBlockIndex,
				"delta": map[string]interface{}{"type": "text_delta", "text": delta.Content},
			})
			outputRunes += len([]rune(delta.Content))
		}

		// finish_reason -> 关闭所有 content block；message_delta/message_stop 推迟到
		// 流结束后统一发送，以便用最终 usage（或估算值）
		if c.FinishReason != "" {
			finishReason = "end_turn"
			if c.FinishReason == "tool_calls" {
				finishReason = "tool_use"
			} else if c.FinishReason == "length" {
				finishReason = "max_tokens"
			}
			// 刷新仍未补齐 id/name 的 tool_use block（兜底，避免悬挂）
			for tcIdx, p := range pendingToolStart {
				if p == nil {
					continue
				}
				if p.id == "" {
					p.id = fmt.Sprintf("toolu_%d", tcIdx)
				}
				if p.name == "" {
					p.name = "unknown"
				}
				writeEvent("content_block_start", map[string]interface{}{
					"type":  "content_block_start",
					"index": p.anthropicIdx,
					"content_block": map[string]interface{}{
						"type": "tool_use", "id": p.id, "name": p.name, "input": map[string]interface{}{},
					},
				})
				if p.bufferedArgs.Len() > 0 {
					writeEvent("content_block_delta", map[string]interface{}{
						"type":  "content_block_delta",
						"index": p.anthropicIdx,
						"delta": map[string]interface{}{"type": "input_json_delta", "partial_json": p.bufferedArgs.String()},
					})
				}
				pendingToolStart[tcIdx] = nil
			}

			// 按 block 开启顺序关闭（而非排序后的 tool 索引）
			for _, idx := range blockOpenOrder {
				writeEvent("content_block_stop", map[string]interface{}{
					"type": "content_block_stop", "index": idx,
				})
			}
			finishedNormal = true

			if chunk.Usage != nil {
				capturedUsage = chunk.Usage
			}
		}

		if chunk.Usage != nil {
			capturedUsage = chunk.Usage
		}
	}
		if err := scanner.Err(); err != nil {
			// 上游连接中断：如果已发了 message_start，必须补发 message_delta + message_stop，
			// 否则客户端拿到的 Message 缺 stop_reason → Anthropic SDK 校验失败 → "empty or malformed response"
			if messageStarted && !finishedNormal {
				for tcIdx, p := range pendingToolStart {
					if p == nil {
						continue
					}
					if p.id == "" {
						p.id = fmt.Sprintf("toolu_%d", tcIdx)
					}
					if p.name == "" {
						p.name = "unknown"
					}
					writeEvent("content_block_start", map[string]interface{}{
						"type": "content_block_start", "index": p.anthropicIdx,
						"content_block": map[string]interface{}{"type": "tool_use", "id": p.id, "name": p.name, "input": map[string]interface{}{}},
					})
					if p.bufferedArgs.Len() > 0 {
						writeEvent("content_block_delta", map[string]interface{}{
							"type": "content_block_delta", "index": p.anthropicIdx,
							"delta": map[string]interface{}{"type": "input_json_delta", "partial_json": p.bufferedArgs.String()},
						})
					}
				}
				for _, idx := range blockOpenOrder {
					writeEvent("content_block_stop", map[string]interface{}{
						"type": "content_block_stop", "index": idx,
					})
				}
				fr := finishReason
				if fr == "" {
					fr = "end_turn"
				}
				outputTokens := 0
				if capturedUsage != nil {
					outputTokens = capturedUsage.CompletionTokens
				}
				if outputTokens <= 0 && outputRunes > 0 {
					outputTokens = estimateOutputTokens(outputRunes)
				}
				usageOut := map[string]interface{}{"output_tokens": outputTokens}
				if capturedUsage != nil && capturedUsage.PromptTokens > 0 {
					usageOut["input_tokens"] = capturedUsage.PromptTokens
				}
				writeEvent("message_delta", map[string]interface{}{
					"type": "message_delta",
					"delta": map[string]interface{}{
						"stop_reason":   fr,
						"stop_sequence": nil,
					},
					"usage": usageOut,
				})
				writeEvent("message_stop", map[string]interface{}{
					"type": "message_stop",
				})
			}
			return capturedUsage, firstByteMs, model, fmt.Errorf("读取 SSE 流失败: %w", err)
		}
	// 没发 message_start 但有 data 行 -> 上游返回了非标准 SSE（如错误 data），记录前几行便于排查
	if firstByteMs == 0 && len(firstDataLines) > 0 {
		snippet := strings.Join(firstDataLines, " | ")
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		return capturedUsage, firstByteMs, model, fmt.Errorf("no message_start, firstChoices=%d, parseErr=%v, first data: %s", firstChoicesCount, firstParseErr, snippet)
	}

	// 流结束：补发终止序列。正常结束时 block 已在 finish_reason 分支关闭；
	// 流截断（无 finish_reason）时先关闭未收尾的 block，再统一发 message_delta + message_stop。
	// 这样客户端一定能等到 message_stop，且 usage 可用最终值/估算值。
	if messageStarted {
		if !finishedNormal {
			// 刷新未补齐 id/name 的 tool_use block
			for tcIdx, p := range pendingToolStart {
				if p == nil {
					continue
				}
				if p.id == "" {
					p.id = fmt.Sprintf("toolu_%d", tcIdx)
				}
				if p.name == "" {
					p.name = "unknown"
				}
				writeEvent("content_block_start", map[string]interface{}{
					"type": "content_block_start", "index": p.anthropicIdx,
					"content_block": map[string]interface{}{"type": "tool_use", "id": p.id, "name": p.name, "input": map[string]interface{}{}},
				})
				if p.bufferedArgs.Len() > 0 {
					writeEvent("content_block_delta", map[string]interface{}{
						"type": "content_block_delta", "index": p.anthropicIdx,
						"delta": map[string]interface{}{"type": "input_json_delta", "partial_json": p.bufferedArgs.String()},
					})
				}
			}
			// 按开启顺序关闭所有 content block
			for _, idx := range blockOpenOrder {
				writeEvent("content_block_stop", map[string]interface{}{
					"type": "content_block_stop", "index": idx,
				})
			}
			if finishReason == "" {
				finishReason = "end_turn"
			}
		}

		// 解析最终 usage：上游未返回 output token 时按输出字符数兜底估算
		outputTokens := 0
		if capturedUsage != nil {
			outputTokens = capturedUsage.CompletionTokens
		}
		estimated := false
		if outputTokens <= 0 && outputRunes > 0 {
			outputTokens = estimateOutputTokens(outputRunes)
			estimated = true
		}
		usageOut := map[string]interface{}{"output_tokens": outputTokens}
		if capturedUsage != nil && capturedUsage.PromptTokens > 0 {
			usageOut["input_tokens"] = capturedUsage.PromptTokens
		}
		if estimated {
			usageOut["estimated"] = true
		}
		writeEvent("message_delta", map[string]interface{}{
			"type": "message_delta",
			"delta": map[string]interface{}{
				"stop_reason":   finishReason,
				"stop_sequence": nil,
			},
			"usage": usageOut,
		})
		writeEvent("message_stop", map[string]interface{}{
			"type": "message_stop",
		})
	}

	// 兜底：usage 仍缺失时构造一个带估算值的 usage 供日志统计
	if (capturedUsage == nil || capturedUsage.CompletionTokens <= 0) && outputRunes > 0 {
		if capturedUsage == nil {
			capturedUsage = &OpenAIUsage{}
		}
		capturedUsage.CompletionTokens = estimateOutputTokens(outputRunes)
	}
	return capturedUsage, firstByteMs, model, nil
}

// StreamOpenAIPassthrough 透传上游 OpenAI SSE 给客户端（/v1/chat/completions 流式入口）
// 边透传边捕获 usage，返回 usage 和首字节用时（ms）
func StreamOpenAIPassthrough(w io.Writer, r io.Reader) (*OpenAIUsage, int64, error) {
	flusher, canFlush := w.(http.Flusher)
	start := time.Now()
	var firstByteMs int64
	var capturedUsage *OpenAIUsage
	var writeErr error
	sawDone := false

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	// 缓冲阶段：在找到第一个 data: 行之前，把输出暂存在 buf 中
	var buf bytes.Buffer
	buffering := true

	for scanner.Scan() {
		if writeErr != nil {
			break // 客户端已断开
		}
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		// 在写入之前先检查是否为 data: 行
		if firstByteMs == 0 && strings.HasPrefix(trimmed, "data:") {
			firstByteMs = time.Since(start).Milliseconds()
		}

		if buffering {
			fmt.Fprintln(&buf, line)
			// 确认有 data: 行后，刷新缓冲到 w 并切换到直写模式
			if firstByteMs > 0 {
				if _, err := w.Write(buf.Bytes()); err != nil && writeErr == nil {
					writeErr = err
				}
				buf.Reset()
				buffering = false
				if canFlush {
					flusher.Flush()
				}
			}
		} else {
			if _, err := fmt.Fprintln(w, line); err != nil && writeErr == nil {
				writeErr = err
			}
			if canFlush {
				flusher.Flush()
			}
		}

		// 捕获 usage（在最后一个 chunk）
		if strings.HasPrefix(trimmed, "data:") {
			data := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
			if data == "[DONE]" {
				sawDone = true
			} else {
				var chunk streamOpenAIChunk
				if json.Unmarshal([]byte(data), &chunk) == nil && chunk.Usage != nil {
					capturedUsage = chunk.Usage
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return capturedUsage, firstByteMs, fmt.Errorf("读取 SSE 流失败: %w", err)
	}
	// 上游流截断时补发 [DONE]，避免客户端连接异常关闭
	// 客户端已断开时不补发
	if firstByteMs > 0 && !sawDone && writeErr == nil {
		fmt.Fprintf(w, "data: [DONE]\n\n")
		if canFlush {
			flusher.Flush()
		}
	}
	return capturedUsage, firstByteMs, nil
}

// StreamAnthropicPassthrough 透传上游 Anthropic SSE 事件给 Anthropic 客户端
// （入站/出站同协议）。逐行原样写出，顺带从 message_start/message_delta 中
// 提取 input/output token 用量用于日志。返回 usage（output 取 message_stop 前累积值）。
//
// 关键：前几行（event: 消息）在找到第一个 data: 行之前缓冲在内存中；
// 若流结束前从未出现 data: 行，则 firstByteMs==0 且不向 w 写入任何内容，
// 调用方可据此返回 502 而非 200+空body。
func StreamAnthropicPassthrough(w io.Writer, r io.Reader) (*OpenAIUsage, int64, error) {
	flusher, canFlush := w.(http.Flusher)
	start := time.Now()
	var firstByteMs int64
	var writeErr error
	usage := &OpenAIUsage{}

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	sawStop := false

	// 缓冲阶段：在找到第一个 data: 行之前，把输出暂存在 buf 中
	var buf bytes.Buffer
	buffering := true

	for scanner.Scan() {
		if writeErr != nil {
			break // 客户端已断开
		}
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		// 在写入之前先检查是否为 data: 行
		if firstByteMs == 0 && strings.HasPrefix(trimmed, "data:") {
			firstByteMs = time.Since(start).Milliseconds()
		}

		if buffering {
			fmt.Fprintln(&buf, line)
			// 确认有 data: 行后，刷新缓冲到 w 并切换到直写模式
			if firstByteMs > 0 {
				if _, err := w.Write(buf.Bytes()); err != nil && writeErr == nil {
					writeErr = err
				}
				buf.Reset()
				buffering = false
				if canFlush {
					flusher.Flush()
				}
			}
		} else {
			if _, err := fmt.Fprintln(w, line); err != nil && writeErr == nil {
				writeErr = err
			}
			if canFlush {
				flusher.Flush()
			}
		}

		if !strings.HasPrefix(trimmed, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		// message_stop 是正常结束标记
		if data == "[DONE]" {
			sawStop = true
			continue
		}
		// Anthropic SSE 的 JSON 含 type 字段；message_start 带初始 usage，
		// message_delta 带 output_tokens 增量。
		var ev struct {
			Type  string `json:"type"`
			Index int    `json:"index"`
			Msg   struct {
				Usage struct {
					InputTokens  int `json:"input_tokens"`
					OutputTokens int `json:"output_tokens"`
				} `json:"usage"`
			} `json:"message"`
			Usage struct {
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(data), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "message_start":
			usage.PromptTokens = ev.Msg.Usage.InputTokens
			usage.CompletionTokens = ev.Msg.Usage.OutputTokens
		case "message_delta":
			usage.CompletionTokens += ev.Usage.OutputTokens
		case "message_stop":
			sawStop = true
		}
	}
	if err := scanner.Err(); err != nil {
		return usage, firstByteMs, fmt.Errorf("读取 SSE 流失败: %w", err)
	}
	if firstByteMs > 0 && !sawStop && writeErr == nil {
		// 此时 buffering 一定为 false（有 data: 行才可能 firstByteMs > 0）
		fmt.Fprintf(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		if canFlush {
			flusher.Flush()
		}
	}
	return usage, firstByteMs, nil
}

// EstimateOutputTokens 按输出字符数粗估 output token。
// 上游流式不返回 usage 时的兜底口径：约每 3 个字符 1 token（中英混合折中），至少 1。
// 供 proxy 内部流式转换 + service 层（benchmark）共用，保证 TPS 横向对比口径一致。
func EstimateOutputTokens(runeCount int) int {
	if runeCount <= 0 {
		return 0
	}
	est := runeCount / 3
	if est < 1 {
		est = 1
	}
	return est
}

// estimateOutputTokens 内部别名
func estimateOutputTokens(runeCount int) int {
	return EstimateOutputTokens(runeCount)
}
