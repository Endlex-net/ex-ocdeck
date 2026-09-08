/**
 * 导航焦点请求信号（fix-terminal-input-panel-resize design D3）：模块级内存单例。
 *
 * 生产点（导航仍走原生 href/navigate，信号为附加调用）：AppShell 侧栏任务项、
 * 工作台任务切换器、指挥中心任务行。唯一消费方：目标任务 TUI TerminalView
 * （shell 实例 MUST NOT 消费）。单例只保留最新 pending 请求，新覆盖旧；
 * 普通重连自身不产生请求，已消费请求不因重连重新聚焦。
 *
 * - 订阅时同步交付当前 pending 快照（解决「发布早于挂载」）；
 * - 消费/取消按 seq 比较，仅清除匹配 seq 的请求；订阅退订 MUST NOT 删除更晚发布的新请求；
 * - 过期固定 5s，由消费方经 isTerminalFocusExpired 判定；
 * - 等待期取消由请求层负责（覆盖消费方未挂载的窗口，如工作台加载阶段）：
 *   焦点进入任何输入元素即作废；路由离开目标任务由工作台卸载时按 taskID 作废。
 */

export interface TerminalFocusRequest {
  taskID: string;
  /** 单调递增；消费/取消按 seq 比对，仅清除匹配 seq 的请求。 */
  seq: number;
  /** 发布时刻（ms，Date.now）。 */
  ts: number;
}

/** 请求有效期（design D3 固定值）。 */
export const FOCUS_REQUEST_TTL_MS = 5000;

let pending: TerminalFocusRequest | null = null;
let nextSeq = 0;
const listeners = new Set<(req: TerminalFocusRequest) => void>();

/** 等待期输入区取消（design D3「进入任何输入区 → 请求作废」）：焦点进入 input/
 * textarea/contenteditable 即作废当前请求。挂在请求层而非消费方，等待的完整
 * 生命周期（含目标工作台 task 未返回、TerminalView 未挂载的阶段）都有人记录。 */
function handleDocFocusIn(e: FocusEvent): void {
  if (pending && isInputElementTarget(e.target as Element | null)) setPending(null);
}

function setPending(req: TerminalFocusRequest | null): void {
  const had = pending !== null;
  pending = req;
  if (req && !had) document.addEventListener('focusin', handleDocFocusIn);
  else if (!req && had) document.removeEventListener('focusin', handleDocFocusIn);
}

/**
 * 发布焦点请求（新覆盖旧）。返回本次发布的请求快照（含 seq，供消费方按 seq 比对）——
 * 即使某监听器同步消费，返回值也恒为该快照，不会是 null。
 *
 * 分发语义（确定性）：发布是「事件通知」，每个监听器独立收到本次请求快照；某监听器
 * 同步消费（清除）不阻断后续监听器的通知（消费幂等且按 seq 守卫，不会重复聚焦）；
 * 分发期间若重入发布（seq 更大的新请求），旧分发立即终止，由新发布完成自己的分发。
 */
export function requestTerminalFocus(taskID: string): TerminalFocusRequest {
  const req: TerminalFocusRequest = { taskID, seq: ++nextSeq, ts: Date.now() };
  setPending(req);
  for (const cb of [...listeners]) {
    // 发布代数守卫：nextSeq 已前进即发生过重入发布，旧分发立即终止——
    // 不依赖 pending 是否存在（重入的新请求即使被同步消费清空，旧分发仍终止）
    if (nextSeq > req.seq) break;
    cb(req);
  }
  return req;
}

/**
 * 订阅焦点请求：注册时同步交付当前 pending 快照（无 pending 不回调）。
 * 返回退订函数——只移除监听，MUST NOT 删除更晚发布的新请求（作废走
 * clearTerminalFocus / cancelTerminalFocusForTask）。
 */
export function subscribeTerminalFocus(cb: (req: TerminalFocusRequest) => void): () => void {
  listeners.add(cb);
  const current = pending;
  if (current) cb(current);
  return () => {
    listeners.delete(cb);
  };
}

/** 读取当前 pending 请求（不消费）。消费方在「等待 connected」期间以它校验
 * 请求是否仍然有效（被取消/覆盖后本地不再持有状态）。 */
export function pendingTerminalFocus(): TerminalFocusRequest | null {
  return pending;
}

/** 消费/取消共用清除入口：仅当 pending 仍是该 seq 的请求时清除（新请求覆盖后不可误删）。 */
export function clearTerminalFocus(seq: number): void {
  if (pending?.seq === seq) setPending(null);
}

/** 路由离开目标任务（工作台卸载）取消：仅当 pending 仍属于该任务时清除（按
 * taskID 守卫——A 的卸载不得误删其他任务的请求；同任务更早请求由消费按 seq 清除）。 */
export function cancelTerminalFocusForTask(taskID: string): void {
  if (pending?.taskID === taskID) setPending(null);
}

/** 门禁①（过期）：超过 TTL 即过期。 */
export function isTerminalFocusExpired(req: TerminalFocusRequest, now = Date.now()): boolean {
  return now - req.ts >= FOCUS_REQUEST_TTL_MS;
}

/** 门禁⑤（输入元素排除）：input/textarea/contenteditable 一律不允许转移焦点。 */
export function isInputElementTarget(el: Element | null): boolean {
  if (!el) return false;
  const tag = el.tagName;
  if (tag === 'INPUT' || tag === 'TEXTAREA') return true;
  return (el as HTMLElement).isContentEditable === true;
}

/**
 * 门禁④⑤（焦点保护，输入元素排除优先于区域白名单）：activeElement 为 body 或位于
 * .od-sidebar / 任务切换器（.wb-switcher）/ 指挥中心任务行（.cc-page 内 .od-row-clickable）
 * 内，且不是输入元素，才允许把焦点转移给终端。el 为 null 按不允许处理。
 * 任务行选择器限定指挥中心容器，项目管理页的同名行不放行。
 * 保守策略（design D3）：宁可不聚焦不抢焦点。
 */
export function isFocusRequestTargetAllowed(el: Element | null): boolean {
  if (!el || isInputElementTarget(el)) return false;
  if (el === document.body) return true;
  return el.closest('.od-sidebar, .wb-switcher, .cc-page .od-row-clickable') !== null;
}
