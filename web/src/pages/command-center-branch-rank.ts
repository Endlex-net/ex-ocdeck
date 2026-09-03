/**
 * 指挥中心新建任务面板「基准分支下拉过滤后排序」（task-base-branch-context D2）。
 *
 * 纯函数：无 React、无 IO，可单测驱动。排序元组升序稳定（值小优先）：
 * 1. 同名命中：本地名（大小写不敏感剥除 `origin/` 前缀后的部分，无此前缀则整名）等于 query → 0，否则 1；
 * 2. 是否远端：小写后以 `origin/` 开头 → 0，否则 1（其它 remote 名如 `upstream/` 不加分）；
 * 3. 过滤前原顺序下标。
 *
 * query 为 fold 后的 q（`normalizedInput.toLowerCase()`）；`origin/` 仅作 UI 远端标记，
 * 与 `git branch -r` 短名惯例一致（D1）。
 */
export function rankBranchOptions(options: string[], query: string): string[] {
  const ranked = options.map((name, index) => {
    const lower = name.toLowerCase();
    const isRemote = lower.startsWith('origin/');
    const localName = isRemote ? lower.slice('origin/'.length) : lower;
    return { name, sameName: localName === query ? 0 : 1, remote: isRemote ? 0 : 1, index };
  });
  ranked.sort((a, b) => a.sameName - b.sameName || a.remote - b.remote || a.index - b.index);
  return ranked.map((r) => r.name);
}
