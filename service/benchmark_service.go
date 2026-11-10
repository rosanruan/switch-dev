package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"switchdev/db"
	"switchdev/proxy"
)

// BenchmarkTarget 测评目标（上游 + 模型）
type BenchmarkTarget struct {
	Upstream string `json:"upstream"` // joycode/deveco/opencode/workbuddy
	Model    string `json:"model"`    // 代理内部 id
}

// BenchmarkResult 单个上游模型测评结果
type BenchmarkResult struct {
	Upstream      string  `json:"upstream"`
	UpstreamLabel string  `json:"upstreamLabel"`
	Model         string  `json:"model"`
	Success       bool    `json:"success"`
	DurationMs    int64   `json:"durationMs"`        // 总耗时（端到端）
	OutputTokens  int     `json:"outputTokens"`      // 输出 token
	TPS           float64 `json:"tps"`               // output_tokens / (duration/1000)
	Content       string  `json:"content,omitempty"` // 返回文本（供比对）
	ErrorMsg      string  `json:"errorMsg,omitempty"`
	StartedAt     int64   `json:"startedAt"` // 开始时间戳（ms）
}

// BenchmarkService 模型测评服务（暴露给前端）
type BenchmarkService struct {
	core *Core

	mu          sync.Mutex
	cancel      context.CancelFunc            // 当前批量测评的取消函数（停止整批时调用）
	itemCancels map[string]context.CancelFunc // 单个测评项的取消函数，key = upstream|model
	running     bool
}

func NewBenchmarkService(core *Core) *BenchmarkService {
	return &BenchmarkService{core: core, itemCancels: map[string]context.CancelFunc{}}
}

func benchmarkItemKey(upstream, model string) string {
	return upstream + "|" + model
}

func benchmarkLabel(up string) string {
	switch up {
	case "joycode":
		return "京东 JoyCode"
	case "deveco":
		return "华为 DevEco"
	case "opencode":
		return "OpenCode Zen"
	case "workbuddy":
		return "腾讯 WorkBuddy"
	case "mcloud":
		return "移动云盘（灵犀）"
	default:
		return up
	}
}

// RunBenchmark 并发测评多个 target，每完成一个 emit "benchmark:progress"，返回全部结果。
// 可被 StopBenchmark 中断：取消后在途 HTTP 请求立即终止，未完成项标记为已停止。
// apiMode: "anthropic" | "openai-chat" | "openai-responses"
func (s *BenchmarkService) RunBenchmark(targets []BenchmarkTarget, prompt string, maxTokens int, apiMode string) []BenchmarkResult {
	if prompt == "" {
		prompt = "请详细介绍 Go 语言的 goroutine 和 channel 并发模型，包括基本概念、使用示例和注意事项。"
	}
	if maxTokens <= 0 {
		maxTokens = 1024
	}
	if apiMode == "" {
		apiMode = "anthropic"
	}

	port := 8787
	if srv := s.core.Server(); srv != nil && srv.Port > 0 {
		port = srv.Port
	}

	var url string
	switch apiMode {
	case "openai-chat":
		url = fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", port)
	case "openai-responses":
		url = fmt.Sprintf("http://127.0.0.1:%d/v1/responses", port)
	default:
		url = fmt.Sprintf("http://127.0.0.1:%d/v1/messages", port)
	}

	// 建立本次批量测评的可取消 context
	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.cancel = cancel
	s.running = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.cancel = nil
		s.running = false
		s.mu.Unlock()
		cancel()
	}()

	results := make([]BenchmarkResult, len(targets))
	var wg sync.WaitGroup

	for i, t := range targets {
		wg.Add(1)
		go func(idx int, target BenchmarkTarget) {
			defer wg.Done()
			// 单项可取消：ctx 同时受批量取消和单项取消控制
			itemCtx, itemCancel := context.WithCancel(ctx)
			ik := benchmarkItemKey(target.Upstream, target.Model)
			s.mu.Lock()
			s.itemCancels[ik] = itemCancel
			s.mu.Unlock()
			defer func() {
				itemCancel()
				s.mu.Lock()
				delete(s.itemCancels, ik)
				s.mu.Unlock()
			}()
			res := s.benchOne(itemCtx, url, target, prompt, maxTokens, apiMode)
			results[idx] = res
			s.core.EmitEvent("benchmark:progress", res)
			// 写 models 表测评结果
			if s.core.DB() != nil {
				_ = s.core.DB().UpsertModelBench(target.Upstream, target.Model, dbBenchResult(res))
			}
		}(i, t)
	}
	wg.Wait()
	return results
}

// StopBenchmark 停止当前批量测评（取消所有在途请求）。无测评运行时是空操作。
func (s *BenchmarkService) StopBenchmark() {
	s.mu.Lock()
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.core.EmitEvent("benchmark:stopped", true)
}

// StopBenchmarkItem 停止单个测评项（按 upstream+model 定位在途请求）。
// 可用于批量中的某一项，也可用于单独测评。找不到对应在途项时是空操作。
func (s *BenchmarkService) StopBenchmarkItem(upstream, model string) {
	ik := benchmarkItemKey(upstream, model)
	s.mu.Lock()
	cancel := s.itemCancels[ik]
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// benchOne 测单个 target：走本代理流式，实时推送 content chunk 给前端。
// ctx 被取消时（停止批量测评）立即中断 HTTP 并把结果标记为已停止。
// apiMode: "anthropic" | "openai-chat" | "openai-responses"
func (s *BenchmarkService) benchOne(ctx context.Context, url string, target BenchmarkTarget, prompt string, maxTokens int, apiMode string) BenchmarkResult {
	res := BenchmarkResult{
		Upstream:      target.Upstream,
		UpstreamLabel: benchmarkLabel(target.Upstream),
		Model:         target.Model,
		StartedAt:     time.Now().UnixMilli(),
	}

	var reqBody map[string]interface{}
	switch apiMode {
	case "openai-chat":
		reqBody = map[string]interface{}{
			"model": target.Model,
			"messages": []map[string]string{
				{"role": "user", "content": prompt},
			},
			"max_tokens": maxTokens,
			"stream":     true,
		}
	case "openai-responses":
		reqBody = map[string]interface{}{
			"model":  target.Model,
			"input":  prompt,
			"stream": true,
		}
	default: // anthropic
		reqBody = map[string]interface{}{
			"model": target.Model,
			"messages": []map[string]string{
				{"role": "user", "content": prompt},
			},
			"max_tokens": maxTokens,
			"stream":     true,
		}
	}
	bodyBytes, _ := json.Marshal(reqBody)

	client := &http.Client{} // 流式：不设整体超时，用 context 控制
	// 单次请求最多 180s，但会随批量 ctx 一起被取消
	reqCtx, cancel := context.WithTimeout(ctx, 180*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(reqCtx, "POST", url, bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	// 直连模式：只测评指定模型，不触发降级链/全局兜底
	req.Header.Set("X-Switch-Direct", "1")
	// 带上配置的 apiKey（代理严格鉴权，不带会 401）
	if srv := s.core.Server(); srv != nil && srv.ConfigResolver != nil {
		if key := srv.ConfigResolver.GetAPIKey(); key != "" {
			req.Header.Set("x-api-key", key)
		}
	}

	start := time.Now()
	httpResp, err := client.Do(req)
	if err != nil {
		res.DurationMs = time.Since(start).Milliseconds()
		if ctx.Err() == context.Canceled {
			res.ErrorMsg = "已停止"
		} else {
			res.ErrorMsg = fmt.Sprintf("请求失败: %v", err)
		}
		return res
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode != 200 {
		errBody, _ := io.ReadAll(httpResp.Body)
		res.DurationMs = time.Since(start).Milliseconds()
		snippet := string(errBody)
		if len(snippet) > 100 {
			snippet = snippet[:100]
		}
		res.ErrorMsg = fmt.Sprintf("HTTP %d: %s", httpResp.StatusCode, snippet)
		return res
	}

	// 读 SSE 流，按 apiMode 解析不同事件格式，累积 content 并实时推送 chunk 事件
	var contentBuilder strings.Builder
	var outputTokens int
	scanner := bufio.NewScanner(httpResp.Body)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var ev map[string]interface{}
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			continue
		}

		switch apiMode {
		case "openai-chat":
			// OpenAI Chat Completions SSE: choices[0].delta.content
			if choices, ok := ev["choices"].([]interface{}); ok && len(choices) > 0 {
				if choice, ok := choices[0].(map[string]interface{}); ok {
					if delta, ok := choice["delta"].(map[string]interface{}); ok {
						if content, ok := delta["content"].(string); ok && content != "" {
							contentBuilder.WriteString(content)
							s.core.EmitEvent("benchmark:chunk", map[string]interface{}{
								"upstream": target.Upstream,
								"model":    target.Model,
								"delta":    content,
							})
						}
					}
					// usage 在最后一个 chunk 的 choices[0] 中（部分上游）
					if usage, ok := choice["usage"].(map[string]interface{}); ok {
						if v, ok := usage["completion_tokens"].(float64); ok && int(v) > 0 {
							outputTokens = int(v)
						}
					}
				}
			}
			// 顶层 usage（stream_options 包含 usage 时）
			if usage, ok := ev["usage"].(map[string]interface{}); ok {
				if v, ok := usage["completion_tokens"].(float64); ok && int(v) > 0 {
					outputTokens = int(v)
				}
			}

		case "openai-responses":
			// OpenAI Responses SSE: type == "response.output_text.delta", delta = text
			evType, _ := ev["type"].(string)
			switch evType {
			case "response.output_text.delta":
				if delta, ok := ev["delta"].(string); ok && delta != "" {
					contentBuilder.WriteString(delta)
					s.core.EmitEvent("benchmark:chunk", map[string]interface{}{
						"upstream": target.Upstream,
						"model":    target.Model,
						"delta":    delta,
					})
				}
			case "response.completed":
				if resp, ok := ev["response"].(map[string]interface{}); ok {
					if usage, ok := resp["usage"].(map[string]interface{}); ok {
						if v, ok := usage["output_tokens"].(float64); ok && int(v) > 0 {
							outputTokens = int(v)
						}
					}
				}
			}

		default: // anthropic
			// Anthropic SSE: type == "content_block_delta", delta.type == "text_delta", delta.text
			evType, _ := ev["type"].(string)
			switch evType {
			case "content_block_delta":
				if delta, ok := ev["delta"].(map[string]interface{}); ok {
					if dType, _ := delta["type"].(string); dType == "text_delta" {
						if text, ok := delta["text"].(string); ok && text != "" {
							contentBuilder.WriteString(text)
							s.core.EmitEvent("benchmark:chunk", map[string]interface{}{
								"upstream": target.Upstream,
								"model":    target.Model,
								"delta":    text,
							})
						}
					}
				}
			case "message_delta":
				if usage, ok := ev["usage"].(map[string]interface{}); ok {
					if v, ok := usage["output_tokens"].(float64); ok && int(v) > 0 {
						outputTokens = int(v)
					}
				}
			}
		}
	}

	res.DurationMs = time.Since(start).Milliseconds()
	// 流式读取途中被停止：不当作成功
	if ctx.Err() == context.Canceled {
		res.ErrorMsg = "已停止"
		return res
	}
	res.Success = true
	res.OutputTokens = outputTokens
	// 上游流式不返回 usage（如 JoyCode 真流式）时，按输出字符数兜底估算，
	// 保证 TPS 能算出且四模型横向对比口径一致
	if outputTokens <= 0 && contentBuilder.Len() > 0 {
		res.OutputTokens = proxy.EstimateOutputTokens(len([]rune(contentBuilder.String())))
	}
	if res.DurationMs > 0 && res.OutputTokens > 0 {
		res.TPS = float64(res.OutputTokens) / (float64(res.DurationMs) / 1000.0)
	}
	res.Content = contentBuilder.String()
	return res
}

// GetBenchResults 从 DB 读回历史测评结果（前端重进页面时恢复展示）。
// outputTokens 由 TPS×duration 反推；Content 不持久化，留空。
func (s *BenchmarkService) GetBenchResults() []BenchmarkResult {
	if s.core.DB() == nil {
		return nil
	}
	recs, err := s.core.DB().QueryBenchResults()
	if err != nil {
		return nil
	}
	out := make([]BenchmarkResult, 0, len(recs))
	for _, r := range recs {
		res := BenchmarkResult{
			Upstream:      r.Upstream,
			UpstreamLabel: benchmarkLabel(r.Upstream),
			Model:         r.Model,
			Success:       r.Success,
			DurationMs:    r.Duration,
			TPS:           r.TPS,
			ErrorMsg:      r.Error,
		}
		if r.Duration > 0 {
			res.OutputTokens = int(r.TPS * float64(r.Duration) / 1000.0)
		}
		out = append(out, res)
	}
	return out
}

// dbBenchResult 将 BenchmarkResult 转换为 db.BenchResult
func dbBenchResult(res BenchmarkResult) db.BenchResult {
	return db.BenchResult{
		TPS:      res.TPS,
		Duration: res.DurationMs,
		Error:    res.ErrorMsg,
		Success:  res.Success,
	}
}
