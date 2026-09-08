package service

import "switchdev/proxy"

// LogService 请求日志服务（暴露给前端）
type LogService struct {
	core *Core
}

func NewLogService(core *Core) *LogService {
	return &LogService{core: core}
}

// GetRecentLogs 获取最近 N 条日志（内存 buffer，倒序，最新在前）
func (s *LogService) GetRecentLogs(count int) []*proxy.LogEntry {
	return s.core.GetRecentLogs(count)
}

// LogPage 一页日志 + 分页元信息
type LogPage struct {
	Logs   []*proxy.LogEntry `json:"logs"`
	Total  int64             `json:"total"`  // 当前范围+状态下的总条数
	Offset int               `json:"offset"` // 本页起始偏移
	Limit  int               `json:"limit"`  // 每页条数
}

// GetLogsByRange 按日期范围查询日志，从 SQLite 读，倒序
// 日期格式 "YYYY-MM-DD"
func (s *LogService) GetLogsByRange(startDate, endDate string, limit int) []*proxy.LogEntry {
	if s.core.DB() == nil {
		return nil
	}
	logs, err := s.core.DB().QueryLogs(startDate, endDate, "", limit, 0)
	if err != nil {
		return nil
	}
	return logs
}

// GetLogPage 分页查询日志。status 为空表示全部；status 非空时筛选下推到 SQL，
// 保证返回的 Total 与页内条目口径一致（否则页内再过滤会与状态计数对不上）。
func (s *LogService) GetLogPage(startDate, endDate, status string, limit, offset int) *LogPage {
	if s.core.DB() == nil {
		return &LogPage{Limit: limit, Offset: offset}
	}
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}

	counts, err := s.core.DB().LogCountsByRange(startDate, endDate)
	if err != nil {
		counts = nil
	}
	var total int64
	if status == "" {
		for _, n := range counts {
			total += n
		}
	} else {
		total = counts[status]
	}

	// offset 越界（如筛选切换后停在末页）时回到最后一页，避免返回空列表
	if total > 0 && int64(offset) >= total {
		lastPage := (total - 1) / int64(limit)
		offset = int(lastPage * int64(limit))
	}

	logs, err := s.core.DB().QueryLogs(startDate, endDate, status, limit, offset)
	if err != nil {
		return &LogPage{Total: total, Limit: limit, Offset: offset}
	}
	return &LogPage{Logs: logs, Total: total, Limit: limit, Offset: offset}
}

// GetLogDates 列出所有有日志的日期（倒序）
func (s *LogService) GetLogDates() []string {
	if s.core.DB() == nil {
		return listLogDates() // 回退到旧 JSONL
	}
	return s.core.DB().QueryLogDates()
}

// ClearLogs 清空内存日志 + 数据库日志
func (s *LogService) ClearLogs() {
	s.core.ClearLogs()
	if s.core.DB() != nil {
		_ = s.core.DB().ClearLogs()
	}
}

// GetLogStats 获取日志统计（进程内累计，重启归零；仪表盘总览用）
func (s *LogService) GetLogStats() *LogStats {
	return s.core.GetLogStats()
}

// LogCountsByRange 按日期范围统计各状态日志数（从数据库读取，重启后仍准确）
// 返回 total/success/error/authError/fallback 计数，供日志页筛选按钮显示。
func (s *LogService) LogCountsByRange(startDate, endDate string) map[string]int64 {
	if s.core.DB() == nil {
		return nil
	}
	counts, err := s.core.DB().LogCountsByRange(startDate, endDate)
	if err != nil {
		return nil
	}
	return counts
}
