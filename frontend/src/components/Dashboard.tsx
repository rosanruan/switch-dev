import { useState, useEffect, useRef } from "react";
import { ProxyService, LogService } from "../../bindings/switchdev/service";
import type { AllCredStatus, SpeedStats } from "../../bindings/switchdev/service/models";
import type { ProxyStatus } from "../../bindings/switchdev/proxy/models";
import type { Config } from "../../bindings/switchdev/config/models";
import { useWailsEvent } from "../hooks/useWailsEvent";

import CopyButton from "./CopyButton";

const UPSTREAM_LABEL: Record<string, string> = {
  joycode: "JoyCode",
  deveco: "DevEco",
  workbuddy: "WorkBuddy",
};

interface Props {
  proxy: ProxyStatus | null;
  creds: AllCredStatus | null;
  config: Config | null;
  onGoCredentials: () => void;
}

export default function Dashboard({ proxy, creds, config, onGoCredentials }: Props) {
  const [busy, setBusy] = useState(false);
  // 今日使用概览
  const [today, setToday] = useState<{ tokens: number; cost: number } | null>(null);
  // 今日输出速率
  const [speed, setSpeed] = useState<SpeedStats | null>(null);
  // apiKey 显示/隐藏切换
  const [showKey, setShowKey] = useState(false);

  // 总请求数：初始从 proxy.requests（后端从 DB 查），每收到 log:new 就 +1
  const [totalReqs, setTotalReqs] = useState(0);
  const prevProxyReqs = useRef(0);
  useEffect(() => {
    if (proxy?.requests != null && proxy.requests !== prevProxyReqs.current) {
      setTotalReqs(proxy.requests);
      prevProxyReqs.current = proxy.requests;
    }
  }, [proxy?.requests]);
  useWailsEvent("log:new", () => {
    setTotalReqs((n) => n + 1);
  });

  useEffect(() => {
    LogService.GetTodaySummary()
      .then((s) => setToday(s))
      .catch(() => {});
    LogService.GetTodaySpeed()
      .then((s) => setSpeed(s))
      .catch(() => {});
  }, []);

  // 已配置的上游是否全部无效（用于空状态提示）。OpenCode 不在默认流程内，不计入。
  const allInvalid =
    !creds ||
    (!creds.joycode?.valid && !creds.deveco?.valid && !creds.workbuddy?.valid);

  // 根据当前配置计算实际会用的模型链（用于显示）
  const { chainText, chainLength } = (() => {
    if (!config) return { chainText: "-", chainLength: 0 };
    if (config.mode === "ua") {
      const ruleCount = (config.uaRules ?? []).filter((r) => r.enabled).length;
      const mappingCount = (config.uaRules ?? []).reduce((n, r) => n + (r.mappings?.length ?? 0), 0);
      if (mappingCount === 0) return { chainText: ruleCount > 0 ? "已启用规则，未配映射" : "（空）", chainLength: 0 };
      return { chainText: `${ruleCount} 条规则，${mappingCount} 个模型映射`, chainLength: mappingCount };
    }
    if (config.mode === "auto") {
      // auto 链：展开 agent 分组
      const flat: { u: string; m: string }[] = [];
      (config.autoChain ?? []).forEach((c) => c.models.forEach((m) => flat.push({ u: c.upstream, m })));
      if (flat.length === 0) return { chainText: "（空）", chainLength: 0 };
      const text = flat.map((f) => `${UPSTREAM_LABEL[f.u] ?? f.u}/${f.m}`).join(" → ");
      return { chainText: text, chainLength: flat.length };
    }
    // 手动模式：显示已配置降级链的模型数
    const keys = Object.keys(config.manualFallbacks ?? {});
    if (keys.length === 0) return { chainText: "未配降级链（仅走指定模型 + 兜底）", chainLength: 0 };
    return {
      chainText: `已配 ${keys.length} 个模型的降级链：${keys.join(", ")}`,
      chainLength: keys.length,
    };
  })();

  const handleRestart = async () => {
    setBusy(true);
    try {
      await ProxyService.RestartProxy();
    } catch (e) {
      console.error(e);
    } finally {
      setBusy(false);
    }
  };

  const handleStop = async () => {
    setBusy(true);
    try {
      await ProxyService.StopProxy();
    } catch (e) {
      console.error(e);
    } finally {
      setBusy(false);
    }
  };

  const handleStart = async () => {
    setBusy(true);
    try {
      await ProxyService.StartProxy(0);
    } catch (e) {
      console.error(e);
    } finally {
      setBusy(false);
    }
  };

  const running = proxy?.running ?? false;

  return (
    <div className="p-6 space-y-6">
      {/* 空状态引导：三上游全无效时显示 */}
      {allInvalid && (
        <section className="bg-[var(--color-warning)]/10 border border-[var(--color-warning)]/30 rounded-xl p-5">
          <div className="flex items-start gap-3">
            <span className="text-2xl">🚀</span>
            <div className="flex-1">
              <h2 className="font-semibold mb-1">尚未配置任何 agent 工具</h2>
              <p className="text-sm text-[var(--color-text-dim)] mb-3">
                代理通过复用本地 AI 编程工具的登录态调用大模型。请至少安装并登录一个工具（推荐 DevEco Code，auto 模式主力）。
              </p>
              <button
                onClick={onGoCredentials}
                className="px-4 py-1.5 text-sm rounded-lg bg-[var(--color-primary)] hover:opacity-90"
              >
                前往凭据页安装 →
              </button>
            </div>
          </div>
        </section>
      )}

      {/* 代理控制 */}
      <section>
        <div className="bg-[var(--color-surface)] rounded-xl p-5 border border-[var(--color-border)]">
          <div className="flex items-center justify-between mb-4">
            <div className="flex items-center gap-3">
              <span
                className={`w-3 h-3 rounded-full ${
                  running ? "bg-[var(--color-success)]" : "bg-[var(--color-danger)]"
                }`}
              />
              <span className="font-medium">{running ? "运行中" : "已停止"}</span>
            </div>
            <div className="flex gap-2">
              {!running ? (
                <button
                  onClick={handleStart}
                  disabled={busy}
                  className="px-4 py-1.5 text-sm rounded-lg bg-[var(--color-success)] hover:opacity-90 disabled:opacity-50"
                >
                  启动
                </button>
              ) : (
                <button
                  onClick={handleStop}
                  disabled={busy}
                  className="px-4 py-1.5 text-sm rounded-lg bg-[var(--color-danger)] hover:opacity-90 disabled:opacity-50"
                >
                  停止
                </button>
              )}
              <button
                onClick={handleRestart}
                disabled={busy}
                className="px-4 py-1.5 text-sm rounded-lg bg-[var(--color-surface-2)] hover:bg-[var(--color-border)] disabled:opacity-50"
              >
                重启
              </button>
            </div>
          </div>
          <div className="grid grid-cols-4 gap-4 text-sm">
            <div>
              <div className="text-[var(--color-text-dim)]">监听地址</div>
              <div className="font-mono">
                {proxy?.host ?? "127.0.0.1"}:{proxy?.port ?? 8787}
              </div>
            </div>
            <div>
              <div className="text-[var(--color-text-dim)]">运行模式</div>
              <div className="font-mono">
                {proxy?.mode === "manual" ? "手动" : proxy?.mode === "ua" ? "UA" : "auto"}
              </div>
            </div>
            <div className="col-span-2">
              <div className="text-[var(--color-text-dim)]">
                {config?.mode === "manual" ? "手动降级配置" : config?.mode === "ua" ? "UA 路由规则" : "auto 优先级链"}
              </div>
              <div className="font-mono text-xs truncate" title={chainText}>
                {chainText}
              </div>
            </div>
            <div>
              <div className="text-[var(--color-text-dim)]">
                {config?.mode === "ua" ? "UA 兜底" : "全局兜底"}
              </div>
              <div className="font-mono text-xs truncate">
                {config?.mode === "ua"
                  ? (config?.uaGlobalFallback?.upstream
                      ? `${UPSTREAM_LABEL[config.uaGlobalFallback.upstream] ?? config.uaGlobalFallback.upstream}/${config.uaGlobalFallback.model}`
                      : "-")
                  : (config?.globalFallback?.upstream
                      ? `${UPSTREAM_LABEL[config.globalFallback.upstream] ?? config.globalFallback.upstream}/${config.globalFallback.model}`
                      : "-")}
              </div>
            </div>
            <div>
              <div className="text-[var(--color-text-dim)]">总请求数</div>
              <div className="font-mono">{totalReqs}</div>
            </div>
            <div>
              <div className="text-[var(--color-text-dim)]">链长度</div>
              <div className="font-mono">{chainLength}</div>
            </div>
          </div>
          <div className="mt-4 pt-4 border-t border-[var(--color-border)]">
            <div className="text-xs text-[var(--color-text-dim)] mb-2">🔗 接入地址</div>
            <div className="space-y-2">
              <div className="flex items-center gap-2">
                <span className="text-xs text-[var(--color-text-dim)] w-16 shrink-0">Anthropic</span>
                <code className="flex-1 px-3 py-1.5 rounded-md bg-[var(--color-surface-2)] text-sm font-mono text-[var(--color-text)]">
                  http://127.0.0.1:{proxy?.port ?? 8787}
                </code>
                <CopyButton text={`http://127.0.0.1:${proxy?.port ?? 8787}`} />
              </div>
              <div className="flex items-center gap-2">
                <span className="text-xs text-[var(--color-text-dim)] w-16 shrink-0">OpenAI</span>
                <code className="flex-1 px-3 py-1.5 rounded-md bg-[var(--color-surface-2)] text-sm font-mono text-[var(--color-text)]">
                  http://127.0.0.1:{proxy?.port ?? 8787}/v1
                </code>
                <CopyButton text={`http://127.0.0.1:${proxy?.port ?? 8787}/v1`} />
              </div>
              {config?.authEnabled !== false && (
                <div className="flex items-center gap-2">
                  <span className="text-xs text-[var(--color-text-dim)] w-16 shrink-0">apiKey</span>
                  <code className="flex-1 px-3 py-1.5 rounded-md bg-[var(--color-surface-2)] text-sm font-mono text-[var(--color-text)] truncate">
                    {showKey ? (config?.apiKey || "-") : maskKey(config?.apiKey || "")}
                  </code>
                  <button
                    onClick={() => setShowKey((v) => !v)}
                    className="px-2.5 py-1 text-xs rounded-md bg-[var(--color-surface-2)] hover:bg-[var(--color-border)] text-[var(--color-text)]"
                    title={showKey ? "隐藏" : "显示"}
                  >
                    {showKey ? "🙈" : "👁"}
                  </button>
                  <CopyButton text={config?.apiKey || ""} />
                </div>
              )}
            </div>
          </div>
        </div>
      </section>

      {/* 今日使用概览 */}
      <section className="grid grid-cols-3 gap-4">
        <div className="bg-[var(--color-surface)] rounded-xl p-5 border border-[var(--color-border)]">
          <div className="text-sm text-[var(--color-text-dim)] mb-1">今日消耗 Token</div>
          <div className="text-2xl font-bold text-[var(--color-primary)]">{fmtTokens(today?.tokens ?? 0)}</div>
        </div>
        <div className="bg-[var(--color-surface)] rounded-xl p-5 border border-[var(--color-border)]">
          <div className="text-sm text-[var(--color-text-dim)] mb-1">今日消耗费用</div>
          <div className="text-2xl font-bold text-[var(--color-success)]">${(today?.cost ?? 0).toFixed(5)}</div>
        </div>
        <div className="bg-[var(--color-surface)] rounded-xl p-5 border border-[var(--color-border)]">
          <div className="text-sm text-[var(--color-text-dim)] mb-1">今日输出速率</div>
          <div className="text-2xl font-bold text-[var(--color-primary)]">
            {(speed?.overallTps ?? 0).toFixed(1)}
            <span className="text-sm font-normal text-[var(--color-text-dim)] ml-1">t/s</span>
          </div>
          <div className="text-xs text-[var(--color-text-dim)] mt-1">基于 {speed?.totalReqs ?? 0} 次请求</div>
        </div>
      </section>

      {/* 今日模型输出速率明细 */}
      <section className="bg-[var(--color-surface)] rounded-xl p-5 border border-[var(--color-border)]">
        <div className="flex items-center justify-between mb-3">
          <h3 className="font-medium">今日模型输出速率明细</h3>
          <span className="text-xs text-[var(--color-text-dim)]">按输出速率排名</span>
        </div>
        {speed && speed.totalReqs > 0 ? (
          <div>
            <div className="grid grid-cols-12 text-xs text-[var(--color-text-dim)] pb-2 border-b border-[var(--color-border)]">
              <div className="col-span-1">#</div>
              <div className="col-span-6">模型</div>
              <div className="col-span-3 text-right">速率</div>
              <div className="col-span-2 text-right">次数</div>
            </div>
            {speed.byModel.map((m, i) => (
              <div key={m.model} className="grid grid-cols-12 items-center text-sm py-2 border-b border-[var(--color-border)] last:border-0">
                <div className="col-span-1">{rankBadge(i)}</div>
                <div className="col-span-6 font-mono text-xs truncate" title={m.model}>{m.model}</div>
                <div className="col-span-3 text-right font-mono">{m.tps.toFixed(1)} t/s</div>
                <div className="col-span-2 text-right text-[var(--color-text-dim)]">{m.reqs}</div>
              </div>
            ))}
          </div>
        ) : (
          <div className="text-sm text-[var(--color-text-dim)] py-6 text-center">今日暂无输出数据</div>
        )}
      </section>
    </div>
  );
}

// fmtTokens token 数格式化（千分位）
function fmtTokens(n: number): string {
  if (n === 0) return "0";
  const s = String(n);
  let out = "";
  for (let i = s.length; i > 0; i -= 3) {
    if (out) out = "," + out;
    out = s.slice(Math.max(0, i - 3), i) + out;
  }
  return out;
}

// rankBadge 排名标记：前三名奖牌，其余序号
function rankBadge(i: number): string {
  return ["🥇", "🥈", "🥉"][i] ?? String(i + 1);
}

// maskKey 隐藏 apiKey：显示前 10 位，其余用等长 * 代替
function maskKey(key: string): string {
  if (!key) return "";
  if (key.length <= 10) return key;
  return key.slice(0, 10) + "*".repeat(key.length - 10);
}

