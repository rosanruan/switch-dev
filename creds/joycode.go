package creds

import (
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
	"switchdev/paths"
)

// JoyCode 凭据配置
type JoyCodeConfig struct {
	AppID         string
	Secret        string
	FunctionID    string
	Client        string
	ClientVersion string
	Language      string
	VscdbPath     string
	ExtPkgPath    string // 扩展 package.json 路径（读取真实 clientVersion 用）
}

func DefaultJoyCodeConfig() JoyCodeConfig {
	return JoyCodeConfig{
		AppID:         "joycode_ide",
		Secret:        "0691a3f0b37b4a85aeb63ad0fc7db3ed",
		FunctionID:    "chat_completions",
		Client:        "JoyCodeIDE",
		ClientVersion: "3.8.67",
		Language:      "UNKNOWN",
		VscdbPath:     paths.Resolve("JOYCODE_VSCDB", paths.JoyCodeVscdbCandidates()),
		ExtPkgPath:    paths.Resolve("JOYCODE_EXT_PKG", paths.JoyCodeExtPkgCandidates()),
	}
}

// JoyCodeCred 运行时凭据
type JoyCodeCred struct {
	PtKey         string
	UserID        string
	Tenant        string
	LoginType     string
	OrgFullName   string
	Origin        string
	ClientVersion string
	FetchedAt     time.Time
	Valid         bool
}

// JoyCodeCredManager JoyCode 凭据管理器
type JoyCodeCredManager struct {
	mu     sync.RWMutex
	cred   *JoyCodeCred
	config JoyCodeConfig
}

func NewJoyCodeCredManager(config JoyCodeConfig) *JoyCodeCredManager {
	return &JoyCodeCredManager{config: config}
}

// LoadCredsFromVscdb 从 state.vscdb 读取凭据
func (m *JoyCodeCredManager) LoadCredsFromVscdb() (*JoyCodeCred, error) {
	dbPath := m.config.VscdbPath
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("找不到 JoyCode state.vscdb: %s\n请确认 JoyCode 已安装并登录过，或用 JOYCODE_VSCDB 环境变量指定路径", dbPath)
	}

	// 用嵌入式 sqlite 读取（modernc.org/sqlite，纯 Go，无需外部 sqlite3 命令）
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("打开 state.vscdb 失败: %v", err)
	}
	defer db.Close()
	db.Exec("PRAGMA busy_timeout = 5000") // 容忍 JoyCode 进程占用时的短暂锁

	var rawBytes []byte
	if err := db.QueryRow("SELECT value FROM ItemTable WHERE key='JoyCoder.IDE'").Scan(&rawBytes); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("state.vscdb 里没有 JoyCoder.IDE 登录信息。请先在 JoyCode 客户端登录。")
		}
		return nil, fmt.Errorf("读取 state.vscdb 失败: %v", err)
	}

	// 读取客户端真实版本，用于注入 clientVersion 字段；硬编码旧版本会被网关灰度策略拒绝
	// （AI_GRAY_ACCESS_DENIED）。官方客户端注入的是**扩展自身 package.json 的 version**
	// （getVersion()），而不是 state.vscdb 的 releaseNotes/lastVersion（那是应用壳版本，
	// 可能远旧于扩展版本，例如壳 3.0.10 / 扩展 3.8.67）。因此优先读扩展 package.json，
	// 其次 vscdb，最后兜底当前默认版本。
	clientVersion := m.resolveClientVersion(db)

	raw := strings.TrimSpace(string(rawBytes))
	if raw == "" {
		return nil, fmt.Errorf("state.vscdb 里没有 JoyCoder.IDE 登录信息。请先在 JoyCode 客户端登录。")
	}

	// 解析 JSON
	var data struct {
		JoyCoderUser struct {
			PtKey        string `json:"ptKey"`
			UserName     string `json:"userName"`
			UserID       string `json:"userId"`
			LoginType    string `json:"loginType"`
			OrgFullName  string `json:"orgFullName"`
			Tenant       string `json:"tenant"`
			ColorBaseURL string `json:"colorBaseUrl"`
		} `json:"joyCoderUser"`
	}

	if err := parseJSON([]byte(raw), &data); err != nil {
		return nil, fmt.Errorf("state.vscdb 的 JoyCoder.IDE 内容无法解析为 JSON: %v", err)
	}

	u := data.JoyCoderUser
	if u.PtKey == "" {
		return nil, fmt.Errorf("state.vscdb 里 ptKey 为空，请先在 JoyCode 客户端扫码登录")
	}

	loginType := u.LoginType
	if loginType == "" {
		loginType = "PIN_JD_CLOUD"
	}

	orgFullName := u.OrgFullName
	if orgFullName == "" {
		orgFullName = "" // 空串即可
	}

	origin := u.ColorBaseURL
	if origin == "" {
		origin = "https://api-ai.jd.com"
	}

	if clientVersion == "" {
		clientVersion = m.config.ClientVersion
	}

	return &JoyCodeCred{
		PtKey:         u.PtKey,
		UserID:        u.UserID,
		Tenant:        u.Tenant,
		LoginType:     loginType,
		OrgFullName:   orgFullName,
		Origin:        origin,
		ClientVersion: clientVersion,
		FetchedAt:     time.Now(),
		Valid:         false, // 待校验
	}, nil
}

// resolveClientVersion 解析要注入的 clientVersion，优先级与官方客户端对齐：
//  1. 扩展 package.json 的 version（官方 getVersion() 的来源，灰度放行的真实版本）
//  2. state.vscdb 的 releaseNotes/lastVersion（应用壳版本，仅作扩展缺失时的回退）
//  3. 空串（交由调用方回退到 config.ClientVersion 当前默认版本）
func (m *JoyCodeCredManager) resolveClientVersion(db *sql.DB) string {
	// 1. 扩展 package.json version
	if v := readExtPkgVersion(m.config.ExtPkgPath); v != "" {
		return v
	}
	// 2. vscdb releaseNotes/lastVersion
	var v string
	_ = db.QueryRow("SELECT value FROM ItemTable WHERE key='releaseNotes/lastVersion'").Scan(&v)
	return strings.TrimSpace(v)
}

// readExtPkgVersion 从扩展 package.json 读取 version 字段（失败返回空串）
func readExtPkgVersion(pkgPath string) string {
	if pkgPath == "" {
		return ""
	}
	raw, err := os.ReadFile(pkgPath)
	if err != nil {
		return ""
	}
	var pkg struct {
		Version string `json:"version"`
	}
	if err := parseJSON(raw, &pkg); err != nil {
		return ""
	}
	return strings.TrimSpace(pkg.Version)
}

// EnsureCreds 确保有有效凭据
func (m *JoyCodeCredManager) EnsureCreds() (*JoyCodeCred, error) {
	m.mu.RLock()
	if m.cred != nil && m.cred.Valid {
		cred := *m.cred
		m.mu.RUnlock()
		return &cred, nil
	}
	m.mu.RUnlock()

	fresh, err := m.LoadCredsFromVscdb()
	if err != nil {
		return nil, err
	}

	// vscdb 没更新（pt_key 和上次缓存的失效 pt_key 相同）-> 客户端没重登
	m.mu.RLock()
	sameAsCached := m.cred != nil && fresh.PtKey == m.cred.PtKey
	m.mu.RUnlock()

	// 预校验（调 userInfo 端点）
	valid, code, err := m.VerifyCreds(fresh)
	if err != nil {
		m.mu.Lock()
		m.cred = fresh
		m.mu.Unlock()
		return nil, fmt.Errorf("pt_key 校验请求失败: %v", err)
	}
	fresh.Valid = valid

	if !valid {
		m.mu.Lock()
		m.cred = fresh
		m.mu.Unlock()
		if sameAsCached {
			return nil, NewCredentialsError("pt_key 已失效，且 state.vscdb 未更新（pt_key 未变化）。请在 JoyCode 客户端重新扫码登录，登录后代理会自动恢复。登录页：https://joycode.jd.com/portal/login")
		}
		return nil, NewCredentialsError("pt_key 校验失败（USER_INFO 返回 code=%d）。请在 JoyCode 客户端重新扫码登录，登录后代理会自动恢复。登录页：https://joycode.jd.com/portal/login", code)
	}

	m.mu.Lock()
	m.cred = fresh
	m.mu.Unlock()
	return fresh, nil
}

// InvalidateCreds 标记凭据失效
func (m *JoyCodeCredManager) InvalidateCreds() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cred != nil {
		m.cred.Valid = false
	}
}

// GetCred 获取当前凭据（不触发刷新）
func (m *JoyCodeCredManager) GetCred() *JoyCodeCred {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.cred == nil {
		return nil
	}
	cred := *m.cred
	return &cred
}

// CredStatus 返回凭据状态信息
func (m *JoyCodeCredManager) CredStatus() *CredStatusInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()

	info := &CredStatusInfo{
		Source: "state.vscdb",
	}

	if m.cred != nil {
		info.Valid = m.cred.Valid
		info.UserID = m.cred.UserID
		info.LastCheck = m.cred.FetchedAt.Format(time.RFC3339)
		if len(m.cred.PtKey) > 8 {
			info.KeyPreview = m.cred.PtKey[:8] + "..."
		} else {
			info.KeyPreview = m.cred.PtKey
		}
	} else if _, err := os.Stat(m.config.VscdbPath); os.IsNotExist(err) {
		info.Source = "NOT_FOUND"
	}

	// 注入 agent 安装/登录元数据 + Installed 探测
	FillAgentMeta(info, "joycode")
	return info
}

// VerifyCreds 调 USER_INFO 校验 pt_key 有效性
func (m *JoyCodeCredManager) VerifyCreds(cred *JoyCodeCred) (valid bool, code int, err error) {
	t := time.Now().UnixMilli()
	sign := ColorSign(m.config.AppID, "joycode_userInfo", t, nil)
	url := fmt.Sprintf("%s/api?appid=%s&functionId=joycode_userInfo&t=%d&sign=%s",
		cred.Origin, m.config.AppID, t, sign)

	// 使用简化 HTTP 请求
	body, _, err := httpPost(url, "{}", map[string]string{
		"Content-Type": "application/json; charset=UTF-8",
		"ptKey":        cred.PtKey,
		"loginType":    cred.LoginType,
		"Accept":       "application/json",
	})
	if err != nil {
		return false, -1, err
	}

	var resp struct {
		Code int `json:"code"`
		Data struct {
			LoginURL string `json:"loginUrl"`
		} `json:"data"`
	}
	if err := parseJSON(body, &resp); err != nil {
		return false, -1, nil
	}

	return resp.Code == 0, resp.Code, nil
}

// ColorSign 京东云 Color 网关签名
func ColorSign(appid, functionID string, t int64, extra map[string]string) string {
	params := map[string]string{
		"appid":      appid,
		"functionId": functionID,
		"t":          fmt.Sprintf("%d", t),
	}
	for k, v := range extra {
		params[k] = v
	}

	// 按 key 字母序，取 value 用 & 拼接
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var values []string
	for _, k := range keys {
		v := params[k]
		if v != "" {
			values = append(values, v)
		}
	}
	str := strings.Join(values, "&")

	mac := hmac.New(sha256.New, []byte("0691a3f0b37b4a85aeb63ad0fc7db3ed"))
	mac.Write([]byte(str))
	return hex.EncodeToString(mac.Sum(nil))
}
