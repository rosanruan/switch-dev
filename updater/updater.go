package updater

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/minio/selfupdate"

	"switchdev/config"
	"switchdev/version"
)

// Updater 自动升级管理器
type Updater struct {
	cfg        *config.Config
	currentVer string
}

// NewUpdater 创建升级管理器
func NewUpdater(cfg *config.Config) *Updater {
	return &Updater{
		cfg:        cfg,
		currentVer: version.GetVersion(),
	}
}

// SetVersion 覆盖当前版本（测试用）
func (u *Updater) SetVersion(v string) { u.currentVer = v }

// GetCurrentVersion 当前版本号
func (u *Updater) GetCurrentVersion() string { return u.currentVer }

// CheckUpdate 检查是否有新版本（不下载）
func (u *Updater) CheckUpdate(ctx context.Context) (*UpdateInfo, error) {
	uc := &u.cfg.AutoUpdate
	// 兼容旧配置：从未显式设置过 update 字段（Enabled=false 且 Provider=""），
	// 使用默认设置而非静默跳过
	if !uc.Enabled && uc.Provider == "" {
		uc.Enabled = true
		uc.Provider = "github"
		if uc.GitHub.Owner == "" {
			uc.GitHub.Owner = "rosanruan"
		}
		if uc.GitHub.Repo == "" {
			uc.GitHub.Repo = "switch-dev"
		}
	}
	if !uc.Enabled {
		return nil, nil
	}
	// 自定义 URL 优先
	var info *UpdateInfo
	var err error
	if uc.UpdateURL != "" {
		info, err = checkCustomURL(ctx, uc.UpdateURL, u.currentVer)
	} else if uc.Provider == "github" || uc.Provider == "" {
		if uc.GitHub.Owner == "" || uc.GitHub.Repo == "" {
			return nil, nil
		}
		info, err = CheckGitHubRelease(uc.GitHub, u.currentVer)
	}
	if err != nil {
		return nil, err
	}
	// 用户跳过的版本不提示
	if info != nil && info.Version == u.cfg.AutoUpdate.SkippedVersion {
		return nil, nil
	}
	return info, nil
}

// SetSkippedVersion 记录用户跳过的版本号，写入配置并持久化
func (u *Updater) SetSkippedVersion(v string) error {
	u.cfg.SetSkippedUpdateVersion(v)
	return u.cfg.Save()
}

// ApplyUpdate 下载并应用更新（原子替换当前二进制）
func (u *Updater) ApplyUpdate(ctx context.Context, info *UpdateInfo, progress func(UpdateStatus)) error {
	if info == nil {
		return fmt.Errorf("无更新信息")
	}
	// B1: 下载前先探测当前 exe 所在目录是否可写。自我更新是同目录原子替换
	// （selfupdate 写 <exe>.new 再 rename），若装在 Program Files 等无写权限目录，
	// 会在下载 100% 后才报 raw Access denied。提前失败并给出可操作指引。
	if err := ensureInstallDirWritable(); err != nil {
		if progress != nil {
			progress(UpdateStatus{State: "error", Message: err.Error()})
		}
		return err
	}
	if progress != nil {
		progress(UpdateStatus{State: "downloading", Message: "开始下载"})
	}
	// 下载到临时文件
	tmpFile, err := downloadAsset(info.AssetURL, info.AssetSize, progress)
	if err != nil {
		if progress != nil {
			progress(UpdateStatus{State: "error", Message: err.Error()})
		}
		return err
	}
	// 应用完（或失败）后清理临时下载文件
	defer os.Remove(tmpFile)

	if progress != nil {
		progress(UpdateStatus{State: "applying", Message: "应用更新"})
	}
	// selfupdate 原子替换当前运行中的二进制
	file, err := openFile(tmpFile)
	if err != nil {
		if progress != nil {
			progress(UpdateStatus{State: "error", Message: err.Error()})
		}
		return err
	}
	defer file.Close()
	err = selfupdate.Apply(file, selfupdate.Options{})
	if err != nil {
		// B2: 权限类错误归类为可操作指引，而非 raw OS 错误
		err = classifyPermissionError(err)
		if progress != nil {
			progress(UpdateStatus{State: "error", Message: err.Error()})
		}
		return err
	}
	if progress != nil {
		progress(UpdateStatus{State: "done", Message: "更新完成，请重启应用"})
	}
	return nil
}

// checkCustomURL 自定义检查地址（返回 JSON：{version, notes, assetUrl, assetSize}）
func checkCustomURL(ctx context.Context, url, currentVer string) (*UpdateInfo, error) {
	body, err := fetchBytes(ctx, url)
	if err != nil {
		return nil, err
	}
	info := &UpdateInfo{}
	if err := json.Unmarshal(body, info); err != nil {
		return nil, err
	}
	if info.Version == "" {
		return nil, nil
	}
	if versionCompare(trimV(info.Version), currentVer) <= 0 {
		return nil, nil
	}
	// 若响应未显式指定 critical，按版本号段变化判定（minor 变化=强制）
	if !info.Critical {
		cur := parseVersion(currentVer)
		nw := parseVersion(trimV(info.Version))
		info.Critical = nw[0] != cur[0] || nw[1] != cur[1]
	}
	return info, nil
}

// fetchBytes GET 请求返回 body
func fetchBytes(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// openFile 打开文件
func openFile(path string) (*os.File, error) {
	return os.Open(path)
}

// ensureInstallDirWritable 探测当前可执行文件所在目录是否可写。
// 自我更新需在同目录写临时文件再原子替换；不可写（典型：旧版装在 Program Files）
// 时提前返回带指引的错误，避免下载完成后才失败。
func ensureInstallDirWritable() error {
	exe, err := os.Executable()
	if err != nil {
		return nil // 拿不到路径就不预检，交给后续流程
	}
	// macOS .app 包内 / 开发环境（go run 临时目录）跳过预检：
	// .app 更新走 DMG/安装器而非自替换；go run 二进制在系统临时目录，无参考意义
	resolved, rerr := filepath.EvalSymlinks(exe)
	if rerr == nil {
		exe = resolved
	}
	if strings.Contains(exe, ".app/Contents/MacOS") {
		return nil
	}
	dir := filepath.Dir(exe)
	probe, err := os.CreateTemp(dir, ".switch-dev-writable-*")
	if err != nil {
		return permissionDeniedError(dir, err)
	}
	name := probe.Name()
	probe.Close()
	os.Remove(name)
	return nil
}

// permissionDeniedError 构造权限不足的可操作指引错误
func permissionDeniedError(dir string, cause error) error {
	return fmt.Errorf(
		"当前安装目录无写权限，无法完成自我更新：%s\n\n"+
			"这通常是因为本应用是以管理员身份安装到 Program Files 的。请改用最新安装器重装到用户目录：\n"+
			"1. 托盘右键退出 Switch Dev\n"+
			"2. 到「应用和功能」卸载旧版 Switch Dev\n"+
			"3. 下载并运行最新安装器（按用户安装，无需管理员权限）：https://github.com/rosanruan/switch-dev/releases/latest\n\n"+
			"（原始错误：%v）",
		dir, cause,
	)
}

// classifyPermissionError 把 selfupdate 的权限类错误归类为可操作指引；
// 其它错误原样返回。
func classifyPermissionError(err error) error {
	if err == nil {
		return nil
	}
	// 覆盖常见权限错误：errno 级 + 文案匹配（Windows "Access is denied"）
	if errors.Is(err, os.ErrPermission) {
		exe, _ := os.Executable()
		return permissionDeniedError(filepath.Dir(exe), err)
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "access is denied") ||
		strings.Contains(msg, "permission denied") ||
		strings.Contains(msg, "access denied") {
		exe, _ := os.Executable()
		return permissionDeniedError(filepath.Dir(exe), err)
	}
	return err
}
