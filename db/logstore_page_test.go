package db

import (
	"fmt"
	"testing"

	"switchdev/proxy"
)

// seedLogs 插入 n 条日志，状态按 statuses 循环取值
func seedLogs(t *testing.T, d *DB, date string, n int, statuses ...string) {
	t.Helper()
	if len(statuses) == 0 {
		statuses = []string{"success"}
	}
	for i := 0; i < n; i++ {
		e := successEntry("m", "m", "joycode", 1, 2, 3)
		e.Date = date
		// date_time 递增，保证 ORDER BY date_time DESC 顺序确定
		e.DateTime = fmt.Sprintf("%s %02d:%02d:00", date, i/60, i%60)
		e.Status = statuses[i%len(statuses)]
		if err := d.InsertLog(e); err != nil {
			t.Fatalf("InsertLog #%d: %v", i, err)
		}
	}
}

// TestQueryLogsPagination 分页返回不重叠、顺序稳定、总量正确
func TestQueryLogsPagination(t *testing.T) {
	d := newTestDB(t)
	seedLogs(t, d, "2026-09-04", 25)

	page1, err := d.QueryLogs("2026-09-04", "2026-09-04", "", 10, 0)
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	page2, err := d.QueryLogs("2026-09-04", "2026-09-04", "", 10, 10)
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	page3, err := d.QueryLogs("2026-09-04", "2026-09-04", "", 10, 20)
	if err != nil {
		t.Fatalf("page3: %v", err)
	}

	if len(page1) != 10 || len(page2) != 10 || len(page3) != 5 {
		t.Fatalf("每页条数不对：%d/%d/%d，want 10/10/5", len(page1), len(page2), len(page3))
	}

	// 三页合起来应覆盖全部 25 条且无重复
	seen := map[string]bool{}
	for _, p := range [][]*proxy.LogEntry{page1, page2, page3} {
		for _, l := range p {
			if seen[l.ID] {
				t.Errorf("分页出现重复 id %s", l.ID)
			}
			seen[l.ID] = true
		}
	}
	if len(seen) != 25 {
		t.Errorf("三页共 %d 条唯一日志，want 25", len(seen))
	}

	// 倒序：第一页首条的 date_time 应大于第二页首条
	if page1[0].DateTime <= page2[0].DateTime {
		t.Errorf("分页未保持倒序：page1[0]=%s page2[0]=%s", page1[0].DateTime, page2[0].DateTime)
	}
}

// TestQueryLogsStatusFilterPushedDown 状态筛选下推到 SQL，分页与筛选可叠加
func TestQueryLogsStatusFilterPushedDown(t *testing.T) {
	d := newTestDB(t)
	// 20 条：success / error 交替，各 10 条
	seedLogs(t, d, "2026-09-04", 20, "success", "error")

	all, _ := d.QueryLogs("2026-09-04", "2026-09-04", "", 0, 0)
	if len(all) != 20 {
		t.Fatalf("不筛选应返回 20 条，实际 %d", len(all))
	}

	succ, err := d.QueryLogs("2026-09-04", "2026-09-04", "success", 0, 0)
	if err != nil {
		t.Fatalf("筛选查询: %v", err)
	}
	if len(succ) != 10 {
		t.Fatalf("success 应 10 条，实际 %d", len(succ))
	}
	for _, l := range succ {
		if l.Status != "success" {
			t.Errorf("筛选结果混入 status=%s", l.Status)
		}
	}

	// 筛选 + 分页叠加
	p1, _ := d.QueryLogs("2026-09-04", "2026-09-04", "success", 4, 0)
	p2, _ := d.QueryLogs("2026-09-04", "2026-09-04", "success", 4, 4)
	p3, _ := d.QueryLogs("2026-09-04", "2026-09-04", "success", 4, 8)
	if len(p1) != 4 || len(p2) != 4 || len(p3) != 2 {
		t.Errorf("筛选分页条数：%d/%d/%d，want 4/4/2", len(p1), len(p2), len(p3))
	}
	for _, l := range append(append(p1, p2...), p3...) {
		if l.Status != "success" {
			t.Errorf("筛选分页混入 status=%s", l.Status)
		}
	}
}

// TestQueryLogsOffsetWithoutLimit limit<=0 且 offset>0 时 SQL 仍合法
// （SQLite 的 OFFSET 必须跟在 LIMIT 之后，需要 LIMIT -1 占位）
func TestQueryLogsOffsetWithoutLimit(t *testing.T) {
	d := newTestDB(t)
	seedLogs(t, d, "2026-09-04", 10)

	got, err := d.QueryLogs("2026-09-04", "2026-09-04", "", 0, 4)
	if err != nil {
		t.Fatalf("limit=0 offset=4 报错（LIMIT -1 占位缺失？）: %v", err)
	}
	if len(got) != 6 {
		t.Errorf("跳过 4 条后应剩 6 条，实际 %d", len(got))
	}
}

// TestQueryLogsOffsetBeyondTotal offset 超过总数时返回空而非报错
func TestQueryLogsOffsetBeyondTotal(t *testing.T) {
	d := newTestDB(t)
	seedLogs(t, d, "2026-09-04", 5)

	got, err := d.QueryLogs("2026-09-04", "2026-09-04", "", 10, 100)
	if err != nil {
		t.Fatalf("越界 offset 报错: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("越界 offset 应返回空，实际 %d 条", len(got))
	}
}

// TestQueryLogsRespectsDateRange 分页不会漏掉/串入范围外的日志
func TestQueryLogsRespectsDateRange(t *testing.T) {
	d := newTestDB(t)
	seedLogs(t, d, "2026-09-03", 5)
	seedLogs(t, d, "2026-09-04", 5)

	only4, _ := d.QueryLogs("2026-09-04", "2026-09-04", "", 0, 0)
	if len(only4) != 5 {
		t.Errorf("单日应 5 条，实际 %d", len(only4))
	}
	for _, l := range only4 {
		if l.Date != "2026-09-04" {
			t.Errorf("串入了 %s 的日志", l.Date)
		}
	}

	both, _ := d.QueryLogs("2026-09-03", "2026-09-04", "", 0, 0)
	if len(both) != 10 {
		t.Errorf("两日应 10 条，实际 %d", len(both))
	}
}
