import { useEffect, useState, useCallback } from "react";
import { System } from "@wailsio/runtime";
import { ProxyService, ConfigService, ProviderAPIService, UpdaterService } from "../bindings/switchdev/service";
import type {
  AllCredStatus,
} from "../bindings/switchdev/service/models";
import type { ProxyStatus, LogEntry } from "../bindings/switchdev/proxy/models";
import type { Config } from "../bindings/switchdev/config/models";
import { useWailsEvent } from "./hooks/useWailsEvent";
import Dashboard from "./components/Dashboard";
import Credentials from "./components/Credentials";
import Models from "./components/Models";
import Logs from "./components/Logs";
import Settings from "./components/Settings";
import UsageStats from "./components/UsageStats";
import Benchmark from "./components/Benchmark";
import ProviderAPI from "./components/ProviderAPI";
import ErrorBoundary from "./components/ErrorBoundary";

type Tab = "dashboard" | "providerapi" | "credentials" | "models" | "stats" | "logs" | "settings" | "benchmark";

const IS_MAC = System.IsMac();

export default function App() {
  const [tab, setTab] = useState<Tab>("dashboard");
  const [proxy, setProxy] = useState<ProxyStatus | null>(null);
  const [creds, setCreds] = useState<AllCredStatus | null>(null);
  const [logs, setLogs] = useState<LogEntry[]>([]);
  const [config, setConfig] = useState<Config | null>(null);
  const [loading, setLoading] = useState(true);

  // 全局更新弹窗状态
  const [updateInfo, setUpdateInfo] = useState<any>(null);
  const [updateProgress, setUpdateProgress] = useState<{ percent: number; message: string } | null>(null);
  const [updating, setUpdating] = useState(false);

  // 拉取仪表盘聚合数据 + 配置。后端会静默探测本地已安装的 upstream。
  const refreshDashboard = useCallback(async () => {
    try {
      const [d, cfg] = await Promise.all([
        ProxyService.GetDashboard(),
        ConfigService.GetConfig(),
      ]);
      if (d) {
        setProxy(d.proxy);
        setCreds(d.creds);
        setLogs(d.recentLogs?.filter((l): l is LogEntry => l !== null) ?? []);
      }
      if (cfg) setConfig(cfg);
    } catch (e) {
      console.error("拉取仪表盘失败", e);
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    refreshDashboard();
  }, [refreshDashboard]);

  // 配置变化时刷新（设置页保存/重置后触发）
  useWailsEvent("config:change", (data) => {
    setConfig(data as Config);
  });

  // 托盘菜单"功能"子菜单切换主界面 tab（"保存方案..."入口也走 settings）
  useWailsEvent("navigate:tab", (data) => {
    const validTabs: Tab[] = ["dashboard", "providerapi", "credentials", "models", "stats", "logs", "benchmark", "settings"];
    const key = data as Tab;
    if (validTabs.includes(key)) {
      setTab(key);
    }
  });

  // 订阅事件
  useWailsEvent("proxy:status", (data) => {
    setProxy(data as ProxyStatus);
  });
  useWailsEvent("cred:change", (data) => {
    setCreds(data as AllCredStatus);
  });
  useWailsEvent("log:new", (data) => {
    const entry = data as LogEntry;
    setLogs((prev) => [entry, ...prev].slice(0, 200));
  });

  // 全局更新弹窗事件
  useWailsEvent("update:available", (data) => {
    setUpdateInfo(data);
  });
  useWailsEvent("update:progress", (data) => {
    const s = data as { state: string; percent: number; message: string };
    if (s.state === "restarting" || s.state === "done") {
      setUpdateProgress({ percent: 100, message: s.message || "更新完成" });
    } else if (s.state === "error") {
      setUpdateProgress(null);
      setUpdating(false);
    } else {
      setUpdateProgress({ percent: s.percent || 0, message: s.message || "下载中..." });
    }
  });

  const skipVersion = async () => {
    if (!updateInfo) return;
    try {
      await UpdaterService.SetSkippedVersion(updateInfo.version);
    } catch (e) {
      console.error("跳过版本失败", e);
    }
    setUpdateInfo(null);
  };

  const applyUpdate = async () => {
    if (!updateInfo) return;
    setUpdating(true);
    setUpdateProgress({ percent: 0, message: "开始下载..." });
    try {
      await UpdaterService.ApplyUpdate(updateInfo);
    } catch (e) {
      console.error("更新失败", e);
      setUpdating(false);
      setUpdateProgress(null);
    }
  };

  const navItems: { key: Tab; label: string; icon: string }[] = [
    { key: "dashboard", label: "仪表盘", icon: "📊" },
    { key: "providerapi", label: "供应商", icon: "🌐" },
    { key: "credentials", label: "凭据", icon: "🔑" },
    { key: "models", label: "模型", icon: "🤖" },
    { key: "stats", label: "统计", icon: "📈" },
    { key: "logs", label: "日志", icon: "📋" },
    { key: "benchmark", label: "测评", icon: "🏁" },
    { key: "settings", label: "设置", icon: "⚙️" },
  ];

  return (
    <div className="flex flex-col h-screen w-screen overflow-hidden">
      {/* macOS 隐藏标题栏：留 50px 不可见拖动区给红绿灯按钮；Windows/Linux 有原生标题栏，不留 */}
      {IS_MAC && <div className="h-[50px] flex-shrink-0 bg-[var(--color-surface)]" />}

      {/* 顶部栏：应用名（大字号）+ 横排导航 tab */}
      <header className="h-[56px] flex-shrink-0 flex items-center gap-6 px-6 bg-[var(--color-surface)] border-b border-[var(--color-border)]">
        <button
          type="button"
          onClick={() => ProviderAPIService.OpenURL("https://github.com/rosanruan/switch-dev")}
          title="在 GitHub 上查看项目"
          className="leading-tight flex items-baseline gap-2 bg-transparent border-0 p-0"
        >
          <span className="text-xl font-bold text-[var(--color-primary)] tracking-widest">SWITCH</span>
          <span className="text-base font-semibold text-[var(--color-text-dim)] tracking-widest">DEV</span>
        </button>
        <nav className="flex items-center gap-1 ml-6">
          {navItems.map((item) => (
            <button
              key={item.key}
              onClick={() => setTab(item.key)}
              className={`px-3 py-2 rounded-lg text-sm font-medium transition-colors flex items-center gap-1.5 ${
                tab === item.key
                  ? "text-[var(--color-primary)] bg-[var(--color-primary)]/10"
                  : "text-[var(--color-text-dim)] hover:bg-[var(--color-surface-2)] hover:text-[var(--color-text)]"
              }`}
            >
              <span className="text-base leading-none">{item.icon}</span>
              {item.label}
            </button>
          ))}
        </nav>
      </header>

      {/* 主内容区 */}
      <main className="flex-1 overflow-y-auto">
        <ErrorBoundary>
          {loading ? (
            <div className="flex items-center justify-center h-full text-[var(--color-text-dim)]">
              加载中...
            </div>
          ) : tab === "dashboard" ? (
            <Dashboard
              proxy={proxy}
              creds={creds}
              config={config}
              onGoCredentials={() => setTab("credentials")}
            />
          ) : tab === "providerapi" ? (
            <ProviderAPI />
          ) : tab === "credentials" ? (
            <Credentials creds={creds} />
          ) : tab === "models" ? (
            <Models config={config} creds={creds} />
          ) : tab === "stats" ? (
            <UsageStats />
          ) : tab === "logs" ? (
            <Logs logs={logs} />
          ) : tab === "settings" ? (
            <Settings creds={creds} config={config} />
          ) : tab === "benchmark" ? (
            <Benchmark creds={creds} />
          ) : (
            <UsageStats />
          )}
          </ErrorBoundary>
        </main>

        {/* 全局更新弹窗（设置页里已有 UpdatePanel，不重复弹） */}
        {updateInfo && tab !== "settings" && (
          <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/50 backdrop-blur-sm">
            <div className="w-full max-w-lg bg-[var(--color-surface)] rounded-xl border border-[var(--color-border)] shadow-2xl p-6 mx-4 space-y-4">
              <div className="flex items-center justify-between">
                <h2 className="text-lg font-bold">
                  {updateInfo.critical ? "⚠️ 强制更新" : "📦 发现新版本"}
                </h2>
                {updateInfo.critical ? (
                  <span className="text-[10px] px-2 py-0.5 rounded-full bg-[var(--color-danger)]/20 text-[var(--color-danger)] font-medium">
                    强制更新
                  </span>
                ) : (
                  <span className="text-[10px] px-2 py-0.5 rounded-full bg-[var(--color-primary)]/20 text-[var(--color-primary)] font-medium">
                    可选更新
                  </span>
                )}
              </div>

              <div className="text-sm text-[var(--color-text-dim)]">
                新版本 <span className="font-mono font-bold text-[var(--color-primary)]">{updateInfo.version}</span>
                {updateInfo.assetSize > 0 && (
                  <span className="ml-2">（{(updateInfo.assetSize / 1024 / 1024).toFixed(1)} MB）</span>
                )}
              </div>

              {updateInfo.notes && (
                <div>
                  <div className="text-xs font-medium text-[var(--color-text-dim)] mb-1">更新内容</div>
                  <div className="text-xs text-[var(--color-text)] whitespace-pre-wrap bg-[var(--color-bg)] rounded-lg p-3 max-h-48 overflow-y-auto leading-relaxed">
                    {updateInfo.notes}
                  </div>
                </div>
              )}

              {/* 下载进度条 */}
              {updateProgress && (
                <div>
                  <div className="flex justify-between text-xs text-[var(--color-text-dim)] mb-1">
                    <span>{updateProgress.message}</span>
                    <span>{Math.round(updateProgress.percent)}%</span>
                  </div>
                  <div className="h-2 rounded bg-[var(--color-bg)] overflow-hidden">
                    <div className="h-full rounded bg-[var(--color-primary)] transition-all" style={{ width: `${updateProgress.percent}%` }} />
                  </div>
                </div>
              )}

              <div className="flex gap-2 pt-2">
                <button
                  onClick={applyUpdate}
                  disabled={updating}
                  className={`px-5 py-2 text-sm rounded-lg font-medium hover:opacity-90 disabled:opacity-50 ${
                    updateInfo.critical
                      ? "bg-[var(--color-danger)] text-white"
                      : "bg-[var(--color-primary)] text-white"
                  }`}
                >
                  {updating ? "更新中..." : updateInfo.critical ? "立即更新（必需）" : "立即更新"}
                </button>
                {!updateInfo.critical && (
                  <>
                    <button
                      onClick={skipVersion}
                      disabled={updating}
                      className="px-4 py-2 text-sm rounded-lg bg-[var(--color-surface-2)] hover:bg-[var(--color-border)] disabled:opacity-50"
                    >
                      跳过此版本
                    </button>
                    <button
                      onClick={() => { setUpdateInfo(null); setUpdateProgress(null); }}
                      disabled={updating}
                      className="px-4 py-2 text-sm rounded-lg text-[var(--color-text-dim)] hover:bg-[var(--color-surface-2)] disabled:opacity-50 ml-auto"
                    >
                      稍后再说
                    </button>
                  </>
                )}
              </div>
            </div>
          </div>
        )}
    </div>
  );
}
