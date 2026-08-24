package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/google/uuid"
	"switchdev/paths"
	"switchdev/proxy"
)

// AgentModels agent 分组内的一组模型
type AgentModels struct {
	Upstream string   `json:"upstream"`
	Models   []string `json:"models"`
}

// UAModelMap 请求模型 -> 目标上游模型的映射
type UAModelMap struct {
	RequestedModel string         `json:"requestedModel"`
	Target         proxy.ModelRef `json:"target"`
}

// UARule 单条 User-Agent 路由规则
type UARule struct {
	ID       string       `json:"id"`
	Name     string       `json:"name"`
	Pattern  string       `json:"pattern"`
	Enabled  bool         `json:"enabled"`
	Mappings []UAModelMap `json:"mappings"`
	// DefaultTarget 规则级默认目标：UA 命中但请求模型未命中任何 mapping 时，
	// 整个请求路由到该目标（请求模型 id 原样透传给上游，由上游/代理处理）。
	// 留空则回退到 UA 全局兜底（uaGlobalFallback）/正常降级链。
	// 这是「cc-switch 式」的免维护方案——无需为每个客户端枚举请求模型列表。
	DefaultTarget proxy.ModelRef `json:"defaultTarget,omitempty"`
}

// Preset 运行模式方案快照
// 只含降级链相关字段，不含 port/apiKey/update —— 那些是环境配置，
// 不应随方案切换而变（apiKey 变了会让已接入的客户端 401）
type Preset struct {
	Name             string                      `json:"name"`
	Mode             string                      `json:"mode"`
	AutoChain        []AgentModels               `json:"autoChain"`
	ManualFallbacks  map[string][]proxy.ModelRef `json:"manualFallbacks"`
	GlobalFallback   proxy.ModelRef              `json:"globalFallback"`
	UARoutingEnabled bool                        `json:"uaRoutingEnabled"`
	UARules          []UARule                    `json:"uaRules"`
	UAGlobalFallback proxy.ModelRef              `json:"uaGlobalFallback"`
}

// ModelRef 复用 proxy.ModelRef，避免类型不一致
// （config 包已 import proxy，无需重复定义）

// copyChain 深拷贝 auto 链（Config 和 Preset 都要用）
func copyChain(src []AgentModels) []AgentModels {
	dst := make([]AgentModels, len(src))
	for i, ag := range src {
		dst[i] = AgentModels{
			Upstream: ag.Upstream,
			Models:   append([]string{}, ag.Models...),
		}
	}
	return dst
}

// copyFallbacks 深拷贝手动降级链（Config 和 Preset 都要用）
func copyFallbacks(src map[string][]proxy.ModelRef) map[string][]proxy.ModelRef {
	dst := make(map[string][]proxy.ModelRef, len(src))
	for k, v := range src {
		dst[k] = append([]proxy.ModelRef{}, v...)
	}
	return dst
}

// copyUARules 深拷贝 UA 路由规则
func copyUARules(src []UARule) []UARule {
	if src == nil {
		return nil
	}
	dst := make([]UARule, len(src))
	for i, r := range src {
		dst[i] = UARule{
			ID:            r.ID,
			Name:          r.Name,
			Pattern:       r.Pattern,
			Enabled:       r.Enabled,
			DefaultTarget: r.DefaultTarget,
		}
		if r.Mappings != nil {
			dst[i].Mappings = make([]UAModelMap, len(r.Mappings))
			copy(dst[i].Mappings, r.Mappings)
		}
	}
	return dst
}

// copyUpstreams 深拷贝上游开关 map
func copyUpstreams(src map[string]UpstreamSettings) map[string]UpstreamSettings {
	if src == nil {
		return nil
	}
	dst := make(map[string]UpstreamSettings, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

// UpdateConfig 自动升级配置
type UpdateConfig struct {
	Enabled   bool         `json:"enabled"`   // 是否启用自动升级检查
	Provider  string       `json:"provider"`  // "github" | "custom"（默认 github）
	GitHub    GitHubConfig `json:"github"`    // GitHub Releases 配置
	UpdateURL string       `json:"updateUrl"` // 自定义检查地址（优先于 github）
	Channel   string       `json:"channel"`   // "stable" | "beta"（默认 stable）
}

// LogFileConfig 控制台日志落地到文件的配置
type LogFileConfig struct {
	Enabled bool `json:"enabled"` // 是否把控制台日志同时写入文件（默认开启）
}

// GitHubConfig GitHub Releases 配置
type GitHubConfig struct {
	Owner string `json:"owner"` // GitHub 用户名/组织
	Repo  string `json:"repo"`  // 仓库名
	Token string `json:"token"` // 私有仓库 PAT（公开仓库可空）
}

// UpstreamSettings 单个上游的开关配置
type UpstreamSettings struct {
	// Enabled 该上游是否启用；禁用时其下所有模型在调用时被直接跳过（全局生效）
	Enabled bool `json:"enabled"`
}

// Config 代理运行配置
type Config struct {
	Mode             string                      `json:"mode"`                // "auto" | "manual" | "ua"
	AutoChain        []AgentModels               `json:"autoChain"`           // auto 模式优先级链
	ManualFallbacks  map[string][]proxy.ModelRef `json:"manualFallbacks"`     // 手动模式下模型的降级链
	GlobalFallback   proxy.ModelRef              `json:"globalFallback"`      // auto/manual 全局兜底
	UARoutingEnabled bool                        `json:"uaRoutingEnabled"`    // auto/manual 模式下 UA 叠加层开关
	UARules          []UARule                    `json:"uaRules"`             // UA 路由规则
	UAGlobalFallback proxy.ModelRef              `json:"uaGlobalFallback"`    // ua 模式全局兜底
	Port             int                         `json:"port"`                // 代理监听端口
	APIKey           string                      `json:"apiKey"`              // 客户端接入密钥（AuthEnabled=true 时严格校验）
	AuthEnabled      bool                        `json:"authEnabled"`         // 是否要求客户端携带 apiKey；默认 true，关闭后网关不鉴权
	AutoStart        bool                        `json:"autoStart"`           // 登录系统时自动启动（静默到托盘，以 --tray 启动）
	AutoUpdate       UpdateConfig                `json:"update"`              // 自动升级配置
	LogFile          LogFileConfig               `json:"logFile"`             // 控制台日志落地文件配置
	Presets          []Preset                    `json:"presets"`             // 已保存的运行模式方案
	ActivePreset     string                      `json:"activePreset"`        // 当前激活方案名（仅 UI 提示；偏离后置空 = 自定义）
	Provider         ProviderSettings            `json:"provider"`            // 供应商配置相关偏好
	Upstreams        map[string]UpstreamSettings `json:"upstreams,omitempty"` // 各上游启用开关（key=upstream/provider id，缺省视为启用）

	mu   sync.RWMutex `json:"-"`
	path string       `json:"-"`
}

// ProviderSettings 供应商功能偏好
type ProviderSettings struct {
	// AutoBenchmarkOnEdit 进入供应商编辑时自动拉取模型并批量测评
	AutoBenchmarkOnEdit bool `json:"autoBenchmarkOnEdit"`
	// IdleAutoLock 闲置时自动锁定供应商界面（默认开启）
	IdleAutoLock bool `json:"idleAutoLock"`
}

// DefaultConfigPath 默认配置文件路径
func DefaultConfigPath() string {
	return filepath.Join(paths.AppConfigDir(), "config.json")
}

// DefaultPort 默认代理端口
const DefaultPort = 8787

// DefaultAutoBenchmarkOnEdit 控制"进入编辑时自动拉取并测评模型"的默认开关。
// 默认关闭，打包时可用 -ldflags "-X switchdev/config.DefaultAutoBenchmarkOnEdit=true" 改为默认开启。
var DefaultAutoBenchmarkOnEdit = "false"

// generateAPIKey 生成随机 apiKey，格式 rs-<uuid>
// 首次启动或老配置无此字段时调用
func generateAPIKey() string {
	return "rs-" + uuid.NewString()
}

// Defaults 返回默认配置（首次安装：不预设模型，用户按需配置）
func Defaults() *Config {
	return &Config{
		Mode:             "auto",
		AutoChain:        []AgentModels{},
		ManualFallbacks:  map[string][]proxy.ModelRef{},
		GlobalFallback:   proxy.ModelRef{},
		UARoutingEnabled: true,
		UARules: []UARule{
			{ID: "ua-claude-code", Name: "Claude Code", Pattern: "claude-cli", Enabled: true, Mappings: []UAModelMap{}},
			{ID: "ua-codex", Name: "Codex", Pattern: "codex", Enabled: true, Mappings: []UAModelMap{}},
		},
		Port:         DefaultPort,
		AuthEnabled:  true,
		AutoStart:    false,
		Presets:      []Preset{},
		ActivePreset: "",
		Provider: ProviderSettings{
			AutoBenchmarkOnEdit: DefaultAutoBenchmarkOnEdit == "true",
			IdleAutoLock:        true,
		},
		AutoUpdate: UpdateConfig{
			Enabled:  true,
			Provider: "github",
			GitHub: GitHubConfig{
				Owner: "rosanruan",
				Repo:  "switch-dev",
			},
			Channel: "stable",
		},
		LogFile: LogFileConfig{
			Enabled: true,
		},
	}
}

// Load 从文件加载配置；不存在则返回默认配置并自动写盘
func Load(path string) (*Config, error) {
	if path == "" {
		path = DefaultConfigPath()
	}
	c := Defaults()
	c.path = path

	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		// 文件不存在：生成 apiKey 后写默认配置
		c.APIKey = generateAPIKey()
		if err := c.Save(); err != nil {
			return c, fmt.Errorf("保存默认配置失败: %w", err)
		}
		return c, nil
	}
	if err != nil {
		return c, fmt.Errorf("读取配置文件失败: %w", err)
	}

	if err := json.Unmarshal(data, c); err != nil {
		// 损坏的 JSON 用默认并覆盖
		*c = *Defaults()
		c.path = path
		c.APIKey = generateAPIKey()
		c.Save()
		return c, fmt.Errorf("配置 JSON 解析失败，已重置为默认: %w", err)
	}

	// 老配置无 apiKey 字段：先补全，避免被 Validate 当作非法配置重置
	keyGenerated := false
	if c.APIKey == "" {
		c.APIKey = generateAPIKey()
		keyGenerated = true
	}

	// 老配置无 authEnabled 字段：零值 false 会意外关闭鉴权，检测缺省后默认开启
	var probe struct {
		AuthEnabled *bool `json:"authEnabled"`
	}
	if err := json.Unmarshal(data, &probe); err == nil && probe.AuthEnabled == nil {
		c.AuthEnabled = true
	}

	if err := c.Validate(); err != nil {
		// 配置校验失败：除降级链等字段回退默认外，尽量沿用旧 apiKey，
		// 避免一次坏字段（如某条 UA 规则 upstream 无效）把所有已接入客户端静默打成 401。
		// 此处能走到说明 JSON 已成功解析、APIKey 已补全（上方 keyGenerated），旧 key 可用。
		oldAPIKey := c.APIKey
		*c = *Defaults()
		c.path = path
		if strings.TrimSpace(oldAPIKey) != "" {
			c.APIKey = oldAPIKey
		} else {
			c.APIKey = generateAPIKey()
		}
		c.Save()
		return c, fmt.Errorf("配置校验失败，已重置为默认（保留原 apiKey）: %w", err)
	}

	// 补全的字段需持久化（升级用户首次启动）
	if keyGenerated {
		c.Save()
	}

	return c, nil
}

// Save 保存配置到磁盘
func (c *Config) Save() error {
	if c.path == "" {
		c.path = DefaultConfigPath()
	}
	// 确保目录存在
	dir := filepath.Dir(c.path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("创建配置目录失败: %w", err)
	}

	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化配置失败: %w", err)
	}
	if err := os.WriteFile(c.path, data, 0600); err != nil {
		return fmt.Errorf("写入配置文件失败: %w", err)
	}
	return nil
}

// Validate 校验配置合法性
func (c *Config) Validate() error {
	// apiKey
	if strings.TrimSpace(c.APIKey) == "" {
		return fmt.Errorf("apiKey 不能为空")
	}

	// 端口
	if c.Port < 1024 || c.Port > 65535 {
		return fmt.Errorf("无效的端口: %d，应在 1024-65535 之间", c.Port)
	}

	// 升级配置
	if c.AutoUpdate.Provider != "" && c.AutoUpdate.Provider != "github" && c.AutoUpdate.Provider != "custom" {
		return fmt.Errorf("无效的更新 provider: %s", c.AutoUpdate.Provider)
	}
	if c.AutoUpdate.Channel != "" && c.AutoUpdate.Channel != "stable" && c.AutoUpdate.Channel != "beta" {
		return fmt.Errorf("无效的更新 channel: %s", c.AutoUpdate.Channel)
	}

	// 模式
	if c.Mode != "auto" && c.Mode != "manual" && c.Mode != "ua" {
		return fmt.Errorf("无效的模式: %s，应为 auto、manual 或 ua", c.Mode)
	}

	// auto 链（允许为空，首次安装时用户未配置）
	for _, ag := range c.AutoChain {
		if !isValidUpstream(ag.Upstream) {
			return fmt.Errorf("无效的 upstream: %s", ag.Upstream)
		}
		for _, m := range ag.Models {
			if !isValidModel(m) {
				// 仅警告，允许不存在的模型名（可能是新增的，不影响正常运行）
			}
		}
	}

	// 手动降级链
	for key, chain := range c.ManualFallbacks {
		if !isValidModel(key) {
			return fmt.Errorf("manualFallbacks 的 key 不是有效模型: %s", key)
		}
		for _, ref := range chain {
			if !isValidUpstream(ref.Upstream) {
				return fmt.Errorf("manualFallbacks[%s] 中无效的 upstream: %s", key, ref.Upstream)
			}
		}
	}

	// 全局兜底（允许为空，首次安装时用户未配置）
	if c.GlobalFallback.Upstream != "" && !isValidUpstream(c.GlobalFallback.Upstream) {
		return fmt.Errorf("globalFallback 的 upstream 无效: %s", c.GlobalFallback.Upstream)
	}

	// ua 模式全局兜底（允许为空）
	if c.UAGlobalFallback.Upstream != "" && !isValidUpstream(c.UAGlobalFallback.Upstream) {
		return fmt.Errorf("uaGlobalFallback 的 upstream 无效: %s", c.UAGlobalFallback.Upstream)
	}

	// UA 路由规则
	for i, rule := range c.UARules {
		if strings.TrimSpace(rule.Pattern) == "" {
			return fmt.Errorf("uaRules[%d] 的 pattern 不能为空", i)
		}
		for j, m := range rule.Mappings {
			if m.Target.Upstream != "" && !isValidUpstream(m.Target.Upstream) {
				return fmt.Errorf("uaRules[%d].mappings[%d] 的 upstream 无效: %s", i, j, m.Target.Upstream)
			}
		}
		if rule.DefaultTarget.Upstream != "" && !isValidUpstream(rule.DefaultTarget.Upstream) {
			return fmt.Errorf("uaRules[%d] 的 defaultTarget upstream 无效: %s", i, rule.DefaultTarget.Upstream)
		}
	}

	// 方案列表：名字非空、不重名、mode 合法、upstream 合法
	seenPreset := make(map[string]bool, len(c.Presets))
	for _, p := range c.Presets {
		if strings.TrimSpace(p.Name) == "" {
			return fmt.Errorf("方案名不能为空")
		}
		if seenPreset[p.Name] {
			return fmt.Errorf("方案名重复: %s", p.Name)
		}
		seenPreset[p.Name] = true
		if p.Mode != "auto" && p.Mode != "manual" && p.Mode != "ua" {
			return fmt.Errorf("方案 %s 的模式无效: %s", p.Name, p.Mode)
		}
		for _, ag := range p.AutoChain {
			if !isValidUpstream(ag.Upstream) {
				return fmt.Errorf("方案 %s 中无效的 upstream: %s", p.Name, ag.Upstream)
			}
		}
		for key, chain := range p.ManualFallbacks {
			for _, ref := range chain {
				if !isValidUpstream(ref.Upstream) {
					return fmt.Errorf("方案 %s 的 manualFallbacks[%s] 中无效的 upstream: %s", p.Name, key, ref.Upstream)
				}
			}
		}
		if p.GlobalFallback.Upstream != "" && !isValidUpstream(p.GlobalFallback.Upstream) {
			return fmt.Errorf("方案 %s 的 globalFallback upstream 无效: %s", p.Name, p.GlobalFallback.Upstream)
		}
		if p.UAGlobalFallback.Upstream != "" && !isValidUpstream(p.UAGlobalFallback.Upstream) {
			return fmt.Errorf("方案 %s 的 uaGlobalFallback upstream 无效: %s", p.Name, p.UAGlobalFallback.Upstream)
		}
	}

	// ActivePreset 刻意不做存在性校验：
	// Load() 在 Validate 失败时会把整份配置重置为默认（用户会丢掉所有配置），
	// 一个悬空的方案名不值得付这个代价，UI 端降级显示「自定义」即可
	return nil
}

// Clone 深拷贝（线程安全）
func (c *Config) Clone() *Config {
	c.mu.RLock()
	defer c.mu.RUnlock()

	cp := &Config{
		Mode:             c.Mode,
		AutoChain:        copyChain(c.AutoChain),
		ManualFallbacks:  copyFallbacks(c.ManualFallbacks),
		GlobalFallback:   c.GlobalFallback,
		UARoutingEnabled: c.UARoutingEnabled,
		UARules:          copyUARules(c.UARules),
		UAGlobalFallback: c.UAGlobalFallback,
		Port:             c.Port,
		APIKey:           c.APIKey,
		AuthEnabled:      c.AuthEnabled,
		AutoStart:        c.AutoStart,
		AutoUpdate:       c.AutoUpdate,
		LogFile:          c.LogFile,
		ActivePreset:     c.ActivePreset,
		Provider:         c.Provider,
		Upstreams:        copyUpstreams(c.Upstreams),
		path:             c.path,
	}
	// 方案列表必须深拷贝，否则前端改动会串到 Manager 持有的配置上
	cp.Presets = make([]Preset, len(c.Presets))
	for i, p := range c.Presets {
		cp.Presets[i] = Preset{
			Name:             p.Name,
			Mode:             p.Mode,
			AutoChain:        copyChain(p.AutoChain),
			ManualFallbacks:  copyFallbacks(p.ManualFallbacks),
			GlobalFallback:   p.GlobalFallback,
			UARoutingEnabled: p.UARoutingEnabled,
			UARules:          copyUARules(p.UARules),
			UAGlobalFallback: p.UAGlobalFallback,
		}
	}
	return cp
}

// Update 原子更新配置（线程安全），自动保存
func (c *Config) Update(newCfg *Config) error {
	// 校验
	if err := newCfg.Validate(); err != nil {
		return err
	}
	newCfg.path = c.path

	c.mu.Lock()
	c.Mode = newCfg.Mode
	c.AutoChain = newCfg.AutoChain
	c.ManualFallbacks = newCfg.ManualFallbacks
	c.GlobalFallback = newCfg.GlobalFallback
	c.UARoutingEnabled = newCfg.UARoutingEnabled
	c.UARules = newCfg.UARules
	c.UAGlobalFallback = newCfg.UAGlobalFallback
	c.Port = newCfg.Port
	c.APIKey = newCfg.APIKey
	c.AuthEnabled = newCfg.AuthEnabled
	c.AutoStart = newCfg.AutoStart
	c.AutoUpdate = newCfg.AutoUpdate
	c.LogFile = newCfg.LogFile
	c.Presets = newCfg.Presets
	c.ActivePreset = newCfg.ActivePreset
	c.Provider = newCfg.Provider
	c.Upstreams = newCfg.Upstreams
	c.mu.Unlock()

	return c.Save()
}

// GetMode 线程安全获取当前模式
func (c *Config) GetMode() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Mode
}

// GetAPIKey 线程安全获取当前 apiKey
func (c *Config) GetAPIKey() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.APIKey
}

// GetAuthEnabled 线程安全读取鉴权开关
func (c *Config) GetAuthEnabled() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.AuthEnabled
}

// IsUpstreamEnabled 判断上游是否启用（缺省/未配置视为启用，保证升级与新增供应商非破坏）
func (c *Config) IsUpstreamEnabled(name string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.Upstreams == nil {
		return true
	}
	s, ok := c.Upstreams[name]
	if !ok {
		return true
	}
	return s.Enabled
}

// isValidUpstream 检查 upstream 名是否合法。
// 内置 4 上游固定；其余非空名都视为合法的 ProviderAPI 供应商 id
// （内置目录 slug 如 "groq"，或自定义的 "custom-xxxxxx"，由 ProviderAPI 子系统动态管理，
// config 包无法静态枚举，且与 providerapi 包存在导入方向约束）。
// 运行时 pickUpstream 对找不到的 upstream 会安全跳过（executeChain* 里 up==nil 即 continue），
// 因此这里采用与 isValidModel 一致的宽松策略，避免保存降级链/方案时误拒自定义供应商。
func isValidUpstream(u string) bool {
	switch u {
	case "joycode", "deveco", "opencode", "workbuddy":
		return true
	}
	return strings.TrimSpace(u) != ""
}

// isValidModel 检查模型 id 是否在已知白名单中（宽松校验，允许自定义）
func isValidModel(m string) bool {
	// 检查所有已知模型
	if proxy.JoyCodeModelIDs[m] || proxy.DevEcoModelIDs[m] || proxy.OpenCodeModelIDs[m] {
		return true
	}
	// 用 label 反查也接受
	low := strings.ToLower(m)
	for _, v := range proxy.JoyCodeLabelToID {
		if strings.ToLower(v) == low {
			return true
		}
	}
	for _, v := range proxy.DevEcoLabelToID {
		if strings.ToLower(v) == low {
			return true
		}
	}
	return true // 宽松模式：允许不认识的模型名（可能是新增的）
}
