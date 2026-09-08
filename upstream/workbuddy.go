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

// WorkBuddyUpstream 腾讯 CodeBuddy(WorkBuddy) 适配器
// 上游只支持 stream:true：流式请求走 CallStream 直传 SSE，
// 非流式请求走 Call 在内部聚合成完整 JSON，对上层透明
type WorkBuddyUpstream struct {
	mgr          *creds.WorkBuddyCredManager
	client       *http.Client
	streamClient *http.Client // 流式专用：无整体 Timeout（WorkBuddy 两条路径都走上游流式）
}

func NewWorkBuddyUpstream(mgr *creds.WorkBuddyCredManager) *WorkBuddyUpstream {
	return &WorkBuddyUpstream{
		mgr:          mgr,
		client:       &http.Client{Timeout: 120 * time.Second},
		streamClient: &http.Client{}, // 无 Timeout：WorkBuddy 强制流式，聚合可能持续数分钟
	}
}

func (u *WorkBuddyUpstream) Name() string { return "workbuddy" }

func (u *WorkBuddyUpstream) EnsureCreds(ctx context.Context) error {
	_, err := u.mgr.EnsureCreds()
	return err
}

func (u *WorkBuddyUpstream) InvalidateCreds() { u.mgr.InvalidateCreds() }

func (u *WorkBuddyUpstream) VerifyCreds(ctx context.Context) (*VerifyResult, error) {
	cred := u.mgr.GetCred()
	if cred == nil {
		c, err := u.mgr.LoadCreds()
		if err != nil {
			return &VerifyResult{Valid: false, Status: -1}, err
		}
		cred = c
	}
	valid, status, err := u.mgr.VerifyCreds(cred)
	if err != nil {
		return &VerifyResult{Valid: false, Status: -1}, err
	}
	return &VerifyResult{Valid: valid, Status: status}, nil
}

func (u *WorkBuddyUpstream) CredStatus() *creds.CredStatusInfo {
	return u.mgr.CredStatus()
}

// HasValidCreds 凭据是否可用（宽松判定：accessToken 已加载即返回 true）
func (u *WorkBuddyUpstream) HasValidCreds() bool {
	cred := u.mgr.GetCred()
	return cred != nil && cred.AccessToken != ""
}

// FetchModels 调 GET /models 拉取 WorkBuddy 可用模型列表（OpenAI 兼容端点）。
// 失败时返回 error，由上层 MergeModels 回退 DB 缓存或本地种子白名单。
func (u *WorkBuddyUpstream) FetchModels(ctx context.Context) ([]FetchedModel, error) {
	cred, err := u.mgr.EnsureCreds()
	if err != nil {
		return nil, err
	}

	url := u.mgr.Config().BaseURL + "/models"
	body, status, err := httpGet(url, creds.WorkBuddyAuthHeaders(cred))
	if err != nil {
		return nil, fmt.Errorf("workbuddy /models 请求失败: %w", err)
	}
	if status != 200 {
		return nil, fmt.Errorf("workbuddy /models 返回 status=%d", status)
	}

	var resp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("workbuddy /models 解析失败: %w", err)
	}

	result := make([]FetchedModel, 0, len(resp.Data))
	for _, m := range resp.Data {
		if m.ID == "" {
			continue
		}
		result = append(result, FetchedModel{
			ID:     m.ID,
			Label:  m.ID,
			Stream: true,
		})
	}
	return result, nil
}

// Call 调用 WorkBuddy（强制 stream:true + 聚合 SSE + 401 重试）
func (u *WorkBuddyUpstream) Call(ctx context.Context, body []byte) (*Response, error) {
	cred, err := u.mgr.EnsureCreds()
	if err != nil {
		return nil, err
	}

	resp, err := u.doCall(ctx, body, cred)
	if err != nil {
		return nil, err
	}

	if isWorkBuddyTokenInvalid(resp) {
		fmt.Println("[switch-dev] WorkBuddy 收到 401/403（token 失效），刷新并重试一次")
		u.mgr.InvalidateCreds()
		newCred, err := u.mgr.EnsureCreds()
		if err != nil {
			return resp, nil
		}
		return u.doCall(ctx, body, newCred)
	}
	return resp, nil
}

func (u *WorkBuddyUpstream) doCall(ctx context.Context, body []byte, cred *creds.WorkBuddyCred) (*Response, error) {
	req, reqID, err := u.buildChatRequest(ctx, body, cred)
	if err != nil {
		return nil, err
	}

	httpResp, err := u.streamClient.Do(req) // WorkBuddy 强制流式，用无 Timeout 的 client
	if err != nil {
		return nil, err
	}
	defer httpResp.Body.Close()

	// 非 200：原样返回错误体，让上层 isUpstreamErrorResponse 判断
	if httpResp.StatusCode != 200 {
		errBody, _ := io.ReadAll(httpResp.Body)
		return &Response{
			StatusCode: httpResp.StatusCode,
			Body:       errBody,
			ReqID:      reqID,
		}, nil
	}

	// 200：聚合 SSE 流成完整 OpenAIResponse JSON
	aggregated, err := aggregateOpenAISSE(httpResp.Body)
	if err != nil {
		return nil, fmt.Errorf("聚合 WorkBuddy SSE 失败: %w", err)
	}
	return &Response{
		StatusCode: 200,
		Body:       aggregated,
		ReqID:      reqID,
	}, nil
}

// buildChatRequest 构造 WorkBuddy chat 请求（流式与非流式共用）。
// 上游强制 stream:true（非流式返回 code:11101），故两条路径的出站 body 完全一致，
// 区别只在拿到响应后是聚合成完整 JSON 还是直接把 SSE 流交给上层。
// 这里的字段删减与请求头是对齐官方 WorkBuddy CLI 的，漏一项会被风控识别为非官方客户端。
func (u *WorkBuddyUpstream) buildChatRequest(ctx context.Context, body []byte, cred *creds.WorkBuddyCred) (*http.Request, string, error) {
	var m map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, "", fmt.Errorf("解析请求 body 失败: %w", err)
	}
	m["stream"] = true
	delete(m, "stream_options") // 网关不识别，11140
	delete(m, "tenant")         // JoyCode 业务字段，WorkBuddy 不认
	delete(m, "orgFullName")
	delete(m, "userId")
	delete(m, "client")
	delete(m, "clientVersion")
	delete(m, "language")
	// max_tokens 为 0 时部分网关拒；兜底 1024
	if mt, ok := m["max_tokens"].(float64); !ok || mt <= 0 {
		m["max_tokens"] = 1024
	}
	bodyBytes, err := json.Marshal(m)
	if err != nil {
		return nil, "", fmt.Errorf("重新编码请求 body 失败: %w", err)
	}

	url := u.mgr.Config().BaseURL + "/chat/completions"
	reqID := fmt.Sprintf("req-%d-%s", time.Now().Unix(), randString(6))

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Content-Type", "application/json; charset=UTF-8")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", cred.AccessToken))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Private-Data", "false")
	// 与官方 WorkBuddy CLI 的 auth 拦截器一致：每个 chat 请求必带 X-Requested-With；
	// 有 uid 时 X-User-Id 与 X-Userinfo 成对发送，缺一会被风控识别为非官方客户端
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	if cred.UID != "" {
		req.Header.Set("X-User-Id", cred.UID)
		if ui := creds.UserInfoHeader(cred.UID); ui != "" {
			req.Header.Set("X-Userinfo", ui)
		}
	}
	// 不设 Accept-Encoding，让 Go http.Client 自动透明解压 gzip
	return req, reqID, nil
}

// CallStream 真流式调用 WorkBuddy：SSE 流直接交给上层逐块中继，不再等整段生成完。
// 上游本来就只支持 stream:true，此前缺这个方法导致流式请求回退到
// 「Call 聚合完整响应 + 代理拆伪流式」，首字要等整个回答生成完（20-40s）。
func (u *WorkBuddyUpstream) CallStream(ctx context.Context, body []byte) (*StreamResponse, error) {
	cred, err := u.mgr.EnsureCreds()
	if err != nil {
		return nil, err
	}

	sr, err := u.doCallStream(ctx, body, cred)
	if err != nil {
		return nil, err
	}

	// 401/403 均视为 token 失效（与非流式 isWorkBuddyTokenInvalid 口径一致）
	if sr.StatusCode == 401 || sr.StatusCode == 403 {
		fmt.Println("[switch-dev] WorkBuddy 流式收到 401/403（token 失效），刷新并重试一次")
		sr.Body.Close()
		u.mgr.InvalidateCreds()
		newCred, err := u.mgr.EnsureCreds()
		if err != nil {
			return sr, nil
		}
		return u.doCallStream(ctx, body, newCred)
	}
	return sr, nil
}

// doCallStream 流式版 doCall：200 时直接返回 SSE 流（异常情况返回虚拟 502 让 router 降级）
func (u *WorkBuddyUpstream) doCallStream(ctx context.Context, body []byte, cred *creds.WorkBuddyCred) (*StreamResponse, error) {
	req, reqID, err := u.buildChatRequest(ctx, body, cred)
	if err != nil {
		return nil, err
	}

	httpResp, err := u.streamClient.Do(req)
	if err != nil {
		return nil, err
	}

	if httpResp.StatusCode != 200 {
		errBody, _ := io.ReadAll(httpResp.Body)
		httpResp.Body.Close()
		return &StreamResponse{
			StatusCode: httpResp.StatusCode,
			Body:       io.NopCloser(bytes.NewReader(errBody)),
			ReqID:      reqID,
		}, nil
	}

	// 200：peek 检测空流 / 非 SSE 内容，异常则虚拟 502 降级
	br := bufio.NewReader(httpResp.Body)
	peeked, _ := br.Peek(256)
	if len(peeked) == 0 {
		httpResp.Body.Close()
		fmt.Printf("[switch-dev] workbuddy 流式上游返回空流，降级\n")
		return &StreamResponse{
			StatusCode: 502,
			Body:       io.NopCloser(bytes.NewReader([]byte(`{"error":"upstream empty stream"}`))),
			ReqID:      reqID,
		}, nil
	}
	if !strings.Contains(string(peeked), "data:") {
		httpResp.Body.Close()
		snippet := string(peeked)
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		fmt.Printf("[switch-dev] workbuddy 流式上游返回非 SSE 内容，降级: %s\n", snippet)
		errJSON, _ := json.Marshal(map[string]string{"error": "non-sse: " + snippet})
		return &StreamResponse{
			StatusCode: 502,
			Body:       io.NopCloser(bytes.NewReader(errJSON)),
			ReqID:      reqID,
		}, nil
	}

	return &StreamResponse{
		StatusCode: 200,
		Body:       &bufferedReadCloser{br: br, c: httpResp.Body},
		ReqID:      reqID,
	}, nil
}

// isWorkBuddyTokenInvalid token 失效判断（401/403）
func isWorkBuddyTokenInvalid(resp *Response) bool {
	return resp.StatusCode == 401 || resp.StatusCode == 403
}
