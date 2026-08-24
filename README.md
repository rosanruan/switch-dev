# ⚡ Switch Dev

<div align="center">

**不生产 Token，只做 Token 的搬运工。**

一个本地代理 + 桌面管理面板，复用你已安装的 AI 编程工具登录态，
把各家免费模型额度统一搬运成标准 Anthropic / OpenAI 接口，喂给 Claude Code、Codex、cc-switch。

`本地运行` · `复用登录态` · `统一端口` · `多上游自动降级` · `用量可观测` · `MIT 开源`

</div>

---

## 🎯 它解决什么问题

你机器上装着的 JoyCode、DevEco Code、WorkBuddy，后台都自带模型额度——
京东云的 JoyAI / Kimi / GLM、华为的 GLM-5.1、腾讯 CodeBuddy 的 GLM / MiniMax / Kimi / DeepSeek / 混元。

但这些额度是**登录态**：请求要动态签名、要注入业务字段、Token 会过期要刷新，
没法直接填进 Claude Code。

Switch Dev 在本地把这些脏活累活全部包掉：

> **装好并登录一个工具 → 启动 Switch Dev → 客户端指向 `127.0.0.1:8787` → 开跑。**

它本身不提供任何模型、不签发任何 Token、不经过任何中转服务器——
**你的请求从本机出发，用你自己的登录态，直达各家官方接口。**

---

## 🔐 两个核心：搬运 Token，更守住密钥

Switch Dev 的设计围绕两件事：**把各家登录态搬成统一接口**，以及**让搬运过程中的每一把密钥都不出本机**。

| 密钥场景 | 怎么守 |
|---|---|
| 内置上游登录态 | 启动时从各工具本地存储**动态读取**，不硬编码、不上传、不落我们的任何文件 |
| 自定义供应商 API Key | 以 **AES-256-GCM** 加密写入 `credentials.json`；DEK 经 **argon2id** 派生的 KEK 包裹 |
| 主密码 / 自动加密 | 不设主密码也自动加密——随机 DEK 包裹后存入**系统钥匙串**（macOS Keychain / Windows Credential Manager），无感解锁 |
| 恢复码 | 设置主密码时生成 24 位一次性恢复码，脱离钥匙串也能重置 |
| 闲置锁定 | 设了主密码后，5 分钟无操作自动锁定供应商界面，代理继续跑但 Key 不可见 |
| `.sds` 分享 | 整个文件用 argon2id + AES-GCM 加密；带 Key 分享强制设密码，不带 Key 也至少内置密钥混淆；密码可自定义或随机 6 位 |
| 错误 / 日志 | 错误响应里的 `apiKey/token/password/authorization` 等字段自动掩码，日志各截断 4KB |
| 网络边界 | 代理只监听 `127.0.0.1`，默认开启接入 apiKey 鉴权，不内置任何外联上报 |

写盘顺序也做了防呆：**首次加密时先确认钥匙串写入成功才落盘**，清除主密码时若钥匙串写入失败会回滚磁盘——避免出现"用无人知晓的随机密码锁住了凭据"的情况。

---

## ✨ 特色一览

| | 为什么特别 |
|---|---|
| 🔐 **凭据零硬编码** | 启动时从各工具本地存储动态读取登录态，失效自动恢复，重登免重启 |
| 🧩 **四上游统一抽象** | JoyCode / DevEco / OpenCode / WorkBuddy 实现同一套 `Upstream` 接口，路由层无差别遍历 |
| 🪜 **三级降级链** | auto 优先级链、manual 按模型配链、UA 路由按客户端 User-Agent 分流；每条链末尾都有全局兜底 |
| 🌊 **流式全适配** | 上游统一非流式时，代理在本地把完整响应拆成 SSE；WorkBuddy 反向强制流式，内部聚合后再下发 |
| 🛰️ **自带供应商体系** | 可接入任意 OpenAI / Anthropic 兼容 API；Key 经 argon2id + AES-GCM 加密落盘，支持主密码/恢复码/闲置锁定，`.sds` 加密文件可在设备间安全分享 |
| 🖥️ **代理即面板** | HTTP 服务与 Wails v3 桌面 GUI 共存，关窗口后台常驻，也可无界面纯服务运行 |
| 📊 **本地可观测** | 每次请求的 token、缓存命中、费用、速率全部记录，按 agent / 模型 / 时间维度统计 |

---

## 🚀 快速开始

### 1. 装一个工具并登录

Switch Dev 只搬运你已有的登录态，**不代注册、不代登录**。请先在对应工具里完成登录，并确认该工具本身能正常对话，再启动 Switch Dev。

| 工具 | 安装 | 登录 / 前置条件 |
|------|------|----------------|
| **JoyCode** | [官网下载](https://joycode.jd.com) | 客户端扫码登录，确认 JoyCode 内能正常发起对话；⚠️ **使用时需保持 JoyCode 客户端在线**（见下方说明） |
| **DevEco Code** | `npm i -g @deveco/deveco-code` | ⚠️ 需先[注册华为开发者账号并完成实名认证](https://developer.huawei.com)，再 `deveco auth login` 登录；确认 DevEco Studio / DevEco Code 内可正常调用模型 |
| **WorkBuddy** | [官网下载](https://workbuddy.app) | 客户端登录，确认 WorkBuddy 内能正常发起对话 |

> 装一个就能用；四个全装就是「全家桶」。首次启动会自动探测安装状态并引导登录。
> 若某上游单独使用时就无法调用（未登录、未实名、额度耗尽），Switch Dev 里同样会失败——请先在原工具里排查。

> **JoyCode 特别提示：调用时需保持 JoyCode 官方客户端打开并登录。**
> JoyCode 的推理网关采用「活跃会话」放行机制：官方客户端运行时会维持一条到京东后端的 WebSocket 长连接（约 25 秒心跳保活），网关据此判定账号有活跃客户端会话才放行 `chat_completions`。
> 若 JoyCode 客户端未运行，Switch Dev 用本地静态凭据发起的独立请求会被网关拒绝，报 `AI_GRAY_ACCESS_DENIED`（访问受限，请联系管理员开通）。
> 这不是 Switch Dev 的 bug，也无法通过修改请求绕过——**请在使用 JoyCode 上游时保持其官方客户端在线**；未在线时建议在 auto 降级链中于 JoyCode 之后配置 DevEco 或已验证的免费供应商兜底，避免中断对话。

### 2. 启动

```bash
./bin/switch-dev        # macOS / Linux；Windows 双击 .exe
```

代理服务 `127.0.0.1:8787` 立即可用，托盘图标常驻（关窗口不退出）。

### 3. 接入客户端

**Claude Code**（`~/.claude/settings.json`）：
```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://127.0.0.1:8787",
    "ANTHROPIC_AUTH_TOKEN": "sk-switchdev",
    "ANTHROPIC_MODEL": "auto"
  }
}
```
·
**cc-switch**：baseURL `http://127.0.0.1:8787`，apiKey 任意，模型 `auto`。

> `auto` 按你配置的优先级链自动选模型；也可指定具体模型（如 `glm-5.1`、`wb/hy3`）。

---

## 🧠 核心机制

### 四上游适配器

| 上游 | 凭据来源 | 特殊处理 |
|------|---------|---------|
| **JoyCode** | `state.vscdb`（纯 Go `modernc.org/sqlite` 读取，无需外部 sqlite3） | Color 网关签名 + 业务字段注入 + 401 重试 |
| **DevEco** | 三层 AES-256-GCM 解密 | Bearer + Chat-Id + JWT 自动刷新 |
| **OpenCode** | 明文 `auth.json` | 标准 OpenAI 兼容 |
| **WorkBuddy** | 明文 info + refreshToken 续期 | 上游强制 `stream:true`，代理聚合 SSE 后再下发；模型用 `wb/` 前缀隔离重名 |

路径解析跨平台且单一真相（macOS `~/Library/Application Support`、Linux `~/.config`、Windows `%APPDATA%`），
并支持 `JOYCODE_VSCDB` / `WORKBUDDY_INFO_PATH` / `OPENCODE_AUTH_PATH` 等环境变量覆盖。

### 三级路由 + 降级

- **auto**：按优先级链 `[]AgentModels` 依次尝试，凭据失效自动跳过，任意成功即返回，末尾追加全局兜底（自动去重）
- **manual**：为每个模型单独配降级链，未命中走全局兜底
- **ua**：按请求 `User-Agent`（Claude Code / Codex）分流到不同模型，可独立配置兜底
- **方案（Preset）**：把整套路由配置存为命名快照，一键切换；快照是冻结的，切换后继续编辑不回写，需同名保存才更新

### 流式处理

- 上游统一走非流式调用，下游要流式时，代理在本地把完整 JSON 拆成标准 SSE 事件（伪流式）
- **WorkBuddy 例外**：非流式返回 `code:11101`，上游强制 `stream:true`，`aggregateOpenAISSE` 把 chunk 聚合成完整响应后，再复用非流式处理逻辑
- 完整支持 Anthropic `/v1/messages`、OpenAI `/v1/chat/completions`、`/v1/responses`，含 SSE 错误透传

### 供应商生态

除了四家内置上游，还能接入**任意 OpenAI / Anthropic 兼容 API**：

- 凭据本地落盘，argon2id 派生 KEK 包裹 DEK，API Key 以 AES-GCM 密文存储
- 支持**主密码 + 恢复码**：主密码解锁查看，恢复码用于重置；自动加密模式下随机密码写入系统钥匙串无感解锁
- **`.sds` 加密分享**：勾选供应商导出为加密文件，可设自定义密码或随机 6 位码，对方导入即解密；支持覆盖 / 跳过 / 重命名解决 id 冲突
- 目录内置免费 / 注册送 Token 的供应商清单，一键拉取模型并批量测速

### 桌面体验

- 代理服务（`127.0.0.1:8787`）与 GUI 同进程共存，托盘常驻，关闭 / 最小化即后台运行
- 仪表盘聚合：今日 token / 费用 / 输出速率（加权 tok/s）、降级链状态、凭据健康度
- 无界面模式 `wails3 task build:server` 可纯 HTTP 部署
- 接入 apiKey 默认开启并随机生成，可一键开关（关闭后网关不鉴权）

---

## 🛠 构建

依赖：Go 1.25+、Node.js、wails3 CLI。

```bash
# 安装 wails3 CLI
go install github.com/wailsapp/wails/v3/cmd/wails3@v3.0.0-alpha.74

wails3 build              # 桌面二进制 → bin/switch-dev
wails3 dev                # 开发热重载
wails3 task build:server  # 无 GUI 纯代理服务

# 改了 Go service 签名后重新生成前端绑定
wails3 generate bindings -ts -d frontend/bindings ./...

# 全平台裸二进制（自动更新用）
make build-binaries V=x.y.z
```

发布流程见 [CLAUDE.md](./CLAUDE.md) 与 `make help`，一键 `make deploy V=x.y.z`
完成构建 → 推码 → 打 tag → GitHub Release → 上传产物，支持 macOS Universal DMG 与 Windows NSIS。

---

## 📊 统计与费率

- 每次请求记录输入 / 输出 token、缓存命中 token、费用、耗时、状态
- 按天 / 周 / 月 / 自定义范围，按 agent 或模型维度聚合
- 缓存命中率双指标（请求命中 + token 命中），今日按小时、周 / 月按天趋势图
- 内置费率库，设置页可视化增删改查，改完即时生效
- 内置测速：对供应商模型批量基准测评，按 TPS 排名

---

## 🔄 自动更新

- 启动 3s 后首次检查，之后每 6 小时检查 GitHub Releases
- 语义化分级：major / minor 变化为强制更新，patch 为可选
- `github.com/minio/selfupdate` **原子替换**运行中二进制，下载失败不损坏原程序
- 资产名按平台精确匹配（`switch-dev-darwin-arm64` / `-amd64` / `-windows-amd64.exe`）

---

## 🏗 架构

```
Claude Code / Codex / cc-switch
   │  Anthropic /v1/messages   或   OpenAI /v1/chat/completions
   ▼
┌──────────── Switch Dev（Wails v3 · Go · React）────────────┐
│  前端面板：仪表盘 / 供应商 / 凭据 / 模型 / 统计 / 日志 / 设置  │
│       ↕ Wails 绑定 + 事件推送                                │
│  Go 后端                                                     │
│   ├ 代理核心：协议转换 / 签名 / SSE / auto·manual·UA 降级    │
│   ├ 上游适配器：JoyCode / DevEco / OpenCode / WorkBuddy      │
│   ├ 供应商：自定义接入 / argon2id 加密 / .sds 分享           │
│   └ 凭据：vscdb / AES 三层解密 / JWT 刷新 / 钥匙串           │
│                                                              │
│  独立 HTTP 服务 :8787（与 GUI 共存，可无界面运行）           │
└──────────────────────────────────────────────────────────────┘
   │           │           │           │
   ▼           ▼           ▼           ▼
京东云        华为云       OpenCode     腾讯云
(JoyCode)     (DevEco)     Zen          (WorkBuddy)
```

---

## 📂 项目结构

```
switch-dev/
├── main.go                # 入口：Wails app + 窗口 + 托盘 + 代理 + 更新
├── proxy/                 # 代理核心（协议转换 / SSE / 路由 / 鉴权）
├── upstream/              # 四上游适配器（统一 Upstream 接口）
├── providerapi/           # 自定义供应商 + argon2id 凭据库 + .sds 分享
├── creds/                 # 各工具登录态读取与解密
├── config/                # 运行配置（模式 / 降级链 / 方案 / 端口 / 鉴权）
├── pricing/               # 费率库（内置默认 + 可编辑）
├── service/               # Wails 服务层（暴露给前端）
├── updater/               # GitHub Releases + 原子自更新
├── paths/                 # 跨平台路径解析（单一真相）
├── version/               # 版本号（build/config.yml 同步）
├── frontend/              # React + TypeScript + Tailwind v4
└── build/                 # Wails 打包配置 + 图标
```

---

## 📂 数据目录

| 路径 | 内容 |
|------|------|
| `~/.config/switch-dev/config.json` | 运行配置（模式 / 降级链 / 方案 / 端口 / 鉴权）|
| `~/.config/switch-dev/credentials.json` | 自定义供应商凭据（argon2id 加密）|
| `~/.config/switch-dev/pricing.json` | 费率库 |
| `~/.config/switch-dev/switch-dev.db` | 请求日志与统计 |
| `~/.config/switch-dev/logs/` | 按天日志 |

---

## 🔒 安全说明

- 代理仅监听 `127.0.0.1:8787`，**切勿暴露公网**；需要局域网共用时务必自行加反代并开启 apiKey 鉴权
- 内置上游登录态只在调用时从各工具本地存储动态读取，不复制、不缓存到我们的配置目录
- 自定义供应商 API Key 以 argon2id 派生 KEK + AES-256-GCM 加密存储；主密码 / 恢复码只在本机出现
- 首次加密时先确认系统钥匙串写入成功才落盘，清除主密码时若钥匙串失败会回滚磁盘，避免随机密码丢失导致凭据锁死
- 分享文件 `.sds` 始终加密：含 Key 强制口令保护，不含 Key 也至少内置密钥混淆；密码可自定义或随机 6 位
- 日志记录请求 / 响应体（各截断 4KB），敏感字段（apiKey/token/password/authorization）自动掩码
- 接入 apiKey 可在设置中关闭；关闭后网关不鉴权，仅适合受信任的本地环境
- 唯二的外联是启动时检查 GitHub Releases 更新、拉取免费供应商目录，均不携带任何凭据

---

## ❓ FAQ

**Q：它自己提供模型吗？**
不。Switch Dev 不生产一个 Token，也不中转任何流量到第三方服务器。它只在你本机把你已登录工具的请求搬运到各家官方接口。

**Q：要花钱吗？**
复用的是你已登录工具自带的额度，不经过任何付费 API。费用统计是按内置费率表的**估算值**，真实消耗看各工具后台。

**Q：一个工具都不装能跑吗？**
能启动，但没有内置上游可用。装好任意一个并登录后自动恢复，无需重启；也可以在「供应商」页接入你自己的 API Key。

**Q：凭据存在哪？会上传吗？**
内置上游凭据只存在各工具自己的配置里，Switch Dev 调用时动态读取。自定义供应商的 API Key 加密存在本机 `~/.config/switch-dev/credentials.json`（argon2id + AES-GCM）。**没有任何凭据上传。**

**Q：主密码和自动加密是什么关系？**
即使你不设主密码，首次保存供应商时也会自动生成随机密钥加密落盘，密钥存入系统钥匙串，无感加解锁。主动设置主密码后，可用自己的密码解锁，并获得一个 24 位恢复码用于忘记密码时重置。

**Q：主密码忘了怎么办？**
用设置主密码时获得的恢复码重置。恢复码也丢失时，只能「销毁现有配置」，所有供应商和 Key 删除后重新添加。

**Q：支持流式吗？**
支持。上游非流式时代理本地拆 SSE；WorkBuddy 反向强制流式，内部聚合后再下发，客户端体验一致。

**Q：Claude Code 和 Codex 能同时用吗？**
能。分别走 Anthropic / OpenAI 入口，共用 8787 端口；UA 路由还能让两个客户端自动分流到不同模型。

**Q：为什么配了 auto 却总是降级到兜底模型？**
看「日志」页错误信息。常见原因：上游 token 过期（重登对应工具）、免费额度耗尽、模型名变更（设置里刷新模型）。链中凭据无效的上游会自动跳过。

**Q：改了端口连不上？**
客户端 baseURL 的端口要同步改，代理重启需一两秒。

**Q：apiKey 忘了 / 想重置？**
设置 → 通用 → 接入 apiKey 里「重新生成」，旧 key 立即失效。也可直接关闭鉴权（仅限可信本地环境）。

**Q：分享的 .sds 对方导入后无法调用？**
分享时若未勾「分享 API 密钥」，对方需自己编辑供应商填 Key 并重新测评。含密钥的分享确认密码输入正确。

**Q：分享导入的供应商能重新拉取模型吗？**
能。编辑时点「拉取模型」会实时请求该供应商的 `/models` 拿到全量模型，再测评加入，不局限于分享文件里带过来的几个。注意导入的供应商 Key 默认不回显，拉取前需在编辑框填入 Key。

**Q：上游接口变了怎么办？**
模型列表实时拉取，上游新增模型自动出现。签名 / 登录机制变更属于逆向适配问题，欢迎提 PR。

**Q：能给团队 / 公司共用吗？**
代码以 MIT 开源允许商用，但注意：代理默认只监听 `127.0.0.1`，局域网共用需自配反代并开 apiKey 鉴权；内置上游登录态绑定你个人账号，共用等于共享额度，请遵守各工具服务条款。

---

## 💬 反馈与参与

如果这个项目帮你省下了 API 账单，欢迎到 GitHub 给个 ⭐ Star，这是持续维护的最大动力。

- 🐛 遇到 Bug 或有功能建议：请提 [Issue](https://github.com/rosanruan/switch-dev/issues)
- 🔧 适配了新的上游、修了 bug、改进了文案：欢迎提 Pull Request
- 📖 更详细的使用说明见 [docs/usage.md](./docs/usage.md)

> 各上游接口随时可能变化，模型可用性与免费额度以官方为准。如果你发现某条链路失效，开 Issue 说明版本和现象即可，感谢一起把它打磨得更好。

---

## 📄 License

[MIT License](./LICENSE)

代码以 MIT 协议开源，可自由使用、修改、商用分发。
本项目不内置、不转授权任何第三方模型或 Token；
你接入的各 AI 编程工具及其额度，其权利归属与使用约束以对应工具的服务条款为准。
