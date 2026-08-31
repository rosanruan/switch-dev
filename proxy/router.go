package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"switchdev/upstream"
)

// ModelRef 配置中的模型引用（类型别名，避免循环引用 config 包）
type ModelRef struct {
	Upstream string `json:"upstream"`
	Model    string `json:"model"`
}

// chainTimeout 降级链总超时（包含所有上游尝试；长上下文推理可能数分钟，需留足余量）
const chainTimeout = 600 * time.Second

// directCtxKey 直连模式上下文 key：测评等内部请求用，跳过降级链，只打指定模型
type directCtxKey struct{}

// WithDirect 返回带直连标记的 context
func WithDirect(ctx context.Context) context.Context {
	return context.WithValue(ctx, directCtxKey{}, true)
}

func isDirect(ctx context.Context) bool {
	v, _ := ctx.Value(directCtxKey{}).(bool)
	return v
}

// directChain 把请求模型解析为「仅自身」的单元素链（不附加任何降级/兜底）
func directChain(requestedModel string) []ModelRef {
	return []ModelRef{{
		Upstream: ResolveUpstream(requestedModel),
		Model:    ResolveModel(requestedModel),
	}}
}

// ConfigResolver 配置解析接口（Server 持有，避免直接依赖 config 包）
type ConfigResolver interface {
	Resolve(requestedModel string, userAgent string) []ModelRef
	GetMode() string
	GetAPIKey() string
	GetAuthEnabled() bool
	IsUpstreamEnabled(name string) bool
}

// isUpstreamEnabled 判断上游是否启用；未注入 resolver 时默认启用（防御）
func (s *Server) isUpstreamEnabled(name string) bool {
	if s.ConfigResolver == nil {
		return true
	}
	return s.ConfigResolver.IsUpstreamEnabled(name)
}

// callUpstreamAnthropic Anthropic 入口的上游分发（基于配置链遍历）
// 返回 (响应, 上游名, 实际用到的模型, 错误)
func (s *Server) callUpstreamAnthropic(ctx context.Context, body *AnthropicRequest, userAgent string) (*upstream.Response, string, string, error) {
	requestedModel := body.Model
	if requestedModel == "" {
		requestedModel = "auto"
	}
	chain := s.ConfigResolver.Resolve(requestedModel, userAgent)
	if isDirect(ctx) {
		chain = directChain(requestedModel)
	}
	ctx, cancel := context.WithTimeout(ctx, chainTimeout)
	defer cancel()
	return s.executeChain(ctx, body, chain, requestedModel)
}

// callUpstreamOpenAI OpenAI 直通入口的上游分发
func (s *Server) callUpstreamOpenAI(ctx context.Context, rawBody map[string]interface{}, userAgent string) (*upstream.Response, string, string, error) {
	requestedModel, _ := rawBody["model"].(string)
	if requestedModel == "" {
		requestedModel = "auto"
	}
	chain := s.ConfigResolver.Resolve(requestedModel, userAgent)
	if isDirect(ctx) {
		chain = directChain(requestedModel)
	}
	ctx, cancel := context.WithTimeout(ctx, chainTimeout)
	defer cancel()
	return s.executeChain(ctx, rawBody, chain, requestedModel)
}

// executeChain 遍历配置链，尝试每个模型引用，任意成功即返回
// 权重降级（仅针对 provider 模型）：healthy 的 provider 模型按配置顺序优先，
// unhealthy 的 provider 模型排到链尾最后尝试；内置 4 上游严格按配置顺序。
// 返回 (响应, 上游名, 实际用到的模型, 错误)
func (s *Server) executeChain(ctx context.Context, body interface{}, chain []ModelRef, requestedModel string) (*upstream.Response, string, string, error) {
	var lastErr error
	var lastUpstream string

	// 遍历顺序：第一轮 = 非-free 全部 + healthy free；第二轮 = unhealthy free（权重降级）
	ordered := make([]ModelRef, 0, len(chain))
	degraded := make([]ModelRef, 0, len(chain))
	for _, ref := range chain {
		// 只对 provider 模型做健康分组；内置 4 上游严格按配置顺序
		if IsProviderModel(ref.Model) {
			pid, mid := ParseProviderModel(ref.Model)
			if pid != "" && !IsProviderModelHealthy(pid, mid) {
				degraded = append(degraded, ref)
				continue
			}
		}
		ordered = append(ordered, ref)
	}
	ordered = append(ordered, degraded...)

	for _, ref := range ordered {
		up := s.pickUpstream(ref.Upstream)
		if up == nil {
			continue
		}

		// 上游被禁用的直接跳过（全局开关，不发起请求）
		if !s.isUpstreamEnabled(ref.Upstream) {
			fmt.Printf("[switch-dev] 跳过 %s/%s（上游已禁用）\n", ref.Upstream, ref.Model)
			s.recordSkipLog(requestedModel, ref, "upstream_disabled")
			continue
		}

		// 凭据无效的直接跳过（不发起请求）
		if !up.HasValidCreds() {
			fmt.Printf("[switch-dev] 跳过 %s/%s（凭据无效）\n", ref.Upstream, ref.Model)
			s.recordSkipLog(requestedModel, ref, "cred_invalid")
			continue
		}

		// 构造 OpenAI body
		oaiBytes, err := s.buildOpenAIBody(body, ref, false)
		if err != nil {
			lastErr = err
			lastUpstream = ref.Upstream
			continue
		}

		// 发起请求
		resp, err := up.Call(ctx, oaiBytes)
		if err != nil {
			fmt.Printf("[switch-dev] %s/%s 调用失败: %v，降级\n", ref.Upstream, ref.Model, err)
			lastErr = err
			lastUpstream = ref.Upstream
			s.recordFallbackLog(requestedModel, ref, "call_error", err.Error())
			continue
		}

		// 检查上游错误响应
		if isUpstreamErrorResponse(resp) {
			snippet := upstreamErrSnippet(resp)
			friendly := friendlyUpstreamError(ref.Upstream, resp)
			fmt.Printf("[switch-dev] %s/%s 上游错误 (status=%d %s)，降级\n", ref.Upstream, ref.Model, resp.StatusCode, snippet)
			lastErr = fmt.Errorf("%s", friendly)
			lastUpstream = ref.Upstream
			s.recordFallbackLog(requestedModel, ref, "upstream_error", friendly)
			continue
		}

		// 空内容过滤：200 但 body 为空，或解析后 choices/message 内容全空
		// （部分供应商对受限/异常请求返回 200 空响应）。当作失败降级到下一个模型，
		// 而不是把空响应当成功返回给客户端。
		if isEmptyUpstreamResponse(resp) {
			detail := "200 空内容（无 choices 或 message 全空）"
			fmt.Printf("[switch-dev] %s/%s 上游空内容，降级\n", ref.Upstream, ref.Model)
			lastErr = fmt.Errorf("upstream empty content")
			lastUpstream = ref.Upstream
			s.recordFallbackLog(requestedModel, ref, "empty_content", detail)
			continue
		}

		// 成功！返回实际用到的模型引用
		return resp, ref.Upstream, ref.Model, nil
	}

	if lastErr != nil {
		return nil, lastUpstream, "", lastErr
	}
	return nil, "", "", fmt.Errorf("配置链全失败，且无有效凭据的 upstream")
}

// executeChainStream 流式版 executeChain：只走支持 StreamCaller 的上游
// 返回 nil, "", "", nil 表示无流式上游可用（调用方回退伪流式）
// 返回 nil, up, "", err 表示有流式上游但全部失败
// 权重降级：仅对 provider 模型分组（healthy 优先，unhealthy 排链尾）
func (s *Server) executeChainStream(ctx context.Context, body interface{}, chain []ModelRef, requestedModel string) (*upstream.StreamResponse, string, string, error) {
	var lastErr error
	var lastUpstream string
	triedStream := false
	streamUnsupported := false // 上游明确不支持/拒绝流式（如虚拟 406 灰度），应让调用方回退非流式

	ordered := make([]ModelRef, 0, len(chain))
	degraded := make([]ModelRef, 0, len(chain))
	for _, ref := range chain {
		if IsProviderModel(ref.Model) {
			pid, mid := ParseProviderModel(ref.Model)
			if pid != "" && !IsProviderModelHealthy(pid, mid) {
				degraded = append(degraded, ref)
				continue
			}
		}
		ordered = append(ordered, ref)
	}
	ordered = append(ordered, degraded...)

	for _, ref := range ordered {
		up := s.pickUpstream(ref.Upstream)
		if up == nil {
			continue
		}
		// 上游被禁用的直接跳过（全局开关，不发起请求）
		if !s.isUpstreamEnabled(ref.Upstream) {
			fmt.Printf("[switch-dev] 跳过 %s/%s（上游已禁用）\n", ref.Upstream, ref.Model)
			s.recordSkipLog(requestedModel, ref, "upstream_disabled")
			continue
		}
		if !up.HasValidCreds() {
			fmt.Printf("[switch-dev] 跳过 %s/%s（凭据无效）\n", ref.Upstream, ref.Model)
			s.recordSkipLog(requestedModel, ref, "cred_invalid")
			continue
		}
		// 类型断言：只走支持真流式的上游（WorkBuddy/OpenCode）
		sc, ok := up.(upstream.StreamCaller)
		if !ok {
			continue // 不支持流式，跳过（JoyCode/DevEco 走伪流式）
		}
		triedStream = true

		oaiBytes, err := s.buildOpenAIBody(body, ref, true)
		if err != nil {
			lastErr = err
			lastUpstream = ref.Upstream
			continue
		}

		sr, err := sc.CallStream(ctx, oaiBytes)
		if err != nil {
			fmt.Printf("[switch-dev] %s/%s 流式调用失败: %v，降级\n", ref.Upstream, ref.Model, err)
			lastErr = err
			lastUpstream = ref.Upstream
			s.recordFallbackLog(requestedModel, ref, "call_error", err.Error())
			continue
		}

		if sr.StatusCode != 200 {
			errBody, _ := io.ReadAll(sr.Body)
			sr.Body.Close()
			snippet := string(errBody)
			if len(snippet) > 120 {
				snippet = snippet[:120]
			}
			// 虚拟 406：上游明确拒绝流式（如 JoyCode COLOR_FORWARD_EXCEPTION/AI_GRAY 灰度），
			// 不应继续找下一个流式上游，而是让 handler 回退到非流式 Call + 伪流式。
			if sr.StatusCode == 406 {
				fmt.Printf("[switch-dev] %s/%s 流式被上游拒绝（406），回退非流式: %s\n", ref.Upstream, ref.Model, snippet)
				streamUnsupported = true
				lastUpstream = ref.Upstream
				break
			}
			fmt.Printf("[switch-dev] %s/%s 流式上游错误 (status=%d %s)，降级\n", ref.Upstream, ref.Model, sr.StatusCode, snippet)
			lastErr = fmt.Errorf("%s", friendlyStreamError(ref.Upstream, snippet))
			lastUpstream = ref.Upstream
			s.recordFallbackLog(requestedModel, ref, "upstream_error", friendlyStreamError(ref.Upstream, snippet))
			continue
		}

		// 空内容流探测：部分上游（尤其免费/代理供应商）会返回 200 但只有 role 块 +
		// finish_reason、没有任何 content/reasoning/tool_calls。提前预读到第一个内容块，
		// 若整段为空则关掉这个流、降级到链里下一个模型，避免给客户端回 200 空消息。
		peeked, peekErr := peekSSEUntilContent(sr.Body, s.upstreamProtocol(ref))
		if peekErr != nil {
			reason := "empty_content"
			detail := peekErr.Error()
			if !errors.Is(peekErr, errSSEEmptyContent) {
				reason = "stream_error"
			}
			fmt.Printf("[switch-dev] %s/%s 流式%s: %s，降级\n", ref.Upstream, ref.Model, reason, detail)
			lastErr = peekErr
			lastUpstream = ref.Upstream
			s.recordFallbackLog(requestedModel, ref, reason, detail)
			continue
		}
		sr.Body = peeked

		return sr, ref.Upstream, ref.Model, nil
	}

	if !triedStream || streamUnsupported {
		return nil, "", "", nil // 无流式上游可用，或上游明确拒绝流式（灰度），回退伪流式
	}
	return nil, lastUpstream, "", lastErr // 有流式上游但全失败
}

// callUpstreamStream 流式上游分发（Anthropic/OpenAI 入口共用）
// 注意：返回的 StreamResponse.Body 关闭时才会 cancel context，
// 避免函数返回后 context 过早取消导致流读取失败。
func (s *Server) callUpstreamStream(ctx context.Context, body interface{}, userAgent string) (*upstream.StreamResponse, string, string, error) {
	var requestedModel string
	switch b := body.(type) {
	case *AnthropicRequest:
		requestedModel = b.Model
	case map[string]interface{}:
		requestedModel, _ = b["model"].(string)
	}
	if requestedModel == "" {
		requestedModel = "auto"
	}
	chain := s.ConfigResolver.Resolve(requestedModel, userAgent)
	if isDirect(ctx) {
		chain = directChain(requestedModel)
	}
	ctx, cancel := context.WithTimeout(ctx, chainTimeout)
	sr, upName, usedModel, err := s.executeChainStream(ctx, body, chain, requestedModel)
	if err != nil || sr == nil {
		cancel()
		return sr, upName, usedModel, err
	}
	// 流式：把 cancel 绑到 Body.Close，确保流读完/关掉时才 cancel context
	sr.Body = &cancelReadCloser{rc: sr.Body, cancel: cancel}
	return sr, upName, usedModel, nil
}

// pickUpstream 按 upstream 名获取适配器
// 内置 4 上游走 switch；provider API 供应商（provider id）走 ProviderAPIs map
func (s *Server) pickUpstream(name string) upstream.Upstream {
	switch name {
	case "joycode":
		return s.JoyCode
	case "deveco":
		return s.DevEco
	case "opencode":
		return s.OpenCode
	case "workbuddy":
		return s.WorkBuddy
	}
	// provider API 供应商（多供应商平级）
	if free := s.GetProviderAPI(name); free != nil {
		return free
	}
	return nil
}

// buildOpenAIBody 根据 body 类型和模型引用构造 OpenAI 请求 body
// stream 参数指示是否要流式调用（true=流式, false=非流式）。
func (s *Server) buildOpenAIBody(body interface{}, ref ModelRef, stream bool) ([]byte, error) {
	switch b := body.(type) {
	case *AnthropicRequest:
		return s.buildAnthropicOutboundBody(b, ref, stream)
	case map[string]interface{}:
		return s.buildOpenAIOutboundBody(b, ref, stream)
	}
	return nil, fmt.Errorf("unknown body type: %T", body)
}

// upstreamProtocolByName 按 upstream 名查其出站协议（"anthropic" 或 "openai"）
func (s *Server) upstreamProtocolByName(name string) string {
	if up := s.pickUpstream(name); up != nil {
		if pp, ok := up.(interface{ Protocol() string }); ok {
			if pp.Protocol() == "anthropic" {
				return "anthropic"
			}
		}
	}
	return "openai"
}

// upstreamProtocol 返回某个 ModelRef 对应上游的出站协议："anthropic" 或 "openai"（默认）。
// 内置 4 上游均为 OpenAI 协议（代理内部做 Anthropic->OpenAI 转换）；
// free 供应商按其配置的 protocol 决定。
func (s *Server) upstreamProtocol(ref ModelRef) string {
	if up := s.pickUpstream(ref.Upstream); up != nil {
		if pp, ok := up.(interface{ Protocol() string }); ok {
			if pp.Protocol() == "anthropic" {
				return "anthropic"
			}
		}
	}
	return "openai"
}

// buildAnthropicOutboundBody 处理入站 Anthropic(/v1/messages) 请求：
//   - 上游为 anthropic 协议：透传 Anthropic body（只改 model）
//   - 上游为 openai 协议：转成 OpenAI body
// stream 参数控制是否在请求中保留 stream:true。非流式时必须设为 false
// 以避免上游返回 SSE 而非 JSON 响应。
func (s *Server) buildAnthropicOutboundBody(body *AnthropicRequest, ref ModelRef, stream bool) ([]byte, error) {
	if s.upstreamProtocol(ref) == "anthropic" {
		cp := *body
		cp.Model = ref.Model
		if IsProviderModel(ref.Model) {
			cp.Model = stripProviderPrefix(ref.Model)
		}
		if !stream {
			cp.Stream = nil // 去掉 stream:true，让上游返回 JSON 而非 SSE
		}
		return json.Marshal(cp)
	}
	return s.buildAnthropicOpenAIBody(body, ref, stream)
}

// buildOpenAIOutboundBody 处理入站 OpenAI(/v1/chat/completions) 请求：
//   - 上游为 openai 协议：直通（注入 model）
//   - 上游为 anthropic 协议：转成 Anthropic body（一阶段暂不支持，返回明确错误；二阶段补转换器）
func (s *Server) buildOpenAIOutboundBody(body map[string]interface{}, ref ModelRef, stream bool) ([]byte, error) {
	if s.upstreamProtocol(ref) == "anthropic" {
		return nil, fmt.Errorf("该供应商仅支持 Anthropic 协议，请通过 /v1/messages 端点调用")
	}
	return s.buildOpenAIPassthroughBody(body, ref)
}

// buildAnthropicOpenAIBody Anthropic 请求 -> OpenAI body（按上游类型选择转换器）
func (s *Server) buildAnthropicOpenAIBody(body *AnthropicRequest, ref ModelRef, stream bool) ([]byte, error) {
	cp := *body
	cp.Model = ref.Model
	// provider API 模型：还原为原始 model id（去掉 provider/<provider>/ 前缀）
	if IsProviderModel(ref.Model) {
		cp.Model = stripProviderPrefix(ref.Model)
	}
	var oaiBody *OpenAIRequest
	switch ref.Upstream {
	case "joycode":
		oaiBody, _ = AnthropicToOpenAI(&cp)
	case "deveco":
		oaiBody = AnthropicToOpenAIDeveco(&cp)
	case "opencode":
		oaiBody = AnthropicToOpenAIOpencode(&cp)
	case "workbuddy":
		oaiBody = AnthropicToOpenAIWorkbuddy(&cp)
	default:
		// 免费 API 供应商：走通用 OpenAI 转换
		oaiBody, _ = AnthropicToOpenAI(&cp)
	}
	// 非流式调用：确保 stream=false，避免上游返回 SSE 而非 JSON
	if !stream {
		oaiBody.Stream = false
	}
	// free 供应商：强制把 model 改回剥离 provider 前缀后的真实上游 model id，
	// 否则 AnthropicToOpenAI 会用代理内部路由 id 导致上游 model_invalid。
	if IsProviderModel(ref.Model) {
		oaiBody.Model = stripProviderPrefix(ref.Model)
	}
	return json.Marshal(oaiBody)
}

// buildOpenAIPassthroughBody OpenAI 直通 body -> 注入 model + stream:false
// DevEco 需要把配置里的 model id 映射成网关认的 upstream 名
func (s *Server) buildOpenAIPassthroughBody(body map[string]interface{}, ref ModelRef) ([]byte, error) {
	cp := make(map[string]interface{}, len(body)+2)
	for k, v := range body {
		cp[k] = v
	}
	model := ref.Model
	if ref.Upstream == "deveco" {
		if dm := DevEcoModelByID[ref.Model]; dm != nil {
			model = dm.Upstream
		}
	} else if ref.Upstream == "workbuddy" {
		model = stripWbPrefix(ref.Model)
	} else if IsProviderModel(ref.Model) {
		// provider API 模型：还原为原始 model id
		model = stripProviderPrefix(ref.Model)
	}
	cp["model"] = model
	cp["stream"] = false
	return json.Marshal(cp)
}

// recordFallbackLog 记录降级日志
func (s *Server) recordFallbackLog(requestedModel string, ref ModelRef, reason, detail string) {
	entry := &LogEntry{
		Model:     requestedModel,
		Upstream:  ref.Upstream,
		Status:    "fallback",
		ErrorMsg:  fmt.Sprintf("[%s] %s", reason, detail),
	}
	s.recordLog(entry)
}

// recordSkipLog 记录跳过日志
func (s *Server) recordSkipLog(requestedModel string, ref ModelRef, reason string) {
	entry := &LogEntry{
		Model:     requestedModel,
		Upstream:  ref.Upstream,
		Status:    "fallback",
		ErrorMsg:  fmt.Sprintf("[%s] 跳过（凭据无效）", reason),
	}
	s.recordLog(entry)
}

// nowMillis / incRequests / timestampNow 辅助
var _counter int64

func nowMillis() int64                   { return atomic.AddInt64(&_counter, 1) }
func (s *Server) incRequests() int64     { return atomic.AddInt64(&s.requests, 1) }

// cancelReadCloser 包装 io.ReadCloser，在 Close 时同时调用 cancel 释放 context
type cancelReadCloser struct {
	rc     io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelReadCloser) Read(p []byte) (int, error) { return c.rc.Read(p) }
func (c *cancelReadCloser) Close() error {
	err := c.rc.Close()
	c.cancel()
	return err
}
func timestampNow() string               { return time.Now().Format("15:04:05") }

// ====== 以下来自旧 router.go（被 handlers.go 引用） ======

// isUpstreamErrorResponse 判断上游响应是否为错误响应
func isUpstreamErrorResponse(resp *upstream.Response) bool {
	trimmed := strings.TrimSpace(string(resp.Body))
	if trimmed == "" {
		return resp.StatusCode >= 400
	}
	var o map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &o); err != nil {
		return resp.StatusCode >= 400
	}
	if choices, ok := o["choices"]; ok {
		// choices 存在但为空数组 → 上游返回了不完整响应，视为错误
		if arr, ok := choices.([]interface{}); ok && len(arr) == 0 {
			return true
		}
		return false
	}
	_, hasError := o["error"]
	_, hasCode := o["code"]
	_, hasErrorCode := o["errorCode"]
	return hasError || hasCode || hasErrorCode
}

// sensitiveFieldRe 匹配错误响应中可能回显的敏感字段（apiKey/token/secret/password/authorization 等），
// 引号内的字符串值统一替换为 "***"。大小写不敏感。
var sensitiveFieldRe = regexp.MustCompile(`(?i)("?(?:api[_-]?key|access[_-]?token|refresh[_-]?token|id[_-]?token|token|secret|password|passwd|authorization|x-api-key|client[_-]?secret)"?\s*[:=]\s*")([^"]*)(")`)

// bearerTokenRe 匹配 "Bearer <token>" 形式（HTTP 头或错误信息里回显的）
var bearerTokenRe = regexp.MustCompile(`(?i)(Bearer\s+)[A-Za-z0-9._\-]+`)

// redactSensitive 把文本中的敏感字段值与 Bearer token 掩码，避免密钥片段进日志。
func redactSensitive(s string) string {
	s = sensitiveFieldRe.ReplaceAllString(s, "${1}***${3}")
	s = bearerTokenRe.ReplaceAllString(s, "${1}***")
	return s
}

// upstreamErrSnippet 取上游错误响应（先脱敏）的前 120 字符，用于降级日志
func upstreamErrSnippet(resp *upstream.Response) string {
	trimmed := strings.TrimSpace(redactSensitive(string(resp.Body)))
	if len(trimmed) > 120 {
		return trimmed[:120]
	}
	return trimmed
}

// friendlyUpstreamError 把已知上游错误转成可操作的友好文案；
// 无法识别时回退到 snippet（脱敏截断）。up 为上游名（如 joycode），便于给出针对性提示。
func friendlyUpstreamError(up string, resp *upstream.Response) string {
	body := string(resp.Body)
	// JoyCode 灰度门禁：账号未被开通该模型的独立访问。官方客户端在线时持有活跃会话
	// 才能通过灰度；代理用静态 ptKey 游离调用被拒。这不是请求构造 bug，是账号/会话门禁。
	if strings.Contains(body, "AI_GRAY_ACCESS_DENIED") ||
		strings.Contains(body, "COLOR_FORWARD_EXCEPTION") ||
		strings.Contains(body, "AI_GRAY_") {
		return fmt.Sprintf(
			"%s 当前账号未被开通该模型的独立访问（京东灰度门禁 AI_GRAY_ACCESS_DENIED）。"+
				"请保持 JoyCode 官方客户端打开并登录后重试，或在 JoyCode 管理后台为该账号开通此模型权限。"+
				"已自动尝试降级链中的下一个上游。",
			upstreamDisplayName(up),
		)
	}
	return upstreamErrSnippet(resp)
}

// upstreamDisplayName 上游友好名（错误文案用）
func upstreamDisplayName(up string) string {
	switch up {
	case "joycode":
		return "JoyCode"
	case "deveco":
		return "DevEco"
	case "opencode":
		return "OpenCode"
	case "workbuddy":
		return "WorkBuddy"
	}
	return up
}

// friendlyStreamError 流式路径的友好化（输入为已截断的错误文本 snippet）
func friendlyStreamError(up, snippet string) string {
	if strings.Contains(snippet, "AI_GRAY_ACCESS_DENIED") ||
		strings.Contains(snippet, "COLOR_FORWARD_EXCEPTION") ||
		strings.Contains(snippet, "AI_GRAY_") {
		return fmt.Sprintf(
			"%s 当前账号未被开通该模型的独立访问（京东灰度门禁 AI_GRAY_ACCESS_DENIED）。"+
				"请保持 JoyCode 官方客户端打开并登录后重试，或在 JoyCode 管理后台为该账号开通此模型权限。",
			upstreamDisplayName(up),
		)
	}
	return snippet
}

// isEmptyUpstreamResponse 判断 200 响应是否为空内容：body 空，或能解析为 OpenAI JSON
// 但 choices 缺失/为空，或 message 无 text/tool_calls/reasoning。
// 错误响应（含 error/code 字段）不算空，交由错误处理路径判定。
func isEmptyUpstreamResponse(resp *upstream.Response) bool {
	trimmed := strings.TrimSpace(string(resp.Body))
	if trimmed == "" {
		return true
	}
	var oai OpenAIResponse
	if err := json.Unmarshal([]byte(trimmed), &oai); err != nil {
		// 不是 JSON：可能是非标准响应，不当空（让上层按 unparsable 处理）
		return false
	}
	// 明确是错误体的不算空
	var probe map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &probe); err == nil {
		if _, ok := probe["error"]; ok {
			return false
		}
		// Anthropic 成功响应（含 content/stop_reason/type=message）不算空
		if probe["type"] == "message" || probe["content"] != nil || probe["stop_reason"] != nil {
			// content 为非空数组才认为有内容
			if arr, ok := probe["content"].([]interface{}); ok {
				return len(arr) == 0
			}
			return false
		}
	}
	return isResponseContentEmpty(&oai)
}

// extractUpstreamError 从错误响应体提取 msg + code
func extractUpstreamError(body []byte) (msg, code string) {
	var o map[string]interface{}
	if err := json.Unmarshal(body, &o); err != nil {
		return "upstream error", "upstream_error"
	}
	if e, ok := o["error"].(map[string]interface{}); ok {
		if m, ok := e["message"].(string); ok {
			msg = m
		}
		if c, ok := e["code"]; ok {
			code = fmt.Sprint(c)
		}
	}
	if msg == "" {
		if e, ok := o["echo"].(string); ok {
			msg = e
		}
	}
	if msg == "" {
		if m, ok := o["msg"].(string); ok {
			msg = m
		}
	}
	if code == "" {
		if c, ok := o["code"]; ok {
			code = fmt.Sprint(c)
		}
	}
	if code == "" {
		if c, ok := o["errorCode"]; ok {
			code = fmt.Sprint(c)
		}
	}
	if msg == "" {
		msg = "upstream error"
	}
	if code == "" {
		code = "upstream_error"
	}
	return msg, code
}

// clampMaxTokensFromMap 从 map 里取 max_tokens，钳制后返回
func clampMaxTokensFromMap(raw interface{}, limit int) int {
	v := 0
	switch n := raw.(type) {
	case float64:
		v = int(n)
	case int:
		v = n
	case int64:
		v = int(n)
	}
	return ClampMaxTokens(v, limit)
}