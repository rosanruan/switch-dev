package service

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"

	"switchdev/autostart"
	"switchdev/config"
	"switchdev/db"
	"switchdev/proxy"
)

// ConfigService 配置管理服务（暴露给前端）
type ConfigService struct {
	mgr  *config.Manager
	core *Core

	// 模型列表缓存
	modelsMu       sync.RWMutex
	modelsCache    []UpstreamModels
	modelsCacheAt  time.Time
	modelsFetching bool
}

// invalidateModelsCache 由 ConfigService 初始化时注入，供供应商变化时清空模型缓存。
// 用包级钩子避免 ProviderAPIService 反向依赖 ConfigService 实例。
var invalidateModelsCache func()

// NewConfigServiceWithCore 创建配置服务（持有 Core 以访问 upstream）
func NewConfigServiceWithCore(mgr *config.Manager, core *Core) *ConfigService {
	s := &ConfigService{mgr: mgr, core: core}
	// 注册模型缓存失效钩子：供应商增删/模型变化（ProviderAPIService.refresh）时
	// 清空 10 分钟缓存，否则设置页 upstream 下拉要等缓存过期才出现新供应商。
	invalidateModelsCache = s.invalidateModelCache
	return s
}

// 兼容旧调用（无 core，模型列表退化为本地白名单）
func NewConfigService(mgr *config.Manager) *ConfigService {
	return &ConfigService{mgr: mgr}
}

// GetConfig 获取当前配置（克隆，前端可安全读取）
func (s *ConfigService) GetConfig() *config.Config {
	return s.mgr.Get()
}

// SaveConfig 保存配置（校验 + 写盘 + 热加载），端口变化时重启代理
func (s *ConfigService) SaveConfig(cfg *config.Config) error {
	oldPort := s.mgr.Get().Port
	if err := s.mgr.SaveConfig(cfg); err != nil {
		return err
	}
	// 端口变了：停旧代理，用新端口重启
	if cfg.Port != oldPort {
		if err := s.restartProxyOnPort(cfg.Port); err != nil {
			return fmt.Errorf("配置已保存，但代理切换端口失败: %w", err)
		}
	}
	// 同步操作系统登录自启项（按配置注册/注销）
	if err := s.syncAutoStart(cfg.AutoStart); err != nil {
		// 自启写失败不阻断配置保存，仅把错误返回给前端提示
		s.emitConfigChange()
		return fmt.Errorf("配置已保存，但设置开机自启失败: %w", err)
	}
	s.emitConfigChange()
	return nil
}

// ResetConfig 重置为默认配置，端口变化时重启代理
func (s *ConfigService) ResetConfig() error {
	oldPort := s.mgr.Get().Port
	if err := s.mgr.ResetConfig(); err != nil {
		return err
	}
	newPort := s.mgr.Get().Port
	if newPort != oldPort {
		if err := s.restartProxyOnPort(newPort); err != nil {
			return fmt.Errorf("配置已重置，但代理切换端口失败: %w", err)
		}
	}
	// 重置后自启应关闭：注销系统登录项（失败不阻断重置）
	_ = s.syncAutoStart(false)
	s.emitConfigChange()
	return nil
}

// ====== 运行模式方案（Preset）======
//
// 方案是快照语义：保存冻结当前配置，切换覆盖回当前配置。
// 方案不含 port/apiKey，所以这几个操作都不需要重启代理。

// SavePreset 把当前运行模式配置存为方案；同名覆盖
func (s *ConfigService) SavePreset(name string) error {
	if err := s.mgr.SavePreset(name); err != nil {
		return err
	}
	s.emitConfigChange()
	return nil
}

// ApplyPreset 应用方案（立即生效）
func (s *ConfigService) ApplyPreset(name string) error {
	if err := s.mgr.ApplyPreset(name); err != nil {
		return err
	}
	s.emitConfigChange()
	return nil
}

// DeletePreset 删除方案
func (s *ConfigService) DeletePreset(name string) error {
	if err := s.mgr.DeletePreset(name); err != nil {
		return err
	}
	s.emitConfigChange()
	return nil
}

// RenamePreset 重命名方案
func (s *ConfigService) RenamePreset(oldName, newName string) error {
	if err := s.mgr.RenamePreset(oldName, newName); err != nil {
		return err
	}
	s.emitConfigChange()
	return nil
}

// ClearActivePreset 清除当前激活方案标记（切换到"自定义"状态，配置内容不变）
func (s *ConfigService) ClearActivePreset() error {
	cur := s.mgr.Get()
	cur.ActivePreset = ""
	if err := s.mgr.SaveConfig(cur); err != nil {
		return err
	}
	s.emitConfigChange()
	return nil
}

// restartProxyOnPort 停止当前代理，用新端口重启
func (s *ConfigService) restartProxyOnPort(port int) error {
	if s.core == nil || s.core.server == nil {
		return nil
	}
	s.core.server.Stop()
	s.core.server.Port = port
	return s.core.server.Start()
}

// emitConfigChange 推送配置变化事件，并同步推送代理状态（mode 等字段依赖 config）
func (s *ConfigService) emitConfigChange() {
	app := application.Get()
	if app == nil {
		return
	}
	app.Event.Emit("config:change", s.mgr.Get())
	// 代理状态里的 mode 来自 config，需同步刷新让仪表盘立即更新
	if s.core != nil && s.core.server != nil {
		app.Event.Emit("proxy:status", s.core.server.GetStatus())
	}
}

// UpstreamModels 单个 upstream 的可选模型
type UpstreamModels struct {
	Upstream string        `json:"upstream"`
	Source   string        `json:"source"` // "live"（接口实时）| "local"（本地白名单兜底）
	Models   []ModelOption `json:"models"`
}

// ModelOption 模型选项
type ModelOption struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	Context   int    `json:"context,omitempty"`
	Output    int    `json:"output,omitempty"`
	Stream    bool   `json:"stream"`
	Vision    bool   `json:"vision,omitempty"`
	ToolCall  bool   `json:"toolCall,omitempty"`
	Reasoning bool   `json:"reasoning,omitempty"`
	Free      bool   `json:"free,omitempty"` // 限时免费标识
}

// GetAvailableModels 返回各 upstream 可选模型（实时合并 + 缓存 10 分钟）
// 若 core 未注入，回退本地白名单
func (s *ConfigService) GetAvailableModels() []UpstreamModels {
	// 缓存命中（10 分钟内）
	s.modelsMu.RLock()
	if s.modelsCache != nil && time.Since(s.modelsCacheAt) < 10*time.Minute {
		cache := s.modelsCache
		s.modelsMu.RUnlock()
		return cache
	}
	s.modelsMu.RUnlock()

	if s.core == nil {
		// 退化为本地白名单
		return localOnlyModels()
	}

	// 并发拉取三个上游
	result := s.fetchAllModels()

	s.modelsMu.Lock()
	s.modelsCache = result
	s.modelsCacheAt = time.Now()
	s.modelsMu.Unlock()
	return result
}

// InvalidateModelCache 清空模型列表缓存（供应商增删/模型变化时调用）
func (s *ConfigService) InvalidateModelCache() {
	s.invalidateModelCache()
}

func (s *ConfigService) invalidateModelCache() {
	s.modelsMu.Lock()
	s.modelsCache = nil
	s.modelsCacheAt = time.Time{}
	s.modelsMu.Unlock()
}

// RefreshModels 强制刷新模型列表（忽略缓存）
// 同时刷新内置上游的持久化目录，让 /v1/models 和路由层也拿到最新模型
func (s *ConfigService) RefreshModels() []UpstreamModels {
	s.modelsMu.Lock()
	s.modelsCache = nil
	s.modelsMu.Unlock()

	if s.core == nil {
		return localOnlyModels()
	}
	s.refreshCatalog(context.Background())
	result := s.fetchAllModels()
	s.modelsMu.Lock()
	s.modelsCache = result
	s.modelsCacheAt = time.Now()
	s.modelsMu.Unlock()
	return result
}

// fetchAllModels 并发拉取四上游模型，合并本地映射 + 供应商模型，返回结果
func (s *ConfigService) fetchAllModels() []UpstreamModels {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	results := s.fetchUpstreamModels(ctx)

	out := make([]UpstreamModels, 0, len(results)+1)
	for _, r := range results {
		merged := proxy.MergeModels(r.upstream, r.models, r.ok)
		opts := make([]ModelOption, 0, len(merged))
		for _, m := range merged {
			opts = append(opts, ModelOption{
				ID:        m.ID,
				Label:     m.Label,
				Context:   m.Context,
				Output:    m.Output,
				Stream:    m.Stream,
				Vision:    m.Vision,
				ToolCall:  m.ToolCall,
				Reasoning: m.Reasoning,
				Free:      m.Free,
			})
		}
		source := "live"
		if !r.ok {
			source = "local"
		}
		out = append(out, UpstreamModels{Upstream: r.upstream, Source: source, Models: opts})
	}

	// 供应商模型（按 provider 分组，已验证的模型）
	out = append(out, providerAPIModelsGrouped()...)
	return out
}

// providerAPIModelsGrouped 把已注册的免费模型按 provider 分组为 UpstreamModels
func providerAPIModelsGrouped() []UpstreamModels {
	grouped := map[string][]ModelOption{}
	for _, fm := range proxy.ProviderModelList() {
		grouped[fm.ProviderID] = append(grouped[fm.ProviderID], ModelOption{
			ID:       fm.InternalID,
			Label:    fm.Label,
			Context:  fm.Context,
			Stream:   true,
			ToolCall: true,
			Free:     true,
		})
	}
	out := make([]UpstreamModels, 0, len(grouped))
	for pid, opts := range grouped {
		out = append(out, UpstreamModels{Upstream: pid, Source: "free", Models: opts})
	}
	return out
}

// localOnlyModels 目录快照（core 未注入时用；目录本身已含种子兜底）
func localOnlyModels() []UpstreamModels {
	out := make([]UpstreamModels, 0, 4)
	for _, up := range []string{"joycode", "deveco", "opencode", "workbuddy"} {
		out = append(out, UpstreamModels{
			Upstream: up,
			Source:   catalogSource(up),
			Models:   catalogOptions(up),
		})
	}
	return out
}

// catalogOptions 取某上游目录条目的前端选项
func catalogOptions(upstreamName string) []ModelOption {
	models := proxy.CatalogModelsByUpstream(upstreamName)
	opts := make([]ModelOption, 0, len(models))
	for _, m := range models {
		opts = append(opts, ModelOption{
			ID: m.ID, Label: m.Label, Context: m.Context, Output: m.Output,
			Stream: m.Stream, Vision: m.Vision, ToolCall: m.ToolCall,
			Reasoning: m.Reasoning, Free: m.Free,
		})
	}
	return opts
}

// catalogSource 某上游目录里是否含实时数据（任一条目 Live 即视为 live）
func catalogSource(upstreamName string) string {
	for _, m := range proxy.CatalogModelsByUpstream(upstreamName) {
		if m.Live {
			return "live"
		}
	}
	return "local"
}

// GetUASources 列出所有已知的请求来源（User-Agent），用于 UA 路由配置辅助
func (s *ConfigService) GetUASources() []db.SourceInfo {
	if s.core == nil || s.core.DB() == nil {
		return nil
	}
	out, err := s.core.DB().ListSources()
	if err != nil {
		return nil
	}
	return out
}

// GetModelsByUASource 查询某个 source 历史请求过的模型（按次数降序）
func (s *ConfigService) GetModelsByUASource(sourceName string) []db.SourceModelStat {
	if s.core == nil || s.core.DB() == nil {
		return nil
	}
	out, err := s.core.DB().QueryModelsBySource(sourceName)
	if err != nil {
		return nil
	}
	return out
}

// ===== 开机自启动 =====

// syncAutoStart 按 enabled 注册/注销操作系统登录自启项。
func (s *ConfigService) syncAutoStart(enabled bool) error {
	a := autostart.CurrentApp()
	if enabled {
		return a.Enable()
	}
	return a.Disable()
}

// GetAutoStart 返回当前是否已开启开机自启（以系统实际注册状态为准，防止配置与系统不一致）。
func (s *ConfigService) GetAutoStart() bool {
	return autostart.CurrentApp().IsEnabled()
}

// SetAutoStart 切换开机自启，并把结果写回配置。
func (s *ConfigService) SetAutoStart(enabled bool) error {
	if err := s.syncAutoStart(enabled); err != nil {
		return err
	}
	cfg := s.mgr.Get().Clone()
	cfg.AutoStart = enabled
	if err := s.mgr.SaveConfig(cfg); err != nil {
		return err
	}
	s.emitConfigChange()
	return nil
}

// ReconcileAutoStart 启动时调用：让系统自启状态与配置一致（配置优先）。
// 用当前可执行路径重新注册，修复「程序移动位置后旧自启项失效」。
func (s *ConfigService) ReconcileAutoStart() error {
	return s.syncAutoStart(s.mgr.Get().AutoStart)
}
