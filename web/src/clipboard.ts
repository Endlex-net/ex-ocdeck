/** 共享剪贴板写入（workbench-base-ref-and-overflow D5，自 TerminalView 提取）。
 *  Promise<void> 语义与降级链：
 *  ① navigator.clipboard.writeText 可用 → 调用一次，resolve 即成功、reject 即失败
 *    （MUST NOT 再尝试 execCommand）；
 *  ② API 不可用 → execCommand 降级，返回 true 才成功，返回 false 或抛错均失败。
 *  TerminalView（远程程序复制/确认 popover）与页头分支名点击复制共用；
 *  各调用方自行管理反馈 UI（终端 takeToastSlot 节流 / 页头 .od-toast 不节流）。 */

/** 用户手势内复制：有 Clipboard API 走 writeText，否则 execCommand。 */
export function writeTextToClipboard(text: string): Promise<void> {
  if (typeof navigator !== 'undefined' && navigator.clipboard?.writeText) {
    return navigator.clipboard.writeText(text);
  }
  return copyViaExecCommand(text);
}

function copyViaExecCommand(text: string): Promise<void> {
  return new Promise((resolve, reject) => {
    const ta = document.createElement('textarea');
    ta.value = text;
    ta.setAttribute('readonly', '');
    ta.style.position = 'fixed';
    ta.style.left = '-9999px';
    document.body.appendChild(ta);
    ta.select();
    try {
      if (document.execCommand('copy')) resolve();
      else reject(new Error('copy failed'));
    } catch (err) {
      reject(err);
    } finally {
      ta.remove();
    }
  });
}
