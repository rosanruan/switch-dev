// wbprobe 是一次性排查工具（用完即删）：对照探测 WorkBuddy 新旧域名/请求头组合，
// 真实发送一条 "hi" 对话请求。不打印任何 token 值。
package main

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"strings"
	"time"
)

const infoPath = `C:\Users\xyb\AppData\Local\CodeBuddyExtension\Data\Public\auth\workbuddy-desktop-ai.info`

const cliVersion = "2.147.0"

var (
	token  string
	uid    string
	domain string
)

func chatBody(model string) string {
	b, _ := json.Marshal(map[string]interface{}{
		"model":      model,
		"messages":   []map[string]string{{"role": "user", "content": "hi"}},
		"max_tokens": 16,
		"stream":     true,
	})
	return string(b)
}

// probe 发 POST。keepUA=true 保留 Go 默认 UA（现有代理的指纹）；false 清空 UA（新版官方客户端不带）
func probe(label, url, body string, extra map[string]string, keepUA bool) {
	req, err := http.NewRequest("POST", url, strings.NewReader(body))
	if err != nil {
		fmt.Printf("[%s] ERR: %v\n", label, err)
		return
	}
	h := map[string]string{
		"Content-Type":     "application/json; charset=UTF-8",
		"Accept":           "application/json",
		"Authorization":    "Bearer " + token,
		"X-Requested-With": "XMLHttpRequest",
		"X-User-Id":        uid,
		"X-Private-Data":   "false",
	}
	for k, v := range extra {
		h[k] = v
	}
	for k, v := range h {
		req.Header.Set(k, v)
	}
	if !keepUA {
		req.Header.Set("User-Agent", "")
	}
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("[%s] ERR: %v\n", label, err)
		return
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
	snippet := string(data)
	if len(snippet) > 260 {
		snippet = snippet[:260]
	}
	fmt.Printf("[%s] HTTP %d: %q\n", label, resp.StatusCode, snippet)
}

// randHex32 生成 32 位小写字母数字，形状对齐官方 randomUUID().replaceAll("-","")
func randHex32() string {
	const letters = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 32)
	for i := range b {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(letters))))
		b[i] = letters[n.Int64()]
	}
	return string(b)
}

func main() {
	data, err := os.ReadFile(infoPath)
	if err != nil {
		fmt.Println("读 info 文件失败:", err)
		os.Exit(1)
	}
	// 先做结构内省：只打 key 和 JSON 类型，不打任何值
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		fmt.Println("解析顶层失败:", err)
		os.Exit(1)
	}
	fmt.Println("== 顶层 keys ==")
	for k, v := range top {
		fmt.Printf("  %s: %s\n", k, jsonKind(v))
	}
	if raw, ok := top["auth"]; ok {
		var auth map[string]json.RawMessage
		if err := json.Unmarshal(raw, &auth); err == nil {
			fmt.Println("== auth keys ==")
			for k, v := range auth {
				fmt.Printf("  %s: %s\n", k, jsonKind(v))
				if k == "accessToken" || k == "refreshToken" {
					// 打印对象内部的结构（仍然不打值，只打长度）
					dumpShape(k, v)
				}
			}
		}
	}
	for _, key := range []string{"account", "accounts", "allAccounts"} {
		raw, ok := top[key]
		if !ok {
			continue
		}
		fmt.Printf("== %s: %s ==\n", key, jsonKind(raw))
		var arr []map[string]json.RawMessage
		if err := json.Unmarshal(raw, &arr); err == nil {
			for i, e := range arr {
				fmt.Printf("  [%d] keys:\n", i)
				for k, v := range e {
					fmt.Printf("    .%s: %s\n", k, jsonKind(v))
				}
			}
			continue
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err == nil {
			for k, v := range obj {
				var s string
				if err := json.Unmarshal(v, &s); err == nil {
					fmt.Printf("  .%s = string(len=%d)\n", k, len(s))
				} else {
					fmt.Printf("  .%s: %s\n", k, jsonKind(v))
				}
			}
		}
	}
	os.Exit(0)
}

func jsonKind(raw json.RawMessage) string {
	for _, c := range raw {
		switch c {
		case ' ', '\t', '\n', '\r':
			continue
		case '{':
			return "object"
		case '[':
			return "array"
		case '"':
			return "string"
		case 't', 'f':
			return "bool"
		default:
			return "number"
		}
	}
	return "empty"
}

// dumpShape 对 accessToken/refreshToken 的值打形状：字符串给长度，对象给内部 key+类型
func dumpShape(name string, raw json.RawMessage) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		fmt.Printf("    %s = string(len=%d)\n", name, len(s))
		return
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err == nil {
		for k, v := range obj {
			var inner string
			if err := json.Unmarshal(v, &inner); err == nil {
				fmt.Printf("    .%s = string(len=%d)\n", k, len(inner))
			} else {
				fmt.Printf("    .%s = %s\n", k, jsonKind(v))
			}
		}
	}
}

func prefix(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
