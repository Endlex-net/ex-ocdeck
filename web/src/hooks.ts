import { useEffect, useRef, useState, useSyncExternalStore } from 'react';
import { api, ApiError, UNAUTHORIZED_EVENT } from './api';
import { subscribeProjects } from './sse';
import type { Project } from './types';

/** 轮询：立即执行一次，之后每 intervalMs 执行。组件卸载自动清理。 */
export function usePoll(fn: () => void, intervalMs: number, deps: unknown[] = []): void {
  const fnRef = useRef(fn);
  fnRef.current = fn;
  useEffect(() => {
    fnRef.current();
    const id = setInterval(() => fnRef.current(), intervalMs);
    return () => clearInterval(id);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, deps);
}

/**
 * 订阅 matchMedia 查询，返回当前是否匹配；query 变化时重新订阅。
 * SSR 或无 matchMedia 环境（如 jsdom）返回 false。
 */
export function useMediaQuery(query: string): boolean {
  // 惰性初始化：首帧即对齐真实匹配状态，避免移动端先渲染桌面布局再闪动
  const [matches, setMatches] = useState<boolean>(() =>
    typeof window !== 'undefined' && typeof window.matchMedia === 'function'
      ? window.matchMedia(query).matches
      : false,
  );

  useEffect(() => {
    if (typeof window === 'undefined' || typeof window.matchMedia !== 'function') {
      return;
    }
    const mql = window.matchMedia(query);
    // 同步当前值（订阅回调只在变化时触发，初始进入页面若已匹配需立即对齐）
    setMatches(mql.matches);
    const handleChange = (e: MediaQueryListEvent) => setMatches(e.matches);
    mql.addEventListener('change', handleChange);
    return () => mql.removeEventListener('change', handleChange);
  }, [query]);

  return matches;
}

/* ============================ 主题系统 ============================ */

/** 主题偏好（design.md D5：与 index.html 内联脚本共用 localStorage['od-theme'] 与 data-theme）。 */
export type ThemePreference = 'system' | 'light' | 'dark';

export const THEME_KEY = 'od-theme';

export function isThemePreference(v: unknown): v is ThemePreference {
  return v === 'system' || v === 'light' || v === 'dark';
}

/** 解析主题偏好：localStorage['od-theme'] 缺省 system，非法值回退 system。
 *  纯函数，可独立测试（不直接读 localStorage，接收原始值）。 */
export function resolveThemePreference(raw: string | null | undefined): ThemePreference {
  return isThemePreference(raw) ? raw : 'system';
}

/** 根据偏好与系统深色标记推导有效主题（light|dark）。 */
export function resolveEffectiveTheme(pref: ThemePreference, systemDark: boolean): 'light' | 'dark' {
  if (pref === 'system') return systemDark ? 'dark' : 'light';
  return pref;
}

/** 写 <html data-theme> 与 <html data-theme-mode>（与 index.html 内联脚本共用同一 channel，
 *  避免脚本与 hook 状态漂移）。data-theme 为有效主题（light|dark），data-theme-mode 为偏好（system|light|dark）。 */
function applyTheme(pref: ThemePreference, effective: 'light' | 'dark'): void {
  document.documentElement.setAttribute('data-theme', effective);
  document.documentElement.setAttribute('data-theme-mode', pref);
}

/** 主题 hook：管理 system|light|dark，与 index.html 内联脚本共用 localStorage['od-theme'] 与 <html data-theme>。
 *  缺省 system 跟随系统偏好；首帧由 index.html 内联脚本已应用，hook 仅在挂载后接管交互。
 *
 *  跨组件状态共享：使用模块级共享偏好状态 + 订阅通知（而非每个实例本地 state），
 *  同页多个 useTheme() 消费者（App 侧栏切换、SettingsPage 偏好选择）保持一致——
 *  任一实例 setPreference 后全部实例的 preference 同步更新。 */
export function useTheme(): {
  preference: ThemePreference;
  setPreference: (p: ThemePreference) => void;
  effective: 'light' | 'dark';
} {
  const preference = useSyncExternalStore(themeSubscribe, getThemeSnapshot, getThemeSnapshot);

  // 系统深色偏好跟随（system 模式下随系统变化）
  const systemDark = useMediaQuery('(prefers-color-scheme: dark)');

  const effective = resolveEffectiveTheme(preference, systemDark);

  // 偏好或系统变化时同步 <html data-theme> 与 <html data-theme-mode>（与 index.html 脚本一致）。
  // 多消费者各跑此 effect，幂等写同一 DOM 属性，无副作用冲突。
  useEffect(() => {
    applyTheme(preference, effective);
  }, [preference, effective]);

  return { preference, setPreference: setThemePreference, effective };
}

/* ---- 主题模块级共享 store ----
 * preference 在模块级单例持有，setThemePreference 更新后通知所有订阅者。
 * 跨 tab 一致性由 storage 事件监听（下方 effect）同步进模块状态。 */
let themePrefState: ThemePreference = resolveThemePreference(safeReadTheme());
const themeListeners = new Set<() => void>();

function getThemeSnapshot(): ThemePreference {
  return themePrefState;
}

function themeSubscribe(cb: () => void): () => void {
  themeListeners.add(cb);
  return () => themeListeners.delete(cb);
}

function setThemePreference(p: ThemePreference): void {
  if (p === themePrefState) return;
  themePrefState = p;
  try {
    localStorage.setItem(THEME_KEY, p);
  } catch {
    /* localStorage 不可用时静默 */
  }
  themeListeners.forEach((l) => l());
}

// 跨 tab/window 主题变更监听：storage 事件同步进模块状态，通知本页订阅者
if (typeof window !== 'undefined') {
  window.addEventListener('storage', (e: StorageEvent) => {
    if (e.key === THEME_KEY) {
      const next = resolveThemePreference(e.newValue);
      if (next !== themePrefState) {
        themePrefState = next;
        themeListeners.forEach((l) => l());
      }
    }
  });
}

/* 测试导出：主题模块级共享 store 的可观测接口（跨消费者一致性契约测试）。
 *  主题变更 setThemePreference 后，getThemeSnapshot 反映新值且 themeSubscribe 通知已注册订阅者。 */
export const __themeStoreForTest = {
  getSnapshot: getThemeSnapshot,
  setPreference: setThemePreference,
  subscribe: themeSubscribe,
  /** 重置为指定偏好（不写 localStorage，仅重置模块状态 + 通知）。测试间隔离用。 */
  __reset(pref: ThemePreference = 'system') {
    themePrefState = pref;
    themeListeners.forEach((l) => l());
  },
};

function safeReadTheme(): string | null {
  try {
    return localStorage.getItem(THEME_KEY);
  } catch {
    return null;
  }
}

 /* ==================== App 层共享 projects store（SSE 订阅 + 兜底轮询） ====================
  * design.md D4 / projects-stream design D6：壳层侧栏、指挥中心与项目管理页同一数据源；
  * 应用内不得存在第二个 /projects 轮询（store 内部的兜底轮询不算）。
  * store 创建即订阅 /api/v1/projects/stream（帧整表替换快照）+ 30s 常驻兜底轮询——
  * 已知限制：项目 CRUD 不产生领域事件，流外变更（外部新建/删除项目）只能由兜底轮询
  * 覆盖（最长 ~30s 可见）；用户主动操作经 refresh() 立即收敛。
  * store 暴露 single-flight refresh()（trailing 语义）：变更操作成功后调用；
  * 若调用时已有轮询在途，在该请求结束后再补发一次——refresh() 承诺其结果反映调用之后的最新状态。
  * 流/轮询失败保留上次成功数据静默展示（侧栏不闪空态）；error 暴露给页面用于错误提示。 */

/** SSE 流回调句柄：store 拥有帧语义（整表替换/错误通道），工厂只决定传输。 */
export interface StreamHandlers {
  onData: (projects: Project[]) => void;
  onError: (message: string) => void;
}

/** store 依赖：loader / 兜底轮询 timer / SSE 流工厂，便于契约测试受控注入。 */
export interface StoreDeps {
  loader: () => Promise<Project[]>;
  /** 兜底轮询定时器工厂（默认 setInterval，生产 30s）；返回清理函数。 */
  startTimer?: (fire: () => void) => () => void;
  /** SSE 流订阅工厂（生产订阅 /api/v1/projects/stream，见 createProductionStore）。 */
  streamFactory?: (handlers: StreamHandlers) => { close(): void };
}

export interface StoreSnapshot {
  projects: Project[];
  /** 仅在「无数据且请求在途」时为 true（首次加载占位）；有数据后不再为 loading。 */
  loading: boolean;
  /** 首次加载是否已完成（区分初始空态与加载中）。 */
  initialized: boolean;
  /** 最近一次请求错误（成功时清除为空串）；页面用于错误提示，侧栏静默忽略。 */
  error: string;
}

interface ProjectsStore {
  /** 订阅 store 变更。 */
  subscribe: (listener: () => void) => () => void;
  /** 触发一次 single-flight 加载（轮询内部调用）。在途时直接返回在途 Promise、不设 trailing 标记。 */
  pollOnce: () => Promise<void>;
  /** trailing refresh()：变更操作成功后调用，承诺反映调用后的最新状态。
   *  在途时返回的 Promise 在 trailing 请求完成（成功 resolve / 失败 reject）后才 settle。 */
  refresh: () => Promise<void>;
  /** 释放定时器与 SSE 订阅等资源（测试隔离用）。 */
  dispose: () => void;
}

/** 共享 store 句柄：持有生产单例与其快照访问器（useSyncExternalStore 读快照）。 */
interface StoreHandle {
  store: ProjectsStore;
  getSnapshot: () => StoreSnapshot;
}

let storeHandle: StoreHandle | null = null;

/** 共享 projects store 单例（SSE 订阅 + 30s 兜底轮询 + single-flight + trailing refresh）。
 *  首次消费方访问创建 store：建立 /api/v1/projects/stream 订阅并启动兜底轮询；后续访问复用同一实例。
 *  401 生命周期：UNAUTHORIZED_EVENT（api.ts request / sse.ts 流在清 token 后派发）→ 单例重置；
 *  重新认证后消费方首次访问以新 token 懒重建（新订阅 + 立即首次加载）。
 *  生产环境固定 api.listProjects + 30s 兜底 + subscribeProjects；测试通过 createStore 直接注入受控依赖。 */
export function useProjects(): StoreSnapshot {
  const h = getStoreHandle();
  return useSyncExternalStore(
    h.store.subscribe,
    h.getSnapshot,
    h.getSnapshot,
  );
}

/** 暴露 trailing refresh() 供变更操作成功后调用。 */
export function useProjectsRefresh(): () => Promise<void> {
  return getStoreHandle().store.refresh;
}

function getStoreHandle(deps?: StoreDeps): StoreHandle {
  if (storeHandle) return storeHandle;
  storeHandle = deps ? createStore(deps) : createProductionStore();
  bindUnauthorizedReset();
  return storeHandle;
}

/** 生产 store 重置（401 → 重新认证生命周期）：释放当前 store（关闭 SSE 订阅、停止兜底
 *  轮询）并清除单例缓存；下一次消费方访问时懒重建——重建发生在重新认证后（App 切回
 *  已认证渲染），保证新订阅使用新 token。幂等：无在管 store 时为 no-op。 */
export function resetProjectsStore(): void {
  if (!storeHandle) return;
  storeHandle.store.dispose();
  storeHandle = null;
}

/** 401 重置绑定（应用生命周期注册一次）：事件由 request()/sse 流在 clearToken 后派发，
 *  即旧 token 被拒的时刻——此刻释放 store 安全，且 MUST NOT 在事件上急切重建订阅
 *  （token 已清除，重建会用无效 token）；重建交给重新认证后的首次消费方访问。 */
let unauthorizedResetBound = false;
function bindUnauthorizedReset(): void {
  if (unauthorizedResetBound || typeof window === 'undefined') return;
  unauthorizedResetBound = true;
  window.addEventListener(UNAUTHORIZED_EVENT, () => resetProjectsStore());
}

/** 测试专用：以注入依赖模拟消费方对生产单例的访问（走 getStoreHandle 真实路径，
 *  覆盖「事件 → 释放 → 再次访问懒重建」的生命周期）。 */
export const __projectsStoreAccessForTest = (deps?: StoreDeps): StoreHandle =>
  getStoreHandle(deps);

/** 兜底轮询周期（projects-stream design D6）：项目 CRUD 无事件，流外变更最长 ~30s 可见。 */
const PROJECTS_FALLBACK_POLL_MS = 30000;

function createProductionStore(): StoreHandle {
  return createStore({
    loader: () => api.listProjects(),
    startTimer: (fire) => {
      const id = setInterval(fire, PROJECTS_FALLBACK_POLL_MS);
      return () => clearInterval(id);
    },
    streamFactory: (handlers) => subscribeProjects(handlers),
  });
}

/** 创建 projects store（生产单例与测试共用同一实现；实例完全隔离）。
 *  - pollOnce 在途时直接返回在途 Promise、不设 trailing 标记（轮询不产生额外补发）。
 *  - refresh 在途时标记 trailing 并排队 waiter，补发请求结束（成功/失败）后才 settle waiter；
 *    失败时 waiter reject（调用方感知失败），pendingTrailing 不丢（下次触发仍补发）。
 *  - SSE 流（deps.streamFactory，design D6）：创建即订阅，snapshot/update 帧整表替换快照
 *    （首帧即 initialized）；流失败走 error 通道、保留旧数据；dispose 关闭订阅。
 *  - 实例隔离：每次调用独立的 snapshot/listeners/inflight/timer/订阅，dispose() 释放。 */
export function createStore(deps: StoreDeps): StoreHandle {
  // 实例局部状态（不共享模块全局）。
  let snapshot: StoreSnapshot = { projects: [], loading: false, initialized: false, error: '' };
  const listeners = new Set<() => void>();

  const emit = () => {
    for (const l of listeners) l();
  };
  const set = (patch: Partial<StoreSnapshot>) => {
    snapshot = { ...snapshot, ...patch };
    emit();
  };
  const getSnapshot = () => snapshot;

  let inflight: Promise<unknown> | null = null;
  let pendingTrailing = false;
  let initialized = false;
  /** refresh waiter 队列：在途 refresh() 调用方排队，补发请求结束（成功/失败）后 settle。 */
  const refreshWaiters: Array<{ resolve: () => void; reject: (e: Error) => void }> = [];

  /** settle 当前 inflight 上的 refresh waiter：无 pendingTrailing 时释放（成功 resolve / 失败 reject）。 */
  const settleWaiters = (err: Error | null) => {
    if (!pendingTrailing) {
      const ws = refreshWaiters.splice(0);
      for (const w of ws) {
        if (err) w.reject(err);
        else w.resolve();
      }
    }
  };

  const doLoad = (): Promise<Error | null> => {
    const p = (async (): Promise<Error | null> => {
      let loadErr: Error | null = null;
      try {
        const result = await deps.loader();
        initialized = true;
        set({ projects: result, error: '', loading: false, initialized: true });
      } catch (err) {
        loadErr = err instanceof Error ? err : new Error('加载失败');
        // 失败保留上次成功数据静默展示（侧栏不闪空态）；error 暴露给页面。
        set({ error: loadErr.message, loading: false });
      }
      inflight = null;
      if (pendingTrailing) {
        // trailing 补发：在上一请求结束后再发一次，反映调用之后的最新状态。
        pendingTrailing = false;
        void startLoad().catch(() => {
          /* 补发失败已记录 store error；吞掉避免 unhandled rejection */
        });
      } else {
        settleWaiters(loadErr);
      }
      return loadErr;
    })();
    inflight = p;
    return p;
  };

  /** 底层启动一次加载（不检查 inflight；调用方保证无在途）。仅在「无数据+在途」时置 loading。 */
  const startLoad = (): Promise<void> => {
    if (!initialized) set({ loading: true });
    // startLoad 对调用方表现为成功 resolve（错误已记录 store error）。
    return doLoad().then(() => undefined);
  };

  /** pollOnce：轮询用。在途时直接返回在途 Promise、不设 trailing 标记（轮询不产生额外补发）。 */
  const pollOnce = (): Promise<void> => {
    if (inflight) return inflight.then(() => undefined);
    return startLoad();
  };

  /** trailing refresh：在途时排队等待补发请求结束后才 settle；不在途直接加载并等待。
   *  补发请求失败时 waiter reject（调用方感知失败）；pendingTrailing 不丢（下次触发仍补发）。 */
  const refresh = (): Promise<void> => {
    if (!inflight) {
      // 无在途：直接发起加载；失败时 reject（doLoad 返回错误）让调用方感知。
      if (!initialized) set({ loading: true });
      return doLoad().then((err) => {
        if (err) throw err;
      });
    }
    // 在途：标记 trailing，等补发请求结束后 settle。
    pendingTrailing = true;
    return new Promise<void>((resolve, reject) => {
      refreshWaiters.push({ resolve, reject: (e) => reject(e) });
    });
  };

  // SSE 流订阅（design D6）：帧整表替换（首帧即 initialized、清 error）；
  // 流失败走 error 通道、保留旧数据（沿轮询失败语义，侧栏不闪空态）。
  const stream = deps.streamFactory?.({
    onData: (projects) => {
      initialized = true;
      set({ projects, error: '', loading: false, initialized: true });
    },
    onError: (message) => set({ error: message, loading: false }),
  });

  // 启动首次加载与兜底轮询（间隔由 startTimer 注入，生产 30s；
  // single-flight：并发请求合并为一次在途，pollOnce 不产生补发）。
  void pollOnce();
  let stopTimer: (() => void) | undefined;
  if (deps.startTimer) stopTimer = deps.startTimer(() => void pollOnce());

  return {
    store: {
      subscribe(listener: () => void) {
        listeners.add(listener);
        return () => listeners.delete(listener);
      },
      pollOnce,
      refresh,
      dispose: () => {
        stopTimer?.();
        stream?.close();
        listeners.clear();
      },
    },
    getSnapshot,
  };
}

/** 测试隔离用（历史导出名）：与生产 resetProjectsStore 同一实现。 */
export const __resetProjectsStoreForTest = resetProjectsStore;

/* ==================== mutation 编排契约（共享：原 ProjectsPage.tsx，tasks 8.5 迁移） ====================
 *  mutation 成败只由 API 调用决定；成功后才调 refresh()（trailing），refresh 失败静默
 *  （由共享 store error 通道承担），MUST NOT 回落到 mutation 错误文案。
 *  onError 收到原始错误，由调用方格式化为用户文案（区分创建/删除 409 等）。
 *  抽为纯函数供契约测试直接驱动（无 jsdom 渲染环境）。
 *  @returns mutation 是否成功（API 成功为 true；失败为 false 且已调 onError）。
 *  调用方：CommandCenterPage（内联创建）、ProjectsManagePage（注册/删除/任务行操作）、工作台任务操作。 */
export async function runProjectMutation(opts: {
  mutate: () => Promise<unknown>;
  refresh: () => Promise<void>;
  onSuccess: () => void;
  onError: (err: unknown) => void;
}): Promise<boolean> {
  try {
    await opts.mutate();
  } catch (err) {
    opts.onError(err);
    return false;
  }
  // mutation 成功：成功副作用 + trailing refresh（失败静默，store error 通道承担）。
  opts.onSuccess();
  void opts.refresh().catch(() => {});
  return true;
}

/** 映射创建 API 错误为用户文案。 */
export function createErrorMessage(err: unknown): string {
  return err instanceof ApiError ? `[${err.code}] ${err.message}` : '创建失败';
}

/** 映射删除 API 错误为用户文案（409 → 项目下仍有任务；其他 → 通用）。 */
export function deleteErrorMessage(err: unknown): string {
  if (err instanceof ApiError) {
    return err.status === 409 ? '项目下仍有任务，无法删除' : `[${err.code}] ${err.message}`;
  }
  return '删除失败';
}
