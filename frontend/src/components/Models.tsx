import { useCallback, useEffect, useMemo, useState } from "react";
import { ModelService, ConfigService } from "../../bindings/switchdev/service";
import type { ModelDetail, AllCredStatus } from "../../bindings/switchdev/service/models";
import type { Config } from "../../bindings/switchdev/config/models";
import { useWailsEvent } from "../hooks/useWailsEvent";

const UPSTREAM_LABEL: Record<string, string> = {
  joycode: "JoyCode",
  deveco: "DevEco",
  workbuddy: "WorkBuddy",
};

const UPSTREAM_COLOR: Record<string, string> = {
  joycode: "bg-[var(--color-success)]/20 text-[var(--color-success)]",
  deveco: "bg-[var(--color-danger)]/20 text-[var(--color-danger)]",
  workbuddy: "bg-pink-500/20 text-pink-400",
};

// 供应商 source 形如 "名称 (http://...)"，取括号前的名称
function upstreamDisplayName(upstream: string, creds: AllCredStatus | null): string {
  if (UPSTREAM_LABEL[upstream]) return UPSTREAM_LABEL[upstream];
  const src = creds?.providerAPIs?.[upstream]?.source ?? "";
  const name = src.split(" (")[0].trim();
  return name || upstream;
}

export default function Models({ config, creds }: { config: Config | null; creds: AllCredStatus | null }) {
  const [models, setModels] = useState<ModelDetail[]>([]);
  const [loading, setLoading] = useState(true);
  const [refreshing, setRefreshing] = useState(false);

  // 过滤状态
  const [activeUpstream, setActiveUpstream] = useState<string>(""); // "" = 全部

  const load = useCallback(
    () =>
      ModelService.GetModels().then((m) =>
        setModels(
          (m ?? [])
            .filter((x): x is ModelDetail => x !== null)
            .filter((x) => x.upstream !== "opencode")
        )
      ),
    []
  );

  useEffect(() => {
    load().finally(() => setLoading(false));
  }, [load]);

  // 目录刷新/供应商变更后重新拉取
  useWailsEvent("models:change", () => {
    load();
  });

  const refresh = async () => {
    setRefreshing(true);
    try {
      await ConfigService.RefreshModels();
      await load();
    } finally {
      setRefreshing(false);
    }
  };

  // 各上游模型来源（live=接口实时/DB 回读，local=本地种子白名单）
  const sourceByUpstream = useMemo(() => {
    const m = new Map<string, string>();
    for (const model of models) {
      if (model.upstream === "auto") continue;
      // 同上游内只要有一个 live 就算 live
      if (model.source === "live" || !m.has(model.upstream)) {
        m.set(model.upstream, model.source || "local");
      }
    }
    return m;
  }, [models]);

  // 当前配置选中的模型集合
  const selectedSet = useMemo(() => {
    const s = new Set<string>();
    if (config) {
      const addRef = (u: string, m: string) => s.add(`${u}/${m}`);
      (config.autoChain ?? []).forEach((c) => c.models.forEach((m) => addRef(c.upstream, m)));
      Object.entries(config.manualFallbacks ?? {}).forEach(([key, chain]) => {
        s.add(key);
        (chain ?? []).forEach((r) => addRef(r.upstream, r.model));
      });
      if (config.globalFallback) addRef(config.globalFallback.upstream, config.globalFallback.model);
    }
    return s;
  }, [config]);

  // 上游选项（按实际模型聚合，带数量）
  const upstreamOptions = useMemo(() => {
    const counts = new Map<string, number>();
    for (const m of models) {
      counts.set(m.upstream, (counts.get(m.upstream) ?? 0) + 1);
    }
    return Array.from(counts.entries()).map(([up, count]) => ({
      upstream: up,
      label: upstreamDisplayName(up, creds),
      count,
    }));
  }, [models, creds]);

  // 过滤后的模型
  const filtered = useMemo(() => {
    return models.filter((m) => {
      if (activeUpstream && m.upstream !== activeUpstream) return false;
      return true;
    });
  }, [models, activeUpstream]);

  // 排序：选中的排前面
  const sorted = useMemo(
    () =>
      [...filtered].sort((a, b) => {
        const aSel = isSelected(selectedSet, a) ? 0 : 1;
        const bSel = isSelected(selectedSet, b) ? 0 : 1;
        return aSel - bSel;
      }),
    [filtered, selectedSet]
  );

  if (loading) return <div className="p-6 text-[var(--color-text-dim)]">加载中...</div>;

  return (
    <div className="p-6 space-y-4">

      {/* 当前配置概览 */}
      {config && (
        <section className="bg-[var(--color-surface)] rounded-xl p-5 border border-[var(--color-border)]">
          <h2 className="text-sm font-semibold text-[var(--color-text-dim)] uppercase mb-3">
            当前配置（{config.mode === "manual" ? "手动模式" : "auto 模式"}）
          </h2>
          {config.mode === "auto" ? (
            <div className="space-y-2">
              <div className="text-xs text-[var(--color-text-dim)]">auto 优先级链（按序尝试）：</div>
              <div className="flex items-center gap-2 flex-wrap">
                {(config.autoChain ?? []).flatMap((c, ci) =>
                  c.models.map((m, mi) => (
                    <span key={`${ci}-${mi}`} className="flex items-center gap-2">
                      <span className="font-mono text-xs px-2 py-1 rounded bg-[var(--color-surface-2)]">
                        {UPSTREAM_LABEL[c.upstream] ?? c.upstream}/{m}
                      </span>
                      <span className="text-[var(--color-text-dim)] text-xs">→</span>
                    </span>
                  ))
                )}
                <span className="font-mono text-xs px-2 py-1 rounded bg-[var(--color-warning)]/20 text-[var(--color-warning)]">
                  兜底：{UPSTREAM_LABEL[config.globalFallback?.upstream] ?? "?"}/{config.globalFallback?.model}
                </span>
              </div>
            </div>
          ) : (
            <div className="space-y-2">
              <div className="text-xs text-[var(--color-text-dim)]">手动模式：客户端指定模型，失败按降级链走</div>
              {Object.keys(config.manualFallbacks ?? {}).length === 0 ? (
                <div className="text-sm text-[var(--color-text-dim)]">未配置降级链（指定模型失败直接走全局兜底）</div>
              ) : (
                Object.entries(config.manualFallbacks ?? {}).map(([key, chain]) => (
                  <div key={key} className="flex items-center gap-2 flex-wrap text-xs">
                    <span className="font-mono px-2 py-1 rounded bg-[var(--color-primary)]/20 text-[var(--color-primary)]">
                      {key}
                    </span>
                    <span className="text-[var(--color-text-dim)]">→ 失败降级 →</span>
                    {(chain ?? []).map((r, i) => (
                      <span key={i} className="flex items-center gap-2">
                        <span className="font-mono px-2 py-1 rounded bg-[var(--color-surface-2)]">
                          {UPSTREAM_LABEL[r.upstream] ?? r.upstream}/{r.model}
                        </span>
                        {i < (chain?.length ?? 0) - 1 && <span className="text-[var(--color-text-dim)]">→</span>}
                      </span>
                    ))}
                  </div>
                ))
              )}
              <div className="flex items-center gap-2 text-xs pt-2 border-t border-[var(--color-border)] mt-2">
                <span className="text-[var(--color-text-dim)]">全局兜底：</span>
                <span className="font-mono px-2 py-1 rounded bg-[var(--color-warning)]/20 text-[var(--color-warning)]">
                  {UPSTREAM_LABEL[config.globalFallback?.upstream] ?? "?"}/{config.globalFallback?.model}
                </span>
              </div>
            </div>
          )}
        </section>
      )}

      {/* 过滤工具栏：上游标签（放不下自动换行）+ 刷新 */}
      <div className="flex items-center gap-1.5 flex-wrap">
        <UpstreamChip
          label="全部"
          count={models.length}
          active={activeUpstream === ""}
          onClick={() => setActiveUpstream("")}
        />
        {upstreamOptions.map((o) => (
          <UpstreamChip
            key={o.upstream}
            label={o.label}
            count={o.count}
            active={activeUpstream === o.upstream}
            color={UPSTREAM_COLOR[o.upstream]}
            onClick={() =>
              setActiveUpstream((cur) => (cur === o.upstream ? "" : o.upstream))
            }
          />
        ))}
        <button
          onClick={refresh}
          disabled={refreshing}
          title="从上游接口重新拉取模型列表（结果会持久化，重启后仍可用）"
          className="ml-auto px-3 py-1.5 rounded-lg text-xs bg-[var(--color-surface-2)] hover:bg-[var(--color-surface)] border border-[var(--color-border)] disabled:opacity-50"
        >
          {refreshing ? "刷新中..." : "🔄 刷新模型"}
        </button>
      </div>

      {/* 各上游模型来源徽章 */}
      <div className="flex items-center gap-1.5 flex-wrap">
        {upstreamOptions.map((o) => {
          const src = sourceByUpstream.get(o.upstream) ?? "local";
          const isLive = src === "live";
          const isFree = src === "free";
          return (
            <span
              key={o.upstream}
              title={
                isLive
                  ? "接口实时拉取（已持久化）"
                  : isFree
                    ? "第三方供应商已验证模型"
                    : "本地内置白名单（接口拉取失败或该上游无模型接口）"
              }
              className={`text-[11px] px-2 py-0.5 rounded ${
                isLive
                  ? "bg-[var(--color-success)]/15 text-[var(--color-success)]"
                  : isFree
                    ? "bg-[var(--color-primary)]/15 text-[var(--color-primary)]"
                    : "bg-[var(--color-warning)]/15 text-[var(--color-warning)]"
              }`}
            >
              {o.label}: {isLive ? "实时" : isFree ? "免费" : "本地"}
            </span>
          );
        })}
      </div>

      <div className="text-xs text-[var(--color-text-dim)] h-4 leading-4">
        共 {models.length} 个模型，显示 {sorted.length} 个
      </div>

      {/* 模型表格 */}
      <section className="bg-[var(--color-surface)] rounded-xl border border-[var(--color-border)] overflow-x-auto">
        <table className="w-full text-sm min-w-[720px]">
          <thead>
            <tr className="border-b border-[var(--color-border)] text-[var(--color-text-dim)]">
              <th className="text-left px-4 py-3 font-medium w-8 whitespace-nowrap"></th>
              <th className="text-left px-4 py-3 font-medium whitespace-nowrap">模型 ID</th>
              <th className="text-left px-4 py-3 font-medium whitespace-nowrap">显示名</th>
              <th className="text-left px-4 py-3 font-medium whitespace-nowrap">上游</th>
              <th className="text-left px-4 py-3 font-medium whitespace-nowrap">流式</th>
              <th className="text-right px-4 py-3 font-medium whitespace-nowrap">Context</th>
              <th className="text-right px-4 py-3 font-medium whitespace-nowrap">Output</th>
              <th className="text-left px-4 py-3 font-medium whitespace-nowrap">能力</th>
            </tr>
          </thead>
          <tbody>
            {sorted.length === 0 ? (
              <tr>
                <td colSpan={8} className="px-4 py-10 text-center text-[var(--color-text-dim)]">
                  没有匹配的模型
                </td>
              </tr>
            ) : (
              sorted.map((m) => {
                const sel = isSelected(selectedSet, m);
                return (
                  <tr key={m.id} className={`border-b border-[var(--color-border)] last:border-0 ${sel ? "bg-[var(--color-primary)]/5" : ""}`}>
                    <td className="px-4 py-3">
                      {sel && <span className="text-[var(--color-primary)]" title="当前配置选中">★</span>}
                    </td>
                    <td className="px-4 py-3 font-mono text-xs">{m.id}</td>
                    <td className="px-4 py-3">{m.label}</td>
                    <td className="px-4 py-3">
                      <span
                        className={`text-xs px-2 py-0.5 rounded-full ${
                          UPSTREAM_COLOR[m.upstream] ?? "bg-[var(--color-surface-2)]"
                        }`}
                      >
                        {upstreamDisplayName(m.upstream, creds)}
                      </span>
                    </td>
                    <td className="px-4 py-3">{m.stream ? "✓" : "✗"}</td>
                    <td className="px-4 py-3 text-right font-mono text-xs">
                      {m.context > 0 ? (m.context / 1000).toFixed(0) + "k" : "-"}
                    </td>
                    <td className="px-4 py-3 text-right font-mono text-xs">
                      {m.output > 0 ? (m.output / 1000).toFixed(0) + "k" : "-"}
                    </td>
                    <td className="px-4 py-3 text-xs space-x-1">
                      {m.toolCall && (
                        <span className="px-1.5 py-0.5 rounded bg-[var(--color-surface-2)]">tool</span>
                      )}
                      {m.vision && (
                        <span className="px-1.5 py-0.5 rounded bg-[var(--color-surface-2)]">vision</span>
                      )}
                    </td>
                  </tr>
                );
              })
            )}
          </tbody>
        </table>
      </section>
    </div>
  );
}

function UpstreamChip({
  label,
  count,
  active,
  color,
  onClick,
}: {
  label: string;
  count: number;
  active: boolean;
  color?: string;
  onClick: () => void;
}) {
  return (
    <button
      onClick={onClick}
      className={`shrink-0 inline-flex items-center gap-1.5 px-2.5 py-1 text-xs rounded-full border transition-colors ${
        active
          ? color
            ? `${color} border-current/30`
            : "bg-[var(--color-primary)]/15 text-[var(--color-primary)] border-[var(--color-primary)]/40"
          : "bg-[var(--color-surface-2)] text-[var(--color-text-dim)] border-[var(--color-border)] hover:text-[var(--color-text)]"
      }`}
    >
      <span>{label}</span>
      <span className="opacity-70">{count}</span>
    </button>
  );
}

function isSelected(set: Set<string>, m: ModelDetail): boolean {
  return set.has(`${m.upstream}/${m.id}`) || set.has(m.id);
}
