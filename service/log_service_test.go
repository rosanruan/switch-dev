package service

import (
	"fmt"
	"testing"

	"switchdev/proxy"
)

func seedServiceLogs(t *testing.T, core *Core, date string, n int, statuses ...string) {
	t.Helper()
	if len(statuses) == 0 {
		statuses = []string{"success"}
	}
	for i := 0; i < n; i++ {
		e := &proxy.LogEntry{
			Date:     date,
			DateTime: fmt.Sprintf("%s %02d:%02d:00", date, i/60, i%60),
			Model:    "m",
			Upstream: "joycode",
			Status:   statuses[i%len(statuses)],
			Duration: 10,
		}
		if err := core.DB().InsertLog(e); err != nil {
			t.Fatalf("InsertLog #%d: %v", i, err)
		}
	}
}

func newLogTestService(t *testing.T) (*LogService, *Core) {
	t.Helper()
	d := newCatalogTestDB(t)
	core := NewCore()
	core.SetDB(d)
	return NewLogService(core), core
}

// TestGetLogPageTotalAndSlicing Total 是范围内总数，页内条目按 limit/offset 切分
func TestGetLogPageTotalAndSlicing(t *testing.T) {
	svc, core := newLogTestService(t)
	seedServiceLogs(t, core, "2026-09-04", 25)

	p := svc.GetLogPage("2026-09-04", "2026-09-04", "", 10, 0)
	if p.Total != 25 {
		t.Errorf("Total = %d, want 25", p.Total)
	}
	if len(p.Logs) != 10 {
		t.Errorf("首页 %d 条, want 10", len(p.Logs))
	}
	if p.Offset != 0 || p.Limit != 10 {
		t.Errorf("Offset/Limit = %d/%d, want 0/10", p.Offset, p.Limit)
	}

	last := svc.GetLogPage("2026-09-04", "2026-09-04", "", 10, 20)
	if len(last.Logs) != 5 {
		t.Errorf("末页 %d 条, want 5", len(last.Logs))
	}
}

// TestGetLogPageStatusFilterTotal 筛选状态时 Total 只统计该状态
// —— 否则页码总数会按全部条数算，翻到后面全是空页。
func TestGetLogPageStatusFilterTotal(t *testing.T) {
	svc, core := newLogTestService(t)
	seedServiceLogs(t, core, "2026-09-04", 20, "success", "error")

	all := svc.GetLogPage("2026-09-04", "2026-09-04", "", 10, 0)
	if all.Total != 20 {
		t.Errorf("不筛选 Total = %d, want 20", all.Total)
	}

	succ := svc.GetLogPage("2026-09-04", "2026-09-04", "success", 10, 0)
	if succ.Total != 10 {
		t.Errorf("success Total = %d, want 10", succ.Total)
	}
	if len(succ.Logs) != 10 {
		t.Errorf("success 首页 %d 条, want 10", len(succ.Logs))
	}
	for _, l := range succ.Logs {
		if l.Status != "success" {
			t.Errorf("筛选结果混入 status=%s", l.Status)
		}
	}
}

// TestGetLogPageClampsOutOfRangeOffset 从「全部第 3 页」切到只有 1 页的筛选时，
// offset 会越界；后端应回退到末页并回传真实 offset，而不是给一个空列表。
func TestGetLogPageClampsOutOfRangeOffset(t *testing.T) {
	svc, core := newLogTestService(t)
	seedServiceLogs(t, core, "2026-09-04", 25)

	p := svc.GetLogPage("2026-09-04", "2026-09-04", "", 10, 200)
	if len(p.Logs) == 0 {
		t.Fatal("越界 offset 应回退到末页，实际返回空列表")
	}
	if p.Offset != 20 {
		t.Errorf("回退后 Offset = %d, want 20（末页起点）", p.Offset)
	}
	if len(p.Logs) != 5 {
		t.Errorf("末页 %d 条, want 5", len(p.Logs))
	}
}

// TestGetLogPageEmptyRange 空范围返回 Total=0 且不 panic
func TestGetLogPageEmptyRange(t *testing.T) {
	svc, _ := newLogTestService(t)

	p := svc.GetLogPage("2026-01-01", "2026-01-01", "", 10, 0)
	if p == nil {
		t.Fatal("GetLogPage 返回 nil")
	}
	if p.Total != 0 || len(p.Logs) != 0 {
		t.Errorf("空范围应 Total=0/0 条，实际 %d/%d", p.Total, len(p.Logs))
	}
}

// TestGetLogPageDefaultLimit limit<=0 时用默认页大小，避免一次拉全表
func TestGetLogPageDefaultLimit(t *testing.T) {
	svc, core := newLogTestService(t)
	seedServiceLogs(t, core, "2026-09-04", 80)

	p := svc.GetLogPage("2026-09-04", "2026-09-04", "", 0, 0)
	if p.Limit != 50 {
		t.Errorf("Limit = %d, want 50（默认）", p.Limit)
	}
	if len(p.Logs) != 50 {
		t.Errorf("返回 %d 条, want 50", len(p.Logs))
	}
}

// TestGetLogPageNoDB DB 不可用时返回空页而非 nil（前端会直接读 .logs）
func TestGetLogPageNoDB(t *testing.T) {
	svc := NewLogService(NewCore())
	p := svc.GetLogPage("2026-09-04", "2026-09-04", "", 10, 0)
	if p == nil {
		t.Fatal("DB 缺失时不应返回 nil")
	}
	if len(p.Logs) != 0 || p.Total != 0 {
		t.Errorf("DB 缺失应返回空页，实际 %d 条/Total=%d", len(p.Logs), p.Total)
	}
}
