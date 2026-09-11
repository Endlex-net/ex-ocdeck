/** 工作台页头分支区（design D3）的纯逻辑：base_ref 短名转换与 tooltip/aria 模板。
 *  独立成模块以便契约测试直接导入（TaskWorkbenchPage 依赖 xterm，不宜进 node 测试环境）。 */

/** base_ref 展示短名（design D2/D5）：空串返回空串；仅当输入以 `refs/heads/` 或
 *  `refs/remotes/` 开头且前缀后名称非空时移除前缀（refs/remotes 保留 remote 段，
 *  如 refs/remotes/origin/main → origin/main）；其余输入（未匹配前缀、前缀后为空等
 *  异常形态）原样返回，不抛错、不做新增查询。 */
export function baseRefShortName(ref: string): string {
  if (ref === '') return '';
  for (const prefix of ['refs/heads/', 'refs/remotes/']) {
    if (ref.startsWith(prefix) && ref.length > prefix.length) return ref.slice(prefix.length);
  }
  return ref;
}

/** tooltip 唯一模板集（design D3）：完整文本出口 + 复制可发现性。
 *  来源 button——原值与短名不同时携带全限定 ref，相同时（异常形态原样返回）不重复。 */
export function sourceBranchTooltip(shortName: string, fullRef: string): string {
  return shortName === fullRef
    ? `来源分支：${shortName}（点击复制）`
    : `来源分支：${shortName}（${fullRef}）（点击复制）`;
}

export function currentBranchTooltip(name: string): string {
  return `当前分支：${name}（点击复制）`;
}
