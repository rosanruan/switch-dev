package upstream

import (
	"strings"
)

// 本文件实现「文本工具调用 → 结构化 tool_calls」的流式解析。
//
// 背景：灵犀（移动云盘 webai）走私有文本协议，不支持 OpenAI function calling。
// switch-dev 在折叠历史时把 assistant 的 tool_calls 转写成 [调用工具 Name(args)] 文本
// （见 foldMCloudDialogue），上游模型会模仿该格式用纯文本输出工具调用。这里在响应侧
// 把这段文本还原为结构化 tool_calls delta，让下游 StreamOpenAIToAnthropic 复用已有的
// tool_calls -> tool_use 流式转换，从而让 Claude Code 真正拿到可执行的 tool_use block。
//
// 兼容两种模型实际输出过的格式：
//   - [调用工具 Bash({"command":"ls"})]        （qwen 式：参数包在括号里）
//   - [调用工具 Bash] {"command":"ls"}          （deepseek 式：] 后跟 JSON）
//   - [调用工具 Bash]                           （无参数）

// mcStreamToolCall OpenAI 流式 tool_calls delta 的单项（json tag 与下游 streamOpenAIChunk 一致）
type mcStreamToolCall struct {
	Index    int                      `json:"index"`
	ID       string                   `json:"id,omitempty"`
	Type     string                   `json:"type,omitempty"`
	Function mcStreamToolCallFunction `json:"function"`
}

type mcStreamToolCallFunction struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// mcloudToolCall 一次解析出的工具调用
type mcloudToolCall struct {
	index int
	id    string
	name  string
	args  string // JSON 参数原文（可能为空）
}

// mcloudToolCallParser 逐 chunk 累积文本，识别 [调用工具 ...] 标记。
// 只缓冲「从最近的 [ 开始的文本」，普通文本实时透传，尽量保持流式实时性。
type mcloudToolCallParser struct {
	buf       string // 未决文本（遇到 [ 后累积在此）
	inBuf     bool
	toolCalls []mcloudToolCall // 已解析出的工具调用（按出现顺序）
	content   strings.Builder  // 普通文本累积（不含工具调用），供非流式聚合
	nextIndex int
}

func (p *mcloudToolCallParser) hasToolCall() bool { return len(p.toolCalls) > 0 }

// feed 处理一段新文本，返回需要 emit 的 delta 序列。
func (p *mcloudToolCallParser) feed(chunk string) []mcStreamDelta {
	var out []mcStreamDelta

	if !p.inBuf {
		idx := strings.IndexByte(chunk, '[')
		if idx < 0 {
			if chunk != "" {
				p.content.WriteString(chunk)
				out = append(out, mcStreamDelta{Content: chunk})
			}
			return out
		}
		if idx > 0 {
			p.content.WriteString(chunk[:idx])
			out = append(out, mcStreamDelta{Content: chunk[:idx]})
		}
		p.buf = chunk[idx:]
		p.inBuf = true
	} else {
		p.buf += chunk
	}

	for {
		status, tc, consumed := tryParseMCloudToolCall(p.buf)
		switch status {
		case "partial":
			return out
		case "rejected":
			// buf 以 [ 开头但不是 [调用工具 前缀：flush 到下一个 [ 之前，继续找
			rest := p.buf
			next := strings.Index(rest[1:], "[")
			if next < 0 {
				p.content.WriteString(rest)
				out = append(out, mcStreamDelta{Content: rest})
				p.buf = ""
				p.inBuf = false
				return out
			}
			next++ // 相对 rest 的索引
			p.content.WriteString(rest[:next])
			out = append(out, mcStreamDelta{Content: rest[:next]})
			p.buf = rest[next:]
		case "matched":
			out = append(out, p.emitToolCallDeltas(tc)...)
			p.buf = p.buf[consumed:]
			if p.buf == "" {
				p.inBuf = false
				return out
			}
			// 剩余部分可能又含 [，继续循环
		}
	}
}

// flush 流结束时调用：把未决文本作为工具调用（[调用工具 Name] 无参数）或普通文本吐出。
func (p *mcloudToolCallParser) flush() []mcStreamDelta {
	var out []mcStreamDelta
	s := p.buf
	p.buf = ""
	p.inBuf = false
	if s == "" {
		return out
	}

	// 1) 完整工具调用（Name({args}) 或 Name] {args}）
	if status, tc, consumed := tryParseMCloudToolCall(s); status == "matched" {
		out = append(out, p.emitToolCallDeltas(tc)...)
		if consumed < len(s) {
			rest := s[consumed:]
			p.content.WriteString(rest)
			out = append(out, mcStreamDelta{Content: rest})
		}
		return out
	}

	// 2) 无参数工具调用 [调用工具 Name]（流已结束，args 不会再出现）
	if name, ok := tryParseBareToolName(s); ok {
		out = append(out, p.emitToolCallDeltas(mcloudToolCall{name: name})...)
		return out
	}

	// 3) 其余：普通文本
	p.content.WriteString(s)
	out = append(out, mcStreamDelta{Content: s})
	return out
}

// emitToolCallDeltas 把一个工具调用转成 1~2 个 tool_calls delta（先 id+name，再 args），
// 并登记到 parser.toolCalls 供非流式聚合 / finish_reason 判断。
func (p *mcloudToolCallParser) emitToolCallDeltas(tc mcloudToolCall) []mcStreamDelta {
	tc.index = p.nextIndex
	p.nextIndex++
	tc.id = "call_" + randString(12)
	p.toolCalls = append(p.toolCalls, tc)

	var out []mcStreamDelta
	out = append(out, mcStreamDelta{ToolCalls: []mcStreamToolCall{{
		Index:    tc.index,
		ID:       tc.id,
		Type:     "function",
		Function: mcStreamToolCallFunction{Name: tc.name},
	}}})
	if tc.args != "" {
		out = append(out, mcStreamDelta{ToolCalls: []mcStreamToolCall{{
			Index:    tc.index,
			Function: mcStreamToolCallFunction{Arguments: tc.args},
		}}})
	}
	return out
}

const mcloudToolCallPrefix = "[调用工具"

// tryParseMCloudToolCall 尝试从 s 开头解析工具调用。
// status 取值：
//   - "matched"：解析出完整工具调用，consumed 为消费字节数
//   - "partial"：s 是合法前缀但不完整，需要更多输入
//   - "rejected"：s 不是工具调用开头（不匹配 [调用工具）
func tryParseMCloudToolCall(s string) (status string, tc mcloudToolCall, consumed int) {
	const prefix = mcloudToolCallPrefix
	if len(s) < len(prefix) {
		if strings.HasPrefix(prefix, s) {
			return "partial", tc, 0
		}
		return "rejected", tc, 0
	}
	if !strings.HasPrefix(s, prefix) {
		return "rejected", tc, 0
	}

	rest := s[len(prefix):]
	base := len(prefix) // consumed 统一按相对 s 开头的字节数返回

	i := 0
	for i < len(rest) && isToolCallSpace(rest[i]) {
		i++
	}
	j := i
	for j < len(rest) && isToolCallIdent(rest[j]) {
		j++
	}
	if j == i {
		return "partial", tc, 0 // 工具名还没到
	}
	name := rest[i:j]

	k := j
	for k < len(rest) && isToolCallSpace(rest[k]) {
		k++
	}
	if k >= len(rest) {
		return "partial", tc, 0 // 工具名后还没内容
	}

	switch rest[k] {
	case '(':
		// 格式 A: Name({json})
		end, ok := findBalancedParen(rest, k, '(', ')')
		if !ok {
			return "partial", tc, 0 // 括号未闭合
		}
		args := rest[k+1 : end]
		q := end + 1
		for q < len(rest) && isToolCallSpace(rest[q]) {
			q++
		}
		if q >= len(rest) {
			return "partial", tc, 0 // 还没到 ]
		}
		if rest[q] == ']' {
			return "matched", mcloudToolCall{name: name, args: args}, base + q + 1
		}
		return "rejected", tc, 0 // ) 后跟了奇怪内容

	case ']':
		// 格式 B/C: Name] 或 Name] {json}
		q := k + 1
		for q < len(rest) && isToolCallSpace(rest[q]) {
			q++
		}
		if q >= len(rest) {
			// 只有 Name]，参数可能还在后面（下一 chunk）→ partial
			return "partial", tc, 0
		}
		if rest[q] == '{' {
			end, ok := findBalancedParen(rest, q, '{', '}')
			if !ok {
				return "partial", tc, 0 // JSON 未闭合
			}
			args := rest[q : end+1]
			return "matched", mcloudToolCall{name: name, args: args}, base + end + 1
		}
		// Name] 后跟非 { 内容：无参数工具调用，consumed 停在 ] 之后
		return "matched", mcloudToolCall{name: name}, base + k + 1

	default:
		return "rejected", tc, 0
	}
}

// findBalancedParen 从 start（指向 open 括号）起找配对的 close 括号位置。
// 跳过 JSON 字符串（"..." 及 \ 转义），正确处理字符串里嵌套的括号（如命令里含 )）。
func findBalancedParen(s string, start int, open, close byte) (int, bool) {
	depth := 0
	inString := false
	escaped := false
	for p := start; p < len(s); p++ {
		ch := s[p]
		if inString {
			if escaped {
				escaped = false
			} else if ch == '\\' {
				escaped = true
			} else if ch == '"' {
				inString = false
			}
			continue
		}
		switch ch {
		case '"':
			inString = true
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return p, true
			}
		}
	}
	return 0, false
}

// tryParseBareToolName 判断 s 是否为 [调用工具 Name]（无参数，] 结尾）。
func tryParseBareToolName(s string) (string, bool) {
	const prefix = mcloudToolCallPrefix
	if !strings.HasPrefix(s, prefix) {
		return "", false
	}
	rest := s[len(prefix):]
	i := 0
	for i < len(rest) && isToolCallSpace(rest[i]) {
		i++
	}
	j := i
	for j < len(rest) && isToolCallIdent(rest[j]) {
		j++
	}
	if j == i {
		return "", false
	}
	name := rest[i:j]
	for k := j; k < len(rest); k++ {
		if !isToolCallSpace(rest[k]) {
			return name, rest[k] == ']' && k == len(rest)-1
		}
	}
	// 全是空白（无 ]）：不是裸工具调用
	return "", false
}

func isToolCallSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

func isToolCallIdent(b byte) bool {
	return b == '_' ||
		(b >= 'a' && b <= 'z') ||
		(b >= 'A' && b <= 'Z') ||
		(b >= '0' && b <= '9')
}
