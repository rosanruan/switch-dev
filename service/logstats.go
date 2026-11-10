package service

import (
	"sort"
	"strings"
	"time"

	"switchdev/db"
	"switchdev/proxy"
)

// UsageStats 使用统计结果
type UsageStats struct {
	StartDate   string          `json:"startDate"`
	EndDate     string          `json:"endDate"`
	TotalTokens int64           `json:"totalTokens"`
	TotalInput  int64           `json:"totalInput"`
	TotalOutput int64           `json:"totalOutput"`
	TotalCost   float64         `json:"totalCost"`
	TotalReqs   int64           `json:"totalReqs"`
	SuccessReqs int64           `json:"successReqs"`
	ByProvider  []ProviderUsage `json:"byProvider"`
	ByModel     []ModelUsage    `json:"byModel"`
}

// ProviderUsage 供应商（上游）维度统计
type ProviderUsage struct {
	Provider      string  `json:"provider"`      // 上游标识 joycode/deveco/.../供应商ID
	ProviderLabel string  `json:"providerLabel"` // 显示名
	Tokens        int64   `json:"tokens"`
	Input         int64   `json:"input"`
	Output        int64   `json:"output"`
	Cost          float64 `json:"cost"`
	Requests      int64   `json:"requests"`
	SuccessReqs   int64   `json:"successReqs"`
}

// ModelUsage 模型维度统计
type ModelUsage struct {
	Model      string  `json:"model"`      // 实际用到的模型（内部 id，如 provider/<pid>/<mid>）
	ModelLabel string  `json:"modelLabel"` // 显示名（免费模型会带上供应商名）
	Tokens     int64   `json:"tokens"`
	Input      int64   `json:"input"`
	Output     int64   `json:"output"`
	Cost       float64 `json:"cost"`
	Requests   int64   `json:"requests"`
	Percent    float64 `json:"percent"` // token 占比 0-100
}

// builtInProviderLabel 内置上游的显示名（统一用英文 name）
func builtInProviderLabel(up string) string {
	switch up {
	case "joycode", "deveco", "opencode", "workbuddy", "mcloud":
		return up
	default:
		return ""
	}
}

// providerLabel 把上游标识解析为显示名：
// 内置 4 上游用固定中文名；免费供应商优先用 nameProvider 注入的供应商名（来自配置），
// 取不到时回退到 id。
func providerLabel(up string, nameProvider func(string) string) string {
	if label := builtInProviderLabel(up); label != "" {
		return label
	}
	if nameProvider != nil {
		if name := nameProvider(up); name != "" && name != up {
			return name
		}
	}
	return up
}

// ComputeUsageStats 按日期范围统计使用情况。
// nameProvider 用于把免费供应商 id 解析成显示名（可为 nil，此时仅内置上游有中文名）。
func ComputeUsageStats(startDate, endDate string, nameProvider func(string) string) *UsageStats {
	// 默认最近 7 天
	if startDate == "" || endDate == "" {
		endDate = time.Now().Format("2006-01-02")
		startDate = time.Now().AddDate(0, 0, -6).Format("2006-01-02")
	}

	logs := getLogsByRange(startDate, endDate, 0) // 0 = 不限制数量，统计全部

	stats := &UsageStats{
		StartDate: startDate,
		EndDate:   endDate,
	}

	providerMap := map[string]*ProviderUsage{}
	modelMap := map[string]*ModelUsage{}

	for _, log := range logs {
		// 只统计成功请求
		if log.Status != "success" {
			continue
		}
		in := int64(log.InputTokens)
		out := int64(log.OutputTokens)
		// 空 token 且非成功则跳过；成功但 0 token 也计入请求数
		if in == 0 && out == 0 && log.InputTokens == 0 && log.OutputTokens == 0 {
			// 保留请求计数，token 为 0
		}

		// 供应商维度（即日志里的 upstream）
		providerKey := log.Upstream
		if providerKey == "" {
			providerKey = "unknown"
		}
		pg := providerMap[providerKey]
		if pg == nil {
			pg = &ProviderUsage{Provider: providerKey, ProviderLabel: providerLabel(providerKey, nameProvider)}
			providerMap[providerKey] = pg
		}
		pg.Tokens += in + out
		pg.Input += in
		pg.Output += out
		pg.Cost += log.Cost
		pg.Requests++
		pg.SuccessReqs++

		// 模型维度：按"实际发往 base_url 的 model 名"聚合，跨供应商同名模型合并。
		// 大小写不敏感（DevEco 网关用 GLM-5.1、WorkBuddy 用 glm-5.1，应合并为 glm-5.1）。
		usedModel := log.UsedModel
		if usedModel == "" {
			usedModel = log.Model
		}
		if usedModel == "" || usedModel == "auto" {
			usedModel = "未知"
		}
		actualModel := strings.ToLower(proxy.ActualUpstreamModel(usedModel))
		mu := modelMap[actualModel]
		if mu == nil {
			mu = &ModelUsage{Model: actualModel, ModelLabel: actualModel}
			modelMap[actualModel] = mu
		}
		mu.Tokens += in + out
		mu.Input += in
		mu.Output += out
		mu.Cost += log.Cost
		mu.Requests++
	}

	// 汇总
	for _, pg := range providerMap {
		stats.TotalTokens += pg.Tokens
		stats.TotalInput += pg.Input
		stats.TotalOutput += pg.Output
		stats.TotalCost += pg.Cost
		stats.TotalReqs += pg.Requests
		stats.SuccessReqs += pg.SuccessReqs
	}

	// 供应商按 token 降序
	for _, pg := range providerMap {
		stats.ByProvider = append(stats.ByProvider, *pg)
	}
	sort.Slice(stats.ByProvider, func(i, j int) bool {
		return stats.ByProvider[i].Tokens > stats.ByProvider[j].Tokens
	})

	// 模型按 token 降序 + 算占比
	for _, mu := range modelMap {
		if stats.TotalTokens > 0 {
			mu.Percent = float64(mu.Tokens) / float64(stats.TotalTokens) * 100
		}
		stats.ByModel = append(stats.ByModel, *mu)
	}
	sort.Slice(stats.ByModel, func(i, j int) bool {
		return stats.ByModel[i].Tokens > stats.ByModel[j].Tokens
	})

	return stats
}

// GetUsageStats 暴露给前端的统计方法（优先从 db 查，回退到文件扫描）
func (s *LogService) GetUsageStats(startDate, endDate string) *UsageStats {
	if s.core.DB() != nil {
		dbStats, err := s.core.DB().ComputeUsageStats(startDate, endDate)
		if err == nil && dbStats != nil {
			return convertDBUsageStats(dbStats)
		}
	}
	return ComputeUsageStats(startDate, endDate, s.providerNamer())
}

// convertDBUsageStats 将 db.UsageStats 转换为 service.UsageStats
func convertDBUsageStats(d *db.UsageStats) *UsageStats {
	stats := &UsageStats{
		StartDate:   d.StartDate,
		EndDate:     d.EndDate,
		TotalTokens: d.TotalTokens,
		TotalInput:  d.TotalInput,
		TotalOutput: d.TotalOutput,
		TotalCost:   d.TotalCost,
		TotalReqs:   d.TotalReqs,
		SuccessReqs: d.SuccessReqs,
	}
	for _, p := range d.ByProvider {
		stats.ByProvider = append(stats.ByProvider, ProviderUsage{
			Provider:      p.Provider,
			ProviderLabel: p.ProviderLabel,
			Tokens:        p.Tokens,
			Input:         p.Input,
			Output:        p.Output,
			Cost:          p.Cost,
			Requests:      p.Requests,
			SuccessReqs:   p.SuccessReqs,
		})
	}
	for _, m := range d.ByModel {
		stats.ByModel = append(stats.ByModel, ModelUsage{
			Model:      m.Model,
			ModelLabel: m.ModelLabel,
			Tokens:     m.Tokens,
			Input:      m.Input,
			Output:     m.Output,
			Cost:       m.Cost,
			Requests:   m.Requests,
			Percent:    m.Percent,
		})
	}
	return stats
}

// providerNamer 返回一个把免费供应商 id 解析为显示名的闭包（数据来自运行中的代理）。
func (s *LogService) providerNamer() func(string) string {
	return func(id string) string {
		ups := s.core.ProviderAPIUpstreams()
		if ups == nil {
			return id
		}
		u, ok := ups[id]
		if !ok {
			return id
		}
		return u.CredStatus().Source
	}
}

// TodaySummary 今日使用概览（首页展示）
type TodaySummary struct {
	Tokens int64   `json:"tokens"`
	Cost   float64 `json:"cost"`
}

// GetTodaySummary 今日总消耗 token 和费用（优先从 db 查，回退到文件扫描）
func (s *LogService) GetTodaySummary() *TodaySummary {
	today := time.Now().Format("2006-01-02")
	if s.core.DB() != nil {
		if dbStats, err := s.core.DB().ComputeUsageStats(today, today); err == nil && dbStats != nil {
			return &TodaySummary{
				Tokens: dbStats.TotalTokens,
				Cost:   dbStats.TotalCost,
			}
		}
	}
	stats := ComputeUsageStats(today, today, s.providerNamer())
	return &TodaySummary{
		Tokens: stats.TotalTokens,
		Cost:   stats.TotalCost,
	}
}
