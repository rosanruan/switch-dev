import { useEffect, useState, useCallback, useRef } from "react";
import { LogService } from "../../bindings/switchdev/service";
import type { UsageStats as UsageStatsData } from "../../bindings/switchdev/service/models";
import UsageTrendChart from "./UsageTrendChart";

type Range = "yesterday" | "today" | "week" | "month" | "custom";
type Tab = "trend" | "provider" | "model";

const TABS: { key: Tab; label: string }[] = [
  { key: "trend", label: "使用趋势" },
  { key: "provider", label: "按供应商" },
  { key: "model", label: "按模型" },
];

const pad = (n: number) => String(n).padStart(2, "0");
function fmtDate(d: Date): string {
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
}
function dateAddDays(d: Date, days: number): Date {
  return new Date(d.getFullYear(), d.getMonth(), d.getDate() + days);
}
function startOfWeek(d: Date): Date {
  const day = d.getDay() || 7;
  return dateAddDays(d, 1 - day);
}
function endOfWeek(d: Date): Date {
  return dateAddDays(startOfWeek(d), 6);
}
function endOfMonth(d: Date): Date {
  return new Date(d.getFullYear(), d.getMonth() + 1, 0);
}

export default function UsageStats() {
  const [range, setRange] = useState<Range>("week");
  const [tab, setTab] = useState<Tab>("trend");
  const [customStart, setCustomStart] = useState("");
  const [customEnd, setCustomEnd] = useState("");
  const [data, setData] = useState<UsageStatsData | null>(null);
  const [loading, setLoading] = useState(false);

  // 计算当前范围的日期区间
  const calcRange = useCallback((): { start: string; end: string } => {
    const today = new Date();
    const todayStr = fmtDate(today);
    if (range === "today") return { start: todayStr, end: todayStr };
    if (range === "yesterday") {
      const y = dateAddDays(today, -1);
      const yStr = fmtDate(y);
      return { start: yStr, end: yStr };
    }
    if (range === "week") {
      return { start: fmtDate(startOfWeek(today)), end: fmtDate(endOfWeek(today)) };
    }
    if (range === "month") {
      return { start: fmtDate(new Date(today.getFullYear(), today.getMonth(), 1)), end: fmtDate(endOfMonth(today)) };
    }
    // custom
    return { start: customStart || todayStr, end: customEnd || todayStr };
  }, [range, customStart, customEnd]);

  // 趋势图粒度：单日（今日/昨日）按小时，跨天范围按天
  const trendGranularity: "hour" | "day" =
    range === "today" || range === "yesterday" ? "hour" : "day";

  const load = useCallback(async () => {
    setLoading(true);
    try {
      const { start, end } = calcRange();
      const res = await LogService.GetUsageStats(start, end);
      setData(res);
    } catch (e) {
      console.error("统计失败", e);
    } finally {
      setLoading(false);
    }
  }, [calcRange]);

  useEffect(() => {
    load();
  }, [load]);

  // 最大 token 数（条形图比例用）
  const maxProviderTokens = Math.max(...(data?.byProvider ?? []).map((a) => a.tokens), 1);
  const maxModelTokens = Math.max(...(data?.byModel ?? []).map((m) => m.tokens), 1);

  return (
    <div className="p-6 space-y-6">
      {/* 时间范围切换 */}
      <div className="flex items-center gap-2 flex-wrap">
        <RangeBtn label="昨日" active={range === "yesterday"} onClick={() => setRange("yesterday")} />
        <RangeBtn label="今天" active={range === "today"} onClick={() => setRange("today")} />
        <RangeBtn label="本周" active={range === "week"} onClick={() => setRange("week")} />
        <RangeBtn label="本月" active={range === "month"} onClick={() => setRange("month")} />
        <RangeBtn label="自定义" active={range === "custom"} onClick={() => setRange("custom")} />
        {range === "custom" && (
          <div className="flex items-center gap-2">
            <DatePicker value={customStart} onChange={setCustomStart} max={customEnd || undefined} placeholder="开始日期" />
            <span className="text-[var(--color-text-dim)]">至</span>
            <DatePicker value={customEnd} onChange={setCustomEnd} min={customStart || undefined} max={fmtDate(new Date())} placeholder="结束日期" />
          </div>
        )}
        <span className="text-xs text-[var(--color-text-dim)] ml-2">
          范围：{calcRange().start} ~ {calcRange().end}
        </span>
      </div>

      {loading && !data ? (
        <div className="p-8 text-center text-[var(--color-text-dim)]">统计中...</div>
      ) : !data ? (
        <div className="p-8 text-center text-[var(--color-text-dim)]">暂无数据</div>
      ) : (
        <>
          {/* 汇总卡片 */}
          <div className="grid grid-cols-4 gap-4">
            <SumCard label="总 Token" value={fmtTokens(data.totalTokens)} sub={`输入 ${fmtTokens(data.totalInput)} / 输出 ${fmtTokens(data.totalOutput)}`} color="text-[var(--color-text)]" />
            <SumCard label="总请求" value={String(data.totalReqs)} sub={`成功 ${data.successReqs}`} color="text-[var(--color-primary)]" />
            <SumCard label="总费用" value={`$${data.totalCost.toFixed(5)}`} sub="按费率表计算" color="text-[var(--color-success)]" />
            <SumCard label="供应商数" value={String(data.byProvider.length)} sub={`模型 ${data.byModel.length} 个`} color="text-[var(--color-warning)]" />
          </div>

          {/* Tab 切换：使用趋势 / 按供应商 / 按模型（保持挂载，避免趋势图重复请求） */}
          <div className="flex items-center gap-1 border-b border-[var(--color-border)]">
            {TABS.map((t) => (
              <button
                key={t.key}
                onClick={() => setTab(t.key)}
                className={`px-4 py-2 text-sm font-medium transition-colors relative -mb-px border-b-2 ${
                  tab === t.key
                    ? "border-[var(--color-primary)] text-[var(--color-primary)]"
                    : "border-transparent text-[var(--color-text-dim)] hover:text-[var(--color-text)]"
                }`}
              >
                {t.label}
              </button>
            ))}
          </div>

          {/* 使用趋势：单日（今日/昨日）按小时，跨天按天 */}
          <div className={tab === "trend" ? "" : "hidden"}>
            <UsageTrendChart
              startDate={calcRange().start}
              endDate={calcRange().end}
              granularity={trendGranularity}
            />
          </div>

          {/* 供应商维度 */}
          <section
            className={`bg-[var(--color-surface)] rounded-xl p-5 border border-[var(--color-border)] ${
              tab === "provider" ? "" : "hidden"
            }`}
          >
            <h2 className="font-semibold mb-3">按供应商统计</h2>
            {data.byProvider.length === 0 ? (
              <div className="text-sm text-[var(--color-text-dim)] text-center py-4">该范围无数据</div>
            ) : (
              <div className="space-y-3">
                {data.byProvider.map((p) => (
                  <div key={p.provider}>
                    <div className="flex items-center justify-between mb-1 text-sm">
                      <span className="font-medium">{p.providerLabel}</span>
                      <span className="text-[var(--color-text-dim)] text-xs">
                        {fmtTokens(p.tokens)} tokens · {p.requests} 请求 · ${p.cost.toFixed(5)}
                      </span>
                    </div>
                    <div className="h-2 rounded bg-[var(--color-bg)] overflow-hidden">
                      <div
                        className="h-full rounded bg-[var(--color-primary)]"
                        style={{ width: `${(p.tokens / maxProviderTokens) * 100}%` }}
                      />
                    </div>
                    <div className="text-xs text-[var(--color-text-dim)] mt-0.5">
                      输入 {fmtTokens(p.input)} · 输出 {fmtTokens(p.output)}
                    </div>
                  </div>
                ))}
              </div>
            )}
          </section>

          {/* 模型维度 */}
          <section
            className={`bg-[var(--color-surface)] rounded-xl p-5 border border-[var(--color-border)] ${
              tab === "model" ? "" : "hidden"
            }`}
          >
            <h2 className="font-semibold mb-3">按模型统计（实际使用）</h2>
            {data.byModel.length === 0 ? (
              <div className="text-sm text-[var(--color-text-dim)] text-center py-4">该范围无数据</div>
            ) : (
              <div className="space-y-3">
                {data.byModel.map((m) => (
                  <div key={m.model}>
                    <div className="flex items-center justify-between mb-1 text-sm">
                      <span className="font-mono">{m.modelLabel || m.model}</span>
                      <span className="text-[var(--color-text-dim)] text-xs">
                        {fmtTokens(m.tokens)} tokens · {m.requests} 请求 · {m.percent.toFixed(1)}% · ${m.cost.toFixed(5)}
                      </span>
                    </div>
                    <div className="h-2 rounded bg-[var(--color-bg)] overflow-hidden">
                      <div
                        className="h-full rounded bg-[var(--color-primary)]"
                        style={{ width: `${(m.tokens / maxModelTokens) * 100}%` }}
                      />
                    </div>
                    <div className="text-xs text-[var(--color-text-dim)] mt-0.5">
                      输入 {fmtTokens(m.input)} · 输出 {fmtTokens(m.output)} · 占比 {m.percent.toFixed(1)}%
                    </div>
                  </div>
                ))}
              </div>
            )}
          </section>
        </>
      )}
    </div>
  );
}

function RangeBtn({ label, active, onClick }: { label: string; active: boolean; onClick: () => void }) {
  return (
    <button
      onClick={onClick}
      className={`px-3 py-1.5 text-sm rounded-lg transition-colors ${
        active ? "bg-[var(--color-primary)] text-white" : "bg-[var(--color-surface)] hover:bg-[var(--color-surface-2)]"
      }`}
    >
      {label}
    </button>
  );
}

function SumCard({ label, value, sub, color }: { label: string; value: string; sub: string; color: string }) {
  return (
    <div className="bg-[var(--color-surface)] rounded-xl p-4 border border-[var(--color-border)]">
      <div className="text-xs text-[var(--color-text-dim)] mb-1">{label}</div>
      <div className={`text-xl font-bold ${color}`}>{value}</div>
      <div className="text-xs text-[var(--color-text-dim)] mt-0.5">{sub}</div>
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

// ====== DatePicker：自定义日历（与深色主题一致，替代原生 input[type=date]）======

const WEEKDAYS = ["一", "二", "三", "四", "五", "六", "日"];
const MONTHS = ["1月", "2月", "3月", "4月", "5月", "6月", "7月", "8月", "9月", "10月", "11月", "12月"];

function parseDate(s: string): Date | null {
  if (!s) return null;
  const [y, m, d] = s.split("-").map(Number);
  if (!y || !m || !d) return null;
  return new Date(y, m - 1, d);
}

function sameDay(a: Date | null, b: Date | null): boolean {
  return !!a && !!b && a.getFullYear() === b.getFullYear() && a.getMonth() === b.getMonth() && a.getDate() === b.getDate();
}

function DatePicker({
  value,
  onChange,
  min,
  max,
  placeholder,
}: {
  value: string;
  onChange: (v: string) => void;
  min?: string;
  max?: string;
  placeholder?: string;
}) {
  const [open, setOpen] = useState(false);
  const ref = useRef<HTMLDivElement>(null);
  const selected = parseDate(value);
  const today = new Date();
  // 日历视图月份：有选中值则定位到该月，否则当前月
  const [view, setView] = useState(() => {
    const d = selected ?? today;
    return { y: d.getFullYear(), m: d.getMonth() };
  });

  // 打开时把视图定位到选中日期所在月（或当月）
  useEffect(() => {
    if (open) {
      const d = selected ?? new Date();
      setView({ y: d.getFullYear(), m: d.getMonth() });
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open]);

  // 点击外部关闭
  useEffect(() => {
    if (!open) return;
    const handler = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener("mousedown", handler);
    return () => document.removeEventListener("mousedown", handler);
  }, [open]);

  const minD = parseDate(min ?? "");
  const maxD = parseDate(max ?? "");

  // 构造当月日历网格（周一开头，含上下月补齐）
  const first = new Date(view.y, view.m, 1);
  const startOffset = (first.getDay() + 6) % 7; // 周一=0
  const daysInMonth = new Date(view.y, view.m + 1, 0).getDate();
  const cells: { date: Date; inMonth: boolean }[] = [];
  for (let i = 0; i < startOffset; i++) {
    cells.push({ date: new Date(view.y, view.m, i - startOffset + 1), inMonth: false });
  }
  for (let d = 1; d <= daysInMonth; d++) {
    cells.push({ date: new Date(view.y, view.m, d), inMonth: true });
  }
  while (cells.length % 7 !== 0 || cells.length < 42) {
    const last = cells[cells.length - 1].date;
    cells.push({ date: new Date(last.getFullYear(), last.getMonth(), last.getDate() + 1), inMonth: false });
    if (cells.length >= 42) break;
  }

  const isDisabled = (d: Date) => {
    if (minD && d < stripTime(minD)) return true;
    if (maxD && d > stripTime(maxD)) return true;
    return false;
  };

  const moveMonth = (delta: number) => {
    setView((v) => {
      const nm = v.m + delta;
      return { y: v.y + Math.floor(nm / 12), m: ((nm % 12) + 12) % 12 };
    });
  };

  const displayText = value || placeholder || "选择日期";

  return (
    <div ref={ref} className="relative">
      <button
        type="button"
        onClick={() => setOpen((o) => !o)}
        className={`px-2 py-1 text-xs rounded-md border text-left flex items-center gap-1.5 transition-colors ${
          value
            ? "bg-[var(--color-surface-2)] border-[var(--color-border)] text-[var(--color-text)]"
            : "bg-[var(--color-surface-2)] border-[var(--color-border)] text-[var(--color-text-dim)]"
        } hover:border-[var(--color-primary)]`}
      >
        <span className="font-mono">{displayText}</span>
        <span className="text-[var(--color-text-dim)] text-[9px]">{open ? "▲" : "▼"}</span>
      </button>

      {open && (
        <div className="absolute z-50 mt-1 w-60 rounded-lg border border-[var(--color-border)] bg-[var(--color-surface)] shadow-2xl p-2.5 select-none">
          {/* 头部：上一月 / 年月 / 下一月 */}
          <div className="flex items-center justify-between mb-2">
            <button
              type="button"
              onClick={() => moveMonth(-1)}
              className="w-6 h-6 flex items-center justify-center rounded text-[var(--color-text-dim)] hover:bg-[var(--color-surface-2)] hover:text-[var(--color-text)]"
            >
              ‹
            </button>
            <div className="flex items-center gap-1 text-xs font-medium text-[var(--color-text)]">
              <span>{view.y}</span>
              <span className="text-[var(--color-text-dim)]">·</span>
              <span>{MONTHS[view.m]}</span>
            </div>
            <button
              type="button"
              onClick={() => moveMonth(1)}
              className="w-6 h-6 flex items-center justify-center rounded text-[var(--color-text-dim)] hover:bg-[var(--color-surface-2)] hover:text-[var(--color-text)]"
            >
              ›
            </button>
          </div>

          {/* 星期表头 */}
          <div className="grid grid-cols-7 gap-0.5 mb-1">
            {WEEKDAYS.map((w) => (
              <div key={w} className="text-center text-[10px] text-[var(--color-text-dim)] py-0.5">
                {w}
              </div>
            ))}
          </div>

          {/* 日期网格 */}
          <div className="grid grid-cols-7 gap-0.5">
            {cells.map((c, i) => {
              const disabled = isDisabled(c.date);
              const selected_d = sameDay(c.date, selected);
              const isToday = sameDay(c.date, today);
              return (
                <button
                  key={i}
                  type="button"
                  disabled={disabled}
                  onClick={() => {
                    onChange(fmtDate(c.date));
                    setOpen(false);
                  }}
                  className={`h-7 text-[11px] rounded-md flex items-center justify-center transition-colors ${
                    !c.inMonth
                      ? "text-[var(--color-text-dim)]/30"
                      : selected_d
                      ? "bg-[var(--color-primary)] text-white font-medium"
                      : isToday
                      ? "text-[var(--color-primary)] hover:bg-[var(--color-surface-2)] ring-1 ring-inset ring-[var(--color-primary)]/40"
                      : "text-[var(--color-text)] hover:bg-[var(--color-surface-2)]"
                  } ${disabled ? "opacity-25 cursor-not-allowed" : "cursor-pointer"}`}
                >
                  {c.date.getDate()}
                </button>
              );
            })}
          </div>

          {/* 底部：今天 + 清除 */}
          <div className="flex items-center justify-between mt-2 pt-2 border-t border-[var(--color-border)]">
            <button
              type="button"
              onClick={() => {
                onChange(fmtDate(new Date()));
                setOpen(false);
              }}
              className="text-[11px] text-[var(--color-primary)] hover:underline"
            >
              今天
            </button>
            {value && (
              <button
                type="button"
                onClick={() => {
                  onChange("");
                  setOpen(false);
                }}
                className="text-[11px] text-[var(--color-text-dim)] hover:text-[var(--color-danger)]"
              >
                清除
              </button>
            )}
          </div>
        </div>
      )}
    </div>
  );
}

function stripTime(d: Date): Date {
  return new Date(d.getFullYear(), d.getMonth(), d.getDate());
}
