package service

import (
	"context"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"

	"switchdev/updater"
)

// UpdaterService 自动升级服务（暴露给前端）
type UpdaterService struct {
	updater *updater.Updater
}

// onRestartFn 更新成功后重启应用的处理函数（由 main 通过 SetRestartHandler 注入，
// 不暴露给前端——包级函数不会被 Wails 当作 service 方法绑定）。
var onRestartFn func()

// SetRestartHandler 注入更新完成后的重启回调（main 调用）。
func SetRestartHandler(fn func()) {
	onRestartFn = fn
}

func NewUpdaterService(up *updater.Updater) *UpdaterService {
	return &UpdaterService{updater: up}
}

// GetCurrentVersion 当前版本号
func (s *UpdaterService) GetCurrentVersion() string {
	return s.updater.GetCurrentVersion()
}

// CheckUpdate 检查是否有新版本
func (s *UpdaterService) CheckUpdate() (*updater.UpdateInfo, error) {
	return s.updater.CheckUpdate(context.Background())
}

// SetSkippedVersion 记录用户跳过的版本号，该版本不再提示
func (s *UpdaterService) SetSkippedVersion(version string) error {
	return s.updater.SetSkippedVersion(version)
}

// ApplyUpdate 下载并应用更新（阻塞，进度通过事件推送）。
// 二进制替换成功后：先推 done 事件让前端收尾，再由 main 注入的回调重启应用
// （释放端口 -> 启动新进程 -> 退出旧进程）。
func (s *UpdaterService) ApplyUpdate(info *updater.UpdateInfo) error {
	progress := func(status updater.UpdateStatus) {
		s.emitProgress(status)
	}
	if err := s.updater.ApplyUpdate(context.Background(), info, progress); err != nil {
		return err
	}
	// 通知前端即将重启（前端可显示"正在重启…"并停止交互）
	s.emitProgress(updater.UpdateStatus{
		State:   "restarting",
		Percent: 100,
		Message: "更新完成，正在重启…",
	})
	// 延迟一点让事件送达前端，再执行重启
	if onRestartFn != nil {
		go func() {
			time.Sleep(800 * time.Millisecond)
			onRestartFn()
		}()
	}
	return nil
}

// EmitUpdateAvailable 推送有可用更新事件（供 main 启动检查用）
func (s *UpdaterService) EmitUpdateAvailable(info *updater.UpdateInfo) {
	s.emit("update:available", info)
}

// emitProgress 推送更新进度事件
func (s *UpdaterService) emitProgress(status updater.UpdateStatus) {
	s.emit("update:progress", status)
}

// emit 推送事件
func (s *UpdaterService) emit(event string, data interface{}) {
	if app := application.Get(); app != nil {
		app.Event.Emit(event, data)
	}
}
