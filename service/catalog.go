package service

import (
	"context"
	"sync"
	"time"

	"switchdev/db"
	"switchdev/proxy"
	"switchdev/upstream"
)

// ====== 内置上游模型目录：实时拉取 + 落库 + 启动回读 ======
//
// 数据流：
//   启动   -> LoadCatalogFromDB  -> proxy.SetCatalog（重启后 /v1/models 立即是最新一份）
//   刷新   -> RefreshCatalog     -> 拉四上游 -> 落库 -> proxy.SetCatalog -> emit models:change
//
// proxy 不能 import db（db 已经 import proxy，会成环），所以目录必须在这里装配后
// 注入 proxy —— 与 proxy.SetProviderModels 同一个方向。

// catalogUpstreams 参与目录同步的内置上游（顺序与 Core.Upstreams() 返回一致）
var catalogUpstreams = []string{"joycode", "deveco", "opencode", "workbuddy"}

// upstreamFetch 单个上游的拉取结果
type upstreamFetch struct {
	upstream string
	models   []upstream.FetchedModel
	ok       bool
}

// fetchUpstreamModels 并发拉取四上游模型（共享骨架，GetAvailableModels 与 RefreshCatalog 共用）
func (s *ConfigService) fetchUpstreamModels(ctx context.Context) []upstreamFetch {
	jy, de, oc, wb := s.core.Upstreams()
	ups := []upstream.Upstream{jy, de, oc, wb}

	results := make([]upstreamFetch, len(catalogUpstreams))
	var wg sync.WaitGroup
	for i := range catalogUpstreams {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			name := catalogUpstreams[idx]
			u := ups[idx]
			if u == nil {
				results[idx] = upstreamFetch{upstream: name}
				return
			}
			fetched, err := u.FetchModels(ctx)
			results[idx] = upstreamFetch{upstream: name, models: fetched, ok: err == nil}
		}(i)
	}
	wg.Wait()
	return results
}

// LoadCatalogFromDB 启动时从 DB 回读目录并注入 proxy。
// DB 不可用或表里没有目录数据时静默跳过 —— proxy 会退化到种子白名单。
//
// 刻意写成自由函数而非 ConfigService 方法：Wails 会把 service 的所有导出方法
// 暴露给前端，目录编排是内部逻辑，不该出现在 bindings 里。
func LoadCatalogFromDB(s *ConfigService) {
	if s == nil || s.core == nil || s.core.DB() == nil {
		return
	}
	entries, err := s.core.DB().ListCatalog()
	if err != nil || len(entries) == 0 {
		return
	}
	proxy.SetCatalog(proxy.BuildCatalog(catalogEntriesToModels(entries)))
	s.invalidateModelCache()
}

// refreshCatalog 拉取四上游最新模型，落库并重建目录快照。
// 拉取失败的上游**不写库**，保留上次成功的结果 —— 一次网络抖动不该清空整个上游的目录。
func (s *ConfigService) refreshCatalog(ctx context.Context) {
	if s.core == nil {
		return
	}
	fetchCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	results := s.fetchUpstreamModels(fetchCtx)

	database := s.core.DB()
	for _, r := range results {
		if !r.ok || len(r.models) == 0 {
			continue
		}
		if database == nil {
			continue
		}
		merged := proxy.MergeModels(r.upstream, r.models, true)
		// 落库失败不影响本轮内存快照，下轮刷新会再试
		_ = database.SyncUpstreamCatalog(r.upstream, modelInfosToCatalogEntries(r.upstream, merged))
	}

	// 从 DB 回读重建（DB 不可用时退化为直接用本轮拉取结果）
	if database != nil {
		if entries, err := database.ListCatalog(); err == nil && len(entries) > 0 {
			proxy.SetCatalog(proxy.BuildCatalog(catalogEntriesToModels(entries)))
		}
	} else {
		var live []proxy.CatalogModel
		for _, r := range results {
			if !r.ok || len(r.models) == 0 {
				continue
			}
			merged := proxy.MergeModels(r.upstream, r.models, true)
			live = append(live, modelInfosToCatalogModels(r.upstream, merged)...)
		}
		if len(live) > 0 {
			proxy.SetCatalog(proxy.BuildCatalog(live))
		}
	}

	s.invalidateModelCache()
	s.core.EmitEvent("models:change", nil)
}

// StartCatalogRefresh 后台周期刷新目录（启动延迟一次 + 每 interval 一次）
func StartCatalogRefresh(ctx context.Context, s *ConfigService, interval time.Duration) {
	if s == nil {
		return
	}
	select {
	case <-time.After(8 * time.Second): // 让凭据加载先跑完，避免首轮全部拉取失败
	case <-ctx.Done():
		return
	}
	s.refreshCatalog(ctx)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.refreshCatalog(ctx)
		case <-ctx.Done():
			return
		}
	}
}

// skipCatalogModel auto 虚拟模型和空 id 不进目录（与 db.upsertModelInTx 口径一致）
func skipCatalogModel(id string) bool {
	return id == "" || id == "auto"
}

// modelInfosToCatalogEntries MergeModels 结果 -> DB 目录条目
func modelInfosToCatalogEntries(upstreamName string, models []proxy.ModelInfo) []db.CatalogEntry {
	out := make([]db.CatalogEntry, 0, len(models))
	for _, m := range models {
		if skipCatalogModel(m.ID) {
			continue
		}
		out = append(out, db.CatalogEntry{
			UpstreamName: upstreamName,
			ModelID:      m.ID,
			WireName:     m.Wire,
			Label:        m.Label,
			Context:      m.Context,
			Output:       m.Output,
			Stream:       m.Stream,
			Vision:       m.Vision,
			ToolCall:     m.ToolCall,
			Reasoning:    m.Reasoning,
			Free:         m.Free,
		})
	}
	return out
}

// modelInfosToCatalogModels MergeModels 结果 -> proxy 目录条目（DB 不可用时的直通路径）
func modelInfosToCatalogModels(upstreamName string, models []proxy.ModelInfo) []proxy.CatalogModel {
	out := make([]proxy.CatalogModel, 0, len(models))
	for _, m := range models {
		if skipCatalogModel(m.ID) {
			continue
		}
		out = append(out, proxy.CatalogModel{
			ID: m.ID, Upstream: upstreamName, WireName: m.Wire, Label: m.Label,
			Context: m.Context, Output: m.Output,
			Stream: m.Stream, Vision: m.Vision, ToolCall: m.ToolCall,
			Reasoning: m.Reasoning, Free: m.Free,
		})
	}
	return out
}

// catalogEntriesToModels DB 目录条目 -> proxy 目录条目
func catalogEntriesToModels(entries []db.CatalogEntry) []proxy.CatalogModel {
	out := make([]proxy.CatalogModel, 0, len(entries))
	for _, e := range entries {
		if skipCatalogModel(e.ModelID) {
			continue
		}
		out = append(out, proxy.CatalogModel{
			ID: e.ModelID, Upstream: e.UpstreamName, WireName: e.WireName, Label: e.Label,
			Context: e.Context, Output: e.Output,
			Stream: e.Stream, Vision: e.Vision, ToolCall: e.ToolCall,
			Reasoning: e.Reasoning, Free: e.Free,
		})
	}
	return out
}
