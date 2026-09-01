package service

import (
	"switchdev/proxy"
)

// ProxyService 代理控制服务（暴露给前端）
type ProxyService struct {
	core *Core
}

func NewProxyService(core *Core) *ProxyService {
	return &ProxyService{core: core}
}

// StartProxy 启动代理
func (s *ProxyService) StartProxy(port int) error {
	if port > 0 {
		s.core.server.Port = port
	}
	return s.core.server.Start()
}

// StopProxy 停止代理
func (s *ProxyService) StopProxy() error {
	return s.core.server.Stop()
}

// RestartProxy 重启代理
func (s *ProxyService) RestartProxy() error {
	if err := s.core.server.Stop(); err != nil {
		// 忽略错误，继续启动
	}
	return s.core.server.Start()
}

// GetStatus 获取代理运行状态
func (s *ProxyService) GetStatus() *proxy.ProxyStatus {
	return s.core.server.GetStatus()
}

// GetCredStatus 获取三上游凭据状态（合并到代理服务方便前端一次拉取）
func (s *ProxyService) GetCredStatus() *AllCredStatus {
	return s.core.GetCredStatus()
}

// SetPort 修改端口（需重启生效）
func (s *ProxyService) SetPort(port int) {
	s.core.server.Port = port
}

// GetDashboard 获取仪表盘聚合数据（状态 + 凭据 + 统计 + 最近日志）
func (s *ProxyService) GetDashboard() *Dashboard {
	status := s.core.server.GetStatus()
	// 用数据库真实总数覆盖内存计数器（重启后仍准确）
	if s.core.db != nil {
		if total, err := s.core.db.CountLogs(); err == nil {
			status.Requests = total
		}
	}
	return &Dashboard{
		Proxy:      status,
		Creds:      s.core.GetCredStatus(),
		Stats:      s.core.GetLogStats(),
		RecentLogs: s.core.GetRecentLogs(10),
	}
}

// Dashboard 仪表盘聚合数据
type Dashboard struct {
	Proxy      *proxy.ProxyStatus `json:"proxy"`
	Creds      *AllCredStatus     `json:"creds"`
	Stats      *LogStats          `json:"stats"`
	RecentLogs []*proxy.LogEntry  `json:"recentLogs"`
}