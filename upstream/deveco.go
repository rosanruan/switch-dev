package upstream

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"switchdev/creds"
)

// DevEcoUpstream DevEco Code 华为 MaaS 网关适配器
type DevEcoUpstream struct {
	mgr          *creds.DevEcoCredManager
	client       *http.Client
	streamClient *http.Client // 流式专用：无整体 Timeout，由 context 控制超时
}

func NewDevEcoUpstream(mgr *creds.DevEcoCredManager) *DevEcoUpstream {
	return &DevEcoUpstream{
		mgr:          mgr,
		client:       &http.Client{Timeout: 120 * time.Second},
		streamClient: &http.Client{}, // 无 Timeout：流式由 context 控制
	}
}

func (u *DevEcoUpstream) Name() string { return "deveco" }

func (u *DevEcoUpstream) EnsureCreds(ctx context.Context) error {
	_, err := u.mgr.EnsureCreds()
	return err
}

func (u *DevEcoUpstream) InvalidateCreds() { u.mgr.InvalidateCreds() }

func (u *DevEcoUpstream) VerifyCreds(ctx context.Context) (*VerifyResult, error) {
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

func (u *DevEcoUpstream) CredStatus() *creds.CredStatusInfo {
	return u.mgr.CredStatus()
}

// HasValidCreds 凭据是否可用
// 宽松判定：access token 过期但 JWT 有效（可刷新）时仍返回 true，让 Call 触发刷新
// 仅当凭据未加载或 JWT 也过期时才返回 false（需重登）
func (u *DevEcoUpstream) HasValidCreds() bool {
	cred := u.mgr.GetCred()
	if cred == nil {
		return false
	}
	if cred.Valid {
		return true
	}
	// access 过期但 JWT 仍有效 -> 可刷新，不跳过
	if cred.JWTExp > 0 && time.Now().UnixMilli() < cred.JWTExp {
		return true
	}
	return false
}

// FetchModels 调 GET /codeGenie/modelConfig 拉取可用模型列表
func (u *DevEcoUpstream) FetchModels(ctx context.Context) ([]FetchedModel, error) {
	cred, err := u.mgr.EnsureCreds()
	if err != nil {
		return nil, err
	}
	url := fmt.Sprintf("%s/codeGenie/modelConfig?localVersion=0&pluginVersion=%s",
		u.mgr.Config().Origin, u.mgr.Config().PluginVersion)

	body, _, err := httpGet(url, map[string]string{
		"Authorization": fmt.Sprintf("Bearer %s", cred.AccessToken),
		"Accept":        "application/json",
	})
	if err != nil {
		return nil, err
	}

	var resp struct {
		Success bool `json:"success"`
		Body    struct {
			InnerModels []struct {
				ModelConfigs []struct {
					ModelID          string   `json:"model_id"`
					ContextWindow    int      `json:"context_window"`
					Output           int      `json:"output"`
					InputModalities  []string `json:"input_modalities"`
					ThinkingMode     string   `json:"thinking_mode"`
					ToolCallMode     string   `json:"tool_call_mode"`
				} `json:"model_configs"`
			} `json:"inner_models"`
		} `json:"body"`
	}
	if err := creds.ParseJSONPublic(body, &resp); err != nil {
		return nil, err
	}

	result := make([]FetchedModel, 0)
	for _, im := range resp.Body.InnerModels {
		for _, mc := range im.ModelConfigs {
			if mc.ModelID == "" {
				continue
			}
			fm := FetchedModel{
				ID:        mc.ModelID, // 接口返回的 model_id（如 GLM-5.1）
				Label:     mc.ModelID,
				Context:   mc.ContextWindow,
				Output:    mc.Output,
				Stream:    true, // DevEco 都支持流式
				Vision:    containsStr(mc.InputModalities, "image"),
				ToolCall:  mc.ToolCallMode != "none" && mc.ToolCallMode != "",
				Reasoning: mc.ThinkingMode == "on" || mc.ThinkingMode == "configurable",
			}
			result = append(result, fm)
		}
	}
	return result, nil
}

// containsStr 检查字符串切片是否包含某值
func containsStr(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}

// Call 调用 DevEco MaaS 网关（带 token 失效自动刷新重试）
func (u *DevEcoUpstream) Call(ctx context.Context, body []byte) (*Response, error) {
	cred, err := u.mgr.EnsureCreds()
	if err != nil {
		return nil, err
	}

	resp, err := u.doCall(ctx, body, cred)
	if err != nil {
		return nil, err
	}

	// token 失效 -> 刷新并重试一次
	if isDevEcoTokenInvalid(resp) {
		fmt.Println("[switch-dev] DevEco 收到 token 失效信号，触发刷新并重试一次")
		u.mgr.InvalidateCreds()
		oldToken := cred.AccessToken
		newCred, err := u.mgr.EnsureCreds() // 预检失败会自动用 jwtToken 刷新
		if err != nil {
			return resp, nil
		}
		if newCred.AccessToken != oldToken {
			return u.doCall(ctx, body, newCred)
		}
	}
	return resp, nil
}

func (u *DevEcoUpstream) doCall(ctx context.Context, body []byte, cred *creds.DevEcoCred) (*Response, error) {
	// 非流式端点
	url := fmt.Sprintf("%s%s/no-stream/chat/completions",
		u.mgr.Config().Origin, u.mgr.Config().MaasPath)
	reqID := fmt.Sprintf("req-%d-%s", time.Now().Unix(), randString(6))

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json; charset=UTF-8")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", cred.AccessToken))
	req.Header.Set("Chat-Id", randomChatID()) // 必须 32 位无横杠 UUID
	req.Header.Set("X-DevEco-Improvement-Enabled", fmt.Sprintf("%v", u.mgr.ReadImprovementEnabled()))
	req.Header.Set("lang", "en")
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

// CallStream 真流式调用 DevEco（stream:true，走 /chat/completions 端点）
func (u *DevEcoUpstream) CallStream(ctx context.Context, body []byte) (*StreamResponse, error) {
	cred, err := u.mgr.EnsureCreds()
	if err != nil {
		return nil, err
	}
	sr, err := u.doCallStream(ctx, body, cred)
	if err != nil {
		return nil, err
	}
	// token 失效 -> 刷新并重试一次（流式下纯 status 判断）
	if sr.StatusCode == 401 {
		fmt.Println("[switch-dev] DevEco 流式收到 401（token 失效），刷新并重试一次")
		sr.Body.Close()
		u.mgr.InvalidateCreds()
		oldToken := cred.AccessToken
		newCred, err := u.mgr.EnsureCreds()
		if err != nil {
			return sr, nil
		}
		if newCred.AccessToken != oldToken {
			return u.doCallStream(ctx, body, newCred)
		}
	}
	return sr, nil
}

// doCallStream 流式版 doCall：端点 /chat/completions（去掉 no-stream），200 返回 SSE 流
func (u *DevEcoUpstream) doCallStream(ctx context.Context, body []byte, cred *creds.DevEcoCred) (*StreamResponse, error) {
	// 流式端点：/sse/codeGenie/maas/v2/chat/completions（非流式是 /no-stream/chat/completions）
	url := fmt.Sprintf("%s%s/chat/completions",
		u.mgr.Config().Origin, u.mgr.Config().MaasPath)
	reqID := fmt.Sprintf("req-%d-%s", time.Now().Unix(), randString(6))

	// 覆盖 stream 字段为 true
	var m map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("解析请求 body 失败: %w", err)
	}
	m["stream"] = true
	m["stream_options"] = map[string]bool{"include_usage": true} // 让网关在最后一个 chunk 返回 usage
	bodyBytes, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("重新编码请求 body 失败: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json; charset=UTF-8")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", cred.AccessToken))
	req.Header.Set("Chat-Id", randomChatID())
	req.Header.Set("X-DevEco-Improvement-Enabled", fmt.Sprintf("%v", u.mgr.ReadImprovementEnabled()))
	req.Header.Set("lang", "en")
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

	// 200：peek 检测空流 + 非 SSE 内容，异常则虚拟 502 降级（含 peek 内容便于排查）
	br := bufio.NewReader(httpResp.Body)
	peeked, _ := br.Peek(256)
	peekStr := string(peeked)
	if len(peeked) == 0 {
		httpResp.Body.Close()
		fmt.Printf("[switch-dev] deveco 流式上游返回空流，降级\n")
		return &StreamResponse{
			StatusCode: 502,
			Body:       io.NopCloser(bytes.NewReader([]byte(`{"error":"upstream empty stream"}`))),
			ReqID:      reqID,
		}, nil
	}
	// SSE 流必含 data:；无 data: 视为非 SSE 错误页降级
	if !strings.Contains(peekStr, "data:") {
		httpResp.Body.Close()
		snippet := peekStr
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		fmt.Printf("[switch-dev] deveco 流式上游返回非 SSE 内容，降级: %s\n", snippet)
		errJSON, _ := json.Marshal(map[string]string{"error": "non-sse: " + snippet})
		return &StreamResponse{
			StatusCode: 502,
			Body:       io.NopCloser(bytes.NewReader(errJSON)),
			ReqID:      reqID,
		}, nil
	}
	// SSE 错误事件：event: error 且 data 为空时提前降级（有 data 内容的错误交给转换器处理）
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
			fmt.Printf("[switch-dev] deveco 流式上游 SSE error 事件（空 data），降级\n")
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

// isDevEcoTokenInvalid DevEco access token 失效判断
func isDevEcoTokenInvalid(resp *Response) bool {
	if resp.StatusCode == 401 {
		return true
	}
	var o struct {
		ErrorCode int `json:"errorCode"`
		Error     struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if creds.ParseJSONPublic(resp.Body, &o) == nil {
		if o.ErrorCode == 4016 {
			return true
		}
		if o.Error.Message != "" {
			msg := strings.ToLower(o.Error.Message)
			if strings.Contains(msg, "accesstoken") || strings.Contains(msg, "unauthor") {
				return true
			}
		}
	}
	return false
}

// randomChatID 生成 32 位无横杠 UUID（DevEco Chat-Id 要求）
func randomChatID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b) // 32 hex 字符
}