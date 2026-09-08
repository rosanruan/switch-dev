package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

// aggregateFrom 便捷封装
func aggregateFrom(t *testing.T, sse string) map[string]interface{} {
	t.Helper()
	out, err := aggregateOpenAISSE(strings.NewReader(sse))
	if err != nil {
		t.Fatalf("aggregateOpenAISSE: %v", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("聚合结果不是合法 JSON: %v\n%s", err, out)
	}
	return m
}

func firstMessage(t *testing.T, m map[string]interface{}) map[string]interface{} {
	t.Helper()
	choices, ok := m["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		t.Fatalf("聚合结果无 choices: %+v", m)
	}
	msg, ok := choices[0].(map[string]interface{})["message"].(map[string]interface{})
	if !ok {
		t.Fatalf("choices[0] 无 message: %+v", choices[0])
	}
	return msg
}

// TestAggregateKeepsReasoningContent 推理模型的 delta.reasoning_content 必须被保留。
// 此前聚合器的 chunk 结构体没有这个字段，WorkBuddy 等推理模型在非流式路径下
// 思维链是静默丢失的（流式转换器 sse_stream.go 一直是支持的）。
func TestAggregateKeepsReasoningContent(t *testing.T) {
	sse := `data: {"id":"1","model":"glm-5.0","choices":[{"index":0,"delta":{"role":"assistant"}}]}

data: {"id":"1","choices":[{"index":0,"delta":{"reasoning_content":"先想一下："}}]}

data: {"id":"1","choices":[{"index":0,"delta":{"reasoning_content":"用户要打招呼"}}]}

data: {"id":"1","choices":[{"index":0,"delta":{"content":"你好"}}]}

data: {"id":"1","choices":[{"index":0,"delta":{"content":"！"},"finish_reason":"stop"}]}

data: [DONE]
`
	msg := firstMessage(t, aggregateFrom(t, sse))

	rc, _ := msg["reasoning_content"].(string)
	if rc != "先想一下：用户要打招呼" {
		t.Errorf("reasoning_content = %q, want \"先想一下：用户要打招呼\"", rc)
	}
	// 推理内容不能混进正文
	if c, _ := msg["content"].(string); c != "你好！" {
		t.Errorf("content = %q, want \"你好！\"", c)
	}
}

// TestAggregateOmitsEmptyReasoning 无推理内容时不应输出空的 reasoning_content 字段
// （omitempty），否则下游 OpenAIToAnthropic 会多出一个空 text block。
func TestAggregateOmitsEmptyReasoning(t *testing.T) {
	sse := `data: {"id":"1","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}

data: [DONE]
`
	msg := firstMessage(t, aggregateFrom(t, sse))
	if _, present := msg["reasoning_content"]; present {
		t.Errorf("无推理内容时不该出现 reasoning_content 字段：%+v", msg)
	}
}

// TestAggregateContentToolCallsUsage 原有聚合能力不回归
func TestAggregateContentToolCallsUsage(t *testing.T) {
	sse := `data: {"id":"abc","model":"glm-5.0","created":1700000001,"choices":[{"index":0,"delta":{"role":"assistant"}}]}

data: {"id":"abc","choices":[{"index":0,"delta":{"content":"Hello"}}]}

data: {"id":"abc","choices":[{"index":0,"delta":{"content":" world"}}]}

data: {"id":"abc","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"ci"}}]}}]}

data: {"id":"abc","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ty\":\"BJ\"}"}}]},"finish_reason":"tool_calls"}]}

data: {"id":"abc","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":22,"total_tokens":33}}

data: [DONE]
`
	m := aggregateFrom(t, sse)

	if m["id"] != "abc" || m["model"] != "glm-5.0" {
		t.Errorf("id/model 丢失：%+v", m)
	}
	if m["object"] != "chat.completion" {
		t.Errorf("object = %v, want chat.completion", m["object"])
	}

	msg := firstMessage(t, m)
	if c, _ := msg["content"].(string); c != "Hello world" {
		t.Errorf("content = %q, want \"Hello world\"", c)
	}
	if r, _ := msg["role"].(string); r != "assistant" {
		t.Errorf("role = %q, want assistant", r)
	}

	tcs, ok := msg["tool_calls"].([]interface{})
	if !ok || len(tcs) != 1 {
		t.Fatalf("tool_calls 应有 1 个：%+v", msg["tool_calls"])
	}
	tc := tcs[0].(map[string]interface{})
	if tc["id"] != "call_1" || tc["type"] != "function" {
		t.Errorf("tool_call 元信息不对：%+v", tc)
	}
	fn := tc["function"].(map[string]interface{})
	if fn["name"] != "get_weather" {
		t.Errorf("function.name = %v", fn["name"])
	}
	if args, _ := fn["arguments"].(string); args != `{"city":"BJ"}` {
		t.Errorf("arguments 拼接错误: %q", args)
	}

	choice := m["choices"].([]interface{})[0].(map[string]interface{})
	if choice["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason = %v, want tool_calls", choice["finish_reason"])
	}

	usage, ok := m["usage"].(map[string]interface{})
	if !ok {
		t.Fatalf("usage 丢失：%+v", m)
	}
	if usage["total_tokens"].(float64) != 33 {
		t.Errorf("usage = %+v", usage)
	}
}

// TestAggregateSkipsUnparsableLines 非 data: 行、无法解析的 JSON 应被跳过而非报错
func TestAggregateSkipsUnparsableLines(t *testing.T) {
	sse := `event: message

: this is a comment

data: {broken json

data: {"choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}

data: [DONE]
`
	msg := firstMessage(t, aggregateFrom(t, sse))
	if c, _ := msg["content"].(string); c != "ok" {
		t.Errorf("content = %q, want ok", c)
	}
}
