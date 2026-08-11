/* VENDORED 设计源 · 固化于 2026-08-08（OpenSpec change: web-ui-redesign）
 * 来源：Open Design 项目 2396c3bb-e795-41fb-9536-723f69c71734/assets/ocdeck-theme.js（未改动）
 * 用途：主题切换行为规格；实现为 React hook 移植（design.md D5），本文件不直接运行。
 * --------------------------------------------------------------------------
 * ocdeck 主题切换（浅色 / 深色 / 跟随系统）
   在各页 <head> 内同步引入（CSS 之前），避免首屏闪烁。
   偏好持久化于 localStorage['od-theme'] ∈ {system, light, dark}，默认 system。
   全局 API：OD_THEME.get() / OD_THEME.set(mode) / OD_THEME.resolved()
   组件约定：带 data-od-theme-toggle 的按钮自动接管点击与图标切换；
   带 data-od-theme-seg 的分段控件（button[data-mode]）自动接管选中态。 */
(function () {
  var STORE = 'od-theme';
  var MODES = ['system', 'light', 'dark'];
  var mq = window.matchMedia('(prefers-color-scheme: dark)');
  var root = document.documentElement;

  function stored() {
    try {
      var v = localStorage.getItem(STORE);
      return MODES.indexOf(v) >= 0 ? v : 'system';
    } catch (_) { return 'system'; }
  }
  function resolve(mode) {
    return mode === 'system' ? (mq.matches ? 'dark' : 'light') : mode;
  }
  function paint() {
    var mode = stored();
    root.setAttribute('data-theme', resolve(mode));
    root.setAttribute('data-theme-mode', mode);
    syncSeg();
  }
  function set(mode) {
    if (MODES.indexOf(mode) < 0) return;
    try { localStorage.setItem(STORE, mode); } catch (_) {}
    paint();
  }
  function syncSeg() {
    var mode = stored();
    Array.prototype.forEach.call(document.querySelectorAll('[data-od-theme-seg]'), function (seg) {
      Array.prototype.forEach.call(seg.querySelectorAll('button[data-mode]'), function (btn) {
        var on = btn.getAttribute('data-mode') === mode;
        btn.classList.toggle('on', on);
        btn.setAttribute('aria-pressed', on ? 'true' : 'false');
      });
    });
    Array.prototype.forEach.call(document.querySelectorAll('[data-od-theme-toggle]'), function (btn) {
      var next = resolve(stored()) === 'dark' ? '浅色' : '深色';
      btn.setAttribute('aria-label', '切换到' + next + '模式');
      btn.title = '主题：' + (stored() === 'system' ? '跟随系统' : (stored() === 'dark' ? '深色' : '浅色')) + '（点击切换）';
    });
  }

  window.OD_THEME = {
    get: stored,
    set: set,
    resolved: function () { return resolve(stored()); }
  };

  // 系统主题变化时，仅 system 模式下跟随
  if (mq.addEventListener) mq.addEventListener('change', function () { if (stored() === 'system') paint(); });
  else if (mq.addListener) mq.addListener(function () { if (stored() === 'system') paint(); });

  // 委托绑定：切换按钮与分段控件（脚本在 head 同步执行，绑定发生在 DOM 构建后）
  function bind() {
    Array.prototype.forEach.call(document.querySelectorAll('[data-od-theme-toggle]'), function (btn) {
      btn.addEventListener('click', function () {
        var order = { system: 'dark', dark: 'light', light: 'system' };
        set(order[stored()] || 'dark');
      });
    });
    Array.prototype.forEach.call(document.querySelectorAll('[data-od-theme-seg]'), function (seg) {
      seg.addEventListener('click', function (e) {
        var btn = e.target.closest('button[data-mode]');
        if (btn) set(btn.getAttribute('data-mode'));
      });
    });
    syncSeg();
  }
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', bind);
  else bind();

  // 首屏防闪烁：立即设置 data-theme
  paint();
})();
