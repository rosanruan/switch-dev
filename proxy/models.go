package proxy

// ====== JoyCode 模型白名单 ======

type JoyCodeModel struct {
	ID              string `json:"id"`
	Label           string `json:"label"`
	Stream          bool   `json:"stream"`
	OutputMaxTokens int    `json:"outputMaxTokens"`
	Free            bool   `json:"free,omitempty"` // 限时免费
}

var JoyCodeModels = []JoyCodeModel{
	{ID: "JoyAI-Code-1.5", Label: "JoyAI-Code-1.5", Stream: true, OutputMaxTokens: 64000, Free: true},
	{ID: "MiniMax-M3-agent", Label: "MiniMax-M3", Stream: true, OutputMaxTokens: 64000},
	{ID: "MiniMax-M2.7-agent", Label: "MiniMax-M2.7", Stream: false, OutputMaxTokens: 64000},
	{ID: "Kimi-K2.6-agent", Label: "Kimi-K2.6", Stream: true, OutputMaxTokens: 64000},
	{ID: "GLM-5.1-agent", Label: "GLM-5.1", Stream: true, OutputMaxTokens: 64000},
	{ID: "GLM-5-agent", Label: "GLM-5", Stream: true, OutputMaxTokens: 64000},
	{ID: "DeepSeek-V4-Pro-agent", Label: "DeepSeek-V4-Pro", Stream: true, OutputMaxTokens: 64000},
	{ID: "Doubao-Seed-2.0-pro-agent", Label: "Doubao-Seed-2.0-pro", Stream: false, OutputMaxTokens: 64000},
}

var JoyCodeModelIDs map[string]bool
var JoyCodeLabelToID map[string]string
var JoyCodeModelByID map[string]*JoyCodeModel

func init() {
	JoyCodeModelIDs = make(map[string]bool)
	JoyCodeLabelToID = make(map[string]string)
	JoyCodeModelByID = make(map[string]*JoyCodeModel)
	for i := range JoyCodeModels {
		m := &JoyCodeModels[i]
		JoyCodeModelIDs[m.ID] = true
		JoyCodeLabelToID[m.Label] = m.ID
		JoyCodeLabelToID[m.ID] = m.ID // id 也映射到自己
		JoyCodeModelByID[m.ID] = m
	}
}

// ====== DevEco 模型 ======

type DevEcoModel struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Upstream string `json:"upstream"`
	Context  int    `json:"context"`
	Output   int    `json:"output"`
	Free     bool   `json:"free,omitempty"` // 限时免费
}

var DevEcoModels = []DevEcoModel{
	{ID: "glm-5.1", Label: "GLM-5.1 (DevEco)", Upstream: "GLM-5.1", Context: 170000, Output: 131072, Free: true},
}

var DevEcoModelIDs map[string]bool
var DevEcoLabelToID map[string]string
var DevEcoModelByID map[string]*DevEcoModel

func init() {
	DevEcoModelIDs = make(map[string]bool)
	DevEcoLabelToID = make(map[string]string)
	DevEcoModelByID = make(map[string]*DevEcoModel)
	for i := range DevEcoModels {
		m := &DevEcoModels[i]
		DevEcoModelIDs[m.ID] = true
		DevEcoLabelToID[m.ID] = m.ID
		DevEcoLabelToID[m.Label] = m.ID
		DevEcoLabelToID[m.Upstream] = m.ID
		DevEcoModelByID[m.ID] = m
	}
}

// ====== OpenCode Zen 模型 ======

type OpenCodeModel struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Context int    `json:"context"`
	Output  int    `json:"output"`
	Free    bool   `json:"free,omitempty"` // 限时免费
}

var OpenCodeModels = []OpenCodeModel{
	{ID: "deepseek-v4-flash-free", Label: "DeepSeek V4 Flash Free", Context: 200000, Output: 128000, Free: true},
	{ID: "mimo-v2.5-free", Label: "MiMo V2.5 Free", Context: 262144, Output: 64000, Free: true},
	{ID: "ling-3.0-flash-free", Label: "Ling 3.0 Flash Free", Context: 262144, Output: 32768, Free: true},
	{ID: "ling-3.0-tiny-free", Label: "Ling 3.0 Tiny Free", Context: 0, Output: 0, Free: true},
	{ID: "nemotron-3-ultra-free", Label: "Nemotron 3 Ultra Free", Context: 0, Output: 0, Free: true},
	{ID: "north-mini-code-free", Label: "North Mini Code Free", Context: 256000, Output: 64000, Free: true},
	{ID: "laguna-s-2.1-free", Label: "Laguna S 2.1 Free", Context: 256000, Output: 32000, Free: true},
	{ID: "longcat-2.0-free", Label: "LongCat 2.0 Free", Context: 0, Output: 0, Free: true},
}

var OpenCodeModelIDs map[string]bool
var OpenCodeModelByID map[string]*OpenCodeModel

func init() {
	OpenCodeModelIDs = make(map[string]bool)
	OpenCodeModelByID = make(map[string]*OpenCodeModel)
	for i := range OpenCodeModels {
		m := &OpenCodeModels[i]
		OpenCodeModelIDs[m.ID] = true
		OpenCodeModelByID[m.ID] = m
	}
}

// ====== WorkBuddy 模型白名单（腾讯 CodeBuddy，免费档） ======
// 内部 id 用 wb/ 前缀隔离，避免与 DevEco 的 glm-5.1 等重名；发上游前 stripWbPrefix 还原

type WorkBuddyModel struct {
	ID        string `json:"id"` // wb/ 前缀内部 id
	Label     string `json:"label"`
	Context   int    `json:"context"`
	Output    int    `json:"output"`
	Vision    bool   `json:"vision"`
	ToolCall  bool   `json:"toolCall"`
	Reasoning bool   `json:"reasoning"`
	Free      bool   `json:"free,omitempty"` // 限时免费
}

var WorkBuddyModels = []WorkBuddyModel{
	{ID: "wb/glm-5.0", Label: "GLM-5.0 (WorkBuddy)", Output: 48000, Vision: true, ToolCall: true, Reasoning: true},
	{ID: "wb/glm-5.1", Label: "GLM-5.1 (WorkBuddy)", Output: 48000, Vision: true, ToolCall: true, Reasoning: true},
	{ID: "wb/glm-5.0-turbo", Label: "GLM-5.0-Turbo (WorkBuddy)", Output: 48000, Vision: true, ToolCall: true, Reasoning: true},
	{ID: "wb/glm-4.7", Label: "GLM-4.7 (WorkBuddy)", Output: 48000, Vision: true, ToolCall: true, Reasoning: true},
	{ID: "wb/minimax-m2.5", Label: "MiniMax-M2.5 (WorkBuddy)", Output: 48000, Vision: true, ToolCall: true, Reasoning: true},
	{ID: "wb/minimax-m2.7", Label: "MiniMax-M2.7 (WorkBuddy)", Output: 48000, Vision: true, ToolCall: true, Reasoning: true},
	{ID: "wb/kimi-k2.5", Label: "Kimi-K2.5 (WorkBuddy)", Output: 32000, Vision: true, ToolCall: true, Reasoning: true},
	{ID: "wb/kimi-k2.6", Label: "Kimi-K2.6 (WorkBuddy)", Output: 32000, Vision: true, ToolCall: true, Reasoning: true},
	{ID: "wb/kimi-k2-thinking", Label: "Kimi-K2-Thinking (WorkBuddy)", Output: 32000, Vision: true, ToolCall: true, Reasoning: true},
	{ID: "wb/deepseek-v3-2-volc", Label: "DeepSeek-V3-2-Volc (WorkBuddy)", Output: 32000, Vision: true, ToolCall: true, Reasoning: true},
	{ID: "wb/hunyuan-2.0-thinking", Label: "Hunyuan-2.0-Thinking (WorkBuddy)", Output: 16000, ToolCall: true, Reasoning: true},
	{ID: "wb/hunyuan-2.0-instruct", Label: "Hunyuan-2.0-Instruct (WorkBuddy)", Output: 24000, ToolCall: true},
	{ID: "wb/hy3", Label: "Hunyuan Hy3 (WorkBuddy)", Output: 32000, Vision: true, ToolCall: true, Reasoning: true, Free: true},
	{ID: "wb/hy4-preview", Label: "Hy4 Preview (WorkBuddy)", Output: 32000, Vision: true, ToolCall: true, Reasoning: true},
}

var WorkBuddyModelIDs map[string]bool
var WorkBuddyModelByID map[string]*WorkBuddyModel

func init() {
	WorkBuddyModelIDs = make(map[string]bool)
	WorkBuddyModelByID = make(map[string]*WorkBuddyModel)
	for i := range WorkBuddyModels {
		m := &WorkBuddyModels[i]
		WorkBuddyModelIDs[m.ID] = true
		WorkBuddyModelByID[m.ID] = m
	}
}

// stripWbPrefix 去掉 wb/ 前缀，还原成发往上游的 model id
func stripWbPrefix(id string) string {
	if len(id) >= 3 && id[:3] == "wb/" {
		return id[3:]
	}
	return id
}

// ====== 供应商模型（provider/ 前缀隔离，多供应商平级） ======

// ProviderModelPrefix provider 模型内部 id 前缀（provider/<providerID>/<modelID>）
const ProviderModelPrefix = "provider/"

// ProviderModel 供应商模型（动态注册，运行时从各 provider 填充）
type ProviderModel struct {
	InternalID string // 代理内部 id：provider/<providerID>/<modelID>
	ProviderID string // provider id（如 "groq"）
	ModelID    string // 原始 model id（发上游用）
	Label      string // 显示名
	Context    int    // 上下文
}

// ProviderModelHealth 查询 provider 模型健康状态的回调（由 main 注入，避免 proxy 依赖 providerapi 包）
var ProviderModelHealth func(providerID, modelID string) bool

// SetProviderModels 更新免费模型列表（provider 增删/切换时由 main 调用）
// 经原子指针发布，供 ProviderModelList() 读取
func SetProviderModels(models []ProviderModel) {
	providerModelsPtr.Store(&models)
}

// IsProviderModelHealthy 查询 provider 模型健康状态（权重降级用）；非 provider 模型或未配置返回 true
func IsProviderModelHealthy(providerID, modelID string) bool {
	if ProviderModelHealth != nil {
		return ProviderModelHealth(providerID, modelID)
	}
	return true
}

// IsProviderModel 判断 model id 是否 provider 前缀
func IsProviderModel(model string) bool {
	return len(model) > len(ProviderModelPrefix) && model[:len(ProviderModelPrefix)] == ProviderModelPrefix
}

// ParseProviderModel 解析 provider/<providerID>/<modelID>；非 provider 返回空
func ParseProviderModel(internalID string) (providerID, modelID string) {
	if !IsProviderModel(internalID) {
		return "", ""
	}
	rest := internalID[len(ProviderModelPrefix):]
	// providerID 到第一个 /
	slash := -1
	for i := 0; i < len(rest); i++ {
		if rest[i] == '/' {
			slash = i
			break
		}
	}
	if slash < 0 {
		return "", ""
	}
	return rest[:slash], rest[slash+1:]
}

// stripProviderPrefix 去掉 provider/<providerID>/ 前缀，还原成发往上游的 model id
func stripProviderPrefix(internalID string) string {
	_, modelID := ParseProviderModel(internalID)
	if modelID == "" {
		return internalID
	}
	return modelID
}

// ActualUpstreamModel 返回真正发往各上游 base_url 请求体里的 model 名，
// 即把代理内部路由 id 还原为上游认的名字：
//   - provider/<pid>/<mid> → <mid>
//   - wb/<mid>         → <mid>
//   - DevEco 本地 id    → 网关 upstream 名（DevEcoModelByID.Upstream）
//   - 其他（joycode/opencode 等）→ 原样
//
// 用于统计：按"实际请求到 base_url 接口的 model"聚合，而不是按内部 id。
func ActualUpstreamModel(internalID string) string {
	if internalID == "" {
		return ""
	}
	if IsProviderModel(internalID) {
		return stripProviderPrefix(internalID)
	}
	if len(internalID) >= 3 && internalID[:3] == "wb/" {
		return stripWbPrefix(internalID)
	}
	if m, ok := LookupModel(internalID); ok && m.WireName != "" {
		return m.WireName
	}
	return internalID
}

// ====== 模型解析 ======

// ResolveModel 把客户端发来的 model 名解析成代理内部统一 id。
// 未识别的模型名原样返回（调用方据此判断是否识别），不再硬编码兜底到 glm-5.1。
func ResolveModel(requestedModel string) string {
	if requestedModel == "" {
		return ""
	}
	// provider 模型：原样返回（不降级）
	if IsProviderModel(requestedModel) {
		return requestedModel
	}
	if _, ok := LookupModel(requestedModel); ok {
		return requestedModel
	}
	if id, ok := lookupByAlias(requestedModel); ok {
		return id
	}
	return requestedModel
}

// IsKnownModel 判断客户端发来的 model 名能否被识别为某个已知上游模型
// （含 label/wire 名反查）。auto/manual 降级链据此决定链首用请求模型还是 GlobalFallback。
func IsKnownModel(requestedModel string) bool {
	if requestedModel == "" || IsProviderModel(requestedModel) {
		return requestedModel != ""
	}
	if _, ok := LookupModel(requestedModel); ok {
		return true
	}
	_, ok := lookupByAlias(requestedModel)
	return ok
}

// ResolveUpstream 判断 model 走哪个上游
func ResolveUpstream(model string) string {
	m := ResolveModel(model)
	// provider 模型：返回 provider id（作为上游名，由 Server.ProviderAPIs map 查）
	if IsProviderModel(m) {
		pid, _ := ParseProviderModel(m)
		if pid != "" {
			return pid
		}
	}
	if cm, ok := LookupModel(m); ok {
		return cm.Upstream
	}
	return "joycode"
}

// modelContextLimit 返回模型 context 窗口上限（token），用于 usage 合理性校验；未知返回 0
func modelContextLimit(modelID string) int {
	if m, ok := LookupModel(modelID); ok {
		return m.Context
	}
	return 0
}

// ClampMaxTokens 钳制 max_tokens 到模型上限
func ClampMaxTokens(requested, limit int) int {
	v := requested
	if v <= 0 {
		v = 4096
	}
	if limit <= 0 {
		return v
	}
	if v > limit {
		return limit
	}
	return v
}

// ModelInfo 用于 /v1/models 返回
type ModelInfo struct {
	ID       string `json:"id"`
	Object   string `json:"object"`
	Created  int64  `json:"created"`
	OwnedBy  string `json:"owned_by"`
	Label    string `json:"_label,omitempty"`
	Stream   bool   `json:"_stream,omitempty"`
	Upstream string `json:"_upstream,omitempty"`
	Context  int    `json:"_context,omitempty"`
	Output   int    `json:"_output,omitempty"`
	Vision   bool   `json:"_vision,omitempty"`
	ToolCall bool   `json:"_toolCall,omitempty"`
	Free     bool   `json:"_free,omitempty"` // 限时免费标识
	// Reasoning 是否推理模型（接口拉取/种子提供，供前端与目录同步使用）
	Reasoning bool `json:"_reasoning,omitempty"`
	// Wire 发往上游 base_url 的真实 model 名（DevEco 的 id 与 wire 名不同）。
	// 不出现在 /v1/models 响应里，仅供目录同步内部使用。
	Wire string `json:"-"`
}

func lower(s string) string {
	// 简单 lowercase
	result := make([]byte, len(s))
	for i, c := range s {
		if c >= 'A' && c <= 'Z' {
			result[i] = byte(c + 32)
		} else {
			result[i] = byte(c)
		}
	}
	return string(result)
}
