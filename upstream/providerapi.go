package upstream

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"switchdev/creds"
)

// ProviderAPIUpstream 通用免费/自定义 API 上游适配器
// 支持任意 base_url + key 的供应商，按出站协议分两种：
//   - openai（默认）：POST /chat/completions，Authorization: Bearer
//   - anthropic：   POST /v1/messages，x-api-key + anthropic-version
//
// 与内置 4 上游平级，可进降级链。
type ProviderAPIUpstream struct {
	name         string // provider id（如 "groq"、"custom-xxx"）
	displayName  string // 供应商显示名（如 "Groq"）
	getBaseURL   func() string
	getAPIKey    func() string
	getProtocol  func() string // 返回 "openai"（默认）或 "anthropic"
	client       *http.Client
	streamClient *http.Client // 流式专用：无整体 Timeout，由 context 控制超时
}

// NewProviderAPIUpstream 创建免费 API 上游
// providerID 用于路由标识；getBaseURL/getAPIKey/getProtocol 返回当前绑定的 provider 配置
func NewProviderAPIUpstream(providerID string, getBaseURL, getAPIKey, getProtocol func() string) *ProviderAPIUpstream {
	return &ProviderAPIUpstream{
		name:         providerID,
		getBaseURL:   getBaseURL,
		getAPIKey:    getAPIKey,
		getProtocol:  getProtocol,
		client:       &http.Client{Timeout: 120 * time.Second},
		streamClient: &http.Client{}, // 无 Timeout：流式由 context 控制
	}
}

// SetDisplayName 设置供应商显示名（前端展示用）
func (u *ProviderAPIUpstream) SetDisplayName(name string) {
	u.displayName = name
}

// Rebind 热切换绑定的 provider 配置（激活切换时调用）
func (u *ProviderAPIUpstream) Rebind(getBaseURL, getAPIKey, getProtocol func() string) {
	u.getBaseURL = getBaseURL
	u.getAPIKey = getAPIKey
	u.getProtocol = getProtocol
}

// protocol 返回规范化的出站协议
func (u *ProviderAPIUpstream) protocol() string {
	if u.getProtocol != nil && u.getProtocol() == "anthropic" {
		return "anthropic"
	}
	return "openai"
}

func (u *ProviderAPIUpstream) Name() string { return u.name }

// Protocol 返回该上游的出站协议（"openai" 或 "anthropic"），供代理层决定请求/响应转换
func (u *ProviderAPIUpstream) Protocol() string { return u.protocol() }

func (u *ProviderAPIUpstream) EnsureCreds(ctx context.Context) error {
	if u.getAPIKey == nil || u.getAPIKey() == "" {
		return fmt.Errorf("免费 API 供应商 %s 未配置 API Key", u.name)
	}
	return nil
}

func (u *ProviderAPIUpstream) InvalidateCreds() {}

func (u *ProviderAPIUpstream) VerifyCreds(ctx context.Context) (*VerifyResult, error) {
	if u.getAPIKey == nil || u.getAPIKey() == "" {
		return &VerifyResult{Valid: false, Status: -1}, fmt.Errorf("未配置 API Key")
	}
	status, err := u.verifyKey(u.getAPIKey())
	if err != nil {
		return &VerifyResult{Valid: false, Status: status}, err
	}
	// 200/400/404 都视为连通（401/403 才算 key 无效）；404 对 anthropic 是端点正常但方法/路径差异
	valid := status == 200 || status == 400 || status == 404
	return &VerifyResult{Valid: valid, Status: status}, nil
}

func (u *ProviderAPIUpstream) CredStatus() *creds.CredStatusInfo {
	name := u.name
	if u.displayName != "" {
		name = u.displayName
	}
	return &creds.CredStatusInfo{
		Valid:      u.HasValidCreds(),
		Installed:  u.HasValidCreds(),
		KeyPreview: u.keyPreview(),
		Source:     name,
	}
}

func (u *ProviderAPIUpstream) HasValidCreds() bool {
	return u.getAPIKey != nil && u.getAPIKey() != ""
}

// FetchModels 拉取可用模型列表
func (u *ProviderAPIUpstream) FetchModels(ctx context.Context) ([]FetchedModel, error) {
	key := u.getAPIKey()
	if key == "" {
		return nil, fmt.Errorf("未配置 API Key")
	}
	body, status, err := u.getModels(key)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("GET /models 返回 %d", status)
	}
	var resp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	result := make([]FetchedModel, 0, len(resp.Data))
	for _, m := range resp.Data {
		if m.ID == "" {
			continue
		}
		result = append(result, FetchedModel{ID: m.ID, Label: m.ID, Stream: true})
	}
	return result, nil
}

// Call 非流式调用
func (u *ProviderAPIUpstream) Call(ctx context.Context, body []byte) (*Response, error) {
	key := u.getAPIKey()
	if key == "" {
		return nil, fmt.Errorf("免费 API 供应商 %s 未配置 API Key", u.name)
	}
	return u.doCall(ctx, body, key)
}

// CallStream 真流式调用
func (u *ProviderAPIUpstream) CallStream(ctx context.Context, body []byte) (*StreamResponse, error) {
	key := u.getAPIKey()
	if key == "" {
		return nil, fmt.Errorf("免费 API 供应商 %s 未配置 API Key", u.name)
	}
	return u.doCallStream(ctx, body, key)
}

func (u *ProviderAPIUpstream) base() string {
	return strings.TrimSuffix(u.getBaseURL(), "/")
}

// endpoint 返回非流式/流式请求的完整路径
func (u *ProviderAPIUpstream) endpoint() string {
	if u.protocol() == "anthropic" {
		return u.base() + "/v1/messages"
	}
	return u.base() + "/chat/completions"
}

// setAuth 按协议设置鉴权头
func (u *ProviderAPIUpstream) setAuth(req *http.Request, key string) {
	if u.protocol() == "anthropic" {
		req.Header.Set("x-api-key", key)
		req.Header.Set("anthropic-version", "2023-06-01")
	} else {
		req.Header.Set("Authorization", "Bearer "+key)
	}
}

func (u *ProviderAPIUpstream) doCall(ctx context.Context, body []byte, key string) (*Response, error) {
	url := u.endpoint()
	reqID := fmt.Sprintf("req-%d-%s", time.Now().Unix(), randString(6))

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json; charset=UTF-8")
	req.Header.Set("Accept", "application/json")
	u.setAuth(req, key)

	httpResp, err := u.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer httpResp.Body.Close()
	respBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, err
	}
	return &Response{StatusCode: httpResp.StatusCode, Body: respBody, ReqID: reqID}, nil
}

func (u *ProviderAPIUpstream) doCallStream(ctx context.Context, body []byte, key string) (*StreamResponse, error) {
	var bodyBytes []byte
	// openai 协议：强制 stream 并加 include_usage；anthropic 协议 body 已是 anthropic 格式，
	// 由 router 构造好 stream 字段，这里直接透传。
	if u.protocol() == "openai" {
		var m map[string]any
		if err := json.Unmarshal(body, &m); err != nil {
			return nil, fmt.Errorf("解析请求 body 失败: %w", err)
		}
		m["stream"] = true
		m["stream_options"] = map[string]bool{"include_usage": true}
		b, err := json.Marshal(m)
		if err != nil {
			return nil, fmt.Errorf("重新编码请求 body 失败: %w", err)
		}
		bodyBytes = b
	} else {
		bodyBytes = body
	}

	url := u.endpoint()
	reqID := fmt.Sprintf("req-%d-%s", time.Now().Unix(), randString(6))

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	u.setAuth(req, key)

	httpResp, err := u.streamClient.Do(req)
	if err != nil {
		return nil, err
	}
	if httpResp.StatusCode != 200 {
		errBody, _ := io.ReadAll(httpResp.Body)
		httpResp.Body.Close()
		return &StreamResponse{StatusCode: httpResp.StatusCode, Body: io.NopCloser(bytes.NewReader(errBody)), ReqID: reqID}, nil
	}

	// peek 检测空流 + 非 SSE（两种协议的 SSE 都含 data:）
	br := bufio.NewReader(httpResp.Body)
	peeked, _ := br.Peek(256)
	peekStr := string(peeked)
	if len(peeked) == 0 {
		httpResp.Body.Close()
		return &StreamResponse{
			StatusCode: 502,
			Body:       io.NopCloser(bytes.NewReader([]byte(`{"error":"upstream empty stream"}`))),
			ReqID:      reqID,
		}, nil
	}
	if !strings.Contains(peekStr, "data:") {
		httpResp.Body.Close()
		snippet := peekStr
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		errJSON, _ := json.Marshal(map[string]string{"error": "non-sse: " + snippet})
		return &StreamResponse{
			StatusCode: 502,
			Body:       io.NopCloser(bytes.NewReader(errJSON)),
			ReqID:      reqID,
		}, nil
	}
	if strings.Contains(peekStr, "event: error") {
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
			httpResp.Body.Close()
			return &StreamResponse{
				StatusCode: 502,
				Body:       io.NopCloser(bytes.NewReader([]byte(`{"error":"upstream SSE error"}`))),
				ReqID:      reqID,
			}, nil
		}
	}
	return &StreamResponse{StatusCode: 200, Body: &bufferedReadCloser{br: br, c: httpResp.Body}, ReqID: reqID}, nil
}

// verifyKey 用一个轻量请求验证 key 是否有效，返回 HTTP 状态码。
// openai 协议：GET /models；anthropic 协议：POST /v1/messages（最小请求，
// 401=key 无效，400/200=连通且 key 有效）。
func (u *ProviderAPIUpstream) verifyKey(key string) (int, error) {
	var req *http.Request
	var err error
	if u.protocol() == "anthropic" {
		url := u.base() + "/v1/messages"
		req, err = http.NewRequest("POST", url, bytes.NewReader([]byte(`{"model":"test","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)))
	} else {
		url := u.base() + "/models"
		req, err = http.NewRequest("GET", url, nil)
	}
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	u.setAuth(req, key)
	resp, err := u.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// getModels GET /models（仅 openai 协议；FetchModels 用）
func (u *ProviderAPIUpstream) getModels(key string) ([]byte, int, error) {
	url := u.base() + "/models"
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	resp, err := u.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

func (u *ProviderAPIUpstream) keyPreview() string {
	key := u.getAPIKey()
	if len(key) > 8 {
		return key[:8] + "..."
	}
	if key != "" {
		return "***"
	}
	return ""
}
