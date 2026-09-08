import { useEffect, useRef, useState } from "react";
import type { Preset } from "../../bindings/switchdev/config/models";
import ConfirmPopover from "./ConfirmPopover";
import TrashIcon from "./TrashIcon";

// PresetSwitcher 运行模式方案下拉 + 保存按钮
//
// 方案是快照语义：保存冻结当前配置，切换覆盖回当前配置。
// 切换后继续编辑不会回写方案，此时 activePreset 被置空，下拉显示「自定义」。
export default function PresetSwitcher({
  presets,
  activePreset,
  busy,
  onApply,
  onSave,
  onDelete,
  onRename,
  onClearActive,
}: {
  presets: Preset[];
  activePreset: string;
  busy: boolean;
  onApply: (name: string) => void;
  onSave: (name: string) => void;
  onDelete: (name: string) => void;
  onRename: (oldName: string, newName: string) => void;
  onClearActive: () => void;
}) {
  const [open, setOpen] = useState(false);
  const ref = useRef<HTMLDivElement>(null);
  // 保存弹窗：null=关闭，否则为输入中的名字
  const [saveName, setSaveName] = useState<string | null>(null);
  // 重命名弹窗：{old, next}
  const [renaming, setRenaming] = useState<{ old: string; next: string } | null>(null);

  // 点击外部关闭下拉（与 ModelSelect 一致）
  // 确认气泡通过 portal 挂到 body，点它时不应关闭下拉，否则气泡会在 click 前被卸载
  useEffect(() => {
    if (!open) return;
    const handler = (e: MouseEvent) => {
      const t = e.target as HTMLElement;
      if (ref.current?.contains(t) || t.closest("[data-confirm-popover]")) return;
      setOpen(false);
    };
    document.addEventListener("mousedown", handler);
    return () => document.removeEventListener("mousedown", handler);
  }, [open]);

  // 打开保存弹窗时预填当前方案名，方便更新同名方案
  const openSave = () => {
    setSaveName(activePreset || "");
    setOpen(false);
  };

  const trimmedSave = (saveName ?? "").trim();
  const willOverwrite = presets.some((p) => p.name === trimmedSave);

  const trimmedRename = (renaming?.next ?? "").trim();
  const renameConflict =
    !!renaming && trimmedRename !== renaming.old && presets.some((p) => p.name === trimmedRename);

  // 方案摘要：模式 + 链长度，帮用户区分方案
  const summary = (p: Preset): string => {
    if (p.mode === "ua") {
      const n = (p.uaRules ?? []).filter((r) => r.enabled).length;
      return `UA · ${n} 条规则`;
    }
    if (p.mode === "auto") {
      const n = (p.autoChain ?? []).reduce((sum, ag) => sum + (ag.models?.length ?? 0), 0);
      return `auto · ${n} 个模型`;
    }
    const n = Object.keys(p.manualFallbacks ?? {}).length;
    return `手动 · ${n} 条降级链`;
  };

  return (
    <>
      <div className="flex items-center gap-2">
        {/* 方案下拉 */}
        <div ref={ref} className="relative">
          <button
            type="button"
            onClick={() => setOpen(!open)}
            disabled={busy}
            title="切换运行模式方案"
            className="px-3 py-1.5 text-sm rounded-lg bg-[var(--color-surface-2)] border border-[var(--color-border)] hover:bg-[var(--color-border)] disabled:opacity-50 flex items-center gap-1.5 min-w-[130px]"
          >
            <span className="text-xs text-[var(--color-text-dim)]">方案</span>
            <span className={`truncate flex-1 text-left ${activePreset ? "" : "text-[var(--color-text-dim)]"}`}>
              {activePreset || "自定义"}
            </span>
            <span className="text-[var(--color-text-dim)] text-[10px]">{open ? "▲" : "▼"}</span>
          </button>

          {open && (
            <div className="absolute right-0 z-50 mt-1 w-72 bg-[var(--color-surface)] border border-[var(--color-border)] rounded-lg shadow-lg max-h-72 overflow-y-auto">
              {/* 未命名的方案 */}
              <button
                type="button"
                onClick={() => {
                  if (activePreset) onClearActive();
                  setOpen(false);
                }}
                className={`w-full text-left px-3 py-2 text-sm border-b border-[var(--color-border)] ${
                  !activePreset
                    ? "bg-[var(--color-primary)]/10 text-[var(--color-text)]"
                    : "text-[var(--color-text)] hover:bg-[var(--color-surface-2)]"
                }`}
              >
                未命名的方案
                {!activePreset && <span className="text-[10px] ml-2 text-[var(--color-primary)]">当前</span>}
              </button>

              {presets.length === 0 ? (
                <div className="px-3 py-2.5 text-xs text-[var(--color-text-dim)]">
                  暂无已保存方案，点右侧「保存方案」把当前配置存为快照
                </div>
              ) : (
                presets.map((p) => (
                  <div
                    key={p.name}
                    className={`group flex items-center gap-1 px-2 py-1.5 hover:bg-[var(--color-surface-2)] ${
                      p.name === activePreset ? "bg-[var(--color-primary)]/10" : ""
                    }`}
                  >
                    <button
                      type="button"
                      onClick={() => {
                        onApply(p.name);
                        setOpen(false);
                      }}
                      className="flex-1 text-left min-w-0"
                      title={`切换到「${p.name}」并立即生效`}
                    >
                      <div className="text-sm truncate">{p.name}</div>
                      <div className="text-[10px] text-[var(--color-text-dim)]">{summary(p)}</div>
                    </button>
                    <button
                      type="button"
                      onClick={() => {
                        setRenaming({ old: p.name, next: p.name });
                        setOpen(false);
                      }}
                      title="重命名"
                      className="w-6 h-6 rounded text-xs opacity-0 group-hover:opacity-100 hover:bg-[var(--color-border)] shrink-0"
                    >
                      ✎
                    </button>
                    <ConfirmPopover
                      title={`删除方案「${p.name}」？`}
                      onConfirm={() => {
                        onDelete(p.name);
                        setOpen(false);
                      }}
                      triggerClassName="w-6 h-6 rounded text-xs opacity-0 group-hover:opacity-100 hover:bg-[var(--color-surface-2)] text-[var(--color-text-dim)] shrink-0"
                    >
                      <TrashIcon />
                    </ConfirmPopover>
                  </div>
                ))
              )}
            </div>
          )}
        </div>

        {/* 保存方案 */}
        <button
          onClick={openSave}
          disabled={busy}
          className="px-3 py-1.5 text-sm rounded-lg bg-[var(--color-surface-2)] hover:bg-[var(--color-border)] disabled:opacity-50"
          title="把当前运行模式配置存为方案快照"
        >
          💾 保存方案
        </button>
      </div>

      {/* 保存弹窗 */}
      {saveName !== null && (
        <Modal onClose={() => setSaveName(null)}>
          <h3 className="font-semibold mb-2">保存方案</h3>
          <p className="text-sm text-[var(--color-text-dim)] mb-3">
            把当前运行模式配置（模式 / 降级链 / 全局兜底 / UA 路由）冻结为一份命名快照。
          </p>
          <input
            autoFocus
            type="text"
            value={saveName}
            onChange={(e) => setSaveName(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter" && trimmedSave) {
                onSave(trimmedSave);
                setSaveName(null);
              }
            }}
            placeholder="方案名，如「省钱档」"
            className="w-full px-3 py-1.5 text-sm rounded-lg bg-[var(--color-surface-2)] border border-[var(--color-border)] mb-2"
          />
          {presets.length > 0 && (
            <div className="mb-2">
              <div className="text-xs text-[var(--color-text-dim)] mb-1.5">或选择已有方案覆盖：</div>
              <div className="max-h-40 overflow-y-auto rounded-lg border border-[var(--color-border)]">
                {presets.map((p) => (
                  <button
                    key={p.name}
                    type="button"
                    onClick={() => setSaveName(p.name)}
                    className={`w-full px-3 py-2 text-left text-sm flex items-center justify-between hover:bg-[var(--color-surface-2)] ${
                      trimmedSave === p.name ? "bg-[var(--color-primary)]/10" : ""
                    }`}
                  >
                    <span className="truncate font-medium">{p.name}</span>
                    <span className="text-xs text-[var(--color-text-dim)] ml-2 shrink-0">{summary(p)}</span>
                  </button>
                ))}
              </div>
            </div>
          )}
          {willOverwrite && (
            <p className="text-xs text-[var(--color-warning)] mb-3">
              ⚠️ 已存在同名方案「{trimmedSave}」，保存将覆盖它
            </p>
          )}
          <div className="flex gap-2 justify-end mt-3">
            <button
              onClick={() => setSaveName(null)}
              className="px-3 py-1.5 text-sm rounded-lg bg-[var(--color-surface-2)] hover:bg-[var(--color-border)]"
            >
              取消
            </button>
            <button
              onClick={() => {
                onSave(trimmedSave);
                setSaveName(null);
              }}
              disabled={!trimmedSave}
              className="px-4 py-1.5 text-sm rounded-lg bg-[var(--color-primary)] hover:opacity-90 disabled:opacity-50"
            >
              {willOverwrite ? "覆盖保存" : "保存为新方案"}
            </button>
          </div>
        </Modal>
      )}

      {/* 重命名弹窗 */}
      {renaming && (
        <Modal onClose={() => setRenaming(null)}>
          <h3 className="font-semibold mb-2">重命名方案</h3>
          <p className="text-sm text-[var(--color-text-dim)] mb-3">
            原名「{renaming.old}」，只改名字，方案内容不变。
          </p>
          <input
            autoFocus
            type="text"
            value={renaming.next}
            onChange={(e) => setRenaming({ ...renaming, next: e.target.value })}
            onKeyDown={(e) => {
              if (e.key === "Enter" && trimmedRename && !renameConflict) {
                onRename(renaming.old, trimmedRename);
                setRenaming(null);
              }
            }}
            className="w-full px-3 py-1.5 text-sm rounded-lg bg-[var(--color-surface-2)] border border-[var(--color-border)] mb-2"
          />
          {renameConflict && (
            <p className="text-xs text-[var(--color-danger)] mb-3">该名字已被其他方案占用</p>
          )}
          <div className="flex gap-2 justify-end mt-3">
            <button
              onClick={() => setRenaming(null)}
              className="px-3 py-1.5 text-sm rounded-lg bg-[var(--color-surface-2)] hover:bg-[var(--color-border)]"
            >
              取消
            </button>
            <button
              onClick={() => {
                onRename(renaming.old, trimmedRename);
                setRenaming(null);
              }}
              disabled={!trimmedRename || renameConflict}
              className="px-4 py-1.5 text-sm rounded-lg bg-[var(--color-primary)] hover:opacity-90 disabled:opacity-50"
            >
              确定
            </button>
          </div>
        </Modal>
      )}
    </>
  );
}

// Modal 居中弹窗骨架（点遮罩关闭）
function Modal({ children, onClose }: { children: React.ReactNode; onClose: () => void }) {
  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/50" onClick={onClose}>
      <div
        className="bg-[var(--color-surface)] rounded-xl p-5 border border-[var(--color-border)] shadow-xl max-w-sm w-full mx-4"
        onClick={(e) => e.stopPropagation()}
      >
        {children}
      </div>
    </div>
  );
}
