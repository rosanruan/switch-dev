package upstream

import (
	"encoding/json"
	"strings"
)

// 本文件识别灵犀 deepseek 系模型的原生 DSML 工具调用标记。
//
// 背景：灵犀不支持 function calling，请求侧注入的是 [调用工具 ...] 文本协议（见
// mcloudToolsPrompt / foldMCloudDialogue）。但 deepseek 系模型偶尔无视注入的文本协议、
// 按原生习惯输出 DSML 标记（｜ 为全角 U+FF5C，实测泄漏样本 2026-11）：
//
//	<｜｜DSML｜｜ calls>
//	  <｜｜DSML｜｜ invoke name="Read">
//	    <｜｜DSML｜｜ parameter name="file_path" string="true">C:/x/main.js</｜｜DSML｜｜ parameter>
//	  </｜｜DSML｜｜ invoke>
//	</｜｜DSML｜｜ calls>
//
// 旧解析器只认 [调用工具 ...]，DSML 整段泄漏成正文 → 客户端拿到的是文字而非
// tool_use，工具不执行、回合直接死（表现为 Claude Code 对话无声中断）。
// 这里与文本协议并列识别 DSML，feed/flush 集成见 mcloud_toolcall.go。
//
// 解析单位是「一段」而非「整块」：<...calls> 包装（0 个调用，静默吞掉）与单个完整
// invoke（1 个调用）各自独立成段，这样模型漏写 </...calls> 收尾也不会卡住已完整的
// invoke；流式中途截断由 flush 侧 salvageDSML 容错提取。

// dsmlTokenVariants DSML 特殊 token 的全部写法：全角 ｜（实测）与 ASCII ||（防转写差异），
// 开/闭各两种。闭合 token 排前，避免前缀歧义。
var dsmlTokenVariants = []string{
	"</｜｜DSML｜｜", "<｜｜DSML｜｜",
	"</||DSML||", "<||DSML||",
}

// dsmlElements DSML 元素名
var dsmlElements = []string{"calls", "invoke", "parameter"}

// dsmlParam 一个 <...parameter> 的还原结果
type dsmlParam struct {
	name  string
	value string
	raw   bool // string="false"：value 是原样 JSON 值（数字/布尔等），非字符串
}

// matchDSMLToken 检查 s 开头是否为 DSML 特殊 token。
// 完整命中返回 token 字节数与是否闭合 token；s 是某 token 的真前缀（流还没吐完）时
// partial=true；否则零值。
func matchDSMLToken(s string) (n int, closing, partial bool) {
	for _, tok := range dsmlTokenVariants {
		if strings.HasPrefix(s, tok) {
			return len(tok), strings.HasPrefix(tok, "</"), false
		}
	}
	for _, tok := range dsmlTokenVariants {
		if len(s) < len(tok) && strings.HasPrefix(tok, s) {
			return 0, strings.HasPrefix(tok, "</"), true
		}
	}
	return 0, false, false
}

// scanDSMLElement 读 s 开头的元素名（calls/invoke/parameter）。
// name 为已知元素的截断前缀时 partial=true（需要更多输入）；未知名字原样返回由调用方裁决。
func scanDSMLElement(s string) (name string, partial bool, n int) {
	j := 0
	for j < len(s) && isToolCallIdent(s[j]) {
		j++
	}
	name = s[:j]
	if j == 0 && len(s) > 0 {
		// 开头就不是元素名字符：不可能是任何元素的截断前缀（空 s 仍按 partial 处理）
		return name, false, 0
	}
	for _, el := range dsmlElements {
		if name == el {
			return name, false, j
		}
		if len(el) > len(name) && strings.HasPrefix(el, name) {
			return name, true, j
		}
	}
	return name, false, j
}

// scanDSMLAttrs 扫描属性表到 '>' 为止（如 name="Read" string="true"）。
// state："closed"（已见 > 收口）/ "partial"（输入截断，属性表还没写完）/ "junk"（非法语法）。
// 属性值支持双引号包裹与裸标识两种写法。
func scanDSMLAttrs(s string) (attrs map[string]string, state string, n int) {
	attrs = map[string]string{}
	i := 0
	for {
		for i < len(s) && isToolCallSpace(s[i]) {
			i++
		}
		if i >= len(s) {
			return attrs, "partial", i
		}
		if s[i] == '>' {
			return attrs, "closed", i + 1
		}
		j := i
		for j < len(s) && isToolCallIdent(s[j]) {
			j++
		}
		if j == i {
			return attrs, "junk", i
		}
		key := s[i:j]
		i = j
		for i < len(s) && isToolCallSpace(s[i]) {
			i++
		}
		if i >= len(s) {
			return attrs, "partial", i
		}
		if s[i] != '=' {
			return attrs, "junk", i
		}
		i++
		for i < len(s) && isToolCallSpace(s[i]) {
			i++
		}
		if i >= len(s) {
			return attrs, "partial", i
		}
		if s[i] == '"' {
			end := strings.IndexByte(s[i+1:], '"')
			if end < 0 {
				return attrs, "partial", len(s)
			}
			attrs[key] = s[i+1 : i+1+end]
			i = i + 1 + end + 1
		} else {
			j = i
			for j < len(s) && isToolCallIdent(s[j]) {
				j++
			}
			if j == i {
				return attrs, "junk", i
			}
			attrs[key] = s[i:j]
			i = j
		}
	}
}

// tryParseDSMLBlock 从 s 开头（允许前置空白）解析一段 DSML：
//   - <...calls> 或 </...calls> 包装：matched，0 个调用（静默吞掉）
//   - 一个完整 invoke（含 </...invoke> 收口）：matched，1 个调用
//
// status 语义与 tryParseMCloudToolCall 一致（matched/partial/rejected）。
// invoke 内多个 parameter 依次还原；参数 value 里的 '<' 不特殊处理，只认
// 「闭合 token + 元素名 + >」为收尾（findDSMLValueEnd）。
func tryParseDSMLBlock(s string) (status string, tcs []mcloudToolCall, consumed int) {
	i := 0
	for i < len(s) && isToolCallSpace(s[i]) {
		i++
	}
	if i >= len(s) {
		return "partial", nil, 0 // 只有空白：后续可能跟上 DSML
	}
	tl, closing, pr := matchDSMLToken(s[i:])
	if pr {
		return "partial", nil, 0
	}
	if tl == 0 {
		return "rejected", nil, 0
	}
	i += tl
	for i < len(s) && isToolCallSpace(s[i]) {
		i++
	}
	name, np, nn := scanDSMLElement(s[i:])
	if np {
		return "partial", nil, 0
	}
	i += nn

	if name == "calls" {
		// 开/闭 calls 包装：无属性，直取 '>'，0 个调用吞掉即可
		for i < len(s) && isToolCallSpace(s[i]) {
			i++
		}
		if i >= len(s) {
			return "partial", nil, 0
		}
		if s[i] != '>' {
			return "rejected", nil, 0
		}
		return "matched", nil, i + 1
	}
	if closing || name != "invoke" {
		return "rejected", nil, 0
	}

	// invoke 属性表（name 必需），形如 <...invoke name="Read">
	attrs, state, n := scanDSMLAttrs(s[i:])
	if state == "partial" {
		return "partial", nil, 0
	}
	if state != "closed" {
		return "rejected", nil, 0
	}
	i += n
	toolName := attrs["name"]
	if toolName == "" {
		return "rejected", nil, 0
	}

	var params []dsmlParam
	for {
		for i < len(s) && isToolCallSpace(s[i]) {
			i++
		}
		if i >= len(s) {
			return "partial", nil, 0
		}
		tl2, cl2, pr2 := matchDSMLToken(s[i:])
		if pr2 {
			return "partial", nil, 0
		}
		if tl2 == 0 {
			// 非法字符：跳到下一个 '<'（注意从 i+1 找，s[i] 本身可能是不成 token 的 '<'）
			nx := strings.IndexByte(s[i+1:], '<')
			if nx < 0 {
				return "partial", nil, 0
			}
			i += 1 + nx
			continue
		}
		j := i + tl2
		for j < len(s) && isToolCallSpace(s[j]) {
			j++
		}
		nm2, np2, nn2 := scanDSMLElement(s[j:])
		if np2 {
			return "partial", nil, 0
		}
		j += nn2

		if cl2 {
			// 闭合 token：只接受 </...invoke> 收口
			for j < len(s) && isToolCallSpace(s[j]) {
				j++
			}
			if j >= len(s) {
				return "partial", nil, 0
			}
			if s[j] != '>' || nm2 != "invoke" {
				return "rejected", nil, 0
			}
			return "matched", []mcloudToolCall{{name: toolName, args: dsmlArgsJSON(params)}}, j + 1
		}

		// 开 token：<...parameter name="..." [string="..."]>value</...parameter>
		if nm2 != "parameter" {
			return "rejected", nil, 0
		}
		pattrs, st2, n3 := scanDSMLAttrs(s[j:])
		if st2 == "partial" {
			return "partial", nil, 0
		}
		if st2 != "closed" {
			return "rejected", nil, 0
		}
		j += n3
		pn := pattrs["name"]
		if pn == "" {
			return "rejected", nil, 0
		}
		raw := pattrs["string"] == "false"
		end, tl3, elem3, found := findDSMLValueEnd(s, j)
		if !found {
			return "partial", nil, 0 // value 的收尾 token 还没到
		}
		params = append(params, dsmlParam{name: pn, value: s[j:end], raw: raw})
		// 消费 value + 闭合 token + 元素名 + '>'
		k := end + tl3
		for k < len(s) && isToolCallSpace(s[k]) {
			k++
		}
		_, _, nn3 := scanDSMLElement(s[k:])
		k += nn3
		for k < len(s) && isToolCallSpace(s[k]) {
			k++
		}
		if k >= len(s) {
			return "partial", nil, 0
		}
		if s[k] != '>' {
			return "rejected", nil, 0
		}
		k++
		if elem3 == "parameter" {
			i = k
			continue
		}
		// value 的收尾 token 后随的不是 parameter：value 实际撞上了 </...invoke>，
		// 即模型漏写了 </...parameter> —— 参数保留、invoke 一并收口
		if elem3 == "invoke" {
			return "matched", []mcloudToolCall{{name: toolName, args: dsmlArgsJSON(params)}}, k
		}
		return "rejected", nil, 0
	}
}

// findDSMLValueEnd 从 from 起找参数 value 的结束：第一个后随「元素名 + >」的闭合 token。
// 返回 value 结束位置、闭合 token 长度、后随元素名。value 里的 '<'、开 token、不成
// token 的 '<' 一律跳过继续找（value 原样保留）。
func findDSMLValueEnd(s string, from int) (end, tokLen int, elem string, found bool) {
	for pos := from; pos < len(s); {
		rel := strings.IndexByte(s[pos:], '<')
		if rel < 0 {
			return 0, 0, "", false
		}
		i := pos + rel
		tl, cl, pr := matchDSMLToken(s[i:])
		if pr || tl == 0 || !cl {
			pos = i + 1
			continue
		}
		j := i + tl
		for j < len(s) && isToolCallSpace(s[j]) {
			j++
		}
		nm, _, nn := scanDSMLElement(s[j:])
		if nn == 0 {
			pos = i + 1
			continue
		}
		j2 := j + nn
		for j2 < len(s) && isToolCallSpace(s[j2]) {
			j2++
		}
		if j2 >= len(s) || s[j2] != '>' {
			pos = i + 1
			continue
		}
		return i, tl, nm, true
	}
	return 0, 0, "", false
}

// dsmlArgsJSON 把 DSML 参数列表拼成 JSON 对象字符串（保序；无参为 {}）。
// string 参数编码为 JSON 字符串；string="false" 的原样值自身是合法 JSON 时直接内嵌
// （数字/布尔参数），否则按字符串兜底。
func dsmlArgsJSON(params []dsmlParam) string {
	if len(params) == 0 {
		return "{}"
	}
	var b strings.Builder
	b.WriteByte('{')
	for idx, p := range params {
		if idx > 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(p.name)
		b.Write(k)
		b.WriteByte(':')
		if p.raw && json.Valid([]byte(p.value)) {
			b.WriteString(p.value)
		} else {
			v, _ := json.Marshal(p.value)
			b.Write(v)
		}
	}
	b.WriteByte('}')
	return b.String()
}

// salvageDSML 流结束时的容错提取：先按严格语法成段消化（flush 只跑一次、partial
// 不再等待），剩下的 partial 段交给 salvageDSMLTruncatedInvoke 把已成形部分硬抠出来
// —— 客户端对坏参数会回「工具执行失败」的 tool_result，回合能重试；漏成正文是回合直接死。
// ok=false 表示开头不像 DSML，调用方按正文处理。
func salvageDSML(s string) (tcs []mcloudToolCall, consumed int, ok bool) {
	i := 0
	for i < len(s) {
		if st, block, c := tryParseDSMLBlock(s[i:]); st == "matched" {
			tcs = append(tcs, block...)
			i += c
			ok = true
			continue
		}
		if tc, c, found := salvageDSMLTruncatedInvoke(s[i:]); found {
			tcs = append(tcs, tc)
			i += c
			ok = true
			continue
		}
		break
	}
	return tcs, i, ok
}

// salvageDSMLTruncatedInvoke 从 s 开头提取一个没写完的 invoke（流已结束）。
// 只认「完整开 token + invoke + name」；参数 value 缺收尾 token 时取到串尾照转。
// 名字后面的属性表没写完 '>' 则放弃（无法可靠定界，宁漏勿错）。
func salvageDSMLTruncatedInvoke(s string) (tc mcloudToolCall, consumed int, ok bool) {
	i := 0
	for i < len(s) && isToolCallSpace(s[i]) {
		i++
	}
	tl, cl, pr := matchDSMLToken(s[i:])
	if pr || cl || tl == 0 {
		return mcloudToolCall{}, 0, false
	}
	i += tl
	for i < len(s) && isToolCallSpace(s[i]) {
		i++
	}
	nm, np, nn := scanDSMLElement(s[i:])
	if np || nm != "invoke" {
		return mcloudToolCall{}, 0, false
	}
	i += nn
	attrs, state, n := scanDSMLAttrs(s[i:])
	if state != "closed" {
		return mcloudToolCall{}, 0, false
	}
	i += n
	name := attrs["name"]
	if name == "" {
		return mcloudToolCall{}, 0, false
	}

	var params []dsmlParam
	for i < len(s) {
		for i < len(s) && isToolCallSpace(s[i]) {
			i++
		}
		if i >= len(s) {
			break
		}
		tl2, cl2, pr2 := matchDSMLToken(s[i:])
		if pr2 || tl2 == 0 {
			// 不成 token 的内容：跳到下一个 '<'（从 i+1 找，s[i] 可能就是 '<'）
			nx := strings.IndexByte(s[i+1:], '<')
			if nx < 0 {
				i = len(s)
			} else {
				i += 1 + nx
			}
			continue
		}
		i += tl2
		for i < len(s) && isToolCallSpace(s[i]) {
			i++
		}
		nm2, _, nn2 := scanDSMLElement(s[i:])
		i += nn2
		for i < len(s) && isToolCallSpace(s[i]) {
			i++
		}

		var pn string
		raw := false
		if !cl2 && nm2 == "parameter" {
			pattrs, st2, n3 := scanDSMLAttrs(s[i:])
			if st2 != "closed" {
				// parameter 标签没写完流就断：放弃该参数，invoke 到此收尾
				return mcloudToolCall{name: name, args: dsmlArgsJSON(params)}, len(s), true
			}
			i += n3
			pn = pattrs["name"]
			raw = pattrs["string"] == "false"
		} else {
			// 闭合 token 或非 parameter 开 token：吃掉 '>' 后收尾/继续
			if i < len(s) && s[i] == '>' {
				i++
			}
			if cl2 {
				return mcloudToolCall{name: name, args: dsmlArgsJSON(params)}, i, true
			}
			continue
		}

		// value：到「闭合 token + 元素 + >」或串尾
		end, tl3, elem3, found := findDSMLValueEnd(s, i)
		if !found {
			// 串尾截断：value 取剩余全部照转
			if pn != "" {
				params = append(params, dsmlParam{name: pn, value: s[i:], raw: raw})
			}
			return mcloudToolCall{name: name, args: dsmlArgsJSON(params)}, len(s), true
		}
		if pn != "" {
			params = append(params, dsmlParam{name: pn, value: s[i:end], raw: raw})
		}
		i = end + tl3
		for i < len(s) && isToolCallSpace(s[i]) {
			i++
		}
		_, _, nn3 := scanDSMLElement(s[i:])
		i += nn3
		for i < len(s) && isToolCallSpace(s[i]) {
			i++
		}
		if i < len(s) && s[i] == '>' {
			i++
		}
		if elem3 == "invoke" {
			return mcloudToolCall{name: name, args: dsmlArgsJSON(params)}, i, true
		}
	}
	return mcloudToolCall{name: name, args: dsmlArgsJSON(params)}, i, true
}

// nextBufCandidate 返回 s 中下一个「可能开启工具调用」的候选位置：
// 文本协议的 '['，或 DSML 的起始 '<'（其后跟全角｜首字节 0xEF、ASCII '|'、'/'，
// 或恰好位于串尾 —— 拿不准时先缓冲等下一 chunk）。找不到返回 -1。
// 普通 HTML（<div 等）不在此列，零缓冲直传。
func nextBufCandidate(s string) int {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '[':
			return i
		case '<':
			if i+1 >= len(s) {
				return i
			}
			switch s[i+1] {
			case 0xEF, '|', '/':
				return i
			}
		}
	}
	return -1
}
