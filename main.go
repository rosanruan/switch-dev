package main

import (
	"embed"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"

	"switchdev/config"
	"switchdev/creds"
	"switchdev/db"
	"switchdev/logfile"
	"switchdev/paths"
	"switchdev/pricing"
	"switchdev/providerapi"
	"switchdev/proxy"
	"switchdev/service"
	"switchdev/updater"
	"switchdev/upstream"
	"switchdev/version"
)

// 免费 API 目录（内置，运行时从 GitHub 拉取最新覆盖）
//
//go:embed data/provider_apis_catalog.json
var embedProviderCatalog []byte

// 注册送 token 供应商列表（内置，运行时从 GitHub 拉取最新覆盖）
//
//go:embed data/bonus_providers.json
var embedBonusProviders []byte

// Wails 用 embed 包把前端文件嵌入二进制
//
//go:embed all:frontend/dist
var assets embed.FS

// 托盘图标：macOS 用黑白模板（自动适配深浅色菜单栏），其他平台用彩色
//
//go:embed build/tray-icon.png
var trayIconColor []byte

//go:embed build/tray-template.png
var trayIconTemplate []byte

// realQuit 标记：只有托盘「退出」菜单触发时为 true，允许真正退出；
// Dock 右键退出 / Cmd+Q 走 ShouldQuit 钩子时为 false，改为隐藏窗口
var realQuit atomic.Bool

// 全局窗口引用（供托盘菜单使用）
var mainWindow *application.WebviewWindow

// 全局托盘引用（供动态刷新菜单用）
var systemTray *application.SystemTray

func main() {
	// 自动更新重启：新进程带 --wait-for-pid=<oldpid> 启动时，先等旧进程完全退出
	// （释放单实例锁/端口），再继续初始化。必须放在所有副作用之前，避免与旧进程冲突。
	for _, a := range os.Args[1:] {
		if strings.HasPrefix(a, "--wait-for-pid=") {
			waitForProcessExit(a[len("--wait-for-pid="):], 15*time.Second)
		}
	}

	// 开机自启以 --tray 启动：窗口创建后即隐藏，静默驻留托盘。
	startHidden := false
	for _, a := range os.Args[1:] {
		if a == "--tray" {
			startHidden = true
		}
	}

	// 1. 凭据管理器
	jyMgr := creds.NewJoyCodeCredManager(creds.DefaultJoyCodeConfig())
	deMgr := creds.NewDevEcoCredManager(creds.DefaultDevEcoConfig())
	ocMgr := creds.NewOpenCodeCredManager(creds.DefaultOpenCodeConfig())
	wbMgr := creds.NewWorkBuddyCredManager(creds.DefaultWorkBuddyConfig())

	// 2. 上游适配器
	jyUp := upstream.NewJoyCodeUpstream(jyMgr)
	deUp := upstream.NewDevEcoUpstream(deMgr)
	ocUp := upstream.NewOpenCodeUpstream(ocMgr)
	wbUp := upstream.NewWorkBuddyUpstream(wbMgr)

	// 3. 配置管理器
	cfgMgr, err := config.NewManager("")
	if err != nil {
		log.Printf("⚠️ 配置加载失败: %v，使用默认配置", err)
	}

	// 3.5 文件日志：把控制台日志同时落地到 <AppConfigDir>/logs/YYYY-MM-DD.log，
	// 后台压缩旧日志。受 Config.LogFile.Enabled 控制，默认开启。
	logMgr := logfile.New(filepath.Join(paths.AppConfigDir(), "logs"))
	if err := logMgr.Start(cfgMgr.Get().LogFile.Enabled); err != nil {
		log.Printf("⚠️ 文件日志初始化失败: %v", err)
	}
	defer logMgr.Stop()

	// 4. 代理服务（端口从配置读取）
	serverPort := cfgMgr.Get().Port
	if serverPort == 0 {
		serverPort = config.DefaultPort
	}
	server := proxy.NewServer(jyUp, deUp, ocUp, wbUp, "127.0.0.1", serverPort)
	server.ConfigResolver = cfgMgr // 注入配置解析器

	// 4.5 费率管理器（本地无费率文件时从内置硬编码还原）
	pricingMgr := pricing.NewManager()
	if imported, err := pricingMgr.Load(); err != nil {
		log.Printf("⚠️ 费率库加载: %v", err)
	} else if imported {
		log.Printf("✓ 已从内置费率还原 %d 条到自有库", pricingMgr.Count())
	}
	server.Pricing = pricingMgr

	// 5. Core（共享状态）
	core := service.NewCore()
	core.Setup(jyMgr, deMgr, ocMgr, wbMgr, jyUp, deUp, ocUp, wbUp, server)
	core.SetConfigManager(cfgMgr)

	// 5.3 本地数据库（SQLite）
	dbPath := filepath.Join(paths.AppConfigDir(), "switch-dev.db")
	database, err := db.Open(dbPath)
	if err != nil {
		log.Printf("⚠️ 数据库打开失败，将回退到文件日志: %v", err)
	} else {
		core.SetDB(database)
		defer database.Close()
		// 建表后把历史 logs 一次性回填进 usage_stats（幂等，已回填则跳过），
		// 之后统计读聚合表，清空日志不影响统计。
		if err := database.BackfillUsageStats(); err != nil {
			log.Printf("⚠️ 用量统计回填失败: %v", err)
		}
	}

	// 5.5 免费 API（独立文件 + 目录 + 健康监控）
	providerAPIMgr, err := providerapi.NewManager("")
	if err != nil {
		log.Printf("⚠️ 免费 API 配置加载失败: %v", err)
	}
	providerapi.SetEmbedCatalog(embedProviderCatalog)
	providerapi.SetEmbedBonusProviders(embedBonusProviders)
	providerCatalogLoader := &providerapi.CatalogLoader{}
	_ = providerCatalogLoader.LoadCatalog() // 预加载（embed/GitHub/缓存）
	providerMonitor := providerapi.NewMonitor(providerAPIMgr, core)
	// 注入健康查询回调（供 proxy 权重降级用）
	proxy.ProviderModelHealth = func(providerID, modelID string) bool {
		return providerMonitor.IsHealthy(providerID, modelID)
	}
	// 注册免费 API 刷新回调：provider 增删/模型变化时重建上游 + 模型列表
	registerProviderAPIRefresh(server, providerAPIMgr, providerMonitor, core)
	// 注入第三方供应商枚举回调：config.Validate 时能精确校验 provider id 是否已注册
	config.SetAdditionalUpstreamValidator(func(id string) bool {
		return server.GetProviderAPI(id) != nil
	})

	// 6. Wails 服务（暴露给前端）
	providerAPISvc := service.NewProviderAPIService(providerAPIMgr, providerCatalogLoader, providerMonitor, core)
	service.SetProviderAPIRefreshCallback(rebuildProviderAPIs)
	proxySvc := service.NewProxyService(core)
	credsSvc := service.NewCredsService(core)
	modelSvc := service.NewModelService(core)
	logSvc := service.NewLogService(core)
	cfgSvc := service.NewConfigServiceWithCore(cfgMgr, core)
	// 从 DB 回读内置上游模型目录并注入 proxy —— 必须在代理开始服务之前，
	// 这样重启后 /v1/models 和路由层立刻就是上次拉到的最新一份，不必等后台刷新。
	service.LoadCatalogFromDB(cfgSvc)
	// 启动时让操作系统自启项与配置一致（用当前可执行路径重新注册，修复路径变更）
	if err := cfgSvc.ReconcileAutoStart(); err != nil {
		log.Printf("⚠️ 同步开机自启状态失败: %v", err)
	}
	pricingSvc := service.NewPricingService(pricingMgr)
	updaterMgr := updater.NewUpdater(cfgMgr.Get())
	updaterSvc := service.NewUpdaterService(updaterMgr)
	benchmarkSvc := service.NewBenchmarkService(core)

	// 7. 创建 Wails 应用
	app := application.New(application.Options{
		Name:        "Switch Dev",
		Description: "多上游 AI 模型代理管理器",
		Services: []application.Service{
			application.NewService(proxySvc),
			application.NewService(credsSvc),
			application.NewService(modelSvc),
			application.NewService(logSvc),
			application.NewService(cfgSvc),
			application.NewService(pricingSvc),
			application.NewService(updaterSvc),
			application.NewService(benchmarkSvc),
			application.NewService(providerAPISvc),
		},
		Assets: application.AssetOptions{
			Handler: application.AssetFileServerFS(assets),
		},
		// 单实例：重复启动不再开第二个进程，改为激活已有窗口。
		// Windows 用命名 Mutex、macOS 用 flock 锁文件、Linux 用 D-Bus；
		// 第二实例在 application.New() 内通知第一实例后自行退出。
		// 注意 mainWindow 在回调触发时（第二实例启动发生在本实例窗口创建之后）已赋值。
		SingleInstance: &application.SingleInstanceOptions{
			UniqueID: "com.switchdev.app",
			OnSecondInstanceLaunch: func(data application.SecondInstanceData) {
				log.Printf("检测到重复启动，激活已有窗口（args=%v）", data.Args)
				if mainWindow != nil {
					mainWindow.Show()
					mainWindow.UnMinimise()
					mainWindow.Focus()
				}
			},
		},
		// 拦截退出请求：仅托盘「退出」(realQuit=true) 放行；
		// Dock 右键退出 / Cmd+Q 改为隐藏窗口到托盘
		ShouldQuit: func() bool {
			if realQuit.Load() {
				return true
			}
			if mainWindow != nil {
				mainWindow.Hide()
			}
			return false
		},
		Mac: application.MacOptions{
			ApplicationShouldTerminateAfterLastWindowClosed: false, // 关窗口留托盘
		},
	})

	// 7. 主窗口
	mainWindow = app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:            "Switch Dev",
		Width:            960,
		Height:           680,
		MinWidth:         800,
		MinHeight:        600,
		BackgroundColour: application.NewRGB(15, 23, 42), // 深色背景
		Hidden:           startHidden,                    // --tray 开机自启时静默到托盘
		URL:              "/",
		Mac: application.MacWindow{
			InvisibleTitleBarHeight: 50,
			Backdrop:                application.MacBackdropTranslucent,
			TitleBar:                application.MacTitleBarHidden,
		},
	})

	// 拦截窗口关闭：改为隐藏到托盘（不真正退出）
	// 跨平台：macOS 关红叉、Windows/Linux 关闭按钮都触发 WindowClosing
	// 注意：托盘「退出」时 realQuit=true，必须放行（不能 Cancel），否则 cleanup 里
	// window.Close() 被本 hook 取消，窗口/webview 无法销毁，导致退出卡死
	mainWindow.RegisterHook(events.Common.WindowClosing, func(event *application.WindowEvent) {
		if realQuit.Load() {
			return // 真正退出，让 Wails 正常销毁窗口
		}
		if mainWindow != nil {
			mainWindow.Hide()
		}
		event.Cancel() // 非退出场景：取消真正关闭，窗口仅隐藏
	})

	// 拦截最小化：改为隐藏到托盘（Windows/Linux 任务栏不再占位，macOS 不缩到 Dock）
	mainWindow.OnWindowEvent(events.Common.WindowMinimise, func(event *application.WindowEvent) {
		if mainWindow != nil {
			mainWindow.Hide()
		}
	})

	// 8. 系统托盘
	setupSystray(app, server, cfgMgr, cfgSvc, logMgr)

	// 9. 启动代理 + 凭据校验（非致命：失败仅警告，等待客户端登录后自动恢复）
	go startProxyAndCreds(server, jyUp, deUp, ocUp, wbUp, core)

	// 10. 后台定期预检凭据
	go startBackgroundVerify(jyUp, deUp, ocUp, wbUp, core)

	// 10.5 后台周期探测 agent 安装状态（新安装工具时自动校验凭据并推送，无需重启）
	go core.WatchInstalledAgents(app.Context(), 5*time.Second)

	// 10.6 供应商模型健康监控（每 5 分钟探测；只监控 provider 模型）
	go providerMonitor.Start(app.Context())

	// 10.7 内置上游模型目录后台刷新（启动延迟一次 + 每 6 小时；结果落库，重启后可直接回读）
	go service.StartCatalogRefresh(app.Context(), cfgSvc, 6*time.Hour)
	// 首次刷新注册免费上游 + 模型（启动时构建）
	registerProviderAPIRefresh(server, providerAPIMgr, providerMonitor, core)
	// 注入第三方供应商枚举回调：config.Validate 时能精确校验 provider id 是否已注册
	config.SetAdditionalUpstreamValidator(func(id string) bool {
		return server.GetProviderAPI(id) != nil
	})

	// 窗口聚焦时检查更新（长时间未操作后激活）
	mainWindow.OnWindowEvent(events.Common.WindowFocus, func(event *application.WindowEvent) {
		// 在独立 goroutine 中调用，避免阻塞事件循环
		go checkUpdateOnActivation(updaterSvc)
	})
	go startUpdateCheck(updaterSvc)

	// 更新完成后：释放端口 -> 以非托盘模式启动新进程 -> 退出旧进程
	service.SetRestartHandler(func() {
		relaunchForUpdate(server, app)
	})

	// 11. 运行
	if err := app.Run(); err != nil {
		log.Fatal(err)
	}
}

// relaunchForUpdate 自动更新成功后重启应用：
//  1. 先停掉 HTTP 代理释放监听端口；
//  2. 以新二进制启动新进程（剥离 --tray，重启后正常显示主窗口，让用户看到更新结果）；
//  3. 退出旧进程。
//
// 必须在独立 goroutine 中调用，避免阻塞 Wails 事件循环。
func relaunchForUpdate(server *proxy.Server, app *application.App) {
	// 先关端口服务（不推事件，避免退出时 Emit 卡死）
	if server != nil {
		_ = server.StopQuiet()
	}

	exe, err := os.Executable()
	if err != nil {
		log.Printf("⚠️ 更新重启：无法获取可执行路径: %v", err)
	} else {
		// 剥离 --tray（更新是用户主动触发的，重启后显示主窗口）和任何旧的
		// --wait-for-pid（链式重启不应继承），再追加当前 PID：新进程会在 main()
		// 开头等本进程退出、释放单实例锁后再初始化，避免被当成"第二实例"自杀。
		args := make([]string, 0, len(os.Args))
		for _, a := range os.Args[1:] {
			if a == "--tray" || strings.HasPrefix(a, "--wait-for-pid=") {
				continue
			}
			args = append(args, a)
		}
		args = append(args, "--wait-for-pid="+strconv.Itoa(os.Getpid()))
		// 用平台相关方式启动并脱离当前进程组，否则 app.Quit() 会把子进程一起带走
		if err := startNewInstance(exe, args); err != nil {
			log.Printf("⚠️ 更新重启：启动新进程失败: %v", err)
		}
	}

	// 放行真正退出并结束旧进程（延迟，与托盘「退出」同样的退出序列，
	// 避免在当前调用栈（可能还在 Wails 事件循环内）重入 teardown 导致卡死）
	time.AfterFunc(180*time.Millisecond, func() {
		quitApplication(app)
	})
}

// quitApplication 执行真正的退出：放行 ShouldQuit，然后请 Wails 收尾。
// 带兜底看门狗：Wails/WebView2 在 Windows 上偶发 teardown 重入或卡死，
// 若 app.Quit() 在限定时间内未让进程退出，则强制 os.Exit，避免出现
// 「托盘图标残留、任务管理器找不到进程却杀不死」的僵尸状态。
func quitApplication(app *application.App) {
	realQuit.Store(true)

	// 正常退出路径（Wails 在主线程收尾窗口/托盘/WebView2 并 PostQuitMessage）
	app.Quit()

	// 兜底：给正常收尾 5 秒，仍未退出就强制结束。
	// Windows 上 Wails v3 的 cleanup 依赖主线程消息循环；一旦卡死，UI 已无响应，
	// 此时强制退出是唯一能保证不残留僵尸进程/托盘幽灵图标的手段。
	go func() {
		time.Sleep(5 * time.Second)
		log.Printf("⚠️ 正常退出超时（Wails 收尾无响应），强制结束进程")
		os.Exit(0)
	}()
}

// startUpdateCheck 启动后延迟 3s 首次检查更新，之后每 2 小时周期检查。
// 发现新版本时推送 update:available 事件给前端。
func startUpdateCheck(updaterSvc *service.UpdaterService) {
	ticker := time.NewTicker(2 * time.Hour)
	defer ticker.Stop()

	check := func() {
		info, err := updaterSvc.CheckUpdate()
		if err != nil {
			log.Printf("⚠️ 检查更新失败: %v", err)
			return
		}
		if info != nil {
			kind := "普通更新"
			if info.Critical {
				kind = "强制更新"
			}
			log.Printf("发现新版本 %s（当前 %s，%s）", info.Version, updaterSvc.GetCurrentVersion(), kind)
			updaterSvc.EmitUpdateAvailable(info)
		}
	}

	time.Sleep(3 * time.Second)
	check()
	for range ticker.C {
		check()
	}
}

// 上次激活检查的时间（跨进程边界，atomic 保证线程安全）
var lastActivationCheck atomic.Int64

// checkUpdateOnActivation 窗口激活时检查更新（防抖：至少间隔 5 分钟）
func checkUpdateOnActivation(updaterSvc *service.UpdaterService) {
	const minInterval = 5 * time.Minute
	now := time.Now().UnixNano()
	last := lastActivationCheck.Load()
	if last > 0 && now-last < minInterval.Nanoseconds() {
		return
	}
	lastActivationCheck.Store(now)

	info, err := updaterSvc.CheckUpdate()
	if err != nil {
		log.Printf("⚠️ 激活检查更新失败: %v", err)
		return
	}
	if info != nil {
		kind := "普通更新"
		if info.Critical {
			kind = "强制更新"
		}
		log.Printf("发现新版本 %s（当前 %s，%s）", info.Version, updaterSvc.GetCurrentVersion(), kind)
		updaterSvc.EmitUpdateAvailable(info)
	}
}

// setupSystray 配置系统托盘（跨平台）
// - 单击/双击托盘图标：显示/聚焦主窗口
// - 右键菜单：版本号、服务状态+启停、方案切换、检查更新、打开面板、退出
func setupSystray(app *application.App, server *proxy.Server, cfgMgr *config.Manager, cfgSvc *service.ConfigService, logMgr *logfile.Manager) *application.SystemTray {
	tray := app.SystemTray.New()
	systemTray = tray // 存全局引用，供动态刷新用
	// macOS 用模板图（透明背景 + 黑色主体），系统按菜单栏明暗自动反色；
	// 其他平台用彩色图标
	if runtime.GOOS == "darwin" {
		tray.SetTemplateIcon(trayIconTemplate)
	} else {
		tray.SetIcon(trayIconColor)
	}
	tray.SetTooltip("Switch Dev - 双击打开")

	// 显示并聚焦主窗口的公共方法
	showWindow := func() {
		if mainWindow == nil {
			return
		}
		mainWindow.Show()
		mainWindow.UnMinimise() // 从最小化恢复（若被系统最小化）
		mainWindow.Focus()
	}

	// 构建菜单（首次）
	menu := buildTrayMenu(server, cfgMgr, cfgSvc, showWindow, app)
	tray.SetMenu(menu)

	// 重建菜单的公共回调
	rebuild := func() {
		newMenu := buildTrayMenu(server, cfgMgr, cfgSvc, showWindow, app)
		tray.SetMenu(newMenu)
	}

	// 配置变化时重建菜单（方案增删改、激活态变化等）
	cfgMgr.SetOnChange(func() {
		// 同步文件日志开关
		logMgr.SetEnabled(cfgMgr.Get().LogFile.Enabled)
		// SaveConfig 在哪个 goroutine 触发就在哪个 goroutine 调用；
		// SetMenu 内部用 InvokeSync 切主线程，所以这里可以直接调
		rebuild()
	})

	// 服务状态轮询：每秒检查一次，变化时更新状态菜单项
	// （Wails 没有"菜单即将打开"的回调，用轮询近似实现"打开时最新"的效果）
	go func() {
		lastRunning := server.IsRunning()
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			nowRunning := server.IsRunning()
			if nowRunning == lastRunning {
				continue
			}
			lastRunning = nowRunning
			// 只更新状态项，不重建整个菜单（减少闪烁 + 避免关闭中的子菜单状态丢失）
			// 通过重建整份菜单来保证一致性（状态项位置索引可能随方案变化而变）
			rebuild()
		}
	}()

	// 单击/双击托盘图标打开面板（macOS 单击、Windows/Linux 通常双击）
	tray.OnClick(func() {
		showWindow()
	})
	tray.OnDoubleClick(func() {
		showWindow()
	})

	return tray
}

// buildTrayMenu 构建托盘右键菜单
// 菜单项：版本号+服务状态 / GitHub / 功能(各 tab) / 方案 / 打开面板 / 退出
func buildTrayMenu(server *proxy.Server, cfgMgr *config.Manager, cfgSvc *service.ConfigService, showWindow func(), app *application.App) *application.Menu {
	menu := application.NewMenu()
	running := server.IsRunning()

	// ── 版本号 + 服务状态（合并为第一项，禁用仅展示；每次打开菜单时读取最新状态） ──
	statusLabel := "已停止"
	if running {
		statusLabel = "运行中"
	}
	menu.Add("Switch Dev v" + version.GetVersion() + " · 服务：" + statusLabel).SetEnabled(false)

	menu.AddSeparator()

	// ── GitHub Star 引导 ──
	menu.Add("⭐ 去 GitHub 点个 Star").OnClick(func(*application.Context) {
		_ = app.Browser.OpenURL("https://github.com/rosanruan/switch-dev")
	})

	menu.AddSeparator()

	// ── 功能（主界面各 tab，点击打开窗口并切到对应 tab） ──
	tabMenu := menu.AddSubmenu("功能")
	tabs := []struct {
		key   string
		label string
	}{
		{"dashboard", "📊 仪表盘"},
		{"providerapi", "🆓 供应商"},
		{"credentials", "🔑 凭据"},
		{"models", "🤖 模型"},
		{"stats", "📈 统计"},
		{"logs", "📋 日志"},
		{"benchmark", "🏁 测评"},
		{"settings", "⚙️ 设置"},
	}
	for _, t := range tabs {
		tabKey := t.key
		tabMenu.Add(t.label).OnClick(func(*application.Context) {
			showWindow()
			app.Event.Emit("navigate:tab", tabKey)
		})
	}

	menu.AddSeparator()

	// ── 方案切换 ──
	cfg := cfgMgr.Get()
	if len(cfg.Presets) > 0 {
		// 有方案：显示子菜单（radio 形式，激活的打勾）
		presetMenu := menu.AddSubmenu("方案")
		// "未命名的方案"：与已保存方案同级，当前无激活方案时选中禁用，否则可点击切回
		if cfg.ActivePreset == "" {
			presetMenu.AddRadio("未命名的方案", true).SetEnabled(false)
		} else {
			presetMenu.AddRadio("未命名的方案", false).OnClick(func(*application.Context) {
				_ = cfgSvc.ClearActivePreset()
			})
		}
		presetMenu.AddSeparator()
		for _, p := range cfg.Presets {
			name := p.Name
			checked := cfg.ActivePreset == name
			presetMenu.AddRadio(name, checked).OnClick(func(*application.Context) {
				_ = cfgSvc.ApplyPreset(name)
			})
		}
	} else {
		// 无方案：显示"保存方案..."入口，点击跳设置页
		menu.Add("保存方案...").OnClick(func(*application.Context) {
			showWindow()
			app.Event.Emit("navigate:tab", "settings")
		})
	}

	menu.AddSeparator()

	// ── 打开面板 ──
	menu.Add("打开面板").OnClick(func(*application.Context) {
		showWindow()
	})

	menu.AddSeparator()

	// ── 退出 ──
	menu.Add("退出").OnClick(func(*application.Context) {
		// 本回调在托盘右键菜单（TrackPopupMenuEx）的模态消息循环内、主线程上派发。
		// 若在菜单关闭前直接调用 app.Quit()，Wails cleanup 会以 InvokeSync 把
		// 窗口/托盘/WebView2 的销毁排队到主线程，与尚未返回的 TrackPopupMenuEx
		// 模态循环发生重入死锁 —— 表现为：点退出后界面卡死、托盘图标残留、
		// 任务管理器里找不到/杀不掉进程。
		// 因此延迟到菜单彻底关闭、主线程空闲后，再执行真正的退出序列。
		time.AfterFunc(180*time.Millisecond, func() {
			_ = server.StopQuiet() // 先关端口（带 2s 超时，不阻塞退出）
			quitApplication(app)
		})
	})

	return menu
}

// rebuildProviderAPIs 重建免费上游 + 模型列表（provider 增删/模型变化时调用，包级供 ProviderAPIService 用）
var rebuildProviderAPIs func()

// registerProviderAPIRefresh 初始化免费 API 上游注册：
// 遍历 credentials.json 里 verified 的 provider，为每个创建 ProviderAPIUpstream 注册到 server，
// 并填充 proxy.ProviderModels 供模型列表/降级链使用。
func registerProviderAPIRefresh(server *proxy.Server, mgr *providerapi.Manager, monitor *providerapi.Monitor, core *service.Core) {
	rebuild := func() {
		// 清空旧的免费上游
		for pid := range server.GetProviderAPIs() {
			server.RemoveProviderAPI(pid)
		}
		// 重建模型列表 + 上游
		var providerModels []proxy.ProviderModel
		for pid, p := range mgr.GetProviders() {
			// 把 provider 显示名同步到 db upstreams 表（不限 Verified：
			// 历史日志即便 provider 未验证，统计页也要显示友好名称）
			if core != nil && core.DB() != nil {
				_, _ = core.DB().UpsertUpstream(pid, "provider", p.Name, pid)
			}
			if !p.Verified {
				continue
			}
			// 为 provider 创建上游（闭包捕获 baseURL/apiKey/protocol，保证 rebuild 时读到最新）
			baseURL, apiKey, proto := p.BaseURL, p.APIKey, p.EffectiveProtocol()
			up := upstream.NewProviderAPIUpstream(pid,
				func() string { return baseURL },
				func() string { return apiKey },
				func() string { return proto },
			)
			up.SetDisplayName(p.Name)
			server.RegisterProviderAPI(pid, up)

			// 收集 verified 模型
			for _, mo := range p.Models {
				if !mo.Verified {
					continue
				}
				providerModels = append(providerModels, proxy.ProviderModel{
					InternalID: p.InternalID(mo.ID),
					ProviderID: pid,
					ModelID:    mo.ID,
					Label:      mo.ID,
					Context:    mo.Context,
				})
			}
		}
		proxy.SetProviderModels(providerModels)

		// 推送状态刷新（前端凭据页/模型页）；导入供应商的明文 key 不经过事件下发前端
		if core != nil {
			core.EmitEvent("cred:change", core.GetCredStatus())
			core.EmitEvent("providerapi:change", service.SanitizeProviders(mgr.GetProviders()))
			core.EmitEvent("models:change", nil)
		}
	}
	rebuildProviderAPIs = rebuild

	// 首次构建
	rebuild()
}

// startProxyAndCreds 启动时校验凭据（非致命）+ 启动代理
func startProxyAndCreds(server *proxy.Server, jy *upstream.JoyCodeUpstream, de *upstream.DevEcoUpstream, oc *upstream.OpenCodeUpstream, wb *upstream.WorkBuddyUpstream, core *service.Core) {
	// 启动时校验四上游凭据（失败仅警告）
	if err := jy.EnsureCreds(nil); err != nil {
		log.Printf("⚠️ JoyCode 凭据不可用: %v（JoyCode 作为 auto 降级兜底，缺失不影响 DevEco 直连）", err)
	}
	if err := de.EnsureCreds(nil); err != nil {
		log.Printf("⚠️ DevEco 凭据不可用: %v（auto 模式将降级到 JoyCode）", err)
	}
	if err := oc.EnsureCreds(nil); err != nil {
		log.Printf("⚠️ OpenCode 凭据不可用: %v（仅显式选 *-provider 模型时使用）", err)
	}
	if err := wb.EnsureCreds(nil); err != nil {
		log.Printf("⚠️ WorkBuddy 凭据不可用: %v（仅显式选 wb/* 模型时使用）", err)
	}
	core.EmitEvent("cred:change", core.GetCredStatus())

	// 启动代理
	if err := server.Start(); err != nil {
		log.Printf("✗ 代理启动失败: %v", err)
		return
	}
	log.Printf("✓ 代理已启动，监听 http://127.0.0.1:%d", server.Port)
}

// startBackgroundVerify 后台定期预检凭据
func startBackgroundVerify(jy *upstream.JoyCodeUpstream, de *upstream.DevEcoUpstream, oc *upstream.OpenCodeUpstream, wb *upstream.WorkBuddyUpstream, core *service.Core) {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()

	verify := func(u upstream.Upstream) {
		result, err := u.VerifyCreds(nil)
		if err != nil {
			return
		}
		if !result.Valid {
			u.InvalidateCreds()
			core.EmitEvent("cred:change", core.GetCredStatus())
		}
	}

	// 启动后先等一会再开始周期校验
	<-ticker.C
	for range ticker.C {
		go verify(jy)
		go verify(de)
		go verify(oc)
		go verify(wb)
	}
}
