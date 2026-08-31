import { useEffect, useState, useCallback, useRef } from "react";
import { ProviderAPIService, ConfigService } from "../../bindings/switchdev/service";
import type { Catalog, CatalogModel, ProviderConfig, ProviderModel } from "../../bindings/switchdev/providerapi/models";
import { useWailsEvent } from "../hooks/useWailsEvent";
import ProviderPicker from "./ProviderPicker";
import ConfirmPopover from "./ConfirmPopover";
import ShareDialog from "./ShareDialog";
import UnlockScreen from "./UnlockScreen";
import SecuritySettings from "./SecuritySettings";

// 能力推断：从模型名判断（工具/视觉/推理）
function inferVision(name: string): boolean {
  return /vision|vl|visual|multimodal/i.test(name);
}
function inferReasoning(name: string): boolean {
  return /reason|think|thinking|r1|o1|deepseek-reasoner|k2-thinking/i.test(name);
}
function inferTool(name: string): boolean {
  return /tool|function|coder|code|agent/i.test(name);
}

// 解析 context 文本（"1M" -> 1000000, "131K" -> 131000）；解析失败返回 0
function parseContext(s: string): number {
  if (!s) return 0;
  const m = s.match(/^([\d.]+)\s*([KMB])?/i);
  if (!m) return 0;
  const num = parseFloat(m[1]);
  const unit = (m[2] ?? "").toUpperCase();
  if (unit === "M") return Math.round(num * 1000000);
  if (unit === "K") return Math.round(num * 1000);
  if (unit === "B") return Math.round(num * 1000000000);
  return Math.round(num);
}

// 把存储的 context 整数还原为紧凑文本（131000 -> "131K"）；0 返回空串
function formatContext(n: number): string {
  if (!n) return "";
  if (n >= 1_000_000_000) return (n / 1_000_000_000) + "B";
  if (n >= 1_000_000) return (n / 1_000_000) + "M";
  if (n >= 1_000) return (n / 1_000) + "K";
  return String(n);
}

export default function ProviderAPI() {
  const [providers, setProviders] = useState<Record<string, ProviderConfig | null>>({});
  const [catalog, setCatalog] = useState<Catalog | null>(null);
  const [msg, setMsg] = useState<{ type: "ok" | "err"; text: string } | null>(null);

  // 添加表单状态
  const [showAdd, setShowAdd] = useState(false);
  const [fromCatalog, setFromCatalog] = useState(true);
  const [selectedCatalog, setSelectedCatalog] = useState<string>("");
  const [customName, setCustomName] = useState("");
  const [customId, setCustomId] = useState("");
  const [baseURL, setBaseURL] = useState("");
  const [apiKey, setApiKey] = useState("");
  const [showApiKey, setShowApiKey] = useState(false); // 眼睛切换明文/密文
  const [editingImported, setEditingImported] = useState(false); // 当前编辑的是否为导入供应商
  const [candidateModels, setCandidateModels] = useState<CatalogModel[]>([]);
  const [loadingModels, setLoadingModels] = useState(false);

  // 评测状态
  const [benchmarking, setBenchmarking] = useState<Record<string, boolean>>({});
  const [benchResult, setBenchResult] = useState<Record<string, any>>({});

  // 候选模型多选 + 搜索 + 批量测评状态
  const [selectedIds, setSelectedIds] = useState<string[]>([]);
  const [modelSearch, setModelSearch] = useState("");
  const [onlyPassed, setOnlyPassed] = useState(false);
  const [batchRunning, setBatchRunning] = useState(false);
  const [batchProgress, setBatchProgress] = useState({ done: 0, total: 0 });
  // 正在移除（取消加入）的模型 id，用于按钮 loading/禁用
  const [removingId, setRemovingId] = useState("");

  // 分享/导入对话框：
  // - 顶部「分享/导入」按钮：shareInitialId 为空，打开后显示「导出/导入」选择菜单
  // - 单个卡片「分享」：shareInitialId 为该供应商 id，跳过菜单直接进入导出态并预勾选
  const [showShare, setShowShare] = useState(false);
  const [shareInitialId, setShareInitialId] = useState<string | undefined>(undefined);

  // 安全：启动锁 + 设置
  const [locked, setLocked] = useState(false);
  const [autoLockout, setAutoLockout] = useState(false);
  const [showSecurity, setShowSecurity] = useState(false);
  // 通用配置（是否进入编辑时自动拉取并测评模型）
  const [autoBenchOnEdit, setAutoBenchOnEdit] = useState(false);

  // 注册送 token 供应商弹窗
  const [showBonus, setShowBonus] = useState(false);
  const [bonusInline, setBonusInline] = useState(false);
  const [bonusProviders, setBonusProviders] = useState<Array<{ id: string; name: string; baseUrl: string; registerUrl: string; bonusRule: string }>>([]);
  const [bonusLoading, setBonusLoading] = useState(false);
  const [selectedBonus, setSelectedBonus] = useState<string>("");
  const [bonusPage, setBonusPage] = useState(0);
  const [hasBonus, setHasBonus] = useState(false);

  // 进入编辑/新增时的表单快照（name/baseURL/apiKey），用于判断是否有未保存变化
  const formBaseline = useRef<{ name: string; baseURL: string; apiKey: string } | null>(null);
  // 切换目标确认：有未保存变化时暂存要执行的动作，用户选择保存/放弃后执行
  const [pendingSwitch, setPendingSwitch] = useState<null | (() => void)>(null);
  const [savingBeforeSwitch, setSavingBeforeSwitch] = useState(false);

  const [refreshing, setRefreshing] = useState(false);
  // 正在编辑的供应商 id（空表示新增）
  const [editingId, setEditingId] = useState<string>("");
  // 当前表单对应的有效供应商 id（编辑时 = editingId；新增目录选择时 = selectedCatalog；
  // 新增自定义时自动生成并固化）。用于把已验证模型关联到正确的供应商，无论是否已手动保存。
  const [effectiveId, setEffectiveId] = useState<string>("");

  const load = useCallback(async () => {
    const [ps, cat] = await Promise.all([
      ProviderAPIService.GetProviders(),
      ProviderAPIService.GetCatalog(),
    ]);
    const cleaned: Record<string, ProviderConfig | null> = {};
    for (const k of Object.keys(ps ?? {})) {
      cleaned[k] = ps[k] ?? null;
    }
    setProviders(cleaned);
    setCatalog(cat);
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  // 启动后检查是否被锁定（钥匙串没有记住主密码等）
  const checkLock = useCallback(async () => {
    try {
      const info = await ProviderAPIService.GetLockStatus();
      setHasMaster(!!info?.masterSet);
      // 自动加密锁死（更新后随机密钥丢失）：走专门的「重新配置」界面，不弹密码框。
      if (info?.autoLockout) {
        setAutoLockout(true);
        setLocked(true);
        return;
      }
      setAutoLockout(false);
      if (info?.isLocked && info?.remembered) {
        // 钥匙串记住密码：尝试自动解锁，成功则无感进入
        const ok = await ProviderAPIService.TryAutoUnlock();
        setLocked(!ok);
      } else {
        setLocked(!!info?.isLocked);
      }
    } catch {
      // ignore
    }
  }, []);

  useEffect(() => {
    checkLock();
    // 读取通用配置：是否进入编辑时自动拉取并测评 + 是否闲置自动锁定
    ConfigService.GetConfig()
      .then((c) => {
        setAutoBenchOnEdit(!!c?.provider?.autoBenchmarkOnEdit);
        setIdleAutoLock(c?.provider?.idleAutoLock !== false);
      })
      .catch(() => {});
    // 预取注册送 token 供应商列表（只判断有没有数据）
    ProviderAPIService.GetBonusProviders()
      .then((list) => setHasBonus((list ?? []).length > 0))
      .catch(() => {});
  }, [checkLock]);

  // 无操作自动锁定 + 手动锁定
  const lastActivity = useRef<number>(Date.now());
  const [hasMaster, setHasMaster] = useState(false);
  const [idleAutoLock, setIdleAutoLock] = useState(true);
  const doLock = useCallback(async () => {
    await ProviderAPIService.Lock();
    // 尝试用钥匙串记住的密码自动解锁；无法自动解锁才显示解锁界面
    const ok = await ProviderAPIService.TryAutoUnlock();
    setLocked(!ok);
  }, []);
  useEffect(() => {
    ProviderAPIService.GetLockStatus()
      .then((info) => setHasMaster(!!info?.masterSet))
      .catch(() => {});
  }, []);
  useEffect(() => {
    const onActivity = () => {
      lastActivity.current = Date.now();
    };
    const events = ["mousemove", "mousedown", "keydown", "touchstart", "click"];
    events.forEach((e) => window.addEventListener(e, onActivity, true));
    // 5 分钟（300s）无操作触发 UI 锁定；仅当设置了主密码且开启闲置锁定时生效
    const timer = setInterval(() => {
      if (locked || !hasMaster || !idleAutoLock) return;
      const idleMs = Date.now() - lastActivity.current;
      if (idleMs >= 300 * 1000) {
        doLock();
      }
    }, 5000);
    return () => {
      events.forEach((e) => window.removeEventListener(e, onActivity, true));
      clearInterval(timer);
    };
  }, [locked, hasMaster, idleAutoLock, doLock]);

  // 凭据变化时刷新（新增/删除供应商后）
  useWailsEvent("providerapi:change", () => load());
  useWailsEvent("cred:change", () => load());
  // 后端批量测评进度/结果
  const batchOkRef = useRef<string[]>([]);
  const batchTargetsRef = useRef<CatalogModel[]>([]);
  const batchResRef = useRef<Record<string, any>>({});
  // 本会话已确保存在（已 UpsertProvider）的供应商 id，避免逐个加入模型时重复
  // UpsertProvider（整体覆盖且传入 models:[] 会把前一个刚加入的模型冲掉）
  const ensuredProvidersRef = useRef<Set<string>>(new Set());
  useWailsEvent("providerapi:bench", (data: any) => {
    const mid = data?.modelId as string;
    if (mid) {
      const res = data.result ?? {};
      batchResRef.current[mid] = res;
      setBenchResult((p) => ({ ...p, [mid]: res }));
      setBenchmarking((p) => ({ ...p, [mid]: false }));
      if (res?.success) {
        batchOkRef.current = [...batchOkRef.current, mid];
      }
    }
    if (typeof data?.done === "number" && typeof data?.total === "number") {
      setBatchProgress({ done: data.done, total: data.total });
    }
  });

  // 批量测评全部完成（后台 goroutine 结束）
  useWailsEvent("providerapi:bench-done", async (data: any) => {
    setBatchRunning(false);
    const targets = batchTargetsRef.current;
    for (const m of targets) {
      setBenchmarking((p) => ({ ...p, [m.id]: false }));
    }
    const okIds = batchOkRef.current;
    const total = data?.total ?? targets.length;
    // 测评通过的模型自动加入（新增/编辑模式都生效）；已加入的只回写速度，不重复计数。
    const pid = getEffectiveId();
    const alreadyAddedIds = new Set(
      (providers[pid]?.models ?? []).filter((m) => m.verified).map((m) => m.id)
    );
    let autoAdded: string[] = [];
    for (const mid of okIds) {
      if (alreadyAddedIds.has(mid)) {
        // 已加入：回写最新测速
        const m = targets.find((x) => x.id === mid);
        if (m) {
          await ProviderAPIService.AddVerifiedModel(pid, {
            id: m.id,
            context: parseContext(m.context),
            verified: true,
            healthy: true,
            failCount: 0,
            tps: batchResRef.current[mid]?.tps ?? 0,
          } as ProviderModel);
        }
        continue;
      }
      const m = targets.find((x) => x.id === mid);
      if (m) {
        const ok = await persistVerifiedModel(m, batchResRef.current[mid]?.tps ?? 0, pid);
        if (ok) autoAdded.push(mid);
      }
    }
    if (autoAdded.length > 0 || alreadyAddedIds.size > 0) await load();
    // 已加入的模型取消勾选（成功且已加入的都不再保持选中）
    setSelectedIds((ids) =>
      ids.filter((id) => !(okIds.includes(id) && (autoAdded.includes(id) || alreadyAddedIds.has(id))))
    );
    flash(
      "ok",
      `批量测评完成：成功 ${okIds.length}/${total}` +
        (autoAdded.length > 0 ? `，已自动加入 ${autoAdded.length} 个` : "")
    );
    batchTargetsRef.current = [];
    batchOkRef.current = [];
    batchResRef.current = {};
  });

  const flash = (type: "ok" | "err", text: string) => {
    setMsg({ type, text });
    setTimeout(() => setMsg(null), 3000);
  };

  // 选择目录供应商 -> 自动填 base_url
  const onSelectCatalog = (id: string) => {
    setSelectedCatalog(id);
    setEffectiveId(""); // 目录模式以 selectedCatalog 为准
    const p = catalog?.providers.find((x) => x.id === id);
    setBaseURL(p?.base_url ?? "");
    setCandidateModels(p?.free_models ?? []);
    setBenchResult({});
    setSelectedIds([]);
    setModelSearch("");
    setOnlyPassed(false);
    setBatchProgress({ done: 0, total: 0 });
  };

  // 当前表单对应的供应商 id（编辑 / 目录新增 / 自定义新增统一入口）
  // 自定义新增时若无 id 则生成并固化，保证「评测 -> 加入」全程关联同一供应商
  const getEffectiveId = (): string => {
    if (editingId) return editingId;
    if (fromCatalog && selectedCatalog) return selectedCatalog;
    if (customId.trim()) return customId.trim();
    if (effectiveId) return effectiveId;
    const id = "custom-" + Math.random().toString(36).slice(2, 8);
    setEffectiveId(id);
    setCustomId(id);
    return id;
  };

  // 当前表单对应的出站协议：优先已保存 provider 的 protocol，其次目录条目，默认 openai
  const getEffectiveProtocol = (): string => {
    const eid = editingId || (fromCatalog && selectedCatalog ? selectedCatalog : "") || effectiveId;
    const existing = eid ? providers[eid] : undefined;
    if (existing?.protocol) return existing.protocol;
    const catProv = selectedCatalog ? catalog?.providers.find((x) => x.id === selectedCatalog) : undefined;
    return catProv?.protocol ?? "openai";
  };

  // 测评/拉取模型时的凭据决策：
  //  - 输入框填了 key -> 用表单的 baseURL + key（新建目录供应商/用户换 key）
  //  - 输入框为空但该供应商后端已存 key（hasKey，如分享导入的供应商）-> 走 ByID，
  //    明文 key 不经过前端，仅传 providerID + 当前表单 baseURL/protocol 作为覆盖
  //  - 都没有 -> 报错
  type BenchCreds =
    | { ok: true; useID: true; id: string; baseURL: string; protocol: string }
    | { ok: true; useID: false; baseURL: string; apiKey: string; protocol: string }
    | { ok: false; error: string };
  const resolveBenchCreds = (): BenchCreds => {
    const protocol = getEffectiveProtocol();
    if (apiKey.trim()) {
      if (!baseURL.trim()) return { ok: false, error: "请先填写 BaseURL" };
      return { ok: true, useID: false, baseURL: baseURL.trim(), apiKey: apiKey.trim(), protocol };
    }
    const eid = getEffectiveId();
    const existing = eid ? providers[eid] : undefined;
    if (existing?.hasKey) {
      if (!baseURL.trim()) return { ok: false, error: "请先填写 BaseURL" };
      return { ok: true, useID: true, id: eid, baseURL: baseURL.trim(), protocol };
    }
    return { ok: false, error: "请先填写 API Key" };
  };

  // 测试连接 + 拉取模型（用表单 key 或已保存密钥）；autoBench=true 时拉取成功后自动测评全部模型
  const testAndFetch = async (autoBench = false): Promise<CatalogModel[] | undefined> => {
    const creds = resolveBenchCreds();
    if (!creds.ok) {
      flash("err", creds.error);
      return;
    }
    const protocol = creds.protocol;
    setLoadingModels(true);
    setBenchResult({});
    setSelectedIds([]);
    setModelSearch("");
    setOnlyPassed(false);
    setBatchProgress({ done: 0, total: 0 });
    try {
      const catModels = selectedCatalog
        ? catalog?.providers.find((x) => x.id === selectedCatalog)?.free_models ?? []
        : [];

      let fetchedIDs: string[] = [];
      if (protocol === "anthropic") {
        // Anthropic 协议供应商没有标准 GET /models 列表端点；直接用目录内置模型清单
        if (catModels.length === 0) {
          flash("err", "该供应商为 Anthropic 协议且目录无预置模型，无法自动拉取");
          setCandidateModels([]);
          return;
        }
        fetchedIDs = catModels.map((m) => m.id);
      } else {
        const fetched = creds.useID
          ? await ProviderAPIService.FetchProviderModelsByID(creds.id, creds.baseURL)
          : await ProviderAPIService.FetchProviderModels(creds.baseURL, creds.apiKey);
        if (!fetched || fetched.length === 0) {
          flash("err", "未拉取到模型，请检查 BaseURL 和 API Key");
          setCandidateModels([]);
          return;
        }
        fetchedIDs = fetched.map((m) => m.ID);
      }

      // 合并目录模型信息（context/rate_limit/能力）+ 实时模型
      const merged: CatalogModel[] = fetchedIDs.map((mid) => {
        const known = catModels.find((c) => c.id === mid || c.name === mid);
        return {
          id: mid,
          name: known?.name ?? mid,
          context: known?.context ?? "",
          rate_limit: known?.rate_limit ?? "",
        } as CatalogModel;
      });
      setCandidateModels(merged);
      flash("ok", `拉取到 ${merged.length} 个模型`);
      if (autoBench && merged.length > 0) {
        // 自动勾选全部并批量测评
        setSelectedIds(merged.map((m) => m.id));
        // 等状态刷新后再跑
        setTimeout(() => runBatchBenchmark(merged), 0);
      } else if (!editingId && merged.length > 0) {
        // 新增供应商手动拉取成功：默认勾选全部模型（编辑已有供应商时保持原勾选，不替用户改）
        setSelectedIds(merged.map((m) => m.id));
      }
      return merged;
    } catch (e) {
      flash("err", `拉取失败: ${e}`);
    } finally {
      setLoadingModels(false);
    }
  };

  // 评测单个模型；对已加入的模型，测完自动把最新速度回写保存
  const benchmarkOne = async (model: CatalogModel) => {
    const creds = resolveBenchCreds();
    if (!creds.ok) {
      flash("err", creds.error);
      return;
    }
    setBenchmarking((p) => ({ ...p, [model.id]: true }));
    try {
      const res = creds.useID
        ? await ProviderAPIService.BenchmarkModelByID(creds.id, model.id, "", creds.baseURL, creds.protocol, 256)
        : await ProviderAPIService.BenchmarkModel(creds.baseURL, creds.apiKey, model.id, "", creds.protocol, 256);
      setBenchResult((p) => ({ ...p, [model.id]: res ?? {} }));
      if (res?.success) {
        const pid = getEffectiveId();
        const alreadyAdded = providers[pid]?.models?.some(
          (x) => x.id === model.id && x.verified
        );
        if (alreadyAdded) {
          // 已加入模型：把新测速持久化（AddVerifiedModel 同 id 覆盖，保留 healthy/failCount）
          await ProviderAPIService.AddVerifiedModel(pid, {
            id: model.id,
            context: parseContext(model.context),
            verified: true,
            healthy: true,
            failCount: 0,
            tps: res.tps ?? 0,
          } as ProviderModel);
          await load();
        } else {
          // 测评通过且尚未加入 -> 自动加入（新增/编辑模式都生效；
          // 之前用 !editingId 判断导致编辑模式下重测的新模型永远不会被加入）
          const added = await persistVerifiedModel(model, res.tps ?? 0, pid);
          if (added) {
            await load();
            flash("ok", `模型 ${model.id} 测评通过，已自动加入`);
          }
        }
      }
    } catch (e) {
      setBenchResult((p) => ({ ...p, [model.id]: { success: false, errorMsg: String(e) } }));
    } finally {
      setBenchmarking((p) => ({ ...p, [model.id]: false }));
    }
  };

  // 对指定模型批量测评；并发数由后端根据模型数量自动决定
  const runBatchBenchmark = async (targets: CatalogModel[]) => {
    if (targets.length === 0) return;
    const creds = resolveBenchCreds();
    if (!creds.ok) {
      flash("err", creds.error);
      return;
    }
    setBatchRunning(true);
    setBatchProgress({ done: 0, total: targets.length });
    // 标记所有目标为测评中
    for (const m of targets) {
      setBenchmarking((p) => ({ ...p, [m.id]: true }));
    }
    batchOkRef.current = [];
    batchResRef.current = {};
    batchTargetsRef.current = targets;

    try {
      if (creds.useID) {
        await ProviderAPIService.BatchBenchmarkByID(
          creds.id,
          "",
          creds.baseURL,
          creds.protocol,
          256,
          targets.map((m) => m.id)
        );
      } else {
        await ProviderAPIService.BatchBenchmark(
          creds.baseURL,
          creds.apiKey,
          "",
          creds.protocol,
          256,
          targets.map((m) => m.id)
        );
      }
    } catch (e) {
      flash("err", `批量测评失败: ${e}`);
      setBatchRunning(false);
      for (const m of targets) {
        setBenchmarking((p) => ({ ...p, [m.id]: false }));
      }
    }
  };

  const benchmarkSelected = () => {
    const targets = candidateModels.filter((m) => selectedIds.includes(m.id));
    return runBatchBenchmark(targets);
  };

  // 单个勾选切换
  const toggleSelect = (id: string) => {
    setSelectedIds((ids) =>
      ids.includes(id) ? ids.filter((x) => x !== id) : [...ids, id]
    );
  };

  // 批量加入：把所有评测通过（benchResult.success）且尚未加入的模型一次性加入
  const batchAddPassed = async () => {
    const providerId = getEffectiveId();
    const existing0 = providerId ? providers[providerId] : undefined;
    if (!baseURL.trim() || (!apiKey.trim() && !existing0?.hasKey)) {
      flash("err", "请先填写 BaseURL 和 API Key");
      return;
    }
    const addedIds = new Set(
      (activeProvider?.models ?? []).filter((m) => m.verified).map((m) => m.id)
    );
    const passed = candidateModels.filter(
      (m) => benchResult[m.id]?.success && !addedIds.has(m.id)
    );
    if (passed.length === 0) {
      flash("err", "没有可加入的测评通过模型");
      return;
    }
    const catName = fromCatalog
      ? catalog?.providers.find((x) => x.id === selectedCatalog)?.name
      : undefined;
    try {
      const existing = providers[providerId];
      if (!existing) {
        const catProv = catalog?.providers.find((x) => x.id === selectedCatalog);
        const cfg: ProviderConfig = {
          id: providerId,
          name: catName || customName.trim() || providerId,
          baseURL: baseURL.trim(),
          apiKey: apiKey.trim(),
          getAPIKeyURL: catProv?.get_api_key_url ?? "",
          protocol: catProv?.protocol ?? "openai",
          maxContext: 0,
          custom: !fromCatalog,
          verified: false,
          imported: false,
          models: [],
          hasKey: false,
        };
        await ProviderAPIService.UpsertProvider(cfg);
      }
      // 逐个 AddVerifiedModel（后端更新 provider 级 verified 标记，同 id 覆盖）
      for (const m of passed) {
        const pm: ProviderModel = {
          id: m.id,
          context: parseContext(m.context),
          verified: true,
          healthy: true,
          failCount: 0,
          tps: benchResult[m.id]?.tps ?? 0,
        };
        await ProviderAPIService.AddVerifiedModel(providerId, pm);
      }
      flash("ok", `已批量加入 ${passed.length} 个模型`);
      setSelectedIds((ids) => ids.filter((id) => !passed.some((m) => m.id === id)));
      await load();
      // 新增供应商：批量加入即完成创建，退出编辑态回到列表；编辑已有供应商时留在表单继续操作
      if (!editingId) {
        cancelEdit();
      }
    } catch (e) {
      flash("err", `批量加入失败: ${e}`);
    }
  };

  // 持久化一个已通过测评的模型：供应商不存在则先按表单创建，再 AddVerifiedModel。
  // 静默执行（不弹提示、不刷新列表），由调用方统一 flash/load。返回是否真正加入。
  // forceProviderId 可选：批量操作时传入已捕获的 provider id，避免重复生成随机 id 导致多个供应商。
  const persistVerifiedModel = async (model: CatalogModel, tps: number, forceProviderId?: string): Promise<boolean> => {
    const providerId = forceProviderId || getEffectiveId();
    const existingForCheck = providers[providerId];
    if (!baseURL.trim() || (!apiKey.trim() && !existingForCheck?.hasKey)) {
      return false;
    }
    const pm: ProviderModel = {
      id: model.id,
      context: parseContext(model.context),
      verified: true,
      healthy: true,
      failCount: 0,
      tps: tps ?? 0,
    };
    try {
      // 供应商既不在前端 state（异步循环间不刷新）、也未在本会话创建过时才 UpsertProvider。
      // UpsertProvider 是整体覆盖且此处 cfg.models 为空，重复调用会冲掉前一个已加入的模型。
      if (!existingForCheck && !ensuredProvidersRef.current.has(providerId)) {
        const catProv = catalog?.providers.find((x) => x.id === selectedCatalog);
        const cfg: ProviderConfig = {
          id: providerId,
          name: catProv?.name || customName.trim() || providerId,
          baseURL: baseURL.trim(),
          apiKey: apiKey.trim(),
          getAPIKeyURL: catProv?.get_api_key_url ?? "",
          protocol: catProv?.protocol ?? "openai",
          maxContext: 0,
          custom: !fromCatalog,
          verified: false,
          imported: false,
          models: [],
          hasKey: false,
        };
        await ProviderAPIService.UpsertProvider(cfg);
        ensuredProvidersRef.current.add(providerId);
      }
      await ProviderAPIService.AddVerifiedModel(providerId, pm);
      setSelectedIds((ids) => ids.filter((x) => x !== model.id));
      return true;
    } catch {
      return false;
    }
  };

  // 评测通过 -> 加入供应商
  // 若供应商尚未保存，先按当前表单值 UpsertProvider，再加入模型（用户无需单独点「保存」）
  const addVerifiedModel = async (model: CatalogModel) => {
    const ok = await persistVerifiedModel(model, benchResult[model.id]?.tps ?? 0);
    if (!ok) {
      flash("err", "添加失败，请确认已填写 BaseURL 和 API Key");
      return;
    }
    flash("ok", `模型 ${model.id} 已评测通过并加入`);
    await load();
    // 停留在添加/编辑表单：批量测评时会逐个点「加入」，不应每加一个就退出
  };

  // 取消加入：把已加入的模型从供应商移除（保留候选列表里的评测结果，可重新加入）
  const removeModel = async (modelId: string) => {
    const providerId = getEffectiveId();
    if (!providers[providerId]) return;
    setRemovingId(modelId);
    try {
      await ProviderAPIService.RemoveModel(providerId, modelId);
      flash("ok", `已移除 ${modelId}`);
      setSelectedIds((ids) => ids.filter((x) => x !== modelId));
      await load();
    } catch (e) {
      flash("err", `移除失败: ${e}`);
    } finally {
      setRemovingId("");
    }
  };

  // 保存供应商（新建/编辑）
  // 编辑模式（editingId 非空）：用原 id，保留已验证模型和 verified 状态
  // 返回是否保存成功（切换确认时据此决定是否继续）
  const saveProvider = async (): Promise<boolean> => {
    const isEdit = !!editingId;
    const providerId = getEffectiveId();
    // 编辑时保留已通过的模型；若「加入」已自动落盘过供应商，也沿用其已加入模型
    const existing = providers[providerId];
    const catName = catalog?.providers.find((x) => x.id === selectedCatalog)?.name;
    // 导入的供应商不回显 Key：留空表示保留原 Key；其他情况用输入值
    const enteredKey = apiKey.trim();
    const isImported = !!existing?.imported;
    const finalApiKey = isEdit && isImported && enteredKey === ""
      ? (existing.apiKey ?? "")
      : enteredKey;
    // 用户给导入的供应商换了新 Key：该 Key 归用户所有，取消 imported 标记，下次可查看
    const keepImported = isImported && enteredKey === "";
    const catProvForProto = fromCatalog
      ? catalog?.providers.find((x) => x.id === selectedCatalog)
      : undefined;
    const cfg: ProviderConfig = {
      id: providerId,
      name: fromCatalog
        ? (catName ?? providerId)
        : (customName.trim() || providerId),
      baseURL: baseURL.trim(),
      apiKey: finalApiKey,
      getAPIKeyURL: fromCatalog
        ? (catName ? catalog?.providers.find((x) => x.id === selectedCatalog)?.get_api_key_url ?? "" : (existing?.getAPIKeyURL ?? ""))
        : (existing?.getAPIKeyURL ?? ""),
      protocol: existing?.protocol ?? catProvForProto?.protocol ?? "openai",
      maxContext: existing?.maxContext ?? 0,
      custom: isEdit ? (existing?.custom ?? true) : !fromCatalog,
      imported: keepImported,
      verified: existing?.verified ?? false,
      models: existing?.models ?? [],
      hasKey: false,
    };
    try {
      await ProviderAPIService.UpsertProvider(cfg);
      flash("ok", isEdit ? `供应商「${cfg.name}」已更新` : `供应商「${cfg.name}」已保存`);
      cancelEdit();
      await load();
      return true;
    } catch (e) {
      flash("err", `保存失败: ${e}`);
      return false;
    }
  };

  // 删除供应商
  const removeProvider = async (id: string) => {
    try {
      await ProviderAPIService.RemoveProvider(id);
      flash("ok", "已删除");
      await load();
    } catch (e) {
      flash("err", `删除失败: ${e}`);
    }
  };

  // 刷新目录（从 GitHub 拉最新）
  const refreshCatalog = async () => {
    setRefreshing(true);
    try {
      const [cat, changed] = await ProviderAPIService.RefreshCatalog();
      if (cat) setCatalog(cat);
      flash("ok", changed ? "目录已更新" : "目录已是最新");
    } catch (e) {
      flash("err", `刷新失败: ${e}`);
    } finally {
      setRefreshing(false);
    }
  };

  // 用系统浏览器打开外部链接（Wails WebView 里 window.open 不可用）
  const openGetKey = (url: string) => {
    if (!url) return;
    ProviderAPIService.OpenURL(url).catch(() => {});
  };

  // 打开注册送 token 供应商弹窗
  const openBonusModal = async (inline = false) => {
    setShowBonus(true);
    setBonusInline(inline);
    setBonusLoading(true);
    setSelectedBonus("");
    setBonusPage(0);
    try {
      const list = await ProviderAPIService.GetBonusProviders();
      setBonusProviders(list ?? []);
      setHasBonus((list ?? []).length > 0);
    } catch {
      setBonusProviders([]);
    } finally {
      setBonusLoading(false);
    }
  };

  // inline 模式：点击供应商行直接填充并关闭
  const selectBonusInline = (p: { id: string; name: string; baseUrl: string }) => {
    setFromCatalog(false);
    setCustomName(p.name);
    setBaseURL(p.baseUrl);
    setShowBonus(false);
    if (!showAdd) startNew();
  };

  // 确认选择 bonus 供应商（表单内按钮，带确认/取消）：填充到自定义表单
  const confirmBonus = () => {
    const p = bonusProviders.find((x) => x.id === selectedBonus);
    if (!p) return;
    setFromCatalog(false);
    setCustomName(p.name);
    setBaseURL(p.baseUrl);
    setShowBonus(false);
  };

  // 进入编辑/新增时把主滚动容器平滑滚到顶端（自定义缓动，速度较慢）
  const scrollFormIntoView = () => {
    requestAnimationFrame(() => {
      const el = document.querySelector("main");
      const start = el ? el.scrollTop : window.scrollY;
      if (start <= 0) return;
      const duration = 500; // ms，越大越慢
      const t0 = performance.now();
      const easeOutCubic = (t: number) => 1 - Math.pow(1 - t, 3);
      const step = (now: number) => {
        const t = Math.min((now - t0) / duration, 1);
        const y = start * (1 - easeOutCubic(t));
        if (el) el.scrollTop = y;
        else window.scrollTo(0, y);
        if (t < 1) requestAnimationFrame(step);
      };
      requestAnimationFrame(step);
    });
  };

  // 开始新增供应商
  const startNew = () => {
    ensuredProvidersRef.current = new Set();
    setShowAdd(true);
    setEditingId("");
    setEffectiveId("");
    setFromCatalog(true);
    setSelectedCatalog("");
    setCustomName("");
    setCustomId("");
    setBaseURL("");
    setApiKey("");
    setShowApiKey(false);
    setEditingImported(false);
    setCandidateModels([]);
    setBenchResult({});
    setSelectedIds([]);
    setModelSearch("");
    setOnlyPassed(false);
    setBatchProgress({ done: 0, total: 0 });
    formBaseline.current = { name: "", baseURL: "", apiKey: "" };
    scrollFormIntoView();
  };

  // 开始编辑已添加的供应商
  const startEdit = (id: string) => {
    const p = providers[id];
    if (!p) return;
    ensuredProvidersRef.current = new Set();
    setEditingId(id);
    setEffectiveId(id);
    setShowAdd(true);
    setShowApiKey(false);
    setEditingImported(!!p.imported);
    // 分享导入的供应商：不回显原 Key，留空表示不修改；非导入的正常回显
    setApiKey(p.imported ? "" : p.apiKey);
    setBaseURL(p.baseURL);
    setSelectedIds([]);
    setModelSearch("");
    setOnlyPassed(false);
    setBatchProgress({ done: 0, total: 0 });
    // 用已保存模型的最近一次测速结果回填，让编辑时显示 "✓ N tok/s"
    const savedBench: Record<string, any> = {};
    for (const m of p.models ?? []) {
      if (m.verified) {
        savedBench[m.id] = { success: true, tps: m.tps ?? 0 };
      }
    }
    setBenchResult(savedBench);
    // 自动识别来源：内置目录里能找到同 id 的供应商 -> 目录模式；否则自定义
    const cat = catalog?.providers.find((x) => x.id === id);
    if (cat) {
      setFromCatalog(true);
      setSelectedCatalog(id);
      setCustomName("");
      setCustomId(id);
      // 预填目录免费模型（已加入的会在列表里标记「✓ 已加入」）
      setCandidateModels(cat.free_models ?? []);
    } else {
      setFromCatalog(false);
      setSelectedCatalog("");
      setCustomName(p.name);
      setCustomId(p.id);
      // 自定义供应商：预填已加入（verified）模型
      setCandidateModels(
        (p.models ?? [])
          .filter((m) => m.verified)
          .map((m) => ({
            id: m.id,
            name: m.id,
            context: formatContext(m.context),
            rate_limit: "",
          }))
      );
    }
    // 记录进入编辑时的表单快照，用于脏检查
    formBaseline.current = {
      name: cat?.name ?? p.name,
      baseURL: p.baseURL,
      apiKey: p.apiKey,
    };
    scrollFormIntoView();
    // 进入编辑时，若开关开启且有 BaseURL/API Key，自动拉取模型并测评
    // 导入供应商的明文 key 已脱敏（p.apiKey 为空），用 hasKey 判断后端是否持有密钥
    if (autoBenchOnEdit && p.baseURL && (p.apiKey || p.hasKey)) {
      setTimeout(() => testAndFetch(true), 0);
    }
  };

  // 取消编辑/新增
  const cancelEdit = () => {
    ensuredProvidersRef.current = new Set();
    setEditingId("");
    setEffectiveId("");
    setShowAdd(false);
    setApiKey("");
    setShowApiKey(false);
    setEditingImported(false);
    setBaseURL("");
    setCustomName("");
    setCustomId("");
    setCandidateModels([]);
    setSelectedCatalog("");
    setBenchResult({});
    setSelectedIds([]);
    setModelSearch("");
    setOnlyPassed(false);
    setBatchRunning(false);
    setBatchProgress({ done: 0, total: 0 });
    formBaseline.current = null;
  };

  const providerList = Object.entries(providers ?? {}).filter(([, p]) => p !== null) as [string, ProviderConfig][];
  // 当前表单对应的供应商（用于标记候选模型是否已加入）。
  // 不能只看 selectedCatalog：编辑/自定义模式下它为空，会导致已选模型永远检测不到。
  const activeProviderId = editingId || selectedCatalog || customId.trim() || effectiveId;
  const activeProvider = providers[activeProviderId] ?? null;

  // 搜索过滤（模糊匹配 id/name，大小写不敏感）+ 可选「只看测评通过」
  const q = modelSearch.trim().toLowerCase();
  const filteredModels = candidateModels.filter((m) => {
    if (onlyPassed && !benchResult[m.id]?.success) return false;
    if (q) {
      const hay = ((m.name || m.id) + " " + m.id).toLowerCase();
      if (!hay.includes(q)) return false;
    }
    return true;
  });
  // 当前搜索结果均可勾选（含已加入的模型，编辑时可重新勾选测速）
  const allFilteredSelected =
    filteredModels.length > 0 && filteredModels.every((m) => selectedIds.includes(m.id));
  const toggleSelectAllFiltered = () => {
    if (allFilteredSelected) {
      setSelectedIds((ids) => ids.filter((id) => !filteredModels.some((m) => m.id === id)));
    } else {
      setSelectedIds((ids) => {
        const next = new Set(ids);
        filteredModels.forEach((m) => next.add(m.id));
        return Array.from(next);
      });
    }
  };
  // 测评通过且尚未加入的模型数（驱动「加入通过」按钮显示与计数）
  const addedIds = new Set((activeProvider?.models ?? []).filter((m) => m.verified).map((m) => m.id));
  const passedNotAdded = candidateModels.filter(
    (m) => benchResult[m.id]?.success && !addedIds.has(m.id)
  ).length;

  // 当前表单的可保存字段（name/baseURL/apiKey），用于与进入时的快照做脏检查
  const currentFormSnapshot = (): { name: string; baseURL: string; apiKey: string } => {
    const catName = catalog?.providers.find((x) => x.id === selectedCatalog)?.name;
    const name = fromCatalog
      ? (catName ?? selectedCatalog)
      : (customName.trim() || customId.trim() || editingId || "");
    return { name, baseURL: baseURL.trim(), apiKey: apiKey.trim() };
  };
  // 是否有未保存变化（模型加入/移除即时落盘，不纳入；只看表单字段）
  const isFormDirty = (): boolean => {
    if (!formBaseline.current) return false;
    const cur = currentFormSnapshot();
    const b = formBaseline.current;
    return cur.name !== b.name || cur.baseURL !== b.baseURL || cur.apiKey !== b.apiKey;
  };
  // 请求切换：有未保存变化则弹确认，否则直接执行目标动作
  const requestSwitch = (action: () => void) => {
    if (isFormDirty()) setPendingSwitch(() => action);
    else action();
  };
  // 保存当前表单后再执行切换动作
  const confirmSaveAndSwitch = async () => {
    setSavingBeforeSwitch(true);
    const ok = await saveProvider();
    setSavingBeforeSwitch(false);
    if (!ok) return; // 保存失败：留在当前编辑态，不切换
    // saveProvider 成功后已 cancelEdit + load；执行待切换动作
    const action = pendingSwitch;
    setPendingSwitch(null);
    action?.();
  };
  const discardAndSwitch = () => {
    cancelEdit();
    const action = pendingSwitch;
    setPendingSwitch(null);
    action?.();
  };

  // 打开分享对话框：
  // - 顶部「分享/导入」按钮不传 id，进入「导出/导入」选择菜单
  // - 卡片「分享」传 id，跳过菜单直接进入导出态并预勾选该供应商
  const openShare = (initialId?: string) => {
    setShareInitialId(initialId);
    setShowShare(true);
  };

  if (locked) {
    return (
      <UnlockScreen
        autoLockout={autoLockout}
        onUnlocked={() => {
          setAutoLockout(false);
          checkLock();
          load();
        }}
      />
    );
  }

  return (
    <div className="p-6 space-y-6">
      <div className="flex items-center justify-between">
        <div>
          <div className="flex items-center gap-2">
            <h2 className="font-semibold">供应商配置</h2>
            {hasBonus && (
              <button
                onClick={() => openBonusModal(true)}
                title="注册送 Token 的供应商"
                className="px-1.5 py-0.5 text-[10px] leading-tight rounded bg-[var(--color-success)]/15 text-[var(--color-success)] hover:bg-[var(--color-success)]/25 cursor-pointer"
              >
                🎁 注册送Token
              </button>
            )}
          </div>
          <p className="text-xs text-[var(--color-text-dim)] mt-0.5">
            配置大模型 API 供应商，评测通过的模型与内置上游平级使用。
          </p>
        </div>
        <div className="flex gap-2">
          {hasMaster && (
            <button
              onClick={doLock}
              title="立即锁定（需主密码解锁）"
              className="px-3 py-1.5 text-sm rounded-lg bg-[var(--color-surface-2)] hover:bg-[var(--color-border)]"
            >
              🔒
            </button>
          )}
          <button
            onClick={() => setShowSecurity(true)}
            title="安全设置：主密码、恢复码"
            className="px-3 py-1.5 text-sm rounded-lg bg-[var(--color-surface-2)] hover:bg-[var(--color-border)]"
          >
            🔐
          </button>
          <button
            onClick={refreshCatalog}
            disabled={refreshing}
            className="px-3 py-1.5 text-sm rounded-lg bg-[var(--color-surface-2)] hover:bg-[var(--color-border)] disabled:opacity-50"
          >
            {refreshing ? "刷新中..." : "🔄 刷新目录"}
          </button>
          <button
            onClick={() => {
              if (showAdd) {
                requestSwitch(() => {
                  cancelEdit();
                  openShare();
                });
              } else {
                openShare();
              }
            }}
            className="px-3 py-1.5 text-sm rounded-lg bg-[var(--color-surface-2)] hover:bg-[var(--color-border)]"
          >
            🔗 分享/导入
          </button>
          <button
            onClick={() => {
              if (showAdd) {
                requestSwitch(() => cancelEdit());
              } else {
                startNew();
              }
            }}
            className="px-3 py-1.5 text-sm rounded-lg bg-[var(--color-primary)] hover:opacity-90"
          >
            {showAdd ? (editingId ? "取消编辑" : "取消添加") : "＋ 添加供应商"}
          </button>
        </div>
      </div>

      {msg && (
        <div className={`px-4 py-2 rounded-lg text-sm ${msg.type === "ok" ? "bg-[var(--color-success)]/20 text-[var(--color-success)]" : "bg-[var(--color-danger)]/20 text-[var(--color-danger)]"}`}>
          {msg.text}
        </div>
      )}

      {/* 添加/编辑表单 */}
      {showAdd && (
        <section className="bg-[var(--color-surface)] rounded-xl p-5 border border-[var(--color-border)] space-y-3">
          {editingId && (
            <div className="text-sm font-medium text-[var(--color-primary)]">
              ✏️ 编辑供应商：{providers[editingId]?.name ?? editingId}
            </div>
          )}
          {/* 来源切换 tab（与新增一致；编辑时显示但不可切换，避免改供应商身份） */}
          <div className="flex gap-4 text-sm">
            <label className={`flex items-center gap-1.5 ${editingId ? "opacity-60" : "cursor-pointer"}`}>
              <input
                type="radio"
                checked={fromCatalog}
                disabled={!!editingId}
                onChange={() => setFromCatalog(true)}
                className="accent-[var(--color-primary)] disabled:cursor-not-allowed"
              />
              从内置目录选
            </label>
            <label className={`flex items-center gap-1.5 ${editingId ? "opacity-60" : "cursor-pointer"}`}>
              <input
                type="radio"
                checked={!fromCatalog}
                disabled={!!editingId}
                onChange={() => setFromCatalog(false)}
                className="accent-[var(--color-primary)] disabled:cursor-not-allowed"
              />
              自定义添加
            </label>
          </div>

          {fromCatalog ? (
            <div>
              <label className="text-xs text-[var(--color-text-dim)] block mb-1">选择供应商（可搜索名称或模型）</label>
              <ProviderPicker
                providers={catalog?.providers ?? []}
                value={selectedCatalog}
                onChange={onSelectCatalog}
                existingIds={Object.keys(providers ?? {})}
                placeholder="搜索或选择供应商..."
                disabled={!!editingId}
              />
              {selectedCatalog && (
                <div className="mt-2 flex flex-wrap items-center gap-2 text-xs">
                  {(() => {
                    const p = catalog?.providers.find((x) => x.id === selectedCatalog);
                    if (!p) return null;
                    return (
                      <>
                        <span className="px-2 py-0.5 rounded bg-[var(--color-surface-2)] text-[var(--color-text-dim)]">
                          {p.free_models_count ?? 0} 免费模型
                        </span>
                        {p.get_api_key_url && (
                          <button
                            onClick={() => openGetKey(p.get_api_key_url!)}
                            className="px-2 py-0.5 rounded bg-[var(--color-primary)]/15 text-[var(--color-primary)] hover:opacity-90 cursor-pointer"
                          >
                            获取 API Key ↗
                          </button>
                        )}
                      </>
                    );
                  })()}
                </div>
              )}
            </div>
          ) : (
            <div className="grid grid-cols-2 gap-3">
              <input
                value={customName}
                onChange={(e) => setCustomName(e.target.value)}
                placeholder="供应商名称（如 My API）"
                className="px-2 py-1.5 text-sm rounded-md bg-[var(--color-surface-2)] border border-[var(--color-border)]"
              />
              <input
                value={customId}
                onChange={(e) => setCustomId(e.target.value)}
                disabled={!!editingId}
                title={editingId ? "id 不可修改" : ""}
                placeholder="id（留空自动生成）"
                className="px-2 py-1.5 text-sm rounded-md bg-[var(--color-surface-2)] border border-[var(--color-border)] disabled:opacity-50 disabled:cursor-not-allowed"
              />
            </div>
          )}

          {!editingId && hasBonus && (
            <button
              onClick={() => { setFromCatalog(false); openBonusModal(false); }}
              className="self-start text-xs px-2 py-1 rounded-md bg-[var(--color-success)]/15 text-[var(--color-success)] hover:bg-[var(--color-success)]/25"
            >
              🎁 注册送 Token 供应商
            </button>
          )}

          <div className="grid grid-cols-1 gap-3">
            <input
              value={baseURL}
              onChange={(e) => setBaseURL(e.target.value)}
              placeholder="BaseURL（如 https://api.groq.com/openai/v1）"
              className="w-full px-2 py-1.5 text-sm rounded-md bg-[var(--color-surface-2)] border border-[var(--color-border)]"
            />
            <div className="flex gap-2">
              <div className="relative flex-1">
                <input
                  value={apiKey}
                  onChange={(e) => setApiKey(e.target.value)}
                  type={showApiKey ? "text" : "password"}
                  placeholder={
                    editingImported
                      ? "已保存（留空不修改）"
                      : "API Key"
                  }
                  autoComplete="off"
                  className={`w-full px-2 py-1.5 pr-9 text-sm rounded-md bg-[var(--color-surface-2)] border border-[var(--color-border)] ${
                    editingImported ? "opacity-80" : ""
                  }`}
                />
                {!editingImported && apiKey && (
                  <button
                    type="button"
                    onClick={() => setShowApiKey((v) => !v)}
                    title={showApiKey ? "隐藏" : "显示"}
                    className="absolute right-2 top-1/2 -translate-y-1/2 text-[var(--color-text-dim)] hover:text-[var(--color-text)] text-xs leading-none"
                  >
                    {showApiKey ? "🙈" : "👁"}
                  </button>
                )}
              </div>
              <button
                onClick={() => testAndFetch()}
                disabled={loadingModels}
                className="px-3 py-1.5 text-sm rounded-lg bg-[var(--color-surface-2)] hover:bg-[var(--color-border)] disabled:opacity-50 whitespace-nowrap"
              >
                {loadingModels ? "拉取中..." : "⬇️ 拉取模型"}
              </button>
            </div>
          </div>

          {/* 候选模型列表（全选 + 搜索[内嵌通过过滤] + 批量按钮固定最右，一行不晃动） */}
          {candidateModels.length > 0 && (
            <div>
              <div className="flex items-center gap-3 mb-2">
                <label className="flex items-center gap-2 text-sm text-[var(--color-text-dim)] cursor-pointer select-none shrink-0">
                  <input
                    type="checkbox"
                    checked={allFilteredSelected}
                    onChange={toggleSelectAllFiltered}
                    className="w-4 h-4 accent-[var(--color-primary)] cursor-pointer"
                  />
                  {/* 文字块固定宽度：吸收「全选↔取消全选」「N ↔ N/M」长度变化，搜索框不晃 */}
                  <span className="inline-flex items-center w-32 shrink-0">
                    {allFilteredSelected ? "取消全选" : "全选"}
                    <span className={`ml-1.5 ${selectedIds.length > 0 ? "text-[var(--color-primary)]" : ""}`}>
                      {selectedIds.length > 0
                        ? `${selectedIds.length}/${candidateModels.length}`
                        : `${candidateModels.length}`}
                    </span>
                  </span>
                </label>
                {/* 搜索框：缩小 + 右侧内嵌「只看通过」过滤复选框 */}
                <div className="relative flex-1 max-w-xs">
                  <input
                    value={modelSearch}
                    onChange={(e) => setModelSearch(e.target.value)}
                    placeholder="搜索模型..."
                    className="w-full pl-9 pr-16 py-2 text-sm rounded-lg bg-[var(--color-surface-2)] border border-[var(--color-border)] focus:outline-none focus:border-[var(--color-primary)]"
                  />
                  <span className="absolute left-3 top-1/2 -translate-y-1/2 text-[var(--color-text-dim)] text-sm">🔍</span>
                  <label
                    title="只显示测评通过的模型"
                    className="absolute right-2 top-1/2 -translate-y-1/2 flex items-center gap-1 text-[11px] text-[var(--color-text-dim)] cursor-pointer select-none px-1 rounded hover:bg-[var(--color-border)]/40"
                  >
                    <input
                      type="checkbox"
                      checked={onlyPassed}
                      onChange={(e) => setOnlyPassed(e.target.checked)}
                      className="w-3.5 h-3.5 accent-[var(--color-primary)] cursor-pointer"
                    />
                    通过
                  </label>
                </div>
                {/* 右侧固定宽度按钮区：批量加入通过 + 批量测评，ml-auto 顶到行尾；出现/消失不推挤搜索框 */}
                <div className="w-[17.5rem] shrink-0 ml-auto flex items-center justify-end gap-2">
                  {passedNotAdded > 0 && (
                    <button
                      onClick={batchAddPassed}
                      disabled={batchRunning || loadingModels}
                      className="px-3 py-2 text-sm rounded-lg bg-[var(--color-success)]/15 text-[var(--color-success)] hover:bg-[var(--color-success)]/25 disabled:opacity-50 whitespace-nowrap"
                    >
                      ✓ 加入通过 ({passedNotAdded})
                    </button>
                  )}
                  {selectedIds.length > 0 && (
                    <button
                      onClick={benchmarkSelected}
                      disabled={batchRunning || loadingModels}
                      className="px-3 py-2 text-sm rounded-lg bg-[var(--color-primary)] hover:opacity-90 disabled:opacity-50 whitespace-nowrap"
                    >
                      {batchRunning
                        ? `测评 ${batchProgress.done}/${batchProgress.total}`
                        : `⚡ 批量测评 (${selectedIds.length})`}
                    </button>
                  )}
                </div>
              </div>
              {batchRunning && (
                <div className="h-1.5 mb-2 rounded-full bg-[var(--color-surface-2)] overflow-hidden">
                  <div
                    className="h-full bg-[var(--color-primary)] transition-all"
                    style={{
                      width: `${batchProgress.total ? (batchProgress.done / batchProgress.total) * 100 : 0}%`,
                    }}
                  />
                </div>
              )}
              <div className="space-y-2 max-h-72 overflow-y-auto">
                {filteredModels.length === 0 ? (
                  <div className="text-xs text-[var(--color-text-dim)] py-4 text-center">
                    没有匹配「{modelSearch}」的模型
                  </div>
                ) : (
                  filteredModels.map((m) => {
                    const res = benchResult[m.id];
                    const isAdded = !!activeProvider?.models?.some(
                      (x) => x.id === m.id && x.verified
                    );
                    const checked = selectedIds.includes(m.id);
                    return (
                      <div key={m.id} className="flex items-center gap-3 bg-[var(--color-bg)] rounded-lg p-2.5 border border-[var(--color-border)]">
                        {/* 复选框：已加入的模型也可勾选，用于编辑时批量重新测速 */}
                        <input
                          type="checkbox"
                          checked={checked}
                          onChange={() => toggleSelect(m.id)}
                          className="w-4 h-4 accent-[var(--color-primary)] shrink-0 cursor-pointer"
                          title={isAdded ? "已加入，可勾选重新测速" : ""}
                        />
                        <div className="flex-1 min-w-0">
                          <div className="text-sm truncate flex items-center gap-1.5">
                            {m.name || m.id}
                            {inferVision(m.id) && <span className="text-[10px] px-1 rounded bg-blue-500/20 text-blue-400 shrink-0">视觉</span>}
                            {inferReasoning(m.id) && <span className="text-[10px] px-1 rounded bg-purple-500/20 text-purple-400 shrink-0">推理</span>}
                            {inferTool(m.id) && <span className="text-[10px] px-1 rounded bg-emerald-500/20 text-emerald-400 shrink-0">工具</span>}
                          </div>
                          <div className="text-[11px] text-[var(--color-text-dim)] truncate">
                            {m.context && <span>上下文 {m.context}</span>}
                            {m.rate_limit && <span className="ml-2">限流：{m.rate_limit}</span>}
                          </div>
                        </div>
                        {res?.success ? (
                          <span className="text-xs text-[var(--color-success)] whitespace-nowrap">
                            {res.tps > 0 ? `✓ ${res.tps.toFixed(1)} tok/s` : "✓ 已通过"}
                          </span>
                        ) : res && !res.success ? (
                          <span className="text-xs text-[var(--color-danger)] whitespace-nowrap">✗ 失败</span>
                        ) : null}
                        {isAdded ? (
                          <>
                            <span className="text-[10px] px-1.5 py-0.5 rounded bg-[var(--color-success)]/20 text-[var(--color-success)] whitespace-nowrap">
                              ✓ 已加入
                            </span>
                            {/* 取消加入：红色 − 按钮，从供应商移除该模型 */}
                            <button
                              onClick={() => removeModel(m.id)}
                              disabled={removingId === m.id || batchRunning}
                              title="取消加入"
                              className="w-6 h-6 flex items-center justify-center text-xs rounded-md bg-[var(--color-danger)]/15 text-[var(--color-danger)] hover:bg-[var(--color-danger)]/30 disabled:opacity-50 shrink-0 leading-none"
                            >
                              {removingId === m.id ? "…" : "−"}
                            </button>
                          </>
                        ) : (
                          <>
                            <button
                              onClick={() => benchmarkOne(m)}
                              disabled={benchmarking[m.id]}
                              className="px-2 py-1 text-xs rounded-md bg-[var(--color-surface-2)] hover:bg-[var(--color-border)] disabled:opacity-50 whitespace-nowrap"
                            >
                              {benchmarking[m.id] ? "评测中..." : res ? "重新评测" : "评测"}
                            </button>
                            {res?.success && (
                              <button
                                onClick={() => addVerifiedModel(m)}
                                className="px-2 py-1 text-xs rounded-md bg-[var(--color-primary)] hover:opacity-90 whitespace-nowrap"
                              >
                                ✓ 加入
                              </button>
                            )}
                          </>
                        )}
                      </div>
                    );
                  })
                )}
              </div>
            </div>
          )}

          {/* 保存供应商（编辑模式 / 未保存时显示；已从目录选且已保存则隐藏，因为可通过编辑改） */}
          {(editingId || !activeProvider) && (
            <button
              onClick={saveProvider}
              disabled={
                !baseURL.trim() ||
                (!editingId && !apiKey.trim()) || // 新建必须填写 Key
                (!!editingId && !apiKey.trim() && !activeProvider?.hasKey) // 编辑：留空时后端须已有保存的 Key
              }
              className="px-4 py-1.5 text-sm rounded-lg bg-[var(--color-surface-2)] hover:bg-[var(--color-border)] disabled:opacity-50"
            >
              {editingId ? "💾 保存修改" : "💾 保存供应商"}
            </button>
          )}
        </section>
      )}

      {/* 已添加供应商列表 */}
      {providerList.length === 0 && !showAdd ? (
        <div className="text-sm text-[var(--color-text-dim)] py-8 text-center bg-[var(--color-surface)] rounded-xl border border-[var(--color-border)]">
          尚未添加供应商。点击「＋ 添加供应商」开始。
        </div>
      ) : (
        <div className="space-y-3">
          {providerList.map(([id, p]) => {
            const verified = p.models?.filter((m) => m.verified) ?? [];
            const unhealthy = verified.filter((m) => !m.healthy);
            return (
              <div
                key={id}
                className={`group bg-[var(--color-surface)] rounded-xl p-4 border transition-colors cursor-pointer ${
                  editingId === id
                    ? "border-[var(--color-success)] ring-2 ring-[var(--color-success)]/40"
                    : "border-[var(--color-border)] hover:border-[var(--color-success)]"
                }`}
                onClick={() => {
                  if (editingId === id) return;
                  requestSwitch(() => startEdit(id));
                }}
              >
                <div className="flex items-center gap-2">
                  <span className="text-sm font-medium">{p.name}</span>
                  {p.imported && (
                    <span className="text-[10px] px-1.5 py-0.5 rounded bg-[var(--color-primary)]/15 text-[var(--color-primary)]" title="来自分享文件导入">
                      导入
                    </span>
                  )}
                  {p.custom && <span className="text-[10px] px-1.5 py-0.5 rounded bg-[var(--color-surface-2)] text-[var(--color-text-dim)]">自定义</span>}
                  <span className={`text-[10px] px-1.5 py-0.5 rounded ${p.verified ? "bg-[var(--color-success)]/20 text-[var(--color-success)]" : "bg-[var(--color-surface-2)] text-[var(--color-text-dim)]"}`}>
                    {p.verified ? `✓ ${verified.length} 模型` : "未评测"}
                  </span>
                  {unhealthy.length > 0 && (
                    <span className="text-[10px] px-1.5 py-0.5 rounded bg-[var(--color-danger)]/20 text-[var(--color-danger)]">
                      ⚠ {unhealthy.length} 不健康
                    </span>
                  )}
                  <div className="ml-auto flex gap-2" onClick={(e) => e.stopPropagation()}>
                    <button
                      onClick={() => openShare(id)}
                      title="分享此供应商"
                      className="px-2 py-1 text-xs rounded-md bg-[var(--color-surface-2)] hover:bg-[var(--color-border)]"
                    >
                      🔗 分享
                    </button>
                    <button
                      onClick={() => {
                        if (editingId === id) return; // 已在编辑该供应商，无操作
                        requestSwitch(() => startEdit(id));
                      }}
                      className="px-2 py-1 text-xs rounded-md bg-[var(--color-surface-2)] hover:bg-[var(--color-border)]"
                    >
                      编辑
                    </button>
                    <ConfirmPopover
                      title={`删除供应商「${p.name}」？已加入模型会一并移除`}
                      confirmLabel="删除"
                      onConfirm={() => removeProvider(id)}
                      triggerClassName="px-2 py-1 text-xs rounded-md bg-[var(--color-danger)]/15 text-[var(--color-danger)] hover:bg-[var(--color-danger)]/25"
                    >
                      删除
                    </ConfirmPopover>
                  </div>
                </div>
                <div className="text-[11px] text-[var(--color-text-dim)] mt-1 truncate">{p.baseURL}</div>
                {/* 已通过模型 */}
                {verified.length > 0 && (
                  <div className="mt-2 flex flex-wrap gap-1.5">
                    {verified.map((m) => (
                      <span
                        key={m.id}
                        className={`text-[11px] px-2 py-0.5 rounded-full border ${
                          m.healthy
                            ? "bg-[var(--color-bg)] border-[var(--color-border)] text-[var(--color-text)]"
                            : "bg-[var(--color-danger)]/10 border-[var(--color-danger)]/40 text-[var(--color-danger)]"
                        }`}
                        title={m.healthy ? "健康" : "健康监控异常（权重降级）"}
                      >
                        {m.id}
                        {m.healthy ? "●" : "○"}
                      </span>
                    ))}
                  </div>
                )}
              </div>
            );
          })}
        </div>
      )}

      {/* 切换供应商时的未保存变化确认弹窗 */}
      {pendingSwitch && (
        <div
          className="fixed inset-0 z-[300] flex items-center justify-center bg-black/50 p-4"
          onClick={() => setPendingSwitch(null)}
        >
          <div
            className="w-full max-w-sm rounded-xl border border-[var(--color-border)] bg-[var(--color-surface)] p-5 shadow-2xl"
            onClick={(e) => e.stopPropagation()}
          >
            <div className="text-sm font-medium mb-1.5">有未保存的修改</div>
            <p className="text-xs text-[var(--color-text-dim)] mb-4 leading-relaxed">
              当前供应商的配置已修改，切换会丢失这些改动。要先保存再切换，还是放弃修改？
            </p>
            <div className="flex justify-end gap-2">
              <button
                onClick={() => setPendingSwitch(null)}
                className="px-3 py-1.5 text-sm rounded-lg bg-[var(--color-surface-2)] hover:bg-[var(--color-border)]"
              >
                取消
              </button>
              <button
                onClick={discardAndSwitch}
                className="px-3 py-1.5 text-sm rounded-lg bg-[var(--color-surface-2)] text-[var(--color-danger)] hover:bg-[var(--color-danger)]/15"
              >
                放弃
              </button>
              <button
                onClick={confirmSaveAndSwitch}
                disabled={savingBeforeSwitch}
                className="px-3 py-1.5 text-sm rounded-lg bg-[var(--color-primary)] hover:opacity-90 disabled:opacity-50"
              >
                {savingBeforeSwitch ? "保存中..." : "保存并切换"}
              </button>
            </div>
          </div>
        </div>
      )}

      {showShare && (
        <ShareDialog
          providers={providers}
          initialIds={shareInitialId ? [shareInitialId] : undefined}
          onClose={() => {
            setShowShare(false);
            setShareInitialId(undefined);
          }}
          onImported={load}
        />
      )}

      {showSecurity && (
        <SecuritySettings
          onClose={() => {
            setShowSecurity(false);
            checkLock();
            load();
          }}
          onChanged={() => {
            checkLock();
            load();
          }}
        />
      )}

      {/* 注册送 Token 供应商弹窗 */}
      {showBonus && (
        <BonusModal
          providers={bonusProviders}
          loading={bonusLoading}
          inline={bonusInline}
          addedBaseUrls={new Set(providerList.map(([, p]) => p.baseURL.replace(/\/$/, "")))}
          selectedId={selectedBonus}
          page={bonusPage}
          onSelect={setSelectedBonus}
          onPage={setBonusPage}
          onOpenRegister={openGetKey}
          onPick={selectBonusInline}
          onCancel={() => setShowBonus(false)}
          onConfirm={confirmBonus}
        />
      )}
    </div>
  );
}

// BonusModal 注册送 token 供应商选择弹窗
function BonusModal({
  providers,
  loading,
  inline,
  addedBaseUrls,
  selectedId,
  page,
  onSelect,
  onPage,
  onOpenRegister,
  onPick,
  onCancel,
  onConfirm,
}: {
  providers: Array<{ id: string; name: string; baseUrl: string; registerUrl: string; bonusRule: string }>;
  loading: boolean;
  inline: boolean;
  addedBaseUrls: Set<string>;
  selectedId: string;
  page: number;
  onSelect: (id: string) => void;
  onPage: (p: number) => void;
  onOpenRegister: (url: string) => void;
  onPick: (p: { id: string; name: string; baseUrl: string }) => void;
  onCancel: () => void;
  onConfirm: () => void;
}) {
  const PAGE_SIZE = 10;
  const totalPages = Math.max(1, Math.ceil(providers.length / PAGE_SIZE));
  const start = page * PAGE_SIZE;
  const pageItems = providers.slice(start, start + PAGE_SIZE);

  return (
    <div className="fixed inset-0 z-[300] flex items-center justify-center bg-black/50 p-4" onClick={onCancel}>
      <div
        className="w-full max-w-lg rounded-xl border border-[var(--color-border)] bg-[var(--color-surface)] shadow-2xl flex flex-col max-h-[80vh]"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="p-4 border-b border-[var(--color-border)] flex items-center">
          <div className="flex-1">
            <h3 className="font-semibold">🎁 注册送 Token 供应商</h3>
            <p className="text-xs text-[var(--color-text-dim)] mt-1">
              {inline ? "点击供应商直接添加" : "选择供应商后将自动填充名称和 BaseURL"}
            </p>
          </div>
          <button
            onClick={onCancel}
            className="w-7 h-7 flex items-center justify-center rounded-md text-[var(--color-text-dim)] hover:bg-[var(--color-surface-2)] hover:text-[var(--color-text)] shrink-0"
          >
            ✕
          </button>
        </div>

        <div className="flex-1 overflow-y-auto p-2">
          {loading ? (
            <div className="text-center text-sm text-[var(--color-text-dim)] py-8">加载中...</div>
          ) : providers.length === 0 ? (
            <div className="text-center text-sm text-[var(--color-text-dim)] py-8">暂无供应商</div>
          ) : (
            <div className="space-y-1">
              {pageItems.map((p) => {
                const isAdded = addedBaseUrls.has(p.baseUrl.replace(/\/$/, ""));
                return (
                <div
                  key={p.id}
                  role={inline ? "button" : undefined}
                  onClick={() => {
                    if (inline) { if (!isAdded) onPick(p); }
                    else { onSelect(p.id); }
                  }}
                  className={`flex items-start gap-3 p-3 rounded-lg border ${
                    inline
                      ? isAdded
                        ? "cursor-default opacity-60 border-transparent"
                        : "cursor-pointer hover:bg-[var(--color-surface-2)] border-transparent"
                      : "cursor-pointer " + (
                        selectedId === p.id
                          ? "border-[var(--color-primary)] bg-[var(--color-primary)]/10"
                          : "border-transparent hover:bg-[var(--color-surface-2)]"
                      )
                  }`}
                >
                  {/* 左：选择控件（非 inline 为 radio；inline 为「+添加」按钮） */}
                  {!inline ? (
                    <input
                      type="radio"
                      name="bonus-provider"
                      checked={selectedId === p.id}
                      onChange={() => onSelect(p.id)}
                      onClick={(e) => e.stopPropagation()}
                      className="mt-1 accent-[var(--color-primary)] shrink-0"
                    />
                  ) : (
                    <button
                      type="button"
                      disabled={isAdded}
                      onClick={(e) => { e.stopPropagation(); if (!isAdded) onPick(p); }}
                      title={isAdded ? "已添加" : "添加该供应商"}
                      className={`mt-0.5 w-6 h-6 shrink-0 flex items-center justify-center rounded-full border text-sm leading-none
                        ${isAdded
                          ? "border-[var(--color-success)]/50 text-[var(--color-success)] cursor-default"
                          : "border-[var(--color-primary)] text-[var(--color-primary)] hover:bg-[var(--color-primary)]/10"}`}
                    >
                      {isAdded ? "✓" : "+"}
                    </button>
                  )}

                  {/* 中：名称 + bonus 说明 + BaseURL（点击 = 选择/添加） */}
                  <div className="flex-1 min-w-0">
                    <div className="flex items-center gap-2">
                      <span className="text-sm font-medium shrink-0">{p.name}</span>
                      {isAdded && (
                        <span className="text-[10px] px-1.5 py-0.5 rounded bg-[var(--color-success)]/20 text-[var(--color-success)] shrink-0">已添加</span>
                      )}
                    </div>
                    <div className="text-xs mt-1">{p.bonusRule}</div>
                    <div className="flex items-center gap-1.5 mt-1">
                      <span className="text-[10px] text-[var(--color-text-dim)] shrink-0">BaseURL:</span>
                      <span className="text-xs text-[var(--color-text-dim)] font-mono truncate">{p.baseUrl}</span>
                    </div>
                  </div>

                  {/* 右：显式「去注册」跳转按钮（与选择分离，stopPropagation 避免误触选择） */}
                  <button
                    type="button"
                    disabled={!p.registerUrl}
                    onClick={(e) => { e.stopPropagation(); onOpenRegister(p.registerUrl); }}
                    title={p.registerUrl ? "在浏览器中打开注册页" : "暂无注册链接"}
                    className="shrink-0 inline-flex items-center gap-1 px-2.5 py-1 text-xs rounded-md border border-[var(--color-primary)]/40 text-[var(--color-primary)] hover:bg-[var(--color-primary)]/10 disabled:opacity-40 disabled:cursor-not-allowed"
                  >
                    去注册 ↗
                  </button>
                </div>
                );
              })}
            </div>
          )}
        </div>

        {/* 分页 */}
        {providers.length > PAGE_SIZE && (
          <div className="flex items-center justify-center gap-3 p-3 border-t border-[var(--color-border)] text-sm">
            <button
              onClick={() => onPage(Math.max(0, page - 1))}
              disabled={page === 0}
              className="px-2 py-1 rounded bg-[var(--color-surface-2)] hover:bg-[var(--color-border)] disabled:opacity-30"
            >
              上一页
            </button>
            <span className="text-xs text-[var(--color-text-dim)]">{page + 1} / {totalPages}</span>
            <button
              onClick={() => onPage(Math.min(totalPages - 1, page + 1))}
              disabled={page >= totalPages - 1}
              className="px-2 py-1 rounded bg-[var(--color-surface-2)] hover:bg-[var(--color-border)] disabled:opacity-30"
            >
              下一页
            </button>
          </div>
        )}

        {!inline && (
          <div className="flex justify-end gap-2 p-4 border-t border-[var(--color-border)]">
            <button
              onClick={onCancel}
              className="px-4 py-1.5 text-sm rounded-lg bg-[var(--color-surface-2)] hover:bg-[var(--color-border)]"
            >
              取消
            </button>
            <button
              onClick={onConfirm}
              disabled={!selectedId}
              className="px-4 py-1.5 text-sm rounded-lg bg-[var(--color-primary)] hover:opacity-90 disabled:opacity-50"
            >
              确认
            </button>
          </div>
        )}
      </div>
    </div>
  );
}
