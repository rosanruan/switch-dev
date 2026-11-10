package creds

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"switchdev/paths"
)

// OpenCode 配置
type OpenCodeConfig struct {
	BaseURL          string
	AuthPath         string
	VerifyIntervalMs int
}

func DefaultOpenCodeConfig() OpenCodeConfig {
	return OpenCodeConfig{
		BaseURL:          "https://opencode.ai/zen/v1",
		AuthPath:         paths.Resolve("OPENCODE_AUTH_PATH", paths.OpenCodeAuthCandidates()),
		VerifyIntervalMs: 600000,
	}
}

// OpenCodeCred 运行时凭据
type OpenCodeCred struct {
	APIKey    string
	FetchedAt time.Time
	Valid     bool
}

// OpenCodeCredManager OpenCode 凭据管理器
type OpenCodeCredManager struct {
	mu     sync.RWMutex
	cred   *OpenCodeCred
	config OpenCodeConfig
}

func NewOpenCodeCredManager(config OpenCodeConfig) *OpenCodeCredManager {
	return &OpenCodeCredManager{config: config}
}

// Config 返回当前配置
func (m *OpenCodeCredManager) Config() OpenCodeConfig {
	return m.config
}

// LoadCreds 从 auth.json 读 opencode.key（明文）
func (m *OpenCodeCredManager) LoadCreds() (*OpenCodeCred, error) {
	if _, err := os.Stat(m.config.AuthPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("找不到 OpenCode auth.json: %s\n请先安装 OpenCode 并运行 opencode auth login 登录", m.config.AuthPath)
	}

	data, err := os.ReadFile(m.config.AuthPath)
	if err != nil {
		return nil, fmt.Errorf("读取 auth.json 失败: %v", err)
	}

	var auth struct {
		OpenCode struct {
			Type string `json:"type"`
			Key  string `json:"key"`
		} `json:"opencode"`
	}
	if err := json.Unmarshal(data, &auth); err != nil {
		return nil, fmt.Errorf("解析 auth.json 失败: %v", err)
	}

	if auth.OpenCode.Type != "api" || auth.OpenCode.Key == "" {
		return nil, fmt.Errorf("auth.json 里无有效的 opencode api key，请运行 opencode auth login 登录 OpenCode Zen")
	}

	return &OpenCodeCred{
		APIKey:    auth.OpenCode.Key,
		FetchedAt: time.Now(),
		Valid:     false,
	}, nil
}

// EnsureCreds 确保有有效凭据
func (m *OpenCodeCredManager) EnsureCreds() (*OpenCodeCred, error) {
	m.mu.RLock()
	if m.cred != nil && m.cred.Valid {
		cred := *m.cred
		m.mu.RUnlock()
		return &cred, nil
	}
	m.mu.RUnlock()

	fresh, err := m.LoadCreds()
	if err != nil {
		return nil, err
	}

	// 预校验
	valid, status, err := m.VerifyCreds(fresh)
	if err != nil {
		m.mu.Lock()
		m.cred = fresh
		m.mu.Unlock()
		return nil, fmt.Errorf("OpenCode 预校验请求失败: %v", err)
	}
	fresh.Valid = valid

	if !valid {
		m.mu.Lock()
		m.cred = fresh
		m.mu.Unlock()
		return nil, NewCredentialsError("OpenCode apiKey 校验失败（GET /models 返回 status=%d）。请运行 opencode auth login 重新登录 OpenCode Zen", status)
	}

	m.mu.Lock()
	m.cred = fresh
	m.mu.Unlock()
	return fresh, nil
}

// InvalidateCreds 标记凭据失效
func (m *OpenCodeCredManager) InvalidateCreds() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cred != nil {
		m.cred.Valid = false
	}
}

// GetCred 获取当前凭据
func (m *OpenCodeCredManager) GetCred() *OpenCodeCred {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.cred == nil {
		return nil
	}
	cred := *m.cred
	return &cred
}

// CredStatus 返回凭据状态信息
func (m *OpenCodeCredManager) CredStatus() *CredStatusInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()

	info := &CredStatusInfo{
		Source: "auth.json (明文)",
	}

	if m.cred != nil {
		info.Valid = m.cred.Valid
		info.LastCheck = m.cred.FetchedAt.Format(time.RFC3339)
		if len(m.cred.APIKey) > 8 {
			info.KeyPreview = m.cred.APIKey[:8] + "..."
		}
	} else if _, err := os.Stat(m.config.AuthPath); os.IsNotExist(err) {
		info.Source = "NOT_FOUND"
	}

	// 注入 agent 安装/登录元数据 + Installed 探测
	FillAgentMeta(info, "opencode")
	return info
}

// VerifyCreds 调 GET /models 验证 apiKey 有效性
func (m *OpenCodeCredManager) VerifyCreds(cred *OpenCodeCred) (valid bool, statusCode int, err error) {
	url := m.config.BaseURL + "/models"

	body, status, err := httpGet(url, map[string]string{
		"Authorization": fmt.Sprintf("Bearer %s", cred.APIKey),
		"Accept":        "application/json",
	})
	if err != nil {
		return false, -1, err
	}

	if status != 200 {
		return false, status, nil
	}

	var resp struct {
		Data interface{} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return false, status, nil
	}

	return resp.Data != nil, status, nil
}
