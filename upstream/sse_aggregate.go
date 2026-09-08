package upstream

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// 聚合用结构（json tag 与 proxy.OpenAIResponse 一致，供上层 enrichUsage/OpenAIToAnthropic 解析）
type aggregatedChoice struct {
	Index        int               `json:"index"`
	Message      aggregatedMessage `json:"message"`
	FinishReason string            `json:"finish_reason"`
}

type aggregatedMessage struct {
	Role string `json:"role"`
	// ReasoningContent 推理模型的思维链。上游放在 delta.reasoning_content 里，
	// 与 Content 分开累加；json tag 与 proxy.OpenAIMessage 一致，
	// 上层 OpenAIToAnthropic 会把它渲染成独立 text block。
	ReasoningContent string               `json:"reasoning_content,omitempty"`
	Content          string               `json:"content"`
	ToolCalls        []aggregatedToolCall `json:"tool_calls,omitempty"`
}

type aggregatedToolCall struct {
	ID       string             `json:"id"`
	Type     string             `json:"type"`
	Function aggregatedFunction `json:"function"`
}

type aggregatedFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type aggregatedUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type aggregatedResponse struct {
	ID      string             `json:"id"`
	Object  string             `json:"object"`
	Created int64              `json:"created"`
	Model   string             `json:"model"`
	Choices []aggregatedChoice `json:"choices"`
	Usage   *aggregatedUsage   `json:"usage,omitempty"`
}

// aggregateOpenAISSE 把上游 OpenAI 流式 SSE chunk 聚合成一条完整 OpenAIResponse JSON
// 用于上游强制 stream:true 的场景（如 WorkBuddy），让代理上层无感复用非流式处理逻辑
func aggregateOpenAISSE(r io.Reader) ([]byte, error) {
	resp := aggregatedResponse{
		Object:  "chat.completion",
		Choices: []aggregatedChoice{{Index: 0, Message: aggregatedMessage{Role: "assistant"}}},
	}
	choice := &resp.Choices[0]
	toolCallsByIndex := map[int]*aggregatedToolCall{}

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024) // 允许大行
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}

		var chunk struct {
			ID      string `json:"id"`
			Model   string `json:"model"`
			Created int64  `json:"created"`
			Choices []struct {
				Index int `json:"index"`
				Delta struct {
					Role             string `json:"role"`
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					ToolCalls        []struct {
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
			Usage *aggregatedUsage `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue // 跳过无法解析的行
		}

		if resp.ID == "" {
			resp.ID = chunk.ID
		}
		if resp.Model == "" {
			resp.Model = chunk.Model
		}
		if resp.Created == 0 {
			resp.Created = chunk.Created
		}

		if len(chunk.Choices) > 0 {
			c := chunk.Choices[0]
			choice.Message.Content += c.Delta.Content
			choice.Message.ReasoningContent += c.Delta.ReasoningContent
			if c.Delta.Role != "" {
				choice.Message.Role = c.Delta.Role
			}
			for _, tc := range c.Delta.ToolCalls {
				existing := toolCallsByIndex[tc.Index]
				if existing == nil {
					existing = &aggregatedToolCall{Type: "function"}
					toolCallsByIndex[tc.Index] = existing
				}
				if tc.ID != "" {
					existing.ID = tc.ID
				}
				if tc.Type != "" {
					existing.Type = tc.Type
				}
				if tc.Function.Name != "" {
					existing.Function.Name = tc.Function.Name
				}
				existing.Function.Arguments += tc.Function.Arguments
			}
			if c.FinishReason != "" {
				choice.FinishReason = c.FinishReason
			}
		}
		if chunk.Usage != nil {
			resp.Usage = chunk.Usage
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("读取 SSE 流失败: %w", err)
	}

	// tool_calls 按 index 排序挂到 message
	if len(toolCallsByIndex) > 0 {
		indices := make([]int, 0, len(toolCallsByIndex))
		for idx := range toolCallsByIndex {
			indices = append(indices, idx)
		}
		sort.Ints(indices)
		for _, idx := range indices {
			choice.Message.ToolCalls = append(choice.Message.ToolCalls, *toolCallsByIndex[idx])
		}
	}

	return json.Marshal(resp)
}
