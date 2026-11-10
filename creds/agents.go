package creds

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"switchdev/paths"
)

// AgentType agent 工具类型，决定前端引导方式
const (
	AgentTypeGUI = "gui" // GUI 应用（如 JoyCode），引导方式是下载安装包
	AgentTypeCLI = "cli" // CLI 工具（如 DevEco/OpenCode），引导方式是一行安装命令
)

// AgentInfo agent 工具元数据（可配置注册表的一条）
// 加新 agent 工具只需往 AgentRegistry 追加一条，前端自动渲染对应引导卡片
type AgentInfo struct {
	Name        string // 显示名，如 "JoyCode"
	Upstream    string // 上游标识，如 "joycode"，与 CredStatusInfo 对应
	Type        string // AgentTypeGUI | AgentTypeCLI
	Desc        string // 一句话描述
	DownloadURL string // 官网/下载页
	InstallCmd  string // CLI 安装命令（GUI 类型为空）
	LoginCmd    string // 登录命令（GUI 类型为"打开客户端扫码"之类提示）
	LoginURL    string // 浏览器登录页 URL
	// ExecNames CLI 可执行文件名（跨平台探测安装用）；对 CLI 工具，
	// 除凭据文件外，还在 PATH 和 npm 全局 bin 目录查找这些可执行文件，
	// 任一命中即视为已安装（Windows 上 .cmd/.exe/.ps1 shim 都算）
	ExecNames []string
	// Hidden 为 true 时不在默认引导/凭据流程中显示（底层上游仍保留，供显式配置使用）。
	// OpenCode 本质是 API Key 配置，用户通过「供应商配置」接入即可，故默认隐藏。
	Hidden bool
}

// AgentRegistry 已支持的 agent 工具注册表
// 后续要加更多 agent 工具，往这里追加一条 AgentInfo 即可，无需改其他逻辑
var AgentRegistry = []AgentInfo{
	{
		Name:        "JoyCode",
		Upstream:    "joycode",
		Type:        AgentTypeGUI,
		Desc:        "京东云 AI 编程工具（VS Code 套壳），pt_key 登录态存于 state.vscdb",
		DownloadURL: "https://joycode.jd.com",
		InstallCmd:  "", // GUI 应用，无 CLI 安装命令
		LoginCmd:    "打开 JoyCode 客户端扫码登录",
		LoginURL:    "https://joycode.jd.com/portal/login",
	},
	{
		Name:        "DevEco Code",
		Upstream:    "deveco",
		Type:        AgentTypeCLI,
		Desc:        "华为 DevEco Code（基于 OpenCode），OAuth 三层加密存本地",
		DownloadURL: "https://cn.devecostudio.huawei.com",
		InstallCmd:  "npm i -g @deveco/deveco-code",
		LoginCmd:    "deveco auth login",
		LoginURL:    "https://cn.devecostudio.huawei.com",
		ExecNames:   []string{"deveco", "deveco-code"},
	},
	{
		Name:        "OpenCode Zen",
		Upstream:    "opencode",
		Type:        AgentTypeCLI,
		Desc:        "OpenCode 开源 CLI 的免费模型通道，静态 apiKey 明文存本地",
		DownloadURL: "https://opencode.ai",
		InstallCmd:  "npm i -g opencode-ai",
		LoginCmd:    "opencode auth login",
		LoginURL:    "https://opencode.ai",
		ExecNames:   []string{"opencode"},
		Hidden:      true, // 本质是 API Key，统一在「供应商配置」里接入
	},
	{
		Name:        "WorkBuddy",
		Upstream:    "workbuddy",
		Type:        AgentTypeGUI,
		Desc:        "腾讯 CodeBuddy 桌面版，OAuth token 明文存本地，免费模型通道",
		DownloadURL: "https://workbuddy.ai",
		InstallCmd:  "", // GUI 应用，无 CLI 安装命令
		LoginCmd:    "打开 WorkBuddy 客户端登录",
		LoginURL:    "https://copilot.tencent.com/login?platform=workbuddy",
	},
	{
		Name:        "中国移动云盘",
		Upstream:    "mcloud",
		Type:        AgentTypeGUI,
		Desc:        "移动云盘 AI 工作台（灵犀），RCS token 明文存 accountdb.db，Basic 头本地拼装",
		DownloadURL: "https://yun.139.com",
		InstallCmd:  "", // GUI 应用，无 CLI 安装命令
		LoginCmd:    "打开中国移动云盘 App 登录（RCS token 30 天滚动自动续期）",
		LoginURL:    "https://yun.139.com",
	},
}

// FindAgent 按 upstream 名查 agent 元数据，找不到返回 nil
func FindAgent(upstream string) *AgentInfo {
	for i := range AgentRegistry {
		if AgentRegistry[i].Upstream == upstream {
			return &AgentRegistry[i]
		}
	}
	return nil
}

// IsAgentInstalled 探测 agent 是否已安装（任一信号命中即视为已装）：
//  1. 跨平台凭据文件路径（paths 包，与 EnsureCreds 加载同源）
//  2. CLI 可执行文件：PATH 中查找，以及 npm 全局 bin 目录（Windows: %APPDATA%\npm）
func IsAgentInstalled(agent *AgentInfo) bool {
	if agent == nil {
		return false
	}
	// 1. 跨平台候选路径（paths 包，与 EnsureCreds 加载用的是同一套）
	for _, p := range probeCandidates(agent.Upstream) {
		if p == "" {
			continue
		}
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	// 2. CLI 可执行文件探测（PATH + npm 全局 bin）
	if len(agent.ExecNames) > 0 && isExecInstalled(agent.ExecNames) {
		return true
	}
	return false
}

// isExecInstalled 在 PATH 和 npm 全局 bin 目录中查找任一可执行文件。
// Windows 上 exec.LookPath 会按 PATHEXT 匹配 .cmd/.exe/.ps1 等；
// 另显式检查 npm 全局 bin（%APPDATA%\npm），覆盖该目录未进 PATH 的情况。
func isExecInstalled(names []string) bool {
	for _, name := range names {
		// PATH 查找
		if _, err := exec.LookPath(name); err == nil {
			return true
		}
	}
	// npm 全局 bin 目录（Windows: %APPDATA%\npm）
	npmBin := paths.NpmGlobalBinDir()
	if npmBin == "" {
		return false
	}
	for _, name := range names {
		if exeExistsInDir(npmBin, name) {
			return true
		}
	}
	return false
}

// exeExistsInDir 在目录 dir 里查找名为 name 的可执行文件。
// Windows 下尝试常见 shim 后缀（.cmd/.exe/.ps1/.bat）；其他平台直接查同名文件可执行位。
func exeExistsInDir(dir, name string) bool {
	if runtime.GOOS == "windows" {
		for _, ext := range []string{".cmd", ".exe", ".ps1", ".bat"} {
			if info, err := os.Stat(filepath.Join(dir, name+ext)); err == nil && !info.IsDir() {
				return true
			}
		}
		return false
	}
	info, err := os.Stat(filepath.Join(dir, name))
	if err != nil || info.IsDir() {
		return false
	}
	return info.Mode()&0111 != 0
}

// probeCandidates 按 upstream 返回跨平台凭据候选路径（委托 paths 包，单一真相）
func probeCandidates(upstream string) []string {
	switch upstream {
	case "joycode":
		return paths.JoyCodeVscdbCandidates()
	case "workbuddy":
		return paths.WorkBuddyInfoCandidates()
	case "deveco":
		return paths.DevEcoAuthCandidates()
	case "opencode":
		return paths.OpenCodeAuthCandidates()
	case "mcloud":
		// Windows 客户端不写 accountdb.db，登录态在 localStorage leveldb；
		// 探测该目录即视为已安装（是否已登录由凭据加载另行判定）。
		if runtime.GOOS == "windows" {
			return paths.MCLocalStorageLevelDBCandidates()
		}
		return paths.MCAccountdbCandidates()
	}
	return nil
}

// FillAgentMeta 把 AgentRegistry 里对应 upstream 的安装/登录元数据注入 CredStatusInfo
// 三态判定：Installed 由 IsAgentInstalled 探测；元数据始终注入（前端按状态渲染）
func FillAgentMeta(info *CredStatusInfo, upstream string) {
	agent := FindAgent(upstream)
	if agent == nil {
		return
	}
	info.Installed = IsAgentInstalled(agent)
	info.AgentType = agent.Type
	info.InstallCmd = agent.InstallCmd
	info.LoginCmd = agent.LoginCmd
	info.DownloadURL = agent.DownloadURL
	info.LoginURL = agent.LoginURL
}
