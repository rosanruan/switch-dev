package db

import (
	"database/sql"
	"fmt"

	"switchdev/proxy"
)

// InsertLog 将一条日志写入数据库（含关联表 upsert）。
// 在事务中执行，保证关联表一致性。失败时返回错误，调用方可忽略（异步写入）。
func (d *DB) InsertLog(entry *proxy.LogEntry) error {
	if entry == nil {
		return nil
	}

	tx, err := d.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// 1. source
	sourceName := entry.Source
	if sourceName == "" {
		sourceName = "未知"
	}
	var sourceID int64
	err = tx.QueryRow(
		`INSERT INTO sources (name, user_agent) VALUES (?, ?)
		 ON CONFLICT(name, user_agent) DO UPDATE SET name=excluded.name
		 RETURNING id`,
		sourceName, entry.UserAgent,
	).Scan(&sourceID)
	if err != nil {
		return fmt.Errorf("upsert source: %w", err)
	}

	// 2. upstream（INSERT OR IGNORE：不覆盖已有的 type/label，免费供应商由 UpsertUpstream 写入正确名称）
	upstreamName := entry.Upstream
	if upstreamName == "" {
		upstreamName = "unknown"
	}
	var upstreamID int64
	err = tx.QueryRow(
		`INSERT INTO upstreams (name, type, label) VALUES (?, 'builtin', ?)
		 ON CONFLICT(name) DO NOTHING
		 RETURNING id`,
		upstreamName, upstreamLabel(upstreamName),
	).Scan(&upstreamID)
	if err != nil {
		// 已存在：查 id
		err = tx.QueryRow(`SELECT id FROM upstreams WHERE name = ?`, upstreamName).Scan(&upstreamID)
		if err != nil {
			return fmt.Errorf("upsert upstream: %w", err)
		}
	}

	// 3. model（请求的模型）
	modelID := d.upsertModelInTx(tx, upstreamID, entry.Model)

	// 4. used_model（实际用到的模型）
	usedModelName := entry.UsedModel
	usedModelID := d.upsertModelInTx(tx, upstreamID, usedModelName)

	// 5. 加密 body
	encReqBody := d.crypto.EncryptField(entry.RequestBody)
	encRespBody := d.crypto.EncryptField(entry.ResponseBody)

	// 6. INSERT log
	dateTime := entry.DateTime
	if dateTime == "" {
		dateTime = entry.Timestamp
	}
	_, err = tx.Exec(
		`INSERT INTO logs (
			date_time, date, model_id, used_model_id, source_id, upstream_id,
			status, code, duration, error_msg, method, path, stream,
			request_body, response_body,
			input_tokens, output_tokens, cache_hit_tokens,
			cost, cost_text, first_byte_ms
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		dateTime, entry.Date,
		modelID, usedModelID, sourceID, upstreamID,
		entry.Status, entry.Code, entry.Duration, entry.ErrorMsg,
		entry.Method, entry.Path, boolToInt(entry.Stream),
		encReqBody, encRespBody,
		entry.InputTokens, entry.OutputTokens, entry.CacheHitTokens,
		entry.Cost, entry.CostText, entry.FirstByteMs,
	)
	if err != nil {
		return fmt.Errorf("insert log: %w", err)
	}

	return tx.Commit()
}

// upsertModelInTx 在事务中 upsert model 并返回 id
func (d *DB) upsertModelInTx(tx *sql.Tx, upstreamID int64, modelIDStr string) int64 {
	if upstreamID == 0 || modelIDStr == "" || modelIDStr == "auto" {
		return 0
	}
	// INSERT … ON CONFLICT DO NOTHING + RETURNING：新插入返回 id，冲突返回空行
	// （不能用 INSERT OR IGNORE + LastInsertId：modernc 在冲突时 LastInsertId 会泄漏
	// 同事务中前一条 INSERT 的 rowid，导致 FK 外键约束失败）
	var id int64
	err := tx.QueryRow(
		`INSERT INTO models (upstream_id, model_id, label) VALUES (?, ?, ?)
		 ON CONFLICT(upstream_id, model_id) DO NOTHING
		 RETURNING id`,
		upstreamID, modelIDStr, modelIDStr,
	).Scan(&id)
	if err == nil {
		return id
	}
	// 冲突：查已有 id
	var existingID int64
	tx.QueryRow(
		`SELECT id FROM models WHERE upstream_id = ? AND model_id = ?`,
		upstreamID, modelIDStr,
	).Scan(&existingID)
	return existingID
}

// LogRow 查询结果行（JOIN 后的扁平结构）
type LogRow struct {
	ID             int64
	DateTime       string
	Date           string
	ModelName      string
	UsedModelName  string
	SourceName     string
	UpstreamName   string
	UpstreamLabel  string
	Status         string
	Code           int
	Duration       int64
	ErrorMsg       string
	Method         string
	Path           string
	Stream         bool
	RequestBody    string
	ResponseBody   string
	InputTokens    int
	OutputTokens   int
	CacheHitTokens int
	Cost           float64
	CostText       string
	FirstByteMs    int64
}

// QueryLogs 按日期范围查询日志，JOIN 关联表还原名称。
// status 非空时只返回该状态（分页时筛选必须下推到 SQL，否则页内再过滤会与
// LogCountsByRange 的计数对不上）；limit<=0 表示不限制；offset>0 时跳过前 N 条。
func (d *DB) QueryLogs(startDate, endDate, status string, limit, offset int) ([]*proxy.LogEntry, error) {
	q := `
		SELECT
			l.id, l.date_time, l.date,
			COALESCE(m1.model_id, '') AS model_name,
			COALESCE(m2.model_id, '') AS used_model_name,
			COALESCE(s.name, '') AS source_name,
			COALESCE(u.name, '') AS upstream_name,
			COALESCE(u.label, '') AS upstream_label,
			l.status, l.code, l.duration, l.error_msg,
			l.method, l.path, l.stream,
			l.request_body, l.response_body,
			l.input_tokens, l.output_tokens, l.cache_hit_tokens,
			l.cost, l.cost_text, l.first_byte_ms
		FROM logs l
		LEFT JOIN sources s    ON l.source_id = s.id
		LEFT JOIN upstreams u  ON l.upstream_id = u.id
		LEFT JOIN models m1    ON l.model_id = m1.id
		LEFT JOIN models m2    ON l.used_model_id = m2.id
		WHERE l.date BETWEEN ? AND ?
	`
	args := []any{startDate, endDate}
	if status != "" {
		q += " AND l.status = ?"
		args = append(args, status)
	}
	q += " ORDER BY l.date_time DESC, l.id DESC"
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	} else if offset > 0 {
		// SQLite 的 OFFSET 必须跟在 LIMIT 后面，无上限时用 -1 占位
		q += " LIMIT -1"
	}
	if offset > 0 {
		q += " OFFSET ?"
		args = append(args, offset)
	}

	rows, err := d.conn.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []*proxy.LogEntry
	for rows.Next() {
		var r LogRow
		var streamInt int
		err := rows.Scan(
			&r.ID, &r.DateTime, &r.Date,
			&r.ModelName, &r.UsedModelName, &r.SourceName,
			&r.UpstreamName, &r.UpstreamLabel,
			&r.Status, &r.Code, &r.Duration, &r.ErrorMsg,
			&r.Method, &r.Path, &streamInt,
			&r.RequestBody, &r.ResponseBody,
			&r.InputTokens, &r.OutputTokens, &r.CacheHitTokens,
			&r.Cost, &r.CostText, &r.FirstByteMs,
		)
		if err != nil {
			continue
		}
		r.Stream = streamInt == 1

		// Upstream：优先用 label（供应商显示名），为空时回退到 name（内置上游的英文名）
		upstreamDisplay := r.UpstreamLabel
		if upstreamDisplay == "" {
			upstreamDisplay = r.UpstreamName
		}

		entry := &proxy.LogEntry{
			ID:             fmt.Sprintf("%d", r.ID),
			Timestamp:      r.DateTime[11:],
			DateTime:       r.DateTime,
			Date:           r.Date,
			Model:          r.ModelName,
			UsedModel:      r.UsedModelName,
			Source:         r.SourceName,
			Upstream:       upstreamDisplay,
			Status:         r.Status,
			Code:           r.Code,
			Duration:       r.Duration,
			ErrorMsg:       r.ErrorMsg,
			Method:         r.Method,
			Path:           r.Path,
			Stream:         r.Stream,
			RequestBody:    d.crypto.DecryptField(r.RequestBody),
			ResponseBody:   d.crypto.DecryptField(r.ResponseBody),
			InputTokens:    r.InputTokens,
			OutputTokens:   r.OutputTokens,
			CacheHitTokens: r.CacheHitTokens,
			Cost:           r.Cost,
			CostText:       r.CostText,
			FirstByteMs:    r.FirstByteMs,
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// CountLogs 返回日志总数
func (d *DB) CountLogs() (int64, error) {
	var count int64
	err := d.conn.QueryRow(`SELECT COUNT(*) FROM logs`).Scan(&count)
	return count, err
}

// LogCountsByRange 按日期范围统计各状态日志数（重启后仍准确，不依赖内存计数器）
func (d *DB) LogCountsByRange(startDate, endDate string) (map[string]int64, error) {
	rows, err := d.conn.Query(
		`SELECT status, COUNT(*) FROM logs WHERE date BETWEEN ? AND ? GROUP BY status`,
		startDate, endDate,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := map[string]int64{}
	for rows.Next() {
		var status string
		var n int64
		if err := rows.Scan(&status, &n); err != nil {
			continue
		}
		counts[status] = n
	}
	return counts, nil
}

// ClearLogs 清空所有日志（不删关联表，保留 source/upstream/model 元数据）
func (d *DB) ClearLogs() error {
	_, err := d.conn.Exec(`DELETE FROM logs`)
	return err
}

// QueryLogDates 返回所有有日志的日期（降序）
func (d *DB) QueryLogDates() []string {
	rows, err := d.conn.Query(`SELECT DISTINCT date FROM logs ORDER BY date DESC`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var dates []string
	for rows.Next() {
		var date string
		if err := rows.Scan(&date); err != nil {
			continue
		}
		dates = append(dates, date)
	}
	return dates
}

// upstreamLabel 返回上游的显示名（内置上游直接用英文 name；免费供应商由 UpsertUpstream 写入 cfg.Name）
func upstreamLabel(name string) string {
	return name
}
