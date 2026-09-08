package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

// DB 封装 SQLite 连接 + 字段加密器
type DB struct {
	conn   *sql.DB
	crypto *Crypto
}

// Open 打开（或创建）SQLite 数据库，执行建表和内置 upstream 初始化
func Open(path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, fmt.Errorf("创建数据目录失败: %w", err)
	}

	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)&_pragma=synchronous(NORMAL)"
	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}
	conn.SetMaxOpenConns(4)

	if err := conn.Ping(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("连接数据库失败: %w", err)
	}

	d := &DB{conn: conn, crypto: NewCrypto()}
	if err := d.migrate(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("建表失败: %w", err)
	}
	if err := d.syncBuiltins(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("初始化内置上游失败: %w", err)
	}

	go d.runJSONLMigration()
	return d, nil
}

// Close 关闭数据库连接
func (d *DB) Close() error {
	return d.conn.Close()
}

// SetDEK 注入或清除数据加密密钥
func (d *DB) SetDEK(dek []byte) {
	d.crypto.SetDEK(dek)
}

// Raw 返回底层 *sql.DB（迁移等特殊场景使用）
func (d *DB) Raw() *sql.DB {
	return d.conn
}

// migrate 执行建表（IF NOT EXISTS，幂等）
func (d *DB) migrate() error {
	schemas := []string{
		`CREATE TABLE IF NOT EXISTS sources (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			name       TEXT NOT NULL DEFAULT '',
			user_agent TEXT NOT NULL DEFAULT '',
			UNIQUE(name, user_agent)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_sources_name ON sources(name)`,

		`CREATE TABLE IF NOT EXISTS providers (
			id             TEXT PRIMARY KEY,
			name           TEXT NOT NULL DEFAULT '',
			base_url       TEXT NOT NULL DEFAULT '',
			protocol       TEXT NOT NULL DEFAULT '',
			created_at     TEXT NOT NULL DEFAULT ''
		)`,

		`CREATE TABLE IF NOT EXISTS upstreams (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			name        TEXT NOT NULL UNIQUE,
			type        TEXT NOT NULL DEFAULT 'builtin',
			label       TEXT NOT NULL DEFAULT '',
			provider_id TEXT DEFAULT NULL,
			FOREIGN KEY (provider_id) REFERENCES providers(id) ON DELETE SET NULL
		)`,

		`CREATE TABLE IF NOT EXISTS models (
			id             INTEGER PRIMARY KEY AUTOINCREMENT,
			upstream_id    INTEGER NOT NULL,
			model_id       TEXT NOT NULL DEFAULT '',
			label          TEXT NOT NULL DEFAULT '',
			context        INTEGER NOT NULL DEFAULT 0,
			output         INTEGER NOT NULL DEFAULT 0,
			stream         INTEGER NOT NULL DEFAULT 0,
			vision         INTEGER NOT NULL DEFAULT 0,
			tool_call      INTEGER NOT NULL DEFAULT 0,
			free           INTEGER NOT NULL DEFAULT 0,
			reasoning      INTEGER NOT NULL DEFAULT 0,
			wire_name      TEXT NOT NULL DEFAULT '',
			available      INTEGER NOT NULL DEFAULT 1,
			catalog_source TEXT NOT NULL DEFAULT '',
			synced_at      TEXT NOT NULL DEFAULT '',
			verified       INTEGER NOT NULL DEFAULT 0,
			healthy        INTEGER NOT NULL DEFAULT 0,
			bench_tps      REAL NOT NULL DEFAULT 0,
			bench_duration INTEGER NOT NULL DEFAULT 0,
			bench_at       TEXT NOT NULL DEFAULT '',
			bench_error    TEXT NOT NULL DEFAULT '',
			FOREIGN KEY (upstream_id) REFERENCES upstreams(id),
			UNIQUE(upstream_id, model_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_models_upstream ON models(upstream_id)`,

		`CREATE TABLE IF NOT EXISTS logs (
			id               INTEGER PRIMARY KEY AUTOINCREMENT,
			date_time        TEXT NOT NULL,
			date             TEXT NOT NULL,
			model_id         INTEGER NOT NULL DEFAULT 0,
			used_model_id    INTEGER NOT NULL DEFAULT 0,
			source_id        INTEGER NOT NULL DEFAULT 0,
			upstream_id      INTEGER NOT NULL DEFAULT 0,
			status           TEXT NOT NULL DEFAULT '',
			code             INTEGER NOT NULL DEFAULT 0,
			duration         INTEGER NOT NULL DEFAULT 0,
			error_msg        TEXT NOT NULL DEFAULT '',
			method           TEXT NOT NULL DEFAULT '',
			path             TEXT NOT NULL DEFAULT '',
			stream           INTEGER NOT NULL DEFAULT 0,
			request_body     TEXT NOT NULL DEFAULT '',
			response_body    TEXT NOT NULL DEFAULT '',
			input_tokens     INTEGER NOT NULL DEFAULT 0,
			output_tokens    INTEGER NOT NULL DEFAULT 0,
			cache_hit_tokens INTEGER NOT NULL DEFAULT 0,
			cost             REAL NOT NULL DEFAULT 0,
			cost_text        TEXT NOT NULL DEFAULT '',
			first_byte_ms    INTEGER NOT NULL DEFAULT 0,
			FOREIGN KEY (model_id)      REFERENCES models(id),
			FOREIGN KEY (used_model_id) REFERENCES models(id),
			FOREIGN KEY (source_id)     REFERENCES sources(id),
			FOREIGN KEY (upstream_id)   REFERENCES upstreams(id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_logs_date_status   ON logs(date, status)`,
		`CREATE INDEX IF NOT EXISTS idx_logs_date_datetime ON logs(date, date_time DESC, id DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_logs_upstream  ON logs(upstream_id)`,
		`CREATE INDEX IF NOT EXISTS idx_logs_source    ON logs(source_id)`,
		`CREATE INDEX IF NOT EXISTS idx_logs_model     ON logs(model_id)`,

		// usage_stats：按小时桶聚合的用量统计表。与 logs 解耦 —— 清空 logs 不影响统计。
		// 只累计 status='success' 的请求（与现有统计口径一致）。
		// model_id 存「实际生效模型」（used_model 优先，回退 model），source_id/upstream_id 归一化外键。
		`CREATE TABLE IF NOT EXISTS usage_stats (
			hour             TEXT NOT NULL,
			date             TEXT NOT NULL,
			upstream_id      INTEGER NOT NULL DEFAULT 0,
			model_id         INTEGER NOT NULL DEFAULT 0,
			source_id        INTEGER NOT NULL DEFAULT 0,
			reqs             INTEGER NOT NULL DEFAULT 0,
			input_tokens     INTEGER NOT NULL DEFAULT 0,
			output_tokens    INTEGER NOT NULL DEFAULT 0,
			cache_hit_tokens INTEGER NOT NULL DEFAULT 0,
			cache_hit_reqs   INTEGER NOT NULL DEFAULT 0,
			cost             REAL NOT NULL DEFAULT 0,
			duration_ms      INTEGER NOT NULL DEFAULT 0,
			output_reqs      INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (hour, upstream_id, model_id, source_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_usage_stats_date ON usage_stats(date)`,

		`CREATE TABLE IF NOT EXISTS config (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
	}
	for _, schema := range schemas {
		if _, err := d.conn.Exec(schema); err != nil {
			return fmt.Errorf("执行 schema 失败: %w\nSQL: %s", err, schema)
		}
	}

	// 增量加列：老库建表时没有这些列，CREATE TABLE IF NOT EXISTS 不会补。
	// 必须在哨兵行之前执行，否则新列的 NOT NULL DEFAULT 对已有行不生效。
	if err := d.addColumns("models", []columnDef{
		{"reasoning", "INTEGER NOT NULL DEFAULT 0"},
		{"wire_name", "TEXT NOT NULL DEFAULT ''"},
		{"available", "INTEGER NOT NULL DEFAULT 1"},
		{"catalog_source", "TEXT NOT NULL DEFAULT ''"},
		{"synced_at", "TEXT NOT NULL DEFAULT ''"},
	}); err != nil {
		return err
	}

	// 哨兵行：id=0 的 source/upstream/model，供日志外键引用空值（model="" 或 "auto" 时 upsertModelInTx 返回 0）
	// SQLite AUTOINCREMENT 从 1 开始，手动插入 id=0 不会冲突
	d.conn.Exec(`INSERT OR IGNORE INTO sources (id, name, user_agent) VALUES (0, '', '')`)
	d.conn.Exec(`INSERT OR IGNORE INTO upstreams (id, name, type, label) VALUES (0, 'unknown', 'builtin', '未知')`)
	d.conn.Exec(`INSERT OR IGNORE INTO models (id, upstream_id, model_id, label) VALUES (0, 0, '', '')`)

	return nil
}

// columnDef 增量加列的列定义
type columnDef struct {
	name string
	decl string // 类型 + 约束，如 "INTEGER NOT NULL DEFAULT 0"
}

// addColumns 幂等地给已有表加列。本项目没有版本化迁移，靠 PRAGMA table_info
// 探测现有列名，缺失才 ALTER。重复执行安全（升级路径会反复调用）。
func (d *DB) addColumns(table string, cols []columnDef) error {
	rows, err := d.conn.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return fmt.Errorf("读取 %s 表结构失败: %w", table, err)
	}
	existing := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err == nil {
			existing[name] = true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("读取 %s 表结构失败: %w", table, err)
	}

	for _, c := range cols {
		if existing[c.name] {
			continue
		}
		_, err := d.conn.Exec(fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, table, c.name, c.decl))
		// 并发/重入下可能已被别的路径加上，这种错误可忽略
		if err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("给 %s 加列 %s 失败: %w", table, c.name, err)
		}
	}
	return nil
}
