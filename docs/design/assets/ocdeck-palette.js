/* VENDORED 设计源 · 固化于 2026-08-08（OpenSpec change: web-ui-redesign）
 * 来源：Open Design 项目 2396c3bb-e795-41fb-9536-723f69c71734/assets/ocdeck-palette.js（未改动）
 * 用途：⌘K 命令面板行为规格参考；实现为 React 组件移植（design.md D10），本文件不直接运行。
 * --------------------------------------------------------------------------
 * ocdeck 全局命令面板（⌘K / Ctrl+K）
 * 用法：页面在加载本脚本前可选定义 window.OD_PALETTE_EXTRA = [{group,label,hint,href,action,keywords}]
 * 所有屏幕位于 screens/ 目录，href 均为同目录相对链接。 */
(function () {
  'use strict';

  /* ---------- 全局指令注册表 ---------- */
  var GLOBAL = [
    { group: '页面', label: '指挥中心', hint: '首页', href: 'command-center.html', keywords: 'command center home 首页 活跃' },
    { group: '页面', label: '项目管理', hint: '工作区', href: 'projects.html', keywords: 'projects workspace 项目 工作区' },
    { group: '页面', label: '设置 · 终端外观', href: 'configs.html#appearance', keywords: 'settings terminal font 设置 终端 字体' },
    { group: '页面', label: '设置 · 环境变量', href: 'configs.html#env', keywords: 'settings env 环境变量' },
    { group: '页面', label: '设置 · opencode 配置', href: 'configs.html#opencode', keywords: 'settings opencode config 配置文件' },
    { group: '页面', label: '设置 · AI 配置', href: 'configs.html#ai', keywords: 'settings ai provider key 模型' },

    { group: '任务', label: '重构 agent 通信', hint: 'ocdeck · feat/agent-comm', href: 'task-workbench.html', keywords: 'task workbench busy 工作台', dot: 'busy' },
    { group: '任务', label: '修 diff 行号 bug', hint: 'ocdeck · fix/diff-linenum', href: 'task-workbench.html', keywords: 'task workbench 工作台', dot: 'idle' },
    { group: '任务', label: '落地页文案', hint: 'blog-next · feat/landing-copy', href: 'task-workbench.html', keywords: 'task workbench 工作台', dot: 'idle' },
    { group: '任务', label: '图片压缩脚本', hint: 'blog-next · chore/img-compress', href: 'task-workbench.html', keywords: 'task workbench 工作台', dot: 'idle' },

    { group: '操作', label: '新建任务', hint: '指挥中心', href: 'command-center.html', focus: 'new-task-name', keywords: 'new create task 新建 创建' },
    { group: '操作', label: '注册项目', hint: '项目管理', href: 'projects.html', keywords: 'register add project 注册 添加' }
  ];

  /* ---------- DOM ---------- */
  var overlay = document.createElement('div');
  overlay.className = 'od-palette-overlay';
  overlay.setAttribute('data-od-id', 'command-palette');
  overlay.innerHTML =
    '<div class="od-palette" role="dialog" aria-modal="true" aria-label="命令面板">' +
      '<div class="od-palette-input-row">' +
        '<svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round"><circle cx="11" cy="11" r="7"/><path d="M20 20l-3.5-3.5"/></svg>' +
        '<input class="od-palette-input" type="text" placeholder="搜索页面、任务、操作…" aria-label="搜索命令">' +
        '<span class="od-palette-kbd">esc</span>' +
      '</div>' +
      '<div class="od-palette-list" role="listbox"></div>' +
      '<div class="od-palette-foot"><span><b>↑↓</b>选择</span><span><b>⏎</b>执行</span><span><b>esc</b>关闭</span></div>' +
    '</div>';
  document.body.appendChild(overlay);

  var input = overlay.querySelector('.od-palette-input');
  var list = overlay.querySelector('.od-palette-list');
  var items = [];
  var current = 0;

  function allCommands() {
    var extra = Array.isArray(window.OD_PALETTE_EXTRA) ? window.OD_PALETTE_EXTRA : [];
    return extra.concat(GLOBAL);
  }

  function matches(cmd, q) {
    if (!q) return true;
    var hay = (cmd.group + ' ' + cmd.label + ' ' + (cmd.hint || '') + ' ' + (cmd.keywords || '')).toLowerCase();
    return q.toLowerCase().split(/\s+/).every(function (w) { return hay.indexOf(w) !== -1; });
  }

  function render() {
    var q = input.value.trim();
    var cmds = allCommands().filter(function (c) { return matches(c, q); });
    items = cmds;
    current = 0;
    list.innerHTML = '';
    if (!cmds.length) {
      list.innerHTML = '<div class="od-palette-empty">没有匹配的命令</div>';
      return;
    }
    var lastGroup = null;
    cmds.forEach(function (cmd, i) {
      if (cmd.group !== lastGroup) {
        lastGroup = cmd.group;
        var g = document.createElement('div');
        g.className = 'od-palette-group';
        g.textContent = cmd.group;
        list.appendChild(g);
      }
      var b = document.createElement('button');
      b.type = 'button';
      b.className = 'od-palette-item' + (i === current ? ' current' : '');
      b.setAttribute('role', 'option');
      b.setAttribute('aria-selected', i === current ? 'true' : 'false');
      var dot = cmd.dot
        ? '<span class="od-agent-' + cmd.dot + ' od-palette-dot" aria-hidden="true"><span class="od-agent-dot"></span></span>'
        : '';
      b.innerHTML = dot + '<span>' + cmd.label + '</span>' +
        (cmd.hint ? '<span class="od-palette-hint">' + cmd.hint + '</span>' : '');
      b.addEventListener('click', function () { run(cmd); });
      b.addEventListener('mousemove', function () {
        if (current !== i) { current = i; paintCurrent(); }
      });
      list.appendChild(b);
    });
  }

  function paintCurrent() {
    var nodes = list.querySelectorAll('.od-palette-item');
    nodes.forEach(function (n, i) {
      n.classList.toggle('current', i === current);
      n.setAttribute('aria-selected', i === current ? 'true' : 'false');
    });
    var el = nodes[current];
    if (el) {
      /* 面板内部滚动定位，不用 scrollIntoView（会破坏嵌入式预览），手动算 offset */
      list.scrollTop = el.offsetTop - list.clientHeight / 2 + el.clientHeight / 2;
    }
  }

  function run(cmd) {
    close();
    if (typeof cmd.action === 'function') { cmd.action(); return; }
    if (cmd.href) {
      var same = location.pathname.endsWith('/' + cmd.href.split('#')[0]);
      if (same && cmd.focus) {
        var t = document.getElementById(cmd.focus);
        if (t) {
          /* 目标可能在折叠面板内：先广播事件让页面展开，再聚焦 */
          document.dispatchEvent(new CustomEvent('od:palette-focus', { detail: { id: cmd.focus } }));
          t.focus();
          return;
        }
      }
      location.href = cmd.href;
    }
  }

  function open() {
    overlay.classList.add('open');
    input.value = '';
    render();
    input.focus();
  }
  function close() { overlay.classList.remove('open'); }
  function isOpen() { return overlay.classList.contains('open'); }

  /* ---------- 事件 ---------- */
  document.addEventListener('keydown', function (e) {
    if ((e.metaKey || e.ctrlKey) && String(e.key).toLowerCase() === 'k') {
      e.preventDefault();
      isOpen() ? close() : open();
      return;
    }
    if (!isOpen()) return;
    if (e.key === 'Escape') { e.preventDefault(); close(); }
    else if (e.key === 'ArrowDown') { e.preventDefault(); if (items.length) { current = Math.min(items.length - 1, current + 1); paintCurrent(); } }
    else if (e.key === 'ArrowUp') { e.preventDefault(); if (items.length) { current = Math.max(0, current - 1); paintCurrent(); } }
    else if (e.key === 'Enter') { e.preventDefault(); if (items[current]) run(items[current]); }
  });
  input.addEventListener('input', render);
  overlay.addEventListener('mousedown', function (e) { if (e.target === overlay) close(); });

  window.odPalette = { open: open, close: close };
})();
