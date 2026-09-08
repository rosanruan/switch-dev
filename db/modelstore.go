package db

import "time"

// ModelMeta 模型元数据（用于 upsert）
type ModelMeta struct {
	Label    string
	Context  int
	Output   int
	Stream   bool
	Vision   bool
	ToolCall bool
	Free     bool
}

// BenchResult 测评结果（写入 models 表的 bench_* 字段）
type BenchResult struct {
	TPS      float64
	Duration int64
	Error    string
	Success  bool
}

// ModelEntry 批量同步模型时的条目
type ModelEntry struct {
	UpstreamName string
	ModelID      string
	Label        string
	Context      int
	Output       int
	Stream       bool
	Vision       bool
	ToolCall     bool
	Free         bool
}

// UpsertModel 按 (upstream_id, model_id) 去重插入/更新，返回 model id。
// meta 中非零值才覆盖已有记录。
func (d *DB) UpsertModel(upstreamID int64, modelID, label string, meta ModelMeta) (int64, error) {
	if upstreamID == 0 {
		return 0, nil
	}

	// 先尝试 INSERT OR IGNORE
	res, err := d.conn.Exec(
		`INSERT OR IGNORE INTO models (upstream_id, model_id, label, context, output, stream, vision, tool_call, free)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		upstreamID, modelID, label,
		meta.Context, meta.Output,
		boolToInt(meta.Stream), boolToInt(meta.Vision), boolToInt(meta.ToolCall), boolToInt(meta.Free),
	)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	if id > 0 {
		return id, nil // 新插入
	}
	// 已存在：查 id，并用非零值更新
	var existingID int64
	err = d.conn.QueryRow(
		`SELECT id FROM models WHERE upstream_id = ? AND model_id = ?`,
		upstreamID, modelID,
	).Scan(&existingID)
	if err != nil {
		return 0, err
	}
	// 更新 label 和元数据（COALESCE 逻辑：只在新值非零时覆盖）
	_, err = d.conn.Exec(
		`UPDATE models SET
			label    = CASE WHEN ? != '' THEN ? ELSE label END,
			context  = CASE WHEN ? > 0 THEN ? ELSE context END,
			output   = CASE WHEN ? > 0 THEN ? ELSE output END,
			stream   = CASE WHEN ? = 1 THEN 1 ELSE stream END,
			vision   = CASE WHEN ? = 1 THEN 1 ELSE vision END,
			tool_call= CASE WHEN ? = 1 THEN 1 ELSE tool_call END,
			free     = CASE WHEN ? = 1 THEN 1 ELSE free END
		 WHERE id = ?`,
		label, label,
		meta.Context, meta.Context,
		meta.Output, meta.Output,
		boolToInt(meta.Stream), boolToInt(meta.Vision), boolToInt(meta.ToolCall), boolToInt(meta.Free),
		existingID,
	)
	return existingID, err
}

// UpsertModelByUpstreamName 按 upstream name + model_id upsert（便捷方法）
func (d *DB) UpsertModelByUpstreamName(upstreamName, modelID, label string, meta ModelMeta) (int64, error) {
	upID := d.QueryUpstreamID(upstreamName)
	if upID == 0 {
		// upstream 不存在，先创建（默认 builtin 类型）
		var err error
		upID, err = d.UpsertUpstream(upstreamName, "builtin", upstreamName, "")
		if err != nil {
			return 0, err
		}
	}
	return d.UpsertModel(upID, modelID, label, meta)
}

// BenchRecord 持久化的测评结果（models 表中的 bench_* 列）
type BenchRecord struct {
	Upstream string
	Model    string
	TPS      float64
	Duration int64  // 毫秒
	BenchAt  string // 测评时间文本
	Error    string
	Success  bool
}

// QueryBenchResults 返回所有测评过的模型结果（bench_at 非空），按时间倒序。
func (d *DB) QueryBenchResults() ([]BenchRecord, error) {
	rows, err := d.conn.Query(`
		SELECT u.name, m.model_id, m.bench_tps, m.bench_duration, m.bench_at, m.bench_error, m.verified
		FROM models m
		JOIN upstreams u ON m.upstream_id = u.id
		WHERE m.bench_at != ''
		ORDER BY m.bench_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BenchRecord
	for rows.Next() {
		var r BenchRecord
		var verified int
		if err := rows.Scan(&r.Upstream, &r.Model, &r.TPS, &r.Duration, &r.BenchAt, &r.Error, &verified); err != nil {
			continue
		}
		// 有 TPS 且无错误记为成功（verified 列可能被其他路径置位，不作为唯一依据）
		r.Success = r.TPS > 0 && r.Error == ""
		out = append(out, r)
	}
	return out, nil
}

// UpsertModelBench 更新模型测评结果
func (d *DB) UpsertModelBench(upstreamName, modelID string, bench BenchResult) error {
	upID := d.QueryUpstreamID(upstreamName)
	if upID == 0 {
		var err error
		upID, err = d.UpsertUpstream(upstreamName, "builtin", upstreamName, "")
		if err != nil {
			return err
		}
	}
	// 确保 model 行存在
	modelDBID, err := d.UpsertModel(upID, modelID, modelID, ModelMeta{})
	if err != nil {
		return err
	}

	verified := 0
	healthy := 0
	errMsg := bench.Error
	if bench.Success {
		verified = 1
		healthy = 1
		errMsg = ""
	}

	_, err = d.conn.Exec(
		`UPDATE models SET
			verified = ?, healthy = ?,
			bench_tps = ?, bench_duration = ?,
			bench_at = ?, bench_error = ?
		 WHERE id = ?`,
		verified, healthy,
		bench.TPS, bench.Duration,
		time.Now().Format("2006-01-02 15:04:05"),
		errMsg,
		modelDBID,
	)
	return err
}

// UpsertModelVerified 标记模型为已验证
func (d *DB) UpsertModelVerified(upstreamName, modelID, label string) error {
	upID := d.QueryUpstreamID(upstreamName)
	if upID == 0 {
		var err error
		upID, err = d.UpsertUpstream(upstreamName, "provider", upstreamName, upstreamName)
		if err != nil {
			return err
		}
	}
	id, err := d.UpsertModel(upID, modelID, label, ModelMeta{})
	if err != nil {
		return err
	}
	_, err = d.conn.Exec(`UPDATE models SET verified = 1 WHERE id = ?`, id)
	return err
}

// BatchUpsertModels 批量同步模型元数据
func (d *DB) BatchUpsertModels(entries []ModelEntry) error {
	tx, err := d.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(
		`INSERT OR IGNORE INTO models (upstream_id, model_id, label, context, output, stream, vision, tool_call, free)
		 SELECT id, ?, ?, ?, ?, ?, ?, ?, ? FROM upstreams WHERE name = ?`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, e := range entries {
		_, err := stmt.Exec(
			e.ModelID, e.Label,
			e.Context, e.Output,
			boolToInt(e.Stream), boolToInt(e.Vision), boolToInt(e.ToolCall), boolToInt(e.Free),
			e.UpstreamName,
		)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// QueryModelID 按 upstream_id + model_id 查询 model 主键
func (d *DB) QueryModelID(upstreamID int64, modelID string) int64 {
	if upstreamID == 0 || modelID == "" {
		return 0
	}
	var id int64
	err := d.conn.QueryRow(
		`SELECT id FROM models WHERE upstream_id = ? AND model_id = ?`,
		upstreamID, modelID,
	).Scan(&id)
	if err != nil {
		return 0
	}
	return id
}

// ====== 模型目录（内置上游的实时模型清单持久化） ======

// CatalogEntry 目录条目。model_id 存代理内部 id（含 wb/ 前缀），
// wire_name 存发往上游 base_url 的真实 model 名（DevEco 两者不同）。
type CatalogEntry struct {
	UpstreamName string
	ModelID      string
	WireName     string
	Label        string
	Context      int
	Output       int
	Stream       bool
	Vision       bool
	ToolCall     bool
	Reasoning    bool
	Free         bool
}

// SyncUpstreamCatalog 用一次拉取结果全量替换某上游的目录（单事务）。
// 与 UpsertModel 不同，这里的元数据是**全量覆盖**：实时结果把 context 改小、
// 把能力位关掉都必须能生效。
//
// 本次结果中缺席的模型只置 available=0，绝不 DELETE —— models 行被
// logs.model_id / logs.used_model_id 外键引用，删了会让历史日志和用量统计断链。
func (d *DB) SyncUpstreamCatalog(upstreamName string, entries []CatalogEntry) error {
	if upstreamName == "" {
		return nil
	}
	upID := d.QueryUpstreamID(upstreamName)
	if upID == 0 {
		var err error
		upID, err = d.UpsertUpstream(upstreamName, "builtin", upstreamName, "")
		if err != nil {
			return err
		}
	}

	now := time.Now().Format("2006-01-02 15:04:05")

	tx, err := d.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// 先把本上游的目录行全部标记为不可用，命中的再逐条置回 —— 省掉「算差集」的往返。
	// 限定 catalog_source='live'：日志写入路径（upsertModelInTx）也会建 models 行，
	// 那些行不属于目录，不该被这里翻动。
	if _, err := tx.Exec(
		`UPDATE models SET available = 0 WHERE upstream_id = ? AND catalog_source = 'live'`, upID,
	); err != nil {
		return err
	}

	ins, err := tx.Prepare(
		`INSERT INTO models
		    (upstream_id, model_id, label, context, output, stream, vision, tool_call, free,
		     reasoning, wire_name, available, catalog_source, synced_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, 'live', ?)
		 ON CONFLICT(upstream_id, model_id) DO UPDATE SET
		    label = excluded.label,
		    context = excluded.context,
		    output = excluded.output,
		    stream = excluded.stream,
		    vision = excluded.vision,
		    tool_call = excluded.tool_call,
		    free = excluded.free,
		    reasoning = excluded.reasoning,
		    wire_name = excluded.wire_name,
		    available = 1,
		    catalog_source = 'live',
		    synced_at = excluded.synced_at`)
	if err != nil {
		return err
	}
	defer ins.Close()

	for _, e := range entries {
		// 与 upsertModelInTx 对齐：空 id 和 auto 虚拟模型不入库
		if e.ModelID == "" || e.ModelID == "auto" {
			continue
		}
		label := e.Label
		if label == "" {
			label = e.ModelID
		}
		wire := e.WireName
		if wire == "" {
			wire = e.ModelID
		}
		if _, err := ins.Exec(
			upID, e.ModelID, label, e.Context, e.Output,
			boolToInt(e.Stream), boolToInt(e.Vision), boolToInt(e.ToolCall), boolToInt(e.Free),
			boolToInt(e.Reasoning), wire, now,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListCatalog 回读全部可用目录条目（仅内置上游，供应商模型由 providerapi 自己管）
func (d *DB) ListCatalog() ([]CatalogEntry, error) {
	return d.queryCatalog("")
}

// ListCatalogByUpstream 回读某上游的可用目录条目
func (d *DB) ListCatalogByUpstream(upstreamName string) ([]CatalogEntry, error) {
	return d.queryCatalog(upstreamName)
}

func (d *DB) queryCatalog(upstreamName string) ([]CatalogEntry, error) {
	q := `SELECT u.name, m.model_id, m.wire_name, m.label,
	             m.context, m.output, m.stream, m.vision, m.tool_call, m.reasoning, m.free
	      FROM models m
	      JOIN upstreams u ON m.upstream_id = u.id
	      WHERE m.available = 1 AND m.catalog_source = 'live' AND m.model_id != ''`
	args := []interface{}{}
	if upstreamName != "" {
		q += ` AND u.name = ?`
		args = append(args, upstreamName)
	}
	q += ` ORDER BY u.name, m.model_id`

	rows, err := d.conn.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []CatalogEntry
	for rows.Next() {
		var e CatalogEntry
		var stream, vision, toolCall, reasoning, free int
		if err := rows.Scan(&e.UpstreamName, &e.ModelID, &e.WireName, &e.Label,
			&e.Context, &e.Output, &stream, &vision, &toolCall, &reasoning, &free); err != nil {
			continue
		}
		e.Stream = stream == 1
		e.Vision = vision == 1
		e.ToolCall = toolCall == 1
		e.Reasoning = reasoning == 1
		e.Free = free == 1
		out = append(out, e)
	}
	return out, rows.Err()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
