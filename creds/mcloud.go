package creds

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/opt"
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
// Windows 客户端不写 accountdb.db，改走 localStorage LevelDB（见 loadCredsWindows）。
func (m *MCloudCredManager) LoadCreds() (*MCloudCred, error) {
	// Windows：仅当用户显式用 MCLOUD_ACCOUNTDB 指定了 db 时才走 SQLite 路径，
	// 否则读 localStorage（Windows 版移动云盘的真实登录态存储）。
	if runtime.GOOS == "windows" && os.Getenv("MCLOUD_ACCOUNTDB") == "" {
		return m.loadCredsWindows()
	}
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

// ===== Windows（Electron）登录态读取：Chromium localStorage LevelDB =====

// mcLocalUserInfo 移动云盘 Windows 客户端 localStorage 中 `UserInfo` 键的 JSON 结构。
// 与 macOS accountdb.db 的 t_account 表字段对应：
//
//	account      <-> account        手机号
//	authToken    <-> token          RCS token（形如 "<rand>|1|RCS|<issuedMs>|<sig>"）
//	userDomainId <-> userDomainId   请求体 userId
//	atExpiretime <-> tokenValidTime token 有效期（秒）
type mcLocalUserInfo struct {
	Account      string `json:"account"`
	AuthToken    string `json:"authToken"`
	UserDomainID string `json:"userDomainId"`
	AtExpireTime int64  `json:"atExpiretime"`
}

// loadCredsWindows 从 Windows 版移动云盘的 localStorage 读取登录态。
func (m *MCloudCredManager) loadCredsWindows() (*MCloudCred, error) {
	dir := paths.Resolve("MCLOUD_LEVELDB", paths.MCLocalStorageLevelDBCandidates())
	if _, err := os.Stat(dir); err != nil {
		return nil, fmt.Errorf("找不到移动云盘 localStorage 目录: %s\n请先安装并登录中国移动云盘 App，或用 MCLOUD_LEVELDB 环境变量指定", dir)
	}
	// 客户端运行时独占 LOCK，goleveldb 无法直接打开原目录，复制副本再读。
	tmp, err := copyLevelDBDir(dir)
	if err != nil {
		return nil, fmt.Errorf("复制移动云盘 localStorage 失败: %v", err)
	}
	defer os.RemoveAll(tmp)

	raw, err := readMCloudUserInfoJSON(tmp)
	if err != nil {
		return nil, err
	}
	var ui mcLocalUserInfo
	if err := json.Unmarshal(raw, &ui); err != nil {
		return nil, fmt.Errorf("解析移动云盘 UserInfo 失败: %v", err)
	}
	if ui.AuthToken == "" {
		return nil, fmt.Errorf("移动云盘 UserInfo.authToken 为空。请先在中国移动云盘 App 登录")
	}
	validSecs := ui.AtExpireTime
	if validSecs <= 0 {
		validSecs = 2592000 // 默认 30 天
	}
	return &MCloudCred{
		Phone:     ui.Account,
		UserID:    ui.UserDomainID,
		RCSToken:  ui.AuthToken,
		ExpiresAt: mcTokenIssuedMs(ui.AuthToken) + validSecs*1000,
		FetchedAt: time.Now(),
		Valid:     false,
	}, nil
}

// mcTokenIssuedMs 从 RCS token 第 4 段解析签发毫秒时间戳（"<rand>|1|RCS|<issuedMs>|<sig>"）。
// 解析失败返回 0，此时 ExpiresAt 仅由有效期决定（IsExpired 对 ExpiresAt<=0 视为不过期）。
func mcTokenIssuedMs(token string) int64 {
	parts := strings.Split(token, "|")
	if len(parts) < 4 {
		return 0
	}
	ms, err := strconv.ParseInt(parts[3], 10, 64)
	if err != nil {
		return 0
	}
	return ms
}

// readMCloudUserInfoJSON 在 localStorage leveldb 副本中定位含 authToken 的登录态 JSON。
func readMCloudUserInfoJSON(dir string) ([]byte, error) {
	db, err := leveldb.OpenFile(dir, &opt.Options{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("打开移动云盘 localStorage leveldb 失败: %v", err)
	}
	defer db.Close()
	it := db.NewIterator(nil, nil)
	defer it.Release()
	for it.Next() {
		v := decodeLocalStorageValue(it.Value())
		if strings.Contains(v, `"authToken"`) {
			return []byte(v), nil
		}
	}
	if err := it.Error(); err != nil {
		return nil, fmt.Errorf("读取移动云盘 localStorage 失败: %v", err)
	}
	return nil, fmt.Errorf("移动云盘 localStorage 中未找到登录态（UserInfo.authToken）。请先在中国移动云盘 App 登录")
}

// decodeLocalStorageValue 解码 Chromium localStorage 的 value：
// 首字节 0x01 = UTF-8，0x00 = UTF-16LE。
func decodeLocalStorageValue(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	switch b[0] {
	case 0x01:
		return string(b[1:])
	case 0x00:
		u := make([]uint16, 0, (len(b)-1)/2)
		for i := 1; i+1 < len(b); i += 2 {
			u = append(u, uint16(b[i])|uint16(b[i+1])<<8)
		}
		return string(utf16.Decode(u))
	}
	return string(b)
}

// copyLevelDBDir 把 localStorage leveldb 复制到临时目录（跳过被客户端独占的 LOCK 与
// leveldb 自身的信息日志 LOG），返回临时目录路径，调用方负责 os.RemoveAll。
func copyLevelDBDir(src string) (string, error) {
	tmp, err := os.MkdirTemp("", "mcloud-ls-")
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		os.RemoveAll(tmp)
		return "", err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		switch name {
		case "LOCK", "LOG", "LOG.old":
			continue // LOCK 被客户端独占；LOG 是 leveldb 信息日志，非数据
		}
		data, err := os.ReadFile(filepath.Join(src, name))
		if err != nil {
			continue // 单个文件被占用则跳过（尽量读出可用数据）
		}
		_ = os.WriteFile(filepath.Join(tmp, name), data, 0o600)
	}
	return tmp, nil
}
