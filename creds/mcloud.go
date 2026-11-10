package creds

import (
	"database/sql"
	"encoding/base64"
	"fmt"
	"os"
	"sync"
	"time"

	_ "modernc.org/sqlite"
	"switchdev/paths"
)

// MCloud（中国移动云盘 AI 工作台 / 139 邮箱 webai 灵犀）配置
type MCloudConfig struct {
	BaseURL string
	DBPath  string
}

func DefaultMCloudConfig() MCloudConfig {
	return MCloudConfig{
		BaseURL: "https://ai.yun.139.com/api/outer",
		DBPath:  paths.Resolve("MCLOUD_ACCOUNTDB", paths.MCAccountdbCandidates()),
	}
}

// MCloudCred 运行时凭据
type MCloudCred struct {
	Phone     string // 手机号（t_account.account）
	UserID    string // userDomainId（请求体 userId 字段）
	RCSToken  string // t_account.token，30 天滚动
	ExpiresAt int64  // 毫秒时间戳（tokenGetTime + tokenValidTime*1000）
	FetchedAt time.Time
	Valid     bool
}

// MCloudCredManager 移动云盘凭据管理器
type MCloudCredManager struct {
	mu     sync.RWMutex
	cred   *MCloudCred
	config MCloudConfig
}

func NewMCloudCredManager(config MCloudConfig) *MCloudCredManager {
	return &MCloudCredManager{config: config}
}

// Config 返回当前配置
func (m *MCloudCredManager) Config() MCloudConfig { return m.config }

// LoadCreds 从 accountdb.db 读取 RCS token（明文 SQLite，modernc.org/sqlite 纯 Go 驱动）
func (m *MCloudCredManager) LoadCreds() (*MCloudCred, error) {
	if _, err := os.Stat(m.config.DBPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("找不到移动云盘 accountdb.db: %s\n请先安装中国移动云盘 App 并登录，或用 MCLOUD_ACCOUNTDB 环境变量指定路径", m.config.DBPath)
	}

	db, err := sql.Open("sqlite", m.config.DBPath)
	if err != nil {
		return nil, fmt.Errorf("打开 accountdb.db 失败: %v", err)
	}
	defer db.Close()
	db.Exec("PRAGMA busy_timeout = 5000") // 容忍移动云盘进程占用时的短暂锁

	var phone, token, domainID sql.NullString
	var getTime, validTime sql.NullInt64
	if err := db.QueryRow("SELECT account, token, userDomainId, tokenGetTime, tokenValidTime FROM t_account LIMIT 1").
		Scan(&phone, &token, &domainID, &getTime, &validTime); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("accountdb.db 的 t_account 表为空。请先在中国移动云盘 App 登录")
		}
		return nil, fmt.Errorf("读取 t_account 失败: %v", err)
	}
	if token.String == "" {
		return nil, fmt.Errorf("t_account.token 为空。请先在中国移动云盘 App 登录")
	}

	var expiresAt int64
	if getTime.Valid && validTime.Valid {
		// tokenGetTime 毫秒时间戳，tokenValidTime 有效期秒数（实测 2592000 = 30 天）
		expiresAt = getTime.Int64 + validTime.Int64*1000
	}

	return &MCloudCred{
		Phone:     phone.String,
		UserID:    domainID.String,
		RCSToken:  token.String,
		ExpiresAt: expiresAt,
		FetchedAt: time.Now(),
		Valid:     false,
	}, nil
}

// IsExpired token 是否已过有效期窗口
func (c *MCloudCred) IsExpired() bool {
	return c.ExpiresAt > 0 && c.ExpiresAt <= time.Now().UnixMilli()
}

// EnsureCreds 确保有有效凭据。移动云盘的 Basic 头是纯本地拼装（无网络换票），
// 本地有效期检查是唯一的预检手段；服务端拒绝（401 / code 01000002）由上游层重读重试兜底。
func (m *MCloudCredManager) EnsureCreds() (*MCloudCred, error) {
	m.mu.RLock()
	if m.cred != nil && m.cred.Valid && !m.cred.IsExpired() {
		cred := *m.cred
		m.mu.RUnlock()
		return &cred, nil
	}
	m.mu.RUnlock()

	fresh, err := m.LoadCreds()
	if err != nil {
		return nil, err
	}
	if fresh.IsExpired() {
		m.mu.Lock()
		m.cred = fresh
		m.mu.Unlock()
		return nil, NewCredentialsError("移动云盘 RCS token 已过期（30 天滚动）。打开中国移动云盘 App 登录即可自动续期")
	}
	fresh.Valid = true
	m.mu.Lock()
	m.cred = fresh
	m.mu.Unlock()
	return fresh, nil
}

// InvalidateCreds 标记凭据失效（服务端 401/01000002 时调用，下次 EnsureCreds 重读 DB）
func (m *MCloudCredManager) InvalidateCreds() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cred != nil {
		m.cred.Valid = false
	}
}

// GetCred 获取当前凭据
func (m *MCloudCredManager) GetCred() *MCloudCred {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.cred == nil {
		return nil
	}
	cred := *m.cred
	return &cred
}

// CredStatus 凭据状态信息
func (m *MCloudCredManager) CredStatus() *CredStatusInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()

	info := &CredStatusInfo{Source: "accountdb.db"}
	if m.cred != nil {
		info.Valid = m.cred.Valid && !m.cred.IsExpired()
		info.UserID = m.cred.Phone
		info.LastCheck = m.cred.FetchedAt.Format(time.RFC3339)
		if m.cred.ExpiresAt > 0 {
			info.ExpiresAt = time.UnixMilli(m.cred.ExpiresAt).Format(time.RFC3339)
		}
		if len(m.cred.RCSToken) > 8 {
			info.KeyPreview = m.cred.RCSToken[:8] + "..."
		}
	} else if _, err := os.Stat(m.config.DBPath); os.IsNotExist(err) {
		info.Source = "NOT_FOUND"
	}

	FillAgentMeta(info, "mcloud")
	return info
}

// MCloudBasicAuth 拼装 Authorization 头：Basic base64("mobile:<手机号>:<RCS token>")。
// 纯本地拼装，无需网络换票（逆向自 webai getHeaderAiChat）。
func MCloudBasicAuth(cred *MCloudCred) string {
	raw := fmt.Sprintf("mobile:%s:%s", cred.Phone, cred.RCSToken)
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(raw))
}
