import { useState, useEffect, useMemo, useRef, useCallback } from "react";
import { createPortal } from "react-dom";
import { BenchmarkService, ModelService, ProviderAPIService } from "../../bindings/switchdev/service";
import type { BenchmarkResult, ModelDetail, AllCredStatus } from "../../bindings/switchdev/service/models";
import { useWailsEvent } from "../hooks/useWailsEvent";
import { ModelSelect } from "./ModelSelect";
import ConfirmPopover from "./ConfirmPopover";
import TrashIcon from "./TrashIcon";

const DEFAULT_PROMPT = "请详细介绍 Go 语言的 goroutine 和 channel 并发模型，包括基本概念、使用示例和注意事项。";
const STORAGE_KEY = "benchmark.items.v2";
const APIMODE_KEY = "benchmark.apimode";

type ApiMode = "anthropic" | "openai-chat" | "openai-responses";

const API_MODES: { value: ApiMode; label: string; desc: string }[] = [
  { value: "anthropic", label: "Anthropic Messages", desc: "/v1/messages" },
  { value: "openai-chat", label: "OpenAI Chat Completions", desc: "/v1/chat/completions" },
  { value: "openai-responses", label: "OpenAI Responses", desc: "/v1/responses" },
];

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

interface BenchItem {
  id: string;
  upstream: string;
  model: string;
}

const itemKey = (it: BenchItem) => `${it.upstream}|${it.model}`;

let _seq = 0;
const newItemId = () => {
  _seq += 1;
  return `bi-${Date.now().toString(36)}-${_seq}`;
};

function upstreamDisplayName(upstream: string, creds: AllCredStatus | null): string {
  if (UPSTREAM_LABEL[upstream]) return UPSTREAM_LABEL[upstream];
  const src = creds?.providerAPIs?.[upstream]?.source ?? "";
  return src.split(" (")[0].trim() || upstream;
}

function defaultModel(models: ModelDetail[], upstream: string): string {
  const up = models.filter((m) => m.upstream === upstream);
  return up.find((m) => m.free)?.id ?? up[0]?.id ?? "";
}

export default function Benchmark({ creds }: { creds: AllCredStatus | null }) {
  const [prompt, setPrompt] = useState(DEFAULT_PROMPT);
  const [maxTokens, setMaxTokens] = useState(1024);
  const [apiMode, setApiMode] = useState<ApiMode>("anthropic");
  const [models, setModels] = useState<ModelDetail[]>([]);
  const [items, setItems] = useState<BenchItem[]>([]);
  const [results, setResults] = useState<Record<string, BenchmarkResult | null>>({});
  const [running, setRunning] = useState(false);
  const [singleRunning, setSingleRunning] = useState<Record<string, boolean>>({});
  const [expanded, setExpanded] = useState<Record<string, boolean>>({});
  const [streamingContent, setStreamingContent] = useState<Record<string, string>>({});

  // 添加测评项面板状态
  const [pickOpen, setPickOpen] = useState(false);
  const [pickUpstream, setPickUpstream] = useState<string>("");
  const [pickSelected, setPickSelected] = useState<Set<string>>(new Set());

  const seeded = useRef(false);

  const [locked, setLocked] = useState(false);

  const refreshModels = useCallback(() => {
    ModelService.GetModels()
      .then((m) =>
        setModels(
          (m ?? [])
            .filter((x): x is ModelDetail => x !== null)
            .filter((x) => x.upstream !== "opencode")
        )
      )
      .catch(() => {});
  }, []);

  const checkLock = useCallback(() => {
    ProviderAPIService.GetLockStatus()
      .then((info) => setLocked(!!info?.isLocked))
      .catch(() => {});
  }, []);

  useEffect(() => {
    checkLock();
  }, [checkLock]);

  useWailsEvent("providerapi:change", () => {
    checkLock();
    refreshModels();
  });

  useEffect(() => {
    refreshModels();
  }, [refreshModels]);

  // 恢复 apiMode
  useEffect(() => {
    const saved = localStorage.getItem(APIMODE_KEY);
    if (saved === "anthropic" || saved === "openai-chat" || saved === "openai-responses") {
      setApiMode(saved);
    }
  }, []);

  // 恢复上一次测评结果（从 DB 读回）
  useEffect(() => {
    BenchmarkService.GetBenchResults()
      .then((list) => {
        if (!list || list.length === 0) return;
        const map: Record<string, BenchmarkResult | null> = {};
        for (const r of list) {
          if (r && r.upstream && r.model) map[`${r.upstream}|${r.model}`] = r;
        }
        setResults((prev) => ({ ...map, ...prev }));
      })
      .catch(() => {});
  }, []);

  useEffect(() => {
    localStorage.setItem(APIMODE_KEY, apiMode);
  }, [apiMode]);

  // 恢复测评项
  useEffect(() => {
    if (models.length === 0 || seeded.current) return;
    seeded.current = true;
    let initial: BenchItem[] = [];
    try {
      const saved = localStorage.getItem(STORAGE_KEY);
      if (saved) {
        const parsed = JSON.parse(saved) as Array<Partial<BenchItem>>;
        if (Array.isArray(parsed)) {
          initial = parsed
            .filter((p) => p && p.upstream && p.model)
            .map((p) => ({ id: p.id || newItemId(), upstream: p.upstream!, model: p.model! }));
        }
      }
    } catch { /* ignore */ }
    if (initial.length === 0) {
      const ups = Array.from(new Set(models.map((m) => m.upstream)));
      initial = ups
        .map((up) => ({ id: newItemId(), upstream: up, model: defaultModel(models, up) }))
        .filter((it) => it.model);
    }
    setItems(initial);
  }, [models]);

  // 清理不存在的模型
  useEffect(() => {
    if (models.length === 0) return;
    setItems((prev) => {
      const valid = new Set(models.map((m) => `${m.upstream}|${m.id}`));
      const next = prev.map((it) => {
        if (valid.has(`${it.upstream}|${it.model}`)) return it;
        const fallback = defaultModel(models, it.upstream);
        return fallback ? { ...it, model: fallback } : it;
      }).filter((it) => valid.has(`${it.upstream}|${it.model}`));
      return next.length === prev.length && next.every((it, i) => it.model === prev[i].model) ? prev : next;
    });
  }, [models]);

  useEffect(() => {
    if (seeded.current) localStorage.setItem(STORAGE_KEY, JSON.stringify(items));
  }, [items]);

  // 进度事件
  useWailsEvent("benchmark:progress", (data) => {
    const r = data as BenchmarkResult;
    if (r && r.upstream) setResults((prev) => ({ ...prev, [`${r.upstream}|${r.model}`]: r }));
  });

  useWailsEvent("benchmark:chunk", (data) => {
    const d = data as { upstream: string; model: string; delta: string };
    if (d?.upstream && d.model) {
      const k = `${d.upstream}|${d.model}`;
      setStreamingContent((prev) => ({ ...prev, [k]: (prev[k] || "") + d.delta }));
    }
  });

  const modelsFor = (up: string) => models.filter((m) => m.upstream === up);

  const usedModelsByUpstream = useMemo(() => {
    const m = new Map<string, Set<string>>();
    items.forEach((it) => {
      if (!it.model) return;
      if (!m.has(it.upstream)) m.set(it.upstream, new Set());
      m.get(it.upstream)!.add(it.model);
    });
    return m;
  }, [items]);

  // 所有供应商
  const allUpstreams = useMemo(
    () => Array.from(new Set(models.map((m) => m.upstream))),
    [models]
  );

  // ── 添加面板逻辑 ──────────────────────────────────────────

  // 打开面板：初始化选中状态（已有模型默认勾选）
  const openPickPanel = () => {
    const existing = new Set<string>();
    items.forEach((it) => existing.add(`${it.upstream}|${it.model}`));
    setPickSelected(existing);
    setPickUpstream(allUpstreams[0] ?? "");
    setPickOpen(true);
  };

  const closePickPanel = () => {
    setPickOpen(false);
  };

  // Esc 关闭 + 阻止 body 滚动
  useEffect(() => {
    if (!pickOpen) return;
    const onKey = (e: KeyboardEvent) => { if (e.key === "Escape") closePickPanel(); };
    document.addEventListener("keydown", onKey);
    const prev = document.body.style.overflow;
    document.body.style.overflow = "hidden";
    return () => {
      document.removeEventListener("keydown", onKey);
      document.body.style.overflow = prev;
    };
  }, [pickOpen]);

  const togglePickModel = (upstream: string, modelId: string) => {
    const key = `${upstream}|${modelId}`;
    setPickSelected((prev) => {
      const next = new Set(prev);
      if (next.has(key)) next.delete(key);
      else next.add(key);
      return next;
    });
  };

  // 确定：同步勾选状态——新勾选的添加，反选已有的移除
  const confirmPick = () => {
    const existingByKey = new Map(items.map((it) => [`${it.upstream}|${it.model}`, it]));
    const keep: BenchItem[] = [];
    const add: BenchItem[] = [];
    pickSelected.forEach((key) => {
      if (existingByKey.has(key)) {
        keep.push(existingByKey.get(key)!);
      } else {
        const [upstream, ...rest] = key.split("|");
        const model = rest.join("|");
        if (upstream && model) {
          add.push({ id: newItemId(), upstream, model });
        }
      }
    });
    if (add.length > 0 || keep.length !== items.length) {
      setItems([...keep, ...add]);
    }
    setPickOpen(false);
  };

  // 面板中变更数量（新增 + 移除）
  const pickDelta = useMemo(() => {
    const existingKeys = new Set(items.map((it) => `${it.upstream}|${it.model}`));
    let add = 0, remove = 0;
    pickSelected.forEach((key) => {
      if (!existingKeys.has(key)) add++;
    });
    existingKeys.forEach((key) => {
      if (!pickSelected.has(key)) remove++;
    });
    return { add, remove, total: add + remove };
  }, [pickSelected, items]);

  // 当前供应商的模型
  const pickUpstreamModels = useMemo(
    () => (pickUpstream ? modelsFor(pickUpstream) : []),
    [pickUpstream, models]
  );

  // 每个供应商的选中数/总数
  const pickUpstreamCount = useCallback((up: string) => {
    const all = modelsFor(up);
    let selected = 0;
    all.forEach((m) => {
      if (pickSelected.has(`${up}|${m.id}`)) selected++;
    });
    return { selected, total: all.length };
  }, [models, pickSelected]);

  // ── 列表操作 ─────────────────────────────────────────────

  const updateItem = (idx: number, patch: Partial<BenchItem>) =>
    setItems((prev) => prev.map((it, i) => (i === idx ? { ...it, ...patch } : it)));

  const removeItem = (idx: number) =>
    setItems((prev) => prev.filter((_, i) => i !== idx));

  const clearAll = () => {
    setItems([]);
    setResults({});
    setStreamingContent({});
    setExpanded({});
  };

  // ── 测评执行 ─────────────────────────────────────────────

  const run = async () => {
    const valid = items.filter((it) => it.model);
    if (valid.length === 0) return;
    setRunning(true);
    setResults((prev) => {
      const next = { ...prev };
      for (const it of valid) next[itemKey(it)] = null;
      return next;
    });
    setStreamingContent((prev) => {
      const next = { ...prev };
      for (const it of valid) delete next[itemKey(it)];
      return next;
    });
    try {
      await BenchmarkService.RunBenchmark(valid, prompt, maxTokens, apiMode);
    } finally {
      setRunning(false);
    }
  };

  const runSingle = async (it: BenchItem) => {
    const k = itemKey(it);
    setSingleRunning((prev) => ({ ...prev, [k]: true }));
    setResults((prev) => ({ ...prev, [k]: null }));
    setStreamingContent((prev) => ({ ...prev, [k]: "" }));
    try {
      await BenchmarkService.RunBenchmark([it], prompt, maxTokens, apiMode);
    } finally {
      setSingleRunning((prev) => ({ ...prev, [k]: false }));
    }
  };

  const ranked = items
    .map((it) => results[itemKey(it)])
    .filter((r): r is BenchmarkResult => !!r && r.success)
    .sort((a, b) => b.tps - a.tps);
  const rankOf = (k: string) => ranked.findIndex((r) => `${r.upstream}|${r.model}` === k) + 1;

  const bottomLabel = apiMode === "anthropic"
    ? "走本代理 /v1/messages 端到端"
    : apiMode === "openai-chat"
    ? "走本代理 /v1/chat/completions 端到端"
    : "走本代理 /v1/responses 端到端";

  return (
    <div className="p-6 space-y-6">
      {locked && (
        <div className="rounded-xl border border-[var(--color-warning)]/40 bg-[var(--color-warning)]/10 px-4 py-3 text-sm text-[var(--color-warning)]">
          🔒 供应商已锁定，可照常选择供应商模型；发起测评需先到「供应商」页输入主密码解锁。
        </div>
      )}

      {/* 配置区 */}
      <section className="bg-[var(--color-surface)] rounded-xl p-5 border border-[var(--color-border)] space-y-4">
        <div>
          <label className="text-sm text-[var(--color-text-dim)]">测评提示词（统一基准）</label>
          <textarea
            value={prompt}
            onChange={(e) => setPrompt(e.target.value)}
            rows={3}
            className="w-full mt-1 px-3 py-2 rounded-lg bg-[var(--color-surface-2)] border border-[var(--color-border)] text-sm resize-y focus:outline-none focus:border-[var(--color-primary)]"
          />
        </div>

        <div className="flex items-center gap-3 flex-wrap">
          <label className="text-sm text-[var(--color-text-dim)]">最大输出</label>
          <select
            value={maxTokens}
            onChange={(e) => setMaxTokens(Number(e.target.value))}
            className="px-2 py-1 text-sm rounded-md bg-[var(--color-surface-2)] border border-[var(--color-border)]"
          >
            <option value={256}>256</option>
            <option value={512}>512</option>
            <option value={1024}>1024</option>
            <option value={2048}>2048</option>
          </select>
          <span className="text-xs text-[var(--color-text-dim)]">token（越大越能测稳态速率，但更慢）</span>

          <div className="ml-auto flex items-center gap-2">
            <button
              onClick={openPickPanel}
              disabled={running || allUpstreams.length === 0}
              className="px-3 py-1.5 text-sm rounded-lg bg-[var(--color-surface-2)] hover:bg-[var(--color-border)] disabled:opacity-50"
            >
              ＋ 添加测评项
            </button>
            {items.length > 0 && (
              <ConfirmPopover
                title="清空所有测评项？"
                confirmLabel="清空"
                onConfirm={clearAll}
                triggerClassName={`px-3 py-1.5 text-sm rounded-lg text-[var(--color-danger)] hover:bg-[var(--color-danger)]/10 ${running ? "pointer-events-none opacity-40" : ""}`}
              >
                🗑 清空
              </ConfirmPopover>
            )}
          </div>
        </div>

        {/* 接口协议 radio */}
        <div className="flex items-center gap-4 flex-wrap">
          <span className="text-sm text-[var(--color-text-dim)] shrink-0">接口协议：</span>
          {API_MODES.map((m) => (
            <label
              key={m.value}
              className={`flex items-center gap-1.5 px-3 py-1.5 text-sm rounded-lg cursor-pointer border transition-colors ${
                apiMode === m.value
                  ? "border-[var(--color-primary)] bg-[var(--color-primary)]/10 text-[var(--color-primary)]"
                  : "border-[var(--color-border)] bg-[var(--color-surface-2)] text-[var(--color-text-dim)] hover:text-[var(--color-text)]"
              }`}
            >
              <input
                type="radio"
                name="apiMode"
                value={m.value}
                checked={apiMode === m.value}
                onChange={() => setApiMode(m.value)}
                className="accent-[var(--color-primary)]"
              />
              <span>{m.label}</span>
            </label>
          ))}
          {running ? (
            <button
              onClick={() => BenchmarkService.StopBenchmark()}
              className="ml-auto px-4 py-1.5 text-sm rounded-lg bg-[var(--color-danger)] hover:opacity-90"
            >
              ⏹ 停止测评
            </button>
          ) : (
            <button
              onClick={run}
              disabled={items.length === 0 || Object.values(singleRunning).some(Boolean)}
              className="ml-auto px-4 py-1.5 text-sm rounded-lg bg-[var(--color-primary)] hover:opacity-90 disabled:opacity-50"
            >
              🏁 开始测评
            </button>
          )}
        </div>
      </section>

      {/* 添加测评项弹框 */}
      {pickOpen && createPortal(
        <div
          className="fixed inset-0 z-[100] flex items-center justify-center bg-black/40 backdrop-blur-sm p-4"
          onClick={closePickPanel}
        >
          <div
            onClick={(e) => e.stopPropagation()}
            className="bg-[var(--color-surface)] rounded-xl border border-[var(--color-border)] shadow-2xl flex flex-col w-full overflow-hidden animate-[fadeIn_0.15s_ease-out]"
            style={{ maxWidth: 640, maxHeight: "80vh" }}
          >
            {/* 标题栏 */}
            <div className="flex items-center justify-between px-5 py-3.5 border-b border-[var(--color-border)] shrink-0">
              <h3 className="text-sm font-medium">选择测评模型</h3>
              <button
                onClick={closePickPanel}
                className="w-7 h-7 flex items-center justify-center rounded-md text-[var(--color-text-dim)] hover:bg-[var(--color-surface-2)] hover:text-[var(--color-text)]"
              >
                ✕
              </button>
            </div>

            {/* 左右分栏 */}
            <div className="flex flex-1 min-h-0">
              {/* 左侧：供应商列表 */}
              <div className="w-44 shrink-0 border-r border-[var(--color-border)] bg-[var(--color-surface-2)] overflow-y-auto">
                {allUpstreams.map((up) => {
                  const { selected, total } = pickUpstreamCount(up);
                  const isActive = pickUpstream === up;
                  return (
                    <button
                      key={up}
                      onClick={() => setPickUpstream(up)}
                      className={`w-full px-3 py-2.5 text-sm text-left flex items-center justify-between gap-2 transition-colors ${
                        isActive
                          ? "bg-[var(--color-primary)]/15 text-[var(--color-primary)] font-medium"
                          : "hover:bg-[var(--color-surface)] text-[var(--color-text)]"
                      }`}
                    >
                      <span className="truncate">{upstreamDisplayName(up, creds)}</span>
                      <span className={`text-xs shrink-0 ${selected > 0 ? "text-[var(--color-primary)]" : "text-[var(--color-text-dim)]"}`}>
                        {selected > 0 ? `${selected}/${total}` : total}
                      </span>
                    </button>
                  );
                })}
              </div>

              {/* 右侧：模型列表 */}
              <div className="flex-1 p-3 overflow-y-auto">
                {pickUpstreamModels.length === 0 ? (
                  <div className="text-sm text-[var(--color-text-dim)] text-center py-8">
                    请选择左侧供应商
                  </div>
                ) : (
                  <div className="grid grid-cols-2 gap-1">
                    {pickUpstreamModels.map((m) => {
                      const key = `${pickUpstream}|${m.id}`;
                      const checked = pickSelected.has(key);
                      return (
                        <label
                          key={m.id}
                          className={`flex items-center gap-2 px-2.5 py-2 text-sm rounded-md cursor-pointer transition-colors ${
                            checked
                              ? "bg-[var(--color-primary)]/10 hover:bg-[var(--color-primary)]/15"
                              : "hover:bg-[var(--color-surface-2)]"
                          }`}
                        >
                          <input
                            type="checkbox"
                            checked={checked}
                            onChange={() => togglePickModel(pickUpstream, m.id)}
                            className="accent-[var(--color-primary)] shrink-0"
                          />
                          <span className="truncate flex-1">{m.label}</span>
                        </label>
                      );
                    })}
                  </div>
                )}
              </div>
            </div>

            {/* 底部操作栏 */}
            <div className="flex items-center justify-between px-5 py-3 border-t border-[var(--color-border)] bg-[var(--color-surface-2)] shrink-0">
              <span className="text-xs text-[var(--color-text-dim)]">
                {pickDelta.total > 0
                  ? `将新增 ${pickDelta.add} 个${pickDelta.remove > 0 ? `，移除 ${pickDelta.remove} 个` : ""}`
                  : "未做更改"}
              </span>
              <div className="flex gap-2">
                <button
                  onClick={closePickPanel}
                  className="px-4 py-1.5 text-sm rounded-lg bg-[var(--color-surface)] border border-[var(--color-border)] hover:bg-[var(--color-border)]"
                >
                  取消
                </button>
                <button
                  onClick={confirmPick}
                  disabled={pickDelta.total === 0}
                  className="px-4 py-1.5 text-sm rounded-lg bg-[var(--color-primary)] hover:opacity-90 disabled:opacity-50 text-white"
                >
                  确定
                </button>
              </div>
            </div>
          </div>
        </div>,
        document.body
      )}

      {/* 测评项列表 */}
      <section className="space-y-3">
        {items.map((it, idx) => {
          const k = itemKey(it);
          const r = results[k];
          const rank = rankOf(k);
          const upModels = modelsFor(it.upstream);
          const medal = rank > 0 ? ["🥇", "🥈", "🥉", "4", "5"][rank - 1] : "";
          const isBusy = (running && !r) || !!singleRunning[k];
          return (
            <div key={it.id} className="bg-[var(--color-surface)] rounded-xl p-5 border border-[var(--color-border)]">
              <div className="flex items-center gap-3 mb-3 flex-wrap">
                <span
                  className={`text-xs px-2 py-0.5 rounded-full shrink-0 ${
                    UPSTREAM_COLOR[it.upstream] ?? "bg-[var(--color-surface-2)]"
                  }`}
                >
                  {upstreamDisplayName(it.upstream, creds)}
                </span>
                <ModelSelect
                  options={upModels}
                  value={it.model}
                  onChange={(id) => updateItem(idx, { model: id })}
                  disabledIds={usedModelsByUpstream.get(it.upstream)}
                  hideFreeBadge
                  className="w-56"
                />
                {isBusy ? (
                  <button
                    onClick={() => BenchmarkService.StopBenchmarkItem(it.upstream, it.model)}
                    title="停止该测评项"
                    className="px-2 py-1 text-xs rounded-md bg-[var(--color-danger)]/80 hover:bg-[var(--color-danger)] text-white"
                  >
                    ⏹ 停止
                  </button>
                ) : (
                  <button
                    onClick={() => runSingle(it)}
                    disabled={running || !it.model}
                    className="px-2 py-1 text-xs rounded-md bg-[var(--color-surface-2)] hover:bg-[var(--color-border)] disabled:opacity-50"
                  >
                    单独测评
                  </button>
                )}
                <ConfirmPopover
                  title="移除该测评项？"
                  confirmLabel="移除"
                  onConfirm={() => removeItem(idx)}
                  triggerClassName={`px-2 py-1 text-xs rounded-md text-[var(--color-text-dim)] hover:bg-[var(--color-surface-2)] disabled:opacity-50 ${isBusy ? "pointer-events-none opacity-40" : ""}`}
                >
                  <TrashIcon />
                </ConfirmPopover>
                {isBusy && (
                  <span className="text-xs text-[var(--color-text-dim)] animate-pulse">⏳ 请求中...</span>
                )}
                {rank > 0 && <span className="ml-auto text-lg font-bold">{medal}</span>}
              </div>
              {r ? (
                r.success ? (
                  <>
                    <div className="grid grid-cols-3 gap-4">
                      <div>
                        <div className="text-xs text-[var(--color-text-dim)] mb-0.5">输出速率</div>
                        <div className="text-xl font-bold text-[var(--color-primary)]">
                          {r.tps.toFixed(1)} <span className="text-xs font-normal text-[var(--color-text-dim)]">tok/s</span>
                        </div>
                      </div>
                      <div>
                        <div className="text-xs text-[var(--color-text-dim)] mb-0.5">总耗时</div>
                        <div className="text-xl font-bold">
                          {(r.durationMs / 1000).toFixed(2)} <span className="text-xs font-normal text-[var(--color-text-dim)]">s</span>
                        </div>
                      </div>
                      <div>
                        <div className="text-xs text-[var(--color-text-dim)] mb-0.5">输出 token</div>
                        <div className="text-xl font-bold">{r.outputTokens}</div>
                      </div>
                    </div>
                    {r.content && (
                      <div className="mt-3 pt-3 border-t border-[var(--color-border)]">
                        <button
                          onClick={() => setExpanded((prev) => ({ ...prev, [k]: !prev[k] }))}
                          className="text-xs text-[var(--color-text-dim)] hover:text-[var(--color-text)]"
                        >
                          {expanded[k] ? "▼ 收起返回内容" : "▶ 查看返回内容"}
                        </button>
                        {expanded[k] && (
                          <div className="mt-2 px-3 py-2 rounded-md bg-[var(--color-surface-2)] text-xs whitespace-pre-wrap max-h-60 overflow-y-auto">
                            {r.content}
                          </div>
                        )}
                      </div>
                    )}
                  </>
                ) : r && r.errorMsg === "已停止" ? (
                  <div className="mt-3 px-3 py-2 rounded-md bg-[var(--color-surface-2)] text-xs whitespace-pre-wrap max-h-60 overflow-y-auto">
                    {streamingContent[k] || ""}
                    {streamingContent[k] ? "\n" : ""}
                    <span className="text-[var(--color-text-dim)]">⏹ 已停止</span>
                  </div>
                ) : (
                  <div className="text-sm text-[var(--color-danger)]">✗ {r.errorMsg || "测评失败"}</div>
                )
              ) : isBusy && streamingContent[k] ? (
                <div className="mt-3 px-3 py-2 rounded-md bg-[var(--color-surface-2)] text-xs whitespace-pre-wrap max-h-60 overflow-y-auto">
                  {streamingContent[k]}<span className="animate-pulse">▋</span>
                </div>
              ) : (
                <div className="text-xs text-[var(--color-text-dim)]">
                  {isBusy ? <span className="animate-pulse">⏳ 等待模型响应...</span> : "未测评"}
                </div>
              )}
            </div>
          );
        })}
      </section>

      <div className="text-xs text-[var(--color-text-dim)] text-center">
        口径：{bottomLabel}，tps = 输出 token / 总耗时（含 TTFT + 网络 + 代理开销），与首页速率统计一致
      </div>
    </div>
  );
}
