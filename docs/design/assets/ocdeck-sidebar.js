/* VENDORED 设计源 · 固化于 2026-08-08（OpenSpec change: web-ui-redesign）
 * 来源：Open Design 项目 2396c3bb-e795-41fb-9536-723f69c71734/assets/ocdeck-sidebar.js（未改动）
 * 用途：侧栏折叠行为规格；实现为 React 组件移植（design.md D1/壳层），本文件不直接运行。
 * --------------------------------------------------------------------------
 * ocdeck 侧栏折叠：收起为 60px 图标轨（保留导航图标与任务状态点），
   折叠按钮常驻侧栏底栏、图标随状态翻转。状态持久化于 localStorage
   （ocdeck:side-collapsed），⌘B / Ctrl+B 快捷切换。
   仅 ≥768px 生效（平板可用）；无 .od-shell/.od-sidebar 的页面自动 no-op。 */
(function () {
  var sidebar = document.querySelector('.od-shell > .od-sidebar');
  if (!sidebar) return;
  var body = document.body;
  var KEY = 'ocdeck:side-collapsed';

  var ICON_CLOSE = '<svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="3" y="4" width="18" height="16" rx="2"/><path d="M9 4v16"/><path d="m14 9-3 3 3 3"/></svg>';
  var ICON_OPEN = '<svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="3" y="4" width="18" height="16" rx="2"/><path d="M9 4v16"/><path d="m12 9 3 3-3 3"/></svg>';
  var ICON_SEARCH = '<svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><circle cx="11" cy="11" r="7"/><path d="m20 20-3.5-3.5"/></svg>';

  function read() { try { return localStorage.getItem(KEY) === '1'; } catch (_) { return false; } }
  function store(v) { try { localStorage.setItem(KEY, v ? '1' : '0'); } catch (_) {} }

  // 折叠态可用性：图标轨下文字被隐藏，为导航项与任务项补 tooltip
  sidebar.querySelectorAll('.od-nav-item, .od-nav-task').forEach(function (el) {
    if (!el.title) el.title = el.textContent.trim();
  });

  // ⌘K 入口在图标轨下只显示放大镜图标
  var cmdk = sidebar.querySelector('.od-sidebar-cmdk');
  if (cmdk) {
    cmdk.insertAdjacentHTML('afterbegin', ICON_SEARCH);
    if (!cmdk.title) cmdk.title = '命令面板（⌘K）';
  }

  // 折叠按钮：复用 od-theme-btn 视觉，常驻侧栏底栏（图标轨下仍可点）
  var collapseBtn = document.createElement('button');
  collapseBtn.type = 'button';
  collapseBtn.className = 'od-theme-btn od-side-collapse';
  collapseBtn.setAttribute('data-od-id', 'sidebar-collapse');
  var foot = sidebar.querySelector('.od-sidebar-foot');
  if (!foot) return;
  foot.insertBefore(collapseBtn, foot.querySelector('.od-theme-btn') || null);

  function apply(v) {
    body.classList.toggle('od-side-collapsed', v);
    collapseBtn.setAttribute('aria-expanded', String(!v));
    collapseBtn.innerHTML = v ? ICON_OPEN : ICON_CLOSE;
    var label = v ? '展开侧边栏（⌘B）' : '收起侧边栏（⌘B）';
    collapseBtn.setAttribute('aria-label', label);
    collapseBtn.title = label;
  }
  function toggle() {
    var v = !body.classList.contains('od-side-collapsed');
    store(v);
    apply(v);
  }

  collapseBtn.addEventListener('click', toggle);
  window.addEventListener('keydown', function (e) {
    if ((e.metaKey || e.ctrlKey) && !e.altKey && !e.shiftKey && (e.key === 'b' || e.key === 'B')) {
      e.preventDefault();
      toggle();
    }
  });

  apply(read());
})();
