import { useEffect, useState } from "react";
import { UpdaterService } from "../../bindings/switchdev/service";
import type { UpdateInfo } from "../../bindings/switchdev/updater/models";
import { useWailsEvent } from "../hooks/useWailsEvent";

export default function UpdatePanel() {
  const [currentVersion, setCurrentVersion] = useState<string>("");
  const [checking, setChecking] = useState(false);
  const [updating, setUpdating] = useState(false);
  const [restarting, setRestarting] = useState(false);
  const [updateInfo, setUpdateInfo] = useState<UpdateInfo | null>(null);
  const [progress, setProgress] = useState<{ percent: number; message: string } | null>(null);
  const [msg, setMsg] = useState<{ type: "ok" | "err"; text: string } | null>(null);

  // 初始拉取当前版本
  useEffect(() => {
    UpdaterService.GetCurrentVersion().then((v) => setCurrentVersion(v ?? ""));
  }, []);

  // 后台定时检查（main 启动时 + 每 6 小时推送 update:available）
  useWailsEvent("update:available", (data) => {
    setUpdateInfo(data as UpdateInfo);
  });

  // 下载进度
  useWailsEvent("update:progress", (data) => {
    const s = data as { state: string; percent: number; message: string };
    if (s.state === "restarting") {
      setProgress({ percent: 100, message: s.message || "更新完成，正在重启…" });
      setRestarting(true);
      setUpdating(true); // 重启中保持按钮禁用
    } else if (s.state === "done") {
      setProgress({ percent: 100, message: s.message || "更新完成，请重启应用" });
      setUpdating(false);
    } else if (s.state === "error") {
      setMsg({ type: "err", text: s.message || "更新失败" });
      setUpdating(false);
    } else {
      setProgress({ percent: s.percent || 0, message: s.message || "下载中..." });
    }
  });

  const check = async () => {
    setChecking(true);
    setMsg(null);
    try {
      const info = await UpdaterService.CheckUpdate();
      if (info) {
        setUpdateInfo(info);
      } else {
        setMsg({ type: "ok", text: "当前已是最新版本" });
      }
    } catch (e) {
      setMsg({ type: "err", text: `检查失败: ${e}` });
    } finally {
      setChecking(false);
    }
  };

  const apply = async () => {
    if (!updateInfo) return;
    setUpdating(true);
    setMsg(null);
    try {
      await UpdaterService.ApplyUpdate(updateInfo);
      // done 事件会处理
    } catch (e) {
      setMsg({ type: "err", text: `更新失败: ${e}` });
      setUpdating(false);
    }
  };

  const isCritical = updateInfo?.critical ?? false;

  return (
    <section className="bg-[var(--color-surface)] rounded-xl p-5 border border-[var(--color-border)]">
      <div className="flex items-center justify-between mb-3">
        <h2 className="font-semibold">自动升级</h2>
        <span className="text-xs px-2 py-1 rounded bg-[var(--color-surface-2)] text-[var(--color-text-dim)]">
          当前版本 {currentVersion || "-"}
        </span>
      </div>

      {msg && (
        <div className={`mb-3 px-4 py-2 rounded-lg text-sm whitespace-pre-wrap leading-relaxed ${msg.type === "ok" ? "bg-[var(--color-success)]/20 text-[var(--color-success)]" : "bg-[var(--color-danger)]/20 text-[var(--color-danger)]"}`}>
          {msg.text}
        </div>
      )}

      {updateInfo ? (
        <div className="space-y-3">
          {/* 强制更新横幅 */}
          {isCritical && (
            <div className="px-3 py-2 rounded-lg bg-[var(--color-danger)]/15 border border-[var(--color-danger)]/40 text-xs text-[var(--color-danger)]">
              ⚠️ 这是重要版本更新（{currentVersion} → {updateInfo.version}），包含不兼容变更或关键修复，请尽快更新。
            </div>
          )}

          <div className="flex items-center gap-2 flex-wrap">
            <span className="text-sm">发现新版本：</span>
            {/* 当前版本 -> 新版本，标签化避免把当前版本误读为检测到的版本 */}
            <span className="font-mono text-sm text-[var(--color-text-dim)]">{currentVersion || "-"}</span>
            <span className="text-[var(--color-text-dim)]">→</span>
            <span className="font-mono text-sm font-bold text-[var(--color-primary)]">{updateInfo.version}</span>
            <span
              className={`text-[10px] px-1.5 py-0.5 rounded-full ${
                isCritical
                  ? "bg-[var(--color-danger)]/20 text-[var(--color-danger)]"
                  : "bg-[var(--color-primary)]/20 text-[var(--color-primary)]"
              }`}
            >
              {isCritical ? "强制更新" : "可选更新"}
            </span>
            {updateInfo.assetSize ? (
              <span className="text-xs text-[var(--color-text-dim)]">
                ({(updateInfo.assetSize / 1024 / 1024).toFixed(1)} MB)
              </span>
            ) : null}
          </div>

          {/* 更新日志（changelog） */}
          {updateInfo.notes && (
            <div>
              <div className="text-xs font-medium text-[var(--color-text-dim)] mb-1">更新内容</div>
              <div className="text-xs text-[var(--color-text)] whitespace-pre-wrap bg-[var(--color-bg)] rounded-lg p-3 max-h-60 overflow-y-auto leading-relaxed">
                {updateInfo.notes}
              </div>
            </div>
          )}

          {/* 下载进度 */}
          {progress && (
            <div>
              <div className="flex justify-between text-xs text-[var(--color-text-dim)] mb-1">
                <span>{progress.message}</span>
                <span>{Math.round(progress.percent)}%</span>
              </div>
              <div className="h-2 rounded bg-[var(--color-bg)] overflow-hidden">
                <div className="h-full rounded bg-[var(--color-primary)] transition-all" style={{ width: `${progress.percent}%` }} />
              </div>
            </div>
          )}

          <div className="flex gap-2">
            <button
              onClick={apply}
              disabled={updating}
              className={`px-4 py-1.5 text-sm rounded-lg hover:opacity-90 disabled:opacity-50 ${
                isCritical ? "bg-[var(--color-danger)]" : "bg-[var(--color-primary)]"
              }`}
            >
              {restarting ? "正在重启..." : updating ? "更新中..." : isCritical ? "立即更新（必需）" : "立即更新"}
            </button>
            {/* 强制更新不提供「忽略」；仅可选更新可稍后 */}
            {!isCritical && (
              <button
                onClick={() => setUpdateInfo(null)}
                disabled={updating}
                className="px-3 py-1.5 text-sm rounded-lg bg-[var(--color-surface-2)] hover:bg-[var(--color-border)] disabled:opacity-50"
              >
                稍后再说
              </button>
            )}
          </div>
        </div>
      ) : (
        <div className="flex items-center justify-between gap-4">
          <p className="text-xs text-[var(--color-text-dim)]">
            启动时及每 2 小时自动检查 GitHub Releases，发现新版本会自动提示。更新会替换当前二进制。
          </p>
          <button
            onClick={check}
            disabled={checking}
            className="px-4 py-1.5 text-sm rounded-lg bg-[var(--color-primary)] hover:opacity-90 disabled:opacity-50 whitespace-nowrap shrink-0"
          >
            {checking ? "检查中..." : "🔍 立即检查"}
          </button>
        </div>
      )}
    </section>
  );
}
