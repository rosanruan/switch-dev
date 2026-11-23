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

// ====== DSML（灵犀 deepseek 系模型的原生工具调用标记）======

// dsmlRealSample 2026-11 实测泄漏样本：deepseek 系模型无视注入的 [调用工具 ...] 文本
// 协议，按原生习惯输出 DSML 标记（｜ 为全角 U+FF5C；参数属性与收尾 token 常被换行
// 拆开）。旧解析器不认识，整段泄漏成正文 → 客户端不执行工具、回合无声中断。
const dsmlRealSample = "Let me read main.js and app.go to ground the answer.\n" +
	"<｜｜DSML｜｜ calls>\n" +
	"<｜｜DSML｜｜ invoke name=\"Read\">\n" +
	"<｜｜DSML｜｜ parameter name=\"file_path\"\n" +
	"string=\"true\">C:/Users/xyb/Downloads/DramaKit/gui/DramaKit-GUI/frontend/src/main.js</｜｜DSML｜｜\n" +
	"parameter>\n" +
	"</｜｜DSML｜｜ invoke>\n" +
	"<｜｜DSML｜｜ invoke name=\"Read\">\n" +
	"<｜｜DSML｜｜ parameter name=\"file_path\"\n" +
	"string=\"true\">C:/Users/xyb/Downloads/DramaKit/gui/DramaKit-GUI/app.go</｜｜DSML｜｜\n" +
	"parameter>\n" +
	"</｜｜DSML｜｜ invoke>\n" +
	"</｜｜DSML｜｜ calls>"

// TestParseDSMLRealSample 实测泄漏样本逐字入流式解析器：必须还原出 2 个 Read 调用、
// 参数为合法 JSON、前置正文保留、DSML 标记不泄漏进 content。
func TestParseDSMLRealSample(t *testing.T) {
	content, tcs := parseAll(dsmlRealSample)
	if content != "Let me read main.js and app.go to ground the answer.\n" {
		t.Fatalf("前置正文被破坏: %q", content)
	}
	if len(tcs) != 2 {
		t.Fatalf("期望 2 个工具调用（而非泄漏成正文），got %d: content=%q", len(tcs), content)
	}
	wantArgs := []string{
		`{"file_path":"C:/Users/xyb/Downloads/DramaKit/gui/DramaKit-GUI/frontend/src/main.js"}`,
		`{"file_path":"C:/Users/xyb/Downloads/DramaKit/gui/DramaKit-GUI/app.go"}`,
	}
	for i, want := range wantArgs {
		if tcs[i].name != "Read" {
			t.Errorf("第 %d 个调用名 = %q, want Read", i, tcs[i].name)
		}
		if tcs[i].args != want || !json.Valid([]byte(tcs[i].args)) {
			t.Errorf("第 %d 个调用参数 = %q, want %q", i, tcs[i].args, want)
		}
	}
	if strings.Contains(content, "DSML") {
		t.Fatalf("DSML 标记泄漏进 content: %q", content)
	}
}

// TestParseDSMLAcrossChunks 上游逐小块吐出（token/属性/参数值任意位置截断），
// partial 等待逻辑必须拼回完整调用。
func TestParseDSMLAcrossChunks(t *testing.T) {
	for _, size := range []int{1, 7, 13, 64} {
		var chunks []string
		for i := 0; i < len(dsmlRealSample); i += size {
			end := i + size
			if end > len(dsmlRealSample) {
				end = len(dsmlRealSample)
			}
			chunks = append(chunks, dsmlRealSample[i:end])
		}
		content, tcs := parseAll(chunks...)
		if content != "Let me read main.js and app.go to ground the answer.\n" {
			t.Fatalf("size=%d 前置正文被破坏: %q", size, content)
		}
		if len(tcs) != 2 || tcs[0].name != "Read" || tcs[1].name != "Read" {
			t.Fatalf("size=%d 期望 2 个 Read 调用, got %d: %+v", size, len(tcs), tcs)
		}
		if !json.Valid([]byte(tcs[0].args)) || !json.Valid([]byte(tcs[1].args)) {
			t.Fatalf("size=%d 参数 JSON 非法: %+v", size, tcs)
		}
	}
}

func TestParseDSMLNoParams(t *testing.T) {
	content, tcs := parseAll("<｜｜DSML｜｜ calls><｜｜DSML｜｜ invoke name=\"ListAgents\"></｜｜DSML｜｜ invoke></｜｜DSML｜｜ calls>")
	if content != "" {
		t.Fatalf("无参调用泄漏成正文: %q", content)
	}
	if len(tcs) != 1 || tcs[0].name != "ListAgents" || tcs[0].args != "{}" {
		t.Fatalf("无参调用解析错误: content=%q calls=%+v", content, tcs)
	}
}

func TestParseDSMLMultiParams(t *testing.T) {
	// 两个参数，顺序还原
	content, tcs := parseAll(
		"<｜｜DSML｜｜ invoke name=\"Grep\">",
		"<｜｜DSML｜｜ parameter name=\"pattern\" string=\"true\">func main</｜｜DSML｜｜ parameter>",
		"<｜｜DSML｜｜ parameter name=\"glob\" string=\"true\">*.go</｜｜DSML｜｜ parameter>",
		"</｜｜DSML｜｜ invoke>")
	if content != "" {
		t.Fatalf("多参数调用泄漏成正文: %q", content)
	}
	want := `{"pattern":"func main","glob":"*.go"}`
	if len(tcs) != 1 || tcs[0].name != "Grep" || tcs[0].args != want {
		t.Fatalf("多参数解析错误: calls=%+v", tcs)
	}
}

func TestParseDSMLRawNumberParam(t *testing.T) {
	// string="false"：参数值是原样 JSON（数字），直接内嵌而非编码成字符串
	content, tcs := parseAll(
		`<｜｜DSML｜｜ invoke name="TodoWrite">` +
			`<｜｜DSML｜｜ parameter name="count" string="false">3</｜｜DSML｜｜ parameter>` +
			`</｜｜DSML｜｜ invoke>`)
	if content != "" {
		t.Fatalf("原样数值参数泄漏成正文: %q", content)
	}
	want := `{"count":3}`
	if len(tcs) != 1 || tcs[0].args != want || !json.Valid([]byte(tcs[0].args)) {
		t.Fatalf("原样数值参数解析错误: calls=%+v", tcs)
	}
}

func TestParseDSMLMissingWrapperClose(t *testing.T) {
	// 模型漏写 </...calls> 收尾：invoke 本身完整，必须照常还原、不泄漏
	content, tcs := parseAll(
		`<｜｜DSML｜｜ calls><｜｜DSML｜｜ invoke name="Read"><｜｜DSML｜｜ parameter name="file_path" string="true">a.go</｜｜DSML｜｜ parameter></｜｜DSML｜｜ invoke>`)
	if content != "" {
		t.Fatalf("缺 calls 收尾泄漏成正文: %q", content)
	}
	if len(tcs) != 1 || tcs[0].name != "Read" || tcs[0].args != `{"file_path":"a.go"}` {
		t.Fatalf("缺 calls 收尾解析错误: calls=%+v", tcs)
	}
}

func TestParseDSMLStrayCloseConsumed(t *testing.T) {
	// 残留的孤立 </...calls> 收尾 token 静默吞掉；其后尾随空白的归属由「后面还有
	// 没有单元」决定：无单元跟随时按正文透传，跟正文时原样保留，跟单元时随单元吞掉
	content, tcs := parseAll("</｜｜DSML｜｜ calls>\n")
	if content != "\n" || len(tcs) != 0 {
		t.Fatalf("孤立收尾 token 处理错误: content=%q calls=%+v", content, tcs)
	}
	content, tcs = parseAll("</｜｜DSML｜｜ calls>\n好的")
	if content != "\n好的" || len(tcs) != 0 {
		t.Fatalf("收尾 token 后正文被破坏: content=%q calls=%+v", content, tcs)
	}
	content, tcs = parseAll("</｜｜DSML｜｜ calls>\n<｜｜DSML｜｜ invoke name=\"Read\"></｜｜DSML｜｜ invoke>")
	if content != "" || len(tcs) != 1 || tcs[0].name != "Read" {
		t.Fatalf("收尾 token 后单元间空白应被吞掉: content=%q calls=%+v", content, tcs)
	}
}

func TestParseDSMLTruncatedValueSalvage(t *testing.T) {
	// 流在参数值中途被截断：value 取剩余全部照转（客户端报参数错误可重试），不泄漏
	content, tcs := parseAll(
		`<｜｜DSML｜｜ calls><｜｜DSML｜｜ invoke name="Read">` +
			`<｜｜DSML｜｜ parameter name="file_path" string="true">C:/x/ma`)
	if content != "" {
		t.Fatalf("截断参数泄漏成正文: %q", content)
	}
	if len(tcs) != 1 || tcs[0].name != "Read" || tcs[0].args != `{"file_path":"C:/x/ma"}` {
		t.Fatalf("截断参数容错解析错误: calls=%+v", tcs)
	}
}

func TestParseDSMLTruncatedInvokeHeader(t *testing.T) {
	// 流断在 invoke 头部：有名字就转 tool_call（空参数由客户端报错重试）
	content, tcs := parseAll(`<｜｜DSML｜｜ calls><｜｜DSML｜｜ invoke name="Read">`)
	if content != "" {
		t.Fatalf("截断 invoke 泄漏成正文: %q", content)
	}
	if len(tcs) != 1 || tcs[0].name != "Read" || tcs[0].args != "{}" {
		t.Fatalf("截断 invoke 容错解析错误: calls=%+v", tcs)
	}
}

func TestParseDSMLParamClosedByInvokeClose(t *testing.T) {
	// 模型漏写 </...parameter>：value 撞上 </...invoke> —— 参数保留、invoke 一并收口
	content, tcs := parseAll(
		`<｜｜DSML｜｜ invoke name="Read">` +
			`<｜｜DSML｜｜ parameter name="file_path" string="true">a.go</｜｜DSML｜｜ invoke>`)
	if content != "" {
		t.Fatalf("缺 parameter 收尾泄漏成正文: %q", content)
	}
	if len(tcs) != 1 || tcs[0].name != "Read" || tcs[0].args != `{"file_path":"a.go"}` {
		t.Fatalf("缺 parameter 收尾解析错误: calls=%+v", tcs)
	}
}

// TestParseDSMLNotTriggeredByOrdinaryText 普通 HTML/含尖括号文本零误吞、零泄漏。
func TestParseDSMLNotTriggeredByOrdinaryText(t *testing.T) {
	cases := []string{
		`<div>hello</div>`,
		`a < b`,
		"x <｜ y",
		"</div>",
		"<|| foo",
		"5 < 6 > 4",
	}
	for _, c := range cases {
		content, tcs := parseAll(c)
		if content != c || len(tcs) != 0 {
			t.Errorf("普通文本 %q 被误吞: content=%q calls=%+v", c, content, tcs)
		}
	}
}

func TestParseDSMLMixedWithTextProtocol(t *testing.T) {
	// 同一段输出混用两种格式（deepseek 系模型实测会漂移）：各自还原
	content, tcs := parseAll(
		`[调用工具 Bash({"command":"ls"})]`,
		"<｜｜DSML｜｜ invoke name=\"Read\"><｜｜DSML｜｜ parameter name=\"file_path\" string=\"true\">a.go</｜｜DSML｜｜ parameter></｜｜DSML｜｜ invoke>")
	if content != "" {
		t.Fatalf("混用格式泄漏成正文: %q", content)
	}
	if len(tcs) != 2 {
		t.Fatalf("期望 2 个工具调用, got %d", len(tcs))
	}
	if tcs[0].name != "Bash" || tcs[0].args != `{"command":"ls"}` {
		t.Errorf("文本协议调用解析错误: %+v", tcs[0])
	}
	if tcs[1].name != "Read" || tcs[1].args != `{"file_path":"a.go"}` {
		t.Errorf("DSML 调用解析错误: %+v", tcs[1])
	}
}

func TestParseDSMLNameCorrection(t *testing.T) {
	// DSML 里的工具名笔误同样按请求工具表纠错
	content, tcs := parseAllWithTools([]string{"Read", "Bash"},
		`<｜｜DSML｜｜ invoke name="read"><｜｜DSML｜｜ parameter name="file_path" string="true">a.go</｜｜DSML｜｜ parameter></｜｜DSML｜｜ invoke>`)
	if content != "" || len(tcs) != 1 || tcs[0].name != "Read" {
		t.Fatalf("DSML 工具名纠错失败: content=%q calls=%+v", content, tcs)
	}
}

func TestParseDSMLASCIIVariant(t *testing.T) {
	// ASCII || 变体（防上游转写差异）同样识别
	content, tcs := parseAll(
		`<||DSML|| calls><||DSML|| invoke name="Bash">` +
			`<||DSML|| parameter name="command" string="true">ls</||DSML|| parameter>` +
			`</||DSML|| invoke></||DSML|| calls>`)
	if content != "" {
		t.Fatalf("ASCII 变体泄漏成正文: %q", content)
	}
	if len(tcs) != 1 || tcs[0].name != "Bash" || tcs[0].args != `{"command":"ls"}` {
		t.Fatalf("ASCII 变体解析错误: calls=%+v", tcs)
	}
}

// TestMCloudCallStreamDSMLToolCall 端到端：上游逐帧输出实测泄漏样本（含换行拆开的
// 属性与收尾 token），CallStream 应还原出 2 个 Read 的 tool_calls delta、
// finish_reason=tool_calls，且 DSML 标记不泄漏进 content。
func TestMCloudCallStreamDSMLToolCall(t *testing.T) {
	frames := []string{
		"Let me read main.js and app.go to ground the answer.\n<｜｜DSML｜｜ calls>\n<｜｜DSML｜｜ invoke name=\"Read\">\n<｜｜DSML｜｜ parameter name=\"file_path\"\n",
		"string=\"true\">C:/Users/xyb/Downloads/DramaKit/gui/DramaKit-GUI/frontend/src/main.js</｜｜DSML｜｜\n",
		"parameter>\n</｜｜DSML｜｜ invoke>\n<｜｜DSML｜｜ invoke name=\"Read\">\n<｜｜DSML｜｜ parameter name=\"file_path\"\n",
		"string=\"true\">C:/Users/xyb/Downloads/DramaKit/gui/DramaKit-GUI/app.go</｜｜DSML｜｜\n",
		"parameter>\n</｜｜DSML｜｜ invoke>\n</｜｜DSML｜｜ calls>",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/assistant/chat/config"):
			io.WriteString(w, `{"success":true,"code":"0000"}`)
		case strings.HasSuffix(r.URL.Path, "/assistant/chat/v2/add"):
			w.Header().Set("Content-Type", "text/event-stream")
			fl := w.(http.Flusher)
			for i, f := range frames {
				io.WriteString(w, mcDataFrame("blian_qwen38_max", f, "", i == len(frames)-1))
				fl.Flush()
			}
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
				"name":       "Read",
				"parameters": map[string]interface{}{"properties": map[string]interface{}{"file_path": map[string]string{"type": "string"}}},
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
	if !strings.Contains(s, `"content":"Let me read main.js and app.go to ground the answer.\n"`) {
		t.Errorf("前置正文丢失:\n%s", s)
	}
	if !strings.Contains(s, `"finish_reason":"tool_calls"`) {
		t.Errorf("流输出缺 finish_reason=tool_calls:\n%s", s)
	}
	if strings.Contains(s, "DSML") {
		t.Errorf("DSML 标记泄漏到输出:\n%s", s)
	}

	// 解码 SSE，按 index 聚合两个 tool_call 的 name/arguments
	type gotTC struct{ name, args string }
	got := map[int]*gotTC{}
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") || strings.Contains(line, "[DONE]") {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					ToolCalls []struct {
						Index    int `json:"index"`
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
				g := got[tc.Index]
				if g == nil {
					g = &gotTC{}
					got[tc.Index] = g
				}
				if tc.Function.Name != "" {
					g.name = tc.Function.Name
				}
				g.args += tc.Function.Arguments
			}
		}
	}
	wantArgs := []string{
		`{"file_path":"C:/Users/xyb/Downloads/DramaKit/gui/DramaKit-GUI/frontend/src/main.js"}`,
		`{"file_path":"C:/Users/xyb/Downloads/DramaKit/gui/DramaKit-GUI/app.go"}`,
	}
	for i, want := range wantArgs {
		g := got[i]
		if g == nil {
			t.Errorf("缺第 %d 个 tool_call:\n%s", i, s)
			continue
		}
		if g.name != "Read" {
			t.Errorf("第 %d 个调用名 = %q, want Read", i, g.name)
		}
		if g.args != want || !json.Valid([]byte(g.args)) {
			t.Errorf("第 %d 个调用参数 = %q, want %q", i, g.args, want)
		}
	}
}
