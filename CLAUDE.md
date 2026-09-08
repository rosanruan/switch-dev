# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

# Switch Dev (Wails v3 版)

## 项目性质
把 JoyCode/DevEco/OpenCode/WorkBuddy 四套 AI 编程工具的登录态模型能力，外加可接入的第三方 OpenAI/Anthropic 兼容供应商（免费 API / 自带 Key），通过本地代理暴露为标准 Anthropic/OpenAI 接口，供 Claude Code/Codex/cc-switch 复用。带 Wails v3 桌面 GUI。

## 关键架构
- 代理 HTTP 服务独立运行在 `127.0.0.1:8787`（`proxy.Server`），与 Wails 窗口共存
- 四上游统一实现 `upstream.Upstream` 接口（Call/VerifyCreds/EnsureCreds/InvalidateCreds/CredStatus）
- 凭据动态读取，不硬编码：JoyCode 从 state.vscdb（用 `modernc.org/sqlite` 纯 Go 驱动，无需外部 sqlite3 CLI），DevEco 三层 AES-256-GCM 解密，OpenCode 明文 auth.json，WorkBuddy 明文 info + refreshToken 续期
- 凭据路径跨平台解析（`paths.*Candidates()`，单一真相）：macOS `~/Library/Application Support`、Linux `~/.config`、Windows `%APPDATA%`；`IsAgentInstalled` 探测与 `EnsureCreds` 加载共用同一套候选路径，环境变量 `JOYCODE_VSCDB`/`WORKBUDDY_INFO_PATH`/`OPENCODE_AUTH_PATH` 等可覆盖
- 运行模式分 auto / manual / ua 三种，均可通过前端 Settings 自由配置
- auto 模式：`Config.AutoChain []AgentModels` 按优先级排列，每个 AgentModels 按 upstream 分组包含模型列表；`expandAutoChain()` 扁平化为 `[]ModelRef` 执行，末尾追加 `GlobalFallback`（去重）；客户端发任意具体模型名也按此链执行（不再按模型名硬路由）
- manual 模式：`Config.ManualFallbacks map[string][]ModelRef` 为每个模型单独配降级链，`expandManual()` 解析请求模型 → 查链 → 追加 GlobalFallback；未识别的模型名直接落到 GlobalFallback
- ua 模式：完全由 User-Agent 规则驱动，不叠加 auto/manual 链。`Config.UARules []UARule` 按 UA 子串匹配，命中规则后：先匹配 `Mappings`（请求模型精确映射），未命中则用规则级 `DefaultTarget`（cc-switch 式默认目标，请求模型 id 原样透传，免维护客户端模型清单），整条链末尾追加 `UAGlobalFallback`
  - auto/manual 模式下 UA 路由是叠加层（`UARoutingEnabled` 开关）：UA 命中时把目标插链首，正常降级链追加在后兜底；ua 模式下 UA 路由始终启用
  - `UARule{ID,Name,Pattern,Enabled,Mappings,DefaultTarget}`、`UAModelMap{RequestedModel,Target}`、`UAGlobalFallback` 都属于方案快照字段（随 Preset 保存/切换）
- 全局兜底：`Config.GlobalFallback ModelRef`，auto/manual 共享，所有链都失败时的最终保底；`UAGlobalFallback` 是 ua 模式专用兜底
- 方案（Preset）：`Config.Presets []Preset` + `Config.ActivePreset string`，把运行模式路由字段（mode/autoChain/manualFallbacks/globalFallback/uaRoutingEnabled/uaRules/uaGlobalFallback）存为命名**快照**
  - 快照语义：保存冻结当前配置，切换覆盖回当前配置；切换后继续编辑**不回写方案**，需再次同名保存才更新
  - 方案**不含** port/apiKey/update（环境配置；apiKey 变了会让已接入客户端 401）
  - `ActivePreset` 仅作 UI 提示、不参与 `Resolve`；前端检测到当前配置偏离方案时置空（下拉显示「自定义」）
  - `Validate` 刻意**不校验** `ActivePreset` 存在性 —— `Load()` 校验失败会把整份配置重置为默认，悬空方案名不值得让用户丢配置
  - Manager 的 `SavePreset/ApplyPreset/DeletePreset/RenamePreset` 一律走 `Get()` 取克隆 → 改 → `SaveConfig`；**不能自己持锁再调 SaveConfig**（后者会 `m.mu.Lock()`，死锁）
  - 前端偏离检测的指纹（`Settings.tsx: modeFingerprint`）必须规范化：manualFallbacks 是 map，Go 序列化排序 key 而前端新增是插入顺序，直接 `JSON.stringify` 会把内容相同的误判为偏离；同时要归一 Go 空 slice 的 `null`
- 降级执行：`router.executeChain` / `executeChainStream` 逐个尝试，跳过凭据无效的 upstream，任意成功即返回
- 默认 auto 链为空（用户自行配置）；旧版硬编码的 DevEco→JoyCode 降级及 `AutoModel*` 常量已移除
- **改 `Config` 结构必须同步三处**：`Defaults()` / `Clone()` / `Update()` —— `Clone()` 与 `copyUARules()`/`copyChain()`/`copyFallbacks()` 都是手写逐字段的，漏了新字段会静默丢数据（`config/preset_test.go: TestCloneIsolatesPresets`、`resolver_test.go: TestCloneIsolatesUARuleDefaultTarget` 守这条）
- **流式**：四个内置上游 + 供应商**全部实现 `upstream.StreamCaller`（`CallStream`）**，客户端要流式时把上游 SSE 逐块直传（`bufferedReadCloser` + 每事件 `Flush()`），首字延迟 ≈ 上游首字。伪流式（`Call` 拿完整响应 + `WriteAnthropicSSE` 拆 SSE）只是**兜底**：上游没实现 `CallStream`、或明确拒绝流式（JoyCode 灰度虚拟 406）时才走
  - `executeChainStream`（`proxy/router.go`）对未实现 `StreamCaller` 的上游会跳过并记 `stream_unsupported` 日志，链里下一个顶上；整条链都没有流式上游时返回 `nil,"","",nil` 让 handler 回退伪流式
  - **加新上游务必实现 `CallStream`** —— 只实现 `Call` 会让流式请求退化成「等整段生成完再一次性吐出」，首字延迟等于完整生成耗时（WorkBuddy 曾因此慢 20-40s）
  - 各 `doCallStream` 都用 `bufio.Reader.Peek(256)` 做空流/非 SSE 校验。**注意 `Peek(n)` 会阻塞到攒够 n 字节**（实测：单 chunk ~78 字节、上游每 chunk 间隔 200ms 时，Peek 额外增加约 600ms 首字延迟）。这是所有上游共有的固定成本，量级远小于伪流式，但写这类测试时假网关的第一段必须 ≥256 字节，否则卡住的是 Peek 而不是被测逻辑
- **WorkBuddy 特殊性**：上游只认 `stream:true`（非流式返回 `code:11101`）。`CallStream` 直传 SSE；`Call`（非流式客户端请求、模型测评用）内部读 SSE 经 `aggregateOpenAISSE` 聚合成完整 OpenAIResponse JSON，对上层透明。两条路径共用 `buildChatRequest` 构造请求 —— 那批 `delete`（`stream_options` 会触发 11140）与 `X-Requested-With`/`X-User-Id`+`X-Userinfo` 头是对齐官方 CLI 绕风控的，**漏一项会被识别为非官方客户端**；也是唯一不能注入 `stream_options.include_usage` 的上游（其余四个都注入以拿 usage，WorkBuddy 缺 usage 时由 `sse_stream.go` 按 rune 估算兜底）
- 模型 id 隔离：WorkBuddy 模型用 `wb/` 前缀（如 `wb/glm-5.0`），避免与 DevEco 的 glm-5.1 重名；`ResolveUpstream` 识别前缀路由，发上游前 `stripWbPrefix` 还原
- service.Core 实现 proxy.EventLogger，记录日志 + 通过 `application.Get().Event.Emit` 推送事件到前端

## 模型目录（内置上游模型清单动态化）
`proxy/catalog.go` 是**唯一**的模型真相来源，三层叠加：**种子（`proxy/models.go` 四个硬编码 slice）+ 实时拉取 + SQLite 回读**。
- **并发**：`Catalog` 构建后不可变，整体经 `atomic.Pointer` 换指针（`SetCatalog`），热路径读一次原子 Load、无锁。`ProviderModels` 同样改成了原子快照（`SetProviderModels` 写 / `ProviderModelList()` 读）—— 别再直读包级 slice，那是 data race
- **访问器**：`LookupModel(id)` / `CatalogModelsByUpstream(up)` / `CatalogSnapshot()`。`ResolveModel`/`IsKnownModel`/`ResolveUpstream`/`ActualUpstreamModel`/`modelContextLimit` 签名不变，内部全改走快照，**不要再新增直读 `*ModelByID` 静态 map 的代码**
- **`CatalogModel.WireName`** 是「发往上游 base_url 的真实 model 名」的唯一真相，只经 `ActualUpstreamModel` 消费。DevEco 内部 id 与 wire 名不同（`glm-5.1` ↔ `GLM-5.1`）；`MergeModels` 必须在调 `applyLocalMeta` **之前**把 `ModelInfo.Wire` 填好（该函数会覆写 `mi.ID`）
- **重名优先级**固定 workbuddy → opencode → deveco → joycode（`upstreamOrder`），先到先得，必须与旧 `ResolveModel` 查找顺序一致；动态条目不能劫持其它上游的种子模型
- **数据流**：启动 `service.LoadCatalogFromDB(cfgSvc)`（main.go，须在代理开始服务前）→ 后台 `service.StartCatalogRefresh` 每 6 小时 → `refreshCatalog` 拉四上游 → 落库 → 重建快照 → emit `models:change`
- **拉取失败的上游不写库**，保留上次成功结果 —— 否则一次网络抖动就清空该上游整个目录
- **`models` 表行被 `logs.model_id`/`used_model_id` 外键引用**，上游下线的模型只能置 `available=0`，**绝不能 DELETE**（会让历史日志与用量统计断链，`db/catalog_test.go` 守这条）
- `db.SyncUpstreamCatalog` 是**全量覆盖**语义（能把 context 改小、把能力位关掉）；旧的 `db.UpsertModel` 只能升不能降，别拿来做目录同步。落库时跳过 `""` 和 `"auto"`（对齐 `db/logstore.go: upsertModelInTx`）
- `db` 包 import `proxy`，故 **`proxy` 不能 import `db`**；目录必须在 `service`/`main` 装配后注入 proxy
- 加列走 `db.addColumns`（`PRAGMA table_info` 探测 + `ALTER TABLE`，幂等），本项目**没有版本化迁移框架**
- WorkBuddy 无 `/models` 端点（`upstream/workbuddy.go: FetchModels` 主动返回 error），只用种子白名单，UI 标「本地」
- 目录编排函数刻意写成**自由函数**（`service.LoadCatalogFromDB`/`service.StartCatalogRefresh`）而非 ConfigService 方法 —— Wails 会把 service 的所有导出方法暴露到前端 bindings，内部逻辑不该泄漏进去

## 构建与测试
```bash
wails3 dev                    # 开发热重载（前端 + Go 一起）
wails3 build                  # 桌面产物 bin/switch-dev
wails3 task build:server      # 无 GUI 纯服务模式（产物 bin/switch-dev-server）
make build-binaries V=x.y.z   # 全平台裸二进制到 dist/（发布/自动更新用）
make test                     # go test 全部包（已排除 build/ 模板目录）
go test -race ./proxy/ ./db/ ./service/   # 目录快照有并发读写，改动这几个包务必带 -race
go test ./config/... -run TestUARuleDefaultTarget -v   # 跑单个包/单个测试
gofmt -w <file>.go            # 或 make fmt（排除 frontend/node_modules 与 bindings）
(cd frontend && npx tsc --noEmit)   # 前端类型检查；UI 改动后务必跑
```
改了 Go service 签名/结构体后需重新生成前端绑定：`wails3 generate bindings -ts -d frontend/bindings ./...`（生成的 `frontend/bindings/**` 勿手改）。
前端是 React + TS + Tailwind v4（包管理器 npm，`frontend/package.json`），通过 Wails 生成的 bindings 调用后端 service。

## 其它核心子系统
- **providerapi（第三方供应商 + 凭据保险库）**：`providerapi/`。接入任意 OpenAI/Anthropic 兼容 API；`vault.go` 用 argon2id 派生 KEK + AES-256-GCM 加密 API Key 落 `credentials.json`，未设主密码时随机 DEK 存入系统钥匙串（`keystore/`）无感解锁；支持主密码 + 24 位恢复码、闲置锁定、`.sds` 加密分享文件（`share.go`）；`monitor.go` 每 5 分钟后台健康探测，不健康模型在降级链中自动降权。供应商 id 形如目录 slug（`groq`）或 `custom-xxxxxx`，由本子系统动态管理，config 层无法静态枚举——故 `config.isValidUpstream` 对非内置四上游的非空 id 一律放行，运行时 `pickUpstream` 找不到则安全跳过。
- **db（SQLite 持久化，modernc.org/sqlite 纯 Go 驱动）**：`db/`。`db.Open()` 打开 `switch-dev.db`，`migrate.go` 建表。日志 `logs` 与统计 `usage_stats`（按小时桶聚合，`usagestats.go`，清空 logs 不影响统计）解耦；另有 `modelstore`/`sourcestore`/`upstreamstore` 做维度归一。日志写入走异步：`proxy.Server.recordLog -> service.Core.RecordLog -> db.InsertLog + UpsertUsageStat`。
- **上游开关**：`Config.Upstreams map[string]UpstreamSettings`（key=upstream/provider id，缺省视为启用，保证升级非破坏）。停用时该上游下所有模型在 `executeChain*` 中被直接跳过；`IsUpstreamEnabled()` 线程安全读取。
- **autostart / relaunch / logfile / pricing**：`autostart/` 跨平台开机自启（macOS LaunchAgent、Windows 注册表、Linux XDG）；`relaunch_{darwin,linux,windows}.go` 自更新后用 `setsid` 等方式脱离进程组重启新进程；`logfile/` 把控制台日志按天落地 + gzip 轮转；`pricing/` 内置费率库 + 估算费用。

## 自动更新机制
- 版本号来源：`build/config.yml` 的 `info.version`，构建时 `make sync-version` 同步到 `version/config.yml`（Go embed 读取）
- 检测时机：**启动后延迟 3s 首次检查 + 每 6 小时周期检查**（`main.go: startUpdateCheck`），发现新版本推 `update:available` 事件；前端 UpdatePanel 也提供「立即检查」手动触发
- 检测逻辑（`updater/github.go`）：GET `https://api.github.com/repos/{Owner}/{Repo}/releases/latest`，比较 `tag_name`（去 v 前缀）与当前版本，仅看 **non-prerelease 的最新 release**
- **更新分级**（`UpdateInfo.Critical`）：major 或 minor 段变化（如 0.0.x → 0.1.0）= 强制更新，前端不显示「稍后再说」；仅 patch 段变化（0.0.3 → 0.0.4）= 可选更新，可忽略
- 资产名匹配（`assetName()` 硬编码）：`switch-dev-darwin-arm64` / `switch-dev-darwin-amd64` / `switch-dev-windows-amd64.exe`（+ linux 运行时检测 `switch-dev-linux-amd64`，但 `make build-binaries` 暂不构建 linux），**发布时资产文件名必须精确匹配**
- 下载应用（`updater/updater.go`）：`downloadAsset` 下载到临时文件（临时文件由 `ApplyUpdate` 用完后清理，**不能在 downloadAsset 里 defer Remove**）-> `github.com/minio/selfupdate` 原子替换运行中二进制 -> 提示重启
- changelog：`UpdateInfo.Notes` 取自 GitHub Release body，由 `make release` 从 `CHANGELOG.md` 自动提取对应版本章节；前端 UpdatePanel 展示
- 配置：`Config.AutoUpdate`（Enabled/Provider/GitHub{Owner,Repo,Token}/UpdateURL/Channel），默认 `rosanruan/switch-dev` 公开仓库，无需 Token

## 发布流程（一键）
```bash
# 1. 日常开发时把改动记进 CHANGELOG.md 的 [Unreleased] 章节
#    忘记记录可用 make changelog-auto 从 commit 生成草稿再润色
# 2. 定版：[Unreleased] -> [0.0.5] + 今天日期，并新开空 [Unreleased]
make changelog-release V=0.0.5
# 3. 更新 build/config.yml 的 info.version 为 0.0.5
# 4. 一键发布
make deploy V=0.0.5   # 构建 -> 推码 -> 打 tag -> 创建 Release -> 上传全部产物
```
deploy 链：`build-binaries -> dist -> push -> tag -> release -> upload`
- `build-binaries`：3 个裸二进制（darwin arm64/amd64 + windows amd64），自动更新用
- `dist`：`dmg-universal`（lipo 合并双架构 -> Universal DMG）+ `nsis`（Windows 安装包）
- `upload`：裸二进制（必需，缺失报错）+ DMG/NSIS 安装包（可选，存在则传）
- `release`：从 CHANGELOG.md 提取 `## [x.y.z]` 章节作为 release notes，**找不到该章节直接报错**
- 产物名必须匹配 `assetName()`，否则 updater 找不到对应平台资产
- 注意：tag 必须是 `vX.Y.Z` 格式，Release 不能是 prerelease（否则 `/releases/latest` 返回不到）

## CHANGELOG 维护
- 格式：Keep a Changelog + 语义化版本，分类小标题固定 `### 新增`/`### 修复`/`### 变更`/`### 移除`/`### 废弃`/`### 安全`
- `make changelog-auto`：扫描上个 tag 到 HEAD 的 commit，按 Conventional Commits 前缀（`feat:`/`fix:`/其他）归类插入 `[Unreleased]`，生成的是**草稿**需人工润色为面向用户的描述
- `make changelog-release V=x.y.z`：把 `[Unreleased]` 改名为 `[x.y.z] - 今天日期`，并在上面新开空 `[Unreleased]`；空章节或版本已存在会报错拒绝
- 章节标题必须是 `## [x.y.z]` 格式（`release` 用 awk 精确匹配提取），新版本在上、旧版本在下

## 验证过的能力
- /health 返回四上游凭据状态
- /v1/models 返回目录快照（纯种子时 31 个，含 13 个 `wb/*` 免费档；实时拉取/DB 回读后随上游变化）
- /v1/messages 非流式（auto 降级链）✓
- /v1/messages 流式（降级链 + 真流式直传，未实现 CallStream 的上游回退伪流式）✓
- /v1/messages 非流式 + 流式（`wb/glm-5.0`->WorkBuddy：流式直传 SSE，非流式走 `aggregateOpenAISSE` 聚合）✓
- /v1/chat/completions ✓
- 首页「今日输出速率」统计（加权 tok/s + 按模型排名）✓
- 凭据四上游全部有效（实测 2026-08-08）

## 与旧版关系
`../switch-free/` 是原 Node.js 单文件版（joycode-color-proxy.js）。本目录是 Go+GUI 重构，端口/路由/格式完全兼容，可 drop-in 替换。逆向过程文档在旧版目录。

## 降级链关键文件
| 文件 | 职责 |
|---|---|
| `config/config.go` | Config 数据结构（AutoChain/ManualFallbacks/GlobalFallback/UARules/UAGlobalFallback/Upstreams/Presets）、默认值、Validate 校验、加载/保存、copyChain/copyFallbacks/copyUARules 深拷贝助手 |
| `config/resolver.go` | Resolve — 按模式分派；expandAutoChain/expandManual/expandUALocked 链展开；matchUARule（UA 子串匹配 → Mappings 精确映射 → DefaultTarget 默认目标） |
| `config/manager.go` | Manager 线程安全包装、SaveConfig/ResetConfig、方案 CRUD（SavePreset/ApplyPreset/DeletePreset/RenamePreset） |
| `config/*_test.go` | 方案快照语义、Clone 隔离、UA 默认目标解析等守卫测试 |
| `proxy/router.go` | executeChain/executeChainStream — 降级链遍历执行、ModelRef/ConfigResolver 定义、上游错误友好归类 |
| `proxy/catalog.go` | 模型目录注册表 —— Catalog/CatalogModel、atomic.Pointer 快照交换、BuildCatalog/SetCatalog/SeedCatalogEntries、LookupModel/CatalogSnapshot/ProviderModelList |
| `proxy/models.go` | 四个种子白名单 slice + ResolveModel/ResolveUpstream/ActualUpstreamModel（内部走目录快照）、`wb/` 前缀路由 |
| `proxy/models_merge.go` | MergeModels — 实时结果 + 种子元数据合并、`ModelInfo.Wire` 填充、拉取失败回退种子 |
| `db/modelstore.go` | CatalogEntry、SyncUpstreamCatalog（全量覆盖 + 软下线）、ListCatalog/ListCatalogByUpstream 回读 |
| `db/db.go` | migrate() 建表 + addColumns 幂等加列助手（无版本化迁移框架） |
| `service/catalog.go` | 目录编排 —— LoadCatalogFromDB（启动回读）/StartCatalogRefresh（6h 后台）/refreshCatalog（拉取→落库→重建快照→emit models:change）、fetchUpstreamModels 共享并发骨架 |
| `service/config_service.go` | 前端-后端桥接，SaveConfig/GetConfig/GetAvailableModels（10min 缓存）/RefreshModels + 四个方案方法 |
| `frontend/src/components/Settings.tsx` | 降级链编辑 UI（auto 链、手动降级链、UA 规则含默认目标、兜底选择器）、模型来源徽章（三种模式可见）、配置项改动自动保存（debounce 600ms）、`modeFingerprint` 偏离检测、完整配置 JSON 实时预览 |
| `frontend/src/components/Models.tsx` | 模型总览表 + 「🔄 刷新模型」+ `models:change` 订阅 + 实时/本地/免费来源徽章 |
| `frontend/src/components/PresetSwitcher.tsx` | 方案下拉（切换/重命名/删除）+ 保存方案弹窗 |
