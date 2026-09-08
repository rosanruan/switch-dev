package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"switchdev/creds"
)

// 编译期保证 WorkBuddy 实现了 StreamCaller —— 少了它，流式请求会静默退化成
// 「Call 聚合完整响应 + 伪流式」，首字延迟等于整段生成耗时。
var _ StreamCaller = (*WorkBuddyUpstream)(nil)

// wbGateway 包一层假网关：EnsureCreds 会先打 /plugin/auth/state 预校验，
// 这里统一应答 code:0（有效），其余路径交给 chat handler。
func wbGateway(chat http.HandlerFunc) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/plugin/auth/state") {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"code":0,"msg":"ok"}`)
			return
		}
		chat(w, r)
	}))
}

// newTestWorkBuddy 造一个指向假网关的 WorkBuddy 上游（凭据写临时文件，不碰真实环境）
func newTestWorkBuddy(t *testing.T, baseURL, uid string) *WorkBuddyUpstream {
	t.Helper()
	info := map[string]interface{}{
		"account": map[string]string{"uid": uid, "nickname": "tester"},
		"auth": map[string]interface{}{
			"accessToken":  "test-token",
			"refreshToken": "test-refresh",
			// 远未过期，避免 EnsureCreds 触发 refresh
			"expiresAt": time.Now().Add(24 * time.Hour).UnixMilli(),
		},
	}
	data, _ := json.Marshal(info)
	path := filepath.Join(t.TempDir(), "workbuddy-desktop.info")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatalf("写临时凭据: %v", err)
	}
	mgr := creds.NewWorkBuddyCredManager(creds.WorkBuddyConfig{
		BaseURL:          baseURL,
		InfoPath:         path,
		VerifyIntervalMs: 600000,
	})
	return NewWorkBuddyUpstream(mgr)
}

func sseChunk(content string) string {
	return fmt.Sprintf("data: {\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"content\":%q}}]}\n\n", content)
}

// firstBurst 模拟真实网关的开头：role 块 + 首个内容块。
// 各上游 doCallStream 都用 Peek(256) 做非 SSE/空流校验，而 Peek(n) 会阻塞到
// 攒够 n 字节，所以测试的第一段必须 >=256 字节，否则卡住的是 Peek 而非被测逻辑。
func firstBurst(content string) string {
	return `data: {"id":"chatcmpl-test","object":"chat.completion.chunk","created":1700000000,` +
		`"model":"glm-5.0","system_fingerprint":"fp_test","choices":[{"index":0,` +
		`"delta":{"role":"assistant","content":""},"logprobs":null,"finish_reason":null}]}` +
		"\n\n" + sseChunk(content)
}

// TestWorkBuddyCallStreamIsIncremental 本次修复的核心断言：
// 上游还没写完，客户端就应该能读到已经发出的部分。
// 服务端写完开头后阻塞，直到测试确认收到数据才写剩下的 —— 如果实现里
// 有任何「读全量再返回」的逻辑（比如误用 aggregateOpenAISSE），这里会超时。
func TestWorkBuddyCallStreamIsIncremental(t *testing.T) {
	releaseSecond := make(chan struct{})
	srv := wbGateway(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl := w.(http.Flusher)
		io.WriteString(w, firstBurst("第一块"))
		fl.Flush()
		<-releaseSecond // 卡住，模拟上游还在生成后续内容
		io.WriteString(w, sseChunk("第二块"))
		io.WriteString(w, "data: [DONE]\n\n")
		fl.Flush()
	})
	defer srv.Close()

	up := newTestWorkBuddy(t, srv.URL, "uid-1")
	sr, err := up.CallStream(context.Background(), []byte(`{"model":"glm-5.0","messages":[]}`))
	if err != nil {
		close(releaseSecond)
		t.Fatalf("CallStream: %v", err)
	}
	defer sr.Body.Close()
	if sr.StatusCode != 200 {
		close(releaseSecond)
		t.Fatalf("StatusCode = %d, want 200", sr.StatusCode)
	}

	// 后续内容还没发，这里必须能读到已发出的部分
	got := make(chan string, 1)
	go func() {
		buf := make([]byte, 1024)
		n, _ := sr.Body.Read(buf)
		got <- string(buf[:n])
	}()

	select {
	case s := <-got:
		if !contains(s, "第一块") {
			t.Errorf("首次 Read 未拿到已发出的内容: %q", s)
		}
		t.Logf("上游未写完即读到 %d 字节", len(s))
	case <-time.After(3 * time.Second):
		close(releaseSecond)
		t.Fatal("上游只发了开头就读不到数据 —— 说明实现在等整个流结束（伪流式回退）")
	}

	close(releaseSecond)
	rest, _ := io.ReadAll(sr.Body)
	if !contains(string(rest), "第二块") {
		t.Errorf("后续内容缺失: %q", rest)
	}
}

// TestWorkBuddyCallStreamRequestShape 出站 body 与请求头必须与非流式一致
// （那批 delete 和风控头是对齐官方 CLI 的，漏一项会被判非官方客户端）
func TestWorkBuddyCallStreamRequestShape(t *testing.T) {
	var gotBody map[string]interface{}
	var gotHeader http.Header
	srv := wbGateway(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &gotBody)
		gotHeader = r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, firstBurst("hi")+"data: [DONE]\n\n")
	})
	defer srv.Close()

	up := newTestWorkBuddy(t, srv.URL, "uid-42")
	// 故意带上 WorkBuddy 不认的字段 + max_tokens=0
	req := `{"model":"glm-5.0","messages":[],"max_tokens":0,"stream":false,
	         "stream_options":{"include_usage":true},"tenant":"t","orgFullName":"o",
	         "userId":"u","client":"c","clientVersion":"v","language":"zh"}`
	sr, err := up.CallStream(context.Background(), []byte(req))
	if err != nil {
		t.Fatalf("CallStream: %v", err)
	}
	io.ReadAll(sr.Body)
	sr.Body.Close()

	if gotBody["stream"] != true {
		t.Errorf("stream = %v, want true（上游非流式返回 11101）", gotBody["stream"])
	}
	// stream_options 会触发 11140，是唯一不能注入的上游
	if _, present := gotBody["stream_options"]; present {
		t.Error("stream_options 未被删除，WorkBuddy 网关会返回 11140")
	}
	for _, k := range []string{"tenant", "orgFullName", "userId", "client", "clientVersion", "language"} {
		if _, present := gotBody[k]; present {
			t.Errorf("字段 %s 未被删除，WorkBuddy 不认", k)
		}
	}
	if mt, _ := gotBody["max_tokens"].(float64); mt != 1024 {
		t.Errorf("max_tokens = %v, want 1024（0 值兜底）", gotBody["max_tokens"])
	}

	if gotHeader.Get("X-Requested-With") != "XMLHttpRequest" {
		t.Error("缺 X-Requested-With，会被风控识别为非官方客户端")
	}
	if gotHeader.Get("Authorization") != "Bearer test-token" {
		t.Errorf("Authorization = %q", gotHeader.Get("Authorization"))
	}
	if gotHeader.Get("X-User-Id") != "uid-42" {
		t.Errorf("X-User-Id = %q, want uid-42", gotHeader.Get("X-User-Id"))
	}
	// 有 uid 时 X-Userinfo 必须成对出现
	if gotHeader.Get("X-Userinfo") == "" {
		t.Error("有 uid 时缺 X-Userinfo，缺一会被风控拦")
	}
}

// TestWorkBuddyCallStreamDegrades 空流 / 非 SSE 内容返回虚拟 502，让 router 降级到链里下一个
func TestWorkBuddyCallStreamDegrades(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"空流", ""},
		{"非 SSE（JSON 错误体）", `{"code":11101,"message":"stream required"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := wbGateway(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(200)
				io.WriteString(w, c.body)
			})
			defer srv.Close()

			up := newTestWorkBuddy(t, srv.URL, "uid-1")
			sr, err := up.CallStream(context.Background(), []byte(`{"messages":[]}`))
			if err != nil {
				t.Fatalf("CallStream: %v", err)
			}
			defer sr.Body.Close()
			if sr.StatusCode != 502 {
				t.Errorf("StatusCode = %d, want 502（虚拟 502 触发降级）", sr.StatusCode)
			}
		})
	}
}

// TestWorkBuddyCallStreamNon200PassesThrough 非 200 原样带错误体返回，交给 router 归类
func TestWorkBuddyCallStreamNon200PassesThrough(t *testing.T) {
	srv := wbGateway(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		io.WriteString(w, `{"code":429,"message":"rate limited"}`)
	})
	defer srv.Close()

	up := newTestWorkBuddy(t, srv.URL, "uid-1")
	sr, err := up.CallStream(context.Background(), []byte(`{"messages":[]}`))
	if err != nil {
		t.Fatalf("CallStream: %v", err)
	}
	defer sr.Body.Close()
	if sr.StatusCode != 429 {
		t.Errorf("StatusCode = %d, want 429", sr.StatusCode)
	}
	body, _ := io.ReadAll(sr.Body)
	if !contains(string(body), "rate limited") {
		t.Errorf("错误体丢失: %q", body)
	}
}

// TestWorkBuddyCallStreamRetriesOn401And403 401 与 403 都应触发一次凭据刷新重试
// （非流式 isWorkBuddyTokenInvalid 判的是 401||403，别只判 401）
func TestWorkBuddyCallStreamRetriesOn401And403(t *testing.T) {
	for _, code := range []int{401, 403} {
		t.Run(fmt.Sprintf("%d", code), func(t *testing.T) {
			var calls int32
			srv := wbGateway(func(w http.ResponseWriter, r *http.Request) {
				if atomic.AddInt32(&calls, 1) == 1 {
					w.WriteHeader(code)
					io.WriteString(w, `{"message":"token expired"}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, firstBurst("retried")+"data: [DONE]\n\n")
			})
			defer srv.Close()

			up := newTestWorkBuddy(t, srv.URL, "uid-1")
			sr, err := up.CallStream(context.Background(), []byte(`{"messages":[]}`))
			if err != nil {
				t.Fatalf("CallStream: %v", err)
			}
			defer sr.Body.Close()

			if n := atomic.LoadInt32(&calls); n != 2 {
				t.Fatalf("上游被调用 %d 次，want 2（%d 应触发一次重试）", n, code)
			}
			if sr.StatusCode != 200 {
				t.Errorf("重试后 StatusCode = %d, want 200", sr.StatusCode)
			}
			body, _ := io.ReadAll(sr.Body)
			if !contains(string(body), "retried") {
				t.Errorf("重试后内容不对: %q", body)
			}
		})
	}
}

// TestWorkBuddyCallAndCallStreamSameOutboundBody 两条路径的出站 body 必须一致
// （共用 buildChatRequest，避免风控字段只在一条路径上被维护）
func TestWorkBuddyCallAndCallStreamSameOutboundBody(t *testing.T) {
	var bodies []string
	srv := wbGateway(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(raw))
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, firstBurst("ok")+"data: [DONE]\n\n")
	})
	defer srv.Close()

	up := newTestWorkBuddy(t, srv.URL, "uid-1")
	req := []byte(`{"model":"glm-5.0","messages":[],"tenant":"t","stream_options":{"include_usage":true}}`)

	if _, err := up.Call(context.Background(), req); err != nil {
		t.Fatalf("Call: %v", err)
	}
	sr, err := up.CallStream(context.Background(), req)
	if err != nil {
		t.Fatalf("CallStream: %v", err)
	}
	io.ReadAll(sr.Body)
	sr.Body.Close()

	if len(bodies) != 2 {
		t.Fatalf("上游收到 %d 个请求，want 2", len(bodies))
	}
	if bodies[0] != bodies[1] {
		t.Errorf("两条路径出站 body 不一致：\n非流式: %s\n流式:   %s", bodies[0], bodies[1])
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
