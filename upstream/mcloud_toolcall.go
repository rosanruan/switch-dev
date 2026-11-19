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
	tools     map[string]string // 小写工具名 → 规范名（来自请求 tools 清单），纠模型笔误的大小写
}

func (p *mcloudToolCallParser) hasToolCall() bool { return len(p.toolCalls) > 0 }

// setToolNames 登记请求 tools 清单的规范名，供 emitToolCallDeltas 做名字纠错。
func (p *mcloudToolCallParser) setToolNames(names []string) {
	if len(names) == 0 {
		return
	}
	m := make(map[string]string, len(names))
	for _, n := range names {
		if n != "" {
			m[strings.ToLower(n)] = n
		}
	}
	p.tools = m
}

// canonicalName 把模型笔误的工具名（如 bash）映射回请求清单里的规范名（Bash）。
// 找不到映射时原样返回，由客户端报「未知工具」触发重试。
func (p *mcloudToolCallParser) canonicalName(name string) string {
	if p.tools == nil {
		return name
	}
	if c, ok := p.tools[strings.ToLower(name)]; ok {
		return c
	}
	return name
}

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

// flush 流结束时调用，循环处理到缓冲清空：先严格解析，失败再容错提取（模型笔误），
// 都不像工具调用才按正文吐出。循环保证「一个畸形调用 + 后续正常调用」同缓冲时
// 全部还原，不再整段泄漏成正文。
func (p *mcloudToolCallParser) flush() []mcStreamDelta {
	var out []mcStreamDelta
	s := p.buf
	p.buf = ""
	p.inBuf = false
	for s != "" {
		if status, tc, consumed := tryParseMCloudToolCall(s); status == "matched" {
			out = append(out, p.emitToolCallDeltas(tc)...)
			s = s[consumed:]
			continue
		}
		if tc, consumed, ok := salvageMCloudToolCall(s); ok {
			out = append(out, p.emitToolCallDeltas(tc)...)
			s = s[consumed:]
			continue
		}
		// 非工具调用：正文透传到下一个 [ 为止（其后可能还藏着工具调用，回循环继续）
		var cut int
		switch {
		case strings.HasPrefix(s, "["):
			if next := strings.IndexByte(s[1:], '['); next >= 0 {
				cut = next + 1
			} else {
				cut = len(s)
			}
		default:
			if idx := strings.IndexByte(s, '['); idx > 0 {
				cut = idx
			} else {
				cut = len(s)
			}
		}
		text := s[:cut]
		p.content.WriteString(text)
		out = append(out, mcStreamDelta{Content: text})
		s = s[cut:]
	}
	return out
}

// emitToolCallDeltas 把一个工具调用转成 1~2 个 tool_calls delta（先 id+name，再 args），
// 并登记到 parser.toolCalls 供非流式聚合 / finish_reason 判断。
func (p *mcloudToolCallParser) emitToolCallDeltas(tc mcloudToolCall) []mcStreamDelta {
	tc.index = p.nextIndex
	p.nextIndex++
	tc.id = "call_" + randString(12)
	tc.name = p.canonicalName(tc.name)
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

// salvageMCloudToolCall 流结束时的容错提取（仅 flush 调用）。严格语法对模型笔误
// （缺 )、缺 ]、] 前多写 }）会永远停在 partial，流结束时整段泄漏成正文 ——
// 客户端拿到的是普通文本：不执行工具、不回 tool_result，agent loop 直接死。
// 这里强制提取：名字照常识别，参数取名字后第一个括号平衡的 {...}；JSON 没闭合
// （流被截断）时参数按原文照转。哪怕参数是坏的也照样返回 tool_call —— 客户端会
// 对坏参数回「工具执行失败」的 tool_result，回合能继续重试；漏成正文是回合直接死。
// ok=false 表示不像工具调用（无名字，或名字后跟正文），调用方按正文处理。
func salvageMCloudToolCall(s string) (tc mcloudToolCall, consumed int, ok bool) {
	const prefix = mcloudToolCallPrefix
	if !strings.HasPrefix(s, prefix) {
		return mcloudToolCall{}, 0, false
	}
	rest := s[len(prefix):]
	base := len(prefix)
	i := 0
	for i < len(rest) && isToolCallSpace(rest[i]) {
		i++
	}
	j := i
	for j < len(rest) && isToolCallIdent(rest[j]) {
		j++
	}
	if j == i {
		return mcloudToolCall{}, 0, false // 名字都没有
	}
	name := rest[i:j]

	k := j
	for k < len(rest) && isToolCallSpace(rest[k]) {
		k++
	}
	if k < len(rest) && rest[k] == '(' {
		k++ // 容忍缺 ) 的 Name({json} 笔误：跳过开括号直取 JSON
		for k < len(rest) && isToolCallSpace(rest[k]) {
			k++
		}
	}
	if k < len(rest) && rest[k] == '{' {
		if end, balanced := findBalancedParen(rest, k, '{', '}'); balanced {
			args := rest[k : end+1]
			q := end + 1
			for q < len(rest) && (isToolCallSpace(rest[q]) || rest[q] == ')' || rest[q] == ']' || rest[q] == '}') {
				q++ // 吞掉参数后的 ) ] } 收尾残渣，剩余文本留给外层循环
			}
			return mcloudToolCall{name: name, args: args}, base + q, true
		}
		// JSON 未闭合（截断）：参数按原文截到结尾照转，客户端报参数错误后可重试
		return mcloudToolCall{name: name, args: rest[k:]}, len(s), true
	}
	// 无参数：名字后只剩 ] ) } 空白或已到头才当裸名字调用，跟了正文则按正文处理
	q := k
	for q < len(rest) && (isToolCallSpace(rest[q]) || rest[q] == ']' || rest[q] == ')' || rest[q] == '}') {
		q++
	}
	if q < len(rest) {
		return mcloudToolCall{}, 0, false
	}
	return mcloudToolCall{name: name}, base + q, true
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
