package service

import (
	"context"
	"testing"

	"switchdev/db"
	"switchdev/proxy"
)

// TestCatalogEntriesToModelsSkipsAutoAndEmpty auto 虚拟模型和空 id 不进目录
func TestCatalogEntriesToModelsSkipsAutoAndEmpty(t *testing.T) {
	got := catalogEntriesToModels([]db.CatalogEntry{
		{UpstreamName: "deveco", ModelID: "auto", Label: "Auto"},
		{UpstreamName: "deveco", ModelID: "", Label: ""},
		{UpstreamName: "deveco", ModelID: "glm-5.1", WireName: "GLM-5.1", Label: "GLM-5.1"},
	})
	if len(got) != 1 || got[0].ID != "glm-5.1" {
		t.Fatalf("want 仅 glm-5.1, got %+v", got)
	}
	if got[0].WireName != "GLM-5.1" {
		t.Errorf("WireName = %q, want GLM-5.1", got[0].WireName)
	}
}

// TestModelInfosToCatalogEntriesCarriesWire MergeModels 的 Wire 字段要传到 DB 条目
func TestModelInfosToCatalogEntriesCarriesWire(t *testing.T) {
	got := modelInfosToCatalogEntries("deveco", []proxy.ModelInfo{
		{ID: "auto", Label: "Auto"}, // 应跳过
		{ID: "glm-5.1", Wire: "GLM-5.1", Label: "GLM-5.1 (DevEco)", Context: 170000, Output: 131072, Stream: true, ToolCall: true},
	})
	if len(got) != 1 {
		t.Fatalf("want 1 条, got %d: %+v", len(got), got)
	}
	e := got[0]
	if e.ModelID != "glm-5.1" || e.WireName != "GLM-5.1" {
		t.Errorf("id/wire = %q/%q, want glm-5.1/GLM-5.1", e.ModelID, e.WireName)
	}
	if e.UpstreamName != "deveco" || e.Context != 170000 || !e.Stream || !e.ToolCall {
		t.Errorf("元数据丢失：%+v", e)
	}
}

// TestRefreshCatalogKeepsDataWhenFetchFails 全部上游拉取失败时（无凭据/断网），
// RefreshCatalog 不得清空已落库的目录 —— 否则一次网络抖动就让重启后模型全丢。
func TestRefreshCatalogKeepsDataWhenFetchFails(t *testing.T) {
	d := newCatalogTestDB(t)

	if err := d.SyncUpstreamCatalog("deveco", []db.CatalogEntry{
		{UpstreamName: "deveco", ModelID: "glm-5.1", WireName: "GLM-5.1", Label: "GLM-5.1", Stream: true},
	}); err != nil {
		t.Fatalf("首轮落库: %v", err)
	}

	// core 的四个上游都是 nil（等价于凭据不可用/拉取失败）
	core := NewCore()
	core.SetDB(d)
	svc := NewConfigServiceWithCore(nil, core)

	svc.refreshCatalog(context.Background())

	got, err := d.ListCatalogByUpstream("deveco")
	if err != nil {
		t.Fatalf("回读: %v", err)
	}
	if len(got) != 1 || got[0].ModelID != "glm-5.1" {
		t.Errorf("拉取失败后目录被清空了：%+v", got)
	}

	// 目录快照也应保住 DB 里的那份（回读路径）
	LoadCatalogFromDB(svc)
	if _, ok := proxy.LookupModel("glm-5.1"); !ok {
		t.Error("glm-5.1 应仍在目录快照中")
	}
}

func newCatalogTestDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}
