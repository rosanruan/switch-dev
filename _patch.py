#!/usr/bin/env python3
"""Patch source files to add AllowRemoteAccess feature."""
import re

# ====== 1. config/config.go: Clone() and Update() ======
with open("config/config.go", "r") as f:
    content = f.read()

# Clone() - add AllowRemoteAccess
content = content.replace(
    'AuthEnabled:      c.AuthEnabled,\n\t\t\tAutoStart:        c.AutoStart,',
    'AuthEnabled:      c.AuthEnabled,\n\t\t\tAllowRemoteAccess: c.AllowRemoteAccess,\n\t\t\tAutoStart:        c.AutoStart,'
)

# Update() - add AllowRemoteAccess
content = content.replace(
    'c.AuthEnabled = newCfg.AuthEnabled\n\t\tc.AutoStart = newCfg.AutoStart',
    'c.AuthEnabled = newCfg.AuthEnabled\n\t\tc.AllowRemoteAccess = newCfg.AllowRemoteAccess\n\t\tc.AutoStart = newCfg.AutoStart'
)

with open("config/config.go", "w") as f:
    f.write(content)

print("✅ config/config.go patched")

# ====== 2. main.go: read config for host ======
with open("main.go", "r") as f:
    content = f.read()

content = content.replace(
    'server := proxy.NewServer(jyUp, deUp, ocUp, wbUp, mcUp, "0.0.0.0", serverPort)',
    '\t\tproxyHost := "127.0.0.1"\n\t\tif cfgMgr.Get().AllowRemoteAccess {\n\t\t\tproxyHost = "0.0.0.0"\n\t\t}\n\t\tserver := proxy.NewServer(jyUp, deUp, ocUp, wbUp, mcUp, proxyHost, serverPort)'
)

with open("main.go", "w") as f:
    f.write(content)

print("✅ main.go patched")

# ====== 3. service/config_service.go: extend restart for host ======
with open("service/config_service.go", "r") as f:
    content = f.read()

# SaveConfig: detect AllowRemoteAccess change
old_save = '''func (s *ConfigService) SaveConfig(cfg *config.Config) error {
\toldPort := s.mgr.Get().Port
\tif err := s.mgr.SaveConfig(cfg); err != nil {
\t\treturn err
\t}
\t// 端口变了：停旧代理，用新端口重启
\tif cfg.Port != oldPort {
\t\tif err := s.restartProxyOnPort(cfg.Port); err != nil {
\t\t\treturn fmt.Errorf("配置已保存，但代理切换端口失败: %w", err)
\t\t}
\t}'''

new_save = '''func (s *ConfigService) SaveConfig(cfg *config.Config) error {
\toldCfg := s.mgr.Get()
\tif err := s.mgr.SaveConfig(cfg); err != nil {
\t\treturn err
\t}
\t// 端口或远程访问变了：停旧代理，用新配置重启
\tportChanged := cfg.Port != oldCfg.Port
\thostChanged := cfg.AllowRemoteAccess != oldCfg.AllowRemoteAccess
\tif portChanged || hostChanged {
\t\tif err := s.restartProxy(cfg.Port, cfg.AllowRemoteAccess); err != nil {
\t\t\treturn fmt.Errorf("配置已保存，但代理重启失败: %w", err)
\t\t}
\t}'''

content = content.replace(old_save, new_save)

# Replace restartProxyOnPort with restartProxy (also used in ResetConfig)
old_reset = '''func (s *ConfigService) ResetConfig() error {
\toldPort := s.mgr.Get().Port
\tif err := s.mgr.ResetConfig(); err != nil {
\t\treturn err
\t}
\tnewPort := s.mgr.Get().Port
\tif newPort != oldPort {
\t\tif err := s.restartProxyOnPort(newPort); err != nil {
\t\t\treturn fmt.Errorf("配置已重置，但代理切换端口失败: %w", err)
\t\t}
\t}'''

new_reset = '''func (s *ConfigService) ResetConfig() error {
\toldCfg := s.mgr.Get()
\tif err := s.mgr.ResetConfig(); err != nil {
\t\treturn err
\t}
\tnewCfg := s.mgr.Get()
\tportChanged := newCfg.Port != oldCfg.Port
\thostChanged := newCfg.AllowRemoteAccess != oldCfg.AllowRemoteAccess
\tif portChanged || hostChanged {
\t\tif err := s.restartProxy(newCfg.Port, newCfg.AllowRemoteAccess); err != nil {
\t\t\treturn fmt.Errorf("配置已重置，但代理重启失败: %w", err)
\t\t}
\t}'''

content = content.replace(old_reset, new_reset)

# Replace restartProxyOnPort function with restartProxy
old_func = '''// restartProxyOnPort 停止当前代理，用新端口重启
func (s *ConfigService) restartProxyOnPort(port int) error {
\tif s.core == nil || s.core.server == nil {
\t\treturn nil
\t}
\ts.core.server.Stop()
\ts.core.server.Port = port
\treturn s.core.server.Start()
}'''

new_func = '''// restartProxy 停止当前代理，用新端口/新 host 重启
func (s *ConfigService) restartProxy(port int, allowRemote bool) error {
\tif s.core == nil || s.core.server == nil {
\t\treturn nil
\t}
\ts.core.server.Stop()
\ts.core.server.Port = port
\tif allowRemote {
\t\ts.core.server.Host = "0.0.0.0"
\t} else {
\t\ts.core.server.Host = "127.0.0.1"
\t}
\treturn s.core.server.Start()
}'''

content = content.replace(old_func, new_func)

with open("service/config_service.go", "w") as f:
    f.write(content)

print("✅ service/config_service.go patched")

# ====== 4. dashboard.tsx: update display for remote access ======
with open("frontend/src/components/Dashboard.tsx", "r") as f:
    content = f.read()

# Add getLANIP helper after the component function definition
content = content.replace(
    'export default function Dashboard({ proxy, creds, config, onGoCredentials }: Props) {',
    '''function getLANIP(): string {
  const hostname = window.location.hostname;
  if (hostname !== "127.0.0.1" && hostname !== "localhost" && hostname !== "") return hostname;
  return "0.0.0.0";
}

export default function Dashboard({ proxy, creds, config, onGoCredentials }: Props) {'''
)

# Replace hardcoded 127.0.0.1 in display with dynamic host
# Line 236: display host
content = content.replace(
    'http://127.0.0.1:{proxy?.port ?? 8787}',
    'http://{config?.allowRemoteAccess ? getLANIP() : "127.0.0.1"}:{proxy?.port ?? 8787}'
)

# Line 238: copy button text
content = content.replace(
    'CopyButton text={`http://127.0.0.1:${proxy?.port ?? 8787}`} />',
    'CopyButton text={`http://${config?.allowRemoteAccess ? getLANIP() : "127.0.0.1"}:${proxy?.port ?? 8787}`} />'
)

# Line 243: display host for OpenAI endpoint
content = content.replace(
    'http://127.0.0.1:{proxy?.port ?? 8787}/v1',
    'http://{config?.allowRemoteAccess ? getLANIP() : "127.0.0.1"}:{proxy?.port ?? 8787}/v1'
)

# Line 245: copy button text for OpenAI
content = content.replace(
    'CopyButton text={`http://127.0.0.1:${proxy?.port ?? 8787}/v1`} />',
    'CopyButton text={`http://${config?.allowRemoteAccess ? getLANIP() : "127.0.0.1"}:${proxy?.port ?? 8787}/v1`} />'
)

with open("frontend/src/components/Dashboard.tsx", "w") as f:
    f.write(content)

print("✅ Dashboard.tsx patched")

# ====== 5. Settings.tsx: add toggle for remote access ======
with open("frontend/src/components/Settings.tsx", "r") as f:
    content = f.read()

# Update the hint text on line ~647
content = content.replace(
    'cc-switch 的 baseURL 要对应此端口（http://127.0.0.1:端口）。保存后代理会重启切换端口。',
    'cc-switch 的 baseURL 要对应此端口。开启远程访问后局域网内其他设备也可访问。'
)

# Update the display text on line ~662
content = content.replace(
    '监听 http://127.0.0.1:{cfg.port || 8787}',
    '监听 {cfg?.allowRemoteAccess ? "0.0.0.0" : "127.0.0.1"}:{cfg.port || 8787}'
)

# Find the closing div of the port section and add the toggle
# The port section ends with </section> followed by {/* 接入 apiKey */}
# We need to add the toggle before the closing </section>
old_port_end = '''              </button>
            </div>
          </section>

        {/* 接入 apiKey */}'''

new_port_end = '''              </button>
            </div>
            {/* 允许远程访问开关 */}
            <div className="flex items-center justify-between mt-4 pt-4 border-t border-[var(--color-border)]">
              <div>
                <h2 className="font-semibold text-sm">允许远程访问</h2>
                <p className="text-xs text-[var(--color-text-dim)] mt-0.5">
                  开启后监听 0.0.0.0，局域网内其他设备可通过本机 IP 访问代理。修改后自动热重启代理服务。
                </p>
              </div>
              <label className="inline-flex items-center cursor-pointer shrink-0">
                <input
                  type="checkbox"
                  checked={cfg?.allowRemoteAccess ?? false}
                  onChange={async (e) => {
                    const next = { ...cfg, allowRemoteAccess: e.target.checked };
                    setCfg(next);
                    try {
                      const cur = await ConfigService.GetConfig();
                      await ConfigService.SaveConfig({ ...cur, allowRemoteAccess: e.target.checked });
                      flash("ok", e.target.checked ? "已开启远程访问，代理已热重启" : "已关闭远程访问，仅限本机访问，代理已热重启");
                    } catch {
                      flash("err", "保存失败");
                    }
                  }}
                  className="sr-only peer"
                />
                <div className="w-10 h-5 bg-[var(--color-surface-2)] rounded-full peer-checked:bg-[var(--color-primary)] relative transition-colors after:content-[''] after:absolute after:top-0.5 after:left-0.5 after:bg-white after:rounded-full after:h-4 after:w-4 after:transition-all peer-checked:after:translate-x-5" />
              </label>
            </div>
          </section>

        {/* 接入 apiKey */}'''

content = content.replace(old_port_end, new_port_end)

with open("frontend/src/components/Settings.tsx", "w") as f:
    f.write(content)

print("✅ Settings.tsx patched")
print("\n🎉 All changes applied!")
