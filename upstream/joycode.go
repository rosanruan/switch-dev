package upstream

import (
	"bufio"
	"bytes"
	"context"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"switchdev/creds"
)

// JoyCodeUpstream JoyCode Color 网关适配器
type JoyCodeUpstream struct {
	mgr          *creds.JoyCodeCredManager
	client       *http.Client
	streamClient *http.Client // 流式专用：无整体 Timeout，由 context 控制超时
}

func NewJoyCodeUpstream(mgr *creds.JoyCodeCredManager) *JoyCodeUpstream {
	return &JoyCodeUpstream{
		mgr:    mgr,
		client: &http.Client{
			Timeout: 120 * time.Second, // 非流式：模型推理可能慢
		},
		streamClient: &http.Client{}, // 无 Timeout：流式可能持续数分钟，由 context 控制
	}
}

func (u *JoyCodeUpstream) Name() string { return "joycode" }

func (u *JoyCodeUpstream) EnsureCreds(ctx context.Context) error {
	_, err := u.mgr.EnsureCreds()
	return err
}

func (u *JoyCodeUpstream) InvalidateCreds() { u.mgr.InvalidateCreds() }

func (u *JoyCodeUpstream) VerifyCreds(ctx context.Context) (*VerifyResult, error) {
	cred := u.mgr.GetCred()
	if cred == nil {
		c, err := u.mgr.LoadCredsFromVscdb()
		if err != nil {
			return &VerifyResult{Valid: false, Code: -1}, err
		}
		cred = c
	}
	valid, code, err := u.mgr.VerifyCreds(cred)
	if err != nil {
		return &VerifyResult{Valid: false, Code: -1}, err
	}
	return &VerifyResult{Valid: valid, Code: code}, nil
}

func (u *JoyCodeUpstream) CredStatus() *creds.CredStatusInfo {
	return u.mgr.CredStatus()
}

// HasValidCreds 凭据是否可用
// 宽松判定：凭据已加载（ptKey 非空）就返回 true，让 Call 内部的重读+重试逻辑处理失效
// 仅当凭据未加载时才返回 false
func (u *JoyCodeUpstream) HasValidCreds() bool {
	cred := u.mgr.GetCred()
	return cred != nil && cred.PtKey != ""
}

// FetchModels 调 joycode_modelList 拉取可用模型列表
func (u *JoyCodeUpstream) FetchModels(ctx context.Context) ([]FetchedModel, error) {
	cred, err := u.mgr.EnsureCreds()
	if err != nil {
		return nil, err
	}
	t := time.Now().UnixMilli()
	sign := creds.ColorSign("joycode_ide", "joycode_modelList", t, nil)
	url := fmt.Sprintf("%s/api?appid=joycode_ide&functionId=joycode_modelList&t=%d&sign=%s",
		cred.Origin, t, sign)

	body, _, err := httpPost(url, "{}", map[string]string{
		"Content-Type": "application/json; charset=UTF-8",
		"ptKey":        cred.PtKey,
		"loginType":    cred.LoginType,
		"Accept":       "application/json",
	})
	if err != nil {
		return nil, err
	}

	var resp struct {
		Code int `json:"code"`
		Data []struct {
			ChatApiModel   string   `json:"chatApiModel"`
			Label          string   `json:"label"`
			Features       []string `json:"features"`
			RespMaxTokens  int      `json:"respMaxTokens"`
			MaxTotalTokens int      `json:"maxTotalTokens"`
			SupportStream  bool     `json:"supportStream"`
		} `json:"data"`
	}
	if err := creds.ParseJSONPublic(body, &resp); err != nil {
		return nil, err
	}
	if resp.Code != 0 {
		return nil, fmt.Errorf("joycode_modelList 返回 code=%d", resp.Code)
	}

	result := make([]FetchedModel, 0, len(resp.Data))
	for _, m := range resp.Data {
		if m.ChatApiModel == "" {
			continue
		}
		fm := FetchedModel{
			ID:       m.ChatApiModel,
			Label:    m.Label,
			Output:   m.RespMaxTokens,
			Context:  m.MaxTotalTokens,
			Stream:   m.SupportStream,
			ToolCall: hasFeature(m.Features, "function_call"),
			Vision:   hasFeature(m.Features, "vision"),
			Reasoning: hasFeature(m.Features, "agent"),
		}
		if fm.Label == "" {
			fm.Label = m.ChatApiModel
		}
		result = append(result, fm)
	}
	return result, nil
}

// hasFeature 检查 features 列表是否包含某特性
func hasFeature(features []string, feat string) bool {
	for _, f := range features {
		if f == feat {
			return true
		}
	}
	return false
}

// Call 调用 JoyCode Color 网关（带 401 自动重试）
func (u *JoyCodeUpstream) Call(ctx context.Context, body []byte) (*Response, error) {
	resp, err := u.callWithRetry(ctx, body)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (u *JoyCodeUpstream) callWithRetry(ctx context.Context, body []byte) (*Response, error) {
	cred, err := u.mgr.EnsureCreds()
	if err != nil {
		return nil, err
	}

	// 注入 JoyCode 业务字段（tenant/userId/client/clientVersion/language）
	injectedBody := injectJoyCodeFields(body, cred)
	resp, err := u.doCall(ctx, injectedBody, cred)
	if err != nil {
		return nil, err
	}

	// pt_key 失效 -> 重读 vscdb 并重试一次
	if isPtKeyInvalid(resp) {
		fmt.Println("[switch-dev] JoyCode 收到 401（pt_key 失效），重读 state.vscdb 并重试一次")
		u.mgr.InvalidateCreds()
		oldKey := cred.PtKey
		newCred, err := u.mgr.EnsureCreds()
		if err != nil {
			return resp, nil // 返回原 401 响应，由上层处理
		}
		if newCred.PtKey != oldKey {
			injectedBody = injectJoyCodeFields(body, newCred)
			return u.doCall(ctx, injectedBody, newCred)
		}
	}
	return resp, nil
}

// injectJoyCodeFields 往 OpenAI body 注入 JoyCode 网关必填业务字段
func injectJoyCodeFields(body []byte, cred *creds.JoyCodeCred) []byte {
	var req map[string]interface{}
	if err := creds.ParseJSONPublic(body, &req); err != nil {
		return body // 解析失败则原样返回
	}
	req["tenant"] = cred.Tenant
	req["orgFullName"] = cred.OrgFullName
	req["userId"] = cred.UserID
	req["client"] = "JoyCodeIDE"
	req["clientVersion"] = cred.ClientVersion
	req["language"] = "UNKNOWN"
	req["stream"] = false
	out, err := json.Marshal(req)
	if err != nil {
		return body
	}
	return out
}

func (u *JoyCodeUpstream) doCall(ctx context.Context, body []byte, cred *creds.JoyCodeCred) (*Response, error) {
	t := time.Now().UnixMilli()
	sign := creds.ColorSign("joycode_ide", "chat_completions", t, nil)
	url := fmt.Sprintf("%s/api?appid=joycode_ide&functionId=chat_completions&t=%d&sign=%s",
		cred.Origin, t, sign)
	reqID := fmt.Sprintf("req-%d-%s", t/1000, randString(6))

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json; charset=UTF-8")
	req.Header.Set("x-ms-client-request-id", reqID)
	req.Header.Set("ptKey", cred.PtKey)
	req.Header.Set("loginType", cred.LoginType)
	req.Header.Set("Accept", "application/json") // 不能用 text/event-stream！网关按 Accept 路由
	req.Header.Set("Accept-Encoding", "gzip")

	httpResp, err := u.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer httpResp.Body.Close()

	var reader io.Reader = httpResp.Body
	if httpResp.Header.Get("Content-Encoding") == "gzip" {
		gr, err := gzip.NewReader(httpResp.Body)
		if err == nil {
			reader = gr
			defer gr.Close()
		}
	}

	respBody, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}

	return &Response{
		StatusCode: httpResp.StatusCode,
		Body:       respBody,
		ReqID:      reqID,
	}, nil
}

// CallStream 真流式调用 JoyCode（stream:true，Color 网关返回标准 OpenAI SSE）
// 部分模型被网关灰度拒绝返回 COLOR_FORWARD_EXCEPTION 事件，doCallStream 通过 peek 检测后虚拟 406 降级
func (u *JoyCodeUpstream) CallStream(ctx context.Context, body []byte) (*StreamResponse, error) {
	cred, err := u.mgr.EnsureCreds()
	if err != nil {
		return nil, err
	}
	sr, err := u.doCallStream(ctx, body, cred)
	if err != nil {
		return nil, err
	}
	// 401 重试（纯 status 判断，流式下放弃 body code:401 检测）
	if sr.StatusCode == 401 {
		fmt.Println("[switch-dev] JoyCode 流式收到 401（pt_key 失效），重读 state.vscdb 并重试一次")
		sr.Body.Close()
		u.mgr.InvalidateCreds()
		oldKey := cred.PtKey
		newCred, err := u.mgr.EnsureCreds()
		if err != nil {
			return sr, nil
		}
		if newCred.PtKey != oldKey {
			return u.doCallStream(ctx, body, newCred)
		}
	}
	return sr, nil
}

// doCallStream 流式版 doCall
// - 保持 Accept:application/json（流式由 body stream:true 触发，不改 Accept 避免网关路由风险）
// - peek 检测 COLOR_FORWARD_EXCEPTION（406 灰度拒绝，HTTP 200 但流是错误事件）-> 虚拟 406 降级
// - 正常则返回 gzip 解压后的 SSE 流（bufio.Reader 保留 peek 数据供转换器读取）
func (u *JoyCodeUpstream) doCallStream(ctx context.Context, body []byte, cred *creds.JoyCodeCred) (*StreamResponse, error) {
	// 注入业务字段 + 覆盖 stream:true
	injected := injectJoyCodeFields(body, cred)
	var m map[string]interface{}
	if err := json.Unmarshal(injected, &m); err != nil {
		return nil, fmt.Errorf("解析请求 body 失败: %w", err)
	}
	m["stream"] = true
	m["stream_options"] = map[string]bool{"include_usage": true} // 让网关在最后一个 chunk 返回 usage
	bodyBytes, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("重新编码请求 body 失败: %w", err)
	}

	t := time.Now().UnixMilli()
	sign := creds.ColorSign("joycode_ide", "chat_completions", t, nil)
	url := fmt.Sprintf("%s/api?appid=joycode_ide&functionId=chat_completions&t=%d&sign=%s",
		cred.Origin, t, sign)
	reqID := fmt.Sprintf("req-%d-%s", t/1000, randString(6))

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json; charset=UTF-8")
	req.Header.Set("x-ms-client-request-id", reqID)
	req.Header.Set("ptKey", cred.PtKey)
	req.Header.Set("loginType", cred.LoginType)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "gzip")

	httpResp, err := u.streamClient.Do(req)
	if err != nil {
		return nil, err
	}

	// 非 200：读错误体返回
	if httpResp.StatusCode != 200 {
		errBody, _ := io.ReadAll(httpResp.Body)
		httpResp.Body.Close()
		return &StreamResponse{
			StatusCode: httpResp.StatusCode,
			Body:       io.NopCloser(bytes.NewReader(errBody)),
			ReqID:      reqID,
		}, nil
	}

	// 200：构造 reader（含 gzip 解压）
	var rc io.ReadCloser = httpResp.Body
	if httpResp.Header.Get("Content-Encoding") == "gzip" {
		gr, err := gzip.NewReader(httpResp.Body)
		if err != nil {
			httpResp.Body.Close()
			return nil, fmt.Errorf("gzip 解压失败: %w", err)
		}
		rc = &gzipReadCloser{gr: gr, underlying: httpResp.Body}
	}

	// peek 检测空流 + COLOR_FORWARD_EXCEPTION（406 灰度拒绝，HTTP 200 但流是错误事件）
	br := bufio.NewReader(rc)
	peeked, _ := br.Peek(256)
	if len(peeked) == 0 {
		rc.Close()
		fmt.Printf("[switch-dev] joycode 流式上游返回空流，降级\n")
		return &StreamResponse{
			StatusCode: 502,
			Body:       io.NopCloser(bytes.NewReader([]byte(`{"error":"upstream empty stream"}`))),
			ReqID:      reqID,
		}, nil
	}
	if bytes.Contains(peeked, []byte("COLOR_FORWARD_EXCEPTION")) || bytes.Contains(peeked, []byte("AI_GRAY_")) {
		errBody, _ := io.ReadAll(br)
		rc.Close()
		snippet := string(errBody)
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		fmt.Printf("[switch-dev] JoyCode 流式收到灰度拒绝（COLOR_FORWARD_EXCEPTION/AI_GRAY_），降级到伪流式: %s\n", snippet)
		return &StreamResponse{
			StatusCode: 406, // 虚拟 406，让 executeChainStream 降级到伪流式（Call 非流式）
			Body:       io.NopCloser(bytes.NewReader(errBody)),
			ReqID:      reqID,
		}, nil
	}
	if !bytes.Contains(peeked, []byte("data:")) {
		rc.Close()
		snippet := string(peeked)
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		fmt.Printf("[switch-dev] joycode 流式上游返回非 SSE 内容，回退非流式: %s\n", snippet)
		return &StreamResponse{
			StatusCode: 406,
			Body:       io.NopCloser(bytes.NewReader(peeked)),
			ReqID:      reqID,
		}, nil
	}
	// event: error 且 data 为空（如 DevEco 网关错误帧），提前降级
	peekStr := string(peeked)
	if bytes.Contains(peeked, []byte("event: error")) {
		hasData := false
		for _, line := range strings.Split(peekStr, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "data:") {
				data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
				if data != "" && data != "null" && data != "[DONE]" {
					hasData = true
					break
				}
			}
		}
		if !hasData {
			rc.Close()
			fmt.Printf("[switch-dev] joycode 流式上游 SSE error 事件（空 data），降级\n")
			return &StreamResponse{
				StatusCode: 502,
				Body:       io.NopCloser(bytes.NewReader([]byte(`{"error":"upstream SSE error"}`))),
				ReqID:      reqID,
			}, nil
		}
	}

	// 正常 SSE 流（bufio.Reader 包含 peek 的数据，转换器能读到完整流）
	return &StreamResponse{
		StatusCode: 200,
		Body:       &bufferedReadCloser{br: br, c: rc},
		ReqID:      reqID,
	}, nil
}

// gzipReadCloser 包装 gzip.Reader + 底层 ReadCloser，Close 同时关闭两者
type gzipReadCloser struct {
	gr         *gzip.Reader
	underlying io.Closer
}

func (g *gzipReadCloser) Read(b []byte) (int, error) { return g.gr.Read(b) }
func (g *gzipReadCloser) Close() error {
	g.gr.Close()
	return g.underlying.Close()
}

// bufferedReadCloser 包装 bufio.Reader + ReadCloser，保留 peek 的数据供后续读取
type bufferedReadCloser struct {
	br *bufio.Reader
	c  io.Closer
}

func (b *bufferedReadCloser) Read(p []byte) (int, error) { return b.br.Read(p) }
func (b *bufferedReadCloser) Close() error               { return b.c.Close() }

// isPtKeyInvalid 检测响应是否 pt_key 失效
func isPtKeyInvalid(resp *Response) bool {
	if resp.StatusCode == 401 {
		return true
	}
	// 尝试解析 body
	var o struct {
		Code int `json:"code"`
		Data struct {
			PtKey interface{} `json:"ptKey"`
		} `json:"data"`
	}
	if creds.ParseJSONPublic(resp.Body, &o) == nil {
		if o.Code == 401 {
			return true
		}
	}
	return false
}