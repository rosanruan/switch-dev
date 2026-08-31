package proxy

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"switchdev/pricing"
	"switchdev/upstream"
)

// EventLogger 事件日志接口（由 service 层实现，用于记录日志 + 推送 Wails 事件）
type EventLogger interface {
	RecordLog(entry *LogEntry)
	EmitEvent(event string, data interface{})
}

// Server 代理 HTTP 服务
type Server struct {
	JoyCode   *upstream.JoyCodeUpstream
	DevEco    *upstream.DevEcoUpstream
	OpenCode  *upstream.OpenCodeUpstream
	WorkBuddy *upstream.WorkBuddyUpstream

	// 免费 API 上游（动态注册，多供应商平级；key = provider id）
	providerAPIsMu sync.RWMutex
	ProviderAPIs   map[string]upstream.Upstream

	Logger         EventLogger
	ConfigResolver ConfigResolver   // ★ 配置解析器（由 main 注入）
	Pricing        *pricing.Manager // ★ 费率管理器（由 main 注入）
	httpSrv        *http.Server
	Host           string
	Port           int
	requests       int64
	running        atomic.Bool
}

// NewServer 创建代理服务
func NewServer(jy *upstream.JoyCodeUpstream, de *upstream.DevEcoUpstream, oc *upstream.OpenCodeUpstream, wb *upstream.WorkBuddyUpstream, host string, port int) *Server {
	return &Server{
		JoyCode:      jy,
		DevEco:       de,
		OpenCode:     oc,
		WorkBuddy:    wb,
		ProviderAPIs: map[string]upstream.Upstream{},
		Host:         host,
		Port:         port,
	}
}

// RegisterProviderAPI 注册免费 API 上游（provider id -> upstream）
func (s *Server) RegisterProviderAPI(id string, up upstream.Upstream) {
	s.providerAPIsMu.Lock()
	defer s.providerAPIsMu.Unlock()
	s.ProviderAPIs[id] = up
}

// RemoveProviderAPI 注销免费 API 上游
func (s *Server) RemoveProviderAPI(id string) {
	s.providerAPIsMu.Lock()
	defer s.providerAPIsMu.Unlock()
	delete(s.ProviderAPIs, id)
}

// GetProviderAPI 按 provider id 取免费 API 上游
func (s *Server) GetProviderAPI(id string) upstream.Upstream {
	s.providerAPIsMu.RLock()
	defer s.providerAPIsMu.RUnlock()
	return s.ProviderAPIs[id]
}

// GetProviderAPIs 返回所有免费 API 上游（key = provider id）
func (s *Server) GetProviderAPIs() map[string]upstream.Upstream {
	s.providerAPIsMu.RLock()
	defer s.providerAPIsMu.RUnlock()
	out := make(map[string]upstream.Upstream, len(s.ProviderAPIs))
	for k, v := range s.ProviderAPIs {
		out[k] = v
	}
	return out
}

// Start 启动 HTTP 服务（非阻塞）
func (s *Server) Start() error {
	if s.running.Load() {
		return fmt.Errorf("代理已在运行")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleDispatch)

	s.httpSrv = &http.Server{
		Addr:              fmt.Sprintf("%s:%d", s.Host, s.Port),
		Handler:           mux,
		ReadTimeout:       30 * time.Second, // 读取请求体（含长上下文）的上限
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      0, // 禁用：SSE 流式响应可能持续数分钟，Go 的 WriteTimeout 会强杀长连接导致客户端收到截断响应
		IdleTimeout:       120 * time.Second,
	}

	ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", s.Host, s.Port))
	if err != nil {
		return fmt.Errorf("端口 %d 监听失败: %w", s.Port, err)
	}

	s.running.Store(true)
	s.emitStatus()
	go func() {
		if err := s.httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			fmt.Printf("[switch-dev] HTTP 服务退出: %v\n", err)
		}
		s.running.Store(false)
		s.emitStatus()
	}()
	return nil
}

// Stop 停止 HTTP 服务
func (s *Server) Stop() error {
	if !s.running.Load() {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := s.httpSrv.Shutdown(ctx)
	s.running.Store(false)
	s.emitStatus()
	return err
}

// StopQuiet 停止 HTTP 服务但不推送状态事件（退出场景用，避免 Event.Emit 在 app 退出时卡死）
func (s *Server) StopQuiet() error {
	if !s.running.Load() {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := s.httpSrv.Shutdown(ctx)
	s.running.Store(false)
	return err
}

// IsRunning 是否运行中
func (s *Server) IsRunning() bool { return s.running.Load() }

// GetStatus 返回代理状态
func (s *Server) GetStatus() *ProxyStatus {
	mode := "auto"
	if s.ConfigResolver != nil {
		mode = s.ConfigResolver.GetMode()
	}
	return &ProxyStatus{
		Running:  s.running.Load(),
		Port:     s.Port,
		Host:     s.Host,
		Mode:     mode,
		Requests: atomic.LoadInt64(&s.requests),
	}
}

// emitStatus 推送代理状态变化事件
func (s *Server) emitStatus() {
	if s.Logger != nil {
		s.Logger.EmitEvent("proxy:status", s.GetStatus())
	}
}

// handleDispatch 统一入口分发
func (s *Server) handleDispatch(w http.ResponseWriter, r *http.Request) {
	// CORS
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "*")
	w.Header().Set("Access-Control-Allow-Methods", "*")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// 鉴权：除健康检查外，校验客户端 apiKey（用户关闭鉴权时放行）
	if r.URL.Path != "/" && r.URL.Path != "/health" {
		if s.ConfigResolver.GetAuthEnabled() && !s.checkAPIKey(r) {
			s.handleAuthFailure(w, r)
			return
		}
	}

	// X-Switch-Direct:1 为内部测评直连模式（只打请求指定的模型，跳过降级链/兜底）
	if r.Header.Get("X-Switch-Direct") == "1" {
		r = r.WithContext(WithDirect(r.Context()))
	}

	switch {
	case r.Method == http.MethodGet && (r.URL.Path == "/" || r.URL.Path == "/health"):
		s.handleHealth(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
		s.handleModels(w, r)
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/messages"):
		s.handleAnthropicMessages(w, r)
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/chat/completions"):
		s.handleOpenAIChatCompletions(w, r)
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/responses"):
		s.handleResponses(w, r)
	default:
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte("not found"))
	}
}

// checkAPIKey 校验请求的 apiKey（支持 x-api-key 和 Authorization: Bearer）
// 用恒定时间比较防时序攻击
func (s *Server) checkAPIKey(r *http.Request) bool {
	if s.ConfigResolver == nil {
		return true
	}
	expected := s.ConfigResolver.GetAPIKey()
	if expected == "" {
		return true // 未配置 key 时放行（不应发生，Load 保证非空）
	}
	clientKey := r.Header.Get("x-api-key")
	if clientKey == "" {
		if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
			clientKey = strings.TrimPrefix(auth, "Bearer ")
		}
	}
	return subtle.ConstantTimeCompare([]byte(clientKey), []byte(expected)) == 1
}

// handleAuthFailure 处理网关入站鉴权失败：记一条可观测日志（含 UA + 脱敏后的收到 key），
// 再返回可操作文案——明确提示「应填 Switch Dev 的 rs- 接入密钥，而非第三方供应商 Key」。
// 注意：这是网关自己的鉴权（校验 Claude Code/Codex 等客户端带进来的 key），与上游供应商的 401 无关。
func (s *Server) handleAuthFailure(w http.ResponseWriter, r *http.Request) {
	received := receivedClientKey(r)
	// 记日志：reason 固定，便于在日志页/SQLite 里筛出「客户端 key 不匹配」类问题。
	s.recordLog(&LogEntry{
		Status:   "auth_error",
		Code:     http.StatusUnauthorized,
		Source:   sourceFromUA(r.Header.Get("User-Agent")),
		Upstream: "gateway",
		Method:   r.Method,
		Path:     r.URL.Path,
		ErrorMsg: fmt.Sprintf("网关鉴权失败：客户端 key 不匹配（收到 %s）。请把 ANTHROPIC_API_KEY 设为 Switch Dev「设置 → 接入 apiKey」里的 rs- 密钥，而不是第三方供应商 Key；如在设置页重新生成过 apiKey，客户端需同步更新", maskKeyForLog(received)),
	})
	msg := "invalid api key: 请把客户端（如 ANTHROPIC_API_KEY）设为 Switch Dev「设置 → 接入 apiKey」显示的 rs- 密钥，而不是第三方供应商 Key；网关本身未联系上游"
	if strings.HasPrefix(r.URL.Path, "/v1/chat/completions") {
		writeOpenAIError(w, http.StatusUnauthorized, msg)
		return
	}
	writeAnthropicError(w, http.StatusUnauthorized, "authentication_error", msg)
}

// receivedClientKey 从请求里取出客户端带来的 key（x-api-key 优先，其次 Authorization: Bearer）。
func receivedClientKey(r *http.Request) string {
	if k := r.Header.Get("x-api-key"); k != "" {
		return k
	}
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	return ""
}

// maskKeyForLog 脱敏收到的 key：保留前 4 位 + 标注长度，不写明文。
// 空 key 单独标注，便于区分「没带 key」和「带错 key」。
func maskKeyForLog(k string) string {
	if k == "" {
		return "<empty>"
	}
	if len(k) <= 4 {
		return fmt.Sprintf("****(len=%d)", len(k))
	}
	return fmt.Sprintf("%s****(len=%d)", k[:4], len(k))
}

// handleHealth 健康检查（显示三上游凭据状态）
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	jcStatus := s.JoyCode.CredStatus()
	deStatus := s.DevEco.CredStatus()
	ocStatus := s.OpenCode.CredStatus()
	wbStatus := s.WorkBuddy.CredStatus()

	resp := map[string]interface{}{
		"ok":                 jcStatus.Valid || deStatus.Valid || ocStatus.Valid || wbStatus.Valid,
		"service":            "switch-dev",
		"joycodeCredValid":   jcStatus.Valid,
		"joycodeUserId":      jcStatus.UserID,
		"devecoCredValid":    deStatus.Valid,
		"devecoTokenExpiry":  deStatus.ExpiresAt,
		"opencodeCredValid":  ocStatus.Valid,
		"workbuddyCredValid": wbStatus.Valid,
		"workbuddyUserId":    wbStatus.UserID,
	}

	// 免费 API 凭据状态（动态）
	providerCreds := map[string]interface{}{}
	anyProviderValid := false
	for pid, up := range s.GetProviderAPIs() {
		cs := up.CredStatus()
		providerCreds[pid] = map[string]interface{}{
			"valid":   cs.Valid,
			"preview": cs.KeyPreview,
		}
		if cs.Valid {
			anyProviderValid = true
		}
	}
	resp["providerAPIs"] = providerCreds
	if len(providerCreds) > 0 {
		resp["ok"] = jcStatus.Valid || deStatus.Valid || ocStatus.Valid || wbStatus.Valid || anyProviderValid
	}

	json.NewEncoder(w).Encode(resp)
}

// handleModels 模型列表
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	var data []ModelInfo

	// auto 虚拟模型
	data = append(data, ModelInfo{
		ID: "auto", Object: "model", Created: 1700000000, OwnedBy: "multi",
		Label:  "Auto（DevEco GLM-5.1，失败降级 JoyCode）",
		Stream: true, Upstream: "deveco",
	})

	// OpenCode Zen 模型
	for _, m := range OpenCodeModels {
		data = append(data, ModelInfo{
			ID: m.ID, Object: "model", Created: 1700000000, OwnedBy: "opencode",
			Label: m.Label, Stream: true, Upstream: "opencode",
			Context: m.Context, Output: m.Output, ToolCall: true,
		})
	}

	// DevEco 模型
	for _, m := range DevEcoModels {
		data = append(data, ModelInfo{
			ID: m.ID, Object: "model", Created: 1700000000, OwnedBy: "huawei",
			Label: m.Label, Stream: true, Upstream: "deveco",
			Context: m.Context, Output: m.Output, ToolCall: true,
		})
	}

	// JoyCode 模型
	for _, m := range JoyCodeModels {
		data = append(data, ModelInfo{
			ID: m.ID, Object: "model", Created: 1700000000, OwnedBy: "jd",
			Label: m.Label, Stream: m.Stream, Upstream: "joycode", ToolCall: true,
		})
	}

	// WorkBuddy 模型
	for _, m := range WorkBuddyModels {
		data = append(data, ModelInfo{
			ID: m.ID, Object: "model", Created: 1700000000, OwnedBy: "tencent",
			Label: m.Label, Stream: true, Upstream: "workbuddy",
			Context: m.Context, Output: m.Output, Vision: m.Vision, ToolCall: m.ToolCall,
		})
	}

	// 供应商模型（动态注册的 verified 模型）
	for _, m := range ProviderModels {
		data = append(data, ModelInfo{
			ID: m.InternalID, Object: "model", Created: 1700000000,
			OwnedBy: m.ProviderID,
			Label:   m.Label, Stream: true, Upstream: m.ProviderID,
			Context: m.Context, Free: true,
		})
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"object": "list",
		"data":   data,
	})
}

// recordLog 记录请求日志（entry 由 handlers 填充业务字段，server 补 ID/时间戳）
func (s *Server) recordLog(entry *LogEntry) {
	atomic.AddInt64(&s.requests, 1)
	if s.Logger == nil {
		return
	}
	now := time.Now()
	if entry.ID == "" {
		entry.ID = fmt.Sprintf("log-%d-%d", now.UnixNano(), atomic.LoadInt64(&s.requests))
	}
	if entry.Timestamp == "" {
		entry.Timestamp = now.Format("15:04:05")
	}
	entry.DateTime = now.Format("2006-01-02 15:04:05")
	entry.Date = now.Format("2006-01-02")
	s.Logger.RecordLog(entry)
}
