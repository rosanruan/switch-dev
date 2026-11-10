package upstream

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"switchdev/creds"
)

// 编译期保证 MCloud 实现了 StreamCaller —— 少了它，流式请求会静默退化成
// 「Call 聚合完整响应 + 伪流式」，首字延迟等于整段生成耗时。
var _ StreamCaller = (*MCloudUpstream)(nil)

// newTestMCloud 造一个指向假网关的 mcloud 上游：
// accountdb.db 建在临时目录（t_account 表 + 一条未过期凭据），不碰真实环境。
func newTestMCloud(t *testing.T, baseURL string) *MCloudUpstream {
	t.Helper()
	path := filepath.Join(t.TempDir(), "accountdb.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("建临时 accountdb: %v", err)
	}
	_, err = db.Exec(`CREATE TABLE t_account (
		account TEXT, token TEXT, userDomainId TEXT,
		tokenGetTime INTEGER, tokenValidTime INTEGER)`)
	if err != nil {
		t.Fatalf("建 t_account: %v", err)
	}
	_, err = db.Exec(`INSERT INTO t_account VALUES (?, ?, ?, ?, ?)`,
		"13800000000", "rcs-token-xxxx", "uid-12345",
		time.Now().UnixMilli(), 2592000)
	if err != nil {
		t.Fatalf("插凭据: %v", err)
	}
	db.Close()

	mgr := creds.NewMCloudCredManager(creds.MCloudConfig{BaseURL: baseURL, DBPath: path})
	return NewMCloudUpstream(mgr)
}

// mcFrame 拼一帧灵犀 SSE（event:AI-chat-sse-simple 格式）
func mcFrame(payload string) string {
	return "id:1\nretry:3000\nevent:AI-chat-sse-simple\ndata:" + payload + "\n\n"
}

// mcDataFrame 拼一帧正常输出的 data JSON
func mcDataFrame(modelType, out, reasoning string, finish bool) string {
	fr := map[string]interface{}{
		"index":      0,
		"resultType": 1,
		"outContent": out,
		"modelType":  modelType,
	}
	if reasoning != "" {
		fr["reasoningContent"] = reasoning
	}
	if finish {
		fr["finishReason"] = "stop"
	}
	payload, _ := json.Marshal(map[string]interface{}{
		"success": true,
		"code":    "0000",
		"data": map[string]interface{}{
			"sessionId":    "s-1",
			"flowResult":   fr,
			"finishReason": map[bool]string{true: "stop", false: ""}[finish],
		},
	})
	return mcFrame(string(payload))
}

func mcErrFrame(code, msg string) string {
	payload, _ := json.Marshal(map[string]interface{}{
		"success": false,
		"code":    code,
		"message": msg,
	})
	return mcFrame(string(payload))
}

// mcChatBody 造 OpenAI 格式请求体
func mcChatBody(model string, msgs ...string) []byte {
	type msg struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	ms := make([]msg, len(msgs))
	for i, m := range msgs {
		parts := strings.SplitN(m, ":", 2)
		ms[i] = msg{Role: parts[0], Content: parts[1]}
	}
	b, _ := json.Marshal(map[string]interface{}{"model": model, "messages": ms})
	return b
}

// TestMCloudFoldDialogue 折叠：system 前置、User/Assistant/Tool 标签、超限截断
func TestMCloudFoldDialogue(t *testing.T) {
	// 基本折叠
	d, m, err := foldMCloudDialogue(mcChatBody("blian_qwen38_max",
		"system:你是助手", "user:你好", "assistant:你好！", "user:继续"))
	if err != nil {
		t.Fatalf("fold: %v", err)
	}
	if m != "blian_qwen38_max" {
		t.Errorf("modelType = %q", m)
	}
	want := "你是助手\n\nUser: 你好\n\nAssistant: 你好！\n\nUser: 继续"
	if d != want {
		t.Errorf("dialogue = %q, want %q", d, want)
	}

	// 缺 model
	if _, _, err := foldMCloudDialogue([]byte(`{"messages":[]}`)); err == nil {
		t.Error("缺 model 应报错")
	}

	// 超限截断：jiutian 上限 9000 字符，3 轮超长 user 各 4000 字符 → 只保最新
	long := strings.Repeat("长", 4000)
	d, _, err = foldMCloudDialogue(mcChatBody("jiutian",
		"user:"+long, "assistant:"+long, "user:"+long))
	if err != nil {
		t.Fatalf("fold 超限: %v", err)
	}
	if !strings.Contains(d, "（早期对话因超过模型输入上限已截断）") {
		t.Error("截断后应包含截断标记")
	}
	// 保住的是最新一轮
	if !strings.Contains(d, long) {
		t.Error("最新一轮应完整保留")
	}
	if strings.Count(d, "长") > 9000 {
		t.Errorf("折叠后长度 %d 超过 jiutian 上限", strings.Count(d, "长"))
	}
	// 最早一轮应被丢弃
	if strings.Count(d, "User: "+long) != 1 {
		t.Errorf("超长轮次应只剩最新一轮，实际 %d 轮", strings.Count(d, "User: "+long))
	}
}

// TestMCloudCallStreamConvertsSSE 流式：上游自有 SSE → OpenAI SSE，含 finish + [DONE]
func TestMCloudCallStreamConvertsSSE(t *testing.T) {
	var setCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/assistant/chat/config"):
			atomic.AddInt32(&setCalls, 1)
			io.WriteString(w, `{"success":true,"code":"0000"}`)
		case strings.HasSuffix(r.URL.Path, "/assistant/chat/v2/add"):
			w.Header().Set("Content-Type", "text/event-stream")
			fl := w.(http.Flusher)
			io.WriteString(w, mcDataFrame("blian_qwen38_max", "", "思考中", false))
			fl.Flush()
			io.WriteString(w, mcDataFrame("blian_qwen38_max", "你好", "", false))
			io.WriteString(w, mcDataFrame("blian_qwen38_max", "世界", "", true))
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
	for _, want := range []string{`"role":"assistant"`, `"reasoning_content":"思考中"`, `"content":"你好"`, `"content":"世界"`, `"finish_reason":"stop"`, "data: [DONE]"} {
		if !strings.Contains(s, want) {
			t.Errorf("流输出缺 %q:\n%s", want, s)
		}
	}
	if setCalls != 1 {
		t.Errorf("config/set 调用 %d 次，want 1", setCalls)
	}
}

// TestMCloudSetIfChanged 同模型连发不重复 set；换模型重新 set
func TestMCloudSetIfChanged(t *testing.T) {
	var setCalls int32
	chat := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, mcDataFrame("blian_qwen38_max", "ok", "", true))
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/assistant/chat/config") {
			atomic.AddInt32(&setCalls, 1)
			io.WriteString(w, `{"success":true,"code":"0000"}`)
			return
		}
		chat(w, r)
	}))
	defer srv.Close()

	up := newTestMCloud(t, srv.URL)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		sr, err := up.CallStream(ctx, mcChatBody("blian_qwen38_max", "user:hi"))
		if err != nil {
			t.Fatalf("第 %d 次 CallStream: %v", i+1, err)
		}
		io.Copy(io.Discard, sr.Body)
		sr.Body.Close()
	}
	if got := atomic.LoadInt32(&setCalls); got != 1 {
		t.Errorf("同模型连发后 config/set 调用 %d 次，want 1", got)
	}
}

// TestMCloudAuthRetry 401 过渡态：invalidate → 重读 DB → 重试成功
func TestMCloudAuthRetry(t *testing.T) {
	var chatCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/assistant/chat/config") {
			io.WriteString(w, `{"success":true,"code":"0000"}`)
			return
		}
		if atomic.AddInt32(&chatCalls, 1) == 1 {
			w.WriteHeader(401)
			io.WriteString(w, "unauthorized")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, mcDataFrame("blian_qwen38_max", "ok", "", true))
	}))
	defer srv.Close()

	up := newTestMCloud(t, srv.URL)
	sr, err := up.CallStream(context.Background(), mcChatBody("blian_qwen38_max", "user:hi"))
	if err != nil {
		t.Fatalf("CallStream 应在 401 后重试成功: %v", err)
	}
	defer sr.Body.Close()
	if chatCalls != 2 {
		t.Errorf("chat 调用 %d 次，want 2（重试一次）", chatCalls)
	}
}

// TestMCloudAuthRetrySSEFrame 01000002 业务码帧同样触发鉴权重试
func TestMCloudAuthRetrySSEFrame(t *testing.T) {
	var chatCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/assistant/chat/config") {
			io.WriteString(w, `{"success":true,"code":"0000"}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if atomic.AddInt32(&chatCalls, 1) == 1 {
			io.WriteString(w, mcErrFrame("01000002", "登录态切换中"))
			return
		}
		io.WriteString(w, mcDataFrame("blian_qwen38_max", "ok", "", true))
	}))
	defer srv.Close()

	up := newTestMCloud(t, srv.URL)
	sr, err := up.CallStream(context.Background(), mcChatBody("blian_qwen38_max", "user:hi"))
	if err != nil {
		t.Fatalf("CallStream 应在 01000002 后重试成功: %v", err)
	}
	defer sr.Body.Close()
	if chatCalls != 2 {
		t.Errorf("chat 调用 %d 次，want 2", chatCalls)
	}
}

// TestMCloudModelMismatchRetry 首帧模型不一致 → 重置 currentModel → 重新 set → 重试
func TestMCloudModelMismatchRetry(t *testing.T) {
	var chatCalls, setCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/assistant/chat/config") {
			atomic.AddInt32(&setCalls, 1)
			io.WriteString(w, `{"success":true,"code":"0000"}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if atomic.AddInt32(&chatCalls, 1) == 1 {
			// 服务端用旧偏好模型响应（模拟用户在 App 里切过模型）
			io.WriteString(w, mcDataFrame("huoshan_glm_5_3", "旧模型响应", "", true))
			return
		}
		io.WriteString(w, mcDataFrame("blian_qwen38_max", "正确模型", "", true))
	}))
	defer srv.Close()

	up := newTestMCloud(t, srv.URL)
	// 预热：建立 currentModel = blian_qwen38_max
	if err := up.EnsureCreds(context.Background()); err != nil {
		t.Fatalf("EnsureCreds: %v", err)
	}
	sr, err := up.CallStream(context.Background(), mcChatBody("blian_qwen38_max", "user:hi"))
	if err != nil {
		t.Fatalf("CallStream 应在模型不一致后重试成功: %v", err)
	}
	defer sr.Body.Close()
	if chatCalls != 2 {
		t.Errorf("chat 调用 %d 次，want 2", chatCalls)
	}
	if setCalls != 2 {
		t.Errorf("config/set 调用 %d 次，want 2（mismatch 后强制重 set）", setCalls)
	}
}

// TestMCloudCallAggregates 非流式：SSE 聚合为完整 OpenAIResponse JSON
func TestMCloudCallAggregates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/assistant/chat/config") {
			io.WriteString(w, `{"success":true,"code":"0000"}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, mcDataFrame("blian_qwen38_max", "", "推理", false))
		io.WriteString(w, mcDataFrame("blian_qwen38_max", "你好", "", false))
		io.WriteString(w, mcDataFrame("blian_qwen38_max", "世界", "", true))
	}))
	defer srv.Close()

	up := newTestMCloud(t, srv.URL)
	resp, err := up.Call(context.Background(), mcChatBody("blian_qwen38_max", "user:hi"))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("StatusCode = %d", resp.StatusCode)
	}
	var got struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Role             string `json:"role"`
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(resp.Body, &got); err != nil {
		t.Fatalf("解析聚合响应: %v\n%s", err, resp.Body)
	}
	if len(got.Choices) != 1 || got.Choices[0].Message.Content != "你好世界" {
		t.Errorf("聚合 content = %+v", got.Choices)
	}
	if got.Choices[0].Message.ReasoningContent != "推理" {
		t.Errorf("聚合 reasoning = %q", got.Choices[0].Message.ReasoningContent)
	}
	if got.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %q", got.Choices[0].FinishReason)
	}
}

// TestMCloudFetchModels 模型列表：返回裸 modelType（mc/ 前缀由 MergeModels 加）
func TestMCloudFetchModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/assistant/chat/config/get") {
			http.NotFound(w, r)
			return
		}
		// 校验请求带全 body（userId 等）；空 body 只返回兜底单模型
		var body map[string]interface{}
		json.NewDecoder(r.Body).Decode(&body)
		if body["userId"] != "uid-12345" {
			t.Errorf("config/get 请求缺 userId: %v", body)
		}
		io.WriteString(w, `{"success":true,"code":"0000","data":{"modelList":[
			{"modelType":"blian_qwen38_max","description":"千问38旗舰","maxInputLength":800000,"enableSetThinking":true},
			{"modelType":"jiutian","savePath":"九天","maxInputLength":9000}
		]}}`)
	}))
	defer srv.Close()

	up := newTestMCloud(t, srv.URL)
	models, err := up.FetchModels(context.Background())
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("模型数 = %d, want 2", len(models))
	}
	if models[0].ID != "blian_qwen38_max" {
		t.Errorf("id 应为裸 modelType，got %q", models[0].ID)
	}
	if models[0].Label != "千问38旗舰" || !models[0].Reasoning {
		t.Errorf("meta = %+v", models[0])
	}
	if models[1].Label != "九天" {
		t.Errorf("savePath 兜底 label = %q", models[1].Label)
	}
}

// TestMCloudVerifyCreds 凭据验证：200+0000 有效
func TestMCloudVerifyCreds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"success":true,"code":"0000","data":{"modelList":[]}}`)
	}))
	defer srv.Close()

	up := newTestMCloud(t, srv.URL)
	if err := up.EnsureCreds(context.Background()); err != nil {
		t.Fatalf("EnsureCreds: %v", err)
	}
	res, err := up.VerifyCreds(context.Background())
	if err != nil || !res.Valid {
		t.Errorf("VerifyCreds = %+v, err=%v；want valid", res, err)
	}
}

// TestMCloudBasicAuthFormat Basic 头格式：Basic base64("mobile:手机号:token")
func TestMCloudBasicAuthFormat(t *testing.T) {
	cred := &creds.MCloudCred{Phone: "13800000000", RCSToken: "tok"}
	got := creds.MCloudBasicAuth(cred)
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("mobile:13800000000:tok"))
	if got != want {
		t.Errorf("Basic 头 = %q, want %q", got, want)
	}
}

// TestMCloudHeaders 必备头族齐全（漏头会被风控识别为非官方客户端）
func TestMCloudHeaders(t *testing.T) {
	h := mcloudHeaders("Basic x", "v1")
	for _, k := range []string{"Authorization", "Content-Type", "x-yun-api-version", "x-yun-app-channel", "x-yun-client-info", "x-yun-tid"} {
		if h[k] == "" {
			t.Errorf("缺头 %s", k)
		}
	}
	if h["x-yun-app-channel"] != mcloudSourceChannel {
		t.Errorf("channel = %q", h["x-yun-app-channel"])
	}
}

// TestMCloudCredManager 过期 token：EnsureCreds 报含「打开 App」指引的错误
func TestMCloudCredManager(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "accountdb.db")
	db, _ := sql.Open("sqlite", path)
	db.Exec(`CREATE TABLE t_account (account TEXT, token TEXT, userDomainId TEXT, tokenGetTime INTEGER, tokenValidTime INTEGER)`)
	// 已过期：getime 在 40 天前，有效期 30 天
	db.Exec(`INSERT INTO t_account VALUES (?, ?, ?, ?, ?)`,
		"13800000000", "tok", "uid", time.Now().Add(-40*24*time.Hour).UnixMilli(), 2592000)
	db.Close()

	mgr := creds.NewMCloudCredManager(creds.MCloudConfig{BaseURL: "http://x", DBPath: path})
	_, err := mgr.EnsureCreds()
	if err == nil {
		t.Fatal("过期 token 应报错")
	}
	if !strings.Contains(err.Error(), "打开中国移动云盘 App") {
		t.Errorf("错误应含续期指引: %v", err)
	}

	// 文件不存在
	mgr2 := creds.NewMCloudCredManager(creds.MCloudConfig{BaseURL: "http://x", DBPath: filepath.Join(dir, "none.db")})
	if _, err := mgr2.EnsureCreds(); err == nil {
		t.Error("缺 accountdb.db 应报错")
	}
}

// TestMCloudUsageEstimated 灵犀不返回 usage：流式末帧与非流式聚合都必须带
// 字符估算的 token（否则日志列表输入/输出全空、计费为 0）。
// dialogue="User: hi"（8 runes → 2 token），输出"你好世界"（4 runes → 1 token）。
func TestMCloudUsageEstimated(t *testing.T) {
	newGateway := func() *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/assistant/chat/config") {
				io.WriteString(w, `{"success":true,"code":"0000"}`)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, mcDataFrame("blian_qwen38_max", "你好世界", "", true))
		}))
	}

	// 流式：末帧 usage
	srv := newGateway()
	defer srv.Close()
	up := newTestMCloud(t, srv.URL)
	sr, err := up.CallStream(context.Background(), mcChatBody("mc/blian_qwen38_max", "user:hi"))
	if err != nil {
		t.Fatalf("CallStream: %v", err)
	}
	all, _ := io.ReadAll(sr.Body)
	sr.Body.Close()
	if !strings.Contains(string(all), `"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}`) {
		t.Errorf("流式末帧缺估算 usage: %s", all)
	}

	// 非流式：聚合响应 usage
	up2 := newTestMCloud(t, srv.URL)
	resp, err := up2.Call(context.Background(), mcChatBody("mc/blian_qwen38_max", "user:hi"))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	var got struct {
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(resp.Body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Usage == nil || got.Usage.PromptTokens != 2 || got.Usage.CompletionTokens != 1 {
		t.Errorf("非流式聚合 usage = %+v, want prompt=2 completion=1", got.Usage)
	}
}

// TestMCloudStripsMcPrefix 回归：内部路由 id（mc/blian_qwen38_max）必须在发上游前
// 剥成裸 modelType —— 否则 config/set 收到 mc/... 被上游判 01000001 参数错误。
func TestMCloudStripsMcPrefix(t *testing.T) {
	var setModelType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/assistant/chat/config") {
			var body map[string]interface{}
			json.NewDecoder(r.Body).Decode(&body)
			setModelType, _ = body["modelType"].(string)
			io.WriteString(w, `{"success":true,"code":"0000"}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, mcDataFrame("blian_qwen38_max", "ok", "", true))
	}))
	defer srv.Close()

	up := newTestMCloud(t, srv.URL)
	sr, err := up.CallStream(context.Background(), mcChatBody("mc/blian_qwen38_max", "user:hi"))
	if err != nil {
		t.Fatalf("CallStream: %v", err)
	}
	defer sr.Body.Close()
	if setModelType != "blian_qwen38_max" {
		t.Errorf("config/set modelType = %q, want 裸名 blian_qwen38_max", setModelType)
	}
	// 模型一致性校验也按裸名比对，不应误判 mismatch 重试
	all, _ := io.ReadAll(sr.Body)
	if !strings.Contains(string(all), `"content":"ok"`) {
		t.Errorf("流输出异常: %s", all)
	}
}
