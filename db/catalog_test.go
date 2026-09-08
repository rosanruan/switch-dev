package db

import (
	"testing"
)

func catalogEntry(upstreamName, modelID, wire, label string) CatalogEntry {
	return CatalogEntry{
		UpstreamName: upstreamName, ModelID: modelID, WireName: wire, Label: label,
		Context: 1000, Output: 500, Stream: true, ToolCall: true,
	}
}

// TestSyncUpstreamCatalogRoundTrip 落库 -> 回读往返
func TestSyncUpstreamCatalogRoundTrip(t *testing.T) {
	d := newTestDB(t)

	entries := []CatalogEntry{
		catalogEntry("deveco", "glm-5.1", "GLM-5.1", "GLM-5.1 (DevEco)"),
		catalogEntry("deveco", "GLM-6.0", "GLM-6.0", "GLM-6.0"),
	}
	if err := d.SyncUpstreamCatalog("deveco", entries); err != nil {
		t.Fatalf("SyncUpstreamCatalog: %v", err)
	}

	got, err := d.ListCatalogByUpstream("deveco")
	if err != nil {
		t.Fatalf("ListCatalogByUpstream: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("回读 %d 条，want 2：%+v", len(got), got)
	}
	byID := map[string]CatalogEntry{}
	for _, e := range got {
		byID[e.ModelID] = e
	}
	if e := byID["glm-5.1"]; e.WireName != "GLM-5.1" || e.Context != 1000 || !e.Stream || !e.ToolCall {
		t.Errorf("glm-5.1 回读不符：%+v", e)
	}
	if _, ok := byID["GLM-6.0"]; !ok {
		t.Error("live-only 模型 GLM-6.0 未回读到")
	}
}

// TestSyncUpstreamCatalogOverwritesDownward 实时结果必须能把元数据改小、把能力位关掉
// （旧的 UpsertModel 只能升不能降，所以目录同步不能复用它）
func TestSyncUpstreamCatalogOverwritesDownward(t *testing.T) {
	d := newTestDB(t)

	big := CatalogEntry{
		UpstreamName: "opencode", ModelID: "m-free", Label: "M",
		Context: 200000, Output: 64000, Stream: true, Vision: true, ToolCall: true, Reasoning: true, Free: true,
	}
	if err := d.SyncUpstreamCatalog("opencode", []CatalogEntry{big}); err != nil {
		t.Fatalf("首轮同步: %v", err)
	}

	small := CatalogEntry{
		UpstreamName: "opencode", ModelID: "m-free", Label: "M",
		Context: 8000, Output: 2000, Stream: true,
	}
	if err := d.SyncUpstreamCatalog("opencode", []CatalogEntry{small}); err != nil {
		t.Fatalf("二轮同步: %v", err)
	}

	got, _ := d.ListCatalogByUpstream("opencode")
	if len(got) != 1 {
		t.Fatalf("回读 %d 条，want 1", len(got))
	}
	e := got[0]
	if e.Context != 8000 || e.Output != 2000 {
		t.Errorf("元数据未下调：context=%d output=%d，want 8000/2000", e.Context, e.Output)
	}
	if e.Vision || e.ToolCall || e.Reasoning || e.Free {
		t.Errorf("能力位未关闭：%+v", e)
	}
}

// TestSyncUpstreamCatalogSoftDeletesMissing 上游下线的模型只置 available=0，不删行
func TestSyncUpstreamCatalogSoftDeletesMissing(t *testing.T) {
	d := newTestDB(t)

	if err := d.SyncUpstreamCatalog("joycode", []CatalogEntry{
		catalogEntry("joycode", "model-a", "model-a", "A"),
		catalogEntry("joycode", "model-b", "model-b", "B"),
	}); err != nil {
		t.Fatalf("首轮同步: %v", err)
	}

	// 第二轮 model-b 消失
	if err := d.SyncUpstreamCatalog("joycode", []CatalogEntry{
		catalogEntry("joycode", "model-a", "model-a", "A"),
	}); err != nil {
		t.Fatalf("二轮同步: %v", err)
	}

	got, _ := d.ListCatalogByUpstream("joycode")
	if len(got) != 1 || got[0].ModelID != "model-a" {
		t.Errorf("回读应只剩 model-a，实际 %+v", got)
	}

	// 行必须还在（被 logs 外键引用，删了会断链）
	var cnt, available int
	if err := d.conn.QueryRow(
		`SELECT COUNT(*), COALESCE(MAX(available), -1) FROM models m
		 JOIN upstreams u ON m.upstream_id = u.id
		 WHERE u.name = 'joycode' AND m.model_id = 'model-b'`,
	).Scan(&cnt, &available); err != nil {
		t.Fatalf("查 model-b: %v", err)
	}
	if cnt != 1 {
		t.Fatalf("model-b 行被删除了（cnt=%d），外键会断链", cnt)
	}
	if available != 0 {
		t.Errorf("model-b available = %d, want 0", available)
	}

	// 再次出现应恢复可用
	if err := d.SyncUpstreamCatalog("joycode", []CatalogEntry{
		catalogEntry("joycode", "model-a", "model-a", "A"),
		catalogEntry("joycode", "model-b", "model-b", "B"),
	}); err != nil {
		t.Fatalf("三轮同步: %v", err)
	}
	got, _ = d.ListCatalogByUpstream("joycode")
	if len(got) != 2 {
		t.Errorf("model-b 重新上线后应可回读，实际 %+v", got)
	}
}

// TestSoftDeletedModelStillJoinsInLogs 软下线的模型行仍能被日志 JOIN 查出模型名
// —— 这是「不能 DELETE」的根本原因。
func TestSoftDeletedModelStillJoinsInLogs(t *testing.T) {
	d := newTestDB(t)

	if err := d.SyncUpstreamCatalog("joycode", []CatalogEntry{
		catalogEntry("joycode", "doomed-model", "doomed-model", "Doomed"),
	}); err != nil {
		t.Fatalf("同步: %v", err)
	}

	entry := successEntry("doomed-model", "doomed-model", "joycode", 10, 20, 100)
	if err := d.InsertLog(entry); err != nil {
		t.Fatalf("InsertLog: %v", err)
	}

	// 上游下线该模型
	if err := d.SyncUpstreamCatalog("joycode", []CatalogEntry{}); err != nil {
		t.Fatalf("下线同步: %v", err)
	}

	var name string
	if err := d.conn.QueryRow(
		`SELECT m.model_id FROM logs l JOIN models m ON l.model_id = m.id
		 ORDER BY l.id DESC LIMIT 1`,
	).Scan(&name); err != nil {
		t.Fatalf("软下线后日志 JOIN 失败（说明行被删了）: %v", err)
	}
	if name != "doomed-model" {
		t.Errorf("日志 JOIN 出的模型名 = %q, want doomed-model", name)
	}
}

// TestSyncUpstreamCatalogSkipsAutoAndEmpty auto 虚拟模型和空 id 不入库
func TestSyncUpstreamCatalogSkipsAutoAndEmpty(t *testing.T) {
	d := newTestDB(t)

	if err := d.SyncUpstreamCatalog("deveco", []CatalogEntry{
		catalogEntry("deveco", "auto", "auto", "Auto"),
		catalogEntry("deveco", "", "", ""),
		catalogEntry("deveco", "real-model", "real-model", "Real"),
	}); err != nil {
		t.Fatalf("SyncUpstreamCatalog: %v", err)
	}

	got, _ := d.ListCatalogByUpstream("deveco")
	if len(got) != 1 || got[0].ModelID != "real-model" {
		t.Errorf("auto/空 id 应被跳过，实际回读 %+v", got)
	}
}

// TestSyncUpstreamCatalogLeavesLogCreatedRowsAlone 日志写入路径建的 models 行
// （catalog_source=” ）不属于目录，同步时不该被翻动。
func TestSyncUpstreamCatalogLeavesLogCreatedRowsAlone(t *testing.T) {
	d := newTestDB(t)

	// 日志写入会 upsert 一个 models 行
	if err := d.InsertLog(successEntry("log-only-model", "log-only-model", "joycode", 1, 2, 3)); err != nil {
		t.Fatalf("InsertLog: %v", err)
	}
	// 目录同步一个完全不同的模型
	if err := d.SyncUpstreamCatalog("joycode", []CatalogEntry{
		catalogEntry("joycode", "catalog-model", "catalog-model", "C"),
	}); err != nil {
		t.Fatalf("SyncUpstreamCatalog: %v", err)
	}

	var available int
	if err := d.conn.QueryRow(
		`SELECT m.available FROM models m JOIN upstreams u ON m.upstream_id = u.id
		 WHERE u.name = 'joycode' AND m.model_id = 'log-only-model'`,
	).Scan(&available); err != nil {
		t.Fatalf("查 log-only-model: %v", err)
	}
	if available != 1 {
		t.Errorf("日志建的行被目录同步置为不可用（available=%d）", available)
	}

	// 但它不该出现在目录回读里（catalog_source != 'live'）
	got, _ := d.ListCatalogByUpstream("joycode")
	for _, e := range got {
		if e.ModelID == "log-only-model" {
			t.Error("日志建的行不该出现在目录回读中")
		}
	}
}

// TestAddColumnsIdempotent 重复 migrate（升级路径会反复调用）必须安全
func TestAddColumnsIdempotent(t *testing.T) {
	d := newTestDB(t)

	for i := 0; i < 3; i++ {
		if err := d.migrate(); err != nil {
			t.Fatalf("第 %d 次 migrate 失败: %v", i+1, err)
		}
	}

	// 新列都在
	rows, err := d.conn.Query(`SELECT name FROM pragma_table_info('models')`)
	if err != nil {
		t.Fatalf("PRAGMA: %v", err)
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var n string
		rows.Scan(&n)
		cols[n] = true
	}
	for _, want := range []string{"reasoning", "wire_name", "available", "catalog_source", "synced_at"} {
		if !cols[want] {
			t.Errorf("列 %s 缺失", want)
		}
	}
}

// TestAddColumnsOnLegacyTable 模拟老库（没有新列）走加列路径
func TestAddColumnsOnLegacyTable(t *testing.T) {
	d := newTestDB(t)

	if _, err := d.conn.Exec(`DROP TABLE models`); err != nil {
		t.Fatalf("DROP: %v", err)
	}
	// 重建成加列之前的老结构
	if _, err := d.conn.Exec(`CREATE TABLE models (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		upstream_id INTEGER NOT NULL,
		model_id TEXT NOT NULL DEFAULT '',
		label TEXT NOT NULL DEFAULT '',
		context INTEGER NOT NULL DEFAULT 0,
		output INTEGER NOT NULL DEFAULT 0,
		stream INTEGER NOT NULL DEFAULT 0,
		vision INTEGER NOT NULL DEFAULT 0,
		tool_call INTEGER NOT NULL DEFAULT 0,
		free INTEGER NOT NULL DEFAULT 0,
		verified INTEGER NOT NULL DEFAULT 0,
		healthy INTEGER NOT NULL DEFAULT 0,
		bench_tps REAL NOT NULL DEFAULT 0,
		bench_duration INTEGER NOT NULL DEFAULT 0,
		bench_at TEXT NOT NULL DEFAULT '',
		bench_error TEXT NOT NULL DEFAULT '',
		UNIQUE(upstream_id, model_id)
	)`); err != nil {
		t.Fatalf("重建老表: %v", err)
	}
	// 老库里已有一行数据
	if _, err := d.conn.Exec(
		`INSERT INTO models (upstream_id, model_id, label) VALUES (1, 'legacy', 'Legacy')`,
	); err != nil {
		t.Fatalf("插入老数据: %v", err)
	}

	if err := d.migrate(); err != nil {
		t.Fatalf("老库 migrate 失败: %v", err)
	}

	// 老行的新列应取默认值（available 默认 1）
	var available int
	var wire string
	if err := d.conn.QueryRow(
		`SELECT available, wire_name FROM models WHERE model_id = 'legacy'`,
	).Scan(&available, &wire); err != nil {
		t.Fatalf("查老行: %v", err)
	}
	if available != 1 || wire != "" {
		t.Errorf("老行新列默认值不符：available=%d wire=%q", available, wire)
	}

	// 加列后目录同步可用
	if err := d.SyncUpstreamCatalog("joycode", []CatalogEntry{
		catalogEntry("joycode", "post-migrate", "post-migrate", "P"),
	}); err != nil {
		t.Fatalf("加列后同步失败: %v", err)
	}
}
