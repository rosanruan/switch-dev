# Switch Dev - 构建 / 打包 / 发布
#
# 用法:
#   make build          # 构建本机 macOS 桌面版（bin/switch-dev）
#   make package        # 打包 .app（含图标 + plist，macOS）
#   make dmg            # 打包 macOS DMG 安装镜像（含 .app + Applications 快捷方式）
#   make windows        # 交叉编译 Windows .exe（含图标嵌入）
#   make nsis           # 打包 Windows NSIS 安装程序（需 makensis）
#   make deb            # 打包 Linux deb 安装包（amd64）
#   make deb-arm64      # 打包 Linux deb 安装包（arm64）
#   make dist           # 一键打包所有平台安装包（DMG + NSIS + deb amd64/arm64）
#   make build-server   # 构建本机 server 版（无 GUI，bin/switch-dev-server）
#   make build-binaries # 构建全平台裸二进制到 dist/（自动更新用）
#   make build-server-binaries # 构建全平台 server 裸二进制到 dist/（无 GUI 服务器部署）
#   make test           # 运行 Go 测试
#   make fmt            # 格式化 Go 代码
#   make version        # 显示当前版本号
#   make changelog-auto # 从 git commit 生成 changelog 草稿到 [Unreleased]
#   make changelog-release # 发布定版：[Unreleased] -> [V] + 日期，新开空 Unreleased
#   make tag            # 打 git tag（如 make tag v=0.1.0）
#   make release        # 创建 GitHub Release
#   make upload         # 上传 dist/ 构建产物到 GitHub Release
#   make push           # 推送代码 + tag 到远程
#   make deploy         # 构建 + 推码 + 打 tag + 发 release + 上传产物（一键发布）
#   make clean          # 清理产物

# 仓库信息
REPO        ?= rosanruan/switch-dev
# 版本号（默认从 build/config.yml 读）
V           ?= $(shell grep -oE 'version: "[0-9]+\.[0-9]+\.[0-9]+"' build/config.yml | head -1 | grep -oE '[0-9]+\.[0-9]+\.[0-9]+')
TAG         ?= v$(V)
# 二进制名
APP         := switch-dev
# 产物目录
DIST        := dist
# 架构
ARCH        ?= amd64

.PHONY: build package package-universal dmg dmg-universal windows nsis nsis-arm64 deb dist build-server build-frontend build-binaries build-server-binaries build-darwin-arm64 build-darwin-amd64 build-windows-amd64 build-windows-arm64 build-linux-amd64 build-linux-arm64 build-server-linux-amd64 build-server-linux-arm64 build-server-darwin-arm64 build-server-darwin-amd64 test fmt version tag release upload push deploy clean sync-version changelog-auto changelog-release

## 同步版本号：V -> build/config.yml + version/config.yml（两处保持一致，单一真相来源是 V 参数 / build/config.yml）
sync-version:
	@perl -i -pe 's/version: "[0-9]+\.[0-9]+\.[0-9]+"/version: "$(V)"/' build/config.yml
	@perl -i -pe 's/version: "[0-9]+\.[0-9]+\.[0-9]+"/version: "$(V)"/' version/config.yml
	@# Windows .exe 文件属性版本（syso 从 info.json 读取）
	@perl -i -pe 's/("(?:file_version|ProductVersion)"\s*:\s*")[0-9]+\.[0-9]+\.[0-9]+(")/$${1}$(V)$${2}/' build/windows/info.json
	@# macOS .app/DMG Finder 显示版本（Info.plist 的 CFBundleVersion / CFBundleShortVersionString）
	@perl -0777 -i -pe 's/(CFBundle(?:ShortVersionString|Version)<\/key>\s*<string>)[0-9]+\.[0-9]+\.[0-9]+(<\/string>)/$${1}$(V)$${2}/g' build/darwin/Info.plist build/darwin/Info.dev.plist
	@echo "✅ 同步版本号 $(V) -> config.yml + version/config.yml + info.json + Info.plist"

## 构建本机 macOS 桌面版（wails3 完整 GUI）
build: sync-version
	wails3 build
	@echo "✅ 构建完成: bin/$(APP)"

## 打包 .app（含图标 + plist + codesign，仅 macOS）
package: build
	@mkdir -p bin/$(APP).app/Contents/MacOS
	@mkdir -p bin/$(APP).app/Contents/Resources
	@cp build/darwin/icons.icns bin/$(APP).app/Contents/Resources/
	@if [ -f build/darwin/Assets.car ]; then cp build/darwin/Assets.car bin/$(APP).app/Contents/Resources/; fi
	@cp bin/$(APP) bin/$(APP).app/Contents/MacOS/
	@cp build/darwin/Info.plist bin/$(APP).app/Contents/
	@codesign --force --deep --sign - bin/$(APP).app
	@echo "✅ 打包完成: bin/$(APP).app"

## 打包 universal（arm64+amd64）.app：复用 dist/ 下两个裸二进制，lipo 合并后打包
package-universal: build-binaries
	@test -f $(DIST)/$(APP)-darwin-arm64 || (echo "❌ 缺少 dist/$(APP)-darwin-arm64"; exit 1)
	@test -f $(DIST)/$(APP)-darwin-amd64 || (echo "❌ 缺少 dist/$(APP)-darwin-amd64"; exit 1)
	@mkdir -p bin
	lipo -create -output bin/$(APP) $(DIST)/$(APP)-darwin-arm64 $(DIST)/$(APP)-darwin-amd64
	@mkdir -p bin/$(APP).app/Contents/MacOS
	@mkdir -p bin/$(APP).app/Contents/Resources
	@cp build/darwin/icons.icns bin/$(APP).app/Contents/Resources/
	@if [ -f build/darwin/Assets.car ]; then cp build/darwin/Assets.car bin/$(APP).app/Contents/Resources/; fi
	@cp bin/$(APP) bin/$(APP).app/Contents/MacOS/
	@cp build/darwin/Info.plist bin/$(APP).app/Contents/
	@codesign --force --deep --sign - bin/$(APP).app
	@echo "✅ Universal 打包完成: bin/$(APP).app ($$(lipo -archs bin/$(APP)))"

## 打包 macOS DMG 安装镜像
dmg: package
	@mkdir -p $(DIST)
	@rm -f $(DIST)/$(APP)-darwin-$(ARCH).dmg
	@# 清理上次失败可能残留的挂载与临时 DMG（hdiutil create 不覆盖已存在文件）
	@hdiutil detach "/Volumes/Switch Dev" 2>/dev/null || true
	@hdiutil detach "/Volumes/Switch Dev 1" 2>/dev/null || true
	@rm -f /tmp/switch-dev_rw.dmg
	@# 创建可写 DMG
	hdiutil create -size 50m -volname "Switch Dev" \
		-fs HFS+ -fsargs "-c c=64,a=16,e=16" /tmp/switch-dev_rw.dmg
	@# 挂载
	hdiutil attach /tmp/switch-dev_rw.dmg -readwrite -noverify -noautoopen
	@# 复制 .app + Applications 快捷方式 + 卷图标
	cp -R bin/$(APP).app "/Volumes/Switch Dev/Switch Dev.app"
	ln -sf /Applications "/Volumes/Switch Dev/Applications"
	cp build/darwin/icons.icns "/Volumes/Switch Dev/.VolumeIcon.icns"
	SetFile -c 'icnC' "/Volumes/Switch Dev/.VolumeIcon.icns"
	SetFile -a C "/Volumes/Switch Dev"
	@# 设置窗口布局（图标 96px，左侧 .app 右侧 Applications）
	osascript -e 'tell application "Finder"' \
		-e 'set dmg to disk "Switch Dev"' \
		-e 'open dmg' \
		-e 'set dmgWin to container window of dmg' \
		-e 'set current view of dmgWin to icon view' \
		-e 'set icon size of icon view options of dmgWin to 96' \
		-e 'set arrangement of icon view options of dmgWin to not arranged' \
		-e 'set label position of icon view options of dmgWin to bottom' \
		-e 'set bounds of dmgWin to {100, 100, 620, 460}' \
		-e 'set position of item "Switch Dev.app" of dmgWin to {175, 190}' \
		-e 'set position of item "Applications" of dmgWin to {435, 190}' \
		-e 'close dmgWin' \
		-e 'end tell'
	@sync
	@sleep 2
	@# 卸载并压缩为只读 DMG
	hdiutil detach "/Volumes/Switch Dev"
	hdiutil convert /tmp/switch-dev_rw.dmg -format UDZO -o $(DIST)/$(APP)-darwin-$(ARCH).dmg
	@rm -f /tmp/switch-dev_rw.dmg
	@echo "✅ DMG 打包完成: $(DIST)/$(APP)-darwin-$(ARCH).dmg"

## 打包 macOS Universal（arm64+amd64）DMG 安装镜像
dmg-universal: package-universal
	@mkdir -p $(DIST)
	@rm -f $(DIST)/$(APP)-darwin-universal.dmg
	@# 清理上次失败可能残留的挂载与临时 DMG（hdiutil create 不覆盖已存在文件）
	@hdiutil detach "/Volumes/Switch Dev" 2>/dev/null || true
	@hdiutil detach "/Volumes/Switch Dev 1" 2>/dev/null || true
	@rm -f /tmp/switch-dev_rw.dmg
	@# 创建可写 DMG
	hdiutil create -size 80m -volname "Switch Dev" \
		-fs HFS+ -fsargs "-c c=64,a=16,e=16" /tmp/switch-dev_rw.dmg
	@# 挂载
	hdiutil attach /tmp/switch-dev_rw.dmg -readwrite -noverify -noautoopen
	@# 复制 .app + Applications 快捷方式 + 卷图标
	cp -R bin/$(APP).app "/Volumes/Switch Dev/Switch Dev.app"
	ln -sf /Applications "/Volumes/Switch Dev/Applications"
	cp build/darwin/icons.icns "/Volumes/Switch Dev/.VolumeIcon.icns"
	SetFile -c 'icnC' "/Volumes/Switch Dev/.VolumeIcon.icns"
	SetFile -a C "/Volumes/Switch Dev"
	@# 设置窗口布局
	osascript -e 'tell application "Finder"' \
		-e 'set dmg to disk "Switch Dev"' \
		-e 'open dmg' \
		-e 'set dmgWin to container window of dmg' \
		-e 'set current view of dmgWin to icon view' \
		-e 'set icon size of icon view options of dmgWin to 96' \
		-e 'set arrangement of icon view options of dmgWin to not arranged' \
		-e 'set label position of icon view options of dmgWin to bottom' \
		-e 'set bounds of dmgWin to {100, 100, 620, 460}' \
		-e 'set position of item "Switch Dev.app" of dmgWin to {175, 190}' \
		-e 'set position of item "Applications" of dmgWin to {435, 190}' \
		-e 'close dmgWin' \
		-e 'end tell'
	@sync
	@sleep 2
	@# 卸载并压缩为只读 DMG
	hdiutil detach "/Volumes/Switch Dev"
	hdiutil convert /tmp/switch-dev_rw.dmg -format UDZO -o $(DIST)/$(APP)-darwin-universal.dmg
	@rm -f /tmp/switch-dev_rw.dmg
	@echo "✅ Universal DMG 打包完成: $(DIST)/$(APP)-darwin-universal.dmg"

## 交叉编译 Windows .exe（含图标嵌入）
windows: sync-version
	@mkdir -p bin
	@# 生成 syso（将图标嵌入 .exe）
	wails3 generate syso -arch $(ARCH) \
		-icon build/windows/icon.ico \
		-manifest build/windows/wails.exe.manifest \
		-info build/windows/info.json \
		-out wails_windows_$(ARCH).syso
	@# 交叉编译
	GOOS=windows CGO_ENABLED=0 GOARCH=$(ARCH) \
		go build -tags production -trimpath -buildvcs=false \
		-ldflags="-w -s -H windowsgui" -o bin/$(APP).exe .
	@rm -f wails_windows_$(ARCH).syso
	@echo "✅ Windows 构建完成: bin/$(APP).exe"

## 打包 Windows NSIS 安装程序 - amd64（需 makensis）
nsis: windows
	@mkdir -p $(DIST)
	@# 生成 WebView2 引导程序
	wails3 generate webview2bootstrapper -dir build/windows/nsis
	@# 构建 NSIS 安装包（显式注入版本号/公司/产品名/版权，避免 wails_tools.nsh 的缓存默认值过期）
	makensis -DARG_WAILS_AMD64_BINARY="$(shell pwd)/bin/$(APP).exe" \
		-DINFO_PRODUCTVERSION="$(V)" \
		-DINFO_COMPANYNAME="Switch Dev" \
		-DINFO_PRODUCTNAME="Switch Dev" \
		-DINFO_COPYRIGHT="(c) 2025-2026, Switch Dev Contributors" \
		"$(shell pwd)/build/windows/nsis/project.nsi"
	@# 复制到 dist
	cp bin/$(APP)-amd64-installer.exe $(DIST)/$(APP)-windows-amd64-installer.exe
	@echo "✅ NSIS amd64 安装包完成: $(DIST)/$(APP)-windows-amd64-installer.exe"

## 打包 Windows NSIS 安装程序 - arm64（需 makensis）
nsis-arm64: build-windows-arm64
	@mkdir -p $(DIST)
	wails3 generate webview2bootstrapper -dir build/windows/nsis
	makensis -DARG_WAILS_ARM64_BINARY="$(shell pwd)/dist/$(APP)-windows-arm64.exe" \
		-DINFO_PRODUCTVERSION="$(V)" \
		-DINFO_COMPANYNAME="Switch Dev" \
		-DINFO_PRODUCTNAME="Switch Dev" \
		-DINFO_COPYRIGHT="(c) 2025-2026, Switch Dev Contributors" \
		"$(shell pwd)/build/windows/nsis/project.nsi"
	cp bin/$(APP)-arm64-installer.exe $(DIST)/$(APP)-windows-arm64-installer.exe
	@echo "✅ NSIS arm64 安装包完成: $(DIST)/$(APP)-windows-arm64-installer.exe"

## 打包 Linux deb 安装包（需先构建对应架构二进制）
deb: build-linux-amd64
	@wails3 generate .desktop -name "$(APP)" -exec "$(APP)" -icon "$(APP)" -outputfile "build/linux/$(APP).desktop" -categories "Development;"
	@GOARCH=amd64 wails3 tool package -name "$(APP)" -format deb -config ./build/linux/nfpm/nfpm.yaml -out $(DIST)
	@# wails3 tool package 输出文件名可能不带架构，重命名确保匹配 upload 通配
	@if [ -f $(DIST)/$(APP).deb ] && [ ! -f $(DIST)/$(APP)_*_amd64.deb ]; then mv $(DIST)/$(APP).deb $(DIST)/$(APP)_amd64.deb; fi
	@echo "✅ deb 打包完成: $(DIST)/$(APP)_*_amd64.deb"

## 打包 Linux arm64 deb 安装包
deb-arm64: build-linux-arm64
	@wails3 generate .desktop -name "$(APP)" -exec "$(APP)" -icon "$(APP)" -outputfile "build/linux/$(APP).desktop" -categories "Development;"
	@GOARCH=arm64 wails3 tool package -name "$(APP)" -format deb -config ./build/linux/nfpm/nfpm.yaml -out $(DIST)
	@if [ -f $(DIST)/$(APP).deb ] && [ ! -f $(DIST)/$(APP)_*_arm64.deb ]; then mv $(DIST)/$(APP).deb $(DIST)/$(APP)_arm64.deb; fi
	@echo "✅ deb arm64 打包完成: $(DIST)/$(APP)_*_arm64.deb"

## 一键打包所有平台安装包（Universal DMG + NSIS amd64/arm64 + deb amd64/arm64）
dist: dmg-universal nsis nsis-arm64 deb deb-arm64
	@echo ""
	@echo "🎉 全部安装包打包完成:"
	@ls -lh $(DIST)/*

## 构建本机 server 版（无 GUI 纯 HTTP 代理）
build-server: sync-version
	wails3 task build:server
	@echo "✅ server 构建完成: bin/$(APP)-server"

## ===== 裸二进制构建（自动更新用，产物名须匹配 updater/github.go 的 assetName()）=====

## 构建前端（多平台/多架构共用，避免重复构建）
build-frontend:
	cd frontend && npm run build
	@echo "✅ 前端构建完成"

## 构建 macOS arm64 裸二进制
build-darwin-arm64: sync-version build-frontend
	@mkdir -p $(DIST)
	GOOS=darwin CGO_ENABLED=1 GOARCH=arm64 \
		CGO_CFLAGS="-mmacosx-version-min=10.15" \
		CGO_LDFLAGS="-mmacosx-version-min=10.15" \
		MACOSX_DEPLOYMENT_TARGET="10.15" \
		go build -tags production -trimpath -buildvcs=false \
		-ldflags="-w -s" -o $(DIST)/$(APP)-darwin-arm64 .
	@echo "✅ macOS arm64: $(DIST)/$(APP)-darwin-arm64"

## 构建 macOS amd64 裸二进制
build-darwin-amd64: sync-version build-frontend
	@mkdir -p $(DIST)
	GOOS=darwin CGO_ENABLED=1 GOARCH=amd64 \
		CGO_CFLAGS="-mmacosx-version-min=10.15" \
		CGO_LDFLAGS="-mmacosx-version-min=10.15" \
		MACOSX_DEPLOYMENT_TARGET="10.15" \
		go build -tags production -trimpath -buildvcs=false \
		-ldflags="-w -s" -o $(DIST)/$(APP)-darwin-amd64 .
	@echo "✅ macOS amd64: $(DIST)/$(APP)-darwin-amd64"

## 交叉编译 Windows amd64 裸二进制（CGO_ENABLED=0，含图标嵌入）
build-windows-amd64: sync-version build-frontend
	@mkdir -p $(DIST)
	wails3 generate syso -arch amd64 \
		-icon build/windows/icon.ico \
		-manifest build/windows/wails.exe.manifest \
		-info build/windows/info.json \
		-out wails_windows_amd64.syso
	GOOS=windows CGO_ENABLED=0 GOARCH=amd64 \
		go build -tags production -trimpath -buildvcs=false \
		-ldflags="-w -s -H windowsgui" -o $(DIST)/$(APP)-windows-amd64.exe .
	@rm -f wails_windows_amd64.syso
	@echo "✅ Windows amd64: $(DIST)/$(APP)-windows-amd64.exe"

## 交叉编译 Windows arm64 裸二进制（CGO_ENABLED=0，含图标嵌入）
build-windows-arm64: sync-version build-frontend
	@mkdir -p $(DIST)
	wails3 generate syso -arch arm64 \
		-icon build/windows/icon.ico \
		-manifest build/windows/wails.exe.manifest \
		-info build/windows/info.json \
		-out wails_windows_arm64.syso
	GOOS=windows CGO_ENABLED=0 GOARCH=arm64 \
		go build -tags production -trimpath -buildvcs=false \
		-ldflags="-w -s -H windowsgui" -o $(DIST)/$(APP)-windows-arm64.exe .
	@rm -f wails_windows_arm64.syso
	@echo "✅ Windows arm64: $(DIST)/$(APP)-windows-arm64.exe"

## Docker 交叉编译 Linux amd64 裸二进制（需 wails-cross 镜像，首次运行 task setup:docker 构建）
build-linux-amd64: sync-version build-frontend
	@docker info > /dev/null 2>&1 || (echo "❌ Docker 未运行" && exit 1)
	@docker image inspect wails-cross:latest > /dev/null 2>&1 || (echo "❌ wails-cross 镜像不存在，先运行 task setup:docker" && exit 1)
	@mkdir -p $(DIST)
	docker run --rm \
		-v "$(shell pwd):/app" \
		-v "$(shell go env GOPATH)/pkg/mod:/go/pkg/mod" \
		-e APP_NAME="$(APP)" \
		wails-cross linux amd64
	docker run --rm --entrypoint chown -v "$(shell pwd):/app" wails-cross -R $$(id -u):$$(id -g) /app/bin
	mv bin/$(APP)-linux-amd64 $(DIST)/$(APP)-linux-amd64
	@echo "✅ Linux amd64: $(DIST)/$(APP)-linux-amd64"

## Docker 交叉编译 Linux arm64 裸二进制（通过 QEMU 模拟运行 arm64 容器，较慢）
build-linux-arm64: sync-version build-frontend
	@docker info > /dev/null 2>&1 || (echo "❌ Docker 未运行" && exit 1)
	@if ! docker image inspect wails-cross-arm64:latest > /dev/null 2>&1; then \
		echo "🔄 首次构建 arm64 镜像（耐心等待）..."; \
		docker build --platform linux/arm64 \
			--build-arg BASE_IMAGE=golang:1.25-bookworm-arm64 \
			-t wails-cross-arm64 \
			-f build/docker/Dockerfile.cross build/docker/; \
	fi
	@mkdir -p $(DIST)
	docker run --rm --platform linux/arm64 \
		-v "$(shell pwd):/app" \
		-v "$(shell go env GOPATH)/pkg/mod:/go/pkg/mod" \
		-e APP_NAME="$(APP)" \
		wails-cross-arm64 linux arm64
	docker run --rm --platform linux/arm64 --entrypoint chown -v "$(shell pwd):/app" wails-cross-arm64 -R $$(id -u):$$(id -g) /app/bin
	mv bin/$(APP)-linux-arm64 $(DIST)/$(APP)-linux-arm64
	@echo "✅ Linux arm64: $(DIST)/$(APP)-linux-arm64"

## 构建全平台裸二进制到 dist/（自动更新检测的资产）
build-binaries: build-darwin-arm64 build-darwin-amd64 build-windows-amd64 build-windows-arm64 build-linux-amd64 build-linux-arm64
	@echo ""
	@echo "🎉 全部裸二进制构建完成:"
	@ls -lh $(DIST)/$(APP)-*

## ===== Server 裸二进制构建（无 GUI，供服务器部署，不走自动更新）=====

## 构建 server Linux amd64 裸二进制（CGO_ENABLED=0，无需 Docker）
build-server-linux-amd64: sync-version build-frontend
	@mkdir -p $(DIST)
	GOOS=linux CGO_ENABLED=0 GOARCH=amd64 \
		go build -tags server -trimpath -buildvcs=false \
		-ldflags="-w -s" -o $(DIST)/$(APP)-server-linux-amd64 .
	@echo "✅ Server Linux amd64: $(DIST)/$(APP)-server-linux-amd64"

## 构建 server Linux arm64 裸二进制（CGO_ENABLED=0，无需 Docker）
build-server-linux-arm64: sync-version build-frontend
	@mkdir -p $(DIST)
	GOOS=linux CGO_ENABLED=0 GOARCH=arm64 \
		go build -tags server -trimpath -buildvcs=false \
		-ldflags="-w -s" -o $(DIST)/$(APP)-server-linux-arm64 .
	@echo "✅ Server Linux arm64: $(DIST)/$(APP)-server-linux-arm64"

## 构建 server macOS arm64 裸二进制（CGO_ENABLED=0）
build-server-darwin-arm64: sync-version build-frontend
	@mkdir -p $(DIST)
	GOOS=darwin CGO_ENABLED=0 GOARCH=arm64 \
		go build -tags server -trimpath -buildvcs=false \
		-ldflags="-w -s" -o $(DIST)/$(APP)-server-darwin-arm64 .
	@echo "✅ Server macOS arm64: $(DIST)/$(APP)-server-darwin-arm64"

## 构建 server macOS amd64 裸二进制（CGO_ENABLED=0）
build-server-darwin-amd64: sync-version build-frontend
	@mkdir -p $(DIST)
	GOOS=darwin CGO_ENABLED=0 GOARCH=amd64 \
		go build -tags server -trimpath -buildvcs=false \
		-ldflags="-w -s" -o $(DIST)/$(APP)-server-darwin-amd64 .
	@echo "✅ Server macOS amd64: $(DIST)/$(APP)-server-darwin-amd64"

## 构建全平台 server 裸二进制到 dist/
build-server-binaries: build-server-linux-amd64 build-server-linux-arm64 build-server-darwin-arm64 build-server-darwin-amd64
	@echo ""
	@echo "🎉 全部 server 裸二进制构建完成:"
	@ls -lh $(DIST)/$(APP)-server-*

## 运行 Go 测试（排除 wails 模板目录 build/，其 package main 无 func main 会编译报错）
test:
	go test $$(go list ./... | grep -v '/build/')

## 格式化 Go 代码
fmt:
	gofmt -w $$(find . -name '*.go' -not -path './frontend/node_modules/*' -not -path '*/bindings/*')
	@echo "✅ Go 代码已格式化"

## 显示当前版本号
version:
	@echo "版本: $(V) | tag: $(TAG)"

## 打 git tag（已存在则跳过）
tag:
	@test -n "$(V)" || (echo "❌ 版本号为空" && exit 1)
	@if git rev-parse $(TAG) >/dev/null 2>&1; then \
		echo "ℹ️ tag $(TAG) 已存在，跳过"; \
	else \
		git tag -a $(TAG) -m "Release $(TAG)"; \
		echo "✅ 已打 tag: $(TAG)"; \
	fi

## 从 git commit 生成 changelog 草稿，插入 CHANGELOG.md 的 [Unreleased] 章节
## 扫描上个 tag（无 tag 则从首次提交）到 HEAD，按 Conventional Commits 前缀归类
changelog-auto:
	@test -f CHANGELOG.md || (echo "❌ 找不到 CHANGELOG.md" && exit 1)
	@grep -q '^## \[Unreleased\]' CHANGELOG.md || (echo "❌ CHANGELOG.md 缺少 '## [Unreleased]' 章节" && exit 1)
	@last=$$(git describe --tags --abbrev=0 2>/dev/null || echo ""); \
	if [ -n "$$last" ]; then range="$$last..HEAD"; echo "🔄 提取 $$last 到 HEAD 的提交..."; \
	else range="HEAD"; echo "🔄 无历史 tag，提取全部提交..."; fi; \
	feats=$$(git log $$range --no-merges --pretty=format:'%s' | grep -E '^feat(\(.+\))?:' | sed -E 's/^feat(\(.+\))?: */- /' || true); \
	fixes=$$(git log $$range --no-merges --pretty=format:'%s' | grep -E '^fix(\(.+\))?:' | sed -E 's/^fix(\(.+\))?: */- /' || true); \
	others=$$(git log $$range --no-merges --pretty=format:'%s' | grep -vE '^(feat|fix)(\(.+\))?:' | sed -E 's/^[a-z]+(\(.+\))?: */- /' || true); \
	if [ -z "$$feats$$fixes$$others" ]; then echo "ℹ️ 无新提交，未改动 CHANGELOG.md"; exit 0; fi; \
	{ \
		echo "<!-- 以下由 make changelog-auto 生成，请人工润色为面向用户的描述后删除本行注释 -->"; \
		[ -n "$$feats" ] && { echo "### 新增"; echo "$$feats"; echo ""; }; \
		[ -n "$$fixes" ] && { echo "### 修复"; echo "$$fixes"; echo ""; }; \
		[ -n "$$others" ] && { echo "### 变更"; echo "$$others"; echo ""; }; \
	} > .changelog-draft.tmp; \
	awk '/^## \[Unreleased\]/{print; print ""; while((getline line < ".changelog-draft.tmp") > 0) print line; next} {print}' CHANGELOG.md > CHANGELOG.md.new; \
	mv CHANGELOG.md.new CHANGELOG.md; \
	rm -f .changelog-draft.tmp; \
	echo "✅ 草稿已插入 CHANGELOG.md 的 [Unreleased]，请人工润色"

## 发布定版：把 [Unreleased] 改名为 [V] - 今天日期，并在上面新开空 [Unreleased]
changelog-release:
	@test -n "$(V)" || (echo "❌ 版本号为空（用 make changelog-release V=x.y.z）" && exit 1)
	@test -f CHANGELOG.md || (echo "❌ 找不到 CHANGELOG.md" && exit 1)
	@grep -q '^## \[Unreleased\]' CHANGELOG.md || (echo "❌ CHANGELOG.md 缺少 '## [Unreleased]' 章节" && exit 1)
	@if grep -q '^## \[$(V)\]' CHANGELOG.md; then echo "❌ CHANGELOG.md 已存在 [$(V)] 章节"; exit 1; fi
	@# 校验 Unreleased 下有内容（到下一个 ## 之前非空）
	@content=$$(awk '/^## \[Unreleased\]/{flag=1; next} /^## \[/{flag=0} flag' CHANGELOG.md | grep -v '^[[:space:]]*$$' | grep -v '^<!--' || true); \
	if [ -z "$$content" ]; then \
		echo "❌ [Unreleased] 章节为空，先记录变更（或运行 make changelog-auto 生成草稿）"; exit 1; \
	fi
	@today=$$(date +%Y-%m-%d); \
	awk -v ver="$(V)" -v d="$$today" '/^## \[Unreleased\]/{print "## [Unreleased]"; print ""; print "## [" ver "] - " d; next} {print}' CHANGELOG.md > CHANGELOG.md.new; \
	mv CHANGELOG.md.new CHANGELOG.md; \
	echo "✅ CHANGELOG.md 已定版: [$(V)] - $$today（并新开空 [Unreleased]）"

## 创建 GitHub Release（自动从 CHANGELOG.md 提取对应版本的章节作为 release notes）
release:
	@test -n "$(V)" || (echo "❌ 版本号为空" && exit 1)
	@test -n "$$(gh auth status 2>&1 | grep -o 'Logged in')" || (echo "❌ 请先运行 gh auth login" && exit 1)
	@test -f CHANGELOG.md || (echo "❌ 找不到 CHANGELOG.md" && exit 1)
	@echo "🔄 从 CHANGELOG.md 提取 [$(V)] 章节..."
	@# 提取 "## [x.y.z]" 到下一个 "## [" 之间的内容；去掉标题行本身
	@awk '/^## \[$(V)\]/{flag=1; next} /^## \[/{flag=0} flag' CHANGELOG.md > .release-notes.tmp
	@if [ ! -s .release-notes.tmp ]; then \
		echo "❌ CHANGELOG.md 中找不到 [$(V)] 章节，请先添加变更记录"; exit 1; \
	fi
	@echo "🔄 创建 Release $(TAG) 到 $(REPO)..."
	gh release create $(TAG) \
		--repo $(REPO) \
		--title "Switch Dev $(TAG)" \
		--notes-file .release-notes.tmp \
		|| echo "⚠️ Release 可能已存在，尝试 upload 资产"
	@rm -f .release-notes.tmp
	@echo "✅ Release $(TAG) 已创建"

## 上传 dist/ 产物到 GitHub Release：
##   - 裸二进制（自动更新用，名称须匹配 updater/github.go 的 assetName()，必须存在）
##   - 安装包 DMG/NSIS/deb（用户手动下载用，存在则上传，缺失跳过并提示）
upload:
	@test -n "$(V)" || (echo "❌ 版本号为空" && exit 1)
	@test -n "$$(gh auth status 2>&1 | grep -o 'Logged in')" || (echo "❌ 请先运行 gh auth login" && exit 1)
	@echo "🔄 上传 dist/ 产物到 Release $(TAG)..."
	@required="$(DIST)/$(APP)-darwin-arm64 $(DIST)/$(APP)-darwin-amd64 $(DIST)/$(APP)-windows-amd64.exe $(DIST)/$(APP)-linux-amd64"; \
	missing=""; \
	for f in $$required; do [ -f "$$f" ] || missing="$$missing $$f"; done; \
	if [ -n "$$missing" ]; then echo "❌ 缺少裸二进制:$$missing（先 make build-binaries）"; exit 1; fi; \
	optional="$(DIST)/$(APP)-linux-arm64 $(DIST)/$(APP)-windows-arm64.exe $(DIST)/$(APP)-darwin-universal.dmg $(DIST)/$(APP)-windows-amd64-installer.exe $(DIST)/$(APP)-windows-arm64-installer.exe $(DIST)/$(APP)_*_amd64.deb $(DIST)/$(APP)_*_arm64.deb $(DIST)/$(APP)-server-linux-amd64 $(DIST)/$(APP)-server-linux-arm64 $(DIST)/$(APP)-server-darwin-arm64 $(DIST)/$(APP)-server-darwin-amd64"; \
	toUpload="$$required"; \
	for f in $$optional; do [ -f "$$f" ] && toUpload="$$toUpload $$f" || echo "ℹ️ 跳过可选安装包（不存在）: $$f"; done; \
	gh release upload $(TAG) $$toUpload --repo $(REPO) --clobber; \
	echo "✅ 已上传:$$toUpload"

## 推送代码 + tag 到远程
push:
	git push origin main
	@echo "✅ 代码已推送"
	git push origin $(TAG) 2>/dev/null || echo "ℹ️ tag $(TAG) 已存在或无新 tag 可推"

## 一键发布：构建裸二进制 + 安装包（DMG/NSIS）-> 打 tag -> 推码（含 tag）-> 发 release -> 上传全部产物
deploy: build-binaries build-server-binaries dist tag push release upload
	@echo "🎉 发布流程完成: https://github.com/$(REPO)/releases/tag/$(TAG)"

## 清理产物
clean:
	rm -rf bin $(DIST)
	rm -f $(APP)
	@echo "✅ 已清理"