package creds

import "fmt"

// CredStatusInfo 凭据状态信息（供 /health 端点和前端展示用）
type CredStatusInfo struct {
	Valid      bool   `json:"valid"`
	Installed  bool   `json:"installed"` // ★ 工具是否装了（凭据文件存在）
	Enabled    bool   `json:"enabled"`   // ★ 上游是否启用（禁用则调用时跳过其下所有模型）
	UserID     string `json:"userId,omitempty"`
	ExpiresAt  string `json:"expiresAt,omitempty"`
	KeyPreview string `json:"keyPreview,omitempty"`
	Source     string `json:"source"`
	LastCheck  string `json:"lastCheck"`

	// ★ 安装/登录引导元数据（从 AgentRegistry 注入）
	AgentType   string `json:"agentType,omitempty"`   // "gui" | "cli"
	InstallCmd  string `json:"installCmd,omitempty"`  // CLI 安装命令
	LoginCmd    string `json:"loginCmd,omitempty"`    // 登录命令
	DownloadURL string `json:"downloadUrl,omitempty"` // 官网/下载页
	LoginURL    string `json:"loginUrl,omitempty"`    // 浏览器登录页
}

// CredentialsError 凭据错误（无效/过期/需重登），与普通网络错误区分
type CredentialsError struct {
	Msg string
}

func (e *CredentialsError) Error() string { return e.Msg }

// NewCredentialsError 构造凭据错误
func NewCredentialsError(format string, args ...interface{}) *CredentialsError {
	return &CredentialsError{Msg: fmt.Sprintf(format, args...)}
}
