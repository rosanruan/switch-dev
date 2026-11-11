package creds

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"switchdev/paths"
)

// WorkBuddy 配置
type WorkBuddyConfig struct {
	BaseURL          string
	InfoPath         string
	VerifyIntervalMs int
}

func DefaultWorkBuddyConfig() WorkBuddyConfig {
	return WorkBuddyConfig{
		BaseURL:          "https://copilot.tencent.com/v2",
		InfoPath:         paths.Resolve("WORKBUDDY_INFO_PATH", paths.WorkBuddyInfoCandidates()),
		VerifyIntervalMs: 600000,
	}
}

// WorkBuddyCred 运行时凭据
type WorkBuddyCred struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    int64 // 毫秒时间戳
	UID          string
	Nickname     string
	// Domain 来自 info 文件 auth.domain，新版 WorkBuddy AI 客户端会记录实际登录域
	// （如 www.workbuddy.ai）。非空时上游 base URL 以它为准——官方 2026 年把主端点
	// 从 copilot.tencent.com 迁到了新域，旧域的 /v2/chat/completions 已被网关 401 拒绝。
	Domain    string
	FetchedAt time.Time
	Valid     bool
}

// BaseURL 返回该凭据实际使用的上游 base URL：info 文件记录了登录域（auth.domain）时以它为准
// —— 新版 WorkBuddy AI 客户端（2026-09）把主端点从 copilot.tencent.com 迁到 www.workbuddy.ai，
// 旧域的 /v2/chat/completions 已被网关 401 拒绝；无 domain（旧版 CodeBuddy 客户端）回退 fallback。
func (c *WorkBuddyCred) BaseURL(fallback string) string {
	if c.Domain != "" {
		return "https://" + c.Domain + "/v2"
	}
	return fallback
}

// Platform 返回该凭据对应产品的 platform 参数（auth/state 鉴权用）：新版 workbuddy-ai，旧版 workbuddy
func (c *WorkBuddyCred) Platform() string {
	if c.Domain != "" {
		return "workbuddy-ai"
	}
	return "workbuddy"
}

// WorkBuddyCredManager WorkBuddy 凭据管理器
type WorkBuddyCredManager struct {
	mu     sync.RWMutex
	cred   *WorkBuddyCred
	config WorkBuddyConfig
}

func NewWorkBuddyCredManager(config WorkBuddyConfig) *WorkBuddyCredManager {
	return &WorkBuddyCredManager{config: config}
}

// Config 返回当前配置
func (m *WorkBuddyCredManager) Config() WorkBuddyConfig { return m.config }

// LoadCreds 从 info 文件读 accessToken/refreshToken（明文 JSON）
func (m *WorkBuddyCredManager) LoadCreds() (*WorkBuddyCred, error) {
	if _, err := os.Stat(m.config.InfoPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("找不到 WorkBuddy 凭据文件: %s\n请先安装 WorkBuddy 客户端并登录", m.config.InfoPath)
	}
	data, err := os.ReadFile(m.config.InfoPath)
	if err != nil {
		return nil, fmt.Errorf("读取 WorkBuddy info 失败: %v", err)
	}

	var info struct {
		Account struct {
			UID      string `json:"uid"`
			Nickname string `json:"nickname"`
			UIN      string `json:"uin"`
		} `json:"account"`
		Auth struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			TokenType    string `json:"tokenType"`
			ExpiresIn    int64  `json:"expiresIn"`
			ExpiresAt    int64  `json:"expiresAt"`
			// domain 是新版 WorkBuddy AI 客户端记录的登录域（如 www.workbuddy.ai），
			// 旧版 CodeBuddy 的 info 文件没有该字段
			Domain string `json:"domain"`
		} `json:"auth"`
	}
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, fmt.Errorf("解析 WorkBuddy info 失败: %v", err)
	}
	if info.Auth.AccessToken == "" {
		return nil, fmt.Errorf("WorkBuddy info 里无 accessToken，请在 WorkBuddy 客户端重新登录")
	}

	return &WorkBuddyCred{
		AccessToken:  info.Auth.AccessToken,
		RefreshToken: info.Auth.RefreshToken,
		ExpiresAt:    info.Auth.ExpiresAt,
		UID:          info.Account.UID,
		Nickname:     info.Account.Nickname,
		Domain:       normalizeWorkBuddyDomain(info.Auth.Domain),
		FetchedAt:    time.Now(),
		Valid:        false,
	}, nil
}

// normalizeWorkBuddyDomain 归一化 info 文件里的 domain：剥掉可能的协议前缀与斜杠，
// 只留 host（"https://www.workbuddy.ai/" -> "www.workbuddy.ai"）
func normalizeWorkBuddyDomain(domain string) string {
	d := strings.TrimSpace(domain)
	d = strings.TrimPrefix(d, "https://")
	d = strings.TrimPrefix(d, "http://")
	d = strings.Trim(d, "/")
	return d
}

// EnsureCreds 确保有有效凭据（含自动 refresh 续期）
func (m *WorkBuddyCredManager) EnsureCreds() (*WorkBuddyCred, error) {
	// 内存有效且未快过期（剩余 >5 分钟）-> 直接返回，避免每次都打网络
	m.mu.RLock()
	if m.cred != nil && m.cred.Valid && m.cred.ExpiresAt > time.Now().UnixMilli()+300000 {
		cred := *m.cred
		m.mu.RUnlock()
		return &cred, nil
	}
	m.mu.RUnlock()

	fresh, err := m.LoadCreds()
	if err != nil {
		return nil, err
	}

	// 若磁盘上的 token 已过期，先 refresh
	if fresh.ExpiresAt > 0 && fresh.ExpiresAt <= time.Now().UnixMilli() {
		rf, rerr := m.RefreshToken(fresh)
		if rerr == nil {
			fresh.AccessToken = rf.AccessToken
			if rf.RefreshToken != "" {
				fresh.RefreshToken = rf.RefreshToken
			}
			if rf.ExpiresAt > 0 {
				fresh.ExpiresAt = rf.ExpiresAt
			}
		} else {
			m.mu.Lock()
			m.cred = fresh
			m.mu.Unlock()
			return nil, NewCredentialsError("WorkBuddy accessToken 已过期且刷新失败: %v。请重新登录 WorkBuddy 客户端", rerr)
		}
	}

	// 预校验
	valid, status, verr := m.VerifyCreds(fresh)
	if verr != nil {
		m.mu.Lock()
		m.cred = fresh
		m.mu.Unlock()
		return nil, fmt.Errorf("WorkBuddy 预校验请求失败: %v", verr)
	}
	if !valid {
		// 校验失败，尝试 refresh 一次
		if rf, rerr := m.RefreshToken(fresh); rerr == nil {
			fresh.AccessToken = rf.AccessToken
			if rf.RefreshToken != "" {
				fresh.RefreshToken = rf.RefreshToken
			}
			if rf.ExpiresAt > 0 {
				fresh.ExpiresAt = rf.ExpiresAt
			}
			valid, status, verr = m.VerifyCreds(fresh)
		}
	}
	fresh.Valid = valid
	m.mu.Lock()
	m.cred = fresh
	m.mu.Unlock()

	if !valid {
		return nil, NewCredentialsError("WorkBuddy accessToken 校验失败（status=%d）。请重新登录 WorkBuddy 客户端", status)
	}
	return fresh, nil
}

// InvalidateCreds 标记凭据失效
func (m *WorkBuddyCredManager) InvalidateCreds() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cred != nil {
		m.cred.Valid = false
	}
}

// GetCred 获取当前凭据
func (m *WorkBuddyCredManager) GetCred() *WorkBuddyCred {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.cred == nil {
		return nil
	}
	cred := *m.cred
	return &cred
}

// CredStatus 凭据状态信息
func (m *WorkBuddyCredManager) CredStatus() *CredStatusInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()

	info := &CredStatusInfo{Source: "workbuddy-desktop.info"}
	if m.cred != nil {
		info.Valid = m.cred.Valid
		info.UserID = m.cred.Nickname
		info.LastCheck = m.cred.FetchedAt.Format(time.RFC3339)
		if m.cred.ExpiresAt > 0 {
			info.ExpiresAt = time.UnixMilli(m.cred.ExpiresAt).Format(time.RFC3339)
		}
		if len(m.cred.AccessToken) > 8 {
			info.KeyPreview = m.cred.AccessToken[:8] + "..."
		}
	} else if _, err := os.Stat(m.config.InfoPath); os.IsNotExist(err) {
		info.Source = "NOT_FOUND"
	}

	FillAgentMeta(info, "workbuddy")
	return info
}

// WorkBuddyAuthHeaders 返回与官方 WorkBuddy 客户端一致的鉴权请求头。
// 用于 /plugin/auth/* 鉴权链路与 /models 拉取，使指纹与官方客户端一致，避免风控误伤。
// 对齐新版 CLI（@tencent-ai/codebuddy-code 2.147.x）auth 拦截器：X-User-Id / X-Domain / X-Product。
// 注意：新版个人账号的请求不再携带 X-Userinfo（仅企业或代登场景才发，且格式已变为
// {uin,owner_uin,...}），旧格式 base64({"sub":uid}) 已是过期指纹，这里不再构造。
func WorkBuddyAuthHeaders(cred *WorkBuddyCred) map[string]string {
	h := map[string]string{
		"Authorization":    fmt.Sprintf("Bearer %s", cred.AccessToken),
		"Content-Type":     "application/json",
		"Accept":           "application/json",
		"X-Requested-With": "XMLHttpRequest",
		"X-Product":        "SaaS",
	}
	if cred.UID != "" {
		h["X-User-Id"] = cred.UID
	}
	if cred.Domain != "" {
		h["X-Domain"] = cred.Domain
	}
	return h
}

// VerifyCreds 调 /v2/plugin/auth/state 验证 accessToken（code:0 = 有效）
func (m *WorkBuddyCredManager) VerifyCreds(cred *WorkBuddyCred) (valid bool, statusCode int, err error) {
	url := cred.BaseURL(m.config.BaseURL) + "/plugin/auth/state?platform=" + cred.Platform()
	body, status, err := httpPost(url, "", WorkBuddyAuthHeaders(cred))
	if err != nil {
		return false, -1, err
	}
	if status != 200 {
		return false, status, nil
	}
	var resp struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return false, status, nil
	}
	return resp.Code == 0, status, nil
}

// WorkBuddyRefreshResult refresh 结果
type WorkBuddyRefreshResult struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    int64
}

// RefreshToken 用 refreshToken 续期 accessToken（内存缓存，不回写 info 文件）
func (m *WorkBuddyCredManager) RefreshToken(cred *WorkBuddyCred) (*WorkBuddyRefreshResult, error) {
	url := cred.BaseURL(m.config.BaseURL) + "/plugin/auth/token/refresh"
	headers := WorkBuddyAuthHeaders(cred)
	headers["X-Refresh-Token"] = cred.RefreshToken
	headers["X-Auth-Refresh-Source"] = "plugin"
	body, status, err := httpPost(url, "", headers)
	if err != nil {
		return nil, fmt.Errorf("refresh 请求失败: %v", err)
	}
	if status != 200 {
		return nil, fmt.Errorf("refresh 返回 status=%d", status)
	}
	var resp struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresAt    int64  `json:"expiresAt"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("refresh 响应解析失败: %v", err)
	}
	if resp.Code != 0 || resp.Data.AccessToken == "" {
		return nil, fmt.Errorf("refresh 失败: code=%d msg=%s", resp.Code, resp.Msg)
	}
	return &WorkBuddyRefreshResult{
		AccessToken:  resp.Data.AccessToken,
		RefreshToken: resp.Data.RefreshToken,
		ExpiresAt:    resp.Data.ExpiresAt,
	}, nil
}
