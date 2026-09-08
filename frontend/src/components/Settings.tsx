import { useEffect, useRef, useState } from "react";
import { UpdaterService, ConfigService, ProviderAPIService } from "../../bindings/switchdev/service";
import type { Config, AgentModels, Preset, UARule, UAModelMap } from "../../bindings/switchdev/config/models";
import type { ModelRef } from "../../bindings/switchdev/proxy/models";
import type { UpstreamModels } from "../../bindings/switchdev/service/models";
import type { AllCredStatus } from "../../bindings/switchdev/service/models";
import CopyButton from "./CopyButton";
import PricingEditor from "./PricingEditor";
import UpdatePanel from "./UpdatePanel";
import PresetSwitcher from "./PresetSwitcher";
import ConfirmPopover from "./ConfirmPopover";
import TrashIcon from "./TrashIcon";
import { ModelSelect, FreeBadge } from "./ModelSelect";
import { useWailsEvent } from "../hooks/useWailsEvent";

const UPSTREAM_LABEL: Record<string, string> = {
  joycode: "JoyCode",
  deveco: "DevEco",
  workbuddy: "WorkBuddy",
};

// 运行模式 tab 中，第三方供应商（非内置四上游）的模型不展示「免费」徽章——
// 它们是用户自带 Key 接入的，后端虽然统一标了 free:true，但对用户没有「免费」语义，
// 反而与内置免费档混淆。内置上游（joycode/deveco/opencode/workbuddy）保留该标识。
function stripFreeForProviders(list: UpstreamModels[]): UpstreamModels[] {
  const BUILTIN = new Set(["joycode", "deveco", "opencode", "workbuddy"]);
  return list.map((u) =>
    BUILTIN.has(u.upstream)
      ? u
      : { ...u, models: (u.models ?? []).map((m) => ({ ...m, free: false })) }
  );
}

type SettingsTab = "general" | "mode" | "pricing" | "update" | "about";

// modeFingerprint 运行模式四字段的规范化指纹，用于判断当前配置是否已偏离所选方案
//
// 必须规范化，不能直接 JSON.stringify 整个对象：
//   1. manualFallbacks 是 map —— Go 序列化时排序 key，前端新增时是插入顺序，
//      不排序会把「内容相同、key 顺序不同」误判为偏离
//   2. Go 的空 slice 可能序列化成 null 而非 []，需归一
function modeFingerprint(x: {
  mode: string;
  autoChain: AgentModels[] | null;
  manualFallbacks: { [k: string]: ModelRef[] | undefined } | null;
  globalFallback: ModelRef | null;
  uaRoutingEnabled?: boolean;
  uaRules?: UARule[] | null;
  uaGlobalFallback?: ModelRef | null;
}): string {
  const chain = (x.autoChain ?? []).map((ag) => [ag.upstream, ag.models ?? []]);
  const fb = Object.keys(x.manualFallbacks ?? {})
    .sort()
    .map((k) => [k, (x.manualFallbacks?.[k] ?? []).map((r) => [r.upstream, r.model])]);
  const gf = [x.globalFallback?.upstream ?? "", x.globalFallback?.model ?? ""];
  const ugf = [x.uaGlobalFallback?.upstream ?? "", x.uaGlobalFallback?.model ?? ""];
  const uaRules = (x.uaRules ?? []).map((r) => [
    r.id, r.name, r.pattern, r.enabled,
    (r.mappings ?? []).map((m) => [m.requestedModel, m.target.upstream, m.target.model]),
  ]);
  return JSON.stringify([x.mode, chain, fb, gf, !!x.uaRoutingEnabled, uaRules, ugf]);
}

// matchesPreset 当前配置是否与某方案内容完全一致
function matchesPreset(cfg: Config, p: Preset): boolean {
  return modeFingerprint(cfg) === modeFingerprint(p);
}

export default function Settings({ creds, config }: { creds: AllCredStatus | null; config: Config | null }) {
  // config 由 App 提供（已在启动时拉好），不再异步等待，避免"加载中"
  const [cfg, setCfg] = useState<Config | null>(config);
  const [available, setAvailable] = useState<UpstreamModels[]>([]);
  // 所有已配置供应商的 id->显示名（含未验证的，用于运行模式选择器显示供应商名而非 custom-xxx）
  const [providerNames, setProviderNames] = useState<Record<string, string>>({});
  const [msg, setMsg] = useState<{ type: "ok" | "err"; text: string; undo?: () => void } | null>(null);
  // 手动降级链添加器的 state（必须在早返回之前，遵守 Hooks 规则）
  const [newManualKey, setNewManualKey] = useState("");
  const [newManualUpstream, setNewManualUpstream] = useState("joycode");
  const [newManualModel, setNewManualModel] = useState("");
  const [refreshingModels, setRefreshingModels] = useState(false);
  const [savingPort, setSavingPort] = useState(false);
  const [savingKey, setSavingKey] = useState(false);
  const [showKey, setShowKey] = useState(false);
  const [showCfgKey, setShowCfgKey] = useState(false); // 配置 JSON 预览里 apiKey 是否明文（独立于上方输入框）
  // 设置页 tab：运行模式 / 通用 / 费率 / 更新 / 关于
  const [tab, setTab] = useState<SettingsTab>("mode");
  // UA 路由：历史请求来源列表（辅助配置）
  const [uaSources, setUaSources] = useState<{ name: string; userAgent: string; count: number }[]>([]);
  // UA 路由：每个规则的历史模型缓存
  const [uaHistory, setUaHistory] = useState<Record<string, { model: string; count: number }[]>>({});
  // 每个规则新增 mapping 的临时输入
  const [uaDraft, setUaDraft] = useState<Record<string, { requestedModel: string; upstream: string; model: string }>>({});
  // 方案操作进行中（禁用下拉/按钮，避免并发写配置）
  const [presetBusy, setPresetBusy] = useState(false);
  // 标记是否已完成首次配置同步（跳过自动保存）
  const mountedRef = useRef(false);
  // 方案切换刚写过配置：跳过紧随其后的那次自动保存，避免重复写盘
  const skipNextSaveRef = useRef(false);
  // toast 自动消失定时器（连续 flash 时清掉上一个，避免提前关闭新消息）
  const msgTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  // 当前 toast 是否带撤销按钮（供自动保存判断要不要让位）
  const undoActiveRef = useRef(false);

  // flash 顶部提示；undo 传入时额外渲染「撤销」按钮，展示时间延长到 6s
  const flash = (type: "ok" | "err", text: string, undo?: () => void) => {
    if (msgTimerRef.current) clearTimeout(msgTimerRef.current);
    undoActiveRef.current = !!undo;
    setMsg({ type, text, undo });
    msgTimerRef.current = setTimeout(() => {
      setMsg(null);
      undoActiveRef.current = false;
    }, undo ? 6000 : 3000);
  };

  // 卸载时清理定时器
  useEffect(() => {
    return () => {
      if (msgTimerRef.current) clearTimeout(msgTimerRef.current);
    };
  }, []);

  // flashUndo 破坏性操作后提示可撤销：点击撤销把 cfg 回滚到操作前的快照
  // 回滚同样会触发自动保存 effect，所以配置文件也会跟着还原
  const flashUndo = (text: string, snapshot: Config) => {
    flash("ok", text, () => {
      setCfg(snapshot);
      flash("ok", "已撤销");
    });
  };

  const load = async () => {
    // config 用 props 的（App 已提前拉），只拉模型列表（后端有缓存，快）
    const a = await ConfigService.GetAvailableModels();
    setAvailable(stripFreeForProviders((a ?? []).filter((x): x is UpstreamModels => x !== null)));
    loadProviderNames();
    loadUaSources();
  };

  // 拉取所有已配置供应商的 id->显示名（含未验证的 custom-xxx，供运行模式选择器显示友好名）
  const loadProviderNames = async () => {
    try {
      const ps = await ProviderAPIService.GetProviders();
      const map: Record<string, string> = {};
      (ps ? Object.entries(ps) : []).forEach(([id, p]) => {
        if (p && p.name) map[id] = p.name;
      });
      setProviderNames(map);
    } catch {
      // 忽略：拿不到就回退显示 id
    }
  };

  useEffect(() => {
    load();
    // config prop 变化时同步（App 刷新后）
    if (config) {
      setCfg(config);
      // 自动加载已有规则（如内置 Claude Code/Codex）的历史模型
      (config.uaRules ?? []).forEach((r) => {
        if (r.name) loadRuleHistory(r.id, r.name);
      });
      // 延迟标记已挂载，避免首次同步触发自动保存
      requestAnimationFrame(() => { mountedRef.current = true; });
    } else {
      mountedRef.current = true;
    }
  }, []);

  // 供应商增删/模型变化时刷新上游列表与供应商名（后端已在此时清掉模型缓存）。
  // 这样在「供应商」页添加并验证模型后切回设置页，upstream 下拉能立即出现新供应商。
  useWailsEvent("providerapi:change", () => {
    loadProviderNames();
    ConfigService.RefreshModels()
      .then((a) => setAvailable(stripFreeForProviders((a ?? []).filter((x): x is UpstreamModels => x !== null))))
      .catch(() => {});
  });
  useWailsEvent("cred:change", () => {
    ConfigService.RefreshModels()
      .then((a) => setAvailable(stripFreeForProviders((a ?? []).filter((x): x is UpstreamModels => x !== null))))
      .catch(() => {});
  });
  // 后台目录刷新完成：只读缓存即可，别再触发一次拉取（RefreshModels 会重新打上游）
  useWailsEvent("models:change", () => {
    ConfigService.GetAvailableModels()
      .then((a) => setAvailable(stripFreeForProviders((a ?? []).filter((x): x is UpstreamModels => x !== null))))
      .catch(() => {});
  });

  // 自动保存：运行模式配置变更时即时保存（跳过首次同步）
  // 只提交运行模式四个字段，避免把「通用」tab 里未点保存的端口改动一起带上
  useEffect(() => {
    if (!cfg || !mountedRef.current) return;
    // 方案切换/保存刚写过配置，这次变更是它引起的，不用再写一遍
    if (skipNextSaveRef.current) {
      skipNextSaveRef.current = false;
      return;
    }
    const t = setTimeout(async () => {
      try {
        const cur = await ConfigService.GetConfig();
        if (!cur) throw new Error("获取配置失败");
        // 偏离检测：当前配置与激活方案不一致时清空激活标记（下拉显示「自定义」）
        // 方案是快照，编辑当前配置不回写方案
        let active = cur.activePreset ?? "";
        if (active) {
          const p = (cur.presets ?? []).find((x) => x?.name === active);
          if (!p || !matchesPreset({ ...cfg } as Config, p)) active = "";
        }
        await ConfigService.SaveConfig({
          ...cur,
          mode: cfg.mode,
          autoChain: cfg.autoChain,
          manualFallbacks: cfg.manualFallbacks,
          globalFallback: cfg.globalFallback,
          uaRoutingEnabled: cfg.uaRoutingEnabled,
          uaRules: cfg.uaRules,
          uaGlobalFallback: cfg.uaGlobalFallback,
          activePreset: active,
        });
        // 本地同步激活标记，避免 UI 还显示旧方案名
        if (active !== (cfg.activePreset ?? "")) {
          setCfg((prev) => (prev ? { ...prev, activePreset: active } : prev));
        }
        // 保存成功的提示不要抢占正在展示的撤销提示（撤销窗口比保存耗时长）
        if (!undoActiveRef.current) flash("ok", "已保存并生效");
      } catch (e) {
        flash("err", `保存失败: ${e}`);
      }
    }, 600);
    return () => clearTimeout(t);
  }, [cfg?.mode, cfg?.autoChain, cfg?.manualFallbacks, cfg?.globalFallback, cfg?.uaRoutingEnabled, cfg?.uaRules, cfg?.uaGlobalFallback]);

  // ====== 运行模式方案（快照语义）======
  // 后端写盘后重新拉配置同步到本地，并置 skipNextSaveRef 防止自动保存重复写一遍

  const syncAfterPreset = async () => {
    const fresh = await ConfigService.GetConfig();
    if (fresh) {
      skipNextSaveRef.current = true;
      setCfg(fresh);
    }
  };

  const savePreset = async (name: string) => {
    setPresetBusy(true);
    try {
      await ConfigService.SavePreset(name);
      await syncAfterPreset();
      flash("ok", `方案「${name}」已保存`);
    } catch (e) {
      flash("err", `保存方案失败: ${e}`);
    } finally {
      setPresetBusy(false);
    }
  };

  const applyPreset = async (name: string) => {
    setPresetBusy(true);
    try {
      await ConfigService.ApplyPreset(name);
      await syncAfterPreset();
      flash("ok", `已切换到方案「${name}」并生效`);
    } catch (e) {
      flash("err", `切换方案失败: ${e}`);
    } finally {
      setPresetBusy(false);
    }
  };

  const deletePreset = async (name: string) => {
    // 删前留一份快照，撤销时用 SavePreset 重建
    const doomed = (cfg?.presets ?? []).find((p) => p?.name === name);
    setPresetBusy(true);
    try {
      await ConfigService.DeletePreset(name);
      await syncAfterPreset();
      // 撤销：把当前配置临时换成被删方案的内容再存回去，然后恢复现场
      flash("ok", `方案「${name}」已删除`, doomed ? () => restorePreset(doomed) : undefined);
    } catch (e) {
      flash("err", `删除方案失败: ${e}`);
    } finally {
      setPresetBusy(false);
    }
  };

  // restorePreset 重建被删的方案：直接把方案对象写回 presets 数组
  // （不走 SavePreset —— 那个存的是「当前配置」，而当前配置可能已经变了）
  const restorePreset = async (p: Preset) => {
    setPresetBusy(true);
    try {
      const cur = await ConfigService.GetConfig();
      if (!cur) throw new Error("获取配置失败");
      const presets = [...(cur.presets ?? [])];
      if (!presets.some((x) => x?.name === p.name)) presets.push(p);
      await ConfigService.SaveConfig({ ...cur, presets });
      await syncAfterPreset();
      flash("ok", `已恢复方案「${p.name}」`);
    } catch (e) {
      flash("err", `恢复方案失败: ${e}`);
    } finally {
      setPresetBusy(false);
    }
  };

  const renamePreset = async (oldName: string, newName: string) => {
    setPresetBusy(true);
    try {
      await ConfigService.RenamePreset(oldName, newName);
      await syncAfterPreset();
      flash("ok", `方案已重命名为「${newName}」`);
    } catch (e) {
      flash("err", `重命名失败: ${e}`);
    } finally {
      setPresetBusy(false);
    }
  };

  const refreshModels = async () => {
    setRefreshingModels(true);
    try {
      const a = await ConfigService.RefreshModels();
      setAvailable(stripFreeForProviders((a ?? []).filter((x): x is UpstreamModels => x !== null)));
      flash("ok", "模型列表已刷新");
    } catch (e) {
      flash("err", `刷新失败: ${e}`);
    } finally {
      setRefreshingModels(false);
    }
  };

  // 凭据有效性（用于提示哪些 upstream 可用；含免费 API 供应商）
  const credValid: Record<string, boolean> = {
    joycode: creds?.joycode?.valid ?? false,
    deveco: creds?.deveco?.valid ?? false,
    workbuddy: creds?.workbuddy?.valid ?? false,
    ...Object.fromEntries(
      Object.entries(creds?.providerAPIs ?? {}).map(([id, st]) => [id, st?.valid ?? false])
    ),
  };

  // upstream 显示名：内置用固定 label，免费 API 供应商优先用凭据 source（已验证），
  // 再回退到已配置供应商的名字（含未验证的 custom-xxx），最后才显示 id
  const labelOf = (up: string): string => {
    if (UPSTREAM_LABEL[up]) return UPSTREAM_LABEL[up];
    const src = creds?.providerAPIs?.[up]?.source;
    return src || providerNames[up] || up;
  };

  if (!cfg) return <div className="p-6 text-[var(--color-text-dim)]">加载配置中...</div>;

  // 只保存端口（不影响运行模式配置）
  const savePort = async () => {
    setSavingPort(true);
    try {
      const cur = await ConfigService.GetConfig();
      if (!cur) throw new Error("获取配置失败");
      await ConfigService.SaveConfig({ ...cur, port: cfg.port });
      flash("ok", "端口已保存，代理已重启");
    } catch (e) {
      flash("err", `保存端口失败: ${e}`);
    } finally {
      setSavingPort(false);
    }
  };

  // 切换"进入编辑时自动拉取并测评模型"
  const toggleAutoBench = async (enabled: boolean) => {
    try {
      const cur = await ConfigService.GetConfig();
      if (!cur) throw new Error("获取配置失败");
      const curFree = cur.provider ?? { autoBenchmarkOnEdit: false };
      const next = { ...cur, provider: { ...curFree, autoBenchmarkOnEdit: enabled } };
      await ConfigService.SaveConfig(next);
      setCfg(next);
      flash("ok", enabled ? "已开启自动测评" : "已关闭自动测评");
    } catch (e) {
      flash("err", `保存失败: ${e}`);
    }
  };

  // 切换"闲置时自动锁定供应商界面"
  const toggleIdleAutoLock = async (enabled: boolean) => {
    try {
      const cur = await ConfigService.GetConfig();
      if (!cur) throw new Error("获取配置失败");
      const curFree = cur.provider ?? { autoBenchmarkOnEdit: false, idleAutoLock: true };
      const next = { ...cur, provider: { ...curFree, idleAutoLock: enabled } };
      await ConfigService.SaveConfig(next);
      setCfg(next);
      flash("ok", enabled ? "已开启闲置自动锁定" : "已关闭闲置自动锁定");
    } catch (e) {
      flash("err", `保存失败: ${e}`);
    }
  };

  // 切换"控制台日志落地文件"
  const toggleLogFile = async (enabled: boolean) => {
    try {
      const cur = await ConfigService.GetConfig();
      if (!cur) throw new Error("获取配置失败");
      const next = { ...cur, logFile: { ...(cur.logFile ?? {}), enabled } };
      await ConfigService.SaveConfig(next);
      setCfg(next);
      flash("ok", enabled ? "已开启日志文件" : "已关闭日志文件");
    } catch (e) {
      flash("err", `保存失败: ${e}`);
    }
  };

  // 切换"开机自动启动"：后端同时操作系统登录项（注册表/LaunchAgent/.desktop）
  const toggleAutoStart = async (enabled: boolean) => {
    try {
      await ConfigService.SetAutoStart(enabled);
      const cur = await ConfigService.GetConfig();
      if (cur) setCfg(cur);
      flash("ok", enabled ? "已开启开机自启（登录后静默到托盘）" : "已关闭开机自启");
    } catch (e) {
      flash("err", `设置失败: ${e}`);
    }
  };

  // 切换"接入 apiKey 鉴权"：关闭后网关不校验 key，仪表盘也不显示
  const toggleAuthEnabled = async (enabled: boolean) => {
    try {
      const cur = await ConfigService.GetConfig();
      if (!cur) throw new Error("获取配置失败");
      const next = { ...cur, authEnabled: enabled };
      await ConfigService.SaveConfig(next);
      setCfg(next);
      flash("ok", enabled ? "已开启接入鉴权" : "已关闭接入鉴权，网关请求无需 apiKey");
    } catch (e) {
      flash("err", `保存失败: ${e}`);
    }
  };

  // 重新生成 apiKey：二次确认告知风险 -> 生成 rs-<uuid> -> 保存立即生效
  const regenKey = async () => {
    if (
      !confirm(
        "重新生成后，原 apiKey 立即失效。\n" +
          "所有正在使用旧 key 的客户端（如 cc-switch、Claude Code）将返回 401，需更新为新 key。\n\n" +
          "确认重新生成？"
      )
    ) {
      return;
    }
    setSavingKey(true);
    try {
      const newKey = "rs-" + crypto.randomUUID();
      const cur = await ConfigService.GetConfig();
      if (!cur) throw new Error("获取配置失败");
      await ConfigService.SaveConfig({ ...cur, apiKey: newKey });
      setCfg({ ...cfg, apiKey: newKey });
      setShowKey(true); // 生成后显示新 key，方便复制
      flash("ok", "apiKey 已重新生成并生效");
    } catch (e) {
      flash("err", `重新生成失败: ${e}`);
    } finally {
      setSavingKey(false);
    }
  };

  // ====== auto 链操作 ======
  const allModels = available.flatMap((u) =>
    u.models.map((m) => ({ upstream: u.upstream, model: m.id, label: m.label, free: m.free }))
  );

  const addAutoItem = (upstream: string, model: string) => {
    const chain = [...cfg.autoChain];
    const existing = chain.find((c) => c.upstream === upstream);
    if (existing) {
      existing.models = [...existing.models, model];
    } else {
      chain.push({ upstream, models: [model] } as AgentModels);
    }
    setCfg({ ...cfg, autoChain: chain });
  };

  const removeAutoItem = (upstream: string, model: string) => {
    const prev = cfg;
    const chain = cfg.autoChain
      .map((c) => {
        if (c.upstream !== upstream) return c;
        return { ...c, models: c.models.filter((m) => m !== model) } as AgentModels;
      })
      .filter((c) => c.models.length > 0);
    setCfg({ ...cfg, autoChain: chain });
    flashUndo(`已从链中移除 ${model}`, prev);
  };

  const moveAutoItem = (upstream: string, model: string, dir: -1 | 1) => {
    // 扁平化 -> 移动 -> 重新分组（保持同 upstream 相邻）
    const flat: { upstream: string; model: string }[] = [];
    cfg.autoChain.forEach((c) => c.models.forEach((m) => flat.push({ upstream: c.upstream, model: m })));
    const idx = flat.findIndex((f) => f.upstream === upstream && f.model === model);
    if (idx < 0) return;
    const newIdx = idx + dir;
    if (newIdx < 0 || newIdx >= flat.length) return;
    [flat[idx], flat[newIdx]] = [flat[newIdx], flat[idx]];
    // 重新分组
    const chain: AgentModels[] = [];
    for (const f of flat) {
      const last = chain[chain.length - 1];
      if (last && last.upstream === f.upstream) {
        last.models.push(f.model);
      } else {
        chain.push({ upstream: f.upstream, models: [f.model] } as AgentModels);
      }
    }
    setCfg({ ...cfg, autoChain: chain });
  };

  // 扁平化的 auto 链（用于显示和排序）
  const flatChain: { upstream: string; model: string }[] = [];
  cfg.autoChain.forEach((c) => c.models.forEach((m) => flatChain.push({ upstream: c.upstream, model: m })));

  // ====== 手动降级链操作 ======
  const manualKeys = Object.keys(cfg.manualFallbacks || {});

  const addManualFallback = () => {
    if (!newManualKey || !newManualModel) return;
    const fb = { ...(cfg.manualFallbacks || {}) };
    fb[newManualKey] = [...(fb[newManualKey] || []), { upstream: newManualUpstream, model: newManualModel } as ModelRef];
    setCfg({ ...cfg, manualFallbacks: fb });
    setNewManualModel("");
  };

  const removeManualFallback = (key: string, idx: number) => {
    const prev = cfg;
    const removed = (cfg.manualFallbacks?.[key] || [])[idx];
    const fb = { ...(cfg.manualFallbacks || {}) };
    fb[key] = (fb[key] || []).filter((_, i) => i !== idx);
    if (fb[key].length === 0) delete fb[key];
    setCfg({ ...cfg, manualFallbacks: fb });
    flashUndo(`已移除 ${key} 的降级项${removed ? ` ${removed.model}` : ""}`, prev);
  };

  // ====== UA 路由操作 ======

  const loadUaSources = async () => {
    try {
      const srcs = await ConfigService.GetUASources();
      setUaSources((srcs ?? []).filter((s) => s && s.userAgent));
    } catch {
      // 静默失败：历史数据是辅助功能
    }
  };

  const loadRuleHistory = async (ruleId: string, sourceName: string) => {
    if (!sourceName) return;
    try {
      const models = await ConfigService.GetModelsByUASource(sourceName);
      setUaHistory((prev) => ({ ...prev, [ruleId]: (models ?? []).map((m) => ({ model: m.model, count: m.count })) }));
    } catch {
      // 静默
    }
  };

  const updateUaRule = (ruleId: string, patch: Partial<UARule>) => {
    const rules = (cfg.uaRules ?? []).map((r) => (r.id === ruleId ? { ...r, ...patch } : r));
    setCfg({ ...cfg, uaRules: rules });
  };

  // 选择 UA 来源时：更新 name（用于历史查询），pattern 仅在为空时填入 UA 字符串
  const selectUaSource = (ruleId: string, source: { name: string; userAgent: string }) => {
    const rule = (cfg.uaRules ?? []).find((r) => r.id === ruleId);
    const patch: Partial<UARule> = { name: source.name };
    if (!rule?.pattern) {
      patch.pattern = source.userAgent;
    }
    updateUaRule(ruleId, patch);
    loadRuleHistory(ruleId, source.name);
  };

  const addUaMapping = (ruleId: string) => {
    const draft = uaDraft[ruleId];
    if (!draft || !draft.requestedModel || !draft.upstream || !draft.model) return;
    const rules = (cfg.uaRules ?? []).map((r) => {
      if (r.id !== ruleId) return r;
      const mapping: UAModelMap = {
        requestedModel: draft.requestedModel,
        target: { upstream: draft.upstream, model: draft.model } as ModelRef,
      };
      return { ...r, mappings: [...(r.mappings ?? []), mapping] };
    });
    setCfg({ ...cfg, uaRules: rules });
    setUaDraft({ ...uaDraft, [ruleId]: { requestedModel: "", upstream: draft.upstream, model: "" } });
  };

  const removeUaMapping = (ruleId: string, idx: number) => {
    const rules = (cfg.uaRules ?? []).map((r) => {
      if (r.id !== ruleId) return r;
      return { ...r, mappings: (r.mappings ?? []).filter((_, i) => i !== idx) };
    });
    setCfg({ ...cfg, uaRules: rules });
  };

  const addUaRule = () => {
    const rule: UARule = {
      id: "ua-" + crypto.randomUUID().slice(0, 8),
      name: "",
      pattern: "",
      enabled: true,
      mappings: [],
    };
    setCfg({ ...cfg, uaRules: [...(cfg.uaRules ?? []), rule] });
  };

  const removeUaRule = (ruleId: string) => {
    setCfg({ ...cfg, uaRules: (cfg.uaRules ?? []).filter((r) => r.id !== ruleId) });
  };

  return (
    <div className="p-6 space-y-6">
      {msg && (
        <div className={`flex items-center gap-3 px-4 py-2 rounded-lg text-sm ${msg.type === "ok" ? "bg-[var(--color-success)]/20 text-[var(--color-success)]" : "bg-[var(--color-danger)]/20 text-[var(--color-danger)]"}`}>
          <span className="flex-1">{msg.text}</span>
          {msg.undo && (
            <button
              onClick={msg.undo}
              className="px-2.5 py-1 text-xs rounded-md bg-[var(--color-surface-2)] text-[var(--color-text)] hover:bg-[var(--color-border)] shrink-0"
            >
              撤销
            </button>
          )}
        </div>
      )}

      {/* 顶部 tab 导航 */}
      <div className="flex gap-1 border-b border-[var(--color-border)] pb-px">
        <TabBtn label="🚀 运行模式" active={tab === "mode"} onClick={() => setTab("mode")} />
        <TabBtn label="⚙️ 通用" active={tab === "general"} onClick={() => setTab("general")} />
        <TabBtn label="💰 费率" active={tab === "pricing"} onClick={() => setTab("pricing")} />
        <TabBtn label="🔄 更新" active={tab === "update"} onClick={() => setTab("update")} />
        <TabBtn label="ℹ️ 关于" active={tab === "about"} onClick={() => setTab("about")} />
      </div>

      {/* ===== 通用：代理端口 + 配置 JSON ===== */}
      {tab === "general" && (
      <div className="space-y-6">
        {/* 代理端口 */}
        <section className="bg-[var(--color-surface)] rounded-xl p-5 border border-[var(--color-border)]">
          <h2 className="font-semibold mb-1">代理端口</h2>
          <p className="text-xs text-[var(--color-text-dim)] mb-3">
            cc-switch 的 baseURL 要对应此端口（http://127.0.0.1:端口）。保存后代理会重启切换端口。
          </p>
          <div className="flex items-center gap-3">
            <input
              type="number"
              min={1024}
              max={65535}
              value={cfg.port || 8787}
              onChange={(e) => {
                const p = parseInt(e.target.value, 10);
                if (!isNaN(p)) setCfg({ ...cfg, port: p });
              }}
              className="w-32 px-3 py-1.5 text-sm rounded-lg bg-[var(--color-surface-2)] border border-[var(--color-border)] font-mono"
            />
            <span className="text-sm text-[var(--color-text-dim)]">
              监听 http://127.0.0.1:{cfg.port || 8787}
            </span>
            <button
              onClick={() => setCfg({ ...cfg, port: 8787 })}
              className="px-2.5 py-1 text-xs rounded-md bg-[var(--color-surface-2)] hover:bg-[var(--color-border)]"
            >
              恢复默认 8787
            </button>
            <button
              onClick={savePort}
              disabled={savingPort}
              className="px-4 py-1.5 text-sm rounded-lg bg-[var(--color-primary)] hover:opacity-90 disabled:opacity-50"
            >
              {savingPort ? "保存中..." : "💾 保存端口"}
            </button>
          </div>
        </section>

        {/* 接入 apiKey */}
        <section className="bg-[var(--color-surface)] rounded-xl p-5 border border-[var(--color-border)]">
          <div className="flex items-start justify-between gap-4 mb-1">
            <div>
              <h2 className="font-semibold">接入 apiKey</h2>
              <p className="text-xs text-[var(--color-text-dim)] mt-1">
                开启后客户端调用代理需在 <code className="font-mono">x-api-key</code> 或{" "}
                <code className="font-mono">Authorization: Bearer</code> 头携带此 key，严格校验。关闭后网关不鉴权，任何请求均可访问。
              </p>
            </div>
            <label className="inline-flex items-center cursor-pointer shrink-0 mt-0.5">
              <input
                type="checkbox"
                checked={cfg.authEnabled !== false}
                onChange={(e) => toggleAuthEnabled(e.target.checked)}
                className="sr-only peer"
              />
              <div className="w-10 h-5 bg-[var(--color-surface-2)] rounded-full peer-checked:bg-[var(--color-primary)] relative transition-colors after:content-[''] after:absolute after:top-0.5 after:left-0.5 after:bg-white after:rounded-full after:h-4 after:w-4 after:transition-all peer-checked:after:translate-x-5" />
            </label>
          </div>

          {cfg.authEnabled !== false && (
            <div className="flex items-center gap-3 flex-wrap mt-3 pt-3 border-t border-[var(--color-border)]">
              <input
                type="text"
                value={showKey ? (cfg.apiKey ?? "") : maskKey(cfg.apiKey ?? "")}
                readOnly
                className="flex-1 min-w-[280px] px-3 py-1.5 text-sm rounded-lg bg-[var(--color-surface-2)] border border-[var(--color-border)] font-mono cursor-default"
              />
              <button
                onClick={() => setShowKey((v) => !v)}
                className="px-2.5 py-1.5 text-sm rounded-lg bg-[var(--color-surface-2)] hover:bg-[var(--color-border)]"
                title={showKey ? "隐藏" : "显示"}
              >
                {showKey ? "🙈" : "👁"}
              </button>
              <CopyButton text={cfg.apiKey ?? ""} />
              <button
                onClick={regenKey}
                disabled={savingKey}
                className="px-3 py-1.5 text-sm rounded-lg bg-[var(--color-danger)]/80 hover:bg-[var(--color-danger)] disabled:opacity-50"
                title="重新生成随机 key（原 key 立即失效）"
              >
                {savingKey ? "生成中..." : "🔄 重新生成"}
              </button>
            </div>
          )}
        </section>

        {/* 启动 */}
        <section className="bg-[var(--color-surface)] rounded-xl p-5 border border-[var(--color-border)]">
          <h2 className="font-semibold mb-1">启动</h2>
          <p className="text-xs text-[var(--color-text-dim)] mb-3">
            登录系统时自动在后台启动 Switch Dev，不弹出主窗口（静默驻留托盘，双击托盘图标打开）。
          </p>
          <label className="flex items-center gap-2 cursor-pointer">
            <input
              type="checkbox"
              checked={!!cfg?.autoStart}
              onChange={(e) => toggleAutoStart(e.target.checked)}
              className="w-4 h-4 accent-[var(--color-primary)]"
            />
            <span className="text-sm">开机自动启动（静默到托盘）</span>
          </label>
        </section>

        {/* 供应商配置 */}
        <section className="bg-[var(--color-surface)] rounded-xl p-5 border border-[var(--color-border)]">
          <h2 className="font-semibold mb-1">供应商配置</h2>
          <p className="text-xs text-[var(--color-text-dim)] mb-3">
            编辑供应商时自动拉取模型列表并批量测评，结果实时显示。关闭后需手动点「拉取模型」。
          </p>
          <label className="flex items-center gap-2 cursor-pointer">
            <input
              type="checkbox"
              checked={!!cfg?.provider?.autoBenchmarkOnEdit}
              onChange={(e) => toggleAutoBench(e.target.checked)}
              className="w-4 h-4 accent-[var(--color-primary)]"
            />
            <span className="text-sm">进入编辑时自动拉取并测评模型</span>
          </label>
          <label className="flex items-center gap-2 cursor-pointer mt-3">
            <input
              type="checkbox"
              checked={cfg?.provider?.idleAutoLock !== false}
              onChange={(e) => toggleIdleAutoLock(e.target.checked)}
              className="w-4 h-4 accent-[var(--color-primary)]"
            />
            <span className="text-sm">闲置时自动锁定供应商界面</span>
          </label>
          <p className="text-[11px] text-[var(--color-text-dim)] mt-2">
            无操作 5 分钟后自动锁定供应商界面，需要输入主密码解锁。锁定期间代理调用不受影响。仅在供应商界面已开启主密码后生效。
          </p>
        </section>

        {/* 日志文件 */}
        <section className="bg-[var(--color-surface)] rounded-xl p-5 border border-[var(--color-border)]">
          <h2 className="font-semibold mb-1">日志文件</h2>
          <p className="text-xs text-[var(--color-text-dim)] mb-3">
            把控制台日志同时写入文件（存于配置目录 logs/ 下，按天切分），便于排查问题。旧日志自动压缩：7 天前压缩为 .gz，90 天前删除。
          </p>
          <label className="flex items-center gap-2 cursor-pointer">
            <input
              type="checkbox"
              checked={cfg?.logFile?.enabled !== false}
              onChange={(e) => toggleLogFile(e.target.checked)}
              className="w-4 h-4 accent-[var(--color-primary)]"
            />
            <span className="text-sm">启用日志文件（默认开启）</span>
          </label>
        </section>
      </div>
      )}

      {/* ===== 运行模式：模式切换 + auto链/手动链 + 兜底 + 配置JSON ===== */}
      {tab === "mode" && (
      <div className="space-y-6">
      {/* 模式切换 */}
      <section className="bg-[var(--color-surface)] rounded-xl p-5 border border-[var(--color-border)]">
        <div className="flex items-center justify-between mb-3 gap-3">
          <h2 className="font-semibold shrink-0">运行模式</h2>
          <div className="flex items-center gap-2">
            <PresetSwitcher
              presets={(cfg.presets ?? []).filter((p): p is Preset => p !== null)}
              activePreset={cfg.activePreset ?? ""}
              busy={presetBusy}
              onApply={applyPreset}
              onSave={savePreset}
              onDelete={deletePreset}
              onRename={renamePreset}
              onClearActive={async () => {
                await ConfigService.ClearActivePreset();
                await syncAfterPreset();
              }}
            />
            <button
              onClick={refreshModels}
              disabled={refreshingModels}
              className="px-3 py-1.5 text-sm rounded-lg bg-[var(--color-surface-2)] hover:bg-[var(--color-border)] disabled:opacity-50"
              title="从上游接口重新拉取模型列表（结果会持久化，重启后仍可用）"
            >
              {refreshingModels ? "刷新中..." : "🔄 刷新模型"}
            </button>
          </div>
        </div>
        {/* 各上游模型来源（三种模式都可见） */}
        <div className="flex gap-1.5 flex-wrap mb-3">
          {available.map((u) => (
            <span
              key={u.upstream}
              className={`text-[10px] px-1.5 py-0.5 rounded ${
                u.source === "live"
                  ? "bg-[var(--color-success)]/20 text-[var(--color-success)]"
                  : u.source === "free"
                    ? "bg-[var(--color-primary)]/20 text-[var(--color-primary)]"
                    : "bg-[var(--color-warning)]/20 text-[var(--color-warning)]"
              }`}
              title={
                u.source === "live"
                  ? "接口实时拉取（已持久化）"
                  : u.source === "free"
                    ? "第三方供应商已验证模型"
                    : "本地内置白名单（接口拉取失败或该上游无模型接口）"
              }
            >
              {labelOf(u.upstream)}: {u.source === "live" ? `${u.models.length} 实时` : u.source === "free" ? `${u.models.length} 免费` : `${u.models.length} 本地`}
            </span>
          ))}
        </div>
        <div className="flex gap-4">
          <label className={`flex items-center gap-2 px-4 py-2 rounded-lg cursor-pointer border ${cfg.mode === "auto" ? "border-[var(--color-primary)] bg-[var(--color-primary)]/10" : "border-[var(--color-border)]"}`}>
            <input
              type="radio"
              checked={cfg.mode === "auto"}
              onChange={() => setCfg({ ...cfg, mode: "auto" })}
              className="accent-[var(--color-primary)]"
            />
            <div>
              <div className="text-sm font-medium">auto 模式</div>
              <div className="text-xs text-[var(--color-text-dim)]">客户端发 auto/不指定时，按优先级链依次尝试</div>
            </div>
          </label>
          <label className={`flex items-center gap-2 px-4 py-2 rounded-lg cursor-pointer border ${cfg.mode === "manual" ? "border-[var(--color-primary)] bg-[var(--color-primary)]/10" : "border-[var(--color-border)]"}`}>
            <input
              type="radio"
              checked={cfg.mode === "manual"}
              onChange={() => setCfg({ ...cfg, mode: "manual" })}
              className="accent-[var(--color-primary)]"
            />
            <div>
              <div className="text-sm font-medium">手动模式</div>
              <div className="text-xs text-[var(--color-text-dim)]">客户端指定具体模型时，严格走该模型 + 其降级链</div>
            </div>
          </label>
          <label className={`flex items-center gap-2 px-4 py-2 rounded-lg cursor-pointer border ${cfg.mode === "ua" ? "border-[var(--color-primary)] bg-[var(--color-primary)]/10" : "border-[var(--color-border)]"}`}>
            <input
              type="radio"
              checked={cfg.mode === "ua"}
              onChange={() => setCfg({ ...cfg, mode: "ua" })}
              className="accent-[var(--color-primary)]"
            />
            <div>
              <div className="text-sm font-medium">ua 模式</div>
              <div className="text-xs text-[var(--color-text-dim)]">完全由 User-Agent 规则路由，未命中走 UA 兜底</div>
            </div>
          </label>
        </div>
        <p className="text-xs text-[var(--color-text-dim)] mt-3">
          {cfg.mode === "auto"
            ? "auto 模式：客户端发 auto/不指定时，按下方优先级链依次尝试，末尾追加全局兜底。"
            : cfg.mode === "manual"
            ? "手动模式：客户端指定具体模型时严格走该模型，失败按该模型的降级链走，末尾追加全局兜底。"
            : "ua 模式：根据 User-Agent 规则将请求路由到指定上游模型，未命中或失败时走 UA 兜底。"}
        </p>
      </section>

      {/* auto 优先级链（仅 auto 模式显示） */}
      {cfg.mode === "auto" && (      <section className="bg-[var(--color-surface)] rounded-xl p-5 border border-[var(--color-border)]">
        <div className="flex items-center gap-2 mb-1">
          <h2 className="font-semibold">auto 模式优先级链</h2>
        </div>
        <p className="text-xs text-[var(--color-text-dim)] mb-3">
          按顺序尝试，任意一个成功即返回。凭据无效的会被自动跳过。
        </p>

        {flatChain.length === 0 ? (
          <div className="text-sm text-[var(--color-text-dim)] py-4 text-center">链为空，请添加模型</div>
        ) : (
          <div className="space-y-2 mb-3">
            {flatChain.map((item, idx) => {
              const valid = credValid[item.upstream];
              const opt = allModels.find((m) => m.upstream === item.upstream && m.model === item.model);
              return (
                <div key={`${item.upstream}-${item.model}-${idx}`} className="flex items-center gap-2 bg-[var(--color-bg)] rounded-lg p-2.5 border border-[var(--color-border)]">
                  <span className="text-xs text-[var(--color-text-dim)] w-6 text-center">{idx + 1}</span>
                  <span className="text-xs px-2 py-0.5 rounded bg-[var(--color-surface-2)]">{labelOf(item.upstream)}</span>
                  <span className="flex-1 text-sm font-mono truncate">{item.model}</span>
                  {opt && <span className="text-xs text-[var(--color-text-dim)] truncate">{opt.label}</span>}
                  {opt?.free && <FreeBadge />}
                  <span className={`text-xs px-1.5 py-0.5 rounded ${valid ? "text-[var(--color-success)]" : "text-[var(--color-danger)]"}`}>
                    {valid ? "✓" : "✗"}
                  </span>
                  <button onClick={() => moveAutoItem(item.upstream, item.model, -1)} disabled={idx === 0} className="w-6 h-6 rounded hover:bg-[var(--color-surface-2)] disabled:opacity-30 text-xs">↑</button>
                  <button onClick={() => moveAutoItem(item.upstream, item.model, 1)} disabled={idx === flatChain.length - 1} className="w-6 h-6 rounded hover:bg-[var(--color-surface-2)] disabled:opacity-30 text-xs">↓</button>
                  <ConfirmPopover
                    title="移除该模型？"
                    onConfirm={() => removeAutoItem(item.upstream, item.model)}
                    triggerClassName="w-6 h-6 rounded hover:bg-[var(--color-danger)]/20 text-[var(--color-danger)] text-xs"
                  />
                </div>
              );
            })}
          </div>
        )}

        {/* 添加新项 */}
        <AutoChainAdder available={available} credValid={credValid} onAdd={addAutoItem} upstreamLabel={labelOf} />
      </section>
      )}

      {/* 全局兜底（auto/manual 模式） */}
      {cfg.mode !== "ua" && (
      <section className="bg-[var(--color-surface)] rounded-xl p-5 border border-[var(--color-border)]">
        <h2 className="font-semibold mb-1">全局兜底</h2>
        <p className="text-xs text-[var(--color-text-dim)] mb-3">
          所有链都失败时的最终保底模型（建议选凭据稳定的 upstream）。
        </p>
        <ModelPicker
          available={available}
          credValid={credValid}
          value={cfg.globalFallback}
          onChange={(ref) => setCfg({ ...cfg, globalFallback: ref })}
          upstreamLabel={labelOf}
        />
      </section>
      )}

      {/* 手动模式降级链（仅手动模式显示） */}
      {cfg.mode === "manual" && (
      <section className="bg-[var(--color-surface)] rounded-xl p-5 border border-[var(--color-border)]">
        <h2 className="font-semibold mb-1">手动模式降级链（可选）</h2>
        <p className="text-xs text-[var(--color-text-dim)] mb-3">
          客户端指定某模型失败时，按此链降级。未配置的模型失败后直接走全局兜底。
        </p>

        {manualKeys.length === 0 ? (
          <div className="text-sm text-[var(--color-text-dim)] py-2 mb-2">未配置任何手动降级链</div>
        ) : (
          <div className="space-y-3 mb-3">
            {manualKeys.map((key) => {
              const chain = cfg.manualFallbacks[key] || [];
              return (
                <div key={key} className="bg-[var(--color-bg)] rounded-lg p-3 border border-[var(--color-border)]">
                  <div className="flex items-center gap-2 mb-2">
                    <span className="text-xs text-[var(--color-text-dim)]">指定模型：</span>
                    <code className="px-2 py-0.5 rounded bg-[var(--color-surface-2)] font-mono text-xs">{key}</code>
                    <span className="text-xs text-[var(--color-text-dim)]">→ 失败后尝试：</span>
                  </div>
                  <div className="space-y-1.5 ml-4">
                    {chain.map((ref, idx) => (
                      <div key={idx} className="flex items-center gap-2">
                        <span className="text-xs text-[var(--color-text-dim)] w-4">{idx + 1}</span>
                        <span className="text-xs px-2 py-0.5 rounded bg-[var(--color-surface-2)]">{labelOf(ref.upstream)}</span>
                        <code className="flex-1 font-mono text-xs">{ref.model}</code>
                        <ConfirmPopover
                          title="移除该降级项？"
                          onConfirm={() => removeManualFallback(key, idx)}
                          triggerClassName="w-5 h-5 rounded hover:bg-[var(--color-danger)]/20 text-[var(--color-danger)] text-xs"
                        />
                      </div>
                    ))}
                  </div>
                </div>
              );
            })}
          </div>
        )}

        {/* 添加手动降级 */}
        <div className="flex items-center gap-2 flex-wrap pt-3 border-t border-[var(--color-border)]">
          <span className="text-xs text-[var(--color-text-dim)]">指定模型：</span>
          <select
            value={newManualKey}
            onChange={(e) => setNewManualKey(e.target.value)}
            className="px-2 py-1 text-xs rounded-md bg-[var(--color-surface-2)] border border-[var(--color-border)]"
          >
            <option value="">选择模型...</option>
            {allModels.filter((m) => credValid[m.upstream]).map((m) => (
              <option key={`${m.upstream}-${m.model}`} value={m.model}>
                {labelOf(m.upstream)}/{m.model}
              </option>
            ))}
          </select>
          <span className="text-xs text-[var(--color-text-dim)]">→ 降级到：</span>
          <select
            value={newManualUpstream}
            onChange={(e) => setNewManualUpstream(e.target.value)}
            className="px-2 py-1 text-xs rounded-md bg-[var(--color-surface-2)] border border-[var(--color-border)]"
          >
            {available.filter((u) => credValid[u.upstream]).map((u) => (
              <option key={u.upstream} value={u.upstream}>{labelOf(u.upstream)}</option>
            ))}
          </select>
          <ModelSelect
            options={available.find((u) => u.upstream === newManualUpstream && credValid[u.upstream])?.models ?? []}
            value={newManualModel}
            onChange={(id) => setNewManualModel(id)}
            placeholder="选择模型..."
          />
          <button
            onClick={addManualFallback}
            disabled={!newManualKey || !newManualModel}
            className="px-3 py-1 text-xs rounded-md bg-[var(--color-primary)] hover:opacity-90 disabled:opacity-50"
          >
            + 添加
          </button>
        </div>
      </section>
      )}

      {/* UA 模式兜底（仅 ua 模式显示） */}
      {cfg.mode === "ua" && (
      <section className="bg-[var(--color-surface)] rounded-xl p-5 border border-[var(--color-border)]">
        <h2 className="font-semibold mb-1">UA 模式兜底</h2>
        <p className="text-xs text-[var(--color-text-dim)] mb-3">
          UA 规则未命中或目标模型失败时的最终保底模型。
        </p>
        <ModelPicker
          available={available}
          credValid={credValid}
          value={cfg.uaGlobalFallback ?? ({} as ModelRef)}
          onChange={(ref) => setCfg({ ...cfg, uaGlobalFallback: ref })}
          upstreamLabel={labelOf}
        />
      </section>
      )}

      {/* User-Agent 路由 */}
      <section className="bg-[var(--color-surface)] rounded-xl p-5 border border-[var(--color-border)]">
        <div className="flex items-center justify-between mb-1">
          <h2 className="font-semibold">User-Agent 路由</h2>
          {cfg.mode === "ua" ? (
            <span className="text-xs text-[var(--color-primary)] bg-[var(--color-primary)]/10 px-2 py-0.5 rounded">ua 模式始终启用</span>
          ) : (
            <label className="flex items-center gap-2 cursor-pointer">
              <input
                type="checkbox"
                checked={!!cfg.uaRoutingEnabled}
                onChange={(e) => setCfg({ ...cfg, uaRoutingEnabled: e.target.checked })}
                className="w-4 h-4 accent-[var(--color-primary)]"
              />
              <span className="text-sm">{cfg.uaRoutingEnabled ? "已启用" : "已关闭"}</span>
            </label>
          )}
        </div>
        <p className="text-xs text-[var(--color-text-dim)] mb-3">
          {cfg.mode === "ua"
            ? "ua 模式：根据 User-Agent 分流。命中具体 mapping 用映射目标，否则走该规则的「默认目标」，最后才用 UA 兜底。"
            : "auto/manual 叠加层：根据 User-Agent 分流。命中具体 mapping 用映射目标，否则走该规则的「默认目标」，最后回退正常降级链。"}
        </p>

        <div className="space-y-3">
          {(cfg.uaRules ?? []).map((rule) => {
            const draft = uaDraft[rule.id] ?? { requestedModel: "", upstream: "joycode", model: "" };
            const validUpstreams = available.filter((u) => u.upstream !== "opencode" && credValid[u.upstream]);
            const historyModels = uaHistory[rule.id] ?? [];
            const usedPatterns = (cfg.uaRules ?? []).filter((r) => r.id !== rule.id).map((r) => r.pattern);
            return (
              <div key={rule.id} className="bg-[var(--color-bg)] rounded-lg p-3 border border-[var(--color-border)]">
                <div className="flex items-center gap-2 mb-2 flex-wrap">
                  <input
                    type="checkbox"
                    checked={rule.enabled}
                    onChange={(e) => updateUaRule(rule.id, { enabled: e.target.checked })}
                    className="w-4 h-4 accent-[var(--color-primary)] shrink-0"
                  />
                  <UASelect
                    sources={uaSources}
                    value={rule.pattern}
                    usedValues={usedPatterns}
                    onChange={(source) => {
                      if (source) {
                        selectUaSource(rule.id, source);
                      } else {
                        updateUaRule(rule.id, { name: "", pattern: "" });
                      }
                    }}
                    onCustomInput={(val) => updateUaRule(rule.id, { name: val, pattern: val })}
                  />
                  <ConfirmPopover
                    title="删除该规则？"
                    onConfirm={() => removeUaRule(rule.id)}
                    triggerClassName="w-6 h-6 rounded hover:bg-[var(--color-surface-2)] text-[var(--color-text-dim)] text-xs shrink-0"
                  >
                    <TrashIcon />
                  </ConfirmPopover>
                </div>

                {/* 规则级默认目标：UA 命中但未命中具体 mapping 时整 UA 路由到此（免维护模型清单） */}
                <div className="flex items-center gap-2 flex-wrap ml-6 mb-2 text-xs">
                  <span className="text-[var(--color-text-dim)] shrink-0">默认目标</span>
                  {(() => {
                    const dt = rule.defaultTarget ?? ({} as ModelRef);
                    const has = !!(dt.upstream && dt.model);
                    const dispUp = has ? dt.upstream : validUpstreams[0]?.upstream || "";
                    const models = validUpstreams.find((u) => u.upstream === dispUp)?.models ?? [];
                    const dispModel = has ? dt.model : models[0]?.id || "";
                    return (
                      <>
                        <select
                          value={dispUp}
                          onChange={(e) => {
                            const first = validUpstreams.find((u) => u.upstream === e.target.value)?.models[0];
                            updateUaRule(rule.id, { defaultTarget: { upstream: e.target.value, model: first?.id ?? "" } as ModelRef });
                          }}
                          className="px-2 py-1 text-xs rounded-md bg-[var(--color-surface-2)] border border-[var(--color-border)]"
                        >
                          {validUpstreams.length === 0 && <option value="">无可用凭据</option>}
                          {validUpstreams.map((u) => (
                            <option key={u.upstream} value={u.upstream}>{labelOf(u.upstream)}</option>
                          ))}
                        </select>
                        <ModelSelect
                          options={models}
                          value={dispModel}
                          onChange={(id) => updateUaRule(rule.id, { defaultTarget: { upstream: dispUp, model: id } as ModelRef })}
                          placeholder="目标模型..."
                          className="w-48"
                        />
                        {has && (
                          <button
                            onClick={() => updateUaRule(rule.id, { defaultTarget: undefined })}
                            title="清除默认目标（回退 UA 全局兜底）"
                            className="w-5 h-5 rounded hover:bg-[var(--color-surface-2)] text-[var(--color-text-dim)]"
                          >
                            <TrashIcon />
                          </button>
                        )}
                        {!has && (
                          <span className="text-[var(--color-text-dim)]">（未设置，未命中 mapping 时走 UA 兜底）</span>
                        )}
                      </>
                    );
                  })()}
                </div>

                {/* 映射列表 */}
                {(rule.mappings ?? []).length > 0 && (
                  <div className="space-y-1 ml-6 mb-2">
                    {(rule.mappings ?? []).map((m, idx) => (
                      <div key={idx} className="flex items-center gap-2 text-xs">
                        <code className="px-1.5 py-0.5 rounded bg-[var(--color-surface-2)] font-mono">{m.requestedModel}</code>
                        <span className="text-[var(--color-text-dim)]">→</span>
                        <span className="px-1.5 py-0.5 rounded bg-[var(--color-surface-2)]">{labelOf(m.target.upstream)}</span>
                        <code className="font-mono">{m.target.model}</code>
                        <button
                          onClick={() => removeUaMapping(rule.id, idx)}
                          className="ml-auto w-5 h-5 rounded hover:bg-[var(--color-surface-2)] text-[var(--color-text-dim)]"
                        >
                          <TrashIcon />
                        </button>
                      </div>
                    ))}
                  </div>
                )}

                {/* 添加映射 */}
                <div className="flex items-center gap-2 flex-wrap ml-6 pt-2 border-t border-[var(--color-border)]">
                  <RequestedModelSelect
                    historyModels={historyModels}
                    usedModels={(rule.mappings ?? []).map((m) => m.requestedModel)}
                    value={draft.requestedModel}
                    onChange={(val) => setUaDraft({ ...uaDraft, [rule.id]: { ...draft, requestedModel: val } })}
                  />
                  <span className="text-xs text-[var(--color-text-dim)]">→</span>
                  <select
                    value={draft.upstream}
                    onChange={(e) => setUaDraft({ ...uaDraft, [rule.id]: { ...draft, upstream: e.target.value, model: "" } })}
                    className="px-2 py-1 text-xs rounded-md bg-[var(--color-surface-2)] border border-[var(--color-border)]"
                  >
                    {validUpstreams.map((u) => (
                      <option key={u.upstream} value={u.upstream}>{labelOf(u.upstream)}</option>
                    ))}
                  </select>
                  <ModelSelect
                    options={validUpstreams.find((u) => u.upstream === draft.upstream)?.models ?? []}
                    value={draft.model}
                    onChange={(id) => setUaDraft({ ...uaDraft, [rule.id]: { ...draft, model: id } })}
                    placeholder="目标模型..."
                    className="w-48"
                  />
                  <button
                    onClick={() => addUaMapping(rule.id)}
                    disabled={!draft.requestedModel || !draft.model}
                    className="px-2 py-1 text-xs rounded-md bg-[var(--color-primary)] hover:opacity-90 disabled:opacity-50"
                  >
                    + 添加
                  </button>
                </div>
              </div>
            );
          })}
        </div>

        <button
          onClick={addUaRule}
          className="mt-3 w-full py-2 text-xs rounded-lg border border-dashed border-[var(--color-border)] text-[var(--color-text-dim)] hover:border-[var(--color-primary)] hover:text-[var(--color-primary)]"
        >
          + 新增 UA 规则
        </button>
      </section>

        {/* 配置 JSON 预览（完整实时，与当前配置一致；apiKey 默认脱敏） */}
        {(() => {
          // 完整配置：剔除 Go 序列化不导出/运行期私有字段（mu/path 已被后端 json:"-" 排除，cfg 即完整配置）
          // apiKey 默认脱敏，点眼睛临时显示明文（与上方 apiKey 输入框行为一致）
          const fullCfg = { ...cfg, apiKey: showCfgKey ? cfg.apiKey ?? "" : maskKey(cfg.apiKey ?? "") };
          const fullJson = JSON.stringify(fullCfg, null, 2);
          return (
            <details className="bg-[var(--color-surface)] rounded-xl p-4 border border-[var(--color-border)]">
              <summary className="cursor-pointer text-sm text-[var(--color-text-dim)]">查看完整运行配置 JSON（实时）</summary>
              <div className="relative mt-3">
                <pre className="text-xs font-mono p-3 rounded bg-[var(--color-bg)] overflow-x-auto max-h-96 overflow-y-auto">
{fullJson}
                </pre>
                <button
                  onClick={() => setShowCfgKey((v) => !v)}
                  title={showCfgKey ? "隐藏 apiKey" : "显示 apiKey 明文"}
                  className="absolute top-2 right-2 px-1.5 py-0.5 text-xs rounded bg-[var(--color-surface-2)] hover:bg-[var(--color-border)]"
                >
                  {showCfgKey ? "🙈" : "👁"}
                </button>
              </div>
              <div className="mt-2 flex items-center gap-2">
                <CopyButton text={fullJson} label="复制完整配置" />
                <span className="text-[10px] text-[var(--color-text-dim)]">
                  {showCfgKey ? "⚠️ 当前 apiKey 为明文，复制内容含真实密钥" : "apiKey 已脱敏，复制内容不含真实密钥"}
                </span>
              </div>
            </details>
          );
        })()}
      </div>
      )}

      {/* ===== 费率管理 ===== */}
      {tab === "pricing" && (
      <div className="space-y-6">
        <PricingEditor />
      </div>
      )}

      {/* ===== 自动升级 ===== */}
      {tab === "update" && (
      <div className="space-y-6">
        <UpdatePanel />
      </div>
      )}

      {/* ===== 关于 ===== */}
      {tab === "about" && <AboutSection />}
    </div>
  );
}

// maskKey 隐藏 apiKey：显示前 10 位，其余用等长 * 代替
function maskKey(key: string): string {
  if (!key) return "";
  if (key.length <= 10) return key;
  return key.slice(0, 10) + "*".repeat(key.length - 10);
}

// ====== TabBtn：设置页顶部 tab ======
function TabBtn({ label, active, onClick }: { label: string; active: boolean; onClick: () => void }) {
  return (
    <button
      onClick={onClick}
      className={`px-4 py-2 text-sm font-medium rounded-t-lg transition-colors -mb-px ${
        active
          ? "border border-b-0 border-[var(--color-border)] bg-[var(--color-surface)] text-[var(--color-primary)]"
          : "text-[var(--color-text-dim)] hover:text-[var(--color-text)] hover:bg-[var(--color-surface)]/50"
      }`}
    >
      {label}
    </button>
  );
}

// ====== AutoChainAdder：auto 链添加器 ======
function AutoChainAdder({
  available,
  credValid,
  onAdd,
  upstreamLabel,
}: {
  available: UpstreamModels[];
  credValid: Record<string, boolean>;
  onAdd: (upstream: string, model: string) => void;
  upstreamLabel: (up: string) => string;
}) {
  const validAvailable = available.filter((u) => u.upstream !== "opencode" && credValid[u.upstream]);
  const [upstream, setUpstream] = useState(validAvailable[0]?.upstream ?? "");
  const [model, setModel] = useState("");

  // 首次加载 available 后，如果 upstream 为空（初始时可用列表为空），自动选中第一个
  useEffect(() => {
    if (!upstream && validAvailable.length > 0) {
      setUpstream(validAvailable[0].upstream);
    }
  }, [validAvailable, upstream]);

  const models = validAvailable.find((u) => u.upstream === upstream)?.models ?? [];

  return (
    <div className="flex items-center gap-2 flex-wrap pt-3 border-t border-[var(--color-border)]">
      <select
        value={upstream}
        onChange={(e) => {
          setUpstream(e.target.value);
          setModel("");
        }}
        className="px-2 py-1 text-xs rounded-md bg-[var(--color-surface-2)] border border-[var(--color-border)]"
      >
        {validAvailable.length === 0 && (
          <option value="">无可用凭据</option>
        )}
        {validAvailable.map((u) => (
          <option key={u.upstream} value={u.upstream}>
            {upstreamLabel(u.upstream)}
          </option>
        ))}
      </select>
      <ModelSelect
        options={models}
        value={model}
        onChange={(id) => setModel(id)}
        placeholder="选择模型..."
        className="flex-1 min-w-[200px]"
      />
      <button
        onClick={() => {
          if (model) {
            onAdd(upstream, model);
            setModel("");
          }
        }}
        disabled={!model || !upstream}
        className="px-3 py-1 text-xs rounded-md bg-[var(--color-primary)] hover:opacity-90 disabled:opacity-50"
      >
        + 添加到链
      </button>
    </div>
  );
}

// ====== ModelPicker：单个模型选择器（兜底用） ======
function ModelPicker({
  available,
  credValid,
  value,
  onChange,
  upstreamLabel,
}: {
  available: UpstreamModels[];
  credValid: Record<string, boolean>;
  value: ModelRef;
  onChange: (ref: ModelRef) => void;
  upstreamLabel: (up: string) => string;
}) {
  const validAvailable = available.filter((u) => u.upstream !== "opencode" && credValid[u.upstream]);
  // 未选择 upstream/model 时（首次进入、available 还在加载），纯显示层回退到第一个可用项，
  // 避免下拉框空白；不写入 state，用户真正选择时才触发 onChange/保存
  const displayUpstream = value.upstream || validAvailable[0]?.upstream || "";
  const models = validAvailable.find((u) => u.upstream === displayUpstream)?.models ?? [];
  const displayModel = value.model || models[0]?.id || "";
  return (
    <div className="flex items-center gap-2">
      <select
        value={displayUpstream}
        onChange={(e) => {
          const first = validAvailable.find((u) => u.upstream === e.target.value)?.models[0];
          onChange({ upstream: e.target.value, model: first?.id ?? "" } as ModelRef);
        }}
        className="px-2 py-1.5 text-sm rounded-md bg-[var(--color-surface-2)] border border-[var(--color-border)]"
      >
        {validAvailable.length === 0 && (
          <option value="">无可用凭据</option>
        )}
        {validAvailable.map((u) => (
          <option key={u.upstream} value={u.upstream}>{upstreamLabel(u.upstream)}</option>
        ))}
      </select>
      <ModelSelect
        options={models}
        value={displayModel}
        onChange={(id) => onChange({ upstream: displayUpstream, model: id } as ModelRef)}
        placeholder="选择模型..."
        className="flex-1"
      />
    </div>
  );
}

// ====== AboutSection：关于页面 ======
function AboutSection() {
  const [version, setVersion] = useState<string>("");

  useEffect(() => {
    UpdaterService.GetCurrentVersion().then((v) => setVersion(v ?? ""));
  }, []);

  return (
    <section className="bg-[var(--color-surface)] rounded-xl p-8 border border-[var(--color-border)]">
      <div className="flex flex-col items-center text-center">
        {/* Logo + 应用名（点击跳转 GitHub） */}
        <button
          type="button"
          onClick={() => ProviderAPIService.OpenURL("https://github.com/rosanruan/switch-dev")}
          title="在 GitHub 上查看项目"
          className="flex flex-col items-center bg-transparent border-0 p-0"
        >
          <img
            src="/switch-dev-64.png"
            alt="Switch Dev"
            className="w-16 h-16 mb-4"
            draggable={false}
          />
          <div className="mb-2">
            <span className="text-2xl font-bold text-[var(--color-primary)] tracking-widest">SWITCH</span>
            <span className="text-lg font-semibold text-[var(--color-text-dim)] tracking-widest ml-1.5">DEV</span>
          </div>
        </button>

        {/* 版本号 */}
        <span className="text-sm text-[var(--color-text-dim)] font-mono mb-6">
          v{version || "-"}
        </span>

        {/* 分隔线 */}
        <div className="w-48 h-px bg-[var(--color-border)] mb-6" />

        {/* 功能描述 */}
        <p className="text-sm text-[var(--color-text-dim)] max-w-sm leading-relaxed mb-6">
          本地多上游 LLM 代理，将 JoyCode / DevEco / WorkBuddy
          模型能力暴露为标准 Anthropic / OpenAI 接口，供 Claude Code 等工具复用。
        </p>

        {/* 分隔线 */}
        <div className="w-48 h-px bg-[var(--color-border)] mb-6" />

        {/* 元信息 */}
        <div className="space-y-2 text-xs text-[var(--color-text-dim)]">
          <div className="flex items-center gap-2">
            <span>🛠</span>
            <span>Wails v3 + Go</span>
          </div>
        </div>
      </div>
    </section>
  );
}

// ====== UASelect：User-Agent 可搜索下拉（支持历史来源 + 自由输入） ======
function UASelect({
  sources,
  value,
  usedValues,
  onChange,
  onCustomInput,
}: {
  sources: { name: string; userAgent: string; count: number }[];
  value: string;
  usedValues: string[];
  onChange: (source: { name: string; userAgent: string } | null) => void;
  onCustomInput: (val: string) => void;
}) {
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState(value);
  const ref = useRef<HTMLDivElement>(null);

  useEffect(() => { setQuery(value); }, [value]);

  useEffect(() => {
    if (!open) return;
    const handler = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) {
        setOpen(false);
        if (query !== value) onCustomInput(query);
      }
    };
    document.addEventListener("mousedown", handler);
    return () => document.removeEventListener("mousedown", handler);
  }, [open, query, value, onCustomInput]);

  const used = new Set(usedValues.filter(Boolean));
  const filtered = sources.filter(
    (s) =>
      s.name.toLowerCase().includes(query.toLowerCase()) ||
      s.userAgent.toLowerCase().includes(query.toLowerCase())
  );

  return (
    <div ref={ref} className="relative flex-1 min-w-[160px]">
      <input
        value={query}
        onChange={(e) => { setQuery(e.target.value); setOpen(true); }}
        onFocus={() => setOpen(true)}
        onKeyDown={(e) => {
          if (e.key === "Enter") {
            setOpen(false);
            onCustomInput(query);
          }
        }}
        placeholder="选择或输入 User-Agent..."
        className="w-full px-2 py-1 text-xs rounded-md bg-[var(--color-surface-2)] border border-[var(--color-border)] font-mono"
      />
      {open && (
        <div className="absolute z-50 mt-1 w-full max-h-48 overflow-y-auto rounded-md border border-[var(--color-border)] bg-[var(--color-surface)] shadow-lg">
          {filtered.length === 0 && query && (
            <button
              onClick={() => { onCustomInput(query); setOpen(false); }}
              className="w-full px-2 py-1.5 text-left text-xs hover:bg-[var(--color-surface-2)]"
            >
              使用 "{query}"
            </button>
          )}
          {filtered.map((s) => {
            const isUsed = used.has(s.name) || used.has(s.userAgent);
            return (
              <button
                key={s.userAgent}
                disabled={isUsed}
                onClick={() => { onChange({ name: s.name, userAgent: s.userAgent }); setOpen(false); setQuery(s.name); }}
                className={`w-full px-2 py-1.5 text-left text-xs flex items-center gap-2 ${
                  isUsed ? "opacity-40 cursor-not-allowed" : "hover:bg-[var(--color-surface-2)]"
                }`}
              >
                <span className="font-medium shrink-0">{s.name}</span>
                <span className="text-[var(--color-text-dim)] truncate flex-1 font-mono">{s.userAgent}</span>
                <span className="text-[var(--color-text-dim)] shrink-0">×{s.count}</span>
              </button>
            );
          })}
        </div>
      )}
    </div>
  );
}

// ====== RequestedModelSelect：请求模型可搜索下拉（历史模型 + 自由输入） ======
function RequestedModelSelect({
  historyModels,
  usedModels,
  value,
  onChange,
}: {
  historyModels: { model: string; count: number }[];
  usedModels: string[];
  value: string;
  onChange: (val: string) => void;
}) {
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState(value);
  const ref = useRef<HTMLDivElement>(null);

  useEffect(() => { setQuery(value); }, [value]);

  useEffect(() => {
    if (!open) return;
    const handler = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) {
        setOpen(false);
        if (query !== value) onChange(query);
      }
    };
    document.addEventListener("mousedown", handler);
    return () => document.removeEventListener("mousedown", handler);
  }, [open, query, value, onChange]);

  const used = new Set(usedModels.map((m) => m.toLowerCase()));
  const filtered = historyModels.filter((h) =>
    h.model.toLowerCase().includes(query.toLowerCase())
  );

  return (
    <div ref={ref} className="relative w-44">
      <input
        value={query}
        onChange={(e) => { setQuery(e.target.value); setOpen(true); }}
        onFocus={() => setOpen(true)}
        onKeyDown={(e) => {
          if (e.key === "Enter") { setOpen(false); onChange(query); }
        }}
        placeholder="请求模型"
        className="w-full px-2 py-1 text-xs rounded-md bg-[var(--color-surface-2)] border border-[var(--color-border)] font-mono"
      />
      {open && (
        <div className="absolute z-50 mt-1 w-full max-h-48 overflow-y-auto rounded-md border border-[var(--color-border)] bg-[var(--color-surface)] shadow-lg">
          {query && !historyModels.some((h) => h.model === query) && (
            <button
              onClick={() => { onChange(query); setOpen(false); }}
              className="w-full px-2 py-1.5 text-left text-xs hover:bg-[var(--color-surface-2)]"
            >
              使用 "{query}"
            </button>
          )}
          {filtered.map((h) => {
            const isUsed = used.has(h.model.toLowerCase());
            return (
              <button
                key={h.model}
                disabled={isUsed}
                onClick={() => { onChange(h.model); setOpen(false); }}
                className={`w-full px-2 py-1.5 text-left text-xs flex items-center gap-2 font-mono ${
                  isUsed ? "opacity-40 cursor-not-allowed" : "hover:bg-[var(--color-surface-2)]"
                }`}
              >
                <span className="truncate flex-1">{h.model}</span>
                <span className="text-[var(--color-text-dim)] shrink-0">×{h.count}</span>
              </button>
            );
          })}
          {filtered.length === 0 && !query && (
            <div className="px-2 py-1.5 text-xs text-[var(--color-text-dim)]">无历史模型，请手动输入</div>
          )}
        </div>
      )}
    </div>
  );
}

