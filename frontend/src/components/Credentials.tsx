import { useState } from "react";
import { CredsService } from "../../bindings/switchdev/service";
import type { AllCredStatus } from "../../bindings/switchdev/service/models";
import type { CredStatusInfo } from "../../bindings/switchdev/creds/models";
import CopyButton from "./CopyButton";

interface Props {
  creds: AllCredStatus | null;
}

// 上游 -> 首字母圆形图标（与仪表盘一致：蓝/琥珀/紫）
const AGENT_ICON: Record<string, { initial: string; color: string }> = {
  joycode: { initial: "J", color: "bg-[var(--color-primary)]/20 text-[var(--color-primary)]" },
  deveco: { initial: "D", color: "bg-amber-500/20 text-amber-400" },
  opencode: { initial: "O", color: "bg-purple-500/20 text-purple-400" },
  workbuddy: { initial: "W", color: "bg-pink-500/20 text-pink-400" },
  mcloud: { initial: "移", color: "bg-sky-500/20 text-sky-400" },
};

export default function Credentials({ creds }: Props) {
  // OpenCode 本质是 API Key 配置，不在凭据页作为本地 upstream 展示，
  // 用户可在「供应商配置」里添加。
  const builtins = ["joycode", "deveco", "workbuddy", "mcloud"];
  // 只显示已安装/已配置的内置上游；未安装的静默隐藏
  const visibleBuiltins = builtins.filter((u) => {
    const st = creds?.[u as keyof AllCredStatus] ?? null;
    return !!st?.installed || !!st?.valid;
  });
  const freeIds = Object.keys(creds?.providerAPIs ?? {});
  const hasAny = visibleBuiltins.length > 0 || freeIds.length > 0;

  return (
    <div className="p-6 space-y-6">
      {hasAny ? (
        <div className="space-y-4">
          {visibleBuiltins.map((u) => (
            <CredCard
              key={u}
              upstream={u}
              status={(creds?.[u as keyof AllCredStatus] as CredStatusInfo | null) ?? null}
            />
          ))}
          {/* 供应商配置（动态，多个平级） */}
          {freeIds.map((id) => (
            <CredCard key={id} upstream={id} status={(creds?.providerAPIs?.[id] as CredStatusInfo | null) ?? null} isProviderApi />
          ))}
        </div>
      ) : (
        <NoUpstream />
      )}
    </div>
  );
}

// 未检测到任何本地 upstream 时的提示
function NoUpstream() {
  const installList = [
    { name: "JoyCode", url: "https://joycode.com" },
    { name: "DevEco Code", url: "https://developer.huawei.com" },
    { name: "WorkBuddy", url: "https://workbuddy.cn" },
  ];
  return (
    <div className="bg-[var(--color-surface)] rounded-xl p-8 border border-[var(--color-border)] text-center">
      <div className="text-4xl mb-3">🔍</div>
      <h2 className="font-semibold mb-2">未检测到已安装的上游</h2>
      <p className="text-sm text-[var(--color-text-dim)] mb-5">
        程序会静默检测本地已安装的工具，安装后重新打开此页即可显示。你也可以安装以下支持的上游：
      </p>
      <div className="flex flex-wrap justify-center gap-2">
        {installList.map((it) => (
          <a
            key={it.name}
            href={it.url}
            target="_blank"
            rel="noreferrer"
            className="px-3 py-1.5 text-sm rounded-lg bg-[var(--color-surface-2)] hover:bg-[var(--color-border)]"
          >
            {it.name}
          </a>
        ))}
      </div>
    </div>
  );
}

function CredCard({ upstream, status, isProviderApi }: { upstream: string; status: CredStatusInfo | null; isProviderApi?: boolean }) {
  const [busy, setBusy] = useState(false);
  const [toggling, setToggling] = useState(false);
  const valid = status?.valid ?? false;
  const installed = status?.installed ?? false;
  const enabled = status?.enabled ?? true;
  const isGui = status?.agentType === "gui";
  // 免费 API 供应商用 source（= 供应商显示名）作为名字；内置上游用 nameFromUpstream
  const name = isProviderApi
    ? (status?.source || upstream)
    : status ? nameFromUpstream(upstream) : upstream;
  const icon = AGENT_ICON[upstream] ?? { initial: name[0]?.toUpperCase() ?? "?", color: "bg-[var(--color-surface-2)] text-[var(--color-text-dim)]" };
  const installCmd = status?.installCmd ?? "";
  const loginCmd = status?.loginCmd ?? "";

  const refresh = async () => {
    setBusy(true);
    try {
      await CredsService.RefreshCreds(upstream);
    } finally {
      setBusy(false);
    }
  };

  const toggleEnabled = async (next: boolean) => {
    setToggling(true);
    try {
      await CredsService.SetUpstreamEnabled(upstream, next);
      // 状态由 cred:change 事件回推刷新（enabled 字段在 AllCredStatus 里）
    } catch (e) {
      console.error("切换上游启用状态失败", e);
    } finally {
      setToggling(false);
    }
  };

  // 状态徽章：三态（免费 API 用「已配置/未配置」简化）
  let badge;
  if (isProviderApi) {
    badge = valid ? (
      <span className="text-xs px-2 py-0.5 rounded-full bg-[var(--color-success)]/20 text-[var(--color-success)]">
        ✓ 已配置
      </span>
    ) : (
      <span className="text-xs px-2 py-0.5 rounded-full bg-[var(--color-warning)]/20 text-[var(--color-warning)]">
        ○ 未配置
      </span>
    );
  } else {
    badge = (
      <span className="text-xs px-2 py-0.5 rounded-full bg-[var(--color-danger)]/20 text-[var(--color-danger)]">
        ✗ 未安装
      </span>
    );
    if (valid) {
      badge = (
        <span className="text-xs px-2 py-0.5 rounded-full bg-[var(--color-success)]/20 text-[var(--color-success)]">
          ✓ 有效
        </span>
      );
    } else if (installed) {
      badge = (
        <span className="text-xs px-2 py-0.5 rounded-full bg-[var(--color-warning)]/20 text-[var(--color-warning)]">
          ⚠ 已装未登录
        </span>
      );
    }
  }

  return (
    <div className="bg-[var(--color-surface)] rounded-xl p-5 border border-[var(--color-border)]">
      {/* 标题行 */}
      <div className="flex items-start justify-between mb-3">
        <div>
          <div className="flex items-center gap-2 mb-1">
            <span className={`w-7 h-7 rounded-full flex items-center justify-center text-sm font-bold ${icon.color}`}>
              {icon.initial}
            </span>
            <span className="font-semibold text-lg">{name}</span>
            {isProviderApi ? (
              <span className="text-[10px] px-1.5 py-0.5 rounded bg-[var(--color-primary)]/15 text-[var(--color-primary)]">
                供应商
              </span>
            ) : (
              <span className="text-[10px] px-1.5 py-0.5 rounded bg-[var(--color-surface-2)] text-[var(--color-text-dim)] uppercase">
                {isGui ? "GUI" : "CLI"}
              </span>
            )}
            {badge}
            {!enabled && (
              <span className="text-xs px-2 py-0.5 rounded-full bg-[var(--color-surface-2)] text-[var(--color-text-dim)]">
                ⏸ 已停用
              </span>
            )}
          </div>
        </div>
        <div className="flex items-center gap-3">
          {/* 启用/停用开关（全局生效：停用后调用时跳过该上游所有模型） */}
          <label
            className="flex items-center gap-1.5 cursor-pointer select-none"
            title={enabled ? "点击停用：调用时跳过该上游所有模型" : "点击启用该上游"}
          >
            <input
              type="checkbox"
              checked={enabled}
              disabled={toggling}
              onChange={(e) => toggleEnabled(e.target.checked)}
              className="w-4 h-4 accent-[var(--color-primary)] disabled:opacity-50"
            />
            <span className="text-xs text-[var(--color-text-dim)]">{enabled ? "已启用" : "已停用"}</span>
          </label>
          <div className="flex gap-2">
          {!isProviderApi && installed && (
            <button
              onClick={refresh}
              disabled={busy}
              className="px-3 py-1.5 text-sm rounded-lg bg-[var(--color-surface-2)] hover:bg-[var(--color-border)] disabled:opacity-50"
            >
              {busy ? "..." : "刷新"}
            </button>
          )}
          {!isProviderApi && status?.loginUrl && (installed || isGui) && (
            <button
              onClick={() => CredsService.OpenLoginURL(upstream)}
              className="px-3 py-1.5 text-sm rounded-lg bg-[var(--color-surface-2)] hover:bg-[var(--color-border)]"
            >
              登录页
            </button>
          )}
          </div>
        </div>
      </div>

      {/* 三态操作区 */}
      {isProviderApi ? (
        valid ? (
          // 免费 API 正常：显示凭据详情
          <Details status={status} />
        ) : (
          // 免费 API 未配置：提示去设置页
          <div className="mt-1 pt-3 border-t border-[var(--color-border)]">
            <div className="text-xs text-[var(--color-text-dim)]">
              该供应商未配置或未评测通过模型。请到「供应商配置」页配置 API Key 并评测模型。
            </div>
          </div>
        )
      ) : !installed ? (
        // 未安装：显示安装命令/下载
        <InstallGuide
          isGui={isGui}
          installCmd={installCmd}
          downloadUrl={status?.downloadUrl ?? ""}
          upstream={upstream}
        />
      ) : !valid ? (
        // 已装未登录：显示登录命令
        <LoginGuide isGui={isGui} loginCmd={loginCmd} upstream={upstream} />
      ) : (
        // 正常：显示凭据详情
        <Details status={status} />
      )}
    </div>
  );
}

function InstallGuide({
  isGui,
  installCmd,
  downloadUrl,
  upstream,
}: {
  isGui: boolean;
  installCmd: string;
  downloadUrl: string;
  upstream: string;
}) {
  return (
    <div className="mt-1 pt-3 border-t border-[var(--color-border)]">
      <div className="text-xs text-[var(--color-text-dim)] mb-2">未检测到该工具，请先安装：</div>
      {isGui ? (
        <button
          onClick={() => CredsService.OpenDownloadURL(upstream)}
          className="px-3 py-1.5 text-sm rounded-lg bg-[var(--color-primary)] hover:opacity-90"
        >
          ⬇ 打开下载页
        </button>
      ) : installCmd ? (
        <div className="flex items-center gap-2">
          <code className="flex-1 px-2.5 py-1.5 rounded-md bg-[var(--color-surface-2)] font-mono text-xs truncate">
            {installCmd}
          </code>
          <CopyButton text={installCmd} />
        </div>
      ) : downloadUrl ? (
        <button
          onClick={() => CredsService.OpenDownloadURL(upstream)}
          className="px-3 py-1.5 text-sm rounded-lg bg-[var(--color-primary)] hover:opacity-90"
        >
          ⬇ 打开下载页
        </button>
      ) : null}
    </div>
  );
}

function LoginGuide({ isGui, loginCmd, upstream }: { isGui: boolean; loginCmd: string; upstream: string }) {
  return (
    <div className="mt-1 pt-3 border-t border-[var(--color-border)]">
      <div className="text-xs text-[var(--color-text-dim)] mb-2">已安装但凭据无效，请登录：</div>
      <div className="flex items-center gap-2 flex-wrap">
        {isGui ? (
          <span className="text-sm text-[var(--color-text)]">{loginCmd}</span>
        ) : (
          <code className="px-2.5 py-1.5 rounded-md bg-[var(--color-surface-2)] font-mono text-xs">
            {loginCmd}
          </code>
        )}
        {!isGui && loginCmd && <CopyButton text={loginCmd} label="复制登录命令" />}
      </div>
    </div>
  );
}

function Details({ status }: { status: CredStatusInfo | null }) {
  return (
    <div className="grid grid-cols-2 gap-3 text-sm">
      <Detail label="用户" value={status?.userId || "-"} />
      <Detail label="来源" value={status?.source || "-"} noTruncate />
      <Detail label="key 预览" value={status?.keyPreview || "-"} mono />
      <Detail label="过期时间" value={status?.expiresAt || "-"} />
      <Detail label="最近校验" value={status?.lastCheck || "-"} />
    </div>
  );
}

function Detail({ label, value, mono, noTruncate }: { label: string; value: string; mono?: boolean; noTruncate?: boolean }) {
  return (
    <div>
      <div className="text-xs text-[var(--color-text-dim)] mb-0.5">{label}</div>
      <div className={`${noTruncate ? "" : "truncate"} ${mono ? "font-mono text-xs" : ""}`}>{value}</div>
    </div>
  );
}

function nameFromUpstream(upstream: string): string {
  switch (upstream) {
    case "joycode":
      return "JoyCode";
    case "deveco":
      return "DevEco Code";
    case "opencode":
      return "OpenCode Zen";
    case "workbuddy":
      return "WorkBuddy";
    case "mcloud":
      return "移动云盘（灵犀）";
    default:
      return upstream;
  }
}
