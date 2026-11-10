package db

// syncBuiltins 插入内置上游（幂等）
func (d *DB) syncBuiltins() error {
	builtins := []struct {
		name  string
		label string
	}{
		{"joycode", "joycode"},
		{"deveco", "deveco"},
		{"opencode", "opencode"},
		{"workbuddy", "workbuddy"},
		{"mcloud", "mcloud"},
	}
	for _, b := range builtins {
		_, err := d.conn.Exec(
			`INSERT INTO upstreams (name, type, label, provider_id) VALUES (?, 'builtin', ?, NULL)
			 ON CONFLICT(name) DO UPDATE SET label=excluded.label`,
			b.name, b.label,
		)
		if err != nil {
			return err
		}
	}
	return nil
}

// UpsertUpstream 插入或更新上游，返回 id。
// providerID 仅 type='provider' 时传入，builtin 传空字符串。
func (d *DB) UpsertUpstream(name, typ, label, providerID string) (int64, error) {
	var pid *string
	if providerID != "" {
		pid = &providerID
	}
	res, err := d.conn.Exec(
		`INSERT INTO upstreams (name, type, label, provider_id) VALUES (?, ?, ?, ?)
		 ON CONFLICT(name) DO UPDATE SET type=excluded.type, label=excluded.label, provider_id=excluded.provider_id`,
		name, typ, label, pid,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// RemoveUpstreamByProvider 删除指定 provider 的 upstream 行
func (d *DB) RemoveUpstreamByProvider(providerID string) error {
	_, err := d.conn.Exec(`DELETE FROM upstreams WHERE provider_id = ?`, providerID)
	return err
}

// QueryUpstreamID 按 name 查询 upstream id，不存在返回 0
func (d *DB) QueryUpstreamID(name string) int64 {
	var id int64
	err := d.conn.QueryRow(`SELECT id FROM upstreams WHERE name = ?`, name).Scan(&id)
	if err != nil {
		return 0
	}
	return id
}
