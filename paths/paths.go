package paths

import (
	"os"
	"path/filepath"
	"runtime"
)

// AppConfigDir switch-dev 自身的配置/数据目录（配置、日志、费率等）
//
//	darwin/linux: ~/.config/switch-dev
//	windows: %APPDATA%\switch-dev
func AppConfigDir() string {
	if runtime.GOOS == "windows" {
		if dir := os.Getenv("APPDATA"); dir != "" {
			return filepath.Join(dir, "switch-dev")
		}
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "switch-dev")
}

// AppSupportDir GUI/VSCode 系工具（JoyCode、WorkBuddy）的数据根目录
//
//	darwin: ~/Library/Application Support
//	linux:  ~/.config
//	windows: %APPDATA%
func AppSupportDir() string {
	if runtime.GOOS == "windows" {
		return os.Getenv("APPDATA")
	}
	home, _ := os.UserHomeDir()
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support")
	}
	return filepath.Join(home, ".config")
}

// XDGConfigDir CLI 工具配置目录（XDG_CONFIG_HOME）
//
//	darwin/linux: ~/.config
//	windows: %APPDATA%（Windows 无 XDG 约定，回退）
func XDGConfigDir() string {
	if runtime.GOOS == "windows" {
		return os.Getenv("APPDATA")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config")
}

// XDGDataDir CLI 工具数据目录（XDG_DATA_HOME）
//
//	darwin/linux: ~/.local/share
//	windows: %APPDATA%
func XDGDataDir() string {
	if runtime.GOOS == "windows" {
		return os.Getenv("APPDATA")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share")
}

// XDGStateDir CLI 工具状态目录（XDG_STATE_HOME）
//
//	darwin/linux: ~/.local/state
//	windows: %LOCALAPPDATA%
func XDGStateDir() string {
	if runtime.GOOS == "windows" {
		return os.Getenv("LOCALAPPDATA")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state")
}

// 以下 LiteralXDG* 系列返回「跨平台一致的字面 XDG 路径」（~/.config、~/.local/share、
// ~/.local/state），用于基于 Node.js 的 CLI 工具（DevEco/OpenCode）。这类工具在所有平台
// （含 Windows）都直接用字面路径，不像原生 Windows 程序那样落到 %APPDATA%/%LOCALAPPDATA%。
// 不能用上面的 XDG* 系列，因为那些在 Windows 上会映射到 %APPDATA%，导致找不到文件。

// LiteralConfigDir 字面 ~/.config（所有平台一致）
func LiteralConfigDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config")
}

// LiteralDataDir 字面 ~/.local/share（所有平台一致）
func LiteralDataDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share")
}

// LiteralStateDir 字面 ~/.local/state（所有平台一致）
func LiteralStateDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state")
}

// EnvOr 环境变量覆盖：env 非空则返回 env，否则返回 def
// 作为 Windows 路径不准时的逃生口（如 JOYCODE_VSCDB）
func EnvOr(env, def string) string {
	if v := os.Getenv(env); v != "" {
		return v
	}
	return def
}

// FirstExists 返回第一个已存在的路径；都不存在则返回第一个非空候选作为默认
func FirstExists(candidates []string) string {
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	for _, c := range candidates {
		if c != "" {
			return c
		}
	}
	return ""
}

// Resolve 路径解析（启动时自动探测）：
//
//	envKey 非空 -> 强制用环境变量值（不探测，用户明确指定）
//	否则遍历 candidates，返回第一个已存在的
//	都不存在 -> 返回第一个候选作默认（加载时会报"找不到"）
func Resolve(envKey string, candidates []string) string {
	if e := os.Getenv(envKey); e != "" {
		return e
	}
	return FirstExists(candidates)
}

// JoyCodeVscdbCandidates JoyCode state.vscdb 跨平台候选路径
func JoyCodeVscdbCandidates() []string {
	home, _ := os.UserHomeDir()
	rel := filepath.Join("JoyCode", "User", "globalStorage", "state.vscdb")
	c := []string{
		filepath.Join(home, "Library", "Application Support", rel), // macOS
		filepath.Join(home, ".config", rel),                        // Linux
	}
	if appdata := os.Getenv("APPDATA"); appdata != "" {
		c = append(c, filepath.Join(appdata, rel)) // Windows
	}
	return c
}

// JoyCodeExtPkgCandidates JoyCode 扩展 package.json 跨平台候选路径
//
// clientVersion 必须与官方客户端一致：官方扩展在请求里注入的是**扩展自身 package.json
// 的 version**（getVersion()），而不是 state.vscdb 的 releaseNotes/lastVersion（后者是
// 应用壳版本，可能远旧于扩展版本）。网关灰度按 clientVersion 放行，发旧版本会被拒
// （AI_GRAY_ACCESS_DENIED）。因此我们从扩展安装目录读取真实版本。
func JoyCodeExtPkgCandidates() []string {
	home, _ := os.UserHomeDir()
	rel := filepath.Join("Resources", "app", "extensions", "joycoder-editor", "package.json")
	c := []string{
		filepath.Join("/Applications", "JoyCode.app", "Contents", rel),      // macOS 全局安装
		filepath.Join(home, "Applications", "JoyCode.app", "Contents", rel), // macOS 用户级安装
	}
	if local := os.Getenv("LOCALAPPDATA"); local != "" {
		// Windows 用户级安装（Programs\JoyCode\resources\app\...）
		c = append(c, filepath.Join(local, "Programs", "JoyCode", "resources", "app", "extensions", "joycoder-editor", "package.json"))
	}
	return c
}

// WorkBuddyInfoCandidates WorkBuddy 凭据跨平台候选路径
// WorkBuddy 是腾讯 CodeBuddy 桌面版（Electron）：
//   - macOS: ~/Library/Application Support/CodeBuddyExtension/.../workbuddy-desktop.info
//   - Windows: %LOCALAPPDATA%\CodeBuddyExtension\...\workbuddy-desktop-ai.info
//     （目录结构相同，但 Windows 文件名带 -ai 后缀，且数据根在 LOCALAPPDATA 而非 APPDATA）
//
// 为兼容不同版本/平台，同时探测两个文件名变体与多个目录名/数据根。
func WorkBuddyInfoCandidates() []string {
	home, _ := os.UserHomeDir()
	// macOS/Linux：workbuddy-desktop.info
	rel := filepath.Join("CodeBuddyExtension", "Data", "Public", "auth", "workbuddy-desktop.info")
	c := []string{
		filepath.Join(home, "Library", "Application Support", rel), // macOS
		filepath.Join(home, ".config", rel),                        // Linux
	}
	// Windows：APPDATA / LOCALAPPDATA 下，探测目录名 × 文件名变体
	authDir := filepath.Join("Data", "Public", "auth")
	fileVariants := []string{"workbuddy-desktop-ai.info", "workbuddy-desktop.info"}
	dirVariants := []string{"CodeBuddyExtension", "CodeBuddy", "WorkBuddyAI", "WorkBuddy"}
	for _, base := range windowsConfigDirs() {
		for _, dir := range dirVariants {
			for _, fname := range fileVariants {
				c = append(c, filepath.Join(base, dir, authDir, fname))
			}
		}
	}
	return c
}

// OpenCodeAuthCandidates OpenCode auth.json 跨平台候选路径
func OpenCodeAuthCandidates() []string {
	home, _ := os.UserHomeDir()
	c := []string{
		filepath.Join(home, ".local", "share", "opencode", "auth.json"), // macOS/Linux XDG
		filepath.Join(home, ".config", "opencode", "auth.json"),         // 部分发行版变体
	}
	// Windows：APPDATA / LOCALAPPDATA 下的 opencode 目录
	for _, base := range windowsConfigDirs() {
		c = append(c, filepath.Join(base, "opencode", "auth.json"))
	}
	return c
}

// DevEcoAuthCandidates DevEco auth.json 跨平台候选路径
// KEKDir/KVPath 仍用 XDG 目录（与 AuthPath 平台一致）
func DevEcoAuthCandidates() []string {
	home, _ := os.UserHomeDir()
	c := []string{
		filepath.Join(home, ".local", "share", "deveco", "auth.json"), // macOS/Linux XDG
	}
	// Windows：APPDATA / LOCALAPPDATA 下的 deveco 目录
	for _, base := range windowsConfigDirs() {
		c = append(c, filepath.Join(base, "deveco", "auth.json"))
	}
	return c
}

// MCAccountdbCandidates 中国移动云盘 accountdb.db 跨平台候选路径
// （RCS token 明文存 t_account 表，30 天滚动由 App 登录自动续）
// 注意：仅 macOS 版客户端使用 accountdb.db；Windows 版不写此文件，
// 登录态存于 Chromium localStorage（见 MCLocalStorageLevelDBCandidates）。
func MCAccountdbCandidates() []string {
	home, _ := os.UserHomeDir()
	c := []string{
		filepath.Join(home, "Library", "Application Support", "mCloud", "accountdb.db"), // macOS
	}
	// Windows：保留作逃生口（用户可用 MCLOUD_ACCOUNTDB 显式指定）；实际 Windows 走 localStorage
	for _, base := range windowsConfigDirs() {
		c = append(c, filepath.Join(base, "mCloud", "accountdb.db"))
	}
	return c
}

// MCLocalStorageLevelDBCandidates Windows 版移动云盘（Electron）的 localStorage LevelDB 目录。
// Windows 客户端不写 accountdb.db，登录态（含 RCS token、手机号、userDomainId）
// 以 JSON 存于 Chromium localStorage 的 `UserInfo` 键（origin=file://，value 为 UTF-8）。
// 返回的是 leveldb 目录（非单文件）；客户端运行时该目录被锁，读取方需复制副本后打开。
func MCLocalStorageLevelDBCandidates() []string {
	var c []string
	for _, base := range windowsConfigDirs() {
		c = append(c, filepath.Join(base, "mCloud", "Local Storage", "leveldb"))
	}
	return c
}

// windowsConfigDirs 返回 Windows 上应用数据目录候选（APPDATA + LOCALAPPDATA）；
// 非 Windows 平台返回 nil（调用方据此跳过）
func windowsConfigDirs() []string {
	if runtime.GOOS != "windows" {
		return nil
	}
	var dirs []string
	if appdata := os.Getenv("APPDATA"); appdata != "" {
		dirs = append(dirs, appdata)
	}
	if local := os.Getenv("LOCALAPPDATA"); local != "" && local != os.Getenv("APPDATA") {
		dirs = append(dirs, local)
	}
	return dirs
}

// ProviderAPIConfigPath 返回供应商凭据配置文件路径（credentials.json）。
func ProviderAPIConfigPath() string {
	return filepath.Join(AppConfigDir(), "credentials.json")
}

// ProviderCatalogCachePath 供应商目录本地缓存路径（GitHub 拉取成功后写入）
func ProviderCatalogCachePath() string {
	return filepath.Join(AppConfigDir(), "provider_catalog_cache.json")
}

// NpmGlobalBinDir 返回 npm 全局 bin 目录：
//
//	Windows: %APPDATA%\npm（npm i -g 安装的 .cmd/.exe shim 都在这里）
//	其他平台: ~/.npm-global/bin 及常见前缀（实际更可靠的是 exec.LookPath 查 PATH）
//
// 主要用于 Windows 下探测通过 npm 全局安装的 CLI（opencode/deveco），
// 即使该目录未进 PATH 也能定位到 shim。
func NpmGlobalBinDir() string {
	if runtime.GOOS == "windows" {
		if appdata := os.Getenv("APPDATA"); appdata != "" {
			return filepath.Join(appdata, "npm")
		}
	}
	home, _ := os.UserHomeDir()
	if home != "" {
		return filepath.Join(home, ".npm-global", "bin")
	}
	return ""
}
