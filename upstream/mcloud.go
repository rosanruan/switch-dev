package upstream

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"switchdev/creds"
)

// ====== 常量（逆向自 139 邮箱 webai 前端，对齐官方客户端指纹） ======

const (
	mcloudSourceChannel = "10112"                              // 移动云盘内嵌 webai 渠道号
	mcloudClientInfo    = "||13|1.7.0||||||1512/982|zh-CN||||" // clientType 13 = mcloud macOS 客户端
	mcloudPCVersion     = "1.7.0"                              // versionInfo.pcVersion
	mcloudH5Version     = "3.2.0"                              // versionInfo.h5Version
	mcloudCodeOK        = "0000"                               // 成功码
	mcloudCodeAuthFail  = "01000002"                           // 鉴权失败（webai 登录态切换的过渡态，需重读凭据重试）
	mcloudIdleTimeout   = 120 * time.Second                    // 流卡死看门狗（对齐 webai 首轮 connectionTimeout）
)

// mcloudMaxInput 各模型 maxInputLength（字符，来自 chat/config/get 实测 2026-11-09）。
// 折叠后的 dialogue 超限时丢弃最早轮次；未知模型按保守值 40000。
var mcloudMaxInput = map[string]int{
	"blian_qwen38_max":                     800000,
	"blian_qwen38_flash":                   800000,
	"blian_qwen37_plus":                    800000,
	"blian_qwen36_plus":                    800000,
	"blian_qwen35_plus":                    800000,
	"huoshan_glm_5_3":                      40000,
	"blian_deepseek_v4_pro":                40000,
	"blian_deepseek_v4_flash":              40000,
	"jiutian":                              9000,
	"vlm_huoshan_2_0_lite_thinking_vision": 40000,
}

func mcloudInputLimit(modelType string) int {
	if n, ok := mcloudMaxInput[modelType]; ok {
		return n
	}
	return 40000
}

// ====== 错误类型（供重试分类） ======

// mcloudAuthError 鉴权失败（HTTP 401 或业务码 01000002）。
// 01000002 是 webai 登录态切换的过渡态：应重读 accountdb.db 重拼 Basic 头重试，
// 而非直接判定凭据失效（文档坑点 3）。
type mcloudAuthError struct{ detail string }

func (e *mcloudAuthError) Error() string { return "mcloud 鉴权失败: " + e.detail }

// mcloudModelMismatchError 服务端实际服务的模型与目标不一致
// （账号偏好被外部改动 —— 如用户在 App 里手动切了模型）
type mcloudModelMismatchError struct{ got, want string }

func (e *mcloudModelMismatchError) Error() string {
	return fmt.Sprintf("mcloud 模型不一致: 服务端 %s ≠ 目标 %s", e.got, e.want)
}

// ====== 上游实现 ======

// MCloudUpstream 中国移动云盘（灵犀 / 139 邮箱 webai）适配器。
// 上游协议为自有 dialogueInput/flowResult（非 OpenAI）：请求经折叠转换发出，
// 响应 SSE 帧转换为标准 OpenAI SSE（CallStream）或聚合为 OpenAIResponse JSON（Call）。
// 模型选择是服务端账号级偏好（chat 请求不带模型字段），由 modelMu 串行化
// 「切模型 → 服务端绑定」窗口；锁在收到首个 SSE 数据帧后释放，不锁整条流。
type MCloudUpstream struct {
	mgr          *creds.MCloudCredManager
	client       *http.Client // config set / config get 等短请求
	streamClient *http.Client // chat SSE：无整体超时，卡死由 idleTimeoutReader 兜底

	modelMu      sync.Mutex // 串行化「切模型 → 服务端绑定」窗口
	currentModel string     // 最近一次成功 set 的 modelType
}

func NewMCloudUpstream(mgr *creds.MCloudCredManager) *MCloudUpstream {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 60 * time.Second
	return &MCloudUpstream{
		mgr:          mgr,
		client:       &http.Client{Timeout: 30 * time.Second},
		streamClient: &http.Client{Transport: transport},
	}
}

func (u *MCloudUpstream) Name() string { return "mcloud" }

func (u *MCloudUpstream) EnsureCreds(ctx context.Context) error {
	_, err := u.mgr.EnsureCreds()
	return err
}

func (u *MCloudUpstream) InvalidateCreds() { u.mgr.InvalidateCreds() }

func (u *MCloudUpstream) CredStatus() *creds.CredStatusInfo { return u.mgr.CredStatus() }

func (u *MCloudUpstream) HasValidCreds() bool {
	cred := u.mgr.GetCred()
	return cred != nil && cred.Valid && !cred.IsExpired() && cred.RCSToken != ""
}

// ====== 请求头 ======

// mcloudHeaders 拼装 x-yun-* 头族（对齐 webai getHeaderAiChat；漏头会被风控识别）
func mcloudHeaders(basic, apiVersion string) map[string]string {
	return map[string]string{
		"Authorization":     basic,
		"Content-Type":      "application/json",
		"x-yun-api-version": apiVersion,
		"x-yun-app-channel": mcloudSourceChannel,
		"x-yun-client-info": mcloudClientInfo,
		"x-yun-tid":         uuid.NewString(),
	}
}

// ====== 对话折叠（OpenAI messages → dialogueInput.dialogue） ======

type mcOpenAIMessage struct {
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	ToolCalls []struct {
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
}

// mcContentText 宽松提取消息文本：content 为 string 或分段数组（取 text 段）。
// 灵犀不支持图片输入，图片段以占位符保留结构。
func mcContentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		for _, p := range parts {
			switch p.Type {
			case "text":
				b.WriteString(p.Text)
			case "image_url":
				b.WriteString("[图片内容不支持]")
			}
		}
		return b.String()
	}
	return string(raw)
}

// foldMCloudDialogue 把 OpenAI messages 折叠成单个 dialogue 文本。
// 无状态方案：每轮新会话（sessionId 空），全量历史以转写形式传入，
// 免维护 sessionId 映射（无过期 / 无 GC / 重启安全）。
// 超过模型 maxInputLength 时从最早轮次开始丢弃（system 保留）。
func foldMCloudDialogue(body []byte) (dialogue, modelType string, err error) {
	var req struct {
		Model    string            `json:"model"`
		Messages []mcOpenAIMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return "", "", fmt.Errorf("解析请求 body 失败: %w", err)
	}
	modelType = req.Model
	// 入口路径兜底：OpenAI 直通已在路由层剥过 mc/，Anthropic 转换器也剥；
	// 这里再剥一次（幂等），保证任何路径都不会把内部路由 id 当 modelType 发出去。
	modelType = strings.TrimPrefix(modelType, "mc/")
	if modelType == "" {
		return "", "", fmt.Errorf("mcloud 请求缺少 model 字段")
	}

	var system []string
	type turn struct{ label, text string }
	var turns []turn
	for _, m := range req.Messages {
		text := mcContentText(m.Content)
		switch m.Role {
		case "system":
			if text != "" {
				system = append(system, text)
			}
		case "user":
			turns = append(turns, turn{"User", text})
		case "assistant":
			var b strings.Builder
			b.WriteString(text)
			for _, tc := range m.ToolCalls {
				if b.Len() > 0 {
					b.WriteString("\n")
				}
				fmt.Fprintf(&b, "[调用工具 %s(%s)]", tc.Function.Name, tc.Function.Arguments)
			}
			turns = append(turns, turn{"Assistant", b.String()})
		case "tool":
			turns = append(turns, turn{"Tool result", text})
		default:
			if text != "" {
				turns = append(turns, turn{m.Role, text})
			}
		}
	}

	sysBlock := ""
	if len(system) > 0 {
		sysBlock = strings.Join(system, "\n\n") + "\n\n"
	}
	const truncMarker = "（早期对话因超过模型输入上限已截断）\n\n"

	limit := mcloudInputLimit(modelType)
	if len(sysBlock) > limit {
		return "", "", fmt.Errorf("对话超过 %s 模型输入上限（%d 字符），请换用长上下文模型（如 mc/blian_qwen38_max）", modelType, limit)
	}

	// 从最新往前收纳，放不下的早期轮次丢弃
	var keptRev []string
	total := 0
	for i := len(turns) - 1; i >= 0; i-- {
		s := turns[i].label + ": " + turns[i].text
		if total+len(s)+2 > limit-len(sysBlock)-len(truncMarker) && len(keptRev) > 0 {
			break
		}
		keptRev = append(keptRev, s)
		total += len(s) + 2
	}

	var b strings.Builder
	b.WriteString(sysBlock)
	if len(keptRev) < len(turns) {
		b.WriteString(truncMarker)
	}
	for i := len(keptRev) - 1; i >= 0; i-- {
		b.WriteString(keptRev[i])
		b.WriteString("\n\n")
	}

	dialogue = strings.TrimRight(b.String(), "\n")
	if dialogue == "" {
		return "", "", fmt.Errorf("mcloud 请求折叠后对话为空")
	}
	return dialogue, modelType, nil
}

// ====== 上游请求/响应结构 ======

type mcloudCommand struct {
	Command string `json:"command"`
}

type mcloudVersionInfo struct {
	PCVersion string `json:"pcVersion"`
	H5Version string `json:"h5Version"`
}

type mcloudToolSetting struct {
	ImageToolSetting struct {
		EnableLLMDescribe bool `json:"enableLlmDescribe"`
	} `json:"imageToolSetting"`
}

type mcloudDialogueInput struct {
	Dialogue                 string            `json:"dialogue"`
	Prompt                   string            `json:"prompt"`
	InputTime                string            `json:"inputTime"`
	Commands                 string            `json:"commands"`
	Command                  mcloudCommand     `json:"command"`
	ResourceType             string            `json:"resourceType"`
	ResourceID               string            `json:"resourceId"`
	DialogueType             string            `json:"dialogueType"`
	CommandType              int               `json:"commandType"`
	EnableForceNetworkSearch bool              `json:"enableForceNetworkSearch"`
	EnableAllNetworkSearch   bool              `json:"enableAllNetworkSearch"`
	EnableAiSearch           bool              `json:"enableAiSearch"`
	EnableModelThinking      bool              `json:"enableModelThinking"`
	ExtInfo                  string            `json:"extInfo"`
	VersionInfo              mcloudVersionInfo `json:"versionInfo"`
	ToolSetting              mcloudToolSetting `json:"toolSetting"`
	Attachment               struct{}          `json:"attachment"`
	AIWritingSetting         struct{}          `json:"aiWritingSetting"`
}

type mcloudChatRequest struct {
	ApplicationType  string              `json:"applicationType"`
	DialogueInput    mcloudDialogueInput `json:"dialogueInput"`
	SourceChannel    string              `json:"sourceChannel"`
	UserID           string              `json:"userId"`
	ContinuationInfo any                 `json:"continuationInfo"`
}

func buildMCloudChatBody(dialogue, userID string) ([]byte, error) {
	req := mcloudChatRequest{
		ApplicationType: "chat",
		DialogueInput: mcloudDialogueInput{
			Dialogue:     dialogue,
			InputTime:    time.Now().Format("2006-01-02T15:04:05+08:00"),
			Commands:     "000",
			Command:      mcloudCommand{Command: "000"},
			ResourceType: "0",
			DialogueType: "0",
			CommandType:  1,
			ExtInfo:      "{}",
			VersionInfo:  mcloudVersionInfo{PCVersion: mcloudPCVersion, H5Version: mcloudH5Version},
		},
		SourceChannel: mcloudSourceChannel,
		UserID:        userID,
	}
	return json.Marshal(req)
}

// mcloudSSEFrame 上游 SSE 数据帧（event:AI-chat-sse-simple 的 data JSON）
type mcloudSSEFrame struct {
	Success bool   `json:"success"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Data    struct {
		SessionID  string `json:"sessionId"`
		DialogueID string `json:"dialogueId"`
		FlowResult *struct {
			Index            int    `json:"index"`
			ResultType       int    `json:"resultType"`
			OutContent       string `json:"outContent"`
			ReasoningContent string `json:"reasoningContent"`
			ModelType        string `json:"modelType"`
			ModelName        string `json:"modelName"`
			FinishReason     string `json:"finishReason"`
		} `json:"flowResult"`
		FinishReason string `json:"finishReason"`
	} `json:"data"`
}

// ====== SSE → OpenAI SSE 转换器 ======

type mcStreamDelta struct {
	Role             string `json:"role,omitempty"`
	Content          string `json:"content,omitempty"`
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

type mcStreamChoice struct {
	Index        int           `json:"index"`
	Delta        mcStreamDelta `json:"delta"`
	FinishReason string        `json:"finish_reason,omitempty"`
}

type mcStreamUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type mcStreamChunk struct {
	ID      string           `json:"id"`
	Object  string           `json:"object"`
	Created int64            `json:"created"`
	Model   string           `json:"model"`
	Choices []mcStreamChoice `json:"choices"`
	Usage   *mcStreamUsage   `json:"usage,omitempty"`
}

// mcloudSSEConverter 把灵犀 SSE 流实时转换为标准 OpenAI SSE。
// 同时内部累积 content/reasoning，供非流式路径聚合复用。
type mcloudSSEConverter struct {
	sc       *bufio.Scanner
	body     io.ReadCloser
	buf      bytes.Buffer
	started  bool   // 是否已发首个 delta
	finished bool   // 是否已发 finish + [DONE]
	model    string // 实际服务的 modelType（首帧 flowResult.modelType）
	content  strings.Builder
	reason   strings.Builder
	// promptRunes 折叠后的 dialogue 字符数：灵犀不返回 usage，
	// 输入/输出 token 都按字符估算（供日志/计费），口径同 proxy.EstimateOutputTokens。
	promptRunes int
	sawFrame    bool
	err         error
}

func newMCloudSSEConverter(body io.ReadCloser) *mcloudSSEConverter {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	return &mcloudSSEConverter{sc: sc, body: body}
}

// prime 预读到首个有输出（内容/终止/错误）的帧。CallStream 在返回前调用：
// 用于空流/错误体探测、首帧 modelType 校验与模型锁释放。
func (c *mcloudSSEConverter) prime() error {
	for c.buf.Len() == 0 && !c.finished && c.err == nil {
		if err := c.step(); err != nil {
			return err
		}
	}
	if c.err != nil {
		return c.err
	}
	return nil
}

// step 消费上游一行并转换
func (c *mcloudSSEConverter) step() error {
	if !c.sc.Scan() {
		if err := c.sc.Err(); err != nil {
			c.err = fmt.Errorf("读取 mcloud SSE 失败: %w", err)
			return c.err
		}
		// 上游 EOF：收到过帧则优雅收尾，从未收到帧视为空流错误
		if !c.sawFrame {
			c.err = fmt.Errorf("mcloud 流式上游返回空流")
			return c.err
		}
		if !c.finished {
			c.emitFinish()
		}
		c.finished = true
		return nil
	}

	line := strings.TrimSpace(c.sc.Text())
	if line == "" || strings.HasPrefix(line, "id:") ||
		strings.HasPrefix(line, "retry:") || strings.HasPrefix(line, "event:") {
		return nil
	}
	if !strings.HasPrefix(line, "data:") {
		return nil // 忽略无关行
	}
	data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if data == "" || data == "[DONE]" {
		return nil
	}

	var frame mcloudSSEFrame
	if err := json.Unmarshal([]byte(data), &frame); err != nil {
		return nil // 跳过无法解析的帧
	}
	c.sawFrame = true

	if frame.Code != mcloudCodeOK {
		detail := fmt.Sprintf("code=%s msg=%s", frame.Code, frame.Message)
		if frame.Code == mcloudCodeAuthFail || !frame.Success && strings.Contains(frame.Message, "鉴权") {
			c.err = &mcloudAuthError{detail: detail}
		} else {
			c.err = fmt.Errorf("mcloud 上游错误: %s", detail)
		}
		return c.err
	}

	fr := frame.Data.FlowResult
	if fr != nil {
		if c.model == "" && fr.ModelType != "" {
			c.model = fr.ModelType
		}
		if fr.ReasoningContent != "" {
			c.reason.WriteString(fr.ReasoningContent)
			c.emitDelta(mcStreamDelta{ReasoningContent: fr.ReasoningContent})
		}
		if fr.OutContent != "" {
			c.content.WriteString(fr.OutContent)
			c.emitDelta(mcStreamDelta{Content: fr.OutContent})
		}
	}
	if fr != nil && fr.FinishReason == "stop" || frame.Data.FinishReason == "stop" {
		c.emitFinish()
		c.finished = true
	}
	return nil
}

func (c *mcloudSSEConverter) emitDelta(delta mcStreamDelta) {
	if !c.started {
		delta.Role = "assistant"
		c.started = true
	}
	chunk := mcStreamChunk{
		ID:      "chatcmpl-mcloud-" + randString(8),
		Object:  "chat.completion.chunk",
		Created: time.Now().Unix(),
		Model:   c.model,
		Choices: []mcStreamChoice{{Index: 0, Delta: delta}},
	}
	c.writeChunk(chunk)
}

func (c *mcloudSSEConverter) emitFinish() {
	chunk := mcStreamChunk{
		ID:      "chatcmpl-mcloud",
		Object:  "chat.completion.chunk",
		Created: time.Now().Unix(),
		Model:   c.model,
		Choices: []mcStreamChoice{{Index: 0, Delta: mcStreamDelta{}, FinishReason: "stop"}},
		Usage:   c.usage(),
	}
	c.writeChunk(chunk)
	c.buf.WriteString("data: [DONE]\n\n")
}

// usage 估算整条请求的 token：灵犀任何帧都不带 usage，日志/计费全靠这里。
// 输出 = 正文 + 思考内容的字符估算；输入 = 折叠后 dialogue 的字符估算。
func (c *mcloudSSEConverter) usage() *mcStreamUsage {
	prompt := mcloudEstimateTokens(c.promptRunes)
	completion := mcloudEstimateTokens(len([]rune(c.content.String())) + len([]rune(c.reason.String())))
	if prompt+completion == 0 {
		return nil
	}
	return &mcStreamUsage{PromptTokens: prompt, CompletionTokens: completion, TotalTokens: prompt + completion}
}

// mcloudEstimateTokens 与 proxy.EstimateOutputTokens 同口径（约 3 字符 1 token，至少 1）。
// upstream 不能 import proxy（依赖方向相反），这里保留一份小拷贝。
func mcloudEstimateTokens(runeCount int) int {
	if runeCount <= 0 {
		return 0
	}
	est := runeCount / 3
	if est < 1 {
		est = 1
	}
	return est
}

func (c *mcloudSSEConverter) writeChunk(chunk mcStreamChunk) {
	b, err := json.Marshal(chunk)
	if err != nil {
		return
	}
	c.buf.WriteString("data: ")
	c.buf.Write(b)
	c.buf.WriteString("\n\n")
}

// Read 实现 io.Reader：优先服务已转换字节，空了再消费上游帧
func (c *mcloudSSEConverter) Read(p []byte) (int, error) {
	for c.buf.Len() == 0 {
		if c.finished {
			return 0, io.EOF
		}
		if c.err != nil {
			return 0, c.err
		}
		if err := c.step(); err != nil {
			if c.finished {
				continue
			}
			return 0, err
		}
	}
	return c.buf.Read(p)
}

func (c *mcloudSSEConverter) Close() error { return c.body.Close() }

// ====== 流卡死看门狗 ======

// idleTimeoutReader 超过 timeout 无读取活动则关闭底层流，解除阻塞的 Read。
// 覆盖「上游建立连接后不发帧」的卡死场景（对齐 webai 客户端 120s connectionTimeout）。
type idleTimeoutReader struct {
	rc      io.ReadCloser
	timeout time.Duration
	last    int64 // atomic unix nano
	closed  chan struct{}
	once    sync.Once
}

func newIdleTimeoutReader(rc io.ReadCloser, timeout time.Duration) *idleTimeoutReader {
	r := &idleTimeoutReader{
		rc:      rc,
		timeout: timeout,
		last:    time.Now().UnixNano(),
		closed:  make(chan struct{}),
	}
	go r.watch()
	return r
}

func (r *idleTimeoutReader) Read(p []byte) (int, error) {
	n, err := r.rc.Read(p)
	if n > 0 {
		atomic.StoreInt64(&r.last, time.Now().UnixNano())
	}
	return n, err
}

func (r *idleTimeoutReader) Close() error {
	r.once.Do(func() { close(r.closed) })
	return r.rc.Close()
}

func (r *idleTimeoutReader) watch() {
	t := time.NewTicker(r.timeout / 4)
	defer t.Stop()
	for {
		select {
		case <-r.closed:
			return
		case <-t.C:
			if time.Since(time.Unix(0, atomic.LoadInt64(&r.last))) > r.timeout {
				r.rc.Close()
				return
			}
		}
	}
}

// ====== 调用主流程 ======

// prepareChat 折叠 → 切模型 → 发起 chat → 预读首帧。带鉴权/模型不一致重试。
func (u *MCloudUpstream) prepareChat(ctx context.Context, body []byte) (*mcloudSSEConverter, string, error) {
	dialogue, modelType, err := foldMCloudDialogue(body)
	if err != nil {
		return nil, "", err
	}

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		cred, err := u.mgr.EnsureCreds()
		if err != nil {
			return nil, "", err
		}
		basic := creds.MCloudBasicAuth(cred)

		chatBody, err := buildMCloudChatBody(dialogue, cred.UserID)
		if err != nil {
			return nil, "", err
		}

		conv, reqID, err := u.doStartChat(ctx, chatBody, modelType, basic, len([]rune(dialogue)))
		if err == nil {
			return conv, reqID, nil
		}
		var authErr *mcloudAuthError
		if errors.As(err, &authErr) {
			// 01000002/401 过渡态：重读 accountdb.db 重拼 Basic 头重试
			u.mgr.InvalidateCreds()
			lastErr = err
			continue
		}
		var mmErr *mcloudModelMismatchError
		if errors.As(err, &mmErr) {
			// 账号偏好被外部改动（App 里手动切模型）：强制重新 set 再试
			u.modelMu.Lock()
			u.currentModel = ""
			u.modelMu.Unlock()
			lastErr = err
			continue
		}
		return nil, "", err
	}
	return nil, "", lastErr
}

// doStartChat 单次尝试：持模型锁（切模型 → POST → 首帧确认 → 释放）
// promptRunes 在建转换器时就写入 —— 单帧即结束的上游会在 prime() 内就 emitFinish，
// 事后再补会丢 prompt token。
func (u *MCloudUpstream) doStartChat(ctx context.Context, chatBody []byte, modelType, basic string, promptRunes int) (*mcloudSSEConverter, string, error) {
	reqID := "mc-" + randString(8)

	u.modelMu.Lock()
	if u.currentModel != modelType {
		if err := u.setModelLocked(ctx, modelType, basic); err != nil {
			u.modelMu.Unlock()
			return nil, "", err
		}
	}

	req, err := http.NewRequestWithContext(ctx, "POST",
		u.mgr.Config().BaseURL+"/assistant/chat/v2/add", bytes.NewReader(chatBody))
	if err != nil {
		u.modelMu.Unlock()
		return nil, "", err
	}
	for k, v := range mcloudHeaders(basic, "v1") {
		req.Header.Set(k, v)
	}
	req.Header.Set("Accept", "text/event-stream")

	resp, err := u.streamClient.Do(req)
	if err != nil {
		u.modelMu.Unlock()
		return nil, "", fmt.Errorf("mcloud chat 请求失败: %w", err)
	}
	if resp.StatusCode != 200 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		resp.Body.Close()
		detail := fmt.Sprintf("status=%d body=%s", resp.StatusCode, truncateStr(string(errBody), 200))
		u.modelMu.Unlock()
		if resp.StatusCode == 401 {
			return nil, "", &mcloudAuthError{detail: detail}
		}
		return nil, "", fmt.Errorf("mcloud chat 上游错误: %s", detail)
	}

	conv := newMCloudSSEConverter(newIdleTimeoutReader(resp.Body, mcloudIdleTimeout))
	conv.promptRunes = promptRunes
	if err := conv.prime(); err != nil {
		u.modelMu.Unlock()
		conv.Close()
		return nil, "", err
	}
	// 首帧 modelType 校验：服务端已绑定模型，可安全释放锁
	if conv.model != "" && conv.model != modelType {
		u.modelMu.Unlock()
		conv.Close()
		return nil, "", &mcloudModelMismatchError{got: conv.model, want: modelType}
	}
	u.modelMu.Unlock()
	return conv, reqID, nil
}

// setModelLocked 切换服务端账号级模型偏好（config/set，v1 头）。调用方须持有 modelMu。
func (u *MCloudUpstream) setModelLocked(ctx context.Context, modelType, basic string) error {
	body, _ := json.Marshal(map[string]interface{}{
		"modelType":           modelType,
		"sourceChannel":       mcloudSourceChannel,
		"modelThinkingStatus": false,
	})
	req, err := http.NewRequestWithContext(ctx, "POST",
		u.mgr.Config().BaseURL+"/assistant/chat/config", bytes.NewReader(body))
	if err != nil {
		return err
	}
	for k, v := range mcloudHeaders(basic, "v1") {
		req.Header.Set(k, v)
	}
	resp, err := u.client.Do(req)
	if err != nil {
		return fmt.Errorf("mcloud 切换模型请求失败: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	if resp.StatusCode == 401 {
		return &mcloudAuthError{detail: fmt.Sprintf("config/set status=401 body=%s", truncateStr(string(respBody), 200))}
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("mcloud 切换模型到 %s 失败: status=%d body=%s", modelType, resp.StatusCode, truncateStr(string(respBody), 200))
	}
	var r struct {
		Success bool   `json:"success"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(respBody, &r); err != nil {
		return fmt.Errorf("mcloud 切换模型响应解析失败: %w", err)
	}
	if r.Code == mcloudCodeAuthFail {
		return &mcloudAuthError{detail: "config/set code=" + mcloudCodeAuthFail}
	}
	if !r.Success || r.Code != mcloudCodeOK {
		// 非法模型（如已下线的 01000001）不破坏现有配置：上游继续用上一个模型，这里报错降级
		return fmt.Errorf("mcloud 切换模型到 %s 失败: code=%s msg=%s", modelType, r.Code, r.Message)
	}
	u.currentModel = modelType
	return nil
}

func truncateStr(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// Call 非流式：折叠 → chat（SSE）→ 聚合为完整 OpenAIResponse JSON
func (u *MCloudUpstream) Call(ctx context.Context, body []byte) (*Response, error) {
	conv, reqID, err := u.prepareChat(ctx, body)
	if err != nil {
		return nil, err
	}
	defer conv.Close()

	if _, err := io.Copy(io.Discard, conv); err != nil {
		return nil, err
	}

	resp := aggregatedResponse{
		ID:      "chatcmpl-mcloud-" + randString(8),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   conv.model,
		Choices: []aggregatedChoice{{
			Index: 0,
			Message: aggregatedMessage{
				Role:             "assistant",
				ReasoningContent: conv.reason.String(),
				Content:          conv.content.String(),
			},
			FinishReason: "stop",
		}},
	}
	// 非流式同样补估算 usage，否则日志/计费 token 全为 0
	if u := conv.usage(); u != nil {
		resp.Usage = &aggregatedUsage{PromptTokens: u.PromptTokens, CompletionTokens: u.CompletionTokens, TotalTokens: u.TotalTokens}
	}
	b, err := json.Marshal(resp)
	if err != nil {
		return nil, err
	}
	return &Response{StatusCode: 200, Body: b, ReqID: reqID}, nil
}

// CallStream 流式：上游 SSE 实时转换为 OpenAI SSE 直传
func (u *MCloudUpstream) CallStream(ctx context.Context, body []byte) (*StreamResponse, error) {
	conv, reqID, err := u.prepareChat(ctx, body)
	if err != nil {
		return nil, err
	}
	return &StreamResponse{StatusCode: 200, Body: conv, ReqID: reqID}, nil
}

// ====== config/get（模型列表 / 凭据验证共用） ======

type mcloudModelInfo struct {
	ModelType         string `json:"modelType"`
	Description       string `json:"description"`
	SavePath          string `json:"savePath"`
	MaxInputLength    int    `json:"maxInputLength"`
	EnableSetThinking bool   `json:"enableSetThinking"`
}

// postConfigGet POST /assistant/chat/config/get（v2 头）。
// 必须带全 body —— 空 body 只返回兜底单模型列表（文档实测坑）。
func (u *MCloudUpstream) postConfigGet(ctx context.Context, basic, userID string) (*mcloudConfigResp, int, error) {
	body, _ := json.Marshal(map[string]interface{}{
		"sourceChannel": mcloudSourceChannel,
		"versionInfo": map[string]string{
			"pcVersion": mcloudPCVersion,
			"h5Version": mcloudH5Version,
		},
		"userId": userID,
	})
	req, err := http.NewRequestWithContext(ctx, "POST",
		u.mgr.Config().BaseURL+"/assistant/chat/config/get", bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	for k, v := range mcloudHeaders(basic, "v2") {
		req.Header.Set(k, v)
	}
	resp, err := u.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("mcloud config/get 请求失败: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return nil, resp.StatusCode, fmt.Errorf("mcloud config/get status=%d body=%s", resp.StatusCode, truncateStr(string(respBody), 200))
	}
	var cr mcloudConfigResp
	if err := json.Unmarshal(respBody, &cr); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("mcloud config/get 解析失败: %w", err)
	}
	return &cr, resp.StatusCode, nil
}

type mcloudConfigResp struct {
	Success bool   `json:"success"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Data    struct {
		ModelList []mcloudModelInfo `json:"modelList"`
	} `json:"data"`
}

// FetchModels 拉取灵犀模型列表（config/get 全 body，返回裸 modelType id；
// mc/ 前缀由 proxy.MergeModels 统一添加，与 wb/ 同模式）
func (u *MCloudUpstream) FetchModels(ctx context.Context) ([]FetchedModel, error) {
	cred, err := u.mgr.EnsureCreds()
	if err != nil {
		return nil, err
	}
	cr, _, err := u.postConfigGet(ctx, creds.MCloudBasicAuth(cred), cred.UserID)
	if err != nil {
		return nil, err
	}
	if !cr.Success || cr.Code != mcloudCodeOK {
		return nil, fmt.Errorf("mcloud config/get 失败: code=%s msg=%s", cr.Code, cr.Message)
	}
	result := make([]FetchedModel, 0, len(cr.Data.ModelList))
	for _, m := range cr.Data.ModelList {
		if m.ModelType == "" {
			continue
		}
		label := m.Description
		if label == "" {
			label = m.SavePath
		}
		if label == "" {
			label = m.ModelType
		}
		result = append(result, FetchedModel{
			ID:        m.ModelType,
			Label:     label,
			Context:   m.MaxInputLength, // 上游声明的输入字符预算
			Stream:    true,
			Reasoning: m.EnableSetThinking,
		})
	}
	return result, nil
}

// VerifyCreds 调 config/get 验证 Basic 头（200 + code 0000 = 有效）
func (u *MCloudUpstream) VerifyCreds(ctx context.Context) (*VerifyResult, error) {
	cred := u.mgr.GetCred()
	if cred == nil {
		c, err := u.mgr.LoadCreds()
		if err != nil {
			return &VerifyResult{Valid: false, Status: -1}, err
		}
		cred = c
	}
	if cred.IsExpired() {
		return &VerifyResult{Valid: false, Status: 401}, nil
	}
	cr, status, err := u.postConfigGet(ctx, creds.MCloudBasicAuth(cred), cred.UserID)
	if err != nil {
		return &VerifyResult{Valid: false, Status: -1}, err
	}
	valid := status == 200 && cr.Success && cr.Code == mcloudCodeOK
	return &VerifyResult{Valid: valid, Status: status}, nil
}
