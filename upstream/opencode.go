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

// OpenCodeUpstream OpenCode Zen 适配器
type OpenCodeUpstream struct {
	mgr          *creds.OpenCodeCredManager
	client       *http.Client
	streamClient *http.Client // 流式专用：无整体 Timeout，由 context 控制超时
}

func NewOpenCodeUpstream(mgr *creds.OpenCodeCredManager) *OpenCodeUpstream {
	return &OpenCodeUpstream{
		mgr:          mgr,
		client:       &http.Client{Timeout: 120 * time.Second},
		streamClient: &http.Client{}, // 无 Timeout：流式由 context 控制
	}
}

func (u *OpenCodeUpstream) Name() string { return "opencode" }

func (u *OpenCodeUpstream) EnsureCreds(ctx context.Context) error {
	_, err := u.mgr.EnsureCreds()
	return err
}

func (u *OpenCodeUpstream) InvalidateCreds() { u.mgr.InvalidateCreds() }

func (u *OpenCodeUpstream) VerifyCreds(ctx context.Context) (*VerifyResult, error) {
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

func (u *OpenCodeUpstream) CredStatus() *creds.CredStatusInfo {
	return u.mgr.CredStatus()
}

// HasValidCreds 凭据是否可用
// 宽松判定：apiKey 已加载就返回 true，让 Call 内部的重读逻辑处理失效
func (u *OpenCodeUpstream) HasValidCreds() bool {
	cred := u.mgr.GetCred()
	return cred != nil && cred.APIKey != ""
}

// FetchModels 调 GET /models 拉取可用模型列表
func (u *OpenCodeUpstream) FetchModels(ctx context.Context) ([]FetchedModel, error) {
	cred, err := u.mgr.EnsureCreds()
	if err != nil {
		return nil, err
	}
	url := u.mgr.Config().BaseURL + "/models"

	body, _, err := httpGet(url, map[string]string{
		"Authorization": fmt.Sprintf("Bearer %s", cred.APIKey),
		"Accept":        "application/json",
	})
	if err != nil {
		return nil, err
	}

	var resp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := creds.ParseJSONPublic(body, &resp); err != nil {
		return nil, err
	}

	result := make([]FetchedModel, 0, len(resp.Data))
	for _, m := range resp.Data {
		if m.ID == "" {
			continue
		}
		result = append(result, FetchedModel{
			ID:    m.ID,
			Label: m.ID,
		})
	}
	return result, nil
}

// Call 调用 OpenCode Zen（带 401 重读重试）
func (u *OpenCodeUpstream) Call(ctx context.Context, body []byte) (*Response, error) {
	cred, err := u.mgr.EnsureCreds()
	if err != nil {
		return nil, err
	}

	resp, err := u.doCall(ctx, body, cred)
	if err != nil {
		return nil, err
	}

	if isOpenCodeKeyInvalid(resp) {
		fmt.Println("[switch-dev] OpenCode 收到 401（apiKey 失效），重读 auth.json 并重试一次")
		u.mgr.InvalidateCreds()
		oldKey := cred.APIKey
		newCred, err := u.mgr.EnsureCreds()
		if err != nil {
			return resp, nil
		}
		if newCred.APIKey != oldKey {
			return u.doCall(ctx, body, newCred)
		}
	}
	return resp, nil
}

func (u *OpenCodeUpstream) doCall(ctx context.Context, body []byte, cred *creds.OpenCodeCred) (*Response, error) {
	url := u.mgr.Config().BaseURL + "/chat/completions"
	reqID := fmt.Sprintf("req-%d-%s", time.Now().Unix(), randString(6))

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json; charset=UTF-8")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", cred.APIKey))
	req.Header.Set("Accept", "application/json")

	httpResp, err := u.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer httpResp.Body.Close()

	respBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, err
	}

	return &Response{
		StatusCode: httpResp.StatusCode,
		Body:       respBody,
		ReqID:      reqID,
	}, nil
}

// CallStream 真流式调用 OpenCode（stream:true，直接返回 SSE 流）
func (u *OpenCodeUpstream) CallStream(ctx context.Context, body []byte) (*StreamResponse, error) {
	cred, err := u.mgr.EnsureCreds()
	if err != nil {
		return nil, err
	}
	sr, err := u.doCallStream(ctx, body, cred)
	if err != nil {
		return nil, err
	}
	if sr.StatusCode == 401 {
		fmt.Println("[switch-dev] OpenCode 流式收到 401（apiKey 失效），重读 auth.json 并重试一次")
		sr.Body.Close()
		u.mgr.InvalidateCreds()
		oldKey := cred.APIKey
		newCred, err := u.mgr.EnsureCreds()
		if err != nil {
			return sr, nil
		}
		if newCred.APIKey != oldKey {
			return u.doCallStream(ctx, body, newCred)
		}
	}
	return sr, nil
}

// doCallStream 流式版 doCall：覆盖 stream:true，200 时直接返回 SSE 流
func (u *OpenCodeUpstream) doCallStream(ctx context.Context, body []byte, cred *creds.OpenCodeCred) (*StreamResponse, error) {
	var m map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("解析请求 body 失败: %w", err)
	}
	m["stream"] = true
	m["stream_options"] = map[string]bool{"include_usage": true} // 让上游在最后一个 chunk 返回 usage
	bodyBytes, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("重新编码请求 body 失败: %w", err)
	}

	url := u.mgr.Config().BaseURL + "/chat/completions"
	reqID := fmt.Sprintf("req-%d-%s", time.Now().Unix(), randString(6))

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json; charset=UTF-8")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", cred.APIKey))
	req.Header.Set("Accept", "text/event-stream")

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

	// 200：peek 检测空流 + 非 SSE 内容，异常则虚拟 502 降级
	br := bufio.NewReader(httpResp.Body)
	peeked, _ := br.Peek(256)
	peekStr := string(peeked)
	if len(peeked) == 0 {
		httpResp.Body.Close()
		fmt.Printf("[switch-dev] opencode 流式上游返回空流，降级\n")
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
		fmt.Printf("[switch-dev] opencode 流式上游返回非 SSE 内容，降级: %s\n", snippet)
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
			fmt.Printf("[switch-dev] opencode 流式上游 SSE error 事件（空 data），降级\n")
			return &StreamResponse{
				StatusCode: 502,
				Body:       io.NopCloser(bytes.NewReader([]byte(`{"error":"upstream SSE error"}`))),
				ReqID:      reqID,
			}, nil
		}
	}
	return &StreamResponse{
		StatusCode: 200,
		Body:       &bufferedReadCloser{br: br, c: httpResp.Body},
		ReqID:      reqID,
	}, nil
}

// isOpenCodeKeyInvalid OpenCode apiKey 失效判断
func isOpenCodeKeyInvalid(resp *Response) bool {
	if resp.StatusCode == 401 {
		return true
	}
	var o struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if creds.ParseJSONPublic(resp.Body, &o) == nil {
		if strings.Contains(strings.ToLower(o.Error.Type), "auth") {
			return true
		}
	}
	return false
}