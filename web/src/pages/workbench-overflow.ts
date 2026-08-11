/** 工作台页头「⋯」溢出菜单（WorkbenchOverflow）的纯逻辑判定。
 *  独立成模块以便契约测试直接导入（TaskWorkbenchPage 依赖 xterm，不宜进 node 测试环境）。 */

/** 溢出菜单失焦关闭判定（disclosure 模式）：仅当焦点真实落到溢出区之外的元素
 *  （键盘 Tab、点击可聚焦控件）时才关闭。
 *  relatedTarget 为 null 的失焦 MUST NOT 关闭菜单：触屏（iOS Safari）与桌面 Safari
 *  点击按钮不转移焦点，浏览器派发 focusout(relatedTarget=null) 后焦点落 body——
 *  这是"点击的副作用"而非用户移焦意图；此时关闭会把即将接收 click 的菜单项一起卸载，
 *  点击丢失（回归：归档任务在 ⋯ 菜单点「删除任务」无反应、DeleteTaskModal 弹窗不出）。
 *  此场景由全屏 backdrop 的 click 兜底关闭。 */
export function shouldCloseOverflowOnBlur(
  next: Node | null,
  contains: (n: Node) => boolean,
): boolean {
  return next !== null && !contains(next);
}
