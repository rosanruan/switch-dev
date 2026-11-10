// Package autostart 跨平台管理「登录时自动启动」。
// 纯 Go 实现：Windows 写注册表 HKCU\...\Run，macOS 写 LaunchAgent plist，
// Linux 写 ~/.config/autostart/*.desktop。不依赖 cgo，支持交叉编译。
package autostart

import (
	"os"
	"path/filepath"
)

// AppID 是本应用自启项的稳定唯一标识（注册表值名 / plist 文件名 / desktop 文件名）。
const AppID = "com.switchdev.app"
const AppName = "Switch Dev"

// App 描述一个要在用户登录时自动启动的应用。
type App struct {
	// Name 唯一标识，同时用作注册表值名 / plist 文件名 / desktop 文件名（建议反向域名，如 com.switchdev.app）。
	Name string
	// Exec 启动命令及参数，例如 ["/Applications/Switch Dev.app/.../switch-dev", "--tray"]。
	Exec []string
	// DisplayName 菜单/desktop 中显示的名称（可空，回退 Name）。
	DisplayName string
	// Icon desktop 文件用的图标路径（仅 Linux；可空）。
	Icon string
}

// CurrentApp 返回本程序对应的自启 App：以当前可执行文件路径 + --tray 启动（静默到托盘）。
func CurrentApp() *App {
	exe, err := os.Executable()
	if err != nil {
		exe = os.Args[0]
	}
	// macOS 下从 .app 内运行时 os.Executable 已指向包内二进制，可直接执行
	if abs, err := filepath.EvalSymlinks(exe); err == nil {
		exe = abs
	}
	return &App{
		Name:        AppID,
		DisplayName: AppName,
		Exec:        []string{exe, "--tray"},
	}
}
