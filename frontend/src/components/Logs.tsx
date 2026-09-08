import { useEffect, useState, useCallback, useMemo, useRef } from "react";
import { LogService } from "../../bindings/switchdev/service";
import type { LogEntry } from "../../bindings/switchdev/proxy/models";
import ConfirmPopover from "./ConfirmPopover";

// 复制到剪贴板
async function copyText(text: string) {
  try {
    await navigator.clipboard.writeText(text);
  } catch {
    // fallback
    const ta = document.createElement("textarea");
    ta.value = text;
    document.body.appendChild(ta);
    ta.select();
    document.execCommand("copy");
    document.body.removeChild(ta);
  }
}

interface Props {
  logs: LogEntry[]; // 内存实时日志（最新）
}

// 各状态计数（按当前时间范围从数据库统计）
interface StatusCounts {
  total: number;
  success: number;
  error: number;
  authError: number;
  fallback: number;
}

type Range = "today" | "week" | "month";

type Filter = "all" | "success" | "error" | "auth_error" | "fallback";

const PAGE_SIZE_OPTIONS = [20, 50, 100, 200];

// 日期工具（本应用内纯函数，不依赖外部库）
const pad = (n: number) => String(n).padStart(2, "0");
function fmtDate(d: Date): string {
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
}
function dateAddDays(d: Date, days: number): Date {
  return new Date(d.getFullYear(), d.getMonth(), d.getDate() + days);
}
function startOfWeek(d: Date): Date {
  // 周一为一周开始
  const day = d.getDay() || 7; // 0=周日 -> 7
  return dateAddDays(d, 1 - day);
}

export default function Logs({ logs }: Props) {
  const [range, setRange] = useState<Range>("today");
  const [filter, setFilter] = useState<Filter>("all");
  const [history, setHistory] = useState<LogEntry[]>([]);
  const [availableDates, setAvailableDates] = useState<string[]>([]);
  const [expandedId, setExpandedId] = useState<string | null>(null);
  const [selectedLog, setSelectedLog] = useState<LogEntry | null>(null);
  const [counts, setCounts] = useState<StatusCounts>({
    total: 0, success: 0, error: 0, authError: 0, fallback: 0,
  });
  const [loading, setLoading] = useState(false);
  // 分页：page 从 0 起；pageTotal 是当前范围+筛选下的总条数（后端给）
  const [page, setPage] = useState(0);
  const [pageSize, setPageSize] = useState(50);
  const [pageTotal, setPageTotal] = useState(0);
  // 已计入 DB 计数的实时日志 id（loadHistory 时用当前内存日志初始化）；
  // 之后到达的新日志才实时累加，避免与 DB 统计重复计数。
  const countedLiveRef = useRef<Set<string>>(new Set());
  // 已并入实时列表的日志 id（page 0 去重用，避免多次 effect 重复插入）
  const mergedLiveRef = useRef<Set<string>>(new Set());
  // 始终指向最新内存日志，供 loadHistory 初始化游标用（不进依赖，避免频繁重载）
  const logsRef = useRef<LogEntry[]>(logs);
  logsRef.current = logs;

  // 计算当前范围对应的日期区间
  const calcRange = useCallback((): { start: string; end: string } => {
    const today = new Date();
    const todayStr = fmtDate(today);
    if (range === "today") return { start: todayStr, end: todayStr };
    if (range === "week") {
      const start = startOfWeek(today);
      return { start: fmtDate(start), end: todayStr };
    }
    // month
    const start = new Date(today.getFullYear(), today.getMonth(), 1);
    return { start: fmtDate(start), end: todayStr };
  }, [range]);

  // 拉取当前页日志（按范围 + 状态筛选，均下推到 SQL）+ 该范围内各状态计数
  const loadHistory = useCallback(async () => {
    setLoading(true);
    try {
      const { start, end } = calcRange();
      const status = filter === "all" ? "" : filter;
      const [pageData, statusCounts, dates] = await Promise.all([
        LogService.GetLogPage(start, end, status, pageSize, page * pageSize),
        LogService.LogCountsByRange(start, end),
        LogService.GetLogDates(),
      ]);
      setHistory((pageData?.logs ?? []).filter((l): l is LogEntry => l !== null));
      setPageTotal(pageData?.total ?? 0);
      // 后端会在 offset 越界时回退到末页，前端页码要跟上，避免翻页按钮状态错乱
      if (pageData && pageData.limit > 0) {
        const serverPage = Math.floor(pageData.offset / pageData.limit);
        if (serverPage !== page) setPage(serverPage);
      }
      setAvailableDates((dates ?? []).filter((d): d is string => d !== null));
      const sc = statusCounts ?? {};
      setCounts({
        total: (sc["success"] ?? 0) + (sc["error"] ?? 0) + (sc["auth_error"] ?? 0) + (sc["fallback"] ?? 0),
        success: sc["success"] ?? 0,
        error: sc["error"] ?? 0,
        authError: sc["auth_error"] ?? 0,
        fallback: sc["fallback"] ?? 0,
      });
      // 当前内存里的实时日志（DB 查询时基本已落库）标记为"已计数"，
      // 之后到达的新日志才在实时 effect 里累加，避免与 DB 统计重复。
      countedLiveRef.current = new Set(logsRef.current.map((l) => l.id));
      // 本页已由 SQL 给出，实时并入游标重置为本页内容
      mergedLiveRef.current = new Set((pageData?.logs ?? []).map((l) => l?.id ?? ""));
    } catch (e) {
      console.error("加载日志失败", e);
    } finally {
      setLoading(false);
    }
  }, [calcRange, filter, page, pageSize]);

  // range/filter/page/pageSize 任一变化都重新加载（分页与筛选都在后端做）
  useEffect(() => {
    loadHistory();
  }, [loadHistory]);

  // 切范围或换筛选时回到第一页（否则会停在新条件下不存在的页码上）
  useEffect(() => {
    setPage(0);
  }, [range, filter, pageSize]);

  // 翻页后展开的行已不在当前页，收起避免残留
  useEffect(() => {
    setExpandedId(null);
  }, [page, pageSize, range, filter]);

  // 实时新日志：只在「今天 + 第一页」时并入列表，其它页码/范围下并入会打乱分页顺序。
  // 计数不受此限制 —— 筛选按钮上的数字始终反映最新状态。
  useEffect(() => {
    if (range !== "today" || logs.length === 0) return;

    if (page === 0) {
      const status = filter === "all" ? null : filter;
      const merged = mergedLiveRef.current;
      const fresh = logs.filter(
        (l) => !merged.has(l.id) && (status === null || l.status === status)
      );
      if (fresh.length > 0) {
        fresh.forEach((l) => merged.add(l.id));
        setHistory((prev) => [...fresh, ...prev].slice(0, pageSize));
        setPageTotal((prev) => prev + fresh.length);
      }
    }

    // 计数：只累加 loadHistory 之后新到达、尚未计过的日志
    const counted = countedLiveRef.current;
    const toCount = logs.filter((l) => !counted.has(l.id));
    if (toCount.length === 0) return;
    toCount.forEach((l) => counted.add(l.id));
    setCounts((prev) => {
      const next = { ...prev, total: prev.total + toCount.length };
      toCount.forEach((l) => {
        if (l.status === "success") next.success++;
        else if (l.status === "error") next.error++;
        else if (l.status === "auth_error") next.authError++;
        else if (l.status === "fallback") next.fallback++;
      });
      return next;
    });
  }, [logs, range, page, pageSize, filter]);

  // 列表已按筛选条件从后端取回，不再二次过滤
  const filtered = history;
  const totalPages = Math.max(1, Math.ceil(pageTotal / pageSize));

  // 本页内各状态计数（已加载的 history 就是当前页数据）
  const pageCounts = useMemo(() => {
    const c: StatusCounts = { total: 0, success: 0, error: 0, authError: 0, fallback: 0 };
    for (const l of history) {
      c.total++;
      if (l.status === "success") c.success++;
      else if (l.status === "error") c.error++;
      else if (l.status === "auth_error") c.authError++;
      else if (l.status === "fallback") c.fallback++;
    }
    return c;
  }, [history]);

  const clear = async () => {
    await LogService.ClearLogs();
    setHistory([]);
    setPage(0);
    setPageTotal(0);
    setCounts({ total: 0, success: 0, error: 0, authError: 0, fallback: 0 });
  };

  return (
    <div className="p-6 space-y-4">
      {/* 时间范围切换 + 刷新/清空（同一行，右侧） */}
      <div className="flex items-center gap-2 flex-wrap">
        <RangeBtn label="今天" active={range === "today"} onClick={() => setRange("today")} />
        <RangeBtn label="本周" active={range === "week"} onClick={() => setRange("week")} />
        <RangeBtn label="本月" active={range === "month"} onClick={() => setRange("month")} />
        <span className="text-xs text-[var(--color-text-dim)] ml-2">
          范围：{calcRange().start} ~ {calcRange().end}
        </span>
        <div className="flex gap-2 ml-auto">
          <button
            onClick={loadHistory}
            disabled={loading}
            className="px-3 py-1.5 text-sm rounded-lg bg-[var(--color-surface-2)] hover:bg-[var(--color-border)] disabled:opacity-50"
          >
            {loading ? "加载中..." : "🔄 刷新"}
          </button>
          <ConfirmPopover
            onConfirm={clear}
            title={"清空后日志不可恢复，但用量统计数据会保留。\n确认清空所有日志？"}
            confirmLabel="清空"
            triggerClassName="px-3 py-1.5 text-sm rounded-lg bg-[var(--color-surface-2)] hover:bg-[var(--color-border)]"
          >
            🗑 清空
          </ConfirmPopover>
        </div>
      </div>

      {/* 状态筛选 + 统计（前面本页，后面全范围） */}
      <div className="flex items-center gap-2 flex-wrap">
        <FilterBtn label="全部" pageCount={pageCounts.total} total={counts.total} active={filter === "all"} onClick={() => setFilter("all")} color="text-[var(--color-text)]" />
        <FilterBtn label="成功" pageCount={pageCounts.success} total={counts.success} active={filter === "success"} onClick={() => setFilter("success")} color="text-[var(--color-success)]" />
        <FilterBtn label="错误" pageCount={pageCounts.error} total={counts.error} active={filter === "error"} onClick={() => setFilter("error")} color="text-[var(--color-warning)]" />
        <FilterBtn label="鉴权失败" pageCount={pageCounts.authError} total={counts.authError} active={filter === "auth_error"} onClick={() => setFilter("auth_error")} color="text-[var(--color-danger)]" />
        <FilterBtn label="降级" pageCount={pageCounts.fallback} total={counts.fallback} active={filter === "fallback"} onClick={() => setFilter("fallback")} color="text-[var(--color-text-dim)]" />
        <span className="text-xs text-[var(--color-text-dim)] ml-1">本页 / 全范围</span>
      </div>

      {/* 日志列表 */}
      <div className="bg-[var(--color-surface)] rounded-xl border border-[var(--color-border)] overflow-hidden">
        {filtered.length > 0 && (
          <div className="px-4 py-2 border-b border-[var(--color-border)] flex items-center gap-3 text-xs text-[var(--color-text-dim)] font-medium uppercase tracking-wide whitespace-nowrap">
            <span className="w-24">时间</span>
            <span className="w-20">来源</span>
            <span className="w-28">请求模型</span>
            <span className="w-28">实际模型</span>
            <span className="w-24">代理</span>
            <span className="w-28">真实模型</span>
            <span className="w-24">输入/输出</span>
            <span className="w-24">费用</span>
            <span className="w-16">状态</span>
            <span className="w-14">用时</span>
            <span className="w-6"></span>
          </div>
        )}
        {filtered.length === 0 ? (
          <div className="p-8 text-center text-[var(--color-text-dim)] text-sm">
            {loading ? "加载中..." : "该时间范围暂无日志"}
          </div>
        ) : (
          <div className="divide-y divide-[var(--color-border)]">
            {filtered.map((log) => (
              <LogRow
                key={log.id}
                log={log}
                expanded={expandedId === log.id}
                onToggle={() => setExpandedId(expandedId === log.id ? null : log.id)}
                onDoubleClick={() => setSelectedLog(log)}
              />
            ))}
          </div>
        )}

        {/* 分页 */}
        <div className="flex items-center gap-3 px-4 py-3 border-t border-[var(--color-border)] text-sm flex-wrap">
          <div className="flex items-center gap-1.5">
            <span className="text-xs text-[var(--color-text-dim)]">每页</span>
            <select
              value={pageSize}
              onChange={(e) => setPageSize(Number(e.target.value))}
              className="px-2 py-1 text-xs rounded bg-[var(--color-surface-2)] border border-[var(--color-border)]"
            >
              {PAGE_SIZE_OPTIONS.map((n) => (
                <option key={n} value={n}>{n}</option>
              ))}
            </select>
          </div>
          <span className="text-xs text-[var(--color-text-dim)]">
            共 {pageTotal} 条
            {pageTotal > 0 && `，第 ${page * pageSize + 1}-${Math.min((page + 1) * pageSize, pageTotal)} 条`}
          </span>
          <div className="flex items-center gap-2 ml-auto">
            <button
              onClick={() => setPage(0)}
              disabled={page === 0 || loading}
              className="px-2 py-1 text-xs rounded bg-[var(--color-surface-2)] hover:bg-[var(--color-border)] disabled:opacity-30"
            >
              « 首页
            </button>
            <button
              onClick={() => setPage((p) => Math.max(0, p - 1))}
              disabled={page === 0 || loading}
              className="px-2 py-1 text-xs rounded bg-[var(--color-surface-2)] hover:bg-[var(--color-border)] disabled:opacity-30"
            >
              上一页
            </button>
            <span className="text-xs text-[var(--color-text-dim)] font-mono">
              {page + 1} / {totalPages}
            </span>
            <button
              onClick={() => setPage((p) => Math.min(totalPages - 1, p + 1))}
              disabled={page >= totalPages - 1 || loading}
              className="px-2 py-1 text-xs rounded bg-[var(--color-surface-2)] hover:bg-[var(--color-border)] disabled:opacity-30"
            >
              下一页
            </button>
            <button
              onClick={() => setPage(totalPages - 1)}
              disabled={page >= totalPages - 1 || loading}
              className="px-2 py-1 text-xs rounded bg-[var(--color-surface-2)] hover:bg-[var(--color-border)] disabled:opacity-30"
            >
              末页 »
            </button>
          </div>
        </div>
      </div>

      {/* 历史日期提示 */}
      {availableDates.length > 1 && (
        <div className="text-xs text-[var(--color-text-dim)]">
          历史日志日期：{availableDates.join(" / ")}
        </div>
      )}

      {/* 日志详情弹窗 */}
      {selectedLog && (
        <LogDetailModal log={selectedLog} onClose={() => setSelectedLog(null)} />
      )}
    </div>
  );
}

function RangeBtn({ label, active, onClick }: { label: string; active: boolean; onClick: () => void }) {
  return (
    <button
      onClick={onClick}
      className={`px-3 py-1.5 text-sm rounded-lg transition-colors ${
        active
          ? "bg-[var(--color-primary)] text-white"
          : "bg-[var(--color-surface)] hover:bg-[var(--color-surface-2)] text-[var(--color-text)]"
      }`}
    >
      {label}
    </button>
  );
}

function FilterBtn({
  label,
  pageCount,
  total,
  active,
  onClick,
  color,
}: {
  label: string;
  pageCount: number;
  total: number;
  active: boolean;
  onClick: () => void;
  color: string;
}) {
  return (
    <button
      onClick={onClick}
      title={`本页 ${pageCount} 条 / 当前时间范围共 ${total} 条`}
      className={`px-3 py-1.5 text-sm rounded-lg flex items-center gap-2 transition-colors ${
        active ? "bg-[var(--color-primary)] text-white" : "bg-[var(--color-surface)] hover:bg-[var(--color-surface-2)]"
      }`}
    >
      <span>{label}</span>
      <span className={`font-mono text-xs ${active ? "text-white/80" : color}`}>
        {pageCount}
        <span className={active ? "text-white/50" : "text-[var(--color-text-dim)]"}>/{total}</span>
      </span>
    </button>
  );
}

function LogRow({ log, expanded, onToggle, onDoubleClick }: { log: LogEntry; expanded: boolean; onToggle: () => void; onDoubleClick: () => void }) {
  const color =
    log.status === "success"
      ? "text-[var(--color-success)]"
      : log.status === "auth_error"
      ? "text-[var(--color-danger)]"
      : log.status === "fallback"
      ? "text-[var(--color-text-dim)]"
      : "text-[var(--color-warning)]";

  return (
    <div>
      {/* 概要行（点击展开详情，双击查看完整请求/响应） */}
      <button onClick={onToggle} onDoubleClick={onDoubleClick} className="w-full px-4 py-2.5 hover:bg-[var(--color-surface-2)]/50 text-left">
        <div className="flex items-center gap-3 text-sm">
          <span className="text-[var(--color-text-dim)] font-mono text-xs w-24 whitespace-nowrap">{fmtLogTime(log)}</span>
          <span className="text-xs w-20 truncate" title={log.source || ""}>{log.source || "-"}</span>
          <span className="font-mono text-xs w-28 truncate text-[var(--color-text-dim)]" title={log.model}>{log.model}</span>
          <span className="font-mono text-xs w-28 truncate" title={log.usedModel || log.realModel || ""}>
            {log.usedModel || log.realModel || "-"}
          </span>
          <span className="text-[var(--color-text-dim)] text-xs w-24 truncate" title={log.upstream}>
            {agentLabel(log.upstream)}
          </span>
          <span className="font-mono text-xs w-28 truncate text-[var(--color-text-dim)]" title={log.realModel}>
            {log.realModel || "-"}
          </span>
          <span className="font-mono text-xs w-24">{tokenText(log)}</span>
          <span className="font-mono text-xs w-24 text-[var(--color-primary)]" title={log.costText}>
            {costText(log)}
          </span>
          <span className={`w-16 truncate ${color}`} title={log.errorMsg}>
            {log.status === "success" ? "成功" : log.status === "auth_error" ? "鉴权失败" : log.status === "fallback" ? "降级" : "错误"}
          </span>
          <span className="text-[var(--color-text-dim)] text-xs font-mono w-14">{log.duration}ms</span>
          <span className="text-[var(--color-text-dim)] text-xs w-6 text-center">{expanded ? "▲" : "▼"}</span>
        </div>
      </button>

      {/* 详情（展开时显示） */}
      {expanded && (
        <div className="px-4 pb-3 space-y-2 border-t border-[var(--color-border)]/50">
          <div className="flex gap-3 text-xs text-[var(--color-text-dim)] pt-2 flex-wrap">
            <span>时间：{log.dateTime || log.timestamp}</span>
            <span>来源：{log.source || "-"}</span>
            <span>接口：{log.method || "?"} {log.path || "?"}</span>
            <span>代理：{agentLabel(log.upstream)}</span>
            <span>请求模型：{log.model}</span>
            <span>实际模型：{log.usedModel || "-"}</span>
            <span>真实模型：{log.realModel || "-"}</span>
            <span>输入/输出：{log.inputTokens ?? 0} / {log.outputTokens ?? 0} tokens</span>
            <span>费用：{costText(log)}</span>
            <span>耗时：{log.duration}ms</span>
            {(log.firstByteMs ?? 0) > 0 && <span>首字：{log.firstByteMs}ms</span>}
            <span>状态码：{log.code || "-"}</span>
          </div>
          {log.costText && (
            <div className="text-xs text-[var(--color-text-dim)]">费率：{log.costText}</div>
          )}
          {log.errorMsg && (
            <div className="text-xs text-[var(--color-danger)]">错误：{log.errorMsg}</div>
          )}
          {log.requestBody && (
            <div>
              <div className="text-xs text-[var(--color-text-dim)] mb-1">请求体：</div>
              <pre className="text-xs font-mono p-2 rounded bg-[var(--color-bg)] overflow-x-auto whitespace-pre-wrap max-h-40 overflow-y-auto">
                {prettyJSON(log.requestBody)}
              </pre>
            </div>
          )}
          {log.responseBody && (
            <div>
              <div className="text-xs text-[var(--color-text-dim)] mb-1">响应体：</div>
              <pre className="text-xs font-mono p-2 rounded bg-[var(--color-bg)] overflow-x-auto whitespace-pre-wrap max-h-40 overflow-y-auto">
                {prettyJSON(log.responseBody)}
              </pre>
            </div>
          )}
        </div>
      )}
    </div>
  );
}

// LogDetailModal 双击日志行后的详情弹窗，显示完整请求/响应体
function LogDetailModal({ log, onClose }: { log: LogEntry; onClose: () => void }) {
  // Escape 关闭
  useEffect(() => {
    const handler = (e: KeyboardEvent) => { if (e.key === "Escape") onClose(); };
    window.addEventListener("keydown", handler);
    return () => window.removeEventListener("keydown", handler);
  }, [onClose]);

  const [copied, setCopied] = useState<"req" | "resp" | null>(null);

  const handleCopy = async (text: string, which: "req" | "resp") => {
    await copyText(text);
    setCopied(which);
    setTimeout(() => setCopied(null), 1500);
  };

  return (
    <div className="fixed inset-0 z-[300] flex items-center justify-center bg-black/50" onClick={onClose}>
      <div
        className="bg-[var(--color-surface)] rounded-xl border border-[var(--color-border)] shadow-2xl max-w-4xl w-full mx-4 max-h-[85vh] flex flex-col"
        onClick={(e) => e.stopPropagation()}
      >
        {/* Header */}
        <div className="flex items-center justify-between px-5 py-3 border-b border-[var(--color-border)] shrink-0">
          <div className="text-sm font-medium">
            {log.method || "POST"} {log.path || "/v1/messages"} — {log.model}
            {log.realModel && log.realModel !== log.model && ` → ${log.realModel}`}
          </div>
          <button onClick={onClose} className="text-[var(--color-text-dim)] hover:text-[var(--color-text)] text-lg leading-none px-1">&times;</button>
        </div>

        {/* Body */}
        <div className="overflow-y-auto flex-1 px-5 py-3 space-y-4">
          {/* 元信息 */}
          <div className="flex gap-3 text-xs text-[var(--color-text-dim)] flex-wrap">
            <span>时间：{log.dateTime || log.timestamp}</span>
            <span>来源：{log.source || "-"}</span>
            <span>代理：{agentLabel(log.upstream)}</span>
            <span>状态：{log.status} ({log.code})</span>
            <span>耗时：{log.duration}ms</span>
            {(log.firstByteMs ?? 0) > 0 && <span>首字：{log.firstByteMs}ms</span>}
            <span>Token：↑{log.inputTokens ?? 0} ↓{log.outputTokens ?? 0}</span>
            {log.cost != null && log.cost > 0 && <span>费用：${log.cost.toFixed(5)}</span>}
          </div>
          {log.errorMsg && (
            <div className="text-xs text-[var(--color-danger)]">错误：{log.errorMsg}</div>
          )}

          {/* 请求体 */}
          {log.requestBody && (
            <div>
              <div className="flex items-center gap-2 mb-1">
                <span className="text-xs text-[var(--color-text-dim)] font-medium">请求体</span>
                <button
                  onClick={() => handleCopy(prettyJSON(log.requestBody!), "req")}
                  className="text-xs px-2 py-0.5 rounded bg-[var(--color-surface-2)] hover:bg-[var(--color-border)] text-[var(--color-text-dim)]"
                >
                  {copied === "req" ? "已复制" : "复制"}
                </button>
              </div>
              <pre className="text-xs font-mono p-3 rounded bg-[var(--color-bg)] overflow-x-auto whitespace-pre-wrap max-h-[35vh] overflow-y-auto">
                {prettyJSON(log.requestBody)}
              </pre>
            </div>
          )}

          {/* 响应体 */}
          {log.responseBody && (
            <div>
              <div className="flex items-center gap-2 mb-1">
                <span className="text-xs text-[var(--color-text-dim)] font-medium">响应体</span>
                <button
                  onClick={() => handleCopy(prettyJSON(log.responseBody!), "resp")}
                  className="text-xs px-2 py-0.5 rounded bg-[var(--color-surface-2)] hover:bg-[var(--color-border)] text-[var(--color-text-dim)]"
                >
                  {copied === "resp" ? "已复制" : "复制"}
                </button>
              </div>
              <pre className="text-xs font-mono p-3 rounded bg-[var(--color-bg)] overflow-x-auto whitespace-pre-wrap max-h-[35vh] overflow-y-auto">
                {prettyJSON(log.responseBody)}
              </pre>
            </div>
          )}

          {!log.requestBody && !log.responseBody && (
            <div className="text-xs text-[var(--color-text-dim)] text-center py-4">无请求/响应体数据</div>
          )}
        </div>
      </div>
    </div>
  );
}

// prettyJSON 尝试美化 JSON，失败原样返回
function prettyJSON(s: string): string {
  try {
    const parsed = JSON.parse(s);
    return JSON.stringify(parsed, null, 2);
  } catch {
    return s;
  }
}

// agentLabel 上游显示名
function agentLabel(upstream: string): string {
  switch (upstream) {
    case "joycode":
      return "京东 JoyCode";
    case "deveco":
      return "华为 DevEco";
    default:
      return upstream || "-";
  }
}

// fmtLogTime 将日志时间格式化为 "MM/DD HH:mm"
// dateTime: "2026-08-08 15:04:05" -> "08/08 15:04"
// 兜底用 timestamp ("15:04:05") 或 date+timestamp 拼接
function fmtLogTime(log: LogEntry): string {
  if (log.dateTime && log.dateTime.length >= 16) {
    // "2026-08-08 15:04:05" -> 取 "08/08 15:04"
    const parts = log.dateTime.split(" ");
    if (parts.length === 2) {
      const dateParts = parts[0].split("-");
      const timePart = parts[1].substring(0, 5); // "HH:mm"
      if (dateParts.length === 3) {
        return `${dateParts[1]}/${dateParts[2]} ${timePart}`;
      }
    }
  }
  // 兜底：用 date + timestamp
  if (log.date && log.timestamp) {
    const dateParts = log.date.split("-");
    const timePart = log.timestamp.substring(0, 5);
    if (dateParts.length === 3) {
      return `${dateParts[1]}/${dateParts[2]} ${timePart}`;
    }
  }
  return log.timestamp || "-";
}

// tokenText token 用量文本
function tokenText(log: LogEntry): string {
  if (log.inputTokens !== undefined && log.outputTokens !== undefined) {
    return `↑${log.inputTokens} ↓${log.outputTokens}`;
  }
  return "-";
}

// costText 费用文本（$，3 位小数）
function costText(log: LogEntry): string {
  if (log.cost !== undefined && log.cost > 0) {
    return `$${log.cost.toFixed(5)}`;
  }
  return "-";
}
