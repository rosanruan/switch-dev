package upstream

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"switchdev/creds"
)

// TestJoyCodeStreamProbe 探测 JoyCode 流式 SSE 格式（不接入主流程，仅抓包用）
//
// 运行：go test -run TestJoyCodeStreamProbe -v ./upstream/
//
// 会打印每个模型的 HTTP status、Content-Type、响应前 2KB，用于确认：
//  1. SSE 格式是标准 OpenAI（data:{...}\n\n）还是 Color 信封包装（{code,data}）
//  2. 哪些模型被灰度拒绝返回 406
//  3. 流式路由是 Accept:text/event-stream 触发还是 body stream:true 触发
func TestJoyCodeStreamProbe(t *testing.T) {
	mgr := creds.NewJoyCodeCredManager(creds.DefaultJoyCodeConfig())
	cred, err := mgr.EnsureCreds()
	if err != nil {
		t.Skipf("JoyCode 凭据不可用，跳过探测: %v", err)
	}
	t.Logf("JoyCode 凭据已加载，Origin=%s，userId=%s", cred.Origin, cred.UserID)

	// 覆盖测试模型：含 Stream:true 和 Stream:false 的
	models := []string{
		"JoyAI-Code-1.5",     // Stream:true
		"GLM-5.1-agent",      // Stream:true
		"MiniMax-M2.7-agent", // Stream:false（预期 406 或非流式）
	}

	for _, model := range models {
		t.Run(model, func(t *testing.T) {
			probeJoyCodeStream(t, cred, model, "text/event-stream")
		})
	}

	// 额外测一组：Accept:application/json + stream:true，看路由是否由 Accept 决定
	t.Run("JoyAI-Code-1.5_accept-json", func(t *testing.T) {
		probeJoyCodeStream(t, cred, "JoyAI-Code-1.5", "application/json")
	})
}

func probeJoyCodeStream(t *testing.T, cred *creds.JoyCodeCred, model, accept string) {
	// 构造基础 body
	body := map[string]interface{}{
		"model": model,
		"messages": []map[string]string{
			{"role": "user", "content": "用一句话介绍你自己"},
		},
		"max_tokens": 50,
	}
	bodyBytes, _ := json.Marshal(body)
	// 注入 JoyCode 业务字段（会设 stream:false）
	bodyBytes = injectJoyCodeFields(bodyBytes, cred)
	// 覆盖 stream:true
	var m map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &m); err != nil {
		t.Fatalf("解析 body 失败: %v", err)
		return
	}
	m["stream"] = true
	bodyBytes, _ = json.Marshal(m)

	// 签名 + URL
	tm := time.Now().UnixMilli()
	sign := creds.ColorSign("joycode_ide", "chat_completions", tm, nil)
	url := fmt.Sprintf("%s/api?appid=joycode_ide&functionId=chat_completions&t=%d&sign=%s",
		cred.Origin, tm, sign)

	req, err := http.NewRequestWithContext(context.Background(), "POST", url, bytes.NewReader(bodyBytes))
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json; charset=UTF-8")
	req.Header.Set("x-ms-client-request-id", fmt.Sprintf("probe-%d", tm))
	req.Header.Set("ptKey", cred.PtKey)
	req.Header.Set("loginType", cred.LoginType)
	req.Header.Set("Accept", accept)
	req.Header.Set("Accept-Encoding", "gzip")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Errorf("请求失败: %v", err)
		return
	}
	defer resp.Body.Close()

	// gzip 解压
	var reader io.Reader = resp.Body
	if resp.Header.Get("Content-Encoding") == "gzip" {
		gr, err := gzip.NewReader(resp.Body)
		if err == nil {
			reader = gr
			defer gr.Close()
		}
	}

	// 读前 2KB
	buf := make([]byte, 2048)
	n, _ := reader.Read(buf)
	snippet := string(buf[:n])

	t.Logf("【Accept=%s】模型=%s status=%d Content-Type=%s", accept, model, resp.StatusCode, resp.Header.Get("Content-Type"))
	t.Logf("响应前 %d 字节:\n%s", n, snippet)
}
