package upstream

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// parseAll 把若干 chunk 喂给解析器再 flush，返回普通文本累积和解析出的工具调用。
func parseAll(chunks ...string) (string, []mcloudToolCall) {
	return parseAllWithTools(nil, chunks...)
}

// parseAllWithTools 同 parseAll，但先登记请求 tools 清单（测试名字纠错用）。
func parseAllWithTools(tools []string, chunks ...string) (string, []mcloudToolCall) {
	var p mcloudToolCallParser
	p.setToolNames(tools)
	var content strings.Builder
	for _, c := range chunks {
		for _, d := range p.feed(c) {
			if d.Content != "" {
				content.WriteString(d.Content)
			}
		}
	}
	for _, d := range p.flush() {
		if d.Content != "" {
			content.WriteString(d.Content)
		}
	}
	return content.String(), p.toolCalls
}

func TestParseToolCallQwenFormat(t *testing.T) {
	content, tcs := parseAll(`[调用工具 Bash({"command":"ls -lh"})]`)
	if content != "" {
		t.Fatalf("期望无普通文本，got %q", content)
	}
	if len(tcs) != 1 {
		t.Fatalf("期望 1 个工具调用，got %d", len(tcs))
	}
	if tcs[0].name != "Bash" || tcs[0].args != `{"command":"ls -lh"}` {
		t.Fatalf("工具调用解析错误: name=%q args=%q", tcs[0].name, tcs[0].args)
	}
	if tcs[0].id == "" {
		t.Fatal("期望生成工具调用 id")
	}
}

func TestParseToolCallDeepseekFormat(t *testing.T) {
	content, tcs := parseAll(`[调用工具 Bash] {"command":"find /c"}`)
	if content != "" {
		t.Fatalf("期望无普通文本，got %q", content)
	}
	if len(tcs) != 1 || tcs[0].name != "Bash" || tcs[0].args != `{"command":"find /c"}` {
		t.Fatalf("工具调用解析错误: %+v", tcs)
	}
}

func TestParseToolCallNoArgs(t *testing.T) {
	content, tcs := parseAll(`[调用工具 ListAgents]`)
	if content != "" {
		t.Fatalf("期望无普通文本，got %q", content)
	}
	if len(tcs) != 1 || tcs[0].name != "ListAgents" || tcs[0].args != "" {
		t.Fatalf("无参数工具调用解析错误: %+v", tcs)
	}
}

func TestParseToolCallWithLeadingText(t *testing.T) {
	content, tcs := parseAll(`我先看一下目录。`, `\n`, `[调用工具 Bash({"command":"ls"})]`)
	if content != `我先看一下目录。\n` {
		t.Fatalf("普通文本错误: %q", content)
	}
	if len(tcs) != 1 || tcs[0].name != "Bash" {
		t.Fatalf("工具调用解析错误: %+v", tcs)
	}
}

func TestParseToolCallAcrossChunks(t *testing.T) {
	content, tcs := parseAll(`[调用工`, `具 Bash`, `({"com`, `mand":"ls"})]`)
	if content != "" {
		t.Fatalf("期望无普通文本，got %q", content)
	}
	if len(tcs) != 1 || tcs[0].name != "Bash" || tcs[0].args != `{"command":"ls"}` {
		t.Fatalf("跨 chunk 工具调用解析错误: %+v", tcs)
	}
}

func TestParsePlainTextWithBracket(t *testing.T) {
	content, tcs := parseAll(`这是一个[例子]，不是工具调用`)
	if content != `这是一个[例子]，不是工具调用` {
		t.Fatalf("普通文本被破坏: %q", content)
	}
	if len(tcs) != 0 {
		t.Fatalf("期望 0 个工具调用，got %d", len(tcs))
	}
}

func TestParseMultipleToolCalls(t *testing.T) {
	content, tcs := parseAll(`[调用工具 Read({"file_path":"a.go"})][调用工具 Bash({"command":"go test"})]`)
	if content != "" {
		t.Fatalf("期望无普通文本，got %q", content)
	}
	if len(tcs) != 2 {
		t.Fatalf("期望 2 个工具调用，got %d", len(tcs))
	}
	if tcs[0].name != "Read" || tcs[1].name != "Bash" {
		t.Fatalf("多个工具调用解析错误: %+v", tcs)
	}
	if tcs[0].index != 0 || tcs[1].index != 1 {
		t.Fatalf("工具调用 index 未递增: %d %d", tcs[0].index, tcs[1].index)
	}
}

func TestParseToolCallNestedParenInJSON(t *testing.T) {
	content, tcs := parseAll(`[调用工具 Bash({"command":"echo ) hello"})]`)
	if content != "" {
		t.Fatalf("期望无普通文本，got %q", content)
	}
	if len(tcs) != 1 || tcs[0].args != `{"command":"echo ) hello"}` {
		t.Fatalf("嵌套括号 JSON 解析错误: %+v", tcs)
	}
}

func TestParseRejectsNonToolBracket(t *testing.T) {
	// 以 [ 开头但紧跟的不是「调用工具」，应整体按普通文本透传
	content, tcs := parseAll(`[abc] [调用工具 Bash({"x":1})]`)
	if content != `[abc] ` {
		t.Fatalf("非工具括号文本错误: %q", content)
	}
	if len(tcs) != 1 || tcs[0].name != "Bash" {
		t.Fatalf("后续工具调用应仍被解析: %+v", tcs)
	}
}

func TestToolsPrompt(t *testing.T) {
	tools := []mcloudOpenAITool{{
		Type: "function",
	}}
	tools[0].Function.Name = "Bash"
	tools[0].Function.Description = "执行 shell 命令"
	tools[0].Function.Parameters = []byte(`{"properties":{"command":{"type":"string"}},"required":["command"]}`)

	prompt := mcloudToolsPrompt(tools)
	if !strings.Contains(prompt, "Bash") || !strings.Contains(prompt, "command") {
		t.Fatalf("工具清单缺失 name/required 参数: %q", prompt)
	}
	if !strings.Contains(prompt, "[调用工具") {
		t.Fatalf("工具清单缺失输出格式指令: %q", prompt)
	}

	if mcloudToolsPrompt(nil) != "" {
		t.Fatal("空 tools 应返回空串")
	}
}

// TestMCloudCallStreamToolCall 端到端：上游模型用文本输出工具调用，
// CallStream 应把它转成 tool_calls delta，且 finish_reason=tool_calls。
func TestMCloudCallStreamToolCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/assistant/chat/config"):
			io.WriteString(w, `{"success":true,"code":"0000"}`)
		case strings.HasSuffix(r.URL.Path, "/assistant/chat/v2/add"):
			w.Header().Set("Content-Type", "text/event-stream")
			fl := w.(http.Flusher)
			// 普通文本在前，工具调用按片段拆帧（模拟逐 chunk 输出）
			io.WriteString(w, mcDataFrame("blian_qwen38_max", "我来执行。", "", false))
			fl.Flush()
			io.WriteString(w, mcDataFrame("blian_qwen38_max", `[调用工具 Bash({"co`, "", false))
			io.WriteString(w, mcDataFrame("blian_qwen38_max", `mmand":"ls -lh"})]`, "", true))
			fl.Flush()
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	up := newTestMCloud(t, srv.URL)
	sr, err := up.CallStream(context.Background(), mcChatBody("blian_qwen38_max", "user:hi"))
	if err != nil {
		t.Fatalf("CallStream: %v", err)
	}
	defer sr.Body.Close()

	all, err := io.ReadAll(sr.Body)
	if err != nil {
		t.Fatalf("读流: %v", err)
	}
	s := string(all)
	// 普通文本仍在 content
	if !strings.Contains(s, `"content":"我来执行。"`) {
		t.Errorf("普通文本丢失:\n%s", s)
	}
	// 工具调用转成了 tool_calls delta
	for _, want := range []string{
		`"tool_calls"`,
		`"name":"Bash"`,
		`"arguments":"{\"command\":\"ls -lh\"}"`,
		`"finish_reason":"tool_calls"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("流输出缺 %q:\n%s", want, s)
		}
	}
	// 不应把工具调用文本当普通 content 泄漏
	if strings.Contains(s, "调用工具") {
		t.Errorf("工具调用文本泄漏到 content:\n%s", s)
	}
}

// TestMCloudCallAggregatesToolCall 端到端：非流式 Call 聚合文本工具调用为 tool_calls。
func TestMCloudCallAggregatesToolCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/assistant/chat/config"):
			io.WriteString(w, `{"success":true,"code":"0000"}`)
		case strings.HasSuffix(r.URL.Path, "/assistant/chat/v2/add"):
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, mcDataFrame("blian_qwen38_max", "", "", false))
			io.WriteString(w, mcDataFrame("blian_qwen38_max", `[调用工具 Bash] {"command":"find ."}`, "", true))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	up := newTestMCloud(t, srv.URL)
	resp, err := up.Call(context.Background(), mcChatBody("blian_qwen38_max", "user:hi"))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	var got struct {
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(resp.Body, &got); err != nil {
		t.Fatalf("解析聚合响应: %v\n%s", err, resp.Body)
	}
	if len(got.Choices) != 1 {
		t.Fatalf("choices = %d", len(got.Choices))
	}
	msg := got.Choices[0].Message
	if msg.Content != "" {
		t.Errorf("聚合 content 应不含工具调用文本，got %q", msg.Content)
	}
	if len(msg.ToolCalls) != 1 {
		t.Fatalf("聚合 tool_calls = %+v", msg.ToolCalls)
	}
	tc := msg.ToolCalls[0]
	if tc.Function.Name != "Bash" || tc.Function.Arguments != `{"command":"find ."}` {
		t.Errorf("聚合工具调用错误: %+v", tc)
	}
	if tc.ID == "" || tc.Type != "function" {
		t.Errorf("工具调用缺 id/type: %+v", tc)
	}
	if got.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls", got.Choices[0].FinishReason)
	}
}

// TestMCloudToolsPromptInjected 折叠时 tools 清单注入 system。
func TestMCloudToolsPromptInjected(t *testing.T) {
	tools := []mcloudOpenAITool{{
		Type: "function",
	}}
	tools[0].Function.Name = "Bash"
	tools[0].Function.Description = "执行 shell 命令"
	tools[0].Function.Parameters = []byte(`{"properties":{"command":{"type":"string"}},"required":["command"]}`)

	body, _ := json.Marshal(map[string]interface{}{
		"model":    "blian_qwen38_max",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
		"tools":    tools,
	})
	dialogue, modelType, err := foldMCloudDialogue(body)
	if err != nil {
		t.Fatalf("foldMCloudDialogue: %v", err)
	}
	if modelType != "blian_qwen38_max" {
		t.Errorf("modelType = %q", modelType)
	}
	if !strings.Contains(dialogue, "[调用工具") || !strings.Contains(dialogue, "Bash(command)") {
		t.Errorf("工具清单未注入 dialogue:\n%s", dialogue)
	}
}

// ====== 容错提取（salvage）：模型笔误不再整段泄漏成正文 ======

// TestParseSalvageRealLeakSample 真实泄漏样本逐字符入流式解析器：
// 模型漏写右圆括号 )、] 前多写 }（灵犀实测笔误），严格语法永远判「半截」，
// 流结束时整段泄漏成正文 → 客户端不执行工具、agent loop 直接死。
// 修复后必须产出 tool_use：名字纠成 Bash、参数 JSON 可解析、前置正文保留。
func TestParseSalvageRealLeakSample(t *testing.T) {
	sample := "好的，我先查看一下目录内容。\n[调用工具 bash({\"command\":\"ls -la /Users/rosan/Downloads 2>&1\"}}]"
	content, tcs := parseAllWithTools([]string{"Bash", "Read"}, sample)
	if content != "好的，我先查看一下目录内容。\n" {
		t.Fatalf("前置正文被破坏: %q", content)
	}
	if len(tcs) != 1 {
		t.Fatalf("期望 1 个工具调用（而非泄漏成正文），got %d: content=%q", len(tcs), content)
	}
	if tcs[0].name != "Bash" {
		t.Fatalf("模型笔误的 bash 应纠回 Bash, got %q", tcs[0].name)
	}
	if tcs[0].args != `{"command":"ls -la /Users/rosan/Downloads 2>&1"}` || !json.Valid([]byte(tcs[0].args)) {
		t.Fatalf("容错提取的参数应是指第一个括号平衡的 JSON: %q", tcs[0].args)
	}
}

func TestParseSalvageMissingCloseParen(t *testing.T) {
	// 缺 )：...}}]  —— 真实样本的笔误模式
	content, tcs := parseAll(`[调用工具 Bash({"command":"ls"}}]`)
	if content != "" {
		t.Fatalf("畸形调用泄漏成正文: %q", content)
	}
	if len(tcs) != 1 || tcs[0].name != "Bash" || tcs[0].args != `{"command":"ls"}` {
		t.Fatalf("缺 ) 容错解析错误: content=%q calls=%+v", content, tcs)
	}
}

func TestParseSalvageMissingBracket(t *testing.T) {
	// 缺 ]
	content, tcs := parseAll(`[调用工具 Bash({"command":"ls"})`)
	if content != "" {
		t.Fatalf("畸形调用泄漏成正文: %q", content)
	}
	if len(tcs) != 1 || tcs[0].args != `{"command":"ls"}` {
		t.Fatalf("缺 ] 容错解析错误: content=%q calls=%+v", content, tcs)
	}
}

func TestParseSalvageMissingParenAndBracket(t *testing.T) {
	// ) ] 连缺
	content, tcs := parseAll(`[调用工具 Bash({"command":"ls"}`)
	if content != "" {
		t.Fatalf("畸形调用泄漏成正文: %q", content)
	}
	if len(tcs) != 1 || tcs[0].args != `{"command":"ls"}` {
		t.Fatalf("连缺容错解析错误: content=%q calls=%+v", content, tcs)
	}
}

func TestParseSalvageMalformedThenNormal(t *testing.T) {
	// 一行畸形 + 一行正常连发：单趟解析会把第二个当尾巴漏掉，flush 必须循环到清空
	content, tcs := parseAll(`[调用工具 bash({"x":1}}][调用工具 Read({"file_path":"a"})]`)
	if content != "" {
		t.Fatalf("畸形+正常连发泄漏成正文: %q", content)
	}
	if len(tcs) != 2 {
		t.Fatalf("期望 2 个工具调用, got %d: content=%q", len(tcs), content)
	}
	if tcs[0].name != "bash" || tcs[0].args != `{"x":1}` {
		t.Fatalf("第一个（畸形）解析错误: %+v", tcs[0])
	}
	if tcs[1].name != "Read" || tcs[1].args != `{"file_path":"a"}` {
		t.Fatalf("第二个（正常）解析错误: %+v", tcs[1])
	}
}

func TestParseSalvageBareName(t *testing.T) {
	// 裸名字调用：连 ] 都没写流就结束
	for _, s := range []string{`[调用工具 Bash`, `[调用工具 Bash]`, "[调用工具 Bash   "} {
		content, tcs := parseAll(s)
		if content != "" {
			t.Fatalf("裸名字 %q 泄漏成正文: %q", s, content)
		}
		if len(tcs) != 1 || tcs[0].name != "Bash" || tcs[0].args != "" {
			t.Fatalf("裸名字 %q 解析错误: content=%q calls=%+v", s, content, tcs)
		}
	}
}

func TestParseSalvageTruncatedJSON(t *testing.T) {
	// 流在 JSON 中途被截断：参数原文照转（客户端报参数错误可重试），不再泄漏成正文
	content, tcs := parseAll(`[调用工具 Bash({"command":"ls`)
	if content != "" {
		t.Fatalf("截断 JSON 泄漏成正文: %q", content)
	}
	if len(tcs) != 1 || tcs[0].args != `{"command":"ls` {
		t.Fatalf("截断 JSON 解析错误: content=%q calls=%+v", content, tcs)
	}
}

func TestParseToolNameCorrection(t *testing.T) {
	// 严格格式下模型把 Bash 写成 bash：按请求工具表不区分大小写纠回
	content, tcs := parseAllWithTools([]string{"Bash", "Read"}, `[调用工具 bash({"command":"ls"})]`)
	if content != "" || len(tcs) != 1 || tcs[0].name != "Bash" {
		t.Fatalf("工具名纠错失败: content=%q calls=%+v", content, tcs)
	}
}

func TestParseUnknownToolNameKept(t *testing.T) {
	// 名字不在请求工具表：原样透传，由客户端报「未知工具」触发重试
	content, tcs := parseAllWithTools([]string{"Read"}, `[调用工具 bash({"x":1})]`)
	if content != "" || len(tcs) != 1 || tcs[0].name != "bash" {
		t.Fatalf("未知名不应被改写: content=%q calls=%+v", content, tcs)
	}
}

func TestParseSalvageDoesNotSwallowProse(t *testing.T) {
	// 普通方括号文本 / 不像工具调用的内容不误吞
	cases := []string{
		`这是一个[例子]，不是工具调用`,
		`[调用工具]`,
		"[调用工具 Bash is great]",
		"[调用工具 说明如下]",
	}
	for _, c := range cases {
		content, tcs := parseAll(c)
		if content != c || len(tcs) != 0 {
			t.Errorf("普通文本 %q 被误吞: content=%q calls=%+v", c, content, tcs)
		}
	}
}

// TestMCloudCallStreamSalvagesMalformedToolCall 端到端：上游逐 chunk 输出笔误的工具调用
// （缺 )、多 }），CallStream 应容错还原为 tool_calls delta（名字纠回 Bash），
// finish_reason=tool_calls，且不把调用文本泄漏进 content。
func TestMCloudCallStreamSalvagesMalformedToolCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/assistant/chat/config"):
			io.WriteString(w, `{"success":true,"code":"0000"}`)
		case strings.HasSuffix(r.URL.Path, "/assistant/chat/v2/add"):
			w.Header().Set("Content-Type", "text/event-stream")
			fl := w.(http.Flusher)
			io.WriteString(w, mcDataFrame("blian_qwen38_max", "好的，我先查看目录。\n[调用工具 bash({\"co", "", false))
			fl.Flush()
			io.WriteString(w, mcDataFrame("blian_qwen38_max", `mmand":"ls -la /tmp 2>&1"}}]`, "", true))
			fl.Flush()
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	up := newTestMCloud(t, srv.URL)
	body, _ := json.Marshal(map[string]interface{}{
		"model":    "blian_qwen38_max",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
		"tools": []map[string]interface{}{{
			"type": "function",
			"function": map[string]interface{}{
				"name":       "Bash",
				"parameters": map[string]interface{}{"properties": map[string]interface{}{"command": map[string]string{"type": "string"}}},
			},
		}},
	})
	sr, err := up.CallStream(context.Background(), body)
	if err != nil {
		t.Fatalf("CallStream: %v", err)
	}
	defer sr.Body.Close()

	all, err := io.ReadAll(sr.Body)
	if err != nil {
		t.Fatalf("读流: %v", err)
	}
	s := string(all)
	if !strings.Contains(s, `"content":"好的，我先查看目录。\n"`) {
		t.Errorf("前置正文丢失:\n%s", s)
	}
	if !strings.Contains(s, `"finish_reason":"tool_calls"`) {
		t.Errorf("流输出缺 finish_reason=tool_calls:\n%s", s)
	}
	if strings.Contains(s, "调用工具") {
		t.Errorf("畸形工具调用文本泄漏到 content:\n%s", s)
	}
	// 解码 SSE 校验 tool_calls（json.Marshal 会把 2>&1 转义成 \u003e\u0026，不能 Contains 原文）
	var gotName, gotArgs string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") || strings.Contains(line, "[DONE]") {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					ToolCalls []struct {
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk); err != nil {
			continue
		}
		for _, c := range chunk.Choices {
			for _, tc := range c.Delta.ToolCalls {
				gotName += tc.Function.Name
				gotArgs += tc.Function.Arguments
			}
		}
	}
	if gotName != "Bash" {
		t.Errorf("工具名应纠回 Bash, got %q:\n%s", gotName, s)
	}
	if gotArgs != `{"command":"ls -la /tmp 2>&1"}` || !json.Valid([]byte(gotArgs)) {
		t.Errorf("容错提取的参数错误, got %q:\n%s", gotArgs, s)
	}
}
