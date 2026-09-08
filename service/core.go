package service

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"

	"switchdev/config"
	"switchdev/creds"
	"switchdev/db"
	"switchdev/proxy"
	"switchdev/upstream"
)

// Core 共享核心：持有代理服务、三上游、日志存储，并实现 proxy.EventLogger
type Core struct {
	mu        sync.RWMutex
	server    *proxy.Server
	joycode   *upstream.JoyCodeUpstream
	deveco    *upstream.DevEcoUpstream
	opencode  *upstream.OpenCodeUpstream
	workbuddy *upstream.WorkBuddyUpstream
	db        *db.DB
	cfgMgr    *config.Manager // 配置管理器（上游开关等读写）

	// 凭据管理器（用于刷新操作）
	joycodeMgr   *creds.JoyCodeCredManager
	devecoMgr    *creds.DevEcoCredManager
	opencodeMgr  *creds.OpenCodeCredManager
	workbuddyMgr *creds.WorkBuddyCredManager

	// 请求日志环形 buffer
	logMu     sync.RWMutex
	logs      []*proxy.LogEntry
	logMaxLen int

	// 统计
	statsMu     sync.RWMutex
	totalReqs   int64
	successReqs int64
	errorReqs   int64
	authErrReqs int64
}

func NewCore() *Core {
	return &Core{
		logMaxLen: 500,
	}
}

// SetDB 注入数据库实例
func (c *Core) SetDB(d *db.DB) { c.db = d }

// SetConfigManager 注入配置管理器（上游开关读写用）
func (c *Core) SetConfigManager(m *config.Manager) { c.cfgMgr = m }

// SetUpstreamEnabled 设置上游启用开关（全局生效，禁用后调用时跳过其下所有模型）
func (c *Core) SetUpstreamEnabled(name string, enabled bool) error {
	if c.cfgMgr == nil {
		return fmt.Errorf("配置管理器未初始化")
	}
	cfg := c.cfgMgr.Get()
	if cfg.Upstreams == nil {
		cfg.Upstreams = map[string]config.UpstreamSettings{}
	}
	s := cfg.Upstreams[name]
	s.Enabled = enabled
	cfg.Upstreams[name] = s
	if err := c.cfgMgr.SaveConfig(cfg); err != nil {
		return err
	}
	// 推送凭据状态（含 enabled），让前端卡片即时刷新
	c.EmitEvent("cred:change", c.GetCredStatus())
	return nil
}

// DB 返回数据库实例（可能为 nil，表示未初始化）
func (c *Core) DB() *db.DB { return c.db }

// Setup 初始化所有组件（在 main 中调用）
func (c *Core) Setup(
	jyMgr *creds.JoyCodeCredManager,
	deMgr *creds.DevEcoCredManager,
	ocMgr *creds.OpenCodeCredManager,
	wbMgr *creds.WorkBuddyCredManager,
	jy *upstream.JoyCodeUpstream,
	de *upstream.DevEcoUpstream,
	oc *upstream.OpenCodeUpstream,
	wb *upstream.WorkBuddyUpstream,
	server *proxy.Server,
) {
	c.joycodeMgr = jyMgr
	c.devecoMgr = deMgr
	c.opencodeMgr = ocMgr
	c.workbuddyMgr = wbMgr
	c.joycode = jy
	c.deveco = de
	c.opencode = oc
	c.workbuddy = wb
	c.server = server
	// 把 Core 注册为代理的事件日志器
	server.Logger = c
}

// Server 暴露代理服务
func (c *Core) Server() *proxy.Server { return c.server }

// Upstreams 暴露四上游适配器（供 ConfigService 拉取模型列表）。
// Setup 未调用时字段是 nil 的具体类型指针，直接返回会得到「非 nil 接口包着 nil 指针」，
// 调用方的 u == nil 判空拦不住，方法里解引用就 panic。这里显式转成 nil 接口。
func (c *Core) Upstreams() (upstream.Upstream, upstream.Upstream, upstream.Upstream, upstream.Upstream) {
	var jy, de, oc, wb upstream.Upstream
	if c.joycode != nil {
		jy = c.joycode
	}
	if c.deveco != nil {
		de = c.deveco
	}
	if c.opencode != nil {
		oc = c.opencode
	}
	if c.workbuddy != nil {
		wb = c.workbuddy
	}
	return jy, de, oc, wb
}

// ====== proxy.EventLogger 实现 ======

// RecordLog 记录请求日志（由代理调用）+ 推送到前端
func (c *Core) RecordLog(entry *proxy.LogEntry) {
	c.logMu.Lock()
	c.logs = append(c.logs, entry)
	if len(c.logs) > c.logMaxLen {
		c.logs = c.logs[len(c.logs)-c.logMaxLen:]
	}
	c.logMu.Unlock()

	// 更新统计
	c.statsMu.Lock()
	c.totalReqs++
	switch entry.Status {
	case "success":
		c.successReqs++
	case "error":
		c.errorReqs++
	case "auth_error":
		c.authErrReqs++
	}
	c.statsMu.Unlock()

	// 实时推送到前端
	c.EmitEvent("log:new", entry)

	// 异步持久化到 SQLite + 累计用量统计表
	if c.db != nil {
		go func(e *proxy.LogEntry) {
			if err := c.db.InsertLog(e); err != nil {
				fmt.Fprintf(os.Stderr, "[db] 写入日志失败: %v\n", err)
			}
			// 统计表独立累积：清空 logs 不影响统计。失败仅记日志，不影响主流程。
			if err := c.db.UpsertUsageStat(e); err != nil {
				fmt.Fprintf(os.Stderr, "[db] 累计用量统计失败: %v\n", err)
			}
		}(entry)
	}
}

// EmitEvent 推送事件到前端
func (c *Core) EmitEvent(event string, data interface{}) {
	app := application.Get()
	if app == nil {
		return
	}
	app.Event.Emit(event, data)
}

// ====== 日志查询 ======

// GetRecentLogs 获取最近 N 条日志
func (c *Core) GetRecentLogs(count int) []*proxy.LogEntry {
	c.logMu.RLock()
	defer c.logMu.RUnlock()
	if count <= 0 || count > len(c.logs) {
		count = len(c.logs)
	}
	// 返回倒序（最新在前）
	result := make([]*proxy.LogEntry, count)
	for i := 0; i < count; i++ {
		result[i] = c.logs[len(c.logs)-1-i]
	}
	return result
}

// ClearLogs 清空日志
func (c *Core) ClearLogs() {
	c.logMu.Lock()
	c.logs = nil
	c.logMu.Unlock()
}

// LogStats 日志统计
type LogStats struct {
	Total     int64 `json:"total"`
	Success   int64 `json:"success"`
	Error     int64 `json:"error"`
	AuthError int64 `json:"authError"`
}

// GetLogStats 获取统计
func (c *Core) GetLogStats() *LogStats {
	c.statsMu.RLock()
	defer c.statsMu.RUnlock()
	return &LogStats{
		Total:     c.totalReqs,
		Success:   c.successReqs,
		Error:     c.errorReqs,
		AuthError: c.authErrReqs,
	}
}

// ====== 凭据状态汇总 ======

// AllCredStatus 四上游 + 免费 API 凭据状态
type AllCredStatus struct {
	JoyCode      *creds.CredStatusInfo            `json:"joycode"`
	DevEco       *creds.CredStatusInfo            `json:"deveco"`
	OpenCode     *creds.CredStatusInfo            `json:"opencode"`
	WorkBuddy    *creds.CredStatusInfo            `json:"workbuddy"`
	ProviderAPIs map[string]*creds.CredStatusInfo `json:"providerAPIs,omitempty"` // 免费 API 供应商（动态）
}

// GetCredStatus 获取全部凭据状态
func (c *Core) GetCredStatus() *AllCredStatus {
	c.mu.RLock()
	server := c.server
	c.mu.RUnlock()

	// 启用状态来自配置（缺省视为启用）；逐上游填入 CredStatusInfo
	enabled := func(name string) bool {
		if server == nil || server.ConfigResolver == nil {
			return true
		}
		return server.ConfigResolver.IsUpstreamEnabled(name)
	}

	status := &AllCredStatus{
		JoyCode:   c.joycode.CredStatus(),
		DevEco:    c.deveco.CredStatus(),
		OpenCode:  c.opencode.CredStatus(),
		WorkBuddy: c.workbuddy.CredStatus(),
	}
	status.JoyCode.Enabled = enabled("joycode")
	status.DevEco.Enabled = enabled("deveco")
	status.OpenCode.Enabled = enabled("opencode")
	status.WorkBuddy.Enabled = enabled("workbuddy")
	if server != nil {
		providerAPIs := server.GetProviderAPIs()
		if len(providerAPIs) > 0 {
			status.ProviderAPIs = map[string]*creds.CredStatusInfo{}
			for pid, up := range providerAPIs {
				st := up.CredStatus()
				st.Enabled = enabled(pid)
				status.ProviderAPIs[pid] = st
			}
		}
	}
	return status
}

// ProviderAPIUpstreams 返回免费 API 上游集合（key = provider id）
func (c *Core) ProviderAPIUpstreams() map[string]upstream.Upstream {
	c.mu.RLock()
	server := c.server
	c.mu.RUnlock()
	if server == nil {
		return nil
	}
	return server.GetProviderAPIs()
}

// RefreshCreds 强制刷新某上游凭据
func (c *Core) RefreshCreds(name string) error {
	var err error
	switch name {
	case "joycode":
		c.joycode.InvalidateCreds()
		_, err = c.joycodeMgr.EnsureCreds()
	case "deveco":
		c.deveco.InvalidateCreds()
		_, err = c.devecoMgr.EnsureCreds()
	case "opencode":
		c.opencode.InvalidateCreds()
		_, err = c.opencodeMgr.EnsureCreds()
	case "workbuddy":
		c.workbuddy.InvalidateCreds()
		_, err = c.workbuddyMgr.EnsureCreds()
	default:
		return nil
	}
	// 刷新后推送状态，让前端凭据卡片即时反映（文件被删/登录失效等）
	c.EmitEvent("cred:change", c.GetCredStatus())
	return err
}

// RefreshAllCreds 刷新全部凭据
func (c *Core) RefreshAllCreds() {
	c.RefreshCreds("joycode")
	c.RefreshCreds("deveco")
	c.RefreshCreds("opencode")
	c.RefreshCreds("workbuddy")
}

// WatchInstalledAgents 后台周期探测各 agent 安装状态。
// 检测到「未安装 -> 已安装」时自动校验该上游凭据并推送状态，
// 让引导界面无需重启即可反映新安装的工具。filesystem/PATH 探测很廉价，
// 每 interval 扫一次即可。应在独立 goroutine 中运行，ctx 取消时退出。
func (c *Core) WatchInstalledAgents(ctx context.Context, interval time.Duration) {
	// 已探测为「已安装」的 upstream 集合，避免重复校验
	installed := map[string]bool{}
	for _, a := range creds.AgentRegistry {
		if creds.IsAgentInstalled(&a) {
			installed[a.Upstream] = true
		}
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			changed := false
			for i := range creds.AgentRegistry {
				a := &creds.AgentRegistry[i]
				now := creds.IsAgentInstalled(a)
				if now && !installed[a.Upstream] {
					// 新安装：触发凭据校验（失败仅记录，等待用户登录）
					_ = c.RefreshCreds(a.Upstream)
					changed = true
				}
				installed[a.Upstream] = now
			}
			if changed {
				c.EmitEvent("cred:change", c.GetCredStatus())
			}
		}
	}
}

// emitCredChange 推送凭据状态变化
func (c *Core) emitCredChange() {
	c.EmitEvent("cred:change", c.GetCredStatus())
}

// EmitProviderHealth 免费模型健康状态变化回调（实现 providerapi.HealthReporter）
func (c *Core) EmitProviderHealth(health map[string]map[string]bool) {
	c.EmitEvent("providerapi:health", health)
	// 同时刷新凭据状态（模型健康变化影响可用性展示）
	c.EmitEvent("cred:change", c.GetCredStatus())
}

// timestampNow 当前时间字符串（避免在 hot path 反复格式化）
func timestampNow() string {
	return time.Now().Format("15:04:05")
}
